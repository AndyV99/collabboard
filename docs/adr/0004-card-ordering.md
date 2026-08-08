# 0004. Card ordering: fractional ranks, allocated under a parent row lock

Date: 2026-08-08
Status: accepted

## Context

A Kanban board is an ordered list per column, and the operation the product is
built around is "drag this card to here". Issue #47 asks for that operation, and
issue #45 will broadcast it to every other client watching the board, so the
question is not only "does a move produce the right order" but "does a move
produce the right order when two people do it at the same time".

The schema already leans one way. Migration `00003_domain.sql` gave `columns`
and `cards` a `position numeric` with a comment saying why:

> numeric, not integer: drag-and-drop reordering inserts *between* two
> neighbours, and an exact-precision type lets that be a single-row update
> (midpoint of the two) instead of renumbering the whole list under a concurrent
> edit.

That is a strong hint, not a decision. It fixes the column type; it does not say
how a midpoint is computed, what happens when two midpoints are computed at
once, what the API accepts, or what happens when the fractions get long. Those
are the parts that are expensive to reverse — the API shape especially, because
a client that has learned to send a position is a client that has to be
rewritten — so they are what this record decides.

Constraints from the existing design: every query runs inside a tenant-scoped
transaction opened by `store.WithTenant` (ADR 0001), so a move can use several
statements atomically without any new machinery. Postgres is the only store.
Boards are small — tens of columns, hundreds of cards — and the write rate per
board is human-paced, but the *concurrency* on a single card is exactly what a
collaborative tool exists to have.

## Options

**Integer positions, renumbered on move.** `position int`, contiguous, and a
move rewrites every row between the old and new slot. It is the easiest to read
and the only one where the stored value means something a human recognises.

Against it: a move writes O(n) rows, and — much worse — two concurrent moves in
the same column interleave badly. Each computes its shifts against the order it
read, and the two `UPDATE` sets overlap; under `READ COMMITTED` the second
transaction re-evaluates its `WHERE` against rows the first has already shifted,
so the arithmetic that produced the shift no longer matches the rows being
shifted. The results are not merely "one of the two orders" — they include
orders neither client asked for, and duplicate positions. Making it correct
means either locking the whole column and renumbering (which is the throughput
of the linked list with none of its cheapness) or `SERIALIZABLE` plus a retry
loop on every drag. It is also the option that makes the realtime story worst:
one move produces n changed rows to broadcast.

**Fractional ranks.** `position numeric`, and a move sets one row to a value
strictly between its two new neighbours. One row written, one row to broadcast,
and no other card's rank changes — so a concurrent move of a *different* card
touches a disjoint row and simply does not conflict. The costs are real and
specific: the fractions get longer the more often you subdivide the same gap, so
something has to renumber eventually; the value is meaningless to a human; and
two moves into the *same* gap have to be prevented from computing the same
midpoint.

**Linked list (`prev_id` / `next_id`).** A move rewrites three rows and never
degrades. Against it: reading a column in order becomes a recursive CTE, which
is both slower and unindexable for the board view's "every card on the board,
grouped by column" query — the hot read. And the failure mode is unbounded: one
lost update leaves a dangling or cyclic link, and there is no local repair,
because the correct order is only recoverable from the chain that just broke. A
fractional rank that is slightly wrong sorts slightly wrong; a broken chain
loses the column.

## Decision

**Fractional ranks**, with three details that are part of the decision rather
than implementation trivia.

**1. Midpoints are computed as `(lower + upper) * 0.5`, never `(lower + upper) / 2`.**

This looks like a style choice and is a correctness one. Postgres `numeric`
multiplication is exact: the result's scale is the sum of the operands' scales.
Division is not — `select_div_scale` picks a result scale of at most the larger
input scale (with a floor of roughly 16 significant digits) and *rounds* to it.
Halving with `/` therefore stops gaining precision once the scale reaches that
floor: after about 53 nested inserts into the same gap the "midpoint" rounds to
one of the bounds, and two cards end up with the same rank. With `*` `0.5` the
scale grows by exactly one decimal place per subdivision and the midpoint is
always strictly between its neighbours.

Measured rather than reasoned about, on postgres:16-alpine — subdividing the gap
between 1 and 2 repeatedly, both ways:

```
 step |      div_gap       | div_scale | mul_collapsed | mul_scale
------+--------------------+-----------+---------------+-----------
    0 |                  1 |         0 | f             |         0
   10 | 0.0009765625000000 |        16 | f             |        10
   30 | 0.0000000009313225 |        16 | f             |        30
   50 | 0.0000000000000008 |        16 | f             |        50
   53 | 0.0000000000000001 |        16 | f             |        53
   54 | 0.0000000000000000 |        16 | f             |        54
   60 | 0.0000000000000000 |        16 | f             |        60
```

