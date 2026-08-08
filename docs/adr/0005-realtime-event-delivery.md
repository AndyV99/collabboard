# 0005. Realtime event delivery: publish after commit, best effort, no outbox

Date: 2026-08-08
Status: accepted

## Context

Issue #9 built the WebSocket hub and its Redis fan-out. Issue #47 built the
board, column and card write path. Neither is connected to the other: the hub
publishes only from a demonstration endpoint added so that #9's tests had
something to send, and a card that moves in Postgres tells nobody. Issue #45 is
the join, and the vault's scope for this project is what it has to satisfy —
*"card moves and edits sync to all connected clients in the same board within
~200 ms."*

Wiring the two together is three lines. What is expensive to reverse is
everything around those three lines, because two of the answers become a
contract the frontend is written against and a third becomes a database table if
it is chosen wrongly:

1. **Where** the publish happens relative to the transaction.
2. **What happens** when the write commits and the publish does not.
3. **What ordering** a client may assume, given that the answer to (1) leaves a
   gap between "committed" and "announced".

Two constraints from the existing design bound all three.

`internal/realtime/README.md` is explicit that the stream is **at-most-once and
holds nothing**. Redis pub/sub has no backlog, so an event published while a
client is between instances is gone, and a client re-fetches the board on every
`subscribed` frame precisely because of that. The stream is a latency
optimisation over polling, not a replication channel.

`internal/store`'s `WithTenant` owns the transaction end to end: the callback
gets a `Querier`, and the commit happens after it returns. Nothing inside the
callback can know whether the commit succeeded, because the commit has not
happened yet.

## Options

### Where the publish happens

**Inside the transaction.** The obvious place — the publish sits next to the
write it describes, and there is no second function to look at. It is also
wrong, and not subtly: a transaction that rolls back after publishing has told
every connected client about a change the database never made. Every browser on
the board now shows a card that does not exist, and nothing in the stream will
ever correct it, because the correction would be another event about a write
that did not happen. A refresh is the only fix, and the user has no reason to
suspect they need one. Stale is recoverable; wrong is not.

**After the transaction commits.** The publish cannot describe a write that did
not happen, because it runs only when `WithTenant` returned nil. The cost is the
window this ADR's third question is about.

### When the publish fails after a successful commit

**Fail the request.** Roll the write back — except it is already committed, so
this means answering 500 for a write that happened. The client retries a move it
already made. Strictly worse than every alternative and listed only because it
is what "return the error" quietly becomes.

**Fire and forget.** Log it, answer the client normally. Clients diverge from the
database until something makes them re-fetch.

**Transactional outbox.** Write the event to a table in the same transaction as
the card, and have a poller publish rows and mark them done. At-least-once
delivery to Redis, in commit order, with the durability of the write itself. The
cost is a table, a poller, a dedupe key, a lag metric, and a second failure mode
(the poller falls behind) for a system whose current worst failure is a stale
board.

**Something in between** — an in-memory retry, a small bounded queue drained by a
background goroutine. Cheaper than an outbox, and it makes the failure window
smaller without closing it.

### Ordering

**Nothing.** Say the stream is unordered and require clients to reconcile
against Postgres. Correct and useless: reconciling means re-fetching, which is
what the stream exists to avoid.

**A total order per board, from the transport.** Redis delivers messages on one
channel to every subscriber in the order it received them, one channel per room,
and each instance's dispatch loop is a single goroutine, so every client on every
instance sees the same sequence. That is free — it is already true.

**A total order that matches commit order.** Requires the publish to be
sequenced by something that knows the commit order, which in practice means the
outbox with a single ordered reader.

## Decision

**Publish after the commit, synchronously, before the HTTP response. Log a
publish failure and let the write stand. No outbox.**

### 1. After the commit, and structurally so

`tenantScopedPublish` (`internal/api/crud.go`) takes the transaction body and a
*second* function that describes the event, and calls the second one only after
`WithTenant` has returned nil. The transaction body is handed a `store.Querier`
and nothing else — no publisher is in scope inside it, on the handlers that
exist today or on any written later. "A rolled-back write broadcasts nothing" is
therefore a property of the types rather than a rule to remember.

`TestARolledBackWriteBroadcastsNothing` injects the failure at *commit*, not
inside the callback, because a handler that published as its last statement
inside the transaction would have done everything else right and still be wrong.
`TestARolledBackWriteAssertionHasTeeth` writes that mistake out and requires the
event to escape, so the assertion is known to be able to fail.

### 2. Best effort, and no outbox

The decisive argument is not cost, it is that **an outbox cannot make delivery
reliable here, because the hop it feeds is itself lossy.** An outbox guarantees
at-least-once delivery to Redis; Redis pub/sub then guarantees at-most-once
delivery to a client, and drops everything for anyone who is momentarily
disconnected, mid-reconnect, or between instances during a deploy. Spending a
table, a poller and an ordering mechanism to make one hop of a two-hop path
durable buys a client nothing it can rely on. Making the path *actually* reliable
is a different design — per-client cursors, a replayable log, acknowledgements —
and it is a design this project explicitly does not want, because it would make
an instance restart expensive and would duplicate the source of truth.

