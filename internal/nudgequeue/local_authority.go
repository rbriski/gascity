package nudgequeue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/filelock"

	// modernc registers the local authority's database/sql driver.
	_ "modernc.org/sqlite"
)

const (
	localNudgeAuthorityFileName = "local-authority-v1.sqlite"
	localNudgeAuthoritySchema   = 1
	// LocalNudgeAuthorityProfileStoreWriterIsController is the sole security
	// profile supported by the local single-controller authority journal.
	LocalNudgeAuthorityProfileStoreWriterIsController = string(CommandSecurityProfileStoreWriterIsController)
)

var (
	// ErrLocalNudgeAuthorityConflict reports immutable identity, idempotency,
	// lineage, or state-machine evidence that differs from the durable journal.
	ErrLocalNudgeAuthorityConflict = errors.New("local nudge authority conflict")
	// ErrLocalNudgeAuthorityUnavailable reports a closed or unreadable authority
	// journal. Callers must freeze effects rather than infer authority.
	ErrLocalNudgeAuthorityUnavailable = errors.New("local nudge authority unavailable")
)

// AuthenticatedNudgeRequester is server-owned authentication evidence attached
// to a request context after the API mutation gate verifies it. None of these
// fields are accepted from the nudge request body.
type AuthenticatedNudgeRequester struct {
	PrincipalID     string
	TenantScope     string
	CityScope       string
	CredentialClass string
	EvidenceID      string
}

type authenticatedNudgeRequesterContextKey struct{}

// WithAuthenticatedNudgeRequester attaches trusted requester evidence to ctx.
// Callers must use it only after authenticating the transport credential.
func WithAuthenticatedNudgeRequester(ctx context.Context, requester AuthenticatedNudgeRequester) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, authenticatedNudgeRequesterContextKey{}, requester)
}

// LocalNudgeAuthorityOptions binds one local authority journal to an explicit
// single-controller security profile and immutable policy identity.
type LocalNudgeAuthorityOptions struct {
	Profile         string
	AuthorityID     string
	Issuer          string
	TenantScope     string
	CityScope       string
	CredentialClass string
	PolicyVersion   string
}

// LocalNudgeAuthority is the independently durable authorization, partition,
// and terminal-intent journal for the explicit local single-controller profile.
// It holds an exclusive lock for its lifetime; hosted/multi-controller use is
// intentionally unsupported.
type LocalNudgeAuthority struct {
	mu       sync.RWMutex
	db       *sql.DB
	lock     *os.File
	path     string
	fileInfo os.FileInfo
	store    CommandStoreBinding
	opts     LocalNudgeAuthorityOptions
	closed   bool
}

// LocalNudgeAuthorityPath returns the canonical independent authority database
// path for a city.
func LocalNudgeAuthorityPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, restoreAnchorDirectoryName, localNudgeAuthorityFileName)
}

// OpenLocalNudgeAuthority securely opens or initializes the explicit local
// authority journal. A missing journal may be initialized only against an
// empty command repository; nonempty bootstrap and lineage mismatch fail closed.
func OpenLocalNudgeAuthority(ctx context.Context, cityPath string, state CommandRepositoryState, opts LocalNudgeAuthorityOptions) (_ *LocalNudgeAuthority, err error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return nil, err
	}
	if err := validateLocalNudgeAuthorityOpen(state, opts); err != nil {
		return nil, err
	}
	path := LocalNudgeAuthorityPath(cityPath)
	parent := filepath.Dir(path)
	if err := ensureRestoreAnchorDirectory(parent, osRestoreAnchorFileOps.syncDirectory); err != nil {
		return nil, fmt.Errorf("opening local nudge authority: %w", err)
	}
	lock, err := acquireRestoreAnchorLock(path)
	if err != nil {
		return nil, fmt.Errorf("opening local nudge authority: %w", err)
	}
	defer func() {
		if err != nil {
			_ = filelock.Unlock(lock)
			_ = lock.Close()
		}
	}()

	_, statErr := os.Lstat(path)
	newFile := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !newFile {
		return nil, fmt.Errorf("opening local nudge authority: lstat database: %w", statErr)
	}
	if newFile {
		if state.Revision != 0 || state.SequenceHighWater != 0 {
			return nil, fmt.Errorf("%w: refusing to initialize authority against repository revision %d sequence %d", ErrLocalNudgeAuthorityConflict, state.Revision, state.SequenceHighWater)
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, fmt.Errorf("opening local nudge authority: create database: %w", createErr)
		}
		if syncErr := file.Sync(); syncErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("opening local nudge authority: sync new database: %w", syncErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("opening local nudge authority: close new database: %w", closeErr)
		}
		if syncErr := osRestoreAnchorFileOps.syncDirectory(parent); syncErr != nil {
			return nil, fmt.Errorf("opening local nudge authority: sync database parent: %w", syncErr)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("opening local nudge authority: lstat live database: %w", err)
	}
	if err := validateLocalNudgeAuthorityFileInfo(info); err != nil {
		return nil, fmt.Errorf("opening local nudge authority: %w", err)
	}

	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening local nudge authority sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	authority := &LocalNudgeAuthority{db: db, lock: lock, path: path, fileInfo: info, store: state.Store, opts: opts}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	if err := authority.configure(ctx); err != nil {
		return nil, err
	}
	if err := authority.validateLivePath(); err != nil {
		return nil, err
	}
	if err := authority.initializeOrValidate(ctx, state); err != nil {
		return nil, err
	}
	if err := authority.validateLivePath(); err != nil {
		return nil, err
	}
	return authority, nil
}

