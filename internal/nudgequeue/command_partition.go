package nudgequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	commandPartitionSchemaVersion = "1"
	commandPartitionRoutePrefix   = "gc:control-partition:v1:"
)

// ErrCommandRepositoryPartition reports that a read could not prove which
// trusted city partition owns a command before exposing it to an index.
var ErrCommandRepositoryPartition = errors.New("durable nudge command city partition is unverified")

// ErrCommandPartitionTerminalIntent reports missing, conflicting, or
// unavailable authority-owned intent for a terminal store transition. A
// terminal command row alone is never sufficient to publish membership.
var ErrCommandPartitionTerminalIntent = errors.New("durable nudge command terminal intent is unverified")

// TrustedCityPartition is an opaque capability produced only by trusted
// ingress authority. Its identity has no exported field or constructor, so a
// command's caller-authored city scope cannot create one.
type TrustedCityPartition struct {
	identity [sha256.Size]byte
}

func (p TrustedCityPartition) valid() bool {
	return p.identity != [sha256.Size]byte{}
}

func commandPartitionRoute(partition TrustedCityPartition) string {
	if !partition.valid() {
		return ""
	}
	return commandPartitionRoutePrefix + hex.EncodeToString(partition.identity[:])
}

// TrustedCityPartitionResolver revalidates one untrusted ingress reference
// against independent authority and returns its opaque city partition. The
// reference, including CityScope, is lookup input only and is never authority.
type TrustedCityPartitionResolver interface {
	ResolveCommandPartition(context.Context, TrustedIngressReference) (TrustedCityPartition, error)
}

// CommandPartitionCoverageRequest binds an authority proof to one exact
// transaction-consistent repository snapshot without copying global terminal
// history into the partition-local read path.
type CommandPartitionCoverageRequest struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	SequenceHighWater  uint64
	Partition          TrustedCityPartition
}

// CommandPartitionCoverageEntry is one authority-admitted command that was
// active in the requested partition at the requested repository revision.
type CommandPartitionCoverageEntry struct {
	CommandID string
	Sequence  uint64
}

// CommandPartitionCoverage is the independently retained, complete active set
// for one partition. Every snapshot identity field must exactly echo the
// request. AdmittedCount proves the authority journal is dense through the
// repository sequence high-water; ActiveEntries is the complete partition set
// ordered by sequence.
type CommandPartitionCoverage struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	SequenceHighWater  uint64
	AdmittedCount      uint64
	Partition          TrustedCityPartition
	ActiveEntries      []CommandPartitionCoverageEntry
}

// CommandPartitionMembershipRequest binds one exact command lookup to the
// transaction-consistent repository revision returned by that lookup.
type CommandPartitionMembershipRequest struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Partition          TrustedCityPartition
}

// CommandPartitionMembership reports whether authority admitted the exact
// command to this partition and whether it was active at the requested
// historical revision.
type CommandPartitionMembership struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Partition          TrustedCityPartition
	Found              bool
	Active             bool
	Sequence           uint64
}

// TrustedCommandPartitionCoverageResolver proves that an indexed partition
// query returned every authority-admitted active command. Implementations must
// retain historical membership so a concurrent newer publication cannot make
// an older repository snapshot unverifiable.
type TrustedCommandPartitionCoverageResolver interface {
	ResolveCommandPartitionCoverage(context.Context, CommandPartitionCoverageRequest) (CommandPartitionCoverage, error)
	ResolveCommandPartitionMembership(context.Context, CommandPartitionMembershipRequest) (CommandPartitionMembership, error)
}

// CommandPartitionAdmission is the authority-side membership fact published
// after a durable create. RepositoryRevision is the command's admission
// revision, not a later claim or completion revision.
type CommandPartitionAdmission struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Sequence           uint64
	Partition          TrustedCityPartition
}

// CommandPartitionTerminal is the authority-side membership fact published
// after a durable terminal transition. Replays of the exact fact are
// idempotent; conflicting facts must fail closed.
type CommandPartitionTerminal struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Sequence           uint64
	Partition          TrustedCityPartition
}

