package contract

import (
	"fmt"
	"net/url"
	"strings"
)

// PostgresEndpoint is the effective connection tuple for a postgres-backed
// scope: the values gc projects to bd subprocesses (BEADS_POSTGRES_* legacy
// names) and keys credential lookups on ([host:port] credentials-file
// sections). User and Database may be empty when a bd-main postgres_dsn
// omits them; Host and Port are always populated for a successfully derived
// endpoint (Port defaults to 5432).
type PostgresEndpoint struct {
	Host     string
	Port     string
	User     string
	Database string
}

// ParsePostgresDSNEndpoint derives the endpoint from a postgres:// (or
// postgresql://) URL — the password-free shape bd origin/main persists to
// metadata.json as postgres_dsn (`bd init --backend=postgres`, see beads
// internal/configfile/configfile.go). A missing port defaults to 5432.
// Query parameters (sslmode etc.) are bd's business and are ignored. A
// userinfo password is ignored: bd never persists one (RedactPassword fails
// closed), and the password reaches bd via BEADS_PG_PASSWORD at command
// time.
//
// The libpq keyword/value form ("host=... port=...") is not supported here;
// bd accepts it at init time, but gc requires the URL form to derive an
// endpoint deterministically.
func ParsePostgresDSNEndpoint(dsn string) (PostgresEndpoint, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return PostgresEndpoint{}, fmt.Errorf("postgres_dsn is empty")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return PostgresEndpoint{}, fmt.Errorf("parse postgres_dsn: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
	default:
		return PostgresEndpoint{}, fmt.Errorf("postgres_dsn must be a postgres:// (or postgresql://) URL, got scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return PostgresEndpoint{}, fmt.Errorf("postgres_dsn has no host")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	return PostgresEndpoint{
		Host:     host,
		Port:     port,
		User:     user,
		Database: strings.TrimPrefix(u.Path, "/"),
	}, nil
}

// PostgresEndpoint returns the effective endpoint for a postgres-backed
// scope. Discrete metadata fields (the draft-era split shape) win when
// present; any missing field is derived from PostgresDSN (bd origin/main's
// shape). A scope with neither a DSN nor the complete split-field set is an
// error — LoadMetadataState rejects that shape up front, so states it
// returns always derive cleanly.
func (s MetadataState) PostgresEndpoint() (PostgresEndpoint, error) {
	ep := PostgresEndpoint{
		Host:     strings.TrimSpace(s.PostgresHost),
		Port:     strings.TrimSpace(s.PostgresPort),
		User:     strings.TrimSpace(s.PostgresUser),
		Database: strings.TrimSpace(s.PostgresDatabase),
	}
	if ep.Host != "" && ep.Port != "" && ep.User != "" && ep.Database != "" {
		return ep, nil
	}
	dsn := strings.TrimSpace(s.PostgresDSN)
	if dsn == "" {
		return PostgresEndpoint{}, fmt.Errorf("postgres scope requires postgres_dsn or all of postgres_host, postgres_port, postgres_user, postgres_database")
	}
	derived, err := ParsePostgresDSNEndpoint(dsn)
	if err != nil {
		return PostgresEndpoint{}, err
	}
	if ep.Host == "" {
		ep.Host = derived.Host
	}
	if ep.Port == "" {
		ep.Port = derived.Port
	}
	if ep.User == "" {
		ep.User = derived.User
	}
	if ep.Database == "" {
		ep.Database = derived.Database
	}
	return ep, nil
}
