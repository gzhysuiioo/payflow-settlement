package payflow

import (
	"strings"
	"testing"
)

// TestDuplicateFieldsRejected verifies that any JSON object declaring the
// same field name twice is rejected, in both config and context, at every
// nesting level.
func TestDuplicateFieldsRejected(t *testing.T) {
	cases := []struct {
		name string
		kind string // "config" or "context"
		raw  string
		want string // error must contain this substring
	}{
		// --- config: every object level ---
		{"config top-level flags", "config", `{"flags":[],"flags":[]}`, `duplicate field "flags"`},
		{"config flag enabled", "config", `{"flags":[{"key":"f","enabled":true,"enabled":false,"default":false,"rules":[]}]}`, `duplicate field "enabled"`},
		{"config flag key", "config", `{"flags":[{"key":"f","key":"g","enabled":true,"default":false,"rules":[]}]}`, `duplicate field "key"`},
		{"config flag default", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"default":true,"rules":[]}]}`, `duplicate field "default"`},
		{"config flag rules", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],"rules":[]}]}`, `duplicate field "rules"`},
		{"config rule id", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","id":"s","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `duplicate field "id"`},
		{"config rule value", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"value":false,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `duplicate field "value"`},
		{"config rule conditions", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[],"conditions":[]}]}]}`, `duplicate field "conditions"`},
		{"config condition attribute", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","attribute":"b","op":"eq","value":"x"}]}]}]}`, `duplicate field "attribute"`},
		{"config condition op", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","op":"in","value":"x"}]}]}]}`, `duplicate field "op"`},
		{"config condition value", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x","value":"y"}]}]}]}`, `duplicate field "value"`},
		// --- locations carry array indices ---
		{"config dup in second flag", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[]},{"key":"g","enabled":true,"enabled":false,"default":false,"rules":[]}]}`, `config.flags[1]`},
		{"config dup in second rule", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},{"id":"r2","id":"r3","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`, `config.flags[0].rules[1]`},
		{"config dup in second condition", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r1","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"},{"attribute":"b","op":"eq","op":"in","value":"y"}]}]}]}`, `config.flags[0].rules[0].conditions[1]`},
		// --- nested objects inside additional fields ---
		{"config dup in flag extra object", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],"meta":{"x":1,"x":2}}]}`, `config.flags[0].meta`},
		{"config dup in deeply nested extra", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],"a":{"b":{"c":1,"c":2}}}]}`, `config.flags[0].a.b`},
		{"config dup in condition value object", "config", `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":{"x":1,"x":2}}]}]}]}`, `config.flags[0].rules[0].conditions[0].value`},
		// --- name comparison: decoded strings, case-sensitive, no trimming ---
		{"config unicode escape collision", "config", "{\"flags\":[{\"key\":\"f\",\"enabled\":true,\"en\\u0061bled\":false,\"default\":false,\"rules\":[]}]}", `duplicate field "enabled"`},
		{"config same value still errors", "config", `{"flags":[{"key":"f","enabled":true,"enabled":true,"default":false,"rules":[]}]}`, `duplicate field "enabled"`},
		{"config case differs is fine", "config", `{"flags":[{"key":"f","Enabled":true,"enabled":false,"default":false,"rules":[]}]}`, ``},
		{"config leading space not trimmed", "config", `{"flags":[{"key":"f","enabled":true," enabled":false,"default":false,"rules":[]}]}`, ``},
		{"config trailing space not trimmed", "config", `{"flags":[{"key":"f","enabled":true,"enabled ":false,"default":false,"rules":[]}]}`, ``},
		// --- unselected / disabled flags are not exempt ---
		{"config dup in unselected flag", "config", `{"flags":[{"key":"good","enabled":true,"default":false,"rules":[]},{"key":"bad","enabled":true,"enabled":false,"default":false,"rules":[]}]}`, `config.flags[1]`},
		{"config dup in disabled flag", "config", `{"flags":[{"key":"off","enabled":false,"enabled":true,"default":false,"rules":[]}]}`, `config.flags[0]`},
		{"config dup in rule of disabled flag", "config", `{"flags":[{"key":"off","enabled":false,"default":false,"rules":[{"id":"r","id":"s","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `config.flags[0].rules[0]`},
		// --- first duplicate in document order wins ---
		{"config first dup in file order", "config", `{"flags":[{"key":"f","enabled":true,"enabled":false,"default":false,"rules":[{"id":"r","id":"s","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]}]}`, `duplicate field "enabled"`},
		// --- context ---
		{"context top-level", "context", `{"plan":"pro","plan":"free"}`, `context`},
		{"context nested object", "context", `{"plan":"pro","attrs":{"a":"1","a":"2"}}`, `context.attrs`},
		{"context nested in array", "context", `{"items":[{"k":"1","k":"2"}]}`, `context.items[0]`},
		{"context unicode escape", "context", "{\"plan\":\"pro\",\"pl\\u0061n\":\"free\"}", `duplicate field "plan"`},
		{"context same value still errors", "context", `{"plan":"pro","plan":"pro"}`, `duplicate field "plan"`},
		{"context case differs is fine", "context", `{"Plan":"pro","plan":"free"}`, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.kind == "config" {
				_, err = ParseConfig([]byte(tc.raw))
			} else {
				_, err = ParseContext([]byte(tc.raw))
			}
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("error must state the ambiguity, got %q", err.Error())
			}
		})
	}
}

// TestDuplicateFieldsAcrossObjectsAllowed confirms that the same field name
// in two different objects, and duplicate elements of arrays, are legal.
func TestDuplicateFieldsAcrossObjectsAllowed(t *testing.T) {
	// Same field name ("key", "id", "attribute") in two different objects
	// is legal — the duplicate-field check is per-object.
	cfg := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[
			{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]}]},
		{"key":"g","enabled":true,"default":false,"rules":[
			{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`
	c, err := ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("same name across objects should be legal: %v", err)
	}
	if len(c.Flags) != 2 {
		t.Fatalf("got %d flags", len(c.Flags))
	}

	// Duplicate elements of the in-list array are legal.
	cfg = `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"tier","op":"in","value":["a","a","b"]}]}]}]}`
	c, err = ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("duplicate array elements should be legal: %v", err)
	}
	if len(c.Flags[0].Rules[0].Conditions[0].inVal) != 3 {
		t.Fatalf("inVal = %v", c.Flags[0].Rules[0].Conditions[0].inVal)
	}

	// Additional fields without duplicates are still allowed everywhere.
	cfg = `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[],"note":"x","meta":{"a":1}}]}`
	if _, err := ParseConfig([]byte(cfg)); err != nil {
		t.Fatalf("additional fields should still be allowed: %v", err)
	}
	ctx := `{"plan":"pro","extra":"whatever"}`
	if _, err := ParseContext([]byte(ctx)); err != nil {
		t.Fatalf("context extra fields should still be allowed: %v", err)
	}
}

// TestDuplicateFieldErrorDistinctFromUniquenessErrors confirms the
// duplicate-field error is reported separately from the existing duplicate
// flag key / duplicate rule id uniqueness errors.
func TestDuplicateFieldErrorDistinctFromUniquenessErrors(t *testing.T) {
	// Duplicate flag KEY (same name across two flag objects) keeps the
	// original uniqueness error.
	cfg := `{"flags":[
		{"key":"f","enabled":true,"default":false,"rules":[]},
		{"key":"f","enabled":true,"default":false,"rules":[]}]}`
	_, err := ParseConfig([]byte(cfg))
	if err == nil || !strings.Contains(err.Error(), "duplicate flag key") {
		t.Fatalf("got %v, want duplicate flag key error", err)
	}

	// Duplicate rule id keeps the original uniqueness error.
	cfg = `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"a","op":"eq","value":"x"}]},
		{"id":"r","value":false,"conditions":[{"attribute":"a","op":"eq","value":"y"}]}]}]}`
	_, err = ParseConfig([]byte(cfg))
	if err == nil || !strings.Contains(err.Error(), "duplicate rule id") {
		t.Fatalf("got %v, want duplicate rule id error", err)
	}
}

// TestDuplicateCheckPrecedesTypeErrors confirms a duplicate field is
// reported even when the same document also contains a type error.
func TestDuplicateCheckPrecedesTypeErrors(t *testing.T) {
	// enabled is duplicated AND default has a wrong type.
	cfg := `{"flags":[{"key":"f","enabled":true,"enabled":false,"default":"nope","rules":[]}]}`
	_, err := ParseConfig([]byte(cfg))
	if err == nil || !strings.Contains(err.Error(), `duplicate field "enabled"`) {
		t.Fatalf("got %v, want duplicate field error", err)
	}
}

// TestNoDuplicatesMeansNoRegression confirms the happy paths still parse.
func TestNoDuplicatesMeansNoRegression(t *testing.T) {
	cfg := `{"flags":[{"key":"f","enabled":true,"default":false,"rules":[
		{"id":"r","value":true,"conditions":[{"attribute":"plan","op":"eq","value":"pro"}]}]}]}`
	c, err := ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if c.Find("f") == nil {
		t.Fatal("flag f not found")
	}
	ctx := `{"plan":"pro"}`
	if _, err := ParseContext([]byte(ctx)); err != nil {
		t.Fatalf("valid context: %v", err)
	}
}