// CommandPartitionTerminalIntent is the authority-owned write-ahead proof for
// one exact terminal command transition. CommandDigest is SHA-256 over the
// canonical command wire including the resulting repository revision. The
// intent must be durable before the command-store transaction may commit.
type CommandPartitionTerminalIntent struct {
	Store                    CommandStoreBinding
	RepositoryBeforeRevision uint64
	RepositoryRevision       uint64
	CommandID                string
	Sequence                 uint64
	Partition                TrustedCityPartition
	BeforeCommandDigest      [sha256.Size]byte
	CommandDigest            [sha256.Size]byte
}

// CommandPartitionTerminalResolution is the exact after-state evidence used
// to recover a prepared or already-finalized transition after response loss or
// restart. It intentionally cannot authorize preparation or abort.
type CommandPartitionTerminalResolution struct {
	Store              CommandStoreBinding
	RepositoryRevision uint64
	CommandID          string
	Sequence           uint64
	Partition          TrustedCityPartition
	CommandDigest      [sha256.Size]byte
}

// TrustedCommandPartitionTerminalIntentAuthority durably prepares and later
// verifies exact terminal-transition intent independently of the command
// store. Prepare is idempotent for one exact intent and must reject a competing
// unresolved intent for the same command. Abort removes only the exact intent
// after the caller has proved the store transaction rolled back. Verify accepts
// only an exact prepared or exact already-finalized digest and never derives
// authority from a terminal row.
type TrustedCommandPartitionTerminalIntentAuthority interface {
	PrepareCommandPartitionTerminal(context.Context, CommandPartitionTerminalIntent) error
	AbortCommandPartitionTerminal(context.Context, CommandPartitionTerminalIntent) error
	VerifyCommandPartitionTerminal(context.Context, CommandPartitionTerminalResolution) error
}

// CommandPartitionTerminalRecoveryReader is the narrow unpartitioned store
// view used only to resolve authority-owned preparations before a production
// partition reader or writer becomes reachable.
type CommandPartitionTerminalRecoveryReader interface {
	State(context.Context) (CommandRepositoryState, error)
	Get(context.Context, string) (CommandIndexResolution, error)
}

// TrustedCommandPartitionTerminalRecovery resolves every outstanding terminal
// preparation against exact store bytes during startup. Implementations may
// finalize an exact prepared after-state or abort an exact before-state only at
// its unchanged repository revision; all other states must fail closed.
type TrustedCommandPartitionTerminalRecovery interface {
	RepairCommandPartitionTerminals(context.Context, CommandPartitionTerminalRecoveryReader) error
}

func terminalResolutionForCommand(command Command, partition TrustedCityPartition) (CommandPartitionTerminalResolution, error) {
	if !partition.valid() || command.Terminal == nil || !commandIsTerminalState(command.State) ||
		validateCommandRepositoryBinding(command.Store) != nil || command.ID == "" ||
		command.Order.Sequence == 0 || command.Order.Revision == 0 {
		return CommandPartitionTerminalResolution{}, fmt.Errorf("%w: terminal command identity is incomplete", ErrCommandPartitionTerminalIntent)
	}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return CommandPartitionTerminalResolution{}, fmt.Errorf("%w: encoding exact terminal command: %w", ErrCommandPartitionTerminalIntent, err)
	}
	return CommandPartitionTerminalResolution{
		Store:              command.Store,
		RepositoryRevision: command.Order.Revision,
		CommandID:          command.ID,
		Sequence:           command.Order.Sequence,
		Partition:          partition,
		CommandDigest:      sha256.Sum256(wire),
	}, nil
}

