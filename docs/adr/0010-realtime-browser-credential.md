# 0010. The browser's realtime credential: a same-origin relay, not a token

Date: 2026-08-10
Status: accepted

## Context

Issue #66 connects the board screen to the WebSocket hub. Two decisions already
in this repository make that impossible as written, and the conflict is not
incidental — each of them is load-bearing for its own side.

**The API authenticates a WebSocket handshake with a bearer token carried in a
subprotocol offer.** `apps/api/internal/api/realtime.go` reads
`Sec-WebSocket-Protocol`, strips the prefix `bearer.collabboard.v1.`, and turns
the remainder into an `Authorization: Bearer` header before `requireAuth` runs.
That mechanism exists for exactly one reason, stated in its own doc comment:
**a browser cannot set a header on a WebSocket handshake.** It is a client-side
workaround, and its whole point is that the client holds the token.

**ADR 0007 decided the browser never holds a token.** The session lives in
httpOnly cookies written by Next Route Handlers on the web origin, specifically
so that script in the page cannot read a credential and therefore cannot
exfiltrate one. Its consequences section is explicit about what that buys:
"An XSS in this origin can still act as the user; it cannot become the user
elsewhere or later."

So the handshake wants a token in JavaScript, and the session model exists to
keep one out of it. #66 cannot be built without answering this, and answering it
by accident — reaching for the token because the handshake asks for one — would
quietly undo ADR 0007 without anybody writing down that it had happened.

Two further constraints bound the answer:

- **`apps/api` is out of scope for this issue** (#93 is in flight there), so
  "add a ticket endpoint" is not available. It was checked for rather than
  assumed: a search of the whole API for `ticket`, `ws_token`, `one-time` and
  similar finds only prose. There is no short-lived, narrow-scoped credential to
  mint, and the subprotocol prefix is documented as *not* a second token
  mechanism — same token, same issuer, audience and signature checks, different
  transport.
- **`next.config.ts` sets `output: "standalone"`.** A custom `server.js` that
  proxied the upgrade would replace the server that build produces.

## Options

**Hand the access token to the browser.** A Route Handler returns the current
`cb_at` value; the page opens the WebSocket itself with
`["collabboard.v1", "bearer.collabboard.v1." + token]`. It is the smallest
change — perhaps twenty lines — and it is what the API's design is asking for.

The cost is precise and worth stating plainly rather than in the abstract. **An
XSS anywhere on this origin would get a fifteen-minute, full-scope access
token**, readable from a JS variable, exfiltrable to any host the page can
reach. Not a subscribe-only ticket: the same token that authorises
`DELETE /boards/:id`, `POST /members` and every other write, usable directly
against the API from the attacker's own machine, outside the browser, for up to
fifteen minutes and for as long after that as they can keep re-fetching it from
the endpoint that handed it over. That last part is the sharp edge — the
endpoint is a token oracle, so the exposure is not one fifteen-minute window but
an indefinitely renewable one for as long as the script runs.

That is a material weakening of ADR 0007, not a technicality. ADR 0007 rejected
client-held tokens on three counts and this reintroduces the one that mattered
most.

**Mint a narrower credential.** The honest version of the option above: a
short-lived, subscribe-only ticket, thirty seconds, good for nothing but opening
one stream on one board. An XSS would get thirty seconds of read access to a
board it could already read. This is the right shape, and it is unavailable —
the API verifies only its own HS256 tokens, the web app does not have (and must
not have) the signing secret, and the ticket would have to be issued and
verified by `apps/api`, which this issue may not change.

**Proxy the WebSocket through Next.** The browser talks to the web origin,
authenticated by the cookies it already has; the Next server holds the upstream
WebSocket and attaches the token server-side. No credential reaches JavaScript.
Next Route Handlers cannot accept a WebSocket upgrade, and a custom server would
undo the standalone build — but the *shape* is right, and only the browser-facing
transport needs to change.

**Proxy it as an event stream.** The same idea, using a transport Next
natively supports: the Route Handler returns a `ReadableStream` of Server-Sent
Events. Chosen.

## Decision

**The handshake happens on the Next server, where the token already is. The
browser is given a same-origin event stream and never a credential.**

`GET /api/realtime/boards/:boardId` (`app/api/realtime/boards/[boardId]/route.ts`)
reads the session from the httpOnly cookies, opens the Go API's WebSocket with
the `bearer.collabboard.v1.` subprotocol offer, sends one `subscribe`, and
relays every frame down an SSE body. `lib/realtime/relay.ts` is the relay;
`lib/realtime/client.ts` is the browser's end.

**ADR 0007 is unchanged.** Nothing in this design puts a bearer credential where
script can read it, so there is no weakening to record and no exposure to
quantify. An XSS on this origin can open the stream — it can also call
`/api/proxy/*`, which is the boundary ADR 0007 already describes and accepts —
but it cannot take a token away with it. That is the same blast radius as
before this issue, which is the property that made this option worth the extra
code.

The subprotocol mechanism is still used, and used as specified. It is simply
being used from a place that *could* have set a header, which costs nothing and
avoids inventing a second authentication path into a service this issue may not
modify.

**One board per stream.** The connection could hold sixteen rooms; this relay
subscribes to exactly one, and the stream is opened by a board screen and closed
when it unmounts. "Events for a board the user is not viewing are never applied"
is therefore true because no such event is ever delivered, rather than because a
filter remembers to drop it. The browser client checks the board id anyway.

**The relay is deliberately dumb.** It does not parse frames, decide what a
close code means, reconnect, or hold board state. Reconnecting belongs to the
browser because a reconnect must be followed by a re-fetch of the board
(ADR 0005), and only the browser can ask for the Server Component render that
performs one. A relay that quietly reconnected underneath would hide the
`subscribed` frame the client is required to react to, and the board would go
stale in precisely the situation ADR 0005 exists to handle.

**Token expiry is relayed, not handled.** A streaming Route Handler has already
begun its response and cannot set cookies, so it cannot refresh. Close code 4001
is passed to the browser, which calls `POST /api/auth/refresh` — the existing
route, which rotates the cookies — and opens a new stream that reads the cookie
that call just wrote. Refresh strictly precedes the reconnect, because
reconnecting first would present the same expired token and be closed again
immediately: a loop, not a recovery.

**The API's WebSocket URL is derived from `API_URL`** (`http`→`ws`,
`https`→`wss`) rather than configured separately. Two environment variables
naming one service is two things to get out of step, and the failure when they
disagree is a realtime path silently pointed at the wrong environment while
every REST call is correct.