The recovery path that would justify an outbox therefore has to exist anyway, and
does: a client re-fetches the board on every `subscribed` frame, on close code
`4002` (dropped as a slow consumer), and on reconnect after a `shutdown` frame.
A missed event is the same situation as a missed reconnect, which is the
situation the client is already built for.

What is left is visibility. A publish failure is logged at error level with the
event type, tenant and board (`realtime.publish.failed`), so "this board's
clients stopped seeing each other" is a log line rather than a support ticket.

The vault plans a transactional outbox for **#02 Fulfillment**, and that is the
right place for it: there the consumer is a work queue with retries, the payload
moves money, and a lost message is a lost order rather than a stale column
heading. Same pattern, different consequence of losing a message.

### 3. Synchronously, before the response

The publish is not handed to a background goroutine. It happens between the
commit and the response, which costs the request one Redis `PUBLISH` — measured
sub-millisecond, inside a path whose end-to-end p50 is 1.5 ms against a 200 ms
target — and buys the one ordering property a client can actually build on.

It is bounded at two seconds (`publishTimeout`) and runs on a context detached
from cancellation (`context.WithoutCancel`): the write is already durable, so a
client that hung up must not be the reason every *other* client on the board
never hears about it.

### The ordering guarantees, stated exactly

**Guaranteed:**

- **One total order per board, identical at every client on every instance.** All
  of a room's traffic goes through one Redis channel, and each instance's
  dispatch loop is a single goroutine writing into per-connection FIFO buffers.
  Two clients watching one board can disagree with the database for a moment;
  they cannot disagree with *each other*, which is the failure that produces two
  users describing different boards over a call.
- **Causal order for one client's own writes.** If a client issues a second write
  only after the first one's response arrived — the shape a drag-and-drop
  produces — the first event was published before the response was written, so
  every other client sees the two in the order they were made.
  `TestTwoRapidMovesOfTheSameCardArriveInOrder` demonstrates this across two
  instances over real Redis, and checks the final event against the database.

**Not guaranteed:**

- **That publish order matches commit order for genuinely concurrent writers.**
  Two moves of the same card serialise in Postgres on the card's row lock, so
  their commits are ordered — but each request then publishes from its own
  goroutine, and the loser of that race would have to be descheduled for the
  duration of the other's remaining database round trips for the events to
  invert. It is narrow, it is not impossible, and it is not closed. When it
  happens, every client still agrees, and the disagreement with the database is
  corrected by the next event for that card or by any re-fetch.
- **Delivery.** At-most-once, as it was before this change.

Last-writer-wins at the granularity of one card is already the semantic ADR 0004
chose, and this is the same trade one layer up: two people who moved the same
card within microseconds of each other do not have a meaningfully correct answer
between them, and a client that is briefly wrong about one card is a much smaller
problem than a client that is briefly wrong about the whole board.

### What publishes, and what does not

A realtime room *is* a board, so the rule is: **a write publishes when its effect
is visible to somebody already looking at that board.**

Publishing: `card.created`, `card.updated`, `card.moved`, `card.deleted`,
`column.created`, `column.updated`, `column.moved`, `column.deleted`,
`board.updated`, `board.deleted`.

Not publishing: everything about projects, and board *creation*. Not because
they do not matter, but because there is no room to address them to — at the
moment a board is created nobody can be watching it. A project- or tenant-scoped
room would be a different fan-out unit with its own authorization question, and
it is filed as [#52](https://github.com/AndyV99/collabboard/issues/52) rather
than guessed at.

Cascades produce one event, not n. Deleting a column deletes its cards; deleting
a board deletes both. One user action is one event, and a client that drops the
column drops what was in it.

### The payload

Every payload carries the *same* object representation the REST endpoints return
— `cardBody`, `columnBody`, `boardBody` — so a client has one card type and one
decoder, and a field added to a response cannot silently fail to appear in the
event that announces it. Deletes carry ids only. Moves carry the anchor the mover
named, with an explicit `null` for "first"; the rank is never published, for the
reasons ADR 0004 gives.

Events go to a board room, and room membership already implies authorization:
`StoreAuthorizer` admits a subscriber only if the board resolves inside their own
tenant, membership in this schema is per organization, and every field of every
payload comes from a row a tenant-scoped transaction returned. There is nothing
in an event that a member of that room could not have fetched over REST.

## Consequences

**The demo endpoint is gone.** `POST /api/v1/boards/:board_id/events` let any
authenticated member announce a card move that never happened, to every other
client on the board. It answers 404 now, and the tests that used it drive the
card endpoint instead — including the cross-tenant ones, because the publish path
is one of the two places a board id crosses the tenant boundary.

**A card write now costs a Redis round trip.** Bounded at two seconds, and a
`PUBLISH` that is slow enough to notice is a Redis that is about to be a much
larger problem.

**Clients can diverge silently while Redis is unavailable.** The write path keeps
working, so the database moves on while nobody is told. Clients recover when they
next re-subscribe, and the error log says it is happening — but a client is not
*told* that the stream is degraded, and it should be. Filed as
[#53](https://github.com/AndyV99/collabboard/issues/53) rather than folded in
here.

**Reversal.** Adding an outbox later does not change the event shape, the
handlers, or the frontend: it replaces the body of `EventPublisher`
implementation and adds a poller. That containment is deliberate, and it is why
"not yet" is a cheap answer rather than a bet.
