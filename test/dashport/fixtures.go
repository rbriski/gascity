//go:build integration

package dashport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

const (
	corpusCityName = "dashport-city"
	corpusRigName  = "demo"

	// anchorRunID is the seeded run root's bead id and workflow id. Both the
	// store-side /workflow/{id} read and the event-log runproj routes address the
	// run by this id, so the corpus keeps them in lockstep.
	anchorRunID       = "run-anchor"
	anchorStepID      = "run-anchor.preflight"
	anchorFormula     = "mol-adopt-pr-v2"
	corpusWorkBeadID  = "work-1"
	corpusMailSubject = "seeded handoff"
	corpusMailFrom    = "builder"
	corpusMailTo      = "reviewer"

	transcriptInitialUserID      = "transcript-user-1"
	transcriptInitialAssistantID = "transcript-assistant-1"
	transcriptInitialAnswer      = "Initial structured answer"
	transcriptAppendedUserID     = "transcript-user-2"
	transcriptAppendedPrompt     = "Appended structured prompt"
)

// fixtures is the loaded, seeded corpus plus the state a test drives.
type fixtures struct {
	CityName string
	CityPath string

	config    *config.City
	cityStore beads.Store
	rigStores map[string]beads.Store
	eventProv events.Provider
	mailProv  *beadmail.Provider

	sessionProvider *runtime.Fake
	sessionManager  *session.Manager
	sessionID       string
	transcriptPath  string
}

type claudeTranscriptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeTranscriptEntry struct {
	UUID       string                  `json:"uuid"`
	ParentUUID string                  `json:"parentUuid"`
	Type       string                  `json:"type"`
	Timestamp  string                  `json:"timestamp"`
	SessionID  string                  `json:"sessionId,omitempty"`
	CWD        string                  `json:"cwd,omitempty"`
	Message    claudeTranscriptMessage `json:"message"`
}

// corpusBeads is the on-disk beads.json shape: a sequence counter and the bead
// list (with explicit ids preserved verbatim in the store).
type corpusBeads struct {
	Seq   int          `json:"seq"`
	Beads []beads.Bead `json:"beads"`
}

// loadFixtures reads testdata/dashport, seeds an in-memory city store (beads +
// derived deps), replays the ordered event log into a FileRecorder at
// <cityPath>/.gc/events.jsonl (the exact path the host-side run tailers read),
// seeds one mail message, and returns everything the harness wires into
// api.ServeSeededCity. The event recorder is the SAME object that backs both the
// events feed (State.EventProvider) and the run tailer (the file it writes), so
// there is one event source of truth.
func loadFixtures(t *testing.T) *fixtures {
	t.Helper()

	cityPath := t.TempDir()

	store := seedBeadStore(t)
	rec := seedEventLog(t, cityPath)
	mailProv := seedMail(t, store)
	transcriptRoot := filepath.Join(cityPath, ".gc", "provider-transcripts")
	sessionProvider, sessionManager, sessionID, transcriptPath := seedTranscriptSession(t, store, cityPath, transcriptRoot)

	return &fixtures{
		CityName:  corpusCityName,
		CityPath:  cityPath,
		config:    corpusConfig(cityPath, transcriptRoot),
		cityStore: store,
		rigStores: map[string]beads.Store{corpusRigName: beads.NewMemStore()},
		eventProv: rec,
		mailProv:  mailProv,

		sessionProvider: sessionProvider,
		sessionManager:  sessionManager,
		sessionID:       sessionID,
		transcriptPath:  transcriptPath,
	}
}

func seedTranscriptSession(t *testing.T, store beads.Store, cityPath, transcriptRoot string) (*runtime.Fake, *session.Manager, string, string) {
	t.Helper()
	provider := runtime.NewFake()
	manager := session.NewManagerWithOptions(store, provider)
	workDir := filepath.Join(cityPath, "rigs", corpusRigName)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("create transcript workdir: %v", err)
	}
	info, err := manager.CreateSession(context.Background(), session.CreateOptions{
		Template: "demo/builder",
		Title:    "Structured transcript",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("create transcript session: %v", err)
	}

	transcriptDir := filepath.Join(transcriptRoot, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("create transcript directory: %v", err)
	}
	transcriptPath := filepath.Join(transcriptDir, info.SessionKey+".jsonl")
	writeClaudeTranscript(t, transcriptPath,
		claudeTranscriptEntry{
			UUID:      transcriptInitialUserID,
			Type:      "user",
			Timestamp: "2026-07-14T00:00:00Z",
			SessionID: info.SessionKey,
			CWD:       workDir,
			Message:   claudeTranscriptMessage{Role: "user", Content: "Inspect transcript enrichment"},
		},
		claudeTranscriptEntry{
			UUID:       transcriptInitialAssistantID,
			ParentUUID: transcriptInitialUserID,
			Type:       "assistant",
			Timestamp:  "2026-07-14T00:00:01Z",
			SessionID:  info.SessionKey,
			CWD:        workDir,
			Message:    claudeTranscriptMessage{Role: "assistant", Content: transcriptInitialAnswer},
		},
	)
	return provider, manager, info.ID, transcriptPath
}