func terminalIntentForTransition(repositoryBeforeRevision uint64, before, after Command, partition TrustedCityPartition) (CommandPartitionTerminalIntent, error) {
	resolution, err := terminalResolutionForCommand(after, partition)
	if err != nil {
		return CommandPartitionTerminalIntent{}, err
	}
	if before.Store != after.Store || before.ID != after.ID || before.Order.Sequence != after.Order.Sequence ||
		repositoryBeforeRevision == 0 || repositoryBeforeRevision == ^uint64(0) || before.Order.Revision == 0 ||
		before.Order.Revision > repositoryBeforeRevision || after.Order.Revision != repositoryBeforeRevision+1 ||
		before.Terminal != nil || commandIsTerminalState(before.State) {
		return CommandPartitionTerminalIntent{}, fmt.Errorf("%w: terminal before-state is inconsistent", ErrCommandPartitionTerminalIntent)
	}
	wire, err := EncodeCommandV1(before)
	if err != nil {
		return CommandPartitionTerminalIntent{}, fmt.Errorf("%w: encoding exact pre-terminal command: %w", ErrCommandPartitionTerminalIntent, err)
	}
	return CommandPartitionTerminalIntent{
		Store:                    resolution.Store,
		RepositoryBeforeRevision: repositoryBeforeRevision,
		RepositoryRevision:       resolution.RepositoryRevision,
		CommandID:                resolution.CommandID,
		Sequence:                 resolution.Sequence,
		Partition:                resolution.Partition,
		BeforeCommandDigest:      sha256.Sum256(wire),
		CommandDigest:            resolution.CommandDigest,
	}, nil
}

func prepareCommandPartitionTerminal(ctx context.Context, authority TrustedCommandPartitionTerminalIntentAuthority, repositoryBeforeRevision uint64, before, after Command, partition TrustedCityPartition) (CommandPartitionTerminalIntent, error) {
	if isNilRepositoryDependency(authority) {
		return CommandPartitionTerminalIntent{}, fmt.Errorf("%w: terminal intent authority is required", ErrCommandPartitionTerminalIntent)
	}
	intent, err := terminalIntentForTransition(repositoryBeforeRevision, before, after, partition)
	if err != nil {
		return CommandPartitionTerminalIntent{}, err
	}
	if err := authority.PrepareCommandPartitionTerminal(ctx, intent); err != nil {
		return CommandPartitionTerminalIntent{}, fmt.Errorf("%w: preparing exact terminal transition: %w", ErrCommandPartitionTerminalIntent, err)
	}
	return intent, nil
}

func verifyCommandPartitionTerminal(ctx context.Context, authority TrustedCommandPartitionTerminalIntentAuthority, command Command, partition TrustedCityPartition) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: terminal intent authority is required", ErrCommandPartitionTerminalIntent)
	}
	resolution, err := terminalResolutionForCommand(command, partition)
	if err != nil {
		return err
	}
	if err := authority.VerifyCommandPartitionTerminal(ctx, resolution); err != nil {
		return fmt.Errorf("%w: verifying exact terminal transition: %w", ErrCommandPartitionTerminalIntent, err)
	}
	return nil
}

func abortCommandPartitionTerminal(ctx context.Context, authority TrustedCommandPartitionTerminalIntentAuthority, intent CommandPartitionTerminalIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: terminal intent authority is required", ErrCommandPartitionTerminalIntent)
	}
	if err := authority.AbortCommandPartitionTerminal(ctx, intent); err != nil {
		return fmt.Errorf("%w: aborting rolled-back terminal transition: %w", ErrCommandPartitionTerminalIntent, err)
	}
	return nil
}

// TrustedCommandPartitionMembershipRecorder owns the independent membership
// history used by TrustedCommandPartitionCoverageResolver.
type TrustedCommandPartitionMembershipRecorder interface {
	RecordCommandPartitionAdmission(context.Context, CommandPartitionAdmission) error
	RecordCommandPartitionTerminal(context.Context, CommandPartitionTerminal) error
}

// CommandRepositoryPartitionError identifies the exact read boundary that
// could not prove trusted city ownership. It never copies command content.
type CommandRepositoryPartitionError struct {
	Operation string
	CommandID string
	Err       error
}

// Error describes the fail-closed partition refusal.
func (e *CommandRepositoryPartitionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.CommandID == "" {
		return fmt.Sprintf("%s during %s: %v", ErrCommandRepositoryPartition, e.Operation, e.Err)
	}
	return fmt.Sprintf("%s during %s for command %q: %v", ErrCommandRepositoryPartition, e.Operation, e.CommandID, e.Err)
}

