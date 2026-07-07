package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// doFormulaIR is a one-node agent `do` formula compiled to IR.
const doFormulaIR = `{
  "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
  "name": "review",
  "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
  "origin": {"uri": "t", "line": 0, "col": 0},
  "nodes": [
    {"kind": "block", "id": "b1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0}, "members": [
      {"kind": "do", "id": "summarize", "name": "summarize", "after": [],
       "origin": {"uri": "t", "line": 1, "col": 0},
       "source": {"kind": "prompt"},
       "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
       "body": {"raw": "Summarize this repo's AGENTS.md in 3 bullets.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}
    ]}
  ]
}`

func writeDoFormula(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "review.lumen.json")
	if err := os.WriteFile(path, []byte(doFormulaIR), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	return path
}

// TestRunAgentDoStepWithInjectedHost proves `gc run --agent-cmd` wires an agent
// host into the run and prints the do step outcome. The host is injected as a
// deterministic stub, so the CLI path is exercised without spawning a session.
func TestRunAgentDoStepWithInjectedHost(t *testing.T) {
	path := writeDoFormula(t)

	orig := buildRunAgentHost
	t.Cleanup(func() { buildRunAgentHost = orig })
	var gotOpts runAgentOptions
	buildRunAgentHost = func(_ context.Context, opts runAgentOptions) (enginehost.AgentHost, func(), error) {
		gotOpts = opts
		return &enginehost.StubHost{Results: map[string]enginehost.DoResult{
			"summarize": {Outcome: enginehost.OutcomePass, Output: "- a\n- b\n- c"},
		}}, func() {}, nil
	}

	var out, errb bytes.Buffer
	cmd := newRunCmd(&out, &errb)
	cmd.SetArgs([]string{path, "--agent-cmd", "claude", "--agent-prompt-flag", "-p"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr: %s)", err, errb.String())
	}

	if gotOpts.Command != "claude" || gotOpts.PromptFlag != "-p" {
		t.Errorf("host built with opts %+v, want --agent-cmd/-p wired through", gotOpts)
	}
	s := out.String()
	if !strings.Contains(s, "summarize") || !strings.Contains(s, "[do]") {
		t.Errorf("output missing do step line:\n%s", s)
	}
	if !strings.Contains(s, "outcome: pass") {
		t.Errorf("output missing pass outcome:\n%s", s)
	}
}

// TestRunDoStepWithoutAgentCmdErrorsClearly proves a do formula run without
// --agent-cmd is refused with a message directing the user to set it.
func TestRunDoStepWithoutAgentCmdErrorsClearly(t *testing.T) {
	path := writeDoFormula(t)

	var out, errb bytes.Buffer
	cmd := newRunCmd(&out, &errb)
	cmd.SetArgs([]string{path})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected a non-nil error running a do formula without --agent-cmd")
	}
	if !strings.Contains(errb.String(), "--agent-cmd") {
		t.Errorf("stderr = %q, want a hint to pass --agent-cmd", errb.String())
	}
}
