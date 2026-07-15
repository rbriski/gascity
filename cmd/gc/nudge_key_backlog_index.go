package main

import (
	"container/heap"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
)

type nudgeKeyBacklogEntry struct {
	admitted time.Time
	index    int
}

type nudgeKeyBacklogHeap []*nudgeKeyBacklogEntry

func (h nudgeKeyBacklogHeap) Len() int { return len(h) }

func (h nudgeKeyBacklogHeap) Less(i, j int) bool {
	return h[i].admitted.Before(h[j].admitted)
}

func (h nudgeKeyBacklogHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *nudgeKeyBacklogHeap) Push(value any) {
	entry := value.(*nudgeKeyBacklogEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}

func (h *nudgeKeyBacklogHeap) Pop() any {
	old := *h
	last := len(old) - 1
	entry := old[last]
	old[last] = nil
	entry.index = -1
	*h = old[:last]
	return entry
}

// setPendingLocked updates pending and its oldest-admission index atomically.
// The caller must hold c.mu.
func (c *nudgeKeyController) setPendingLocked(key reconcilekey.Session, next nudgeReconcileBatch) {
	previous, existed := c.pending[key]
	c.pending[key] = next
	if existed && previous.FirstEnqueuedAt.Equal(next.FirstEnqueuedAt) {
		return
	}
	if existed {
		c.removeNudgeKeyBacklogEntryLocked(key, previous)
	}
	c.addNudgeKeyBacklogEntryLocked(key, next)
}

// deletePendingLocked removes pending and its index entry atomically. The
// caller must hold c.mu.
func (c *nudgeKeyController) deletePendingLocked(key reconcilekey.Session) (nudgeReconcileBatch, bool) {
	batch, existed := c.pending[key]
	if !existed {
		return nudgeReconcileBatch{}, false
	}
	c.removeNudgeKeyBacklogEntryLocked(key, batch)
	delete(c.pending, key)
	return batch, true
}

func (c *nudgeKeyController) addNudgeKeyBacklogEntryLocked(key reconcilekey.Session, batch nudgeReconcileBatch) {
	if batch.FirstEnqueuedAt.IsZero() {
		c.backlogUnavailable++
		return
	}
	if c.backlogEntries == nil {
		c.backlogEntries = make(map[reconcilekey.Session]*nudgeKeyBacklogEntry)
	}
	entry := &nudgeKeyBacklogEntry{admitted: batch.FirstEnqueuedAt}
	c.backlogEntries[key] = entry
	heap.Push(&c.backlogOldest, entry)
}

func (c *nudgeKeyController) removeNudgeKeyBacklogEntryLocked(key reconcilekey.Session, batch nudgeReconcileBatch) {
	if batch.FirstEnqueuedAt.IsZero() {
		if c.backlogUnavailable > 0 {
			c.backlogUnavailable--
		}
		return
	}
	entry := c.backlogEntries[key]
	delete(c.backlogEntries, key)
	if entry == nil || entry.index < 0 || entry.index >= len(c.backlogOldest) || c.backlogOldest[entry.index] != entry {
		return
	}
	heap.Remove(&c.backlogOldest, entry.index)
}

// clearPendingLocked releases all pending backlog state. The caller must hold
// c.mu.
func (c *nudgeKeyController) clearPendingLocked() {
	clear(c.pending)
	clear(c.backlogEntries)
	c.backlogOldest = nil
	c.backlogUnavailable = 0
}
