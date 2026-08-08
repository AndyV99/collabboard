package migrate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestRunRejectsUnknownCommandWithoutConnecting(t *testing.T) {
	// The DSN points at a port nothing listens on: if Run validated after
	// connecting, this would fail with a network error rather than the
	// validation error asserted below.
	const unreachable = "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable"

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := Run(context.Background(), logger, unreachable, Command("upp"))
	if err == nil {
		t.Fatal("expected an error for an unknown command, got nil")
	}

	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("expected ErrUnknownCommand, got %v", err)
	}
}

func TestCommandValid(t *testing.T) {
	for _, cmd := range Commands() {
		if !cmd.Valid() {
			t.Errorf("Commands() advertises %q but Valid() rejects it", cmd)
		}
	}

	for _, cmd := range []Command{"", "UP", "migrate", "drop"} {
		if cmd.Valid() {
			t.Errorf("Valid() accepted %q", cmd)
		}
	}
}
