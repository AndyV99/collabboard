package realtime

// One connection: three goroutines, one buffer, and a close that can be
// triggered from any of them.
//
// # The goroutines
//
//	readPump    decodes client frames. Owns the socket's read side entirely,
//	            because coder/websocket forbids concurrent reads.
//	writePump   drains the send buffer onto the socket. The only writer of
//	            data frames.
//	maintain    pings, reaps, re-authorizes, and enforces the token deadline.
//	closer      parked on c.closed; performs the close handshake so that a
//	            close triggered from the hub's fan-out goroutine never blocks
//	            that goroutine on a slow peer's TCP buffer.
//
// # Why a connection outlives its authorization check, and what is done about it
//
// A request is authorized once and is over in milliseconds. A WebSocket is
// authorized once and lives for as long as someone leaves a tab open. Nothing
// in the access token can be withdrawn, so "still allowed" has to be asked
// again, and this file asks it twice over:
//
//   - Every ReauthorizeInterval (30s by default), the subject's membership is
//     re-checked and so is every board it is watching. A revoked membership
//     closes the connection with [StatusMembershipRevoked]; a board that stops
//     resolving unsubscribes that room and tells the client why.
//   - The connection is closed when the access token's own exp passes, with
//     [StatusTokenExpired]. A socket therefore never outlives the credential
//     that opened it, which caps the exposure of anything the periodic check
//     might miss at the token TTL — 15 minutes by default — rather than at
//     "however long the tab stays open".
//
// The honest summary is in README.md: revocation is eventually consistent with
// a 30-second bound, and the connection is hard-capped by token lifetime. The
// alternative — consulting the database on every delivered event — would put a
// query on the hot path of the thing this package exists to make fast, for a
// window that is already shorter than the access token's.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// Close codes from the standard range, named so the call sites read.
const (
	closeNormal    = websocket.StatusNormalClosure
	closeGoingAway = websocket.StatusGoingAway
	closeInternal  = websocket.StatusInternalError
	closeUnsupport = websocket.StatusUnsupportedData
)

// cleanupTimeout bounds the deregistration that runs after a connection ends.
// It deliberately does not inherit the request context — see [Conn.serve].
const cleanupTimeout = 5 * time.Second

// shutdownFlushGrace is how long Hub.Shutdown lets a shutdown frame reach the
// wire before the close handshake overtakes it.
const shutdownFlushGrace = 50 * time.Millisecond

// Conn is one client's WebSocket, as the hub sees it.
type Conn struct {
	id        uuid.UUID
	hub       *Hub
	ws        *websocket.Conn
	principal auth.Principal
	logger    *slog.Logger

	// send is the backpressure boundary. Everything that fans out to this
	// client does a non-blocking send here and nothing does a blocking one, so
	// no code path anywhere can be slowed down by this client's network.
	send chan []byte

	// closed is closed exactly once, by close. It is the signal every goroutine
	// on this connection watches.
	closed    chan struct{}
	closeOnce sync.Once

	// finished is closed when serve returns, i.e. when every goroutine for this
	// connection has stopped. Hub.Shutdown waits on it.
	finished chan struct{}

	closeMu     sync.Mutex
	closeStatus websocket.StatusCode
	closeReason string

	roomsMu sync.Mutex
	rooms   map[Room]struct{}
}

func newConn(hub *Hub, ws *websocket.Conn, principal auth.Principal) *Conn {
	id := uuid.New()

	return &Conn{
		id:        id,
		hub:       hub,
		ws:        ws,
		principal: principal,
		logger: hub.logger.With(
			slog.String("connection_id", id.String()),
			slog.String("tenant_id", principal.TenantID.String()),
			slog.String("user_id", principal.UserID.String()),
		),
		send:        make(chan []byte, hub.cfg.SendBuffer),
		closed:      make(chan struct{}),
		finished:    make(chan struct{}),
		closeStatus: closeNormal,
		rooms:       map[Room]struct{}{},
	}
}

