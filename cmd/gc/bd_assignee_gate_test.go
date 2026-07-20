package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func assigneeGateTestCity() *config.City {
	return &config.City{
		Agents: []config.Agent{
			// Bound agent: canonical address is "gastown/gastown.refinery".
			{Name: "refinery", Dir: "gastown", BindingName: "gastown"},
			// Unbound agent: canonical address is the short form itself.
			{Name: "polecat", Dir: "hello-world"},
		},
	}
}

func TestBdArgsAssigneeValue(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{"long-equals", []string{"update", "id", "--assignee=gastown/gastown.refinery"}, "gastown/gastown.refinery", true},
		{"long-space", []string{"update", "id", "--assignee", "gastown/gastown.refinery"}, "gastown/gastown.refinery", true},
		{"short-equals", []string{"update", "id", "-a=gastown/gastown.refinery"}, "gastown/gastown.refinery", true},
		{"none", []string{"update", "id", "--status=open"}, "", false},
		{"empty-args", []string{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bdArgsAssigneeValue(tc.args)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("bdArgsAssigneeValue(%v) = (%q, %v), want (%q, %v)", tc.args, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestEvaluateAssigneeAddressGateBlocksMalformedBoundShortForm(t *testing.T) {
	cfg := assigneeGateTestCity()
	// This is exactly the ga-tyg failure shape: the short unqualified form of
	// a bound agent, instead of the required binding-qualified address.
	msg := evaluateAssigneeAddressGate(cfg, "gastown/refinery")
	if msg == "" {
		t.Fatal("want a block message for the malformed short-form refinery address, got none")
	}
	if !strings.Contains(msg, "gastown/gastown.refinery") {
		t.Fatalf("block message %q does not suggest the canonical address", msg)
	}
}

func TestEvaluateAssigneeAddressGateAllowsCanonicalBoundAddress(t *testing.T) {
	cfg := assigneeGateTestCity()
	if msg := evaluateAssigneeAddressGate(cfg, "gastown/gastown.refinery"); msg != "" {
		t.Fatalf("canonical bound address rejected: %q", msg)
	}
}

func TestEvaluateAssigneeAddressGateAllowsUnboundShortForm(t *testing.T) {
	cfg := assigneeGateTestCity()
	if msg := evaluateAssigneeAddressGate(cfg, "hello-world/polecat"); msg != "" {
		t.Fatalf("unbound rig's short-form address rejected: %q", msg)
	}
}

func TestEvaluateAssigneeAddressGateBlocksEmptySegments(t *testing.T) {
	cfg := assigneeGateTestCity()
	for _, addr := range []string{"gastown/", "/refinery"} {
		if msg := evaluateAssigneeAddressGate(cfg, addr); msg == "" {
			t.Fatalf("malformed address %q was not rejected", addr)
		}
	}
}

func TestEvaluateAssigneeAddressGateIgnoresNonAddressValues(t *testing.T) {
	cfg := assigneeGateTestCity()
	for _, addr := range []string{"", "gastown__polecat-ci-pwq0", "bbriski@raybeam.com"} {
		if msg := evaluateAssigneeAddressGate(cfg, addr); msg != "" {
			t.Fatalf("non-address value %q unexpectedly rejected: %q", addr, msg)
		}
	}
}

func TestEvaluateAssigneeAddressGateLeavesUnresolvableAddressesAlone(t *testing.T) {
	cfg := assigneeGateTestCity()
	// A rig/agent this cfg doesn't know about at all — not the bug class this
	// gate targets, so it must stay compatible with existing valid handoffs
	// this gate cannot verify.
	if msg := evaluateAssigneeAddressGate(cfg, "other-rig/other-agent"); msg != "" {
		t.Fatalf("unresolvable address unexpectedly rejected: %q", msg)
	}
}

func TestEvaluateAssigneeAddressGateNilConfig(t *testing.T) {
	if msg := evaluateAssigneeAddressGate(nil, "gastown/refinery"); msg != "" {
		t.Fatalf("nil config unexpectedly rejected a resolvable-looking address: %q", msg)
	}
}

func TestRunAssigneeAddressGate(t *testing.T) {
	cfg := assigneeGateTestCity()

	t.Run("blocks malformed target", func(t *testing.T) {
		var stderr bytes.Buffer
		bdArgs := []string{"update", "ga-tyg", "--status=open", "--assignee=gastown/refinery"}
		if !runAssigneeAddressGate(bdArgs, cfg, &stderr) {
			t.Fatal("want block, got allow")
		}
		if !strings.Contains(stderr.String(), "gastown/gastown.refinery") {
			t.Fatalf("stderr missing actionable suggestion: %q", stderr.String())
		}
	})

	t.Run("allows canonical target", func(t *testing.T) {
		var stderr bytes.Buffer
		bdArgs := []string{"update", "ga-tyg", "--status=open", "--assignee=gastown/gastown.refinery"}
		if runAssigneeAddressGate(bdArgs, cfg, &stderr) {
			t.Fatalf("want allow, got block: %q", stderr.String())
		}
	})

	t.Run("no assignee flag is a no-op", func(t *testing.T) {
		var stderr bytes.Buffer
		bdArgs := []string{"update", "ga-tyg", "--status=closed"}
		if runAssigneeAddressGate(bdArgs, cfg, &stderr) {
			t.Fatalf("want allow, got block: %q", stderr.String())
		}
	})
}
