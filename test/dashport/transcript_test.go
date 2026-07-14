//go:build integration

package dashport_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/testutil"
)

type structuredTranscriptSnapshot struct {
	ID                 string                         `json:"id"`
	Provider           string                         `json:"provider"`
	Format             string                         `json:"format"`
	SchemaVersion      string                         `json:"schema_version"`
	Operation          string                         `json:"operation"`
	History            *api.SessionStructuredHistory  `json:"history"`
	StructuredMessages []api.SessionStructuredMessage `json:"structured_messages"`
}

type transcriptSSEFrame struct {
	ID    string
	Event string
	Data  []byte
}

type structuredTranscriptStream struct {
	cancel   context.CancelFunc
	response *http.Response
	reader   *bufio.Reader
}

func TestStructuredTranscriptServeLevel(t *testing.T) {
	h := newHarness(t)

	var snapshot structuredTranscriptSnapshot
	h.getJSON(h.cityURL("/session/"+h.sessionID+"/transcript?format=structured&tail=0"), &snapshot)

	if snapshot.ID != h.sessionID || snapshot.Provider != "claude" {
		t.Fatalf("snapshot identity = id %q provider %q, want %q/claude", snapshot.ID, snapshot.Provider, h.sessionID)
	}
	if snapshot.Format != "structured" || snapshot.SchemaVersion != "session.structured.v1" || snapshot.Operation != "snapshot" {
		t.Fatalf("snapshot contract = format %q version %q operation %q", snapshot.Format, snapshot.SchemaVersion, snapshot.Operation)
	}
	if snapshot.History == nil || snapshot.History.TranscriptStreamID == "" || snapshot.History.Cursor.ResumeToken == "" {
		t.Fatalf("snapshot history = %+v, want stream identity and resume token", snapshot.History)
	}
	if got := structuredMessageIDs(snapshot.StructuredMessages); !equalStrings(got, []string{transcriptInitialUserID, transcriptInitialAssistantID}) {
		t.Fatalf("snapshot message IDs = %v, want seeded user/assistant", got)
	}
	if got := structuredMessageText(snapshot.StructuredMessages[1]); got != transcriptInitialAnswer {
		t.Fatalf("assistant text = %q, want %q", got, transcriptInitialAnswer)
	}

	stream := openStructuredTranscriptStream(t, h, snapshot.History.Cursor.ResumeToken)
	defer stream.Close()
	firstFrame := stream.Next(t)
	if firstFrame.Event != "activity" {
		t.Fatalf("first resumed SSE event = %q, want activity proving exact snapshot suppression (frame: %s)", firstFrame.Event, firstFrame.Data)
	}
	stream.Close()

	appendClaudeTranscript(t, h.transcriptPath, claudeTranscriptEntry{
		UUID:       transcriptAppendedUserID,
		ParentUUID: transcriptInitialAssistantID,
		Type:       "user",
		Timestamp:  "2026-07-14T00:00:02Z",
		Message:    claudeTranscriptMessage{Role: "user", Content: transcriptAppendedPrompt},
	})
	upsertStream := openStructuredTranscriptStream(t, h, snapshot.History.Cursor.ResumeToken)
	defer upsertStream.Close()
	upsertFrame := upsertStream.NextEvent(t, "structured")
	var upsert api.SessionStreamStructuredMessageEvent
	if err := json.Unmarshal(upsertFrame.Data, &upsert); err != nil {
		t.Fatalf("decode structured SSE upsert: %v (data: %s)", err, upsertFrame.Data)
	}
	if upsert.Operation != "upsert" {
		t.Fatalf("SSE operation = %q, want upsert", upsert.Operation)
	}
	wantUpsertIDs := []string{transcriptInitialAssistantID, transcriptAppendedUserID}
	if got := structuredMessageIDs(upsert.StructuredMessages); !equalStrings(got, wantUpsertIDs) {
		t.Fatalf("SSE upsert IDs = %v, want inclusive tail %v", got, wantUpsertIDs)
	}
	if upsert.History == nil || upsert.History.Cursor.ResumeToken == "" || upsert.History.Cursor.ResumeToken == snapshot.History.Cursor.ResumeToken {
		t.Fatalf("SSE upsert cursor = %+v, want a new resume token", upsert.History)
	}
	if upsertFrame.ID != upsert.History.Cursor.ResumeToken {
		t.Fatalf("SSE id = %q, want payload cursor %q", upsertFrame.ID, upsert.History.Cursor.ResumeToken)
	}

	upsertStream.Close()
	if err := h.sessionManager.Close(h.sessionID); err != nil {
		t.Fatalf("close seeded session: %v", err)
	}
	var closedSnapshot structuredTranscriptSnapshot
	h.getJSON(h.cityURL("/session/"+h.sessionID+"/transcript?format=structured&tail=0"), &closedSnapshot)
	wantClosedIDs := []string{transcriptInitialUserID, transcriptInitialAssistantID, transcriptAppendedUserID}
	if got := structuredMessageIDs(closedSnapshot.StructuredMessages); !equalStrings(got, wantClosedIDs) {
		t.Fatalf("closed snapshot IDs = %v, want history parity %v", got, wantClosedIDs)
	}
	if closedSnapshot.History == nil || closedSnapshot.History.Cursor.ResumeToken == "" {
		t.Fatalf("closed snapshot history = %+v, want resume token", closedSnapshot.History)
	}

	closedStream := openStructuredTranscriptStream(t, h, closedSnapshot.History.Cursor.ResumeToken)
	defer closedStream.Close()
	if got := closedStream.response.Header.Get("GC-Session-Status"); got != "stopped" {
		t.Fatalf("closed stream status header = %q, want stopped", got)
	}
	closedFirstFrame := closedStream.Next(t)
	if closedFirstFrame.Event != "activity" {
		t.Fatalf("closed resumed SSE event = %q, want activity proving exact snapshot suppression (frame: %s)", closedFirstFrame.Event, closedFirstFrame.Data)
	}
}

