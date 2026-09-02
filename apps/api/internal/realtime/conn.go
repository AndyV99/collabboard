package realtime

// One connection: three goroutines, one buffer, and a close that can be
// triggered from any of them — or from the hub, which is the case the design
// turns on.
//
// # The goroutines
//
//	readPump    decodes client frames. Owns the socket's read side entirely,
//	            because coder/websocket forbids concurrent reads.
//	writePump   drains the send buffer onto the socket, and sends the close
//	            frame on its way out. The only writer, full stop — see its
//	            comment for why the close handshake cannot live anywhere else.
//	maintain    pings, reaps, re-authorizes, and enforces the token deadline.
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

// authorizeTimeout bounds one call into the [Authorizer].
//
// It is not a tuning knob, it is what keeps a promise. The maintenance loop is
// one goroutine running three timers, and the token-expiry deadline is one of
// them — so an authorization call that blocked indefinitely on an unreachable
// database would hold that goroutine and let a connection outlive the token
// that opened it, which is the one thing this package says cannot happen. The
// same bound on the subscribe path stops a stalled database from parking the
// read pump forever on a frame the client can never be answered about.
//
// Generous against a healthy Postgres (the query is one indexed row) and short
// against an unhealthy one. A timeout is not [ErrForbidden], so it fails closed
// for a new subscription and leaves existing ones alone — see [Conn.reauthorize].
const authorizeTimeout = 5 * time.Second

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

	// Before anything else, so that an idle connection — one that has not named
	// a board yet — is still counted, still drained and still closed.
	if err := c.hub.register(c); err != nil {
		_ = c.ws.Close(closeGoingAway, "server is restarting")

		return
	}

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

	wg.Add(2)

	go func() { defer wg.Done(); c.writePump(ctx) }()
	go func() { defer wg.Done(); c.maintain(ctx) }()

	c.logger.InfoContext(ctx, "realtime connection opened",
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

	// Backstop. If the write pump's handshake completed this is a no-op; if it
	// did not, this is what actually releases the file descriptor.
	_ = c.ws.CloseNow()

	status, reason := c.closeInfo()

	c.logger.InfoContext(ctx, "realtime connection closed",
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
			c.logReadEnd(ctx, err)

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
func (c *Conn) logReadEnd(ctx context.Context, err error) {
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

	c.logger.InfoContext(ctx, "realtime connection read ended",
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
		c.sendFrame(ctx, Frame{Type: FramePong})
	default:
		c.sendFrame(ctx, Frame{
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
		c.sendFrame(ctx, Frame{Type: FrameError, Reason: ReasonInvalid, Message: messageBoardIDNotUUID})

		return
	}

	room := principalRoom(c.principal, boardID)

	if c.holdsRoom(room) {
		c.sendFrame(ctx, Frame{Type: FrameSubscribed, BoardID: &boardID})

		return
	}

	if c.roomCount() >= c.hub.cfg.MaxRoomsPerConnection {
		c.sendFrame(ctx, Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonTooManyRooms,
			Message: "unsubscribe from a board before watching another"})

		return
	}

	// The check, before anything is registered. Every non-nil error is a
	// refusal, including the ones that are not ErrForbidden: an authorizer that
	// could not reach the database has not said yes.
	if err := c.authorizeBoard(ctx, boardID); err != nil {
		c.refuseSubscription(ctx, boardID, err)

		return
	}

	if err := c.hub.join(ctx, room, c); err != nil {
		if errors.Is(err, ErrHubClosed) {
			c.sendFrame(ctx, Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonUnavailable,
				Message: "this instance is restarting; reconnect"})

			return
		}

		c.logger.ErrorContext(ctx, "joining a realtime room failed",
			slog.String("event", "realtime.room.join_failed"),
			slog.String("room", room.String()),
			slog.Any("error", err),
		)

		c.sendFrame(ctx, Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonUnavailable,
			Message: "could not subscribe; try again"})

		return
	}

	c.rememberRoom(room)

	c.logger.InfoContext(ctx, "realtime subscription opened",
		slog.String("event", "realtime.subscription.opened"),
		slog.String("board_id", boardID.String()),
	)

	c.sendFrame(ctx, Frame{Type: FrameSubscribed, BoardID: &boardID})
}

// authorizeBoard and authorizeTenant are the only two calls into the
// [Authorizer], and both are bounded — see [authorizeTimeout].
func (c *Conn) authorizeBoard(ctx context.Context, boardID uuid.UUID) error {
	authCtx, cancel := context.WithTimeout(ctx, authorizeTimeout)
	defer cancel()

	return c.hub.cfg.Authorizer.AuthorizeBoard(authCtx, c.principal, boardID)
}

func (c *Conn) authorizeTenant(ctx context.Context) error {
	authCtx, cancel := context.WithTimeout(ctx, authorizeTimeout)
	defer cancel()

	return c.hub.cfg.Authorizer.AuthorizeTenant(authCtx, c.principal)
}

// refuseSubscription answers a failed authorization.
//
// ErrForbidden and "the database was unreachable" get different reasons,
// because the correct client behaviour differs — one is permanent, the other is
// worth retrying — but neither discloses anything about the board. A refusal
// looks identical for a board in another tenant and a board that does not
// exist.
func (c *Conn) refuseSubscription(ctx context.Context, boardID uuid.UUID, err error) {
	if errors.Is(err, ErrForbidden) {
		c.logger.InfoContext(ctx, "refused a realtime subscription",
			slog.String("event", "realtime.subscription.refused"),
			slog.String("board_id", boardID.String()),
		)

		c.sendFrame(ctx, Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonForbidden,
			Message: "not authorized for that board"})

		return
	}

	c.logger.ErrorContext(ctx, "authorizing a realtime subscription failed",
		slog.String("event", "realtime.subscription.authorize_failed"),
		slog.String("board_id", boardID.String()),
		slog.Any("error", err),
	)

	c.sendFrame(ctx, Frame{Type: FrameError, BoardID: &boardID, Reason: ReasonUnavailable,
		Message: "could not check access; try again"})
}

