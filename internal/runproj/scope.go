package runproj

import (
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// scopeRefRe validates a scope ref. Port of TS SCOPE_REF_RE
// (shared/src/run-detail.ts).
var scopeRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

// sourceWorkflowStoreKeys are the source-attribution metadata keys that carry a
// run's store ref in "<kind>:<ref>" form (e.g. "rig:gascity"). Source-attributed
// graph.v2 runs (pr_review/bugflow/design_review) whose gcg-* root bead lives
// only in the graph_store never emit gc.root_store_ref to the event log; the
// source (mc-*) beads that DID fold instead carry the store ref under their
// workflow-namespaced key. Parallel to sourceRunRootID's key list.
var sourceWorkflowStoreKeys = []string{
	"pr_review.workflow_store",
	"bugflow.workflow_store",
	"design_review.workflow_store",
}

// sourceWorkflowStoreRef returns the first non-empty source-attribution store
// ref across issues (format "<kind>:<ref>"), or "" when none is present. This is
// the store ref a source-attributed graph.v2 run carries when gc.root_store_ref
// is absent, and it is the shared input both the summary scope resolver
// (runScope) and the detail phantom-root synthesizer feed into
// fromRootMetadataScope so the two paths resolve scope identically.
func sourceWorkflowStoreRef(issues []runIssue) string {
	for _, key := range sourceWorkflowStoreKeys {
		if v := metadataString(issues, key); v != "" {
			return v
		}
	}
	return ""
}

// runScopeWithStoreRef is the resolved scope. Port of TS RunScopeWithStoreRef.
type runScopeWithStoreRef struct {
	scopeKind    string
	scopeRef     string
	rootStoreRef string
}

// parseRunScopeKind accepts only "city" or "rig". Port of TS parseRunScopeKind.
func parseRunScopeKind(value string) (string, bool) {
	if value == "city" || value == "rig" {
		return value, true
	}
	return "", false
}

// fromRootMetadataScope resolves the lane scope from root metadata, with the
// gc.root_store_ref fallback. Port of TS fromRootMetadataScope. The bool mirrors
// TS's `null`.
func fromRootMetadataScope(metadata map[string]string) (runScopeWithStoreRef, bool) {
	rootStoreRef := stringValueOrEmpty(metadata[beadmeta.RootStoreRefMetadataKey])

	// Primary: explicit gc.scope_kind / gc.scope_ref pair.
	scopeKind, kindOK := parseRunScopeKind(metadata[beadmeta.ScopeKindMetadataKey])
	scopeRef := stringValueOrEmpty(metadata[beadmeta.ScopeRefMetadataKey])
	if kindOK && scopeRef != "" && scopeRefRe.MatchString(scopeRef) {
		rsr := rootStoreRef
		if rsr == "" {
			rsr = scopeKind + ":" + scopeRef
		}
		return runScopeWithStoreRef{scopeKind: scopeKind, scopeRef: scopeRef, rootStoreRef: rsr}, true
	}

	// Fallback (gascity-dashboard-km0w): recover scope from gc.root_store_ref.
	if rootStoreRef == "" {
		return runScopeWithStoreRef{}, false
	}
	parsedKind, parsedRef, ok := fromStoreRef(rootStoreRef)
	if !ok || !scopeRefRe.MatchString(parsedRef) {
		return runScopeWithStoreRef{}, false
	}
	return runScopeWithStoreRef{scopeKind: parsedKind, scopeRef: parsedRef, rootStoreRef: rootStoreRef}, true
}

// fromStoreRef parses a "<kind>:<ref>" store ref. Port of TS fromStoreRef.
func fromStoreRef(rootStoreRef string) (kind, ref string, ok bool) {
	value := stringValueOrEmpty(rootStoreRef)
	if value == "" {
		return "", "", false
	}
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || colon >= len(value)-1 {
		return "", "", false
	}
	parsedKind, kindOK := parseRunScopeKind(value[:colon])
	parsedRef := stringValueOrEmpty(value[colon+1:])
	if !kindOK || parsedRef == "" {
		return "", "", false
	}
	return parsedKind, parsedRef, true
}

// stringValueOrEmpty trims a value; an all-whitespace or empty value becomes "".
// Mirrors the TS run-scope stringValue (which returns null for empty).
func stringValueOrEmpty(value string) string {
	return strings.TrimSpace(value)
}
