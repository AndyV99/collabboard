package realtime

// The hub: this process's connections, grouped by room.
//
// # Locking
//
// Two mutexes, and the split is the whole design.
//
//   - mu (RWMutex) guards the maps. It is taken for the duration of a map
//     operation and never across I/O, so fan-out — which takes it for read —
//     can never be blocked by a Redis call.
//   - subMu (Mutex) serialises subscription changes at the broker and the
//     reference counts that drive them. It *is* held across a Redis SUBSCRIBE,
//     because two connections joining the same empty room concurrently must not
//     both decide they are the first; but nothing that delivers a message takes
//     it.
//
// The order is always subMu then mu, and no path takes mu first, so there is
// no cycle to deadlock on.
//
// # Fan-out and backpressure
//
// deliver holds a read lock and does one non-blocking channel send per
// connection. It never writes to a socket and never waits for one, so a client
// on a stalled TCP connection cannot slow down anybody else's frame. A client
// whose buffer is full is collected and closed *after* the lock is released —
// see [Hub.deliver].

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// ErrHubClosed is returned by anything that would create work on a hub that is
// shutting down or already shut down.
var ErrHubClosed = errors.New("realtime: hub is closed")

// HubConfig configures a [Hub]. Broker, Authorizer and Logger are required;
// everything else defaults.
type HubConfig struct {
	// Broker carries events between instances.
	Broker Broker

	// Authorizer decides who may watch which board.
	Authorizer Authorizer

	// Logger receives connection lifecycle and fan-out problems.
	Logger *slog.Logger

	// SendBuffer is the per-connection queue depth. Defaults to
	// [DefaultSendBuffer].
	SendBuffer int

	// PingInterval, PongTimeout and WriteTimeout govern keepalive and reaping.
	PingInterval time.Duration
	PongTimeout  time.Duration
	WriteTimeout time.Duration

	// ReadLimit caps an inbound frame, in bytes.
	ReadLimit int64

	// ReauthorizeInterval is how often live subscriptions are re-checked.
	ReauthorizeInterval time.Duration

	// MaxRoomsPerConnection caps how many boards one socket may watch.
	MaxRoomsPerConnection int

	// AllowedOrigins is the list of Origin patterns accepted on the handshake.
	// Empty means same-origin only, which is the safe default and the wrong one
	// for a separately hosted SPA — see internal/config.
	AllowedOrigins []string

	// ShutdownReconnectHint is the base delay a shutdown frame asks clients to
	// wait before reconnecting. Jittered per connection. Defaults to one
	// second.
	ShutdownReconnectHint time.Duration

	// now is time.Now, replaceable in tests.
	now func() time.Time
}

func (c HubConfig) withDefaults() HubConfig {
	if c.SendBuffer <= 0 {
		c.SendBuffer = DefaultSendBuffer
	}

	if c.PingInterval <= 0 {
		c.PingInterval = DefaultPingInterval
	}

	if c.PongTimeout <= 0 {
		c.PongTimeout = DefaultPongTimeout
	}

	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}

	if c.ReadLimit <= 0 {
		c.ReadLimit = DefaultReadLimit
	}

	if c.ReauthorizeInterval <= 0 {
		c.ReauthorizeInterval = DefaultReauthorizeInterval
	}

	if c.MaxRoomsPerConnection <= 0 {
		c.MaxRoomsPerConnection = DefaultMaxRoomsPerConnection
	}

	if c.ShutdownReconnectHint <= 0 {
		c.ShutdownReconnectHint = time.Second
	}

	if c.now == nil {
		c.now = time.Now
	}

	return c
}

// Hub owns the WebSocket connections one process is serving.
type Hub struct {
	cfg    HubConfig
	logger *slog.Logger

	mu    sync.RWMutex
	rooms map[Room]map[*Conn]struct{}
	conns map[*Conn]struct{}

	// closing is set by Shutdown. It is read on the upgrade path so a draining
	// instance refuses new connections instead of accepting one it is about to
	// close.
	closing bool

	subMu sync.Mutex
	subs  map[Room]int

	dispatchDone chan struct{}
	shutdownOnce sync.Once
}

