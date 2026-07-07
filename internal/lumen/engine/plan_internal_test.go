package engine

import (
	"encoding/json"
	"testing"
)

// TestEvalValueInterpWrapper proves evalValue unwraps the emitted
// {"kind":"interp","expr":{...}} value-expression wrapper (seen in the
// linear-do-after / path-interpolation goldens) and recurses into its inner
// expression, rather than hitting the default and erroring.
func TestEvalValueInterpWrapper(t *testing.T) {
	raw := json.RawMessage(`{"kind":"interp","expr":{"kind":"literal","value":"x"}}`)

	got, err := evalValue(raw, map[string]string{})
	if err != nil {
		t.Fatalf("evalValue interp wrapper: %v", err)
	}
	if got != "x" {
		t.Errorf("evalValue = %q, want %q", got, "x")
	}
}

// TestEvalValueInterpWrapperOverRef proves the interp wrapper recurses far
// enough to resolve an inner ref against the live scope.
func TestEvalValueInterpWrapperOverRef(t *testing.T) {
	raw := json.RawMessage(`{"kind":"interp","expr":{"kind":"ref","name":"who"}}`)

	got, err := evalValue(raw, map[string]string{"who": "world"})
	if err != nil {
		t.Fatalf("evalValue interp/ref wrapper: %v", err)
	}
	if got != "world" {
		t.Errorf("evalValue = %q, want %q", got, "world")
	}
}