// serve runs the connection until it ends, then deregisters it.
//
// It blocks, because it is called from the HTTP handler goroutine: the
// connection is hijacked, so that goroutine has nothing else to do and using it
// saves one per client.
func (c *Conn) serve(ctx context.Context) {
	defer close(c.finished)

	// Detached from ctx on purpose, and bounded. By the time this runs the
	// request context is usually already cancelled — that is one of the ways a
	// connection ends — and unsubscribing from Redis with a cancelled context
	// would fail instantly and leave this instance subscribed to a room it has
	// no members in.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		c.hub.remove(cleanupCtx, c)
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.ws.SetReadLimit(c.hub.cfg.ReadLimit)

	var wg sync.WaitGroup

	wg.Add(3)

	go func() { defer wg.Done(); c.writePump(ctx) }()
	go func() { defer wg.Done(); c.maintain(ctx) }()
	go func() { defer wg.Done(); c.closer(ctx) }()

	c.logger.Info("realtime connection opened",
		slog.String("event", "realtime.connection.opened"),
		slog.Time("token_expires_at", c.principal.ExpiresAt),
	)

	c.readPump(ctx)

	// Reached when the peer closed, the socket died, or a frame was refused.
	// Idempotent: if something already decided why this connection is ending,
	// that reason stands.
	c.close(closeNormal, "")

	cancel()
	wg.Wait()

	// Backstop. If the handshake in closer completed this is a no-op; if it
	// did not, this is what actually releases the file descriptor.
	_ = c.ws.CloseNow()

	status, reason := c.closeInfo()

	c.logger.Info("realtime connection closed",
		slog.String("event", "realtime.connection.closed"),
		slog.Int("close_status", int(status)),
		slog.String("close_reason", reason),
	)
}

// readPump owns the read side. It returns on any read error, which includes
// the peer's close frame, a cancelled context and a frame over the read limit.
func (c *Conn) readPump(ctx context.Context) {
	for {
		kind, data, err := c.ws.Read(ctx)
		if err != nil {
			c.logReadEnd(err)

			return
		}

		if kind != websocket.MessageText {
			c.close(closeUnsupport, "this protocol speaks json text frames")

			return
		}

		if !c.handleFrame(ctx, data) {
			return
		}
	}
}

// logReadEnd records why the read side stopped, at a level that reflects
// whether it is worth anybody's attention. A client closing a tab is not.
func (c *Conn) logReadEnd(err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return
	case websocket.CloseStatus(err) != -1:
		return
	}

	select {
	case <-c.closed:
		// The server decided; the read error is the consequence, not news.
		return
	default:
	}

	c.logger.Info("realtime connection read ended",
		slog.String("event", "realtime.connection.read_ended"),
		slog.Any("error", err),
	)
}

// handleFrame processes one client frame. It reports whether the connection
// should keep reading.
//
// A malformed or refused frame is answered with an error frame and the
// connection continues: a client that asks for a board it may not see has made
// a mistake, not an attack, and closing the socket would take its other
// subscriptions down with it. The exception is a frame that is not JSON at all,
// which means the peer is not speaking this protocol.
func (c *Conn) handleFrame(ctx context.Context, data []byte) bool {
	var frame clientFrame

	if err := json.Unmarshal(data, &frame); err != nil {
		c.close(closeUnsupport, "frame is not valid json")

		return false
	}

	switch frame.Type {
	case clientSubscribe:
		c.handleSubscribe(ctx, frame)
	case clientUnsubscribe:
		c.handleUnsubscribe(ctx, frame)
	case clientPing:
		c.sendFrame(Frame{Type: FramePong})
	default:
		c.sendFrame(Frame{
			Type:    FrameError,
			Reason:  ReasonInvalid,
			Message: "unknown frame type",
		})
	}

	return true
}

