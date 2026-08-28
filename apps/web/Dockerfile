# syntax=docker/dockerfile:1
#
# Runtime image for apps/web (Next.js, App Router).
#
# Build from the REPO ROOT, because the context is the repo root:
#
#     docker build -f apps/web/Dockerfile -t collabboard-web:dev .
#
# The root `.dockerignore` keeps that context small. Building from the repo
# root rather than from apps/web costs nothing today and means a shared package
# under a future `packages/` can be added without restructuring this file.
#
# ---------------------------------------------------------------------------
# API_URL IS NOT A BUILD ARG. DO NOT ADD ONE.
# ---------------------------------------------------------------------------
# It is a task-definition / `docker run -e` variable, read from the live process
# environment on every request (see `lib/api.ts`). Adding `ARG API_URL` — or an
# `ENV API_URL` here — would bake one environment's API into the artifact and
# reintroduce exactly the build-time freeze issue #16 removed: the image that
# passed staging would no longer be the image that runs in production.
#
# The test for any future addition to this file: could the same image tag still
# be promoted dev -> staging -> prod unchanged? If not, it belongs in the task
# definition, not here. There is deliberately no default either — unset falls
# back to `http://localhost:8080` in code, which is right for `npm run dev` and
# wrong for a container, so a container that forgot the variable should show up
# as `source: "default"` on /healthz rather than be quietly papered over.

# Pinned to a patch release, not a floating `22-alpine`, for the same reason the
# CI workflow pins its toolchains: a base image refresh should arrive as a
# commit someone reviewed, not as yesterday's green build going red. Bump
# deliberately (Dependabot can do it once #22 lands).
ARG NODE_VERSION=22.23.2
ARG ALPINE_VERSION=3.24

# ---------------------------------------------------------------------------
# Target architecture — MUST match `runtime_platform` in modules/ecs/main.tf
# ---------------------------------------------------------------------------
# arm64, matching that module's `cpu_architecture = "ARM64"`. A mismatch is not
# a build failure; it is a task that dies at start with an exec format error,
# which reads like a broken image rather than a wrong platform.
#
# Note the difference from apps/api/Dockerfile, which pins its *builder* to
# $BUILDPLATFORM and cross-compiles. This one cannot, and the reason is
# concrete rather than cautious: `next build`'s standalone output traces
# node_modules and pulls in sharp's native binding — `@img/sharp-linux-x64` or
# `@img/sharp-linuxmusl-x64` on an x64 builder. npm selects that variant from
# the platform it is *installed on*, so a cross-build would ship an x64 .node
# file inside an arm64 image and Next's image optimisation would fail at
# runtime, long after the build reported success.
#
# So every stage below is pinned to the target platform. On an arm64 runner
# that is native and costs nothing; on an amd64 machine it is qemu emulation
# and the build takes minutes rather than seconds. `.github/workflows/cd.yml`
# therefore builds this image on an arm64 runner.
ARG TARGET_ARCH=arm64

# ---------------------------------------------------------------------------
# Stage 1: dependencies
# ---------------------------------------------------------------------------
# Its own stage so the (slow) install is keyed on the lockfile alone. Editing a
# component re-runs the build stage but reuses this one.
FROM --platform=linux/${TARGET_ARCH} node:${NODE_VERSION}-alpine${ALPINE_VERSION} AS deps

WORKDIR /app

# Lockfile-only copy, before the source, so a source edit does not invalidate
# the install layer.
COPY apps/web/package.json apps/web/package-lock.json ./

# `npm ci` and not `npm install`: it installs exactly the lockfile, and fails
# rather than silently updating it if the two have drifted. Dev dependencies are
# included on purpose — `next build` needs TypeScript to compile `next.config.ts`
# and the app's `.ts`/`.tsx`. None of this reaches the runtime stage.
#
# The cache mount survives between builds without landing in a layer.
# Runs on the target platform (see TARGET_ARCH above), which is what makes npm
# resolve the right `@img/sharp-*` optional dependency. Installing this stage on
# a different architecture than the runtime stage is the one cross-build mistake
# that produces a working build and a broken container.
RUN --mount=type=cache,target=/root/.npm \
    npm ci

# ---------------------------------------------------------------------------
# Stage 2: build
# ---------------------------------------------------------------------------
FROM --platform=linux/${TARGET_ARCH} node:${NODE_VERSION}-alpine${ALPINE_VERSION} AS builder

WORKDIR /app

ENV NEXT_TELEMETRY_DISABLED=1

COPY --from=deps /app/node_modules ./node_modules

