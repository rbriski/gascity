package runtime

import "strings"

// Coarse reason tokens for recoverable provider transport failures. They double
// as the dedup fingerprint for the recovery state machine (ga-qox): one failure
// episode keeps a single reason across reconcile ticks so retries stay bounded,
// while a genuinely different failure class re-arms recovery.
const (
	// transportFailureDNSLookup is a name-resolution failure — Node's ENOTFOUND
	// (Claude) or the Rust std/reqwest "failed to lookup address information"
	// (Codex).
	transportFailureDNSLookup = "dns_lookup_failure"
	// transportFailureStreamDisconnected is Codex's "stream disconnected before
	// completion" — the provider stream dropped mid-response.
	transportFailureStreamDisconnected = "stream_disconnected"
	// transportFailureWSFallback is Codex falling back from WebSockets to HTTPS
	// after a transport timeout.
	transportFailureWSFallback = "transport_ws_fallback"
	// transportFailureRequestSend is the reqwest "error sending request for url"
	// transport error.
	transportFailureRequestSend = "request_send_failure"
)

// transportFailureTailLines bounds how close to the current prompt a transport
// failure must appear to count as the turn's final output. A tight recent
// suffix is what keeps pasted or scrolled-back error text from false-triggering
// recovery: only a live-turn failure that left the session inert at its prompt
// qualifies (ga-qox forensic guidance).
const transportFailureTailLines = 20

// ClassifyInertTransportFailure reports whether pane content shows a provider
// transport failure that aborted the turn and left the session inert at its
// prompt, plus a stable coarse reason token for dedup and telemetry.
//
// recoverable is true only when BOTH hold within the recent tail:
//   - a high-confidence, provider-neutral transport-failure signature (Codex
//     WebSockets→HTTPS fallback timeout / HTTPS stream disconnect, or a DNS
//     lookup failure such as Claude's ENOTFOUND), and
//   - an idle prompt indicator, proving the turn ended and the agent is waiting
//     for input rather than still working.
//
// Requiring the idle prompt means an active turn (still retrying internally, no
// prompt) is never classified; requiring the signature means an ordinary idle
// prompt is never classified; bounding both to the recent tail means historical
// or pasted error text cannot false-trigger. The classifier is pure and
// provider-neutral — it matches provider error strings, never a role or a
// provider name in Go control flow.
func ClassifyInertTransportFailure(content string) (recoverable bool, reason string) {
	tail := recentTail(content, transportFailureTailLines)
	if tail == "" {
		return false, ""
	}
	if !containsPromptIndicator(tail) {
		return false, ""
	}
	reason = transportFailureReason(tail)
	return reason != "", reason
}

// transportFailureReason returns the coarse reason token for the highest-signal
// transport failure present in content, or "" if none. Precedence surfaces the
// most actionable root cause first: a name-resolution failure is reported over
// the downstream stream/transport symptoms it triggers.
func transportFailureReason(content string) string {
	switch {
	case containsDNSLookupFailure(content):
		return transportFailureDNSLookup
	case strings.Contains(content, "stream disconnected before completion"):
		return transportFailureStreamDisconnected
	case strings.Contains(content, "Falling back from WebSockets to HTTPS transport"):
		return transportFailureWSFallback
	case strings.Contains(content, "error sending request for url"):
		return transportFailureRequestSend
	default:
		return ""
	}
}

// containsDNSLookupFailure detects a name-resolution failure across providers:
// Node's ENOTFOUND (Claude) and the Rust std/reqwest "failed to lookup address
// information" (Codex).
func containsDNSLookupFailure(content string) bool {
	return strings.Contains(content, "ENOTFOUND") ||
		strings.Contains(content, "failed to lookup address information")
}

// recentTail returns at most the last n lines of content, joined with newlines.
// It bounds a screen scan to the recent suffix so older scrollback cannot
// contribute to a match.
func recentTail(content string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