func (c *Conn) handleSubscribe(ctx context.Context, frame clientFrame) {
	boardID, ok := parseBoardID(frame.BoardID)
	if !ok {
		c.sendFrame(Frame{Type: FrameError, Reason: ReasonInvalid, Message: messageBoardIDNotUUID})

		return
	}

	room := principalRoom(c.principal, boardID)

	if c.holdsRoom(room) {
		c.sendFrame(Frame{Type: FrameSubscribed, BoardID: &boardID})

		return
	}

	if c.roomCount() >= c.hub.cfg.MaxRoomsPerConnection {
		c.sendFrame(Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonTooManyRooms,
			Message: "unsubscribe from a board before watching another"})

		return
	}

	// The check, before anything is registered. Every non-nil error is a
	// refusal, including the ones that are not ErrForbidden: an authorizer that
	// could not reach the database has not said yes.
	if err := c.hub.cfg.Authorizer.AuthorizeBoard(ctx, c.principal, boardID); err != nil {
		c.refuseSubscription(boardID, err)

		return
	}

	if err := c.hub.join(ctx, room, c); err != nil {
		if errors.Is(err, ErrHubClosed) {
			c.sendFrame(Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonUnavailable,
				Message: "this instance is restarting; reconnect"})

			return
		}

		c.logger.Error("joining a realtime room failed",
			slog.String("event", "realtime.room.join_failed"),
			slog.String("room", room.String()),
			slog.Any("error", err),
		)

		c.sendFrame(Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonUnavailable,
			Message: "could not subscribe; try again"})

		return
	}

	c.rememberRoom(room)

	c.logger.Info("realtime subscription opened",
		slog.String("event", "realtime.subscription.opened"),
		slog.String("board_id", boardID.String()),
	)

	c.sendFrame(Frame{Type: FrameSubscribed, BoardID: &boardID})
}

// refuseSubscription answers a failed authorization.
//
// ErrForbidden and "the database was unreachable" get different reasons,
// because the correct client behaviour differs — one is permanent, the other is
// worth retrying — but neither discloses anything about the board. A refusal
// looks identical for a board in another tenant and a board that does not
// exist.
func (c *Conn) refuseSubscription(boardID uuid.UUID, err error) {
	if errors.Is(err, ErrForbidden) {
		c.logger.Info("refused a realtime subscription",
			slog.String("event", "realtime.subscription.refused"),
			slog.String("board_id", boardID.String()),
		)

		c.sendFrame(Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonForbidden,
			Message: "not authorized for that board"})

		return
	}

	c.logger.Error("authorizing a realtime subscription failed",
		slog.String("event", "realtime.subscription.authorize_failed"),
		slog.String("board_id", boardID.String()),
		slog.Any("error", err),
	)

	c.sendFrame(Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonUnavailable,
		Message: "could not check access; try again"})
}

func (c *Conn) handleUnsubscribe(ctx context.Context, frame clientFrame) {
	boardID, ok := parseBoardID(frame.BoardID)
	if !ok {
		c.sendFrame(Frame{Type: FrameError, Reason: ReasonInvalid, Message: messageBoardIDNotUUID})

		return
	}

	room := principalRoom(c.principal, boardID)

	c.hub.leave(ctx, room, c)
	c.forgetRoom(room)

	c.sendFrame(Frame{Type: FrameUnsubscribed, BoardID: &boardID})
}

// writePump is the only writer of data frames.
func (c *Conn) writePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case payload := <-c.send:
			if !c.write(ctx, payload) {
				return
			}
		}
	}
}

func (c *Conn) write(ctx context.Context, payload []byte) bool {
	// Bounded, so a peer whose receive window is full stalls this connection's
	// writer for at most WriteTimeout rather than forever. Its own send buffer
	// fills meanwhile, and the hub drops it — which is the designed outcome.
	writeCtx, cancel := context.WithTimeout(ctx, c.hub.cfg.WriteTimeout)
	defer cancel()

	if err := c.ws.Write(writeCtx, websocket.MessageText, payload); err != nil {
		c.close(closeInternal, "write failed")

		return false
	}

	return true
}