func validateLocalNudgeAuthorityOpen(state CommandRepositoryState, opts LocalNudgeAuthorityOptions) error {
	if validateCommandRepositoryBinding(state.Store) != nil || state.SchemaVersion != CommandRepositorySchemaVersion ||
		state.WriterVersion != CommandRepositoryWriterVersion || state.SequenceHighWater > state.Revision {
		return fmt.Errorf("%w: command repository state is invalid or unsupported", ErrLocalNudgeAuthorityConflict)
	}
	if opts.Profile != LocalNudgeAuthorityProfileStoreWriterIsController {
		return fmt.Errorf("%w: explicit profile %q is required", ErrLocalNudgeAuthorityConflict, LocalNudgeAuthorityProfileStoreWriterIsController)
	}
	for _, field := range []struct{ name, value string }{
		{"authority id", opts.AuthorityID},
		{"issuer", opts.Issuer},
		{"tenant scope", opts.TenantScope},
		{"city scope", opts.CityScope},
		{"credential class", opts.CredentialClass},
		{"policy version", opts.PolicyVersion},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
		}
	}
	return nil
}

func validateLocalNudgeAuthorityFileInfo(info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: authority database is not a regular file", ErrRestoreAnchorUnsafePath)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: authority database mode is %v, want 0600", ErrRestoreAnchorUnsafePath, info.Mode())
	}
	return nil
}

func (a *LocalNudgeAuthority) configure(ctx context.Context) error {
	if err := a.db.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: ping: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	var journalMode string
	if err := a.db.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&journalMode); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: setting DELETE journal mode: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		return fmt.Errorf("%w: opening local nudge authority: journal mode is %q, want DELETE", ErrLocalNudgeAuthorityUnavailable, journalMode)
	}
	if _, err := a.db.ExecContext(ctx, `PRAGMA synchronous = FULL`); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: setting FULL synchronous mode: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if _, err := a.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: enabling foreign keys: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	var synchronous, foreignKeys int
	if err := a.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: reading synchronous mode: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if synchronous != 2 {
		return fmt.Errorf("%w: opening local nudge authority: synchronous=%d, want FULL(2)", ErrLocalNudgeAuthorityUnavailable, synchronous)
	}
	if err := a.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: reading foreign-key mode: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("%w: opening local nudge authority: foreign_keys=%d, want 1", ErrLocalNudgeAuthorityUnavailable, foreignKeys)
	}
	var integrity string
	if err := a.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("%w: opening local nudge authority: quick_check: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if integrity != "ok" {
		return fmt.Errorf("%w: opening local nudge authority: quick_check=%q", ErrLocalNudgeAuthorityUnavailable, integrity)
	}
	return nil
}

func (a *LocalNudgeAuthority) initializeOrValidate(ctx context.Context, state CommandRepositoryState) error {
	objects, err := localAuthoritySchemaObjects(ctx, a.db)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		if state.Revision != 0 || state.SequenceHighWater != 0 {
			return fmt.Errorf("%w: empty authority database cannot bind a nonempty repository", ErrLocalNudgeAuthorityConflict)
		}
		return a.initializeSchema(ctx, state)
	}
	want := []string{
		"index:memberships_partition_active",
		"index:memberships_partition_terminal",
		"table:authority_meta",
		"table:ingress_grants",
		"table:memberships",
		"table:terminal_preparations",
	}
	if strings.Join(objects, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("%w: authority schema objects=%v, want %v", ErrLocalNudgeAuthorityConflict, objects, want)
	}
	return a.validateMetadata(ctx, state)
}