## Consequences

**Two connections per board screen instead of one**, and the Next tier is now on
the realtime path rather than only the render path. A board with N viewers costs
N browser→Next event streams and N Next→API WebSockets, where the direct design
would have cost N. The web tier must therefore be sized for concurrent viewers,
not just for request rate, and its per-instance file-descriptor and memory limits
are now a realtime concern. This is the real price of the decision and it is not
small.

**A slow browser no longer looks slow to the API.** The relay drains the
upstream WebSocket promptly regardless of how fast the browser reads, so close
code 4002 — the API dropping a client whose send buffer overran — is now much
harder to earn from a browser. It was still implemented and tested, because a
loaded Next instance can earn one itself, and because the code path is the
recovery ADR 0005 relies on. The corollary is worse and is filed separately:
this relay enqueues into the SSE body without consulting backpressure, so a
browser that stops reading makes the Next process buffer without bound. See the
issue filed alongside #66.

**The stream is invisible to the API as a browser.** Every WebSocket the API
sees now originates from a web instance, so its per-connection logs identify the
relay rather than the end user. Nothing depends on that today; anything that
later wants per-viewer attribution will need it forwarded deliberately.

**SSE is one-way, which is enough today and not forever.** The client→server
frames the protocol defines are `subscribe`, `unsubscribe` and `ping`. The relay
sends the first on open, never needs the second (one board per stream, closed on
unmount), and does not need the third — the application-level ping exists so a
*browser* can measure a half-open socket, and the browser's half of this path is
an ordinary HTTP response whose death it learns about directly. If a future
screen needs to watch two boards on one connection, or to send anything upward,
this becomes a real constraint and the relay grows a second, ordinary Route
Handler for the upward direction.

**An idle stream needs a heartbeat.** A board where nobody is doing anything is
silent, and a proxy with an idle-read timeout will close a silent response. The
relay writes an SSE comment every 20 seconds for the browser→Next hop; the Go
API's own 25-second protocol ping keeps the other hop alive and is unrelated.

**Reversal is cheap and the exits are known.** If `apps/api` later grows a
subscribe-scoped ticket endpoint — the option this record wanted and could not
have — the browser can open the WebSocket directly and this route deletes;
`lib/realtime/client.ts` changes transport and keeps its state machine, because
the reconnect policy, the close-code handling and the re-fetch rule are about the
protocol rather than about SSE. If Next later supports WebSocket upgrades in
Route Handlers, the same is true with no API change at all. Neither exit touches
`lib/realtime/protocol.ts`, which describes the wire format and nothing else.
