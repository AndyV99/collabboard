package api

import (
	"encoding/json"
	"testing"
)

// The three states a JSON object can express about one key, and the reason this
// type exists: a *string collapses the first two, and for a nullable column
// that loses a request the user can legitimately make.
func TestOptionalDistinguishesAbsentFromNullFromValue(t *testing.T) {
	type payload struct {
		Assignee Optional[string] `json:"assignee_id"`
	}

	for _, tc := range []struct {
		name      string
		body      string
		wantSet   bool
		wantClear bool
		wantValue string
		hasValue  bool
	}{
		{
			name: "absent key",
			body: `{}`,
			// Nothing was asked for. UnmarshalJSON never ran, which is what
			// makes Present trustworthy -- absence is signalled by the method
			// not being called, not by a value it could produce.
			wantSet: false,
		},
		{
			name:      "explicit null",
			body:      `{"assignee_id": null}`,
			wantSet:   true,
			wantClear: true,
		},
		{
			name:      "a value",
			body:      `{"assignee_id": "3f2b1a00-0000-4000-8000-000000000001"}`,
			wantSet:   true,
			wantClear: false,
			wantValue: "3f2b1a00-0000-4000-8000-000000000001",
			hasValue:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got payload
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.body, err)
			}

			if got.Assignee.Set() != tc.wantSet {
				t.Errorf("Set() = %t, want %t for %s", got.Assignee.Set(), tc.wantSet, tc.body)
			}

			if got.Assignee.Clearing() != tc.wantClear {
				t.Errorf("Clearing() = %t, want %t for %s", got.Assignee.Clearing(), tc.wantClear, tc.body)
			}

			if tc.hasValue {
				if got.Assignee.Value == nil {
					t.Fatalf("Value = nil, want %q", tc.wantValue)
				}

				if *got.Assignee.Value != tc.wantValue {
					t.Errorf("Value = %q, want %q", *got.Assignee.Value, tc.wantValue)
				}
			}
		})
	}
}

// The distinction is only worth anything if absent and null differ. A pointer
// would pass every case above except this one, which is the whole reason the
// type exists -- so it is asserted rather than left implied.
func TestOptionalAbsentAndNullAreNotTheSame(t *testing.T) {
	type payload struct {
		Assignee Optional[string] `json:"assignee_id"`
	}

	var absent, null payload

	if err := json.Unmarshal([]byte(`{}`), &absent); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal([]byte(`{"assignee_id":null}`), &null); err != nil {
		t.Fatal(err)
	}

	if absent.Assignee.Value != nil || null.Assignee.Value != nil {
		t.Fatal("both should carry a nil Value; the difference is Present, not Value")
	}

	if absent.Assignee.Set() == null.Assignee.Set() {
		t.Fatal("absent and explicit null must not be the same request: one leaves the field alone, the other clears it")
	}
}

// A malformed value has to be an error rather than a silently absent field.
func TestOptionalRejectsAValueOfTheWrongType(t *testing.T) {
	type payload struct {
		Assignee Optional[string] `json:"assignee_id"`
	}

	var got payload
	if err := json.Unmarshal([]byte(`{"assignee_id": 42}`), &got); err == nil {
		t.Fatal("a number decoded into Optional[string] without error")
	}
}
