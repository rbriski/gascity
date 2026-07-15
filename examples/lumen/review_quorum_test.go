package lumen_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

func TestReviewQuorumIsARealDocumentWorkflow(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("review-quorum.lumen.json")
	if err != nil {
		t.Fatalf("reading compiled review quorum: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decoding compiled review quorum: %v", err)
	}

	inputNames := make(map[string]bool, len(doc.Input.Fields))
	for _, field := range doc.Input.Fields {
		inputNames[field.Name] = true
	}
	for _, required := range []string{"document_path", "repository_path", "artifact_dir", "objective", "lane_one_id", "lane_two_id"} {
		if !inputNames[required] {
			t.Errorf("compiled formula input is missing %q", required)
		}
	}

	if len(doc.Nodes) != 3 {
		t.Fatalf("compiled formula top-level node count = %d, want 3", len(doc.Nodes))
	}
	topLevel := make(map[string]ir.Node, len(doc.Nodes))
	for _, node := range doc.Nodes {
		topLevel[node.ID] = node
	}

	lanes := requireReviewQuorumNode(t, topLevel, "lanes", ir.NodeScatter, nil)
	var scatter reviewQuorumScatterPayload
	decodeReviewQuorumNode(t, lanes, &scatter)
	if scatter.Form != "members" {
		t.Errorf("lanes.form = %q, want members", scatter.Form)
	}
	memberIDs := make([]string, 0, len(scatter.Members))
	for _, member := range scatter.Members {
		memberIDs = append(memberIDs, member.ID)
	}
	if want := []string{"reviewLaneOne", "reviewLaneTwo"}; !slices.Equal(memberIDs, want) {
		t.Fatalf("lanes.members = %v, want %v", memberIDs, want)
	}

	laneOne := requireReviewQuorumDo(t, scatter.Members[0], nil, "laneOneAgent")
	requireReviewQuorumBody(t, "reviewLaneOne", laneOne.Body.Raw,
		"{{ artifact_dir }}/lane-one.json", "{{ repository_path }}", ".lane-one.XXXXXX", "gc.output_json=$OUTPUT_JSON")
	laneTwo := requireReviewQuorumDo(t, scatter.Members[1], nil, "laneTwoAgent")
	requireReviewQuorumBody(t, "reviewLaneTwo", laneTwo.Body.Raw,
		"{{ artifact_dir }}/lane-two.json", "{{ repository_path }}", ".lane-two.XXXXXX", "gc.output_json=$OUTPUT_JSON")

	synthesisNode := requireReviewQuorumNode(t, topLevel, "synthesize", ir.NodeDo, []string{"lanes"})
	synthesis := requireReviewQuorumDo(t, synthesisNode, []string{"lanes"}, "")
	requireReviewQuorumBody(t, "synthesize", synthesis.Body.Raw,
		"{{ artifact_dir }}/lane-one.json", "{{ artifact_dir }}/lane-two.json",
		"{{ artifact_dir }}/synthesis.json", "{{ repository_path }}", "gc.output_json",
		"copy each finding ID byte-for-byte", "exactly once across incorporated_findings and deferred_findings",
		"($classified_ids | sort) == ($reviewer_ids | sort)")

	verifyNode := requireReviewQuorumNode(t, topLevel, "verify", ir.NodeDo, []string{"synthesize"})
	verification := requireReviewQuorumDo(t, verifyNode, []string{"synthesize"}, "verifierAgent")
	requireReviewQuorumBody(t, "verify", verification.Body.Raw,
		"{{ artifact_dir }}/verification.json", "{{ repository_path }}", "gc.output_json", "{{ synthesize }}",
		"no invented, combined, renamed, omitted, or duplicate finding IDs",
		"($classified_ids | sort) == ($reviewer_ids | sort)")
	if !verification.Body.hasTemplateRef("synthesize") {
		t.Error("verify compiled template does not consume the synthesize output")
	}

	workerPrompt, err := os.ReadFile("review-quorum-live/prompts/lumen-worker.md")
	if err != nil {
		t.Fatalf("reading live worker prompt: %v", err)
	}
	if !strings.Contains(string(workerPrompt), "gc runtime drain-ack") {
		t.Error("live worker prompt does not require the runtime return handshake")
	}
}

