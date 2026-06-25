package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
)

// TestBeadmailConformance_SQLite runs the full mail Provider conformance suite
// against a beadmail provider backed by a real embedded SQLite store (the cutover
// target), proving mail's message operations — type=message, the ephemeral/wisp
// tier, TierBoth scans, and the thread/reply/read label queries — behave
// identically on SQLite as on bd/MemStore. It is the foundation gate for the mail
// SQLite cutover: if mail cannot pass conformance on SQLite, no flag flip is safe.
//
// Both seams are pointed at the same SQLite store here (the conformance suite
// exercises message ops plus recipient/session resolution); the production
// cutover keeps sessions on bd until sessions relocate.
func TestBeadmailConformance_SQLite(t *testing.T) {
	mailtest.RunProviderTests(t, func(t *testing.T) mail.Provider {
		store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcm"))
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			t.Cleanup(func() { _ = closer.CloseStore() })
		}
		return New(store)
	})
}