func structuredMessageIDs(messages []api.SessionStructuredMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func structuredMessageText(message api.SessionStructuredMessage) string {
	for _, block := range message.Blocks {
		if block.Type == "text" {
			return block.Text
		}
	}
	return ""
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func openStructuredTranscriptStream(t *testing.T, h *harness, cursor string) *structuredTranscriptStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*testutil.GoroutineRaceTimeout)
	streamURL := h.cityURL("/session/" + h.sessionID + "/stream?format=structured&after_cursor=" + url.QueryEscape(cursor))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("create structured stream request: %v", err)
	}
	response, err := h.client.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open structured stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		t.Fatalf("structured stream status = %d, want 200 (body: %s)", response.StatusCode, truncate(body))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("structured stream content type = %q, want text/event-stream", contentType)
	}
	return &structuredTranscriptStream{
		cancel:   cancel,
		response: response,
		reader:   bufio.NewReader(response.Body),
	}
}

func (s *structuredTranscriptStream) Close() {
	s.cancel()
	_ = s.response.Body.Close()
}

func (s *structuredTranscriptStream) Next(t *testing.T) transcriptSSEFrame {
	t.Helper()
	for {
		var frame transcriptSSEFrame
		var dataLines []string
		for {
			line, err := s.reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read structured SSE frame: %v", err)
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if frame.Event == "" && frame.ID == "" && len(dataLines) == 0 {
					continue
				}
				frame.Data = []byte(strings.Join(dataLines, "\n"))
				return frame
			}
			switch {
			case strings.HasPrefix(line, "id:"):
				frame.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "event:"):
				frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
}

func (s *structuredTranscriptStream) NextEvent(t *testing.T, event string) transcriptSSEFrame {
	t.Helper()
	for {
		frame := s.Next(t)
		if frame.Event == event {
			return frame
		}
	}
}