# The whole app directory rather than an enumerated list. An enumerated COPY is
# marginally better for cache reuse and considerably worse when someone adds a
# directory and the image silently builds without it. The root `.dockerignore`
# already strips the host's `node_modules`, `.next` and `.env*`.
COPY apps/web/ ./

# `output: "standalone"` in next.config.ts makes this emit `.next/standalone`:
# a `server.js` plus only the traced subset of node_modules the routes reach.
# That is what stage 3 ships instead of the ~450-package install above.
#
# Nothing environment-specific is read here. `API_URL` is absent from this
# stage's environment on purpose: if the build ever starts depending on it, the
# build breaks loudly instead of freezing a value.
RUN npm run build

# `server.js` serves `./public` when it exists, and standalone output does not
# copy it. There is no public/ in this app yet; creating it here means adding
# one later needs no change to this Dockerfile, and the COPY below cannot fail
# on a missing path.
RUN mkdir -p public

# ---------------------------------------------------------------------------
# Stage 3: runtime
# ---------------------------------------------------------------------------
FROM --platform=linux/${TARGET_ARCH} node:${NODE_VERSION}-alpine${ALPINE_VERSION} AS runner

WORKDIR /app

# Deliberately absent: API_URL. See the banner at the top of this file.
#
# HOSTNAME is load-bearing: standalone's server.js binds `localhost` unless told
# otherwise, which is unreachable from outside the container's network
# namespace — the container would look healthy and answer nothing.
ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    PORT=3000 \
    HOSTNAME=0.0.0.0

# Run as the unprivileged `node` user (uid/gid 1000) that the base image already
# provides, rather than minting another one: fewer moving parts, and a uid that
# is stable across base image bumps. Root in a container is still root on the
# host kernel for anything that escapes the namespace, and nothing this process
# does needs privilege — it binds 3000, not 80.
#
# Copying with --chown rather than chown-ing afterwards keeps the file data in a
# single layer instead of duplicating it into a second one.
COPY --from=builder --chown=node:node /app/.next/standalone ./
COPY --from=builder --chown=node:node /app/.next/static ./.next/static
COPY --from=builder --chown=node:node /app/public ./public

# Next writes its fetch/ISR cache under .next/cache. Pre-creating it owned by
# `node` avoids a first-request EACCES against a root-owned parent — an error
# that surfaces as a 500 on a page, far from its cause.
RUN mkdir -p .next/cache && chown -R node:node .next/cache

# Drop the package managers the base image bundles. This stage runs
# `node server.js` and nothing else — npm, yarn and corepack are unreachable
# code here, but they are not unreachable to an attacker who gets command
# execution, and they vendor their own dependency trees.
#
# Not a theoretical tidy-up: with them present, `trivy image --severity
# HIGH,CRITICAL` reports 8 findings against this image (1 critical), every one
# of them inside `/usr/local/lib/node_modules/npm` and none inside the
# application's own traced dependencies. Removing them takes that to zero, so
# the Trivy gate the Security Practices standard asks for can stay meaningful
# instead of being permanently red for reasons nobody in this repo can fix.
#
# It does not shrink the image: the files still exist in the base layer
# underneath, and this only writes whiteouts over them. What it changes is the
# filesystem the container actually runs with — which is what an attacker sees
# and what a scanner reads. A real size win would mean building the runtime
# from bare `alpine` and copying the node binary across; that is a bigger
# change than this issue needs.
RUN rm -rf /usr/local/lib/node_modules/npm \
           /usr/local/lib/node_modules/corepack \
           /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
           /opt/yarn-* /usr/local/bin/yarn /usr/local/bin/yarnpkg

USER node

EXPOSE 3000

# Container-level view of the readiness route the ALB target group will use.
# `node -e` rather than curl/wget so the check depends only on the runtime that
# is already required to be present — and it exercises HTTP end to end, which a
# TCP probe does not: a process wedged after listen() passes a port check.
#
# start-period covers Next's boot; a failing check must not kill a container
# that is merely still starting.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD node -e "fetch('http://127.0.0.1:'+(process.env.PORT||3000)+'/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

LABEL org.opencontainers.image.title="collabboard-web" \
      org.opencontainers.image.description="CollabBoard web front end (Next.js)" \
      org.opencontainers.image.source="https://github.com/AndyV99/collabboard"

# `node server.js`, not `next start` — the standalone output has no `next` CLI.
# Exec form so Node is PID 1 and receives SIGTERM directly; Next's server
# registers a SIGTERM handler, so `docker stop` (and an ECS task drain) shuts
# down promptly instead of waiting out the 10s kill timer. A shell-form CMD
# would put /bin/sh at PID 1 and swallow the signal.
CMD ["node", "server.js"]