func TestReviewQuorumIROriginsArePortable(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("review-quorum.lumen.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	count := 0
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if origin, ok := typed["origin"].(map[string]any); ok {
				if uri, ok := origin["uri"].(string); ok {
					count++
					if filepath.IsAbs(uri) || uri != "review-quorum.lumen" {
						t.Errorf("compiled origin URI = %q, want portable review-quorum.lumen", uri)
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(document)
	if count == 0 {
		t.Fatal("compiled review quorum contains no origin URIs")
	}
}

func TestReviewQuorumLiveCodexAgentsUseSupportedModel(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"laneTwoAgent", "verifierAgent"} {
		path := filepath.Join("review-quorum-live", "agents", name, "agent.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		text := string(data)
		if !strings.Contains(text, `provider = "codex"`) {
			t.Errorf("%s does not route through the Codex provider", path)
		}
		if !strings.Contains(text, `model = "gpt-5.5"`) {
			t.Errorf("%s does not pin the live-gateway-supported gpt-5.5 model", path)
		}
	}
}

func TestReviewQuorumLivePackDoesNotDuplicateRoutedWorkers(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("review-quorum-live", "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "[[named_session]]") {
		t.Fatal("live pack declares named sessions in addition to Lumen-routed ephemeral workers")
	}
}

type reviewQuorumScatterPayload struct {
	Form    string    `json:"form"`
	Members []ir.Node `json:"members"`
}

type reviewQuorumDoPayload struct {
	Source struct {
		Kind string `json:"kind"`
	} `json:"source"`
	Interpreter struct {
		Kind string `json:"kind"`
		Mode struct {
			Kind string `json:"kind"`
		} `json:"mode"`
		Agent *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"agent"`
	} `json:"interpreter"`
	Body reviewQuorumBody `json:"body"`
}

type reviewQuorumBody struct {
	Raw      string `json:"raw"`
	Template struct {
		Parts []struct {
			Kind string `json:"kind"`
			Expr struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"expr"`
		} `json:"parts"`
	} `json:"template"`
}

func (body reviewQuorumBody) hasTemplateRef(name string) bool {
	for _, part := range body.Template.Parts {
		if part.Kind == "interp" && part.Expr.Kind == "ref" && part.Expr.Name == name {
			return true
		}
	}
	return false
}

func requireReviewQuorumNode(t *testing.T, nodes map[string]ir.Node, id string, kind ir.NodeKind, after []string) ir.Node {
	t.Helper()
	node, ok := nodes[id]
	if !ok {
		t.Fatalf("compiled formula is missing top-level node %q", id)
	}
	if node.Kind != kind {
		t.Errorf("%s.kind = %q, want %q", id, node.Kind, kind)
	}
	if !slices.Equal(node.After, after) {
		t.Errorf("%s.after = %v, want %v", id, node.After, after)
	}
	return node
}

func requireReviewQuorumDo(t *testing.T, node ir.Node, after []string, agentName string) reviewQuorumDoPayload {
	t.Helper()
	if node.Kind != ir.NodeDo {
		t.Fatalf("%s.kind = %q, want %q", node.ID, node.Kind, ir.NodeDo)
	}
	if !slices.Equal(node.After, after) {
		t.Errorf("%s.after = %v, want %v", node.ID, node.After, after)
	}
	var payload reviewQuorumDoPayload
	decodeReviewQuorumNode(t, node, &payload)
	if payload.Source.Kind != "prompt" {
		t.Errorf("%s.source.kind = %q, want prompt", node.ID, payload.Source.Kind)
	}
	if payload.Interpreter.Kind != "agent" || payload.Interpreter.Mode.Kind != "do" {
		t.Errorf("%s interpreter = %q/%q, want agent/do", node.ID, payload.Interpreter.Kind, payload.Interpreter.Mode.Kind)
	}
	if agentName == "" {
		if payload.Interpreter.Agent != nil {
			t.Errorf("%s compiled Agent route = %q, want unbound", node.ID, payload.Interpreter.Agent.Name)
		}
		return payload
	}
	if payload.Interpreter.Agent == nil {
		t.Fatalf("%s compiled Agent route is unbound, want %q", node.ID, agentName)
	}
	if payload.Interpreter.Agent.Kind != "ref" || payload.Interpreter.Agent.Name != agentName {
		t.Errorf("%s compiled Agent route = %q/%q, want ref/%q",
			node.ID, payload.Interpreter.Agent.Kind, payload.Interpreter.Agent.Name, agentName)
	}
	return payload
}

func decodeReviewQuorumNode(t *testing.T, node ir.Node, dst any) {
	t.Helper()
	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshaling compiled node %q: %v", node.ID, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("decoding compiled node %q payload: %v", node.ID, err)
	}
}

func requireReviewQuorumBody(t *testing.T, nodeID, body string, contracts ...string) {
	t.Helper()
	for _, contract := range contracts {
		if !strings.Contains(body, contract) {
			t.Errorf("%s compiled prompt is missing contract %q", nodeID, contract)
		}
	}
}
