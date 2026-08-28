# syntax=docker/dockerfile:1
#
# Runtime image for apps/api (Go: Gin REST API + WebSocket hub).
#
# Build from the REPO ROOT, because the context is the repo root:
#
#     docker build -f apps/api/Dockerfile -t collabboard-api:dev .
#
# Same context choice as apps/web/Dockerfile, for the same reason: the root
# `.dockerignore` keeps the context small, and a shared package under a future
# top-level directory can be added without restructuring this file.
#
# ---------------------------------------------------------------------------
# NOTHING ENVIRONMENT-SPECIFIC BELONGS IN THIS FILE
# ---------------------------------------------------------------------------
# Every value the API reads — APP_ENV, HTTP_PORT, POSTGRES_*, REDIS_*,
# REALTIME_*, AUTH_JWT_SECRET — is read from the live process environment by
# internal/config at startup. They come from the ECS task definition
# (infra/terraform/modules/ecs/main.tf), and the two credentials arrive as
# `secrets` entries resolved by the execution role, per ADR 0006.
#
# The test for any future addition, borrowed verbatim from apps/web/Dockerfile:
# could the same image tag still be promoted dev -> staging -> prod unchanged?
# If not, it belongs in the task definition, not here.
#
# That is also why there is no `ENV HTTP_PORT=8080`. The default lives in
# internal/config and is covered by its tests; repeating it here would create a
# second place for it to be wrong. `EXPOSE` below documents the port without
# duplicating the value's authority.

# ---------------------------------------------------------------------------
# Version pins
# ---------------------------------------------------------------------------
# Pinned to patch releases rather than floating tags, for the reason
# apps/web/Dockerfile gives: a base image refresh should arrive as a commit
# someone reviewed, not as yesterday's green build going red.
ARG GO_VERSION=1.26.6
ARG ALPINE_VERSION=3.24

# The runtime base gets a digest as well as a tag, which the Node pins did not
# need. Distroless publishes no versioned tags — `nonroot` is republished in
# place on every rebuild — so the tag alone pins nothing at all. The tag stays
# for readability; the digest is what actually holds. This is a multi-arch
# index digest, so it resolves correctly for either value of TARGET_ARCH below.
#
# Bump by re-running:
#     docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12
ARG RUNTIME_TAG=nonroot
ARG RUNTIME_DIGEST=sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

# ---------------------------------------------------------------------------
# Target architecture — MUST match `runtime_platform` in modules/ecs/main.tf
# ---------------------------------------------------------------------------
# arm64, matching that module's `cpu_architecture = "ARM64"`. This is not a
# free choice: an amd64 image on an ARM64 task definition (and the reverse)
# fails at task start with an exec format error, which surfaces as a task that
# will not stay up rather than as anything naming the architecture. The two
# values must therefore change in the same commit, and `terraform test` asserts
# the module's half so that a one-sided edit fails rather than deploys.
#
# ARM64 because Fargate charges ~20% less for it and the rest of the
# environment is already Graviton (db.t4g.micro, cache.t4g.micro) — the API and
# web tiers were the only x86 things left. It costs nothing here: CGO is off,
# so Go cross-compiles to it from the native builder below with no cross
# toolchain and no emulation.
ARG TARGET_ARCH=arm64

# ---------------------------------------------------------------------------
# Stage 1: build
# ---------------------------------------------------------------------------
# Pinned to $BUILDPLATFORM so the toolchain always runs natively and
# cross-compiles to TARGET_ARCH, rather than being emulated under qemu. With
# CGO disabled that is free — Go cross-compiles without a cross toolchain — and
# it is the difference between a one-minute build and a ten-minute one on a CI
# runner whose architecture does not match the target.
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

WORKDIR /src

# Manifest-only copy before the source, so editing a handler does not
# invalidate the module download layer.
COPY apps/api/go.mod apps/api/go.sum ./

# The cache mount survives between builds without landing in a layer.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# The whole module directory rather than an enumerated list, matching the web
# image's reasoning: an enumerated COPY is marginally better for cache reuse and
# considerably worse when someone adds a package and the image silently builds
# without it. This is load-bearing here in a way it is not for the web app —
# migrations/*.sql reach the binary through //go:embed, so a COPY that missed
# them would produce an image whose `api migrate up` is a no-op rather than a
# build failure. The root `.dockerignore` already strips `apps/api/bin`.
COPY apps/api/ ./

# The RDS trust anchors, carried across from the build context so the runtime
# stage can take them from a stage rather than from the context directly. See
# apps/api/certs/README.md for why this is vendored rather than curl'd here:
# `sslmode=verify-full` is a statement about which authorities are trusted, and
# that belongs in a reviewed commit rather than in whatever the network returns
# during a build.
COPY apps/api/certs/rds-global-bundle.pem /out/rds-global-bundle.pem

ARG TARGET_ARCH

# CGO_ENABLED=0 is what makes the runtime stage's choice possible: no libc, no
# dynamic loader, so the binary runs on a base with no userland at all. It also
# switches the standard library to the pure-Go DNS resolver, which is the
# behaviour you want in a container regardless — the cgo resolver reads an
# /etc/nsswitch.conf that will not exist.
#
# -trimpath strips build-host paths so the same source produces the same binary
# from any checkout. -s -w drop the symbol table and DWARF, which costs a
# readable panic trace nothing here consumes and saves roughly a quarter of the
# binary; stack traces still carry function names and line numbers.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGET_ARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api

