package extmsg

import (
	"context"
	"errors"
	"testing"
)

// childTestAdapter is a minimal TransportAdapter for exercising
// HandleChildConversation with controllable capabilities and callback results.
type childTestAdapter struct {
	caps      AdapterCapabilities
	childRef  *ConversationRef
	childErr  error
	gotRef    ConversationRef
	gotLabel  string
	callCount int
}

func (a *childTestAdapter) Name() string                      { return "child-test" }
func (a *childTestAdapter) Capabilities() AdapterCapabilities { return a.caps }

func (a *childTestAdapter) VerifyAndNormalizeInbound(context.Context, InboundPayload) (*ExternalInboundMessage, error) {
	return nil, errors.New("not implemented")
}

func (a *childTestAdapter) Publish(context.Context, PublishRequest) (*PublishReceipt, error) {
	return nil, errors.New("not implemented")
}

func (a *childTestAdapter) EnsureChildConversation(_ context.Context, ref ConversationRef, label string) (*ConversationRef, error) {
	a.callCount++
	a.gotRef = ref
	a.gotLabel = label
	if a.childErr != nil {
		return nil, a.childErr
	}
	return a.childRef, nil
}

func childParentRef() ConversationRef {
	return ConversationRef{
		ScopeID:        "city-1",
		Provider:       "discord",
		AccountID:      "app-1",
		ConversationID: "chan-1",
		Kind:           ConversationRoom,
	}
}

func childTestDeps(adapter TransportAdapter, parent ConversationRef) ChildConversationDeps {
	reg := NewAdapterRegistry()
	if adapter != nil {
		reg.Register(AdapterKey{Provider: parent.Provider, AccountID: parent.AccountID}, adapter)
	}
	return ChildConversationDeps{Registry: reg}
}

func TestHandleChildConversation_Success(t *testing.T) {
	parent := childParentRef()
	want := &ConversationRef{
		ScopeID:              parent.ScopeID,
		Provider:             parent.Provider,
		AccountID:            parent.AccountID,
		ConversationID:       "thread-7",
		ParentConversationID: parent.ConversationID,
		Kind:                 ConversationThread,
	}
	adapter := &childTestAdapter{caps: AdapterCapabilities{SupportsChildConversations: true}, childRef: want}
	deps := childTestDeps(adapter, parent)

	got, err := HandleChildConversation(context.Background(), deps, ChildConversationRequest{Parent: parent, Label: "build pipeline"})
	if err != nil {
		t.Fatalf("HandleChildConversation: %v", err)
	}
	if got == nil || got.ConversationID != "thread-7" || got.Kind != ConversationThread {
		t.Fatalf("child ref = %+v, want conversation_id=thread-7 kind=thread", got)
	}
	if got.ParentConversationID != parent.ConversationID {
		t.Fatalf("child parent = %q, want %q", got.ParentConversationID, parent.ConversationID)
	}
	if adapter.callCount != 1 {
		t.Fatalf("EnsureChildConversation calls = %d, want 1", adapter.callCount)
	}
	if adapter.gotLabel != "build pipeline" {
		t.Fatalf("forwarded label = %q, want 'build pipeline'", adapter.gotLabel)
	}
	if adapter.gotRef.ConversationID != parent.ConversationID {
		t.Fatalf("forwarded parent ref = %q, want %q", adapter.gotRef.ConversationID, parent.ConversationID)
	}
}

func TestHandleChildConversation_UnsupportedCapability(t *testing.T) {
	parent := childParentRef()
	adapter := &childTestAdapter{
		caps:     AdapterCapabilities{SupportsChildConversations: false},
		childRef: &ConversationRef{ConversationID: "thread-7"},
	}
	deps := childTestDeps(adapter, parent)

	_, err := HandleChildConversation(context.Background(), deps, ChildConversationRequest{Parent: parent, Label: "x"})
	if !errors.Is(err, ErrAdapterUnsupported) {
		t.Fatalf("err = %v, want ErrAdapterUnsupported", err)
	}
	if adapter.callCount != 0 {
		t.Fatalf("adapter callback should not fire when capability is off; calls = %d", adapter.callCount)
	}
}

func TestHandleChildConversation_NoAdapter(t *testing.T) {
	parent := childParentRef()
	deps := childTestDeps(nil, parent) // none registered
	if _, err := HandleChildConversation(context.Background(), deps, ChildConversationRequest{Parent: parent, Label: "x"}); err == nil {
		t.Fatalf("expected error when no adapter is registered for the parent provider")
	}
}

func TestHandleChildConversation_AdapterError(t *testing.T) {
	parent := childParentRef()
	adapter := &childTestAdapter{caps: AdapterCapabilities{SupportsChildConversations: true}, childErr: errors.New("boom")}
	deps := childTestDeps(adapter, parent)
	if _, err := HandleChildConversation(context.Background(), deps, ChildConversationRequest{Parent: parent, Label: "x"}); err == nil {
		t.Fatalf("expected the adapter error to propagate")
	}
}

func TestHandleChildConversation_NilChild(t *testing.T) {
	parent := childParentRef()
	adapter := &childTestAdapter{caps: AdapterCapabilities{SupportsChildConversations: true}, childRef: nil}
	deps := childTestDeps(adapter, parent)
	if _, err := HandleChildConversation(context.Background(), deps, ChildConversationRequest{Parent: parent, Label: "x"}); err == nil {
		t.Fatalf("expected error when the adapter returns a nil child conversation")
	}
}

func TestHandleChildConversation_NilRegistry(t *testing.T) {
	parent := childParentRef()
	if _, err := HandleChildConversation(context.Background(), ChildConversationDeps{}, ChildConversationRequest{Parent: parent}); err == nil {
		t.Fatalf("expected error when the adapter registry is nil")
	}
}