// Unwrap preserves the partition classification and resolver cause.
func (e *CommandRepositoryPartitionError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrCommandRepositoryPartition}
	}
	return []error{ErrCommandRepositoryPartition, e.Err}
}

// CommandPartitionReader is the only read surface that may feed a city-local
// command index. It contains no repository writer or provider capability.
type CommandPartitionReader struct {
	repository commandRepositoryReader
	partition  TrustedCityPartition
	resolver   TrustedCityPartitionResolver
	coverage   TrustedCommandPartitionCoverageResolver
}

// NewCommandPartitionReader binds repository reads to one opaque trusted city
// capability and an independent authority resolver. Missing or typed-nil
// authority fails closed.
func NewCommandPartitionReader(repository *CommandRepository, partition TrustedCityPartition, resolver TrustedCityPartitionResolver) (*CommandPartitionReader, error) {
	if repository == nil {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("command repository is required")}
	}
	if !partition.valid() {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("trusted city partition capability is required")}
	}
	if isNilRepositoryDependency(resolver) {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("trusted city partition resolver is required")}
	}
	coverage, ok := resolver.(TrustedCommandPartitionCoverageResolver)
	if !ok || isNilRepositoryDependency(coverage) {
		return nil, &CommandRepositoryPartitionError{Operation: "construction", Err: errors.New("trusted partition coverage resolver is required")}
	}
	return &CommandPartitionReader{repository: repository.reader, partition: partition, resolver: resolver, coverage: coverage}, nil
}

// Snapshot returns only commands independently resolved to this reader's
// trusted city. The exact partition route is pushed into the backing indexed
// query; foreign commands are represented by content-free sequence ranges so
// the global watermark remains dense without materializing foreign rows.
func (r *CommandPartitionReader) Snapshot(ctx context.Context, maxCommands int) (CommandIndexSnapshot, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) || isNilRepositoryDependency(r.coverage) {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot", Err: errors.New("partition reader is not fully bound")}
	}
	partitionRoute := commandPartitionRoute(r.partition)
	snapshot, err := r.repository.snapshotPartition(ctx, maxCommands, partitionRoute, func(ctx context.Context, row beads.Bead, entry CommandIndexEntry) error {
		partition, err := r.resolveProjectedPartition(ctx, "snapshot", row, entry)
		if err != nil {
			return err
		}
		if partition != r.partition {
			return &CommandRepositoryPartitionError{
				Operation: "snapshot",
				CommandID: row.ID,
				Err:       errors.New("indexed partition projection resolves to a different trusted city"),
			}
		}
		return nil
	})
	if err != nil {
		return CommandIndexSnapshot{}, err
	}
	if err := r.verifySnapshotCoverage(ctx, snapshot, maxCommands); err != nil {
		return CommandIndexSnapshot{}, err
	}
	snapshot, err = sealCommandIndexPartitionSnapshot(snapshot)
	if err != nil {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot coverage", Err: err}
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		return CommandIndexSnapshot{}, &CommandRepositoryPartitionError{Operation: "snapshot coverage", Err: err}
	}
	return snapshot, nil
}

func (r *CommandPartitionReader) verifySnapshotCoverage(ctx context.Context, snapshot CommandIndexSnapshot, maxCommands int) error {
	request := commandPartitionCoverageRequest(snapshot, r.partition)
	coverage, err := r.coverage.ResolveCommandPartitionCoverage(ctx, request)
	if err != nil {
		return &CommandRepositoryPartitionError{Operation: "snapshot coverage", Err: err}
	}
	if err := validateCommandPartitionCoverage(request, coverage, maxCommands); err != nil {
		return &CommandRepositoryPartitionError{Operation: "snapshot coverage", Err: err}
	}
	actual := make([]CommandPartitionCoverageEntry, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		routing := commandIndexEntryRouting(entry)
		actual = append(actual, CommandPartitionCoverageEntry{CommandID: routing.CommandID, Sequence: routing.Sequence})
	}
	if !commandPartitionCoverageEntriesEqual(actual, coverage.ActiveEntries) {
		return &CommandRepositoryPartitionError{
			Operation: "snapshot coverage",
			Err:       fmt.Errorf("indexed active set differs from trusted coverage: returned %d commands, authority requires %d", len(actual), len(coverage.ActiveEntries)),
		}
	}
	return nil
}

