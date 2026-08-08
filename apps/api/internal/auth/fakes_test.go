package auth_test

// Test doubles shared by the unit tests in this package.
//
// Two of them are interesting rather than incidental.
//
// countingDeriver exists because the anti-enumeration property is "exactly one
// argon2id derivation happens whether or not the account exists". That is a
// statement about *what the code does*, and counting is a direct assertion of
// it. A wall-clock comparison would be an indirect one that flakes on a shared
// CI runner, and the integration suite has a loose timing check as a backstop
// for the case where some other step becomes asymmetric.
//
// fakeStore exists because the store's own door is a container away, and the
// login flow's branching — account present, account absent, account present
// with no password — is worth testing at every branch rather than at the two
// the fixture happens to have.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// memoryKV is an in-memory [auth.KeyValue] with real expiry semantics.
type memoryKV struct {
	mu     sync.Mutex
	values map[string]memoryEntry

	// now is settable so a test can move past a ttl without sleeping.
	now func() time.Time

	// failWith, when set, makes every operation fail. Used to exercise the
	// rate limiter's fail-open behaviour.
	failWith error
}

type memoryEntry struct {
	value   []byte
	expires time.Time
}

func newMemoryKV() *memoryKV {
	return &memoryKV{values: map[string]memoryEntry{}, now: time.Now}
}

func (m *memoryKV) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return nil, m.failWith
	}

	entry, ok := m.values[key]
	if !ok || !entry.expires.After(m.now()) {
		return nil, auth.ErrKeyNotFound
	}

	return entry.value, nil
}

func (m *memoryKV) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return m.failWith
	}

	if ttl <= 0 {
		return errors.New("memoryKV: non-positive ttl")
	}

	m.values[key] = memoryEntry{value: value, expires: m.now().Add(ttl)}

	return nil
}

func (m *memoryKV) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return m.failWith
	}

	for _, key := range keys {
		delete(m.values, key)
	}

	return nil
}

// Increment mirrors Redis's INCR + EXPIRE NX: the expiry is set once, when the
// counter is created, so the window is fixed rather than sliding.
func (m *memoryKV) Increment(_ context.Context, key string, ttl time.Duration) (int64, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWith != nil {
		return 0, 0, m.failWith
	}

	now := m.now()

	entry, ok := m.values[key]
	if !ok || !entry.expires.After(now) {
		entry = memoryEntry{value: []byte("0"), expires: now.Add(ttl)}
	}

	count := int64(0)
	for _, b := range entry.value {
		count = count*10 + int64(b-'0')
	}

	count++

	entry.value = []byte(formatInt(count))
	m.values[key] = entry

	return count, entry.expires.Sub(now), nil
}

func (m *memoryKV) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.values)
}

func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}

	var digits []byte

	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}

	return string(digits)
}

// countingDeriver wraps a real deriver and records how many derivations it was
// asked for, with which salts.
type countingDeriver struct {
	mu    sync.Mutex
	calls []derivation

	// inner does the real work. A real argon2id is used rather than a stub so
	// that the timing test measures something.
	inner auth.Deriver
}

type derivation struct {
	salt     []byte
	password string
}

func newCountingDeriver() *countingDeriver {
	return &countingDeriver{inner: auth.NewArgon2Deriver(4)}
}

func (c *countingDeriver) Derive(ctx context.Context, password string, salt []byte, params auth.Argon2Params) ([]byte, error) {
	c.mu.Lock()
	c.calls = append(c.calls, derivation{salt: append([]byte(nil), salt...), password: password})
	c.mu.Unlock()

	return c.inner.Derive(ctx, password, salt, params)
}

func (c *countingDeriver) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.calls)
}

func (c *countingDeriver) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = nil
}

// fakeStore implements auth.Store over maps, standing in for both doors.
//
// It is not a general database fake: it implements exactly the seven pre-tenant
// queries and the three tenant-scoped ones the auth flows use, and it panics on
// anything else, so a test cannot silently exercise a path this fake does not
// actually model.
type fakeStore struct {
	mu sync.Mutex

	usersByEmail map[string]store.IdentityUser
	usersByID    map[uuid.UUID]store.IdentityUser
	credentials  map[uuid.UUID]fakeCredential
	memberships  map[uuid.UUID][]store.UserOrganization

	// reasons records every reason the pre-tenant door was opened with, which
	// is what makes "the audit trail is real" testable.
	reasons []string

	// tenants records every tenant WithTenant was opened for. The BOLA test
	// reads it: proving a request "cannot reach tenant B" is stronger when it
	// also shows tenant B's id was never set as a tenant context in the first
	// place.
	tenants []uuid.UUID

	failWith error
}

type fakeCredential struct {
	params store.PasswordKDFParams

	// verifier is sha256 of the derived key, exactly as the database stores it.
	verifier [32]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		usersByEmail: map[string]store.IdentityUser{},
		usersByID:    map[uuid.UUID]store.IdentityUser{},
		credentials:  map[uuid.UUID]fakeCredential{},
		memberships:  map[uuid.UUID][]store.UserOrganization{},
	}
}

func (f *fakeStore) WithoutTenant(ctx context.Context, reason store.IdentityReason, fn store.IdentityFunc) error {
	f.mu.Lock()
	f.reasons = append(f.reasons, reason.String())
	failure := f.failWith
	f.mu.Unlock()

	if failure != nil {
		return failure
	}

	return fn(ctx, fakeIdentityQuerier{store: f})
}

func (f *fakeStore) WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error {
	f.mu.Lock()
	f.tenants = append(f.tenants, tenantID)
	failure := f.failWith
	f.mu.Unlock()

	if failure != nil {
		return failure
	}

	return fn(ctx, fakeTenantQuerier{store: f, tenantID: tenantID})
}

func (f *fakeStore) tenantsOpened() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]uuid.UUID(nil), f.tenants...)
}

func (f *fakeStore) reasonsUsed() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return strings.Join(f.reasons, ",")
}