func localAuthoritySchemaObjects(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("opening local nudge authority: listing schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var objectType, name string
		if err := rows.Scan(&objectType, &name); err != nil {
			return nil, fmt.Errorf("opening local nudge authority: scanning schema: %w", err)
		}
		names = append(names, objectType+":"+name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("opening local nudge authority: listing schema: %w", err)
	}
	return names, nil
}

func (a *LocalNudgeAuthority) initializeSchema(ctx context.Context, state CommandRepositoryState) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("initializing local nudge authority: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range localNudgeAuthoritySchemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initializing local nudge authority schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO authority_meta (
		singleton, schema_version, profile, store_uuid, restore_epoch, authority_id, issuer,
		tenant_scope, city_scope, credential_class, policy_version, principal_schema, dense_admission_high_water,
		highest_observed_sequence, highest_observed_revision
	) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		localNudgeAuthoritySchema, a.opts.Profile, state.Store.StoreUUID, encodeLocalAuthorityUint64(state.Store.RestoreEpoch),
		a.opts.AuthorityID, a.opts.Issuer, a.opts.TenantScope, a.opts.CityScope, a.opts.CredentialClass,
		a.opts.PolicyVersion, NudgePrincipalSchemaVersion, encodeLocalAuthorityUint64(0),
		encodeLocalAuthorityUint64(0), encodeLocalAuthorityUint64(0)); err != nil {
		return fmt.Errorf("initializing local nudge authority metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("initializing local nudge authority commit: %w", err)
	}
	return osRestoreAnchorFileOps.syncDirectory(filepath.Dir(a.path))
}

var localNudgeAuthoritySchemaStatements = []string{
	`CREATE TABLE authority_meta (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1), schema_version INTEGER NOT NULL,
		profile TEXT NOT NULL, store_uuid TEXT NOT NULL, restore_epoch BLOB NOT NULL CHECK (length(restore_epoch) = 8),
		authority_id TEXT NOT NULL, issuer TEXT NOT NULL, tenant_scope TEXT NOT NULL, city_scope TEXT NOT NULL,
		credential_class TEXT NOT NULL, policy_version TEXT NOT NULL, principal_schema INTEGER NOT NULL,
		dense_admission_high_water BLOB NOT NULL CHECK (length(dense_admission_high_water) = 8),
		highest_observed_sequence BLOB NOT NULL CHECK (length(highest_observed_sequence) = 8),
		highest_observed_revision BLOB NOT NULL CHECK (length(highest_observed_revision) = 8)
	)`,
	`CREATE TABLE ingress_grants (
		reference_id TEXT PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, request_fingerprint BLOB NOT NULL CHECK (length(request_fingerprint) = 32),
		command_id TEXT NOT NULL UNIQUE, principal_schema INTEGER NOT NULL, issuer TEXT NOT NULL, principal_id TEXT NOT NULL,
		tenant_scope TEXT NOT NULL, city_scope TEXT NOT NULL, credential_class TEXT NOT NULL, policy_version TEXT NOT NULL,
		policy_decision_id TEXT NOT NULL, action TEXT NOT NULL, target_session_id TEXT NOT NULL,
		payload_digest TEXT NOT NULL, command_created_at TEXT NOT NULL, issued_at TEXT NOT NULL, expires_at TEXT NOT NULL
	)`,
	`CREATE TABLE memberships (
		command_id TEXT PRIMARY KEY REFERENCES ingress_grants(command_id), sequence BLOB NOT NULL UNIQUE CHECK (length(sequence) = 8),
		admission_revision BLOB NOT NULL CHECK (length(admission_revision) = 8), partition_id BLOB NOT NULL CHECK (length(partition_id) = 32),
		terminal_revision BLOB CHECK (terminal_revision IS NULL OR length(terminal_revision) = 8),
		terminal_digest BLOB CHECK (terminal_digest IS NULL OR length(terminal_digest) = 32),
		CHECK ((terminal_revision IS NULL) = (terminal_digest IS NULL))
	)`,
	`CREATE TABLE terminal_preparations (
		command_id TEXT PRIMARY KEY REFERENCES memberships(command_id), repository_before_revision BLOB NOT NULL CHECK (length(repository_before_revision) = 8),
		before_digest BLOB NOT NULL CHECK (length(before_digest) = 32), terminal_revision BLOB NOT NULL CHECK (length(terminal_revision) = 8),
		terminal_digest BLOB NOT NULL CHECK (length(terminal_digest) = 32)
	)`,
	`CREATE INDEX memberships_partition_active ON memberships(partition_id, admission_revision, sequence) WHERE terminal_revision IS NULL`,
	`CREATE INDEX memberships_partition_terminal ON memberships(partition_id, terminal_revision, sequence) WHERE terminal_revision IS NOT NULL`,
}

