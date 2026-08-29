package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Optional distinguishes the three things a JSON object can say about a field:
// it was absent, it was present and null, or it was present with a value.
//
// # Why a plain pointer is not enough
//
// A `*string` collapses the first two. `{"title": null}` and `{}` both
// unmarshal to nil, which is correct for title and description because those
// columns are NOT NULL -- there is no null to set, so "absent" and "null" mean
// the same thing and losing the distinction loses nothing.
//
// It is not correct for a nullable column. `assignee_id` has three meaningful
// requests: leave the assignee alone, assign it to somebody, and *unassign*.
// Unassigning is a thing people do, so the wire format has to be able to say
// it, and a pointer that reports nil for both "absent" and "null" cannot.
//
// # Why not a sentinel
//
// The usual shortcut is a magic value -- an empty string, a zero uuid --
// meaning "clear this". That is the same ambiguity moved somewhere harder to
// see: it makes a legal value illegal, and it fails silently the first time
// somebody's real data happens to look like the sentinel.
//
// # Scope
//
// Used only where nullability is real. title and description keep `*string`,
// which is a deliberate distinction rather than an inconsistency: a richer type
// there would carry a state the column cannot hold.
type Optional[T any] struct {
	// Present is true when the key appeared in the object at all, whatever its
	// value. False means the caller did not mention this field.
	Present bool

	// Value is nil when the key was present and explicitly null. It is only
	// meaningful when Present is true.
	Value *T
}

// UnmarshalJSON records that the key was present, then decodes it.
//
// encoding/json only calls this for a key that appears in the object, which is
// exactly what makes Present trustworthy: absence is signalled by this method
// never running, not by any value it could produce.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true

	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Value = nil

		return nil
	}

	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decoding optional field: %w", err)
	}

	o.Value = &value

	return nil
}

// Set reports whether the caller asked for this field to be written -- which is
// true both for a value and for an explicit null, and false for an absent key.
func (o Optional[T]) Set() bool { return o.Present }

// Clearing reports whether the caller asked for this field to become null.
func (o Optional[T]) Clearing() bool { return o.Present && o.Value == nil }
