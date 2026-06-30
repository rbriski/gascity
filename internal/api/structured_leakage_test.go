package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

// structuredTranscriptWireAllowedKeys is the allowlist of JSON keys the typed
// structured transcript response can legitimately serialize. Any key outside it
// on the wire is a leaked provider-native key.
func structuredTranscriptWireAllowedKeys() map[string]struct{} {
	return worker.NeutralWireKeys(reflect.TypeOf(sessionTranscriptGetResponse{}))
}

// assertNoStructuredWireLeak fails the test if the serialized structured wire
// carries any provider-native shape. It applies both leakage gates: the
// canonical provider-native token denylist (plus any case-specific extras) and
// the schema allowlist, which catches future native keys the denylist does not
// yet name.
func assertNoStructuredWireLeak(t *testing.T, wire []byte, extraForbidden ...string) {
	t.Helper()
	if leaked := worker.ScanForbiddenTokens(wire, extraForbidden...); len(leaked) > 0 {
		t.Fatalf("structured response leaked provider-native token(s) %v: %s", leaked, wire)
	}
	unexpected, err := worker.UnexpectedWireKeys(wire, structuredTranscriptWireAllowedKeys())
	if err != nil {
		t.Fatalf("scan structured wire keys: %v", err)
	}
	if len(unexpected) > 0 {
		t.Fatalf("structured response carried non-schema key(s) %v: %s", unexpected, wire)
	}
}

// TestStructuredWireTypesHaveNoMapFields enforces the load-bearing assumption
// behind the allowlist leakage gate: the structured wire payload must contain no
// map fields. NeutralWireKeys cannot enumerate a map's dynamic keys, so if one
// is added the allowlist would silently miss provider-native keys nested inside
// it. If this fails, exclude the new map subtree before calling
// UnexpectedWireKeys (and update assertNoStructuredWireLeak accordingly).
func TestStructuredWireTypesHaveNoMapFields(t *testing.T) {
	roots := []reflect.Type{
		reflect.TypeOf(SessionStructuredHistory{}),
		reflect.TypeOf(SessionStructuredMessage{}),
		reflect.TypeOf(SessionStreamStructuredMessageEvent{}),
	}
	for _, root := range roots {
		if path := firstMapField(root, map[reflect.Type]struct{}{}, root.Name()); path != "" {
			t.Fatalf("structured wire type carries a map field at %s; the allowlist leakage gate cannot enumerate its dynamic keys", path)
		}
	}
}

// firstMapField returns the dotted path to the first map-typed field reachable
// from t, or "" if none exists.
func firstMapField(t reflect.Type, seen map[reflect.Type]struct{}, path string) string {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() == reflect.Map {
		return path
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	if _, ok := seen[t]; ok {
		return ""
	}
	seen[t] = struct{}{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		if hit := firstMapField(field.Type, seen, path+"."+field.Name); hit != "" {
			return hit
		}
	}
	return ""
}

// TestStructuredSubagentLineageInliningPending is a skip-marker for a known,
// spec'd-but-unimplemented gap, so the drift stays visible rather than silent:
// SessionStructuredMessage reserves IsSubagent and ParentToolCallID for inline
// subagent nesting, but the structured stream does not yet nest or reference
// subagent messages in the primary payload (engdocs/design/structured-stream-format.md,
// "inline subagent nesting"). Remove the skip and assert the lineage fields are
// populated once inlining lands.
func TestStructuredSubagentLineageInliningPending(t *testing.T) {
	// Compile-time anchor: fail loudly if the reserved lineage carriers are
	// renamed or removed before the feature is implemented.
	_ = SessionStructuredMessage{IsSubagent: true, ParentToolCallID: "parent-call"}
	t.Skip("inline subagent nesting not implemented: SessionStructuredMessage.{IsSubagent,ParentToolCallID} are reserved but never populated; see engdocs/design/structured-stream-format.md")
}

func TestStructuredLeakageGateCatchesInjectedNativeKey(t *testing.T) {
	clean := sessionTranscriptGetResponse{
		ID:            "s1",
		Template:      "Chat",
		Provider:      "claude",
		Format:        "structured",
		SchemaVersion: sessionStructuredSchemaVersion,
		StructuredMessages: []SessionStructuredMessage{{
			ID:     "m1",
			Role:   "assistant",
			Status: "final",
			Blocks: []SessionStructuredBlock{{
				Type:       "tool_result",
				ToolCallID: "call-1",
				Structured: &SessionStructuredToolResult{Kind: "edit", FilePath: "a.go", Patch: "@@ -1 +1 @@"},
			}},
		}},
	}
	wire, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal clean response: %v", err)
	}

	// Baseline: a real typed response passes both gates.
	assertNoStructuredWireLeak(t, wire)

	// A known provider-native key must be caught by BOTH gates.
	if leaked := worker.ScanForbiddenTokens(injectWireKey(t, wire, "toolUseResult")); len(leaked) == 0 {
		t.Fatal("denylist gate failed to catch injected toolUseResult")
	}
	if unexpected, _ := worker.UnexpectedWireKeys(injectWireKey(t, wire, "toolUseResult"), structuredTranscriptWireAllowedKeys()); len(unexpected) == 0 {
		t.Fatal("allowlist gate failed to catch injected toolUseResult")
	}

	// A novel native key the denylist has never seen must still be caught by
	// the allowlist gate — this is the future-proofing the denylist cannot give.
	novel := injectWireKey(t, wire, "someBrandNewProviderKey")
	if leaked := worker.ScanForbiddenTokens(novel); len(leaked) > 0 {
		t.Fatalf("denylist unexpectedly matched a novel key: %v", leaked)
	}
	unexpected, err := worker.UnexpectedWireKeys(novel, structuredTranscriptWireAllowedKeys())
	if err != nil {
		t.Fatalf("scan novel wire: %v", err)
	}
	if len(unexpected) != 1 || unexpected[0] != "someBrandNewProviderKey" {
		t.Fatalf("allowlist gate must catch a novel non-schema key, got %v", unexpected)
	}
}

// injectWireKey decodes the wire, adds key (with a sentinel value) to the first
// structured block, and re-encodes it — simulating a provider-native key
// leaking into the structured projection.
func injectWireKey(t *testing.T, wire []byte, key string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	messages, ok := doc["structured_messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("wire has no structured_messages to inject into: %s", wire)
	}
	message := messages[0].(map[string]any)
	blocks := message["blocks"].([]any)
	block := blocks[0].(map[string]any)
	block[key] = "leak"
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal injected wire: %v", err)
	}
	return out
}
