# Vendored trust anchors

## `rds-global-bundle.pem`

The Amazon RDS global certificate bundle: 108 root certificates, one set per
AWS region. `apps/api/Dockerfile` copies it into the runtime image at
`/etc/ssl/certs/rds-global-bundle.pem`, and `modules/ecs` points
`POSTGRES_SSLROOTCERT` at that path so `sslmode=verify-full` has a CA to
verify against.

**Source:** <https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem>
**SHA-256:** `e5bb2084ccf45087bda1c9bffdea0eb15ee67f0b91646106e466714f9de3c7e3`
**Retrieved:** 2026-08-28

### Why this is vendored rather than downloaded during the build

Both are defensible; this is the choice and the reason, so a reviewer does not
have to reconstruct it.

`verify-full` means "trust exactly these certificate authorities". That is a
trust decision, and a trust decision belongs in a reviewed commit rather than
in whatever the network returns at build time. Vendored, the anchor set is
visible in a diff, the build needs no network, and a rotation arrives as a pull
request somebody read — the same argument the Dockerfile already makes for
pinning its base image to a digest instead of a floating tag.

The cost is 165 KB in the repository and a refresh nobody is reminded to do.
AWS rotates these rarely (the last generation change was `rds-ca-2019` to
`rds-ca-rsa2048-g1`), and an expired anchor fails loudly at connect time rather
than silently, so the failure mode is acceptable.

### Why the global bundle rather than the region's

`us-east-1-bundle.pem` is a few kilobytes and trusts one region's CA instead of
108. It is the tighter choice and it is deliberately not taken: the Dockerfile's
own rule is that the same image tag must be promotable `dev -> staging -> prod`
unchanged, and a region-specific bundle bakes a region into the artifact. The
extra anchors are Amazon's own RDS roots, not third parties, so the widening is
small and bounded.

### Refreshing it

```bash
curl -fsSL -o apps/api/certs/rds-global-bundle.pem \
  https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
sha256sum apps/api/certs/rds-global-bundle.pem   # update the value above
grep -c 'BEGIN CERTIFICATE' apps/api/certs/rds-global-bundle.pem
```

Then rebuild the image. Nothing else changes — the path in the image and the
task definition are stable.
