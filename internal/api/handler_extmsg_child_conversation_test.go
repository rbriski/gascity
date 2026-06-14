package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/extmsg"
)

// childConvTestAdapter is a TransportAdapter with controllable child-conversation
// behavior for exercising POST /extmsg/child-conversation.
type childConvTestAdapter struct {
	caps     extmsg.AdapterCapabilities
	childRef *extmsg.ConversationRef
	childErr error
	calls    int
}

func (a *childConvTestAdapter) Name() string { return "child-conv-test" }

func (a *childConvTestAdapter) Capabilities() extmsg.AdapterCapabilities { return a.caps }

func (a *childConvTestAdapter) VerifyAndNormalizeInbound(context.Context, extmsg.InboundPayload) (*extmsg.ExternalInboundMessage, error) {
	panic("unexpected VerifyAndNormalizeInbound call")
}

func (a *childConvTestAdapter) Publish(context.Context, extmsg.PublishRequest) (*extmsg.PublishReceipt, error) {
	panic("unexpected Publish call")
}

func (a *childConvTestAdapter) EnsureChildConversation(_ context.Context, _ extmsg.ConversationRef, _ string) (*extmsg.ConversationRef, error) {
	a.calls++
	if a.childErr != nil {
		return nil, a.childErr
	}
	return a.childRef, nil
}

func childConvRequestBody(t *testing.T, label string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"conversation": map[string]any{
			"scope_id":        "guild-1",
			"provider":        "discord",
			"account_id":      "acct-1",
			"conversation_id": "chan-1",
			"kind":            "room",
		},
		"label": label,
	})
	if err != nil {
		t.Fatalf("Marshal(body): %v", err)
	}
	return string(body)
}

func TestHandleExtMsgChildConversation_CreatesChild(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	registry := extmsg.NewAdapterRegistry()
	child := &extmsg.ConversationRef{
		ScopeID:              "guild-1",
		Provider:             "discord",
		AccountID:            "acct-1",
		ConversationID:       "thread-9",
		ParentConversationID: "chan-1",
		Kind:                 extmsg.ConversationThread,
	}
	adapter := &childConvTestAdapter{
		caps:     extmsg.AdapterCapabilities{SupportsChildConversations: true},
		childRef: child,
	}
	registry.Register(extmsg.AdapterKey{Provider: "discord", AccountID: "acct-1"}, adapter)
	fs.adapterReg = registry

	req := newPostRequest(cityURL(fs, "/extmsg/child-conversation"), strings.NewReader(childConvRequestBody(t, "build pipeline")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if adapter.calls != 1 {
		t.Fatalf("EnsureChildConversation calls = %d, want 1", adapter.calls)
	}
	var got extmsg.ConversationRef
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, rec.Body.String())
	}
	if got.ConversationID != "thread-9" || got.Kind != extmsg.ConversationThread {
		t.Fatalf("child ref = %+v, want conversation_id=thread-9 kind=thread", got)
	}
	if got.ParentConversationID != "chan-1" {
		t.Fatalf("child parent = %q, want chan-1", got.ParentConversationID)
	}
}

func TestHandleExtMsgChildConversation_UnsupportedReturns400(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	registry := extmsg.NewAdapterRegistry()
	adapter := &childConvTestAdapter{caps: extmsg.AdapterCapabilities{SupportsChildConversations: false}}
	registry.Register(extmsg.AdapterKey{Provider: "discord", AccountID: "acct-1"}, adapter)
	fs.adapterReg = registry

	req := newPostRequest(cityURL(fs, "/extmsg/child-conversation"), strings.NewReader(childConvRequestBody(t, "x")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter callback should not fire when capability is off; calls = %d", adapter.calls)
	}
}
