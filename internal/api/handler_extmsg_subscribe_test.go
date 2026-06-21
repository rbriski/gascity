package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
)

// newExtMsgSubscribeFixture creates a test fixture with extmsg services,
// adapter registry, and a registered client token.
// Returns (fs, srv, clientID, rawToken, convRef).
func newExtMsgSubscribeFixture(t *testing.T) (*fakeState, *Server, string, string, extmsg.ConversationRef) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()

	result, err := extmsg.RegisterClient(context.Background(), fs.cityBeadStore, extmsg.RegisterClientInput{
		AllowNoCredential: true,
	})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	convID := "test-conv-1"
	convRef := extmsg.ConversationRef{
		Provider:       extmsg.ProviderLLMClient,
		AccountID:      result.ClientID,
		ConversationID: convID,
	}
	return fs, srv, result.ClientID, result.Token, convRef
}

func subscribeURL(fs *fakeState, clientID, convID string) string {
	return cityURL(fs, "/extmsg/clients/"+clientID+"/conversations/"+convID+"/subscribe")
}

func TestSubscribeHandler_MissingTokenReturns401(t *testing.T) {
	fs, srv, clientID, _, _ := newExtMsgSubscribeFixture(t)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_InvalidTokenReturns401(t *testing.T) {
	fs, srv, clientID, _, _ := newExtMsgSubscribeFixture(t)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil)
	req.Header.Set("X-GC-Client-Token", "not-a-real-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_AccountMismatchReturns403(t *testing.T) {
	fs, srv, _, token, _ := newExtMsgSubscribeFixture(t)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, "wrong-client-id", "test-conv-1"), nil)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_ServicesUnavailableReturns503(t *testing.T) {
	fs, srv, clientID, token, _ := newExtMsgSubscribeFixture(t)
	fs.extmsgSvc = nil
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_NoBindingStreamsHeartbeats(t *testing.T) {
	fs, srv, clientID, token, _ := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "100ms"
	h := newTestCityHandlerWith(t, fs, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want \"text/event-stream\"", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: heartbeat") {
		t.Errorf("stream missing heartbeat event; body: %s", rec.Body.String())
	}
}

func TestSubscribeHandler_ValidTokenStreamsMessages(t *testing.T) {
	fs, srv, clientID, token, convRef := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "10s"
	h := newTestCityHandlerWith(t, fs, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for the LLMClientAdapter to register in the shared AdapterRegistry.
	adapterKey := extmsg.AdapterKey{Provider: extmsg.ProviderLLMClient, AccountID: clientID}
	deadline := time.Now().Add(time.Second)
	var adapter extmsg.TransportAdapter
	for time.Now().Before(deadline) {
		adapter = fs.adapterReg.Lookup(adapterKey)
		if adapter != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if adapter == nil {
		t.Fatal("LLMClientAdapter not registered after 1s")
	}

	_, err := adapter.Publish(context.Background(), extmsg.PublishRequest{
		SessionID:    "test-session",
		Conversation: convRef,
		Text:         "hello from session",
	})
	if err != nil {
		t.Fatalf("adapter.Publish: %v", err)
	}

	// Give the stream loop time to pick up the event, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	// The SSE framework omits "event: message" for the default event type;
	// check the JSON payload field instead.
	if !strings.Contains(body, `"event":"message"`) {
		t.Errorf("stream missing message event payload; body: %s", body)
	}
	if !strings.Contains(body, "hello from session") {
		t.Errorf("stream missing message text; body: %s", body)
	}
}

func TestSubscribeHandler_AdapterUnregisteredOnDisconnect(t *testing.T) {
	fs, srv, clientID, token, _ := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "10s"
	h := newTestCityHandlerWith(t, fs, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for adapter to appear.
	adapterKey := extmsg.AdapterKey{Provider: extmsg.ProviderLLMClient, AccountID: clientID}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fs.adapterReg.Lookup(adapterKey) != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Cancel the request (simulate client disconnect).
	cancel()
	<-done

	// Adapter must be unregistered after disconnect.
	if fs.adapterReg.Lookup(adapterKey) != nil {
		t.Error("LLMClientAdapter still registered after disconnect")
	}
}
