package auth

// Small helpers the service needs that are not worth a file each.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// withoutTenant is [store.WithoutTenantValue] over the [Store] interface.
//
// The store package's own generic helper takes a *store.Store, which would
// force this package to depend on the concrete type and take a container to
// test. Go does not allow type parameters on methods, so the interface cannot
// carry a generic form of its own — hence one small function here rather than
// a variable declared outside every closure.
func withoutTenant[T any](
	ctx context.Context,
	s Store,
	reason store.IdentityReason,
	fn func(ctx context.Context, q store.IdentityQuerier) (T, error),
) (T, error) {
	var out T

	err := s.WithoutTenant(ctx, reason, func(ctx context.Context, q store.IdentityQuerier) error {
		var ferr error

		out, ferr = fn(ctx, q)

		return ferr
	})
	if err != nil {
		var zero T

		return zero, err
	}

	return out, nil
}

// slugSuffixBytes is the entropy appended to a generated slug.
const slugSuffixBytes = 4

// slugBodyLimit keeps the generated slug inside the column's CHECK constraint,
// which allows 2-63 characters matching ^[a-z0-9][a-z0-9-]{1,62}$.
const slugBodyLimit = 40

// newSlug turns an organization name into a URL slug with a random suffix.
//
// The suffix is not decoration. organizations_slug_key is globally unique, and
// uniqueness is enforced by an index, which is not subject to row-level
// security — so a colliding insert would tell the caller that *some other
// tenant* holds that slug. 00002_tenancy.sql accepts that disclosure for a slug
// the user typed, because it is the same thing any "workspace URL taken" check
// leaks. It is not acceptable for a slug the service generates on the user's
// behalf during registration, where the user never asked for that name and the
// only thing a collision would communicate is the existence of a stranger's
// organization. Random entropy makes the collision practically impossible
// instead.
func newSlug(name string) string {
	var b strings.Builder

	lastDash := false

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) && r < unicode.MaxASCII, unicode.IsDigit(r) && r < unicode.MaxASCII:
			b.WriteRune(r)

			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')

			lastDash = true
		}

		if b.Len() >= slugBodyLimit {
			break
		}
	}

	body := strings.Trim(b.String(), "-")

	suffix := make([]byte, slugSuffixBytes)
	if _, err := rand.Read(suffix); err != nil {
		// crypto/rand failing is not a condition this can degrade through, and
		// a slug is not a security boundary — but a predictable fallback would
		// reintroduce the collision this function exists to avoid, so the
		// caller sees the constraint violation rather than a silent duplicate.
		return ""
	}

	encoded := hex.EncodeToString(suffix)

	if body == "" {
		// Every character of a name can be non-ASCII, and the CHECK requires
		// the slug to start with [a-z0-9].
		return "org-" + encoded
	}

	return body + "-" + encoded
}
