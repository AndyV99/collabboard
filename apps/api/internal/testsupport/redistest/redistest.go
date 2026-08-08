// Package redistest is the integration-test harness for Redis: a throwaway
// server in a container, and a client pointed at it.
//
// # Why a container rather than a fake
//
// internal/auth's KeyValue interface has an in-memory implementation used by
// the unit tests, and it is enough for the *logic* — rotation, reuse detection,
// budgets. It is not enough for the claims that matter operationally, because
// they are claims about Redis: that a refresh token actually expires, that
// EXPIRE NX makes a rate-limit window fixed rather than sliding, that a
// TxPipeline returns the values the limiter reads. A fake that agreed with the
// code would prove the code agrees with itself.
//
// The vault's Standards/Testing Strategy.md asks for real dependencies in
// integration tests, and this is the same reasoning pgtest applies to Postgres.
//
// # Build tag
//
// Everything else here is behind the `integration` tag, so `go build ./...` and
// `go vet ./...` still find a package rather than failing with "build
// constraints exclude all Go files".
package redistest
