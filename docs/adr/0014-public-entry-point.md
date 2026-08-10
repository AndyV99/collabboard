# 0014. The public entry point: the web tier on ECS, and an API that is not on the internet

Date: 2026-08-10
Status: accepted

## Context

#102 has to put two things somewhere reachable — the Go API and the Next.js web
app — and #101 deliberately left the app security group with no ingress rule at
all, so that the rule admitting a load balancer would land in the same change as
the load balancer.

The issue asks for a decision on the web app: deployed here, or hosted elsewhere
with the reasoning written down. It also frames the API as sitting behind an ALB
with a WebSocket path through it, which is the shape the vault's architecture
diagram shows: `Browser --HTTPS/WSS--> ALB --> API`.

That diagram is now out of date, and the difference is the whole of this
decision. **ADR 0010 moved the WebSocket off the browser.** The API
authenticates a handshake with a bearer token in a `Sec-WebSocket-Protocol`
offer, and ADR 0007 decided the browser never holds a token — so the Next.js
server holds the WebSocket to the API and relays it to the page as a same-origin
SSE stream. `apps/web/lib/realtime/stream.ts` derives the `wss://` URL from
`API_URL` server-side; `apps/web/lib/api/browser.ts` states plainly that the
browser's base is a relative path and "CORS does not exist here: every request is
same-origin".

The consequence is that **nothing in a browser ever contacts the Go API**. Its
only client is a Next.js task in a private subnet.

Three further facts bound the answer:

- The web app's own route handlers occupy `/api/auth/*`, `/api/proxy/*` and
  `/api/realtime/*`. The Go API is entirely under `/api/v1/*` plus `/healthz`.