The `/` scale is pinned at 16 and the gap reaches zero at step 54 — from there
every "midpoint" equals a bound. The `*` scale tracks the step count and the gap
never collapses.

**2. Position allocation is serialised on the parent row, not on the siblings.**

Every statement that allocates a position — creating a card, moving a card,
creating a column, moving a column — runs while the transaction holds `SELECT
... FOR UPDATE` on the parent row: the target `columns` row for cards, the
`boards` row for columns. That is one lock per operation, taken before anything
is read, so two concurrent moves can never hold one lock each and wait for the
other's; there is no lock ordering to get wrong and no deadlock to retry.

It also converts the one genuinely bad case into a non-case. Two clients
dropping different cards into the same gap would otherwise compute the same
midpoint from the same neighbours and produce a tie. With the lock, the second
transaction blocks, then reads the column *after* the first has committed, sees
the newly placed card as a neighbour, and subdivides again. Ranks within a
column are therefore distinct by construction, not by luck.

The cost is that moves into one column are serialised. A column is a handful of
rows and the transaction is three statements, so this is microseconds of
contention on an operation a human performs; it would be the wrong trade for a
high-throughput queue and it is the right one here.

**3. The API names a neighbour and never publishes a rank.**

A move is `POST /api/v1/cards/:card_id/move` with `{"column_id": ..., "after_card_id": ...}`,
where a null `after_card_id` means "first". No endpoint accepts a position and
no response contains one.

An index or a position is a claim about a list the client last saw. Two clients
holding the same slightly-stale board send two positions that disagree about
where "third from the top" is, and the server cannot tell a deliberate placement
from a stale one. A neighbour is a claim about a single row, which the database
can still evaluate after someone else has reordered everything around it — and
when that row is no longer in the target column, the move can be *refused*
(409) instead of landing somewhere plausible and wrong. Not publishing the rank
also means no client can come to depend on a number that renumbering will
change.

**Renumbering.** `MoveCard` and `MoveColumn` return `needs_rebalance`, true once
the new rank's scale passes 100 decimal places. The handler, still holding the
parent lock, renumbers the column to 1..n. `numeric` tops out at 16383 digits
after the point, so 100 is conservative by two orders of magnitude; the point of
a low threshold is that the renumbering path runs often enough to be a tested
path rather than a theoretical one.

## Consequences

**What two clients moving the same card produce.** Both transactions take the
target column's lock and then `UPDATE` the same `cards` row, so they serialise
on that row. The second one re-evaluates its `CASE` against the order the first
one left behind. The result is the placement whichever client committed second
asked for — last writer wins, at the granularity of one card — and never a blend
of the two, never a duplicate rank, never a lost card. That is asserted, and the
distribution over repeated runs is reported rather than pinned, in
`TestTwoClientsMovingTheSameCardConcurrently`.

Last-writer-wins is the right semantic here and is worth saying out loud: there
is no meaningful merge of "put this card second" and "put this card third", and
a conflict error would be a worse answer than a placement, because the losing
client is about to be told the truth by the realtime broadcast (#45) anyway.

**What two clients moving different cards into the same gap produce.** Both land
in the gap, in one order or the other, with distinct ranks —
`TestTwoClientsMovingDifferentCardsIntoTheSameGap`. Without the parent lock this
is the case that produces a tie, so that test is the one that would fail if the
lock were ever removed as an optimisation.

**What a stale drag produces.** An `after_card_id` that is not currently a card
in the target column — deleted, moved away, or belonging to another tenant, which
the RLS policy has already made invisible — makes the `UPDATE` match no row, and
the handler answers 409. The alternative, treating an unknown anchor as "move to
the front", would turn every stale client into a wrong-but-successful write.

**Reads stay ordinary.** `ORDER BY position, id` on the existing
`cards_tenant_column_position_idx`. The `id` tiebreaker is not load-bearing —
ranks are distinct by construction — but a list whose order depends on that
argument holding would return rows in an unspecified order the day it stops.

**Easier / harder.** Easier: one row written per move, which is exactly one
event for #45 to broadcast, and a client can apply it without re-fetching the
column. Harder: the stored value is not human-readable, so "why is this card
here" is answered by comparing two long decimals; and the renumbering path is a
second code path that only runs occasionally, which is the kind of code that
rots unless a test drives it — `TestRanksAreRebalancedBeforePrecisionRuns` does,
by nesting 110 moves into one gap.

**Reversal.** If fractional ranks prove wrong — most plausibly because a feature
needs "move to index n" without the client holding the list, or because
per-column serialisation becomes a bottleneck in a way a board never should —
the exit is a one-column migration plus a rewrite of two queries. The API does
not have to change at all, because it never exposed the rank: `after_card_id`
means the same thing over a linked list or over renumbered integers. That
containment is the main reason for decision 3, and it is worth more than the
convenience the alternative would have bought.
