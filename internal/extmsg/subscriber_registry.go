package extmsg

import "sync"

// SSEPublishReceipt from SubscriberRegistry.Publish tracks whether the event was
// delivered and how many events were dropped on a full buffer.
type SSEPublishReceipt struct {
	Delivered bool
	Dropped   int
}

// subscriberEntry holds the channel and cancel func for one subscriber.
type subscriberEntry struct {
	ch     chan SSEEvent
	cancel func()
	drops  int
}

// SubscriberRegistry is a goroutine-safe registry of SSE event subscribers
// keyed by ConversationRef. At most one subscriber per conversation is
// supported; a new Subscribe call replaces a stale entry.
type SubscriberRegistry struct {
	mu          sync.Mutex
	subscribers map[string]*subscriberEntry
}

// NewSubscriberRegistry creates an empty subscriber registry.
func NewSubscriberRegistry() *SubscriberRegistry {
	return &SubscriberRegistry{
		subscribers: make(map[string]*subscriberEntry),
	}
}

func conversationKey(ref ConversationRef) string {
	return conversationLockKey(ref)
}

// Subscribe creates a buffered channel for the given conversation. A second
// call for the same conversation cancels the first entry and replaces it.
// The returned cancel func removes the entry from the registry and closes
// the channel.
func (r *SubscriberRegistry) Subscribe(ref ConversationRef, bufSize int) (<-chan SSEEvent, func()) {
	key := conversationKey(ref)
	ch := make(chan SSEEvent, bufSize)

	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if e, ok := r.subscribers[key]; ok && e.ch == ch {
				delete(r.subscribers, key)
				close(ch)
			}
		})
	}

	r.mu.Lock()
	if old, ok := r.subscribers[key]; ok {
		old.cancel()
	}
	r.subscribers[key] = &subscriberEntry{ch: ch, cancel: cancel}
	r.mu.Unlock()

	return ch, cancel
}

// Publish delivers an event to the subscriber for the given conversation.
// If there is no subscriber, Delivered is false and no error is returned.
// If the subscriber's buffer is full, the oldest queued event is dropped
// and the new event is written; the drop count is tracked on the entry.
// Publish is non-blocking.
func (r *SubscriberRegistry) Publish(ref ConversationRef, event SSEEvent) SSEPublishReceipt {
	key := conversationKey(ref)

	r.mu.Lock()
	entry, ok := r.subscribers[key]
	r.mu.Unlock()

	if !ok {
		return SSEPublishReceipt{Delivered: false}
	}

	dropped := 0
	// Non-blocking send: if the buffer is full, drain one entry first.
	select {
	case entry.ch <- event:
	default:
		// Buffer full: drop oldest.
		select {
		case <-entry.ch:
			dropped = 1
		default:
		}
		// Try again; if still blocked (race with cancel), count as drop.
		select {
		case entry.ch <- event:
		default:
			dropped++
			r.mu.Lock()
			if e, ok := r.subscribers[key]; ok && e == entry {
				e.drops += dropped
			}
			r.mu.Unlock()
			return SSEPublishReceipt{Delivered: false, Dropped: dropped}
		}
	}

	if dropped > 0 {
		r.mu.Lock()
		if e, ok := r.subscribers[key]; ok && e == entry {
			e.drops += dropped
		}
		r.mu.Unlock()
	}

	return SSEPublishReceipt{Delivered: true, Dropped: dropped}
}

// DropCount returns the number of events dropped for the conversation due to
// buffer overflow.
func (r *SubscriberRegistry) DropCount(ref ConversationRef) int {
	key := conversationKey(ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.subscribers[key]; ok {
		return e.drops
	}
	return 0
}
