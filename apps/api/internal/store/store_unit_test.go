package store_test

// The part of internal/store that can be tested without a database, and so is
// not behind the `integration` build tag: the argument checks that exist to
// stop a wiring mistake from ever reaching Postgres.
//
// Everything that actually proves isolation needs a real database and lives in
// the tagged files alongside this one. See the package README for how to run
// each.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// TestWithTenantRejectsBadArguments needs no database: these are the checks that
// stop a wiring mistake from reaching one.
func TestWithTenantRejectsBadArguments(t *testing.T) {
	ctx := context.Background()
	noop := func(context.Context, store.Querier) error { return nil }

	var nilStore *store.Store

	for _, tc := range []struct {
		name  string
		store *store.Store
		id    uuid.UUID
		fn    store.TenantFunc
		want  error
	}{
		{name: "nil store", store: nilStore, id: uuid.New(), fn: noop, want: store.ErrNilPool},
		{name: "nil pool", store: store.New(nil), id: uuid.New(), fn: noop, want: store.ErrNilPool},
		{name: "nil callback", store: store.New(&pgxpool.Pool{}), id: uuid.New(), fn: nil, want: store.ErrNilFunc},
		{name: "zero tenant", store: store.New(&pgxpool.Pool{}), id: uuid.Nil, fn: noop, want: store.ErrNoTenant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.store.WithTenant(ctx, tc.id, tc.fn); !errors.Is(err, tc.want) {
				t.Errorf("WithTenant = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestInTenantReturnsZeroValueOnError: a caller that ignores the error must not
// be handed a value that looks like it came from a committed transaction.
func TestInTenantReturnsZeroValueOnError(t *testing.T) {
	got, err := store.InTenant(context.Background(), store.New(nil), uuid.New(),
		func(context.Context, store.Querier) (string, error) { return "should not escape", nil })
	if !errors.Is(err, store.ErrNilPool) {
		t.Fatalf("InTenant error = %v, want %v", err, store.ErrNilPool)
	}

	if got != "" {
		t.Errorf("InTenant returned %q on error, want the zero value", got)
	}
}