func (a *LocalNudgeAuthority) validateMetadata(ctx context.Context, state CommandRepositoryState) error {
	var (
		schema, principalSchema                                                                         int
		profile, storeUUID, authorityID, issuer, tenantScope, cityScope, credentialClass, policyVersion string
		restoreEpoch, dense, highestSequenceWire, highestRevisionWire                                   []byte
	)
	err := a.db.QueryRowContext(ctx, `SELECT schema_version, profile, store_uuid, restore_epoch, authority_id, issuer,
		tenant_scope, city_scope, credential_class, policy_version, principal_schema, dense_admission_high_water,
		highest_observed_sequence, highest_observed_revision
		FROM authority_meta WHERE singleton = 1`).Scan(
		&schema, &profile, &storeUUID, &restoreEpoch, &authorityID, &issuer,
		&tenantScope, &cityScope, &credentialClass, &policyVersion, &principalSchema, &dense,
		&highestSequenceWire, &highestRevisionWire,
	)
	if err != nil {
		return fmt.Errorf("%w: reading authority metadata: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	epoch, err := decodeLocalAuthorityUint64(restoreEpoch)
	if err != nil {
		return err
	}
	denseHighWater, err := decodeLocalAuthorityUint64(dense)
	if err != nil {
		return err
	}
	highestSequence, err := decodeLocalAuthorityUint64(highestSequenceWire)
	if err != nil {
		return err
	}
	highestRevision, err := decodeLocalAuthorityUint64(highestRevisionWire)
	if err != nil {
		return err
	}
	if schema != localNudgeAuthoritySchema || profile != a.opts.Profile || storeUUID != state.Store.StoreUUID || epoch != state.Store.RestoreEpoch ||
		authorityID != a.opts.AuthorityID || issuer != a.opts.Issuer || tenantScope != a.opts.TenantScope || cityScope != a.opts.CityScope ||
		credentialClass != a.opts.CredentialClass || policyVersion != a.opts.PolicyVersion || principalSchema != int(NudgePrincipalSchemaVersion) ||
		denseHighWater > highestSequence || highestSequence > state.SequenceHighWater || highestRevision > state.Revision {
		return fmt.Errorf("%w: authority metadata differs from configured repository lineage", ErrLocalNudgeAuthorityConflict)
	}
	if highestSequence != state.SequenceHighWater || highestRevision != state.Revision {
		if _, err := a.db.ExecContext(ctx, `UPDATE authority_meta SET highest_observed_sequence = ?, highest_observed_revision = ? WHERE singleton = 1`,
			encodeLocalAuthorityUint64(state.SequenceHighWater), encodeLocalAuthorityUint64(state.Revision)); err != nil {
			return fmt.Errorf("%w: advancing observed repository authority: %w", ErrLocalNudgeAuthorityUnavailable, err)
		}
	}
	return nil
}

func encodeLocalAuthorityUint64(value uint64) []byte {
	wire := make([]byte, 8)
	binary.BigEndian.PutUint64(wire, value)
	return wire
}

func decodeLocalAuthorityUint64(wire []byte) (uint64, error) {
	if len(wire) != 8 {
		return 0, fmt.Errorf("%w: invalid uint64 evidence length %d", ErrLocalNudgeAuthorityConflict, len(wire))
	}
	return binary.BigEndian.Uint64(wire), nil
}

func (a *LocalNudgeAuthority) begin(ctx context.Context) (func(), error) {
	if a == nil {
		return nil, fmt.Errorf("%w: authority is nil", ErrLocalNudgeAuthorityUnavailable)
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return nil, err
	}
	a.mu.RLock()
	if a.closed || a.db == nil || a.lock == nil {
		a.mu.RUnlock()
		return nil, fmt.Errorf("%w: authority is closed", ErrLocalNudgeAuthorityUnavailable)
	}
	if err := a.validateLivePath(); err != nil {
		a.mu.RUnlock()
		return nil, err
	}
	return a.mu.RUnlock, nil
}

func (a *LocalNudgeAuthority) validateLivePath() error {
	info, err := os.Lstat(a.path)
	if err != nil {
		return fmt.Errorf("%w: authority database path is unavailable: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if err := validateLocalNudgeAuthorityFileInfo(info); err != nil {
		return fmt.Errorf("%w: authority database path became unsafe: %w", ErrLocalNudgeAuthorityUnavailable, err)
	}
	if a.fileInfo == nil || !os.SameFile(a.fileInfo, info) {
		return fmt.Errorf("%w: authority database path was replaced", ErrLocalNudgeAuthorityUnavailable)
	}
	return nil
}

// Close releases the SQLite connection and exclusive lifetime lock.
func (a *LocalNudgeAuthority) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	dbErr := a.db.Close()
	unlockErr := filelock.Unlock(a.lock)
	closeErr := a.lock.Close()
	return errors.Join(dbErr, unlockErr, closeErr)
}

// AuthorizeNudgeIngress authenticates the server-owned requester context and
// durably returns one immutable grant for an idempotent request identity.
func (a *LocalNudgeAuthority) AuthorizeNudgeIngress(ctx context.Context, request NudgeIngressAuthorizationRequest) (NudgeAuthorization, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return NudgeAuthorization{}, err
	}
	defer release()
	requester, ok := ctx.Value(authenticatedNudgeRequesterContextKey{}).(AuthenticatedNudgeRequester)
	if !ok || validateAuthenticatedNudgeRequester(requester) != nil || requester.TenantScope != a.opts.TenantScope ||
		requester.CityScope != a.opts.CityScope || requester.CredentialClass != a.opts.CredentialClass {
		return NudgeAuthorization{Disposition: NudgeAuthorizationDenied}, nil
	}
	if err := validateLocalNudgeIngressAuthorizationRequest(request); err != nil {
		return NudgeAuthorization{}, err
	}
	fingerprint := localNudgeAuthorizationFingerprint(request, requester)
	commandID := CommandIDForRequest(a.store, request.RequestID)
	if commandID == "" {
		return NudgeAuthorization{}, fmt.Errorf("%w: invalid deterministic command id", ErrLocalNudgeAuthorityConflict)
	}
	if existing, found, err := a.grantByRequestID(ctx, request.RequestID); err != nil {
		return NudgeAuthorization{}, err
	} else if found {
		if err := a.validatePersistedGrant(existing); err != nil {
			return NudgeAuthorization{}, err
		}
		if existing.fingerprint != fingerprint || existing.commandID != commandID {
			return NudgeAuthorization{}, fmt.Errorf("%w: request id is bound to different authenticated content", ErrLocalNudgeAuthorityConflict)
		}
		return NudgeAuthorization{Disposition: NudgeAuthorizationAllowed, PrincipalSchemaVersion: NudgePrincipalSchemaVersion, CommandCreatedAt: existing.commandCreatedAt, Reference: existing.reference}, nil
	}
	if err := validateNewLocalNudgeIngressAuthorizationRequest(request); err != nil {
		return NudgeAuthorization{}, err
	}
	referenceDigest := sha256.Sum256(append(append([]byte("gascity.local-nudge-authority.reference.v1\x00"), []byte(commandID)...), fingerprint[:]...))
	reference := TrustedIngressReference{
		Issuer: a.opts.Issuer, ReferenceID: "local-ref-" + hex.EncodeToString(referenceDigest[:]),
		PrincipalID: requester.PrincipalID, TenantScope: requester.TenantScope, CityScope: requester.CityScope,
		CredentialClass: requester.CredentialClass, PolicyVersion: a.opts.PolicyVersion,
		PolicyDecisionID: "local-decision-" + hex.EncodeToString(fingerprint[:]), Action: request.Action,
		TargetSessionID: request.Target.SessionID, PayloadDigest: request.PayloadDigest,
		IssuedAt: request.RequestedAt.UTC(), ExpiresAt: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
	}
	if err := insertLocalNudgeGrant(ctx, a.db, request.RequestID, fingerprint, commandID, request.RequestedAt.UTC(), reference); err != nil {
		return NudgeAuthorization{}, err
	}
	persisted, found, err := a.grantByRequestID(ctx, request.RequestID)
	if err != nil {
		return NudgeAuthorization{}, err
	}
	if !found || persisted.fingerprint != fingerprint || persisted.commandID != commandID {
		return NudgeAuthorization{}, fmt.Errorf("%w: concurrent request id is bound to different authenticated content", ErrLocalNudgeAuthorityConflict)
	}
	if err := a.validatePersistedGrant(persisted); err != nil {
		return NudgeAuthorization{}, err
	}
	return NudgeAuthorization{Disposition: NudgeAuthorizationAllowed, PrincipalSchemaVersion: NudgePrincipalSchemaVersion, CommandCreatedAt: persisted.commandCreatedAt, Reference: persisted.reference}, nil
}

func validateAuthenticatedNudgeRequester(requester AuthenticatedNudgeRequester) error {
	for _, field := range []struct{ name, value string }{
		{"requester principal", requester.PrincipalID},
		{"requester tenant", requester.TenantScope},
		{"requester city", requester.CityScope},
		{"requester credential class", requester.CredentialClass},
		{"requester evidence id", requester.EvidenceID},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

func validateLocalNudgeIngressAuthorizationRequest(request NudgeIngressAuthorizationRequest) error {
	if err := validateCommandIdentity("request id", request.RequestID); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if request.Action != NudgeCommandAction {
		return fmt.Errorf("%w: unsupported action %q", ErrLocalNudgeAuthorityConflict, request.Action)
	}
	if !knownDeliveryMode(request.Mode) {
		return fmt.Errorf("%w: unsupported delivery mode %q", ErrLocalNudgeAuthorityConflict, request.Mode)
	}
	if err := validateCommandTarget(request.Mode, request.Target); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	for _, digest := range []struct{ name, value string }{
		{"intent digest", request.IntentDigest}, {"payload digest", request.PayloadDigest},
	} {
		if len(digest.value) != sha256.Size*2 {
			return fmt.Errorf("%w: %s is not canonical SHA-256", ErrLocalNudgeAuthorityConflict, digest.name)
		}
		if _, err := hex.DecodeString(digest.value); err != nil || strings.ToLower(digest.value) != digest.value {
			return fmt.Errorf("%w: %s is not canonical SHA-256", ErrLocalNudgeAuthorityConflict, digest.name)
		}
	}
	if err := validateCommandTime("requested at", request.RequestedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if request.DeliverAtCreation {
		if !request.DeliverAfter.IsZero() {
			return fmt.Errorf("%w: deliver-at-creation request carries an absolute delivery time", ErrLocalNudgeAuthorityConflict)
		}
	} else if err := validateCommandTime("deliver after", request.DeliverAfter); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if err := validateCommandTime("expires at", request.ExpiresAt); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if !request.DeliverAtCreation && !request.ExpiresAt.After(request.DeliverAfter) {
		return fmt.Errorf("%w: expiry is not after absolute delivery time", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func validateNewLocalNudgeIngressAuthorizationRequest(request NudgeIngressAuthorizationRequest) error {
	deliverAfter := request.DeliverAfter
	if request.DeliverAtCreation {
		deliverAfter = request.RequestedAt
	}
	if deliverAfter.Before(request.RequestedAt) {
		return fmt.Errorf("%w: new command delivery time precedes authority-selected creation", ErrNudgeAuthorizationInvalid)
	}
	if request.Mode == DeliveryModeImmediate && !deliverAfter.Equal(request.RequestedAt) {
		return fmt.Errorf("%w: new immediate command is not deliverable at creation", ErrNudgeAuthorizationInvalid)
	}
	if !request.ExpiresAt.After(deliverAfter) {
		return fmt.Errorf("%w: new command expiry is not after delivery", ErrNudgeAuthorizationInvalid)
	}
	return nil
}

func localNudgeAuthorizationFingerprint(request NudgeIngressAuthorizationRequest, requester AuthenticatedNudgeRequester) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "gascity.local-nudge-authority.request.v1")
	for _, value := range []string{
		request.RequestID, request.Action, request.Target.SessionID, request.Target.ContinuationIdentity,
		request.Target.LaunchIdentity, string(request.Target.Policy), request.IntentDigest,
		requester.PrincipalID, requester.TenantScope, requester.CityScope, requester.CredentialClass,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = digest.Write(length[:])
		_, _ = io.WriteString(digest, value)
	}
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], request.Target.IntentGeneration)
	_, _ = digest.Write(generation[:])
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

type localNudgeGrant struct {
	fingerprint      [sha256.Size]byte
	commandID        string
	commandCreatedAt time.Time
	reference        TrustedIngressReference
}

func (a *LocalNudgeAuthority) grantByRequestID(ctx context.Context, requestID string) (localNudgeGrant, bool, error) {
	return scanLocalNudgeGrant(a.db.QueryRowContext(ctx, `SELECT reference_id, request_fingerprint, command_id, principal_schema, issuer,
		principal_id, tenant_scope, city_scope, credential_class, policy_version, policy_decision_id,
		action, target_session_id, payload_digest, command_created_at, issued_at, expires_at FROM ingress_grants WHERE request_id = ?`, requestID))
}

func (a *LocalNudgeAuthority) grantByReferenceID(ctx context.Context, referenceID string) (localNudgeGrant, bool, error) {
	return scanLocalNudgeGrant(a.db.QueryRowContext(ctx, `SELECT reference_id, request_fingerprint, command_id, principal_schema, issuer,
		principal_id, tenant_scope, city_scope, credential_class, policy_version, policy_decision_id,
		action, target_session_id, payload_digest, command_created_at, issued_at, expires_at FROM ingress_grants WHERE reference_id = ?`, referenceID))
}

type localNudgeRowScanner interface{ Scan(...any) error }

func scanLocalNudgeGrant(row localNudgeRowScanner) (localNudgeGrant, bool, error) {
	var (
		fingerprint              []byte
		grant                    localNudgeGrant
		principalSchema          int
		created, issued, expires string
	)
	err := row.Scan(&grant.reference.ReferenceID, &fingerprint, &grant.commandID, &principalSchema, &grant.reference.Issuer,
		&grant.reference.PrincipalID, &grant.reference.TenantScope, &grant.reference.CityScope, &grant.reference.CredentialClass,
		&grant.reference.PolicyVersion, &grant.reference.PolicyDecisionID, &grant.reference.Action, &grant.reference.TargetSessionID,
		&grant.reference.PayloadDigest, &created, &issued, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return localNudgeGrant{}, false, nil
	}
	if err != nil {
		return localNudgeGrant{}, false, fmt.Errorf("reading local nudge grant: %w", err)
	}
	if len(fingerprint) != sha256.Size || principalSchema != int(NudgePrincipalSchemaVersion) {
		return localNudgeGrant{}, false, fmt.Errorf("%w: malformed local nudge grant", ErrLocalNudgeAuthorityConflict)
	}
	copy(grant.fingerprint[:], fingerprint)
	commandCreatedAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return localNudgeGrant{}, false, fmt.Errorf("%w: malformed grant command_created_at", ErrLocalNudgeAuthorityConflict)
	}
	issuedAt, err := time.Parse(time.RFC3339Nano, issued)
	if err != nil {
		return localNudgeGrant{}, false, fmt.Errorf("%w: malformed grant issued_at", ErrLocalNudgeAuthorityConflict)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return localNudgeGrant{}, false, fmt.Errorf("%w: malformed grant expires_at", ErrLocalNudgeAuthorityConflict)
	}
	grant.commandCreatedAt = commandCreatedAt.UTC()
	grant.reference.IssuedAt = issuedAt.UTC()
	grant.reference.ExpiresAt = expiresAt.UTC()
	return grant, true, nil
}

func (a *LocalNudgeAuthority) validatePersistedGrant(grant localNudgeGrant) error {
	reference := grant.reference
	if grant.commandID == "" || grant.fingerprint == ([sha256.Size]byte{}) ||
		reference.Issuer != a.opts.Issuer || reference.TenantScope != a.opts.TenantScope ||
		reference.CityScope != a.opts.CityScope || reference.CredentialClass != a.opts.CredentialClass ||
		reference.PolicyVersion != a.opts.PolicyVersion || reference.Action != NudgeCommandAction {
		return fmt.Errorf("%w: persisted ingress grant differs from authority policy", ErrLocalNudgeAuthorityConflict)
	}
	for _, field := range []struct{ name, value string }{
		{"grant command id", grant.commandID},
		{"grant reference id", reference.ReferenceID},
		{"grant principal id", reference.PrincipalID},
		{"grant policy decision id", reference.PolicyDecisionID},
		{"grant target session id", reference.TargetSessionID},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
		}
	}
	if len(reference.PayloadDigest) != sha256.Size*2 || strings.ToLower(reference.PayloadDigest) != reference.PayloadDigest {
		return fmt.Errorf("%w: persisted ingress payload digest is not canonical", ErrLocalNudgeAuthorityConflict)
	}
	if _, err := hex.DecodeString(reference.PayloadDigest); err != nil {
		return fmt.Errorf("%w: persisted ingress payload digest is not canonical", ErrLocalNudgeAuthorityConflict)
	}
	if err := validateCommandTime("grant issued at", reference.IssuedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if err := validateCommandTime("grant command created at", grant.commandCreatedAt); err != nil || grant.commandCreatedAt.Before(reference.IssuedAt) {
		return fmt.Errorf("%w: persisted ingress command creation time is invalid", ErrLocalNudgeAuthorityConflict)
	}
	if err := validateCommandTime("grant expires at", reference.ExpiresAt); err != nil || !reference.ExpiresAt.After(reference.IssuedAt) {
		return fmt.Errorf("%w: persisted ingress expiry is invalid", ErrLocalNudgeAuthorityConflict)
	}
	referenceDigest := sha256.Sum256(append(append([]byte("gascity.local-nudge-authority.reference.v1\x00"), []byte(grant.commandID)...), grant.fingerprint[:]...))
	if reference.ReferenceID != "local-ref-"+hex.EncodeToString(referenceDigest[:]) ||
		reference.PolicyDecisionID != "local-decision-"+hex.EncodeToString(grant.fingerprint[:]) {
		return fmt.Errorf("%w: persisted ingress grant identity is inconsistent", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func insertLocalNudgeGrant(ctx context.Context, db *sql.DB, requestID string, fingerprint [sha256.Size]byte, commandID string, commandCreatedAt time.Time, reference TrustedIngressReference) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO ingress_grants (
		reference_id, request_id, request_fingerprint, command_id, principal_schema, issuer, principal_id,
		tenant_scope, city_scope, credential_class, policy_version, policy_decision_id, action,
		target_session_id, payload_digest, command_created_at, issued_at, expires_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reference.ReferenceID, requestID, fingerprint[:], commandID, NudgePrincipalSchemaVersion, reference.Issuer, reference.PrincipalID,
		reference.TenantScope, reference.CityScope, reference.CredentialClass, reference.PolicyVersion, reference.PolicyDecisionID,
		reference.Action, reference.TargetSessionID, reference.PayloadDigest, commandCreatedAt.Format(time.RFC3339Nano),
		reference.IssuedAt.Format(time.RFC3339Nano), reference.ExpiresAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("%w: inserting local nudge grant: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	return nil
}

// ResolveTrustedNudgeIngress resolves an exact immutable reference from the
// independent journal. Missing or substituted references are denied.
func (a *LocalNudgeAuthority) ResolveTrustedNudgeIngress(ctx context.Context, reference TrustedIngressReference) (NudgeAuthorization, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return NudgeAuthorization{}, err
	}
	defer release()
	grant, found, err := a.grantByReferenceID(ctx, reference.ReferenceID)
	if err != nil {
		return NudgeAuthorization{}, err
	}
	if !found {
		return NudgeAuthorization{Disposition: NudgeAuthorizationDenied}, nil
	}
	if err := a.validatePersistedGrant(grant); err != nil {
		return NudgeAuthorization{}, err
	}
	if grant.reference != reference {
		return NudgeAuthorization{Disposition: NudgeAuthorizationDenied}, nil
	}
	return NudgeAuthorization{Disposition: NudgeAuthorizationAllowed, PrincipalSchemaVersion: NudgePrincipalSchemaVersion, CommandCreatedAt: grant.commandCreatedAt, Reference: grant.reference}, nil
}

// AuthorizeNudgeClaim revalidates the immutable ingress reference against the
// journal and emits one deterministic current-policy decision.
func (a *LocalNudgeAuthority) AuthorizeNudgeClaim(ctx context.Context, request NudgeClaimAuthorizationRequest) (NudgeClaimAuthorization, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return NudgeClaimAuthorization{}, err
	}
	defer release()
	grant, found, err := a.grantByReferenceID(ctx, request.Command.TrustedIngress.ReferenceID)
	if err != nil {
		return NudgeClaimAuthorization{}, err
	}
	if found {
		if err := a.validatePersistedGrant(grant); err != nil {
			return NudgeClaimAuthorization{}, err
		}
	}
	disposition := NudgeAuthorizationAllowed
	if !found || grant.commandID != request.Command.ID || request.Command.Store != a.store ||
		grant.reference != request.Command.TrustedIngress || request.Partition != trustedCityPartitionFromAuthority(request.Command.TrustedIngress) ||
		request.Command.TrustedIngress.Action != NudgeCommandAction || request.Command.TrustedIngress.TargetSessionID != request.Command.Target.SessionID ||
		request.Command.TrustedIngress.PayloadDigest != ComputeCommandPayloadDigest(request.Command) {
		disposition = NudgeAuthorizationDenied
	}
	digest := localNudgeClaimDecisionDigest(request, a.opts.PolicyVersion)
	return NudgeClaimAuthorization{
		Disposition: disposition, PrincipalSchemaVersion: NudgePrincipalSchemaVersion,
		DecisionID: "local-claim-" + hex.EncodeToString(digest[:]), PolicyVersion: a.opts.PolicyVersion,
		Reference: request.Command.TrustedIngress,
	}, nil
}

func localNudgeClaimDecisionDigest(request NudgeClaimAuthorizationRequest, policyVersion string) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "gascity.local-nudge-authority.claim.v1")
	for _, value := range []string{
		request.Command.ID, request.Command.Store.StoreUUID, request.Command.TrustedIngress.ReferenceID,
		request.ClaimID, request.OwnerID, request.AttemptID, request.BoundLaunchIdentity,
		request.ClaimedAt.UTC().Format(time.RFC3339Nano), request.LeaseUntil.UTC().Format(time.RFC3339Nano), policyVersion,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = digest.Write(length[:])
		_, _ = io.WriteString(digest, value)
	}
	var epoch [8]byte
	binary.BigEndian.PutUint64(epoch[:], request.Command.Store.RestoreEpoch)
	_, _ = digest.Write(epoch[:])
	_, _ = digest.Write(request.Partition.identity[:])
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

var _ NudgeClaimAuthorizer = (*LocalNudgeAuthority)(nil)
