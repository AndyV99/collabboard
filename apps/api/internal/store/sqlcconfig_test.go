package store_test

// The compiler is what actually stops a handler from importing the generated
// querier: internal/store/internal/gen sits under a second `internal`, so any
// package outside internal/store gets
//
//	use of internal package .../internal/store/internal/gen not allowed
//
// That guarantee is a property of where the code is generated, not of the code
// itself, and nothing else in the build would notice if someone re-pointed sqlc
// at a package everyone can import. This test notices — it is the cheap
// counterpart to the migrations tests that assert RLS is still forced.

import (
	"os"
	"strings"
	"testing"
)

func TestSqlcGeneratesIntoAnUnimportablePackage(t *testing.T) {
	raw, err := os.ReadFile("../../sqlc.yaml")
	if err != nil {
		t.Fatalf("reading sqlc.yaml: %v", err)
	}

	cfg := string(raw)

	// Both generated packages, for the same reason. The pre-tenant one matters
	// at least as much: it is the querier that can reach identity data without
	// a tenant, so a package outside internal/store being able to construct one
	// would hand every handler the door WithoutTenant exists to meter.
	for _, wantOut := range []string{
		"out: internal/store/internal/gen",
		"out: internal/store/internal/identitygen",
	} {
		if !strings.Contains(cfg, wantOut) {
			t.Errorf("sqlc.yaml no longer contains %q; generated queriers would become importable outside internal/store, "+
				"and WithTenant/WithoutTenant would stop being the only ways to reach the database", wantOut)
		}
	}

	// package: store would put gen.New — a constructor that binds a querier to
	// anything implementing DBTX, the pool included — in the same package as
	// the helper, exported and one call away.
	if strings.Contains(cfg, "package: store") {
		t.Error("sqlc.yaml generates into package store; that exports gen.New alongside WithTenant and defeats the point")
	}
}