// maintain is keepalive, dead-connection reaping, re-authorization and the
// token deadline, in one goroutine because all four are timers.
func (c *Conn) maintain(ctx context.Context) {
	ping := time.NewTicker(c.hub.cfg.PingInterval)
	defer ping.Stop()

	reauth := time.NewTicker(c.hub.cfg.ReauthorizeInterval)
	defer reauth.Stop()

	// A token with an expiry in the past would fire immediately, which is
	// correct: requireAuth would not have let it through, so this can only be
	// reached by a clock jump.
	expiry := time.NewTimer(time.Until(c.principal.ExpiresAt))
	defer expiry.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-expiry.C:
			c.logger.Info("closing a realtime connection whose token expired",
				slog.String("event", "realtime.connection.token_expired"))
			c.close(StatusTokenExpired, "access token expired; refresh and reconnect")

			return
		case <-ping.C:
			if !c.pingPeer(ctx) {
				return
			}
		case <-reauth.C:
			if !c.reauthorize(ctx) {
				return
			}
		}
	}
}

// pingPeer is the dead-connection reaper.
//
// coder/websocket's Ping sends the frame and waits for the matching pong, so
// this is a liveness probe rather than a hint: a peer that has vanished without
// a TCP FIN — a laptop closed, a NAT entry dropped — fails here and nowhere
// else, because a socket with nothing to write never notices.
func (c *Conn) pingPeer(ctx context.Context) bool {
	pingCtx, cancel := context.WithTimeout(ctx, c.hub.cfg.PongTimeout)
	defer cancel()

	if err := c.ws.Ping(pingCtx); err != nil {
		// Not logged at error: the overwhelmingly common cause is a client that
		// went away, which is ordinary.
		c.logger.Info("reaping an unresponsive realtime connection",
			slog.String("event", "realtime.connection.reaped"),
			slog.Any("error", err),
		)

		c.close(closeGoingAway, "no pong")

		return false
	}

	return true
}

// reauthorize re-checks the membership and every live subscription. It reports
// whether the connection should stay open.
func (c *Conn) reauthorize(ctx context.Context) bool {
	if err := c.hub.cfg.Authorizer.AuthorizeTenant(ctx, c.principal); err != nil {
		if !errors.Is(err, ErrForbidden) {
			// The database was unreachable. Deliberately *not* a disconnect: an
			// unavailable authorizer would otherwise turn a Postgres blip into
			// a fleet-wide disconnect storm, and the next sweep is 30 seconds
			// away. New subscriptions still fail closed, and the token deadline
			// still caps the connection.
			c.logger.Warn("re-authorizing a realtime connection failed",
				slog.String("event", "realtime.connection.reauthorize_failed"),
				slog.Any("error", err),
			)

			return true
		}

		c.logger.Info("closing a realtime connection whose membership was revoked",
			slog.String("event", "realtime.connection.membership_revoked"))

		c.close(StatusMembershipRevoked, "membership revoked")

		return false
	}

	for _, room := range c.roomList() {
		if err := c.hub.cfg.Authorizer.AuthorizeBoard(ctx, c.principal, room.BoardID); err == nil {
			continue
		} else if !errors.Is(err, ErrForbidden) {
			continue
		}

		boardID := room.BoardID

		c.logger.Info("revoking a realtime subscription",
			slog.String("event", "realtime.subscription.revoked"),
			slog.String("board_id", boardID.String()),
		)

		c.hub.leave(ctx, room, c)
		c.forgetRoom(room)
		c.sendFrame(Frame{Type: FrameUnsubscribed, BoardID: &boardID, Reason: ReasonForbidden})
	}

	return true
}

// closer performs the close handshake once something decides the connection is
// over.
//
// It exists so that close itself does no I/O: close is called from the hub's
// fan-out goroutine when a client is too slow, and that goroutine must never
// wait on a socket. It also unblocks readPump, which is otherwise parked in
// Read until the peer says something.
//
// It also has to be a separate goroutine because coder/websocket's Close writes
// the close frame and then waits for the peer's — which needs the read lock
// readPump is holding. The close frame is on the wire before that wait starts,
// so the peer always learns the reason promptly; the wait resolves as soon as
// readPump sees the peer's answering close and lets the lock go, and is capped
// by the library at five seconds if the peer never answers.
func (c *Conn) closer(ctx context.Context) {
	select {
	case <-c.closed:
	case <-ctx.Done():
		// The server is going down or the request context died. Still a
		// graceful close: whatever reason was recorded is more useful to the
		// client than a reset connection.
	}

	status, reason := c.closeInfo()

	_ = c.ws.Close(status, reason)
}

