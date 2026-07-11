package pgauth

import "github.com/gastownhall/gascity/internal/events"

// PostgresCredentialResolvedPayload is emitted on
// events.PostgresCredentialResolved each time gc successfully resolves a
// Postgres password for a scope. The payload identifies the scope and
// the resolution tier that supplied the value; it never carries the
// value itself (asserted by TestPostgresEventOmitsPassword).
type PostgresCredentialResolvedPayload struct {
	ScopeKind string `json:"scope_kind"` // "city" or "rig"
	ScopeName string `json:"scope_name"` // city name, or rig name (no scheme prefix)
	Source    string `json:"source"`     // pgauth.Source.String()
	Host      string `json:"host"`       // effective endpoint host (contract.MetadataState.PostgresEndpoint)
	Port      string `json:"port"`       // effective endpoint port (string; discrete metadata or DSN-derived)
	User      string `json:"user"`       // effective endpoint user (may be empty when the DSN omits one)
}

// IsEventPayload marks PostgresCredentialResolvedPayload as an
// events.Payload variant.
func (PostgresCredentialResolvedPayload) IsEventPayload() {}

func init() {
	events.RegisterPayload(events.PostgresCredentialResolved, PostgresCredentialResolvedPayload{})
}
