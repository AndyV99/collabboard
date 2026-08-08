package auth

// Refresh tokens, and the reason they exist at all.
//
// An access token is a signed claim: nothing is consulted to validate it, which
// is what makes it cheap and what makes it impossible to revoke. A refresh
// token is the opposite — it is a lookup key into Redis, so deleting the record
// ends the session immediately. The pairing is the whole design: the access
// token is short-lived because it cannot be revoked, and the refresh token can
// be revoked because it is long-lived.
//
// # What is stored
//
// The raw token never touches Redis. The key is sha256 of it, exactly as a
// password is not stored in plaintext, because a Redis snapshot, a `KEYS *` in
// a debugging session or an exported RDB file would otherwise be a bag of live
// sessions. sha256 rather than a KDF is right here and wrong for a password:
// the token is 256 bits from crypto/rand, so there is no dictionary to attack
// and nothing to slow an attacker down against.
//
// # Rotation and reuse detection
//
// Every refresh mints a new token and deletes the old one. That bounds the
// value of a stolen refresh token to one use — but only if using it twice is
// *detectable*, otherwise a thief simply refreshes first and the victim's next
// refresh silently fails as though their session had expired.
//
// So each session also stores a pointer to its current token hash. Presenting a
// token whose hash is not the current one means either a replay or a race with
// the legitimate client, and both are answered the same way: the entire session
// is revoked and everyone has to log in again. That is the standard OAuth 2.1
// refresh-token-rotation response, and it is deliberately the paranoid choice —
// a false positive costs one login, a false negative costs the account.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Errors a refresh token can fail with.
var (
	// ErrRefreshUnknown means the token is not in the store: revoked, expired,
	// already rotated away, or never issued. All four are one answer on
	// purpose — telling a caller which would tell an attacker which.
	ErrRefreshUnknown = errors.New("auth: refresh token is not valid")

	// ErrRefreshReused means a token was presented that was superseded by a
	// rotation. The session it belonged to has been revoked by the time this is
	// returned.
	ErrRefreshReused = errors.New("auth: refresh token was already used")
)

// refreshTokenBytes is the entropy in a refresh token. 32 bytes is 256 bits,
// which is not guessable and leaves no reason to make it larger.
const refreshTokenBytes = 32

// Key prefixes. Namespaced so that a shared Redis — the compose stack also
// backs the job queue — cannot collide, and so that `SCAN auth:*` is a
// meaningful operational question.
const (
	refreshKeyPrefix = "auth:refresh:"
	sessionKeyPrefix = "auth:session:"
)

