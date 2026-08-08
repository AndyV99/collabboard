# internal/realtime

The WebSocket hub and its Redis pub/sub fan-out. Issue #9.

```
browser ──ws──▶ instance A ──┐                 ┌──▶ instance A ──ws──▶ browser
                             ├─▶ Redis PUBLISH ┤
browser ──ws──▶ instance B ──┘  (one channel   ├──▶ instance B ──ws──▶ browser
                                 per room)     └──▶ instance C (no members: never SUBSCRIBEd)
```

## The protocol

`GET /api/v1/ws`, behind the same `requireAuth` middleware as every other
authenticated route. Subprotocol `collabboard.v1`. JSON text frames.

Client → server:

```json
{"type": "subscribe",   "board_id": "<uuid>"}
{"type": "unsubscribe", "board_id": "<uuid>"}
{"type": "ping"}
```

Server → client:

```json
{"type": "subscribed",   "board_id": "<uuid>"}
{"type": "unsubscribed", "board_id": "<uuid>", "reason": "forbidden"}
{"type": "event",        "board_id": "<uuid>", "event": {…}}
{"type": "error",        "reason": "forbidden|invalid_request|too_many_subscriptions|unavailable"}
{"type": "shutdown",     "reconnect_after_ms": 1400}
{"type": "pong"}
```

There is **no tenant field in either direction**. A client cannot name an
organization, and a server frame never mentions one.

Close codes in the private range: `4001` token expired, `4002` slow consumer,
`4003` membership revoked. `1001` is a normal instance restart.

## Authentication and authorization

**Authentication** is the same JWT path as REST: same middleware, same issuer,
same signature check. The tenant is the verified `org` claim, read through
`api.PrincipalFromContext`, and nothing merges request data into it — the rule
ADR 0001 and issue #8 established, unchanged.

A browser cannot set request headers on a WebSocket handshake, so the token may
also arrive as a `Sec-WebSocket-Protocol` entry prefixed `bearer.collabboard.v1.`
— a header rather than a query parameter, because a query parameter is a
credential in an access log by construction. `internal/api/realtime.go` lifts it
into `Authorization` and `requireAuth` verifies it identically. It cannot
displace an `Authorization` header that is already present.

**Authorization** is where this surface differs from REST: a board id is client
input, on a connection that is authorized once and then lives for minutes. Two
independent things stop a cross-tenant subscription, and `bola_test.go` tests
each with the other neutralised:

1. `StoreAuthorizer` refuses unless the subject holds a membership in the token's
   tenant **and** the board resolves inside it — one tenant-scoped transaction,
   RLS supplying the tenant predicate. A board in another organization comes back
   as no row, so it is refused exactly as a board that does not exist is: the
   endpoint is not an existence oracle.
2. A room is keyed on `(tenant, board)`, not on `board`. Even a subscription that
   should never have been granted lands in the caller's own tenant's room.

Membership is per organization in this schema, not per board, so "a board in
your own tenant you have no membership for" is not a state the data model can be
in. When per-board ACLs exist, `StoreAuthorizer.AuthorizeBoard` is the one
function that has to learn about them.

## Membership revocation on a live connection

Two bounds, and the second is the one worth quoting:

- **Eventually consistent, 30 seconds.** Every `REALTIME_REAUTHORIZE_INTERVAL`
  the connection re-checks the subject's membership and every board it is
  watching. A revoked membership closes the socket (`4003`); a board that stops
  resolving unsubscribes that room and tells the client why.
- **Hard-capped by the token.** The connection is closed when the access token's
  own `exp` passes (`4001`). **A socket never outlives the credential that opened
  it**, so anything the sweep might miss is bounded by `AUTH_ACCESS_TOKEN_TTL`
  (15 minutes) rather than by how long a tab stays open.

An authorizer that *errors* — Postgres unreachable — does not disconnect anyone.
A database blip would otherwise become a fleet-wide reconnect storm on top of an
already-unhealthy database. New subscriptions still fail closed, and the token
deadline still applies.