func (c *Conn) handleUnsubscribe(ctx context.Context, frame clientFrame) {
	boardID, ok := parseBoardID(frame.BoardID)
	if !ok {
		c.sendFrame(ctx, Frame{Type: FrameError, Reason: ReasonInvalid, Message: messageBoardIDNotUUID})

		return
	}

	room := principalRoom(c.principal, boardID)

	c.hub.leave(ctx, room, c)
	c.forgetRoom(room)

	c.sendFrame(ctx, Frame{Type: FrameUnsubscribed, BoardID: &boardID})
}

// writePump owns every write on this socket, data frames and the close frame
// alike.
//
// # Why the close handshake belongs here and not in a goroutine of its own
//
// coder/websocket serialises writes behind one internal lock, and a data frame
// bound for a peer that has stopped reading holds that lock until it completes
// or hits WriteTimeout. A close handshake attempted from any other goroutine
// therefore has to wait for that frame — and the library's own five-second
// budget for sending a close frame runs concurrently with our write timeout, so
// the two race. The loser is the close frame, and the client gets a reset
// connection instead of the status code that tells it what to do about it. That
// is exactly the case where the status matters most: 4002 is how a dropped slow
// client learns it has missed events and must re-fetch the board.
//
// Cancelling the in-flight write to free the lock does not help and actively
// hurts: coder/websocket treats a cancelled write context as unrecoverable —
// a half-written frame cannot be un-sent — and closes the connection, which
// destroys the very close frame we were trying to make room for.
//
// Doing both here removes the race rather than tuning it. The close is simply
// the next thing this goroutine does after the frame in flight finishes.
//
// The honest limit: a peer that never reads again cannot be told anything, on
// this socket or any other. It gets a reset after WriteTimeout, and the ping
// reaper is what notices it.
func (c *Conn) writePump(ctx context.Context) {
	defer c.finishHandshake()

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
	//
	// Parented on the serve context and deliberately not cancelled by close:
	// coder/websocket kills the connection when a write context is cancelled,
	// which would take the close frame with it. See writePump.
	writeCtx, cancel := context.WithTimeout(ctx, c.hub.cfg.WriteTimeout)
	defer cancel()

	if err := c.ws.Write(writeCtx, websocket.MessageText, payload); err != nil {
		c.close(closeInternal, "write failed")

		return false
	}

	return true
}

