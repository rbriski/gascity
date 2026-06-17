package events

import "encoding/json"

// Domain payload types shared across packages. Payloads specific to one
// package live with their emitter (see internal/api/event_payloads.go and
// internal/extmsg/events.go).

// BeadWorktreeReapedPayload is the typed payload for bead.worktree.reaped
// events. Emitted when the worktree reaper successfully removes a merged
// worktree and its branch after a bead is closed.
type BeadWorktreeReapedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Branch string `json:"branch"`
}

// IsEventPayload marks BeadWorktreeReapedPayload as an events.Payload variant.
func (BeadWorktreeReapedPayload) IsEventPayload() {}

// BeadWorktreeReapSkippedPayload is the typed payload for
// bead.worktree.reap_skipped events. Emitted when the worktree reaper
// decides not to remove a worktree (e.g., unmerged changes, open bead).
type BeadWorktreeReapSkippedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Reason string `json:"reason"`
}

// IsEventPayload marks BeadWorktreeReapSkippedPayload as an events.Payload variant.
func (BeadWorktreeReapSkippedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(BeadWorktreeReaped, BeadWorktreeReapedPayload{})
	RegisterPayload(BeadWorktreeReapSkipped, BeadWorktreeReapSkippedPayload{})
}

// SessionResetStalledPayload is the typed payload for
// session.reset_stalled events. It identifies the session whose reset
// completion has stalled and the reset timestamp used to compute the
// elapsed diagnostic threshold.
type SessionResetStalledPayload struct {
	SessionName      string `json:"session_name"`
	Template         string `json:"template"`
	ResetCommittedAt string `json:"reset_committed_at"`
	ElapsedSeconds   int    `json:"elapsed_s"`
}

// IsEventPayload marks SessionResetStalledPayload as an events.Payload variant.
func (SessionResetStalledPayload) IsEventPayload() {}

// SessionResetStalledPayloadJSON builds the JSON wire form for attachment to
// an Event.Payload field.
func SessionResetStalledPayloadJSON(sessionName, template, resetCommittedAt string, elapsedSeconds int) json.RawMessage {
	b, _ := json.Marshal(SessionResetStalledPayload{
		SessionName:      sessionName,
		Template:         template,
		ResetCommittedAt: resetCommittedAt,
		ElapsedSeconds:   elapsedSeconds,
	})
	return b
}