Every call into the authorizer is bounded at five seconds, and the maintenance
loop checks the token deadline *before* its select rather than only as one of
its cases. Both exist for the same reason: that loop runs the ping, the sweep
and the deadline in one goroutine, so a database that accepts a connection and
then says nothing would otherwise hold all three and let a socket outlive its
token — the one thing this design says cannot happen.
`TestAWedgedAuthorizerCannotOutlastTheToken` uses an authorizer that only ever
answers when its context is cancelled; without the bound it would hang rather
than fail.

The rejected alternative was re-checking on every delivered event: a query on the
hot path of the thing this package exists to make fast, for a window already
shorter than the token's.

## Backpressure: the slow client is dropped

Each connection has a bounded send buffer (`REALTIME_SEND_BUFFER`, 64). Fan-out
does one **non-blocking** send per connection while holding a read lock, and
never touches a socket. When a buffer is full the connection is closed with
`4002`.

Why dropping the client rather than the message, and why never blocking:

- **Blocking** would make one stalled TCP connection the whole board's problem.
  It is the failure this design exists to prevent, so it is not an option.
- **Dropping messages** leaves the client with a board that silently disagrees
  with the server and no way to notice. Nothing in the stream would tell it that
  a card move went missing.
- **Dropping the client** is the only outcome that is both safe and *visible*.
  The client sees `4002`, reconnects, and re-fetches the board from Postgres,
  which is the same recovery path it already has for a dropped network.

Two tests, split on purpose. `TestAFullSendBufferDropsTheClientAndNothingElse`
drives the hub directly, with one connection draining its buffer and one not:
the stalled one is closed with `4002` and the other receives every event. It
touches no socket, because whether a *real* peer's buffers fill depends on the
machine's `net.ipv4.tcp_wmem` — a test that waits for TCP to push back is a test
the kernel decides, and CI proved that by failing it in both directions on the
same code.

`TestASlowClientIsDroppedWithoutStallingOthers` then demonstrates the same thing
end to end over real sockets: a connection that never reads (~1,800 frames,
~30 MB before the server gives up) while another client on the same board
receives every one of them and keeps receiving afterwards. It deliberately does
*not* assert which of the two mechanisms fired — buffer overflow or write
timeout — because that is the machine-dependent part. Both are "the hub gave up
on this peer rather than waiting for it".

One thing worth knowing about the `4002` frame itself. The write pump owns
*every* write on a socket, the close frame included, because coder/websocket
serialises writes behind one lock: a close attempted from another goroutine
waits for whatever data frame is in flight, and the library's own five-second
budget for a close frame then races our write timeout. The loser is the close
frame, and the client gets a reset connection instead of the code that tells it
to re-fetch — precisely when the code matters most. Cancelling the in-flight
write to free the lock is worse, not better: coder treats a cancelled write
context as unrecoverable and closes the connection, destroying the frame we were
making room for. So the close is simply the next thing the write pump does.

The honest limit: **a peer that never reads again cannot be told anything**, on
this socket or any other. It gets a reset once `REALTIME_WRITE_TIMEOUT` expires,
and the ping reaper is what notices it. A client that was merely slow — the
common case — resumes reading, the queued frame drains, and `4002` follows it.

## Instance restart

`http.Server.Shutdown` does not close or wait for hijacked connections, and every
WebSocket is one — so a naive drain returns "done" with every socket still open,
and the process exit resets them. `cmd/api` therefore calls `Hub.Shutdown` first:

1. New upgrades get `503` with `Retry-After`, so a load balancer sends clients to
   a healthy instance instead of this one.
2. Every live connection gets a `shutdown` frame carrying a **jittered**
   `reconnect_after_ms` (1–2s). A close is a fact; a frame is an instruction. Ten
   thousand clients closed at once reconnect at once, into a fleet that is one
   instance short — the jitter is what spreads that.
3. Then a `1001 Going Away` close, and a bounded wait for every connection's
   goroutines.
4. Then the Redis subscription and the HTTP server.