// maintain is keepalive, dead-connection reaping, re-authorization and the
// token deadline, in one goroutine because they are three timers and one of
// them (the ping) does both of the first two.
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
		// Checked before the select, not only as one of its cases. A ping or a
		// re-authorization can block this goroutine for seconds, and select
		// picks uniformly among ready cases — so with a wedged database the
		// expiry could lose that draw repeatedly and a connection would keep
		// running on a token that has already expired. This makes the deadline
		// the first thing considered after anything slow returns.
		if !c.principal.ExpiresAt.IsZero() && c.hub.cfg.now().After(c.principal.ExpiresAt) {
			c.logger.InfoContext(ctx, "closing a realtime connection whose token expired",
				slog.String("event", "realtime.connection.token_expired"))
			c.close(StatusTokenExpired, "access token expired; refresh and reconnect")

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-expiry.C:
			c.logger.InfoContext(ctx, "closing a realtime connection whose token expired",
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
		c.logger.InfoContext(ctx, "reaping an unresponsive realtime connection",
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
	if err := c.authorizeTenant(ctx); err != nil {
		if !errors.Is(err, ErrForbidden) {
			// The database was unreachable. Deliberately *not* a disconnect: an
			// unavailable authorizer would otherwise turn a Postgres blip into
			// a fleet-wide disconnect storm, and the next sweep is 30 seconds
			// away. New subscriptions still fail closed, and the token deadline
			// still caps the connection.
			c.logger.WarnContext(ctx, "re-authorizing a realtime connection failed",
				slog.String("event", "realtime.connection.reauthorize_failed"),
				slog.Any("error", err),
			)

			return true
		}

		c.logger.InfoContext(ctx, "closing a realtime connection whose membership was revoked",
			slog.String("event", "realtime.connection.membership_revoked"))

		c.close(StatusMembershipRevoked, "membership revoked")

		return false
	}

	for _, room := range c.roomList() {
		if err := c.authorizeBoard(ctx, room.BoardID); err == nil {
			continue
		} else if !errors.Is(err, ErrForbidden) {
			continue
		}

		boardID := room.BoardID

		c.logger.InfoContext(ctx, "revoking a realtime subscription",
			slog.String("event", "realtime.subscription.revoked"),
			slog.String("board_id", boardID.String()),
		)

		c.hub.leave(ctx, room, c)
		c.forgetRoom(room)
		c.sendFrame(ctx, Frame{Type: FrameUnsubscribed, BoardID: &boardID, Reason: ReasonForbidden})
	}

	return true
}

// finishHandshake sends the close frame and waits for the peer's answer.
//
// Called from the write pump's defer, so it runs exactly once and never
// concurrently with a data frame. It is also what unblocks readPump: coder's
// Close writes the close frame first and only then waits for the peer's, which
// needs the read lock readPump is holding — so the client learns the reason
// immediately, and the wait resolves as soon as readPump sees the answering
// close and lets the lock go. The library caps that wait at five seconds if the
// peer never answers.
func (c *Conn) finishHandshake() {
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
func (c *Conn) sendFrame(ctx context.Context, frame Frame) {
	payload, err := frame.encode()
	if err != nil {
		c.logger.ErrorContext(ctx, "encoding a realtime frame failed",
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
// The context is the shutdown's, not a request's -- it carries no request id,
// which is correct: a drain is not something a client asked for.
func (c *Conn) notifyShutdown(ctx context.Context, reconnectAfter time.Duration) {
	c.sendFrame(ctx, Frame{
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