// NewHub builds a hub and starts its dispatch loop.
//
// The caller owns the returned hub and must call [Hub.Shutdown].
func NewHub(cfg HubConfig) (*Hub, error) {
	switch {
	case cfg.Broker == nil:
		return nil, errors.New("realtime: hub needs a broker")
	case cfg.Authorizer == nil:
		return nil, errors.New("realtime: hub needs an authorizer")
	case cfg.Logger == nil:
		return nil, errors.New("realtime: hub needs a logger")
	}

	hub := &Hub{
		cfg:          cfg.withDefaults(),
		logger:       cfg.Logger,
		rooms:        map[Room]map[*Conn]struct{}{},
		conns:        map[*Conn]struct{}{},
		subs:         map[Room]int{},
		dispatchDone: make(chan struct{}),
	}

	go hub.dispatch()

	return hub, nil
}

// dispatch is the single consumer of the broker's stream.
//
// One goroutine, so ordering within a room is whatever Redis delivered and does
// not depend on scheduling here. It ends when the broker's channel closes,
// which [Hub.Shutdown] arranges.
func (h *Hub) dispatch() {
	defer close(h.dispatchDone)

	for msg := range h.cfg.Broker.Messages() {
		h.deliver(msg)
	}
}

// deliver fans one message out to the room's local connections.
//
// The read lock is held for the loop, which is safe because the only thing the
// loop does per connection is a non-blocking channel send. Connections whose
// buffer is full are collected and dealt with after the lock is released:
// closing one takes the write lock, so doing it inline would deadlock.
//
// Note that closing a connection here does not deregister it here. That happens
// in [Conn.serve]'s cleanup, which cannot run until the connection's own
// goroutines stop — and for a stalled peer that means waiting out the frame in
// flight, up to WriteTimeout. So a dropped client can still appear in this map
// for a moment afterwards. It is harmless: trySend short-circuits on a closed
// connection, so it receives nothing and costs nothing. Deregistering from this
// goroutine instead would mean taking the write lock in the middle of fan-out,
// which is the one thing this loop exists to avoid.
func (h *Hub) deliver(msg Message) {
	var slow []*Conn

	h.mu.RLock()

	for conn := range h.rooms[msg.Room] {
		if !conn.trySend(msg.Payload) {
			slow = append(slow, conn)
		}
	}

	h.mu.RUnlock()

	for _, conn := range slow {
		// The decision, stated once: a client that cannot keep up is
		// disconnected. Not "drop the message" — a client silently missing an
		// event has a board that disagrees with the server and no way to
		// notice. Not "block" — that would make one stalled TCP connection the
		// whole board's problem, which is the failure this buffer exists to
		// prevent. Closing is the only option that is both safe and visible:
		// the client sees close code 4002, reconnects, and re-fetches. See
		// README.md.
		h.logger.Warn("dropping a slow realtime client",
			slog.String("event", "realtime.client.slow"),
			slog.String("connection_id", conn.id.String()),
			slog.String("tenant_id", conn.principal.TenantID.String()),
			slog.String("user_id", conn.principal.UserID.String()),
			slog.String("room", msg.Room.String()),
			slog.Int("buffer", h.cfg.SendBuffer),
		)

		conn.close(StatusSlowConsumer, "send buffer full; reconnect and refetch")
	}
}

// Publish sends an event to everyone watching a board, on every instance.
//
// It does not deliver locally. The event goes to the broker and arrives back
// through this instance's own subscription, so the local and remote paths are
// the same path — see the package comment.
func (h *Hub) Publish(ctx context.Context, room Room, event Event) error {
	if !room.valid() {
		return fmt.Errorf("realtime: refusing to publish to an incomplete room %s", room)
	}

	boardID := room.BoardID

	payload, err := Frame{
		Type:    FrameEvent,
		BoardID: &boardID,
		Event:   &event,
	}.encode()
	if err != nil {
		return err
	}

	return h.cfg.Broker.Publish(ctx, room, payload)
}