**Nothing is replayed.** Redis pub/sub is at-most-once and holds nothing, so an
event published while a client is between instances is gone. That is deliberate:
Postgres is the source of truth, a client re-fetches the board when it
subscribes, and the stream is a latency optimisation over polling rather than a
replication channel. It is also what makes a restart cheap — the hub holds no
state that is not recoverable from a reconnect.

## Fan-out has one path, not two

`Hub.Publish` does **not** deliver locally. The event goes to Redis and comes
back through this instance's own subscription, so a client on the publishing
instance and a client three instances away receive the same bytes down the same
code path, in the same order Redis chose.

The cost is a Redis round trip on the local hop; the measured p50 for the whole
path — authenticated HTTP request, board authorization against Postgres, PUBLISH,
another instance's fan-out, WebSocket frame at the client — is **1.5 ms**,
against a 200 ms target. The benefit is that the two-instance behaviour is
exercised by every single-instance test rather than being a mode only production
runs.

`RedisBroker.Subscribe` waits for Redis to *confirm* the subscription rather than
returning when the command was written. go-redis sends SUBSCRIBE on the pub/sub
connection and PUBLISH on a pooled one, and Redis promises no ordering between
two connections — so without the confirmation there is a small, load-dependent
window where a client is told it is watching a board and the next event goes past
it.

Every message carries its room inside the envelope as well as in the channel
name, and a receiver drops (and loudly logs) any mismatch. Redis routes by
channel and there is no reason to expect it to lie; the point is that a
misrouting has to be *detectable* rather than assumed away, because a delivered
mismatch is a cross-tenant leak.

## Library: coder/websocket

Over `gorilla/websocket`, for two reasons that this design actually leans on:

- **Context-native.** `Read(ctx)`, `Write(ctx)`, `Ping(ctx)` take contexts
  instead of absolute deadlines, so shutdown cancellation and per-write timeouts
  are the same mechanism as everywhere else in this codebase rather than a
  parallel one built on `SetWriteDeadline`.
- **`Ping` waits for the pong.** Dead-connection reaping is one call with a
  timeout, instead of a pong handler, a shared timestamp and a separate ticker
  reading it. Fewer moving parts in the one place that has to be right for a
  connection nobody is writing to.

It also has no dependencies and a smaller surface. gorilla is the more widely
known choice and would work; nothing here needs its extra knobs.

## Configuration

All optional; defaults in `internal/config`.

| Variable | Default | Notes |
|---|---|---|
| `REALTIME_ALLOWED_ORIGINS` | dev: `localhost:3000,127.0.0.1:3000`; otherwise **empty** | Empty means same-origin only. Defence in depth — the credential is a bearer token, not an ambient cookie. |
| `REALTIME_SEND_BUFFER` | 64 | Frames queued per connection before it is dropped. |
| `REALTIME_PING_INTERVAL` | 25s | Under the 60s default ALB idle timeout. |
| `REALTIME_PONG_TIMEOUT` | 10s | The reaper. |
| `REALTIME_WRITE_TIMEOUT` | 5s | Bounds one frame write. |
| `REALTIME_READ_LIMIT_BYTES` | 32768 | Caps an inbound frame. |
| `REALTIME_REAUTHORIZE_INTERVAL` | 30s | The revocation bound. |
| `REALTIME_MAX_BOARDS_PER_CONNECTION` | 16 | |
| `REALTIME_BROKER_BUFFER` | 256 | Inbound pub/sub queue. |
| `REALTIME_SHUTDOWN_RECONNECT_HINT` | 1s | Jittered to [1s, 2s). |

## Tests

```bash
go test ./internal/realtime/... -race                    # ~6s, no Docker
go test -tags=integration ./internal/realtime/... -count=1  # ~10s, Postgres + Redis
```