// Session is what a refresh token stands for: one login, on one device, in one
// organization.
//
// TenantID is here rather than being re-derived on every refresh so that the
// refresh path knows which membership to re-check. It is re-checked — see
// [Service.Refresh] — because refresh is the natural revalidation point: it is
// infrequent enough that a database round trip is free, and it is the only
// moment between login and expiry when the service gets to change its mind
// about a principal.
type Session struct {
	ID       uuid.UUID `json:"session_id"`
	UserID   uuid.UUID `json:"user_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	Role     string    `json:"role"`
	IssuedAt time.Time `json:"issued_at"`
}

// SessionStore issues, rotates and revokes refresh tokens.
type SessionStore struct {
	kv  KeyValue
	ttl time.Duration
	now func() time.Time
}

// NewSessionStore returns a store whose tokens live for ttl.
func NewSessionStore(kv KeyValue, ttl time.Duration) *SessionStore {
	return &SessionStore{kv: kv, ttl: ttl, now: time.Now}
}

// TTL reports the refresh token lifetime.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

// Issue starts a new session and returns its first refresh token.
//
// The raw token is returned once and never stored, so this is the only moment
// it exists outside the client.
func (s *SessionStore) Issue(ctx context.Context, session Session) (string, error) {
	if session.ID == uuid.Nil {
		session.ID = uuid.New()
	}

	session.IssuedAt = s.now()

	return s.mint(ctx, session)
}

// Rotate exchanges a refresh token for a new one, returning the session it
// belonged to.
//
// On reuse it revokes the session and returns [ErrRefreshReused]. The
// revocation happens before the error is returned, not after, so a caller that
// ignores the error still ends up with a dead session.
func (s *SessionStore) Rotate(ctx context.Context, raw string) (Session, string, error) {
	session, err := s.lookup(ctx, raw)
	if err != nil {
		return Session{}, "", err
	}

	// Delete before mint: if the mint fails, the old token is already gone and
	// the client has to log in again. The other order would leave two live
	// tokens for one session on a partial failure, which is the state reuse
	// detection exists to make impossible.
	if err := s.kv.Delete(ctx, refreshKey(raw)); err != nil {
		return Session{}, "", err
	}

	next, err := s.mint(ctx, session)
	if err != nil {
		return Session{}, "", err
	}

	return session, next, nil
}

// Revoke ends the session a refresh token belongs to.
//
// It revokes the whole session rather than just the presented token, because
// "log me out" means the session, and a token whose session pointer still
// existed would keep the rotation chain alive.
//
// An unknown token is not an error: logging out of a session that is already
// gone has achieved what the caller asked for, and reporting it would let an
// unauthenticated endpoint answer "was this token ever real".
func (s *SessionStore) Revoke(ctx context.Context, raw string) error {
	session, err := s.lookup(ctx, raw)
	if errors.Is(err, ErrRefreshUnknown) || errors.Is(err, ErrRefreshReused) {
		return nil
	}

	if err != nil {
		return err
	}

	return s.kv.Delete(ctx, refreshKey(raw), sessionKey(session.ID))
}

// RevokeSession ends a session by id, without holding one of its tokens. Used
// by the reuse-detection path and by anything that decides a principal should
// stop being one — a membership disappearing on refresh, for instance.
func (s *SessionStore) RevokeSession(ctx context.Context, sessionID uuid.UUID) error {
	current, err := s.kv.Get(ctx, sessionKey(sessionID))
	if errors.Is(err, ErrKeyNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	// The pointer holds the *key* of the live token, so the token itself can be
	// deleted without ever having been seen.
	return s.kv.Delete(ctx, string(current), sessionKey(sessionID))
}

// Lookup returns the session a refresh token belongs to without consuming it.
func (s *SessionStore) Lookup(ctx context.Context, raw string) (Session, error) {
	return s.lookup(ctx, raw)
}

func (s *SessionStore) lookup(ctx context.Context, raw string) (Session, error) {
	if raw == "" {
		return Session{}, ErrRefreshUnknown
	}

	key := refreshKey(raw)

	encoded, err := s.kv.Get(ctx, key)
	if errors.Is(err, ErrKeyNotFound) {
		return Session{}, ErrRefreshUnknown
	}

	if err != nil {
		return Session{}, err
	}

	var session Session
	if err := json.Unmarshal(encoded, &session); err != nil {
		return Session{}, fmt.Errorf("decoding session record: %w", err)
	}

	current, err := s.kv.Get(ctx, sessionKey(session.ID))
	if errors.Is(err, ErrKeyNotFound) {
		// The record survived its own session pointer. Treat as revoked rather
		// than as valid: the pointer is what a revocation deletes.
		return Session{}, ErrRefreshUnknown
	}

	if err != nil {
		return Session{}, err
	}

	if string(current) != key {
		// Reuse. Kill the session — including whichever token is currently
		// live, which may be in the hands of either the thief or the victim.
		if rerr := s.RevokeSession(ctx, session.ID); rerr != nil {
			return Session{}, errors.Join(ErrRefreshReused, rerr)
		}

		if derr := s.kv.Delete(ctx, key); derr != nil {
			return Session{}, errors.Join(ErrRefreshReused, derr)
		}

		return Session{}, ErrRefreshReused
	}

	return session, nil
}

// mint writes a fresh token for an existing session and repoints the session at
// it.
func (s *SessionStore) mint(ctx context.Context, session Session) (string, error) {
	raw, err := newRefreshToken()
	if err != nil {
		return "", err
	}

	encoded, err := json.Marshal(session)
	if err != nil {
		return "", fmt.Errorf("encoding session record: %w", err)
	}

	key := refreshKey(raw)

	if err := s.kv.Set(ctx, key, encoded, s.ttl); err != nil {
		return "", err
	}

	// The pointer gets the same ttl as the record it points at, so a session
	// cannot outlive its own token and leave an unrevocable pointer behind.
	if err := s.kv.Set(ctx, sessionKey(session.ID), []byte(key), s.ttl); err != nil {
		return "", err
	}

	return raw, nil
}

// newRefreshToken returns a URL-safe, unpadded base64 encoding of 256 random
// bits.
func newRefreshToken() (string, error) {
	buf := make([]byte, refreshTokenBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a refresh token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// refreshKey is the storage key for a raw token: a prefix and the hex sha256 of
// the token, so the token itself is never a substring of anything stored.
func refreshKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return refreshKeyPrefix + hex.EncodeToString(sum[:])
}

func sessionKey(id uuid.UUID) string {
	return sessionKeyPrefix + id.String()
}
