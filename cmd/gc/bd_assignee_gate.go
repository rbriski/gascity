package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// Assignee address gate (ga-pr8). A polecat's submit-and-exit step reassigns
// its work bead to the refinery via `gc bd update <id> --assignee=<target>`,
// where <target> is a rig/binding-qualified agent address built from
// template interpolation (e.g. "${GC_RIG:+$GC_RIG/}{{binding_prefix}}refinery").
// An unrendered or stale binding_prefix produces the short, unqualified form
// (e.g. "gastown/refinery" instead of the configured "gastown/gastown.refinery")
// for a rig whose refinery agent is bound. That short form is not a routing
// error bd itself detects — it happily assigns the bead to an address no
// agent's hook ever polls, stranding the bead until a witness recovery scan
// finds it (see ga-tyg).
//
// This gate blocks that specific, mechanically-detectable failure shape
// before the mutation reaches bd: a slash-qualified assignee address that
// exactly matches the short (unbound) identity of an agent which is in fact
// bound to a binding name, so the correct address requires the
// binding-qualified form. It deliberately does NOT reject addresses it
// cannot resolve at all (unknown rigs, cross-city addresses, human/session
// assignees) — only the recognizable mismatch class, so unrelated valid
// handoffs stay unaffected.

// bdArgsAssigneeValue scans bd command args for an --assignee (or -a) value,
// in both "--assignee=value" and "--assignee value" forms. Returns ok=false
// when no assignee flag is present at all (nothing to validate).
func bdArgsAssigneeValue(bdArgs []string) (value string, ok bool) {
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if v, cut := strings.CutPrefix(arg, "--assignee="); cut {
			return v, true
		}
		if v, cut := strings.CutPrefix(arg, "-a="); cut {
			return v, true
		}
		if (arg == "--assignee" || arg == "-a") && i+1 < len(bdArgs) {
			return bdArgs[i+1], true
		}
	}
	return "", false
}

// looksLikeAgentRouteAddress reports whether v has the "<rig>/<agent>" shape
// gc's agent/route addressing uses, as opposed to a session name
// ("city__polecat-xyz") or an empty/unassign value. Only addresses in this
// shape are validated; everything else passes through untouched.
func looksLikeAgentRouteAddress(v string) bool {
	return strings.TrimSpace(v) != "" && strings.Contains(v, "/")
}

// evaluateAssigneeAddressGate checks a slash-qualified assignee address
// against configured agents and returns a non-empty, actionable message when
// the address is malformed or is the known-bad short form of a bound agent.
// An empty return means the address is acceptable (including addresses this
// gate cannot resolve at all, which it intentionally lets through).
func evaluateAssigneeAddressGate(cfg *config.City, assignee string) string {
	if !looksLikeAgentRouteAddress(assignee) {
		return ""
	}
	dir, name := config.ParseQualifiedName(assignee)
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(name) == "" {
		return fmt.Sprintf("malformed agent address %q: expected <rig>/<agent> form", assignee)
	}
	if cfg == nil {
		return ""
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].QualifiedName() == assignee {
			return ""
		}
	}
	if unboundRoutedToIdentities(cfg)[assignee] {
		return ""
	}
	if canonicals, ok := boundRoutedToAliases(cfg)[assignee]; ok && len(canonicals) > 0 {
		if len(canonicals) == 1 {
			return fmt.Sprintf("malformed agent address %q: this rig binds the agent under a binding-qualified name; use %q instead", assignee, canonicals[0])
		}
		return fmt.Sprintf("malformed agent address %q: this rig binds the agent under a binding-qualified name; use one of %s instead", assignee, strings.Join(canonicals, ", "))
	}
	return ""
}

// runAssigneeAddressGate validates any --assignee address on a bd
// invocation before it reaches bd. Returns whether the invocation should be
// blocked; on block it writes an actionable message to stderr. Best-effort:
// it never blocks addresses it cannot resolve against cfg.
func runAssigneeAddressGate(bdArgs []string, cfg *config.City, stderr io.Writer) bool {
	assignee, ok := bdArgsAssigneeValue(bdArgs)
	if !ok {
		return false
	}
	msg := evaluateAssigneeAddressGate(cfg, assignee)
	if msg == "" {
		return false
	}
	fmt.Fprintf(stderr, "gc bd: assignee address gate: %s\n", msg) //nolint:errcheck // best-effort stderr
	return true
}