| File | Tag | What it covers |
|---|---|---|
| `hub_test.go` | — | fan-out, board isolation, deregistration, backpressure, reaping, token expiry, revocation, shutdown, upgrade auth |
| `bola_test.go` | — | cross-tenant subscription and cross-tenant *writes*, plus three "has teeth" tests |
| `unit_test.go` | — | addressing, wiring checks, the misrouting guard, jitter |
| `publisher_test.go` | — | the write path's adapter: room addressing, the payload cap, incomplete rooms |
| `harness_test.go` | — | the real router + real WebSockets over a memory bus and an RLS-modelling fake |
| `realtime_integration_test.go` | `integration` | two instances over real Redis, authorization against live RLS, revocation in the database, latency, restart handover, rollback, ordering |

Every event in every one of those tests exists because a card or column write
committed. Nothing publishes directly except `publisher_test.go`, which is
testing the adapter itself, and the two tests that deliberately drive
`Hub.Publish` to model a mistake — see `bola_test.go`'s
`TestTheRoomKeyIsWhatKeepsTenantsApart`.

The unit tests run against a `MemoryBus` and a fake store; the integration tests
run the same code against real Redis and real policies. The fake could be wrong
about what the policies say — which is exactly why both exist.

## What publishes, and when

Nothing in this package publishes. The write path does, through
`Hub.EventPublisher()` — an adapter satisfying `api.EventPublisher`, which
`internal/api` declares in terms of its own types so the import keeps running
`realtime -> api`. See `publisher.go` and `internal/api/events.go`.

> `POST /api/v1/boards/:board_id/events` is **gone** (#45). It existed only to
> demonstrate fan-out before anything could move a card, and it let a client
> announce a move that never happened. It answers 404.

**The publish happens after the transaction commits and before the HTTP response
is written.** Both halves matter, and both are argued in
[ADR 0005](../../../../docs/adr/0005-realtime-event-delivery.md):

- *After the commit*, because an event published from inside a transaction that
  then rolls back is a change every browser shows and the database never made.
  `internal/api` enforces this structurally — the transaction callback is handed
  a `store.Querier` and no publisher, so there is nothing to publish *with* until
  `WithTenant` has returned.
- *Before the response*, because that is what makes a client's own writes
  causally ordered: a second move issued after the first one's response was
  acknowledged is published second, at every client.

A publish that fails **does not fail the write**. It is logged as
`realtime.publish.failed`, and the client recovers the way it already recovers
from a reconnect: by re-fetching on `subscribed`.

### Events

Event names are a wire contract. A `*.deleted` payload carries ids only; a
`*.moved` payload carries the anchor with an explicit `null` for "first"; every
other payload carries the same object body the REST endpoints return, so a client
has one decoder rather than two.

| Type | Payload |
|---|---|
| `card.created` | `{"card": {…}}` — appended to the end of its column |
| `card.updated` | `{"card": {…}}` — the whole card, not a field diff |
| `card.moved` | `{"card": {…}, "after_card_id": "<uuid>"\|null}` |
| `card.deleted` | `{"card_id": "…", "column_id": "…"}` |
| `column.created` | `{"column": {…}}` — appended to the end of the board |
| `column.updated` | `{"column": {…}}` |
| `column.moved` | `{"column": {…}, "after_column_id": "<uuid>"\|null}` |
| `column.deleted` | `{"column_id": "…"}` — its cards go with it, no per-card event |
| `board.updated` | `{"board": {…}}` |
| `board.deleted` | `{"board_id": "…"}` — the last event the room will carry |

No rank is ever published — see [ADR 0004](../../../../docs/adr/0004-card-ordering.md).
Project writes and board *creation* publish nothing: a room is a board, so at the
moment a board is created there is nobody who could be watching it.

### Ordering, exactly

**Guaranteed.** One total order per board, identical at every client on every
instance — one Redis channel per room, one dispatch goroutine per instance,
per-connection FIFO buffers. And causal order for one client's own writes,
because the publish precedes the response.

**Not guaranteed.** That publish order matches commit order for two *concurrent*
writers. Their transactions serialise in Postgres, but each request publishes
from its own goroutine afterwards, and that race is narrow rather than closed.
Every client still agrees with every other client; the disagreement with the
database is corrected by the next event for that card or by any re-fetch. Nor is
delivery guaranteed: this is at-most-once, as the "nothing is replayed" section
above says.
