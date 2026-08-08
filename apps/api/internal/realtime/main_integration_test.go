//go:build integration

package realtime

// Container lifecycle for this package's integration suite.
//
// Two containers, both for the whole package: Postgres, so the authorizer is
// the real one running against real row-level security policies, and Redis, so
// the fan-out is real pub/sub between processes rather than a channel in one.
//
// The unit tests in this package prove the *logic* against a memory bus and a
// fake that models the policies. What they cannot prove is that the policies
// say what the fake says they say, or that go-redis's pub/sub behaves the way
// the broker assumes. That is what these are for.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/redistest"
)

var (
	testDB    *pgtest.DB
	testRedis *redistest.Server
)

func TestMain(m *testing.M) {
	// The exit code goes through a function so that run's deferred teardown
	// actually runs: os.Exit skips defers.
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	db, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration harness (postgres): %v\n", err)

		// 1, not a skip. A harness that quietly degrades to "no tests ran" when
		// Docker is missing is how a suite stops proving anything without
		// anyone noticing.
		return 1
	}

	defer func() {
		if cerr := db.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "integration harness teardown (postgres): %v\n", cerr)
		}
	}()

	server, err := redistest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration harness (redis): %v\n", err)

		return 1
	}

	defer func() {
		if cerr := server.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "integration harness teardown (redis): %v\n", cerr)
		}
	}()

	testDB = db
	testRedis = server

	return m.Run()
}
