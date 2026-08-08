//go:build integration

package store_test

// Container lifecycle for every integration test in this package.
//
// One container for the package, not one per test: bring-up is seconds and the
// tests do not need isolated *databases*, they need isolated *tenants*, which
// is what the seed helper gives them. Every fixture uses fresh uuids, and a
// test scoped to its own tenant cannot see another test's rows even when they
// share a table — which is the property under test, so sharing the container
// exercises it rather than dodging it.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// testDB is set once by TestMain and read by the helpers in testdb_test.go.
var testDB *pgtest.DB

func TestMain(m *testing.M) {
	// The exit code goes through a function so that run's deferred teardown
	// actually runs: os.Exit skips defers, so calling it here rather than
	// inside run is what keeps the container from surviving the process.
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	db, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration harness: %v\n", err)

		// 1, not a skip. These tests exist to run; a harness that quietly
		// degrades to "no tests ran" when Docker is missing is how a suite
		// stops proving anything without anyone noticing.
		return 1
	}

	// Deferred, so the container is removed whether m.Run reports success,
	// reports failure, or panics its way out of this frame. The one case defers
	// cannot cover — a panic on a test's own goroutine, which kills the process
	// outright — is covered by Testcontainers' Ryuk reaper instead.
	defer func() {
		if cerr := db.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "integration harness teardown: %v\n", cerr)
		}
	}()

	testDB = db

	return m.Run()
}