# ---------------------------------------------------------------------------
# Stage 2: runtime
# ---------------------------------------------------------------------------
# distroless/static rather than alpine: no shell, no package manager, no busybox
# — a static Go binary needs none of them, and everything that is not there is
# something an attacker who gets command execution cannot use and Trivy cannot
# report. What it does carry is the short list a Go service actually needs:
# ca-certificates for outbound TLS (POSTGRES_SSLMODE=require, and ElastiCache's
# in-transit encryption once #112 lands), /etc/passwd and /etc/group with a
# `nonroot` entry, tzdata, and /tmp.
#
# ---------------------------------------------------------------------------
# THE CONSEQUENCE, STATED RATHER THAN DISCOVERED: NO CONTAINER HEALTHCHECK
# ---------------------------------------------------------------------------
# A Docker HEALTHCHECK — and an ECS container `healthCheck` — runs a command
# inside the container. There is no shell and no curl here, so neither is
# possible. apps/web can afford one because `node -e` is already present; this
# image cannot, and adding a health-probe binary purely to be probeable would
# trade the whole point of the base away.
#
# That is not a gap. modules/ecs/main.tf already omits `healthCheck` on all
# three API task definitions and says why, and the ALB target group's health
# check against /healthz answers the same question from outside — which is the
# answer the load balancer actually acts on. The one thing genuinely lost is
# `docker ps` reporting health for a locally run container; `curl` from the host
# against the published port covers that in the dev loop.
FROM --platform=linux/${TARGET_ARCH} ${RUNTIME_IMAGE}:${RUNTIME_TAG}@${RUNTIME_DIGEST} AS runtime

# `nonroot` (uid/gid 65532) ships with the base, so this mints no new identity —
# the same reasoning that made apps/web reuse node's uid 1000. Root in a
# container is still root on the host kernel for anything that escapes the
# namespace, and nothing this process does needs privilege: it binds 8080, not
# 80, and writes nothing to disk. The image is therefore compatible with a
# read-only root filesystem, should the task definition ever set one.
#
# Set explicitly rather than inherited from the `nonroot` tag, so switching the
# tag cannot silently promote this back to root.
USER nonroot:nonroot

# Owned by root and mode 0555: the running user needs to execute it and has no
# reason to be able to rewrite it.
COPY --from=builder --chown=root:root --chmod=0555 /out/api /usr/local/bin/api

# The Amazon RDS global certificate bundle, which is what makes
# POSTGRES_SSLMODE=verify-full reachable rather than merely documented. libpq
# resolves the CA from `sslrootcert`, defaulting to ~/.postgresql/root.crt —
# a path that does not exist in this image and never will, since there is no
# home directory and no way to write one. Without a file here, verify-full
# fails at connect time with "root certificate file does not exist", which
# reads like a broken image rather than a missing setting.
#
# Placed beside the base image's own `ca-certificates` rather than replacing
# them: those are for outbound TLS to AWS APIs and anything else the service
# calls, this one is consulted only when a DSN names it. Neither affects the
# other.
#
# 0444, not 0555: it is data, not a program, and nothing should execute it. Root
# owns it so the `nonroot` process can read it and cannot substitute it — a
# writable trust anchor is not a trust anchor.
#
# The path is referenced by POSTGRES_SSLROOTCERT in
# infra/terraform/modules/ecs/main.tf. Changing it here without changing it
# there produces a task that will not start.
COPY --from=builder --chown=root:root --chmod=0444 \
     /out/rds-global-bundle.pem /etc/ssl/certs/rds-global-bundle.pem

# Matches `api_container_port` in modules/ecs/variables.tf. Documentation only —
# the value that binds comes from HTTP_PORT, defaulted in internal/config.
#
# internal/config leaves HTTP_HOST empty, which produces a ":8080" listen
# address and binds every interface. Worth noting because the web image had to
# set HOSTNAME explicitly to avoid binding loopback and being unreachable from
# outside its network namespace while looking perfectly healthy; this binary
# does not have that failure mode.
EXPOSE 8080

LABEL org.opencontainers.image.title="collabboard-api" \
      org.opencontainers.image.description="CollabBoard API (Go, Gin + WebSocket hub)" \
      org.opencontainers.image.source="https://github.com/AndyV99/collabboard"

# ENTRYPOINT, not CMD, and that distinction is the whole reason the one-shot
# task definitions work.
#
# ECS's `command` maps to Docker's CMD and *replaces* it. With `CMD ["api"]` and
# no entrypoint, modules/ecs/main.tf's `command = ["migrate", "up"]` would ask
# the runtime to exec a program called `migrate`, which does not exist in this
# image — a task that dies immediately with an exec error naming a binary nobody
# wrote. With the binary as ENTRYPOINT, that same override arrives as argv to
# `api`, which is exactly what cmd/api/main.go dispatches on:
#
#     (no override)          -> api                serve HTTP
#     ["migrate", "up"]      -> api migrate up     apply migrations, exit
#     ["provision"]          -> api provision      set the serving role password
#
# Exec form so the binary is PID 1 and receives SIGTERM directly. This matters
# more here than it did for the web image: on SIGTERM, runServe drains the
# WebSocket hub before shutting the HTTP server down, sending every client a
# Going Away close with a jittered reconnect hint — the difference between a
# rolling deploy nobody notices and a synchronised reconnect storm. A shell-form
# ENTRYPOINT would put /bin/sh at PID 1 and swallow the signal, so the drain
# would never run and ECS would reset every live socket at the stopTimeout. It
# would also not work at all here: there is no shell in this image to be PID 1.
ENTRYPOINT ["/usr/local/bin/api"]

# Explicitly empty, so serving is the default mode and no CMD is inherited from
# the base image and silently appended as arguments to `api`.
CMD []