// trySend queues a payload without ever blocking. It reports false when the
// buffer is full, which is the hub's signal to drop this client.
func (c *Conn) trySend(payload []byte) bool {
	select {
	case <-c.closed:
		// Already going away. Not "slow", so the caller must not treat it as a
		// reason to close it again.
		return true
	default:
	}

	select {
	case c.send <- payload:
		return true
	case <-c.closed:
		return true
	default:
		return false
	}
}

// sendFrame queues a server-generated frame. A client too slow to accept its
// own subscription acknowledgement is dropped for the same reason it would be
// dropped for lagging on events.
func (c *Conn) sendFrame(frame Frame) {
	payload, err := frame.encode()
	if err != nil {
		c.logger.Error("encoding a realtime frame failed",
			slog.String("event", "realtime.frame.encode_failed"),
			slog.String("frame_type", frame.Type),
			slog.Any("error", err),
		)

		return
	}

	if !c.trySend(payload) {
		c.close(StatusSlowConsumer, "send buffer full; reconnect and refetch")
	}
}

// notifyShutdown queues the warning frame Hub.Shutdown sends before closing.
//
// It does not wait for the frame to reach the wire. [Hub.Shutdown] pauses once
// for the whole fleet of connections rather than once per connection, because
// per-connection would make a drain cost (connections × grace) — minutes at ten
// thousand sockets, for a pause that is only there to let the write pump get
// ahead of the close handshake.
func (c *Conn) notifyShutdown(reconnectAfter time.Duration) {
	c.sendFrame(Frame{
		Type:             FrameShutdown,
		Message:          "this instance is restarting",
		ReconnectAfterMs: reconnectAfter.Milliseconds(),
	})
}

// close records why the connection is ending and wakes everything watching.
//
// It does no I/O and never blocks, which is what makes it safe to call from the
// hub's fan-out goroutine. The first caller wins: a connection that is already
// closing for one reason does not get relabelled by the consequences of that.
func (c *Conn) close(status websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closeStatus = status
		c.closeReason = reason
		c.closeMu.Unlock()

		close(c.closed)
	})
}

func (c *Conn) closeInfo() (websocket.StatusCode, string) {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	return c.closeStatus, c.closeReason
}

func (c *Conn) rememberRoom(room Room) {
	c.roomsMu.Lock()
	defer c.roomsMu.Unlock()

	c.rooms[room] = struct{}{}
}

func (c *Conn) forgetRoom(room Room) {
	c.roomsMu.Lock()
	defer c.roomsMu.Unlock()

	delete(c.rooms, room)
}

func (c *Conn) forgetRooms() {
	c.roomsMu.Lock()
	defer c.roomsMu.Unlock()

	clear(c.rooms)
}

func (c *Conn) holdsRoom(room Room) bool {
	c.roomsMu.Lock()
	defer c.roomsMu.Unlock()

	_, ok := c.rooms[room]

	return ok
}

func (c *Conn) roomCount() int {
	c.roomsMu.Lock()
	defer c.roomsMu.Unlock()

	return len(c.rooms)
}

// roomList is a snapshot, so callers can iterate without holding the lock over
// a hub call that takes its own.
func (c *Conn) roomList() []Room {
	c.roomsMu.Lock()
	defer c.roomsMu.Unlock()

	rooms := make([]Room, 0, len(c.rooms))
	for room := range c.rooms {
		rooms = append(rooms, room)
	}

	return rooms
}

// parseBoardID rejects everything that is not a real uuid, including the zero
// one — which parses fine and matches no board, so accepting it would create a
// room nobody can be authorized for.
func parseBoardID(raw string) (uuid.UUID, bool) {
	boardID, err := uuid.Parse(raw)
	if err != nil || boardID == uuid.Nil {
		return uuid.Nil, false
	}

	return boardID, true
}