func writeClaudeTranscript(t *testing.T, path string, entries ...claudeTranscriptEntry) {
	t.Helper()
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			t.Fatalf("encode transcript entry %q: %v", entry.UUID, err)
		}
	}
	if err := os.WriteFile(path, payload.Bytes(), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

func appendClaudeTranscript(t *testing.T, path string, entry claudeTranscriptEntry) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		_ = file.Close()
		t.Fatalf("append transcript entry %q: %v", entry.UUID, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close appended transcript: %v", err)
	}
}

// seedBeadStore loads beads.json and returns a MemStore that preserves the
// corpus bead ids and derives parent/needs dependencies, so /beads,
// /workflow/{id}, and /mail all project the real topology.
func seedBeadStore(t *testing.T) beads.Store {
	t.Helper()

	raw := readCorpus(t, "beads.json")
	var cb corpusBeads
	if err := json.Unmarshal(raw, &cb); err != nil {
		t.Fatalf("decode beads.json: %v", err)
	}

	deps := make([]beads.Dep, 0)
	for _, b := range cb.Beads {
		// A step "needs" its predecessor; the workflow snapshot walks DepList
		// down (IssueID == this bead) and emits from=DependsOnID → to=IssueID.
		for _, need := range b.Needs {
			depType, dependsOnID := "blocks", need
			if kind, id, ok := strings.Cut(need, ":"); ok && kind != "" && id != "" {
				depType, dependsOnID = kind, id
			}
			deps = append(deps, beads.Dep{IssueID: b.ID, DependsOnID: dependsOnID, Type: depType})
		}
	}

	return beads.NewMemStoreFrom(cb.Seq, cb.Beads, deps)
}

// seedEventLog replays events.jsonl (in file order) through a FileRecorder at
// <cityPath>/.gc/events.jsonl. Record auto-assigns the seq in call order, so the
// corpus order defines the projected seq order for both the events feed and the
// runproj fold.
func seedEventLog(t *testing.T, cityPath string) events.Provider {
	t.Helper()

	logPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	rec, err := events.NewFileRecorder(logPath, os.Stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder(%s): %v", logPath, err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	for _, line := range splitNonEmptyLines(readCorpus(t, "events.jsonl")) {
		var e events.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("decode event %q: %v", truncate(line), err)
		}
		// Let the recorder assign seq/ts in append order; the corpus seqs are
		// documentation of intended order, not authoritative.
		e.Seq = 0
		rec.Record(e)
	}
	return rec
}

// seedMail sends one message through the city bead store's mail provider so the
// /mail feed and a thread read project a real message bead.
func seedMail(t *testing.T, store beads.Store) *beadmail.Provider {
	t.Helper()
	mp := beadmail.New(store)
	if _, err := mp.Send(corpusMailFrom, corpusMailTo, corpusMailSubject, "please adopt the seeded PR"); err != nil {
		t.Fatalf("seed mail: %v", err)
	}
	return mp
}

// corpusConfig builds the seeded city config in Go (config.City uses TOML tags,
// so it is authored here rather than deserialized from the corpus). It mirrors
// the fake-state defaults but names one rig and one agent the assertions expect.
func corpusConfig(cityPath, transcriptRoot string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: corpusCityName},
		Daemon:    config.DaemonConfig{ObservePaths: []string{transcriptRoot}},
		Agents: []config.Agent{
			{Name: "builder", Dir: corpusRigName, Provider: "test-agent", MaxActiveSessions: intPtr(2)},
		},
		Rigs: []config.Rig{
			{Name: corpusRigName, Path: filepath.Join(cityPath, "rigs", corpusRigName)},
		},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {DisplayName: "Test Agent"},
		},
	}
}

// serveSeededCity wires the loaded corpus into the exported production seam.
// The returned stop function drains the plane's run tailers and status samplers.
func serveSeededCity(ctx context.Context, fx *fixtures) (http.Handler, func(), error) {
	return api.ServeSeededCity(ctx, api.SeededCityDeps{
		CityName:        fx.CityName,
		CityPath:        fx.CityPath,
		Config:          fx.config,
		CityBeadStore:   fx.cityStore,
		RigStores:       fx.rigStores,
		MailProvider:    fx.mailProv,
		EventProvider:   fx.eventProv,
		SessionProvider: fx.sessionProvider,
	}, "")
}

func readCorpus(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "dashport", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus %s: %v", path, err)
	}
	return raw
}

func splitNonEmptyLines(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out
}

func intPtr(n int) *int { return &n }