- `/healthz` is served by *both* tiers, and the API's names each dependency it
  probed and the raw error from any that failed (#31).
- `SetTrustedProxies(nil)` means the API's `ClientIP()` is its peer's address, so
  the per-address login budget is not real behind any proxy (#33), and the
  address-existence oracle on registration is unbudgeted (#73).

## Options

### Where the web tier runs

**Vercel.** The obvious answer for a Next.js app, and it is wrong for this one
for a specific reason rather than a philosophical one: the SSE relay in
`app/api/realtime/boards/[boardId]/route.ts` is meant to hold a connection open
indefinitely, with a 20-second heartbeat that exists precisely to survive a proxy
with an idle-read timeout. Serverless functions have a maximum duration. A relay
that is cut at the platform's limit turns the project's headline feature into
something that works for a few minutes and then silently stops. Hosting it
elsewhere would also mean a second deploy pipeline, a second registry and a
second secret store for #103 to learn, and it would put the tier that holds the
session cookies (ADR 0007) outside the account that holds everything else.

**ECS Fargate, behind the same load balancer.** One pipeline, one registry, one
trust boundary, and a runtime that can hold a connection open for as long as it
likes. #37 containerised `apps/web` for exactly this. It costs about $9/month.

### Where the API sits relative to the internet

**Public, on 443, under a second hostname.** Simplest, one set of listener rules,
and it is what the issue's framing assumes. It publishes `/api/v1/auth/login` to
anyone, which given #33 and #73 is a materially worse position than it looks —
the rate limits that would make that acceptable are not real yet.

**An internal ALB for the API, a public one for the web tier.** Clean, obvious,
no hairpin, and $16.43/month for a second load balancer — which would take this
environment from ~$61 to ~$121, more than doubling it for one property.

**Cloud Map service discovery, no load balancer in front of the API.** Free and
private, but it removes the load balancer from the WebSocket path entirely, which
means no connection draining, no health-check-driven routing, and a DNS TTL's
worth of connection attempts to dead task IPs after every deploy.

**One public load balancer, with the API on a separate listener port whose
security group admits only this environment's own NAT egress address.** An ALB's
security group is per-port, so two hostnames on one listener share one exposure
and two listener ports do not. A task in a private subnet reaching an
internet-facing load balancer goes out through the NAT gateway and back in, so it
arrives from a known, fixed address.

## Decision

**The web tier runs on ECS Fargate. One internet-facing ALB serves both tiers on
three ports: 80 redirects, 443 is the web app and is open to the world, and 8443
is the Go API and is open only to this environment's NAT gateway addresses.**

`api_ingress_cidrs` is derived from `module.network.nat_gateway_public_ips`, so
the allow-list cannot drift from the thing it describes, and an empty list is
rejected at plan time rather than producing a listener nothing can reach.
`api_admin_ingress_cidrs` exists for an operator's own address and validates
against `0.0.0.0/0`.

**The idle timeout is 120 seconds, and it is validated rather than commented.**
The hub pings every 25 seconds and reaps a peer that has not answered within 10,
so bytes cross an otherwise-idle WebSocket at least every 25 seconds; the SSE
relay's own heartbeat is 20. AWS's default of 60 works against those numbers
today and would silently stop working the moment somebody raised the ping
interval. So `modules/alb` takes the ping interval and pong timeout as inputs and
refuses an idle timeout below twice their sum — and the *same two variables* set
`REALTIME_PING_INTERVAL` and `REALTIME_PONG_TIMEOUT` in the task definition, so
there is exactly one place either number is written.

**`/healthz` and `/metrics` are answered with a fixed 404 by both HTTPS
listeners.** A target group health check is issued by the load balancer straight
to the target's address and port and never traverses a listener, so `/healthz`
remains reachable by the health check while being unreachable from outside.
`/metrics` does not exist yet (#12); blocking it now means it arrives unreachable
rather than arriving published. 404 rather than 403, because a 403 confirms that
something is there.

**Target group stickiness is off on both tiers.** It is the reflex answer for
WebSockets and the wrong one: a WebSocket is one TCP connection pinned to a
target for its lifetime, and ADR 0005's Redis fan-out is what makes instances
interchangeable. On the web tier, stickiness would hide #69 rather than fix it —
so the web service runs a single task instead, which is a stated constraint
rather than a disguised one.

**Host-based routing on two names, not path-based on one.** `/api/v1/*` versus
`/api/auth/*` happens to be unambiguous today, and it stays unambiguous only for
as long as nobody adds a `/api/v1` route to the Next app. Two names cannot
collide.

## Consequences

**Every server-rendered page hairpins through the NAT gateway.** The web task
resolves the API's public hostname, gets a public address, and its traffic goes
private subnet → NAT → internet gateway → back to the load balancer. That is the
main cost of this shape: latency measured in single-digit milliseconds, NAT data
processing at $0.045/GB, and an inelegance that has to be explained to anybody
reading the diagram. At staging volume the processing charge is pennies. **If it
ever stops being pennies, the fix is an internal ALB for the API at
+$16.43/month**, which is a scheme change plus an `API_URL` change and nothing
else.

**This is also the load-bearing assumption I could not verify.** With no AWS
credentials there is no way to prove the hairpin works, only to reason that it is
the same routing an EC2 instance in a private subnet uses to reach any public
address. If it does not, the symptom is total: the web tier cannot reach the API
at all.

**The API is not on the internet, so #73 and #33 are contained rather than
live.** The address-existence oracle and the unbudgeted login endpoint are
reachable only from the web tier. It does not fix either — the web tier proxies
requests to both — but it means the unbudgeted surface is behind something rather
than in front of everything.

**#33's answer is now more complicated than "trust the ALB".** Because the
browser never reaches the API, `ClientIP()` yields the *web task's* address in
every topology. The private subnet CIDRs are exported for it, and a correct
trusted-proxy list is necessary but not sufficient — the web tier also has to
forward `X-Forwarded-For`. That is worth knowing before #33 is picked up.

**`nat_gateway_count = 0` is no longer a supported setting.** #101 offered it as
the one-variable way to shed half the bill. It now fails at plan time, twice
over: no egress means no image pull, and no egress address means an empty API
ingress list. `terraform destroy` is the replacement, which staging is configured
to make safe.

**Monthly cost rises by roughly $45**, from ~$61 to ~$106: $16.43 for the load
balancer, $18.02 for two API tasks, $9.01 for one web task, $1.20 for three
Secrets Manager secrets, $0.30 for three alarms, and about a dollar of ECR
storage and logs. This is the issue that finally makes the existing NAT gateway
earn its $32.85 — until now it has been paid for and unused. The next levers, in
order, are Fargate Spot (~70% off the task lines, at the price of a two-minute
eviction that disconnects every WebSocket on the task), ARM64 task definitions
(~20% off, contingent on #103 building arm64 images), and dropping the API to a
single task — which is the cheapest and costs the most, because the fan-out that
makes this project interesting would have nothing to fan out to.
