package auth

// Password derivation.
//
// The KDF runs here, in the application, and never in Postgres — see
// docs/adr/0003-password-verifier-storage.md. The short version: argon2id at
// useful parameters costs tens of milliseconds of CPU and ~19 MiB of memory per
// derivation, and the database is the one component in this system that cannot
// be scaled horizontally. Putting deliberately expensive work on a pooled
// Postgres connection turns a handful of concurrent logins into a service-wide
// stall.
//
// What Postgres does instead is one sha256 over the derived key, which is
// microseconds, and the comparison. The application therefore never receives
// the stored value and never computes it.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the cost parameters and output width of one derivation.
//
// They are stored per credential rather than assumed globally, so raising them
// re-derives each account on its next successful login instead of locking every
// existing account out. The zero value is not usable; [Argon2Params.Validate]
// says why.
type Argon2Params struct {
	// MemoryKiB is argon2id's memory cost, in kibibytes. This is the parameter
	// that does the work: it is what makes a GPU or ASIC attack expensive
	// rather than merely parallel.
	MemoryKiB uint32

	// Iterations is argon2id's time cost.
	Iterations uint32

	// Parallelism is the number of lanes. RFC 9106's recommendations assume
	// the memory is divided across them, so raising this without raising
	// MemoryKiB weakens the derivation rather than strengthening it.
	Parallelism uint8

	// KeyLength is the width of the derived key in bytes.
	KeyLength uint32

	// SaltLength is the width of a freshly generated salt in bytes. It is not
	// part of the derivation — the salt's length is implied by the salt — but
	// it belongs with the other parameters because it is configured with them.
	SaltLength uint32
}

// RFC 9106 gives two recommended parameter sets. These are the second — 19 MiB,
// 2 passes, 1 lane — which is the one intended for memory-constrained
// environments and is a much better fit for a small Fargate task than the
// first (2 GiB). The floors in migration 00005's CHECK constraints match.
const (
	DefaultArgon2MemoryKiB   = 19456
	DefaultArgon2Iterations  = 2
	DefaultArgon2Parallelism = 1
	DefaultArgon2KeyLength   = 32
	DefaultArgon2SaltLength  = 16
)

// DefaultArgon2Params is the parameter set used for new credentials when
// nothing overrides it.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{
		MemoryKiB:   DefaultArgon2MemoryKiB,
		Iterations:  DefaultArgon2Iterations,
		Parallelism: DefaultArgon2Parallelism,
		KeyLength:   DefaultArgon2KeyLength,
		SaltLength:  DefaultArgon2SaltLength,
	}
}

// ErrWeakArgon2Params means the configured parameters are below the floor this
// service will operate at.
var ErrWeakArgon2Params = errors.New("auth: argon2id parameters are below the supported floor")

// Validate rejects parameters weaker than the floors migration 00005 enforces
// in SQL.
//
// Checked in both places on purpose. The database CHECK is the one that cannot
// be bypassed by a code path; this one is the one that fails at startup with a
// message naming the setting, instead of at the first registration with a
// constraint violation.
func (p Argon2Params) Validate() error {
	switch {
	case p.MemoryKiB < DefaultArgon2MemoryKiB:
		return fmt.Errorf("%w: memory is %d KiB, minimum %d", ErrWeakArgon2Params, p.MemoryKiB, DefaultArgon2MemoryKiB)
	case p.Iterations < DefaultArgon2Iterations:
		return fmt.Errorf("%w: %d iterations, minimum %d", ErrWeakArgon2Params, p.Iterations, DefaultArgon2Iterations)
	case p.Parallelism < 1:
		return fmt.Errorf("%w: parallelism must be at least 1", ErrWeakArgon2Params)
	case p.KeyLength < DefaultArgon2KeyLength:
		return fmt.Errorf("%w: key length is %d, minimum %d", ErrWeakArgon2Params, p.KeyLength, DefaultArgon2KeyLength)
	case p.SaltLength < DefaultArgon2SaltLength:
		return fmt.Errorf("%w: salt length is %d, minimum %d", ErrWeakArgon2Params, p.SaltLength, DefaultArgon2SaltLength)
	default:
		return nil
	}
}

// Deriver turns a password and a salt into a key.
//
// An interface rather than a concrete type so tests can count derivations. The
// anti-enumeration property in [Service.Login] is "exactly one derivation
// happens whether or not the account exists", and the cheapest honest way to
// assert that is to substitute a Deriver that counts.
type Deriver interface {
	Derive(ctx context.Context, password string, salt []byte, params Argon2Params) ([]byte, error)
}

// Argon2Deriver derives keys with argon2id, admitting a bounded number of
// derivations at a time.
//
// The bound is not tuning. Login performs a derivation whether or not the
// account exists — that is the anti-enumeration requirement, so it cannot be
// skipped — which makes ~19 MiB per in-flight login an availability surface. A
// semaphore turns "enough concurrent logins to exhaust memory" into a queue and
// then, once the request context expires, into 503s and 429s. An unbounded
// version of this is an OOM kill waiting for a slow afternoon.
type Argon2Deriver struct {
	slots chan struct{}
}

// NewArgon2Deriver returns a deriver admitting maxConcurrent derivations at
// once. A non-positive value is treated as 1.
func NewArgon2Deriver(maxConcurrent int) *Argon2Deriver {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}

	return &Argon2Deriver{slots: make(chan struct{}, maxConcurrent)}
}

// Derive computes the argon2id key for password under salt and params.
//
// It blocks while every slot is taken, and returns ctx.Err() if the caller
// gives up first — so a client that hangs up frees the queue slot it was
// waiting for instead of holding it until the derivation it never wanted
// finishes.
func (d *Argon2Deriver) Derive(ctx context.Context, password string, salt []byte, params Argon2Params) ([]byte, error) {
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for an argon2id slot: %w", ctx.Err())
	}

	// argon2.IDKey is not context aware and does not allocate incrementally, so
	// there is nothing to cancel once it starts. At these parameters it is tens
	// of milliseconds; the cancellation that matters is the queue wait above.
	return argon2.IDKey(
		[]byte(password),
		salt,
		params.Iterations,
		params.MemoryKiB,
		params.Parallelism,
		params.KeyLength,
	), nil
}

// NewSalt returns a fresh salt of the configured width.
func NewSalt(params Argon2Params) ([]byte, error) {
	salt := make([]byte, params.SaltLength)

	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating a salt: %w", err)
	}

	return salt, nil
}

// derivationBudget bounds how long a caller will wait for a derivation slot
// before giving up, when the caller's own context has no deadline. It exists so
// that a queue built up by a burst drains into errors rather than growing until
// the process runs out of memory holding the requests.
const derivationBudget = 5 * time.Second
