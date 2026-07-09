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

// TestActivationForAttemptZeroCompat (T-K1) pins the L5a activation-key
// generalization: attempt 0 is byte-identical to the pre-L5 single-attempt form
// (activationFor), and activationAttempt parses the trailing numeric suffix,
// defaulting a bare or non-numeric key to 0 (the run root and legacy shapes).
func TestActivationForAttemptZeroCompat(t *testing.T) {
	if got, want := activationForAttempt("greet", 0), activationFor("greet"); got != want {
		t.Errorf("activationForAttempt(greet,0) = %q, want activationFor(greet) = %q", got, want)
	}
	if got := activationForAttempt("greet", 12); got != "greet:12" {
		t.Errorf("activationForAttempt(greet,12) = %q, want greet:12", got)
	}
	for _, tc := range []struct {
		activation string
		want       int
	}{
		{"x:0", 0},
		{"x:12", 12},
		{"x", 0},            // bare (a run root stream id): no suffix ⇒ attempt 0
		{"gcg-run-abc", 0},  // a real stream id: last ':' absent ⇒ 0
		{"x:notanumber", 0}, // non-numeric suffix ⇒ 0
	} {
		if got := activationAttempt(tc.activation); got != tc.want {
			t.Errorf("activationAttempt(%q) = %d, want %d", tc.activation, got, tc.want)
		}
	}
}
