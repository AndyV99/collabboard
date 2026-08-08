package auth_test

// Refresh tokens: rotation, revocation, and reuse detection.
//
// The claims worth writing down are the negative ones. That a fresh token works
// is table stakes; that the *previous* one stops working, that revoking kills
// both, and that presenting a rotated-away token takes the whole session down
// with it, are the properties a stolen token runs into.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

const testRefreshTTL = 24 * time.Hour

func newTestSession() auth.Session {
	return auth.Session{
		ID:       uuid.New(),
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Role:     "member",
	}
}

func TestARefreshTokenRoundTrips(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kv := newMemoryKV()
	sessions := auth.NewSessionStore(kv, testRefreshTTL)
	want := newTestSession()

	raw, err := sessions.Issue(ctx, want)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := sessions.Lookup(ctx, raw)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	t.Logf("issued a token of %d characters for session %s", len(raw), got.ID)

	if got.ID != want.ID || got.UserID != want.UserID || got.TenantID != want.TenantID || got.Role != want.Role {
		t.Errorf("Lookup = %+v, want %+v", got, want)
	}
}

// TestTheRawTokenIsNeverAKey is the claim that a Redis snapshot is not a bag of
// live sessions.
func TestTheRawTokenIsNeverAKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kv := newMemoryKV()
	sessions := auth.NewSessionStore(kv, testRefreshTTL)

	raw, err := sessions.Issue(ctx, newTestSession())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	kv.mu.Lock()
	defer kv.mu.Unlock()

	for key, entry := range kv.values {
		t.Logf("stored key %s", key)

		if strings.Contains(key, raw) {
			t.Errorf("the raw refresh token appears in key %q; anyone reading the keyspace holds live sessions", key)
		}

		if strings.Contains(string(entry.value), raw) {
			t.Errorf("the raw refresh token appears in the value at %q", key)
		}
	}
}

func TestRotationInvalidatesThePreviousToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessions := auth.NewSessionStore(newMemoryKV(), testRefreshTTL)
	original := newTestSession()

	first, err := sessions.Issue(ctx, original)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	rotatedSession, second, err := sessions.Rotate(ctx, first)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if rotatedSession.ID != original.ID {
		t.Errorf("rotation changed the session id: %s, want %s", rotatedSession.ID, original.ID)
	}

	if second == first {
		t.Fatal("rotation returned the same token")
	}

	if _, err := sessions.Lookup(ctx, second); err != nil {
		t.Errorf("the rotated token does not work: %v", err)
	}
}

// TestPresentingARotatedTokenRevokesTheWholeSession is the reuse-detection
// claim, and the reason rotation is worth doing at all.
//
// Without it, a thief who refreshes first simply wins: the victim's next
// refresh fails and looks like an ordinary expiry, and nobody learns anything.
// With it, the second presentation kills the session — including the token the
// thief just received.
func TestPresentingARotatedTokenRevokesTheWholeSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessions := auth.NewSessionStore(newMemoryKV(), testRefreshTTL)

	first, err := sessions.Issue(ctx, newTestSession())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, second, err := sessions.Rotate(ctx, first)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// The replay.
	_, _, err = sessions.Rotate(ctx, first)

	t.Logf("re-presenting the rotated token -> %v", err)

	if !errors.Is(err, auth.ErrRefreshReused) {
		t.Fatalf("Rotate with a used token = %v, want %v", err, auth.ErrRefreshReused)
	}

	// And the live token is dead too. This is the part that matters: detection
	// without revocation would just be a log line.
	_, err = sessions.Lookup(ctx, second)

	t.Logf("the token that was live at the time of the replay -> %v", err)

	if !errors.Is(err, auth.ErrRefreshUnknown) {
		t.Errorf("the live token still works after reuse detection: %v", err)
	}
}

func TestRevokeEndsTheSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kv := newMemoryKV()
	sessions := auth.NewSessionStore(kv, testRefreshTTL)

	raw, err := sessions.Issue(ctx, newTestSession())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := sessions.Revoke(ctx, raw); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, err = sessions.Lookup(ctx, raw)

	t.Logf("after Revoke -> %v; %d keys left in the store", err, kv.len())

	if !errors.Is(err, auth.ErrRefreshUnknown) {
		t.Errorf("Lookup after Revoke = %v, want %v", err, auth.ErrRefreshUnknown)
	}

	// Both the record and the session pointer go, or a later rotation would
	// find a dangling pointer.
	if kv.len() != 0 {
		t.Errorf("%d keys survived revocation", kv.len())
	}
}

// TestRevokingAnUnknownTokenIsNotAnError keeps logout from being an oracle:
// an unauthenticated endpoint that answered differently for a real token and a
// made-up one would tell an attacker which is which.
func TestRevokingAnUnknownTokenIsNotAnError(t *testing.T) {
	t.Parallel()

	sessions := auth.NewSessionStore(newMemoryKV(), testRefreshTTL)

	for _, token := range []string{"", "not-a-token", uuid.NewString()} {
		if err := sessions.Revoke(context.Background(), token); err != nil {
			t.Errorf("Revoke(%q) = %v, want nil", token, err)
		}
	}
}

func TestRevokeSessionKillsATokenItNeverSaw(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessions := auth.NewSessionStore(newMemoryKV(), testRefreshTTL)
	session := newTestSession()

	raw, err := sessions.Issue(ctx, session)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := sessions.RevokeSession(ctx, session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	_, err = sessions.Lookup(ctx, raw)

	t.Logf("after RevokeSession(%s) -> %v", session.ID, err)

	if !errors.Is(err, auth.ErrRefreshUnknown) {
		t.Errorf("Lookup = %v, want %v", err, auth.ErrRefreshUnknown)
	}
}

// TestARefreshTokenExpires is what makes the long-lived half of the pair
// bounded. Asserted by moving the store's clock rather than by sleeping.
func TestARefreshTokenExpires(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	kv := newMemoryKV()
	sessions := auth.NewSessionStore(kv, time.Hour)

	raw, err := sessions.Issue(ctx, newTestSession())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	base := time.Now()

	kv.mu.Lock()
	kv.now = func() time.Time { return base.Add(2 * time.Hour) }
	kv.mu.Unlock()

	_, err = sessions.Lookup(ctx, raw)

	t.Logf("two hours after a one-hour token was issued -> %v", err)

	if !errors.Is(err, auth.ErrRefreshUnknown) {
		t.Errorf("Lookup after expiry = %v, want %v", err, auth.ErrRefreshUnknown)
	}
}

// TestTwoTokensAreNeverTheSame is a cheap guard on the generator: a refresh
// token that repeats is a session anyone can join.
func TestTwoTokensAreNeverTheSame(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessions := auth.NewSessionStore(newMemoryKV(), testRefreshTTL)

	seen := map[string]bool{}

	for range 64 {
		raw, err := sessions.Issue(ctx, newTestSession())
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}

		if seen[raw] {
			t.Fatalf("the generator repeated a token after %d issues", len(seen))
		}

		seen[raw] = true
	}

	t.Logf("%d distinct tokens", len(seen))
}