func commandPartitionCoverageEntriesEqual(left, right []CommandPartitionCoverageEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (r *CommandPartitionReader) resolveProjectedPartition(ctx context.Context, operation string, row beads.Bead, entry CommandIndexEntry) (TrustedCityPartition, error) {
	projectedRoute, err := commandPartitionProjection(row)
	if err != nil {
		return TrustedCityPartition{}, &CommandRepositoryPartitionError{Operation: operation, CommandID: row.ID, Err: err}
	}
	partition, routing, err := r.resolveEntryPartition(ctx, operation, entry)
	if err != nil {
		return TrustedCityPartition{}, err
	}
	wantRoute := commandPartitionRoute(partition)
	if projectedRoute != wantRoute {
		return TrustedCityPartition{}, &CommandRepositoryPartitionError{
			Operation: operation,
			CommandID: routing.CommandID,
			Err:       fmt.Errorf("stored partition projection %q differs from trusted authority", projectedRoute),
		}
	}
	return partition, nil
}

func commandPartitionProjection(row beads.Bead) (string, error) {
	route := row.Assignee
	if route == "" || row.Metadata[commandRecordPartitionKeyMetadataKey] != route {
		return "", errors.New("command partition projection is missing or inconsistent")
	}
	if row.Metadata[commandRecordPartitionSchemaMetadataKey] != commandPartitionSchemaVersion {
		return "", fmt.Errorf("command partition projection schema %q is unsupported", row.Metadata[commandRecordPartitionSchemaMetadataKey])
	}
	hexIdentity := strings.TrimPrefix(route, commandPartitionRoutePrefix)
	decoded, err := hex.DecodeString(hexIdentity)
	if !strings.HasPrefix(route, commandPartitionRoutePrefix) || err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != hexIdentity {
		return "", fmt.Errorf("command partition projection %q is non-canonical", route)
	}
	return route, nil
}

func commandPartitionCoverageRequest(snapshot CommandIndexSnapshot, partition TrustedCityPartition) CommandPartitionCoverageRequest {
	return CommandPartitionCoverageRequest{
		Store:              snapshot.Store,
		RepositoryRevision: snapshot.Revision,
		SequenceHighWater:  snapshot.SequenceHighWater,
		Partition:          partition,
	}
}

func validateCommandPartitionCoverage(request CommandPartitionCoverageRequest, coverage CommandPartitionCoverage, maxCommands int) error {
	if coverage.Store != request.Store || coverage.RepositoryRevision != request.RepositoryRevision ||
		coverage.SequenceHighWater != request.SequenceHighWater || coverage.AdmittedCount != request.SequenceHighWater ||
		coverage.Partition != request.Partition {
		return errors.New("trusted coverage is not bound to the exact repository snapshot")
	}
	if len(coverage.ActiveEntries) > maxCommands {
		return fmt.Errorf("trusted partition contains more than %d active commands: %w", maxCommands, ErrCommandRepositorySnapshotLimit)
	}
	var previous uint64
	seenIDs := make(map[string]struct{}, len(coverage.ActiveEntries))
	for index, entry := range coverage.ActiveEntries {
		if validateCommandIdentity("trusted coverage command id", entry.CommandID) != nil || !strings.HasPrefix(entry.CommandID, commandIDPrefix) ||
			entry.Sequence == 0 || entry.Sequence > request.SequenceHighWater {
			return fmt.Errorf("trusted coverage entry %d is invalid", index)
		}
		if index > 0 && entry.Sequence <= previous {
			return errors.New("trusted coverage active entries are not strictly ordered by sequence")
		}
		if _, duplicate := seenIDs[entry.CommandID]; duplicate {
			return fmt.Errorf("trusted coverage repeats command %q", entry.CommandID)
		}
		seenIDs[entry.CommandID] = struct{}{}
		previous = entry.Sequence
	}
	return nil
}

// Get returns an exact command only when independent authority resolves it to
// this reader's partition. A valid foreign command is indistinguishable from a
// missing ID at this city-local boundary.
func (r *CommandPartitionReader) Get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	if r == nil || isNilRepositoryDependency(r.repository.store) || isNilRepositoryDependency(r.repository.verifier) || !r.partition.valid() || isNilRepositoryDependency(r.resolver) {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read", CommandID: commandID, Err: errors.New("partition reader is not fully bound")}
	}
	resolution, err := r.repository.getPartition(ctx, commandID, func(ctx context.Context, row beads.Bead, entry CommandIndexEntry) (bool, error) {
		partition, err := r.resolveProjectedPartition(ctx, "exact read", row, entry)
		if err != nil {
			return false, err
		}
		return partition == r.partition, nil
	})
	if err != nil {
		return CommandIndexResolution{}, err
	}
	trusted, err := r.coverage.ResolveCommandPartitionMembership(ctx, CommandPartitionMembershipRequest{
		Store: resolution.Store, RepositoryRevision: resolution.Revision,
		CommandID: commandID, Partition: r.partition,
	})
	if err != nil {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read membership", CommandID: commandID, Err: err}
	}
	if err := validateCommandPartitionMembership(resolution, commandID, r.partition, trusted); err != nil {
		return CommandIndexResolution{}, &CommandRepositoryPartitionError{Operation: "exact read membership", CommandID: commandID, Err: err}
	}
	return resolution, nil
}