// register admits a connection to the hub, before it has subscribed to
// anything.
//
// Registration is deliberately separate from joining a room. A client that has
// connected but not yet named a board is still a socket this process is holding
// open: it has to be counted, it has to receive a shutdown frame on a drain, and
// it has to be closed rather than left for the process exit to reset. Doing this
// only on the first subscribe — which is where it started — made an idle
// connection invisible to [Hub.Rooms] and to [Hub.Shutdown] alike.
//
// It refuses on a draining hub, which is the second of the two checks on that
// path: the handler looks before upgrading, and this looks after, so a client
// that won the race with Shutdown by microseconds is turned away here instead of
// being accepted by an instance that is going down.
func (h *Hub) register(conn *Conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closing {
		return ErrHubClosed
	}

	h.conns[conn] = struct{}{}

	return nil
}

// join adds conn to room, subscribing at the broker if this is the room's first
// local member.
//
// The broker subscription happens *before* the connection is added to the map,
// so a client is never told it is watching a board the transport is not yet
// delivering.
func (h *Hub) join(ctx context.Context, room Room, conn *Conn) error {
	if !room.valid() {
		return fmt.Errorf("realtime: refusing to join an incomplete room %s", room)
	}

	h.mu.RLock()
	closing := h.closing
	h.mu.RUnlock()

	if closing {
		return ErrHubClosed
	}

	h.subMu.Lock()

	if h.subs[room] == 0 {
		if err := h.cfg.Broker.Subscribe(ctx, room); err != nil {
			h.subMu.Unlock()

			return err
		}
	}

	h.subs[room]++
	h.subMu.Unlock()

	h.mu.Lock()

	if h.rooms[room] == nil {
		h.rooms[room] = map[*Conn]struct{}{}
	}

	h.rooms[room][conn] = struct{}{}
	h.mu.Unlock()

	return nil
}

// leave removes conn from room, unsubscribing at the broker if it was the last
// local member.
func (h *Hub) leave(ctx context.Context, room Room, conn *Conn) {
	h.mu.Lock()

	members, ok := h.rooms[room]
	if !ok {
		h.mu.Unlock()

		return
	}

	if _, member := members[conn]; !member {
		h.mu.Unlock()

		return
	}

	delete(members, conn)

	if len(members) == 0 {
		delete(h.rooms, room)
	}

	h.mu.Unlock()

	h.subMu.Lock()

	h.subs[room]--
	last := h.subs[room] <= 0

	if last {
		delete(h.subs, room)
	}

	h.subMu.Unlock()

	if !last {
		return
	}

	if err := h.cfg.Broker.Unsubscribe(ctx, room); err != nil && !errors.Is(err, ErrBrokerClosed) {
		// Not fatal to anything: the room has no local members, so a stale
		// subscription costs one instance some inbound traffic it drops, and
		// go-redis drops the subscription outright on its next reconnect.
		h.logger.Warn("unsubscribing from a realtime room failed",
			slog.String("event", "realtime.room.unsubscribe_failed"),
			slog.String("room", room.String()),
			slog.Any("error", err),
		)
	}
}

// remove deregisters a connection from every room it holds and from the hub.
//
// Called from exactly one place — the deferred cleanup in [Conn.serve] — so
// that "the connection ended" and "the hub forgot about it" cannot come apart,
// whichever way the connection ended.
func (h *Hub) remove(ctx context.Context, conn *Conn) {
	for _, room := range conn.roomList() {
		h.leave(ctx, room, conn)
	}

	conn.forgetRooms()

	h.mu.Lock()
	delete(h.conns, conn)
	h.mu.Unlock()
}

