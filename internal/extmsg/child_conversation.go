package extmsg

import (
	"context"
	"errors"
	"fmt"
)

// ChildConversationDeps bundles the dependencies for child-conversation
// materialization.
type ChildConversationDeps struct {
	Registry *AdapterRegistry
}

// ChildConversationRequest specifies the parent conversation to spawn a child
// (thread) under, plus a human-friendly label for the child (e.g. a thread
// title).
type ChildConversationRequest struct {
	Parent ConversationRef
	Label  string
}

// HandleChildConversation materializes a child conversation (a thread) under a
// parent conversation by invoking the provider adapter's
// EnsureChildConversation callback.
//
// This is the production trigger for the adapter child-conversation callback:
// the fabric resolves the adapter for the parent conversation, enforces its
// SupportsChildConversations capability (returning ErrAdapterUnsupported when
// the adapter does not advertise support), and forwards the request. The
// adapter creates the platform thread and returns the child reference
// (Kind=thread, ParentConversationID set, a provider-assigned ConversationID),
// which becomes the authoritative identifier for the child going forward.
//
// Recording the child as a conversation group (and registering participants)
// is the caller's responsibility, mirroring the inbound/outbound split where
// HandleX returns the routing result and the caller fans out side effects.
func HandleChildConversation(ctx context.Context, deps ChildConversationDeps, req ChildConversationRequest) (*ConversationRef, error) {
	if deps.Registry == nil {
		return nil, errors.New("adapter registry is nil")
	}
	adapter := deps.Registry.LookupByConversation(req.Parent)
	if adapter == nil {
		return nil, fmt.Errorf("no adapter for %s/%s", req.Parent.Provider, req.Parent.AccountID)
	}
	if !adapter.Capabilities().SupportsChildConversations {
		return nil, fmt.Errorf("%w: provider %q does not support child conversations", ErrAdapterUnsupported, req.Parent.Provider)
	}
	child, err := adapter.EnsureChildConversation(ctx, req.Parent, req.Label)
	if err != nil {
		return nil, fmt.Errorf("ensure child conversation: %w", err)
	}
	if child == nil {
		return nil, errors.New("adapter returned nil child conversation")
	}
	return child, nil
}
