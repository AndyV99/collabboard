package store_test

// The pre-tenant door, checked without a database.
//
// Two kinds of claim live here. The argument checks are ordinary: they stop a
// wiring mistake reaching Postgres. The rest are the interesting ones — they
// assert the *width* of the door, which is a property of the types rather than
// of any behaviour, and which nothing else in the build would notice changing.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// identityQueries is the complete pre-tenant surface, spelled out.
//
// Kept here rather than derived, because a test that read the method set and
// compared it to itself would pass no matter what the method set became. Adding
// a fifth pre-tenant query has to break this test: that is the whole point of
// writing the list down.
var identityQueries = []string{
	"CreateUser",
	"FindUserByEmail",
	"ListUserOrganizations",
	"ResolveUserIDByEmail",
}

func TestWithoutTenantRejectsBadArgumentsBeforeTouchingTheDatabase(t *testing.T) {
	ctx := context.Background()
	noop := func(context.Context, store.IdentityQuerier) error { return nil }

	for _, tc := range []struct {
		name   string
		store  *store.Store
		reason store.IdentityReason
		fn     store.IdentityFunc
		want   error
	}{
		{
			name:   "nil store",
			store:  nil,
			reason: store.ReasonLogin,
			fn:     noop,
			want:   store.ErrNilPool,
		},
		{
			name:   "store with no pool",
			store:  store.New(nil),
			reason: store.ReasonLogin,
			fn:     noop,
			want:   store.ErrNilPool,
		},
		{
			name:   "nil callback",
			store:  store.New(nil),
			reason: store.ReasonLogin,
			fn:     nil,
			want:   store.ErrNilPool,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.store.WithoutTenant(ctx, tc.reason, tc.fn)

			t.Logf("%s -> %v", tc.name, err)

			if !errors.Is(err, tc.want) {
				t.Errorf("WithoutTenant = %v, want %v", err, tc.want)
			}
		})
	}

	// The zero IdentityReason is refused too, but only once a pool exists — the
	// wiring checks above run first, so asserting it here would pass for the
	// wrong reason. TestWithoutTenantRefusesTheZeroReason in pretenant_test.go
	// makes that claim against a real pool.
}

// TestEveryReasonIsDistinctAndNamed keeps the audit log meaningful: two reasons
// that stringify the same would be indistinguishable in a log, and a blank one
// would say nothing at all.
func TestEveryReasonIsDistinctAndNamed(t *testing.T) {
	reasons := map[string]store.IdentityReason{
		"ReasonLogin":             store.ReasonLogin,
		"ReasonListOrganizations": store.ReasonListOrganizations,
		"ReasonInviteLookup":      store.ReasonInviteLookup,
		"ReasonRegisterUser":      store.ReasonRegisterUser,
	}

	seen := make(map[string]string, len(reasons))

	for name, reason := range reasons {
		got := reason.String()

		t.Logf("%s -> %q", name, got)

		if got == "" || got == "unspecified" {
			t.Errorf("%s stringifies to %q; it would be useless in an audit log", name, got)
		}

		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both stringify to %q", name, prev, got)
		}

		seen[got] = name
	}
}

// TestTheIdentityQuerierExposesExactlyThePreTenantQueries is the width of the
// door, asserted.
//
// Everything about the pre-tenant path's safety rests on this set being small
// and every member of it being individually justified in identity_query.sql. A
// fifth method appearing here is not necessarily wrong — but it must not happen
// without someone editing this list and, in doing so, being asked why.
func TestTheIdentityQuerierExposesExactlyThePreTenantQueries(t *testing.T) {
	typ := reflect.TypeOf((*store.IdentityQuerier)(nil)).Elem()

	got := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}

	slices.Sort(got)

	t.Logf("store.IdentityQuerier exposes %v", got)

	if !slices.Equal(got, identityQueries) {
		t.Errorf("store.IdentityQuerier = %v, want exactly %v — the pre-tenant path changed width", got, identityQueries)
	}
}

// TestTheTwoQueriersShareNoMethods is the structural claim underneath "the
// pre-tenant path cannot reach tenant-scoped data".
//
// If a method name ever appeared on both interfaces, code written against one
// would compile against the other, and the compile error this path relies on
// would quietly become a type assertion away.
func TestTheTwoQueriersShareNoMethods(t *testing.T) {
	tenant := reflect.TypeOf((*store.Querier)(nil)).Elem()
	identity := reflect.TypeOf((*store.IdentityQuerier)(nil)).Elem()

	for i := range tenant.NumMethod() {
		name := tenant.Method(i).Name

		if _, shared := identity.MethodByName(name); shared {
			t.Errorf("%q is on both store.Querier and store.IdentityQuerier; the two doors overlap", name)
		}
	}

	t.Logf("tenant querier has %d methods, identity querier has %d, overlap 0",
		tenant.NumMethod(), identity.NumMethod())
}

// TestTenantScopedQueriesDoNotCompileAgainstTheIdentityQuerier turns the
// compiler into the assertion.
//
// The two reflection tests above describe the method set; this one proves what
// that method set *means* — that a handler which reaches for a tenant-scoped
// query through the pre-tenant door does not build. It writes the offending
// package, runs the real toolchain over it, and asserts the failure and its
// message. The package is created inside the module (imports have to resolve)
// and removed again whether the test passes or fails.
func TestTenantScopedQueriesDoNotCompileAgainstTheIdentityQuerier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  string
		wantErr string
	}{
		{
			name: "calling a tenant-scoped query on the identity querier",
			source: `package compileprobe

import (
	"context"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

func Widen(ctx context.Context, q store.IdentityQuerier) {
	_, _ = q.ListProjects(ctx)
}
`,
			wantErr: "q.ListProjects undefined",
		},
		{
			name: "importing the generated identity package from elsewhere",
			source: `package compileprobe

import (
	_ "github.com/AndyV99/collabboard/apps/api/internal/store/internal/identitygen"
)
`,
			// Written into internal/api rather than internal/store, because
			// from inside internal/store the import is legal — which is the
			// asymmetry the nested internal package buys.
			wantErr: "use of internal package",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := buildProbe(t, tc.source)

			t.Logf("go build said:\n%s", out)

			if err == nil {
				t.Fatal("the probe compiled; the pre-tenant door is wider than it should be")
			}

			if !strings.Contains(out, tc.wantErr) {
				t.Errorf("go build output does not mention %q, so it failed for some other reason", tc.wantErr)
			}
		})
	}
}

// buildProbe writes source into a throwaway package inside the module and
// compiles it, returning the toolchain's combined output.
func buildProbe(t *testing.T, source string) (string, error) {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}

	// internal/api, not internal/store: the second probe has to sit where the
	// internal-package rule applies, and the first one is unaffected by where
	// it lives.
	moduleRoot := filepath.Join("..", "..")

	dir, err := os.MkdirTemp(filepath.Join(moduleRoot, "internal", "api"), "compileprobe")
	if err != nil {
		t.Fatalf("creating probe package: %v", err)
	}

	t.Cleanup(func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Errorf("removing probe package: %v", rerr)
		}
	})

	if werr := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(source), 0o600); werr != nil {
		t.Fatalf("writing probe source: %v", werr)
	}

	cmd := exec.CommandContext(t.Context(), goBin, "build", "./internal/api/"+filepath.Base(dir))
	cmd.Dir = moduleRoot

	out, err := cmd.CombinedOutput()

	return string(out), err
}