// Shutdown drains the hub: no new connections, a shutdown frame and a Going
// Away close to every live one, then the broker.
//
// # Why a frame and not just a close
//
// A WebSocket close is a fact, not an instruction. Every browser client will
// reconnect, and if a rolling deploy closes ten thousand connections at once
// they all reconnect at once, into a fleet that is one instance short. The
// frame carries a jittered delay so the herd arrives spread out, and it names
// the reason so a client can distinguish "the server is restarting, come back"
// from "your token expired, refresh first". See README.md.
//
// Note that this has to be called explicitly: http.Server.Shutdown does not
// touch hijacked connections, which is what every WebSocket is. cmd/api calls
// it before draining the HTTP server.
func (h *Hub) Shutdown(ctx context.Context) error {
	var err error

	h.shutdownOnce.Do(func() {
		err = h.shutdown(ctx)
	})

	return err
}

func (h *Hub) shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	live := slices.Collect(maps.Keys(h.conns))
	h.mu.Unlock()

	h.logger.Info("realtime hub draining",
		slog.String("event", "realtime.hub.draining"),
		slog.Int("connections", len(live)),
	)

	// Two passes with one pause between them, rather than
	// notify-pause-close per connection. The pause exists so a write pump gets
	// the frame onto the wire before the close handshake overtakes it, and it is
	// the same pause for everybody: doing it per connection would make a drain
	// take (connections × grace), which at ten thousand sockets is minutes
	// rather than the milliseconds it costs here.
	for _, conn := range live {
		conn.notifyShutdown(h.reconnectHint())
	}

	if len(live) > 0 {
		timer := time.NewTimer(shutdownFlushGrace)

		select {
		case <-timer.C:
		case <-ctx.Done():
		}

		timer.Stop()
	}

	for _, conn := range live {
		conn.close(closeGoingAway, "server is restarting")
	}

	// Waits for every connection's goroutines to finish, so the process does
	// not exit with sockets half-closed and clients unsure whether they were
	// dropped or the network broke.
	for _, conn := range live {
		select {
		case <-conn.finished:
		case <-ctx.Done():
			h.logger.Warn("realtime hub drain timed out",
				slog.String("event", "realtime.hub.drain_timeout"),
				slog.Any("error", ctx.Err()),
			)

			return h.closeBroker()
		}
	}

	return h.closeBroker()
}

func (h *Hub) closeBroker() error {
	if err := h.cfg.Broker.Close(); err != nil {
		return err
	}

	<-h.dispatchDone

	h.logger.Info("realtime hub stopped", slog.String("event", "realtime.hub.stopped"))

	return nil
}

// reconnectHint jitters the reconnect delay over [hint, 2*hint).
//
// math/rand/v2 rather than crypto/rand: this is load spreading, and an
// adversary who can predict their own reconnect delay gains nothing they could
// not get by ignoring the hint entirely.
func (h *Hub) reconnectHint() time.Duration {
	base := h.cfg.ShutdownReconnectHint

	return base + time.Duration(rand.Int64N(int64(base))) //nolint:gosec // load spreading, not a secret
}

// Rooms reports how many rooms have at least one local member, and how many
// connections this instance is serving. For the metrics endpoint in #12 and for
// tests that need to assert deregistration actually happened.
func (h *Hub) Rooms() (rooms, connections int) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return len(h.rooms), len(h.conns)
}

// connectionsFor is the test seam for "who is in this room". Unexported: the
// answer is only meaningful inside a lock, so handing it out would be handing
// out a snapshot that is wrong by the time it is read.
func (h *Hub) connectionsFor(room Room) int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return len(h.rooms[room])
}

// principalRoom builds the room a principal means when it names a board. The
// tenant half never comes from the frame — this function is the reason that is
// structurally true rather than a rule to remember.
func principalRoom(principal auth.Principal, boardID uuid.UUID) Room {
	return Room{TenantID: principal.TenantID, BoardID: boardID}
}
