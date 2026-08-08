// Package realtime is the WebSocket hub and its Redis pub/sub fan-out.
//
// # The shape
//
//	browser ──ws──▶ instance A ──┐                ┌──▶ instance A ──ws──▶ browser
//	                             ├─▶ Redis PUBLISH┤
//	browser ──ws──▶ instance B ──┘   (one channel ├──▶ instance B ──ws──▶ browser
//	                                  per room)   └──▶ instance C (no members: SUBSCRIBE never issued)
//
// A [Hub] holds the connections one process is serving, grouped by [Room] —
// a (tenant, board) pair. A [Broker] carries events between processes. Nothing
// is delivered locally at publish time: an event goes to Redis and comes back
// through this instance's own subscription, so every client on every instance
// sees the same bytes down the same code path. One path is easier to reason
// about than two, and it means the two-instance behaviour is exercised by the
// single-instance tests rather than being a separate mode that only production
// runs. The cost is a Redis round trip on the local hop, measured in
// realtime_integration_test.go and well inside the ~200 ms budget.
//
// # Authorization
//
// The upgrade runs behind the same requireAuth middleware as every other
// authenticated route, and the tenant comes from the verified "org" claim via
// [github.com/AndyV99/collabboard/apps/api/internal/api.PrincipalFromContext]
// — the same rule as ADR 0001 and issue #8. A client never names a tenant.
//
// A board id *is* client input, so subscribing is authorized: see
// [StoreAuthorizer]. bola_test.go attacks that the way auth_bola_test.go
// attacks the REST surface, including a deliberately vulnerable authorizer that
// only validates the id's format, to show the assertions can fail.
//
// # Lifetime
//
// A WebSocket is authorized once and then lives for minutes, so this package
// treats "still authorized" as a recurring question rather than a settled one:
// every subscription is re-checked on an interval, and a connection is closed
// when the access token that authorized it expires. See conn.go.
//
// # What the hub is not
//
// It is not a log. Redis pub/sub is at-most-once and holds nothing, so an event
// published while a client is disconnected is gone. That is deliberate:
// Postgres is the source of truth, a client re-fetches the board on connect,
// and the stream is a latency optimisation over polling rather than a
// replication channel. It is also what makes an instance restart cheap — see
// [Hub.Shutdown] and README.md.
package realtime

import (
	"time"

	"github.com/coder/websocket"
)

// Subprotocol is the WebSocket subprotocol this service speaks. It is
// negotiated on every upgrade, so a client that asks for something else is
// refused by the handshake rather than by the first frame it does not
// understand.
const Subprotocol = "collabboard.v1"

// Close codes in the private 4000-4999 range, which RFC 6455 reserves for
// application use.
//
// They exist so a client can tell the three "reconnect" cases apart, because
// the correct client behaviour differs for each: refresh the token, reconnect
// immediately and re-fetch, or back off. A single 1011 would collapse all three
// into "something went wrong", and a client that cannot distinguish them
// reconnects in a hot loop against a server that is refusing it on purpose.
const (
	// StatusTokenExpired means the access token that authorized this connection
	// has reached its exp. The client should refresh and reconnect.
	StatusTokenExpired websocket.StatusCode = 4001

	// StatusSlowConsumer means this connection's send buffer filled up and the
	// server dropped it rather than stall the board's other clients. The client
	// should reconnect and re-fetch the board, because it has missed events.
	StatusSlowConsumer websocket.StatusCode = 4002

	// StatusMembershipRevoked means the subject is no longer a member of the
	// organization the connection was authorized for.
	StatusMembershipRevoked websocket.StatusCode = 4003
)

// Defaults for [HubConfig]. Every one of them is a tuning knob rather than a
// correctness requirement, which is why they can have defaults at all.
const (
	// DefaultSendBuffer is how many frames may be queued for one connection
	// before it is considered slow and dropped. See [HubConfig.SendBuffer].
	DefaultSendBuffer = 64

	// DefaultPingInterval is how often the server pings an idle connection.
	// Comfortably under the 60s idle timeout an AWS ALB applies by default, so
	// a quiet board does not get its connections reaped by the load balancer.
	DefaultPingInterval = 25 * time.Second

	// DefaultPongTimeout is how long a pong may take before the connection is
	// considered dead. This is the dead-connection reaper: a peer that has
	// vanished without a FIN — the laptop-lid case — is detected here and
	// nowhere else.
	DefaultPongTimeout = 10 * time.Second

	// DefaultWriteTimeout bounds a single frame write. A client whose TCP
	// receive window is full blocks the write; this is what stops that
	// connection's write pump from blocking forever. It does not affect other
	// connections, which is the point of the per-connection buffer.
	DefaultWriteTimeout = 5 * time.Second

	// DefaultReadLimit caps an inbound frame. Client frames are a type and a
	// uuid, so this is three orders of magnitude of headroom, and it stops a
	// client from making the server allocate on demand.
	DefaultReadLimit int64 = 32 << 10

	// DefaultReauthorizeInterval is how often every live subscription is
	// re-checked against the database. This is the bound on how long a revoked
	// membership keeps receiving events.
	DefaultReauthorizeInterval = 30 * time.Second

	// DefaultMaxRoomsPerConnection caps how many boards one socket may watch.
	// A user has one board open; a handful covers tabs. Unbounded, it is a
	// cheap way to make the server hold state on a client's say-so.
	DefaultMaxRoomsPerConnection = 16

	// DefaultBrokerBuffer is how many inbound pub/sub messages may queue before
	// the broker's reader blocks. The hub's dispatch loop only ever does
	// non-blocking channel sends, so this queue drains at memory speed.
	DefaultBrokerBuffer = 256
)
