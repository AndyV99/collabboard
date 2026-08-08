package auth

// Rate limiting on login.
//
// Two counters per attempt, and the pair is the point.
//
//   - **Per account.** The tight one. Credential stuffing works by trying a
//     handful of common passwords against a very large list of addresses, so
//     the signal is attempts against *one* address. The key is a keyed hash of
//     the address rather than the address, so a Redis dump is not a list of
//     everyone who has tried to log in.
//   - **Per client address.** The loose one. It catches the other shape — one
//     source walking a list of addresses — and it has to be loose because a
//     corporate NAT or a mobile carrier puts thousands of legitimate users
//     behind one address. On its own it would either be useless or lock out an
//     office; paired with the per-account limit it does not have to carry the
//     load.
//
// Both counters are incremented on every attempt, successful or not. Counting
// only failures would let an attacker with one valid credential reset their own
// budget, and it would leak: an endpoint that lets you keep trying is telling
// you the last password was right.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrRateLimited means an attempt was refused before any credential work
// happened.
var ErrRateLimited = errors.New("auth: too many attempts")

// RateLimitError carries how long the caller has to wait.
//
// A typed error rather than a wrapped sentinel with the duration in the
// message, because the HTTP layer has to put the number in a Retry-After header
// and parsing it back out of a string would be silly. Unwrap keeps
// errors.Is(err, ErrRateLimited) working for callers that only care that it
// happened.
type RateLimitError struct {
	RetryAfter time.Duration
}

// Error implements error.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: retry in %s", ErrRateLimited.Error(), e.RetryAfter.Round(time.Second))
}

// Unwrap makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// RateLimitConfig is the pair of budgets and the window they share.
type RateLimitConfig struct {
	// PerAccount is how many attempts one email address gets per window.
	PerAccount int

	// PerAddress is how many attempts one client IP gets per window.
	PerAddress int

	// Window is how long a budget lasts. Fixed rather than sliding — see
	// KeyValue.Increment.
	Window time.Duration
}

// Validate rejects a configuration that would disable the limiter by accident.
func (c RateLimitConfig) Validate() error {
	switch {
	case c.PerAccount < 1:
		return errors.New("auth: per-account login limit must be at least 1")
	case c.PerAddress < 1:
		return errors.New("auth: per-address login limit must be at least 1")
	case c.Window <= 0:
		return errors.New("auth: login rate limit window must be positive")
	default:
		return nil
	}
}

// Decision is the outcome of a limit check.
type Decision struct {
	// Allowed is false when either budget is exhausted.
	Allowed bool

	// RetryAfter is how long until the exhausted budget resets. Zero when
	// allowed.
	RetryAfter time.Duration

	// Scope names which budget refused, for the log line. Never returned to the
	// client: "you are limited on this account" and "you are limited on this
	// address" is an oracle for whether an address has other traffic.
	Scope string
}

// Limiter counts login attempts.
type Limiter struct {
	kv     KeyValue
	cfg    RateLimitConfig
	pepper []byte
	logger *slog.Logger
}

// NewLimiter returns a limiter. pepper keys the hash of the account identifier;
// it is derived from the service's signing secret rather than configured
// separately, so there is one secret to rotate.
func NewLimiter(kv KeyValue, cfg RateLimitConfig, pepper []byte, logger *slog.Logger) *Limiter {
	return &Limiter{kv: kv, cfg: cfg, pepper: pepper, logger: logger}
}

// Allow counts one attempt against both budgets and reports whether it may
// proceed.
//
// # Failing open
//
// If Redis is unreachable, this logs and allows. That is a deliberate choice
// and the argument for it is narrower than "availability beats security": the
// same Redis backs the refresh-token store, so a login cannot *succeed* while
// it is down — [Service.Login] needs to write a session record and will fail.
// Failing closed here would therefore trade nothing for nothing while making
// the failure report as "too many attempts", which is the least useful thing an
// operator could be told. It is called out here because the reasoning stops
// holding the moment something else can issue a session.
func (l *Limiter) Allow(ctx context.Context, email, clientIP string) Decision {
	budgets := []struct {
		scope string
		key   string
		limit int
	}{
		{scope: "account", key: "auth:login:account:" + l.accountHash(email), limit: l.cfg.PerAccount},
		{scope: "address", key: "auth:login:address:" + clientIP, limit: l.cfg.PerAddress},
	}

	decision := Decision{Allowed: true}

	for _, budget := range budgets {
		if budget.key == "" {
			continue
		}

		count, remaining, err := l.kv.Increment(ctx, budget.key, l.cfg.Window)
		if err != nil {
			l.logger.WarnContext(ctx, "login rate limiter unavailable, allowing the attempt",
				slog.String("scope", budget.scope),
				slog.Any("error", err),
			)

			continue
		}

		if count > int64(budget.limit) {
			// Both budgets are still incremented — the loop does not break —
			// so an attacker cannot keep one counter cold by tripping the
			// other first.
			decision.Allowed = false

			if remaining > decision.RetryAfter {
				decision.RetryAfter = remaining
				decision.Scope = budget.scope
			}
		}
	}

	return decision
}

// accountHash is a keyed hash of the normalised address.
//
// Keyed rather than a bare sha256: an unkeyed hash of an email address is
// reversible by anyone with a wordlist of addresses, which is every attacker.
// With the pepper, a Redis dump is a set of opaque counters.
func (l *Limiter) accountHash(email string) string {
	mac := hmac.New(sha256.New, l.pepper)
	mac.Write([]byte(NormalizeEmail(email)))

	return hex.EncodeToString(mac.Sum(nil))
}

// NormalizeEmail is the canonical form used for lookups, uniqueness and rate
// limiting: trimmed and lowercased.
//
// It matches users_email_key, which is UNIQUE on lower(email), and the
// identity functions, which compare lower(btrim(...)). Doing it in one place
// means the rate limiter and the lookup cannot disagree about whether
// "A@b.com" and "a@b.com " are the same account — if they could, the per-
// account budget would be trivially bypassed by changing the capitalisation.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