func validateCommandPartitionMembership(resolution CommandIndexResolution, commandID string, partition TrustedCityPartition, trusted CommandPartitionMembership) error {
	if trusted.Store != resolution.Store || trusted.RepositoryRevision != resolution.Revision ||
		trusted.CommandID != commandID || trusted.Partition != partition {
		return errors.New("trusted membership is not bound to the exact command read")
	}
	if !trusted.Found {
		if trusted.Active || trusted.Sequence != 0 || resolution.Found {
			return errors.New("repository command exists without trusted partition membership")
		}
		return nil
	}
	if !resolution.Found || trusted.Sequence == 0 {
		return errors.New("authority-owned command is absent from the repository read")
	}
	routing := commandIndexEntryRouting(resolution.Entry)
	if routing.CommandID != commandID || routing.Sequence != trusted.Sequence {
		return errors.New("repository command identity or sequence differs from trusted membership")
	}
	if resolution.Entry.Command == nil {
		return errors.New("authority-owned command has no supported lifecycle state")
	}
	active := !commandIsTerminalState(resolution.Entry.Command.State)
	if active != trusted.Active {
		return errors.New("repository command lifecycle differs from trusted membership")
	}
	return nil
}

func (r *CommandPartitionReader) resolveEntryPartition(ctx context.Context, operation string, entry CommandIndexEntry) (TrustedCityPartition, CommandRoutingHeader, error) {
	routing := commandIndexEntryRouting(entry)
	if entry.Command == nil {
		return TrustedCityPartition{}, routing, &CommandRepositoryPartitionError{
			Operation: operation,
			CommandID: routing.CommandID,
			Err:       errors.New("command version does not expose a supported trusted-ingress reference"),
		}
	}
	partition, err := r.resolver.ResolveCommandPartition(ctx, entry.Command.TrustedIngress)
	if err != nil {
		return TrustedCityPartition{}, routing, &CommandRepositoryPartitionError{Operation: operation, CommandID: routing.CommandID, Err: err}
	}
	if !partition.valid() {
		return TrustedCityPartition{}, routing, &CommandRepositoryPartitionError{
			Operation: operation,
			CommandID: routing.CommandID,
			Err:       errors.New("resolver returned no trusted city partition"),
		}
	}
	return partition, routing, nil
}
