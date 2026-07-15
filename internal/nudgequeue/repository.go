package nudgequeue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/google/uuid"
)

const (
	// CommandRepositorySchemaVersion is the exact durable row and metadata
	// contract understood by this repository implementation.
	CommandRepositorySchemaVersion uint32 = 1
	// CommandRepositoryWriterVersion identifies the writer protocol that owns
	// dense command sequence and repository revision allocation.
	CommandRepositoryWriterVersion uint32 = 2
	// MaxCommandRepositorySnapshotCommands is the maximum exact active command
	// set returned by one reconstruction. Terminal lifetime is represented by a
	// bounded checkpoint and does not consume this limit.
	MaxCommandRepositorySnapshotCommands = beads.MaxAtomicReadWriteListLimit - 1

	commandRepositorySchemaVersionMetadataKey     = "gc.control.repository.schema_version"
	commandRepositoryWriterVersionMetadataKey     = "gc.control.repository.writer_version"
	commandRepositoryStoreUUIDMetadataKey         = "gc.control.repository.store_uuid"
	commandRepositoryRestoreEpochMetadataKey      = "gc.control.repository.restore_epoch"
	commandRepositoryRevisionMetadataKey          = "gc.control.repository.revision"
	commandRepositorySequenceHighWaterMetadataKey = "gc.control.repository.sequence_high_water"
	commandRepositoryPartitionSchemaMetadataKey   = "gc.control.repository.command_partition_schema_version"

	commandRecordBeadType                   = "task"
	commandRecordTitle                      = "durable control command"
	commandRecordKindMetadataKey            = beadmeta.ControlRecordKindMetadataKey
	commandRecordKindMetadataValue          = "command"
	commandRecordCommandKindMetadataKey     = beadmeta.ControlCommandKindMetadataKey
	commandRecordCommandKindMetadataValue   = "nudge"
	commandRecordPartitionKeyMetadataKey    = beadmeta.ControlCommandPartitionMetadataKey
	commandRecordPartitionSchemaMetadataKey = beadmeta.ControlPartitionSchemaMetadataKey
	commandRecordRequestIDMetadataKey       = beadmeta.ControlCommandRequestIDMetadataKey
	commandRecordWireMetadataKey            = beadmeta.ControlCommandWireMetadataKey

	commandRequestIDDomainV1 = "gascity.nudge-command.request-id.v1"
	commandIDPrefix          = "gc-nudge-"
)

var (
	// ErrCommandRepositoryUnsupported reports that the selected Beads store
	// cannot provide one history-row plus durable-metadata transaction.
	ErrCommandRepositoryUnsupported = errors.New("durable nudge command repository is unsupported")
	// ErrCommandRepositorySchemaSkew reports partial, malformed, newer, or
	// otherwise incompatible repository metadata.
	ErrCommandRepositorySchemaSkew = errors.New("durable nudge command repository schema skew")
	// ErrCommandRepositoryLineage reports a store binding/high-water mismatch or
	// refusal from the independently retained lineage verifier.
	ErrCommandRepositoryLineage = errors.New("durable nudge command repository lineage mismatch")
	// ErrCommandRepositorySnapshotLimit reports an invalid bound or a command
	// active set too large to return completely under the caller's bound.
	ErrCommandRepositorySnapshotLimit = errors.New("durable nudge command snapshot exceeds its bound")
	// ErrCommandRepositoryCheckpointRequired reports a terminal tail that must
	// be incorporated by the explicit checkpoint writer before bounded read-only
	// reconstruction can proceed.
	ErrCommandRepositoryCheckpointRequired = errors.New("durable nudge command checkpoint catch-up is required")
	// ErrCommandRepositoryRecord reports a row that violated the centralized
	// command bead metadata/wire contract.
	ErrCommandRepositoryRecord = errors.New("invalid durable nudge command record")
	// ErrCommandRepositoryIdempotencyConflict reports reuse of one deterministic
	// request identity for a different immutable command.
	ErrCommandRepositoryIdempotencyConflict = errors.New("durable nudge command idempotency conflict")
	// ErrCommandRepositoryInvalidRequest reports caller-owned authoritative
	// envelope fields or an invalid request identity.
	ErrCommandRepositoryInvalidRequest = errors.New("invalid durable nudge command create request")
)

// CommandRepositoryState is the complete transaction-consistent authority
// evidence required by the repository and independent restore anchor.
type CommandRepositoryState struct {
	Store             CommandStoreBinding
	SchemaVersion     uint32
	WriterVersion     uint32
	Revision          uint64
	SequenceHighWater uint64
}

// CommandRepositoryLineageVerifier checks independently retained store
// identity and monotonic high waters without mutating that evidence.
type CommandRepositoryLineageVerifier interface {
	VerifyCommandRepositoryLineage(context.Context, CommandRepositoryState) error
}

// CommandRepositoryLineageWriter owns every mutation of independently retained
// lineage evidence. Provision requires the one-shot capability produced by the
// exact all-absent repository initialization winner. Advance may only repair or
// advance an existing anchor within the same store UUID and restore epoch.
type CommandRepositoryLineageWriter interface {
	ProvisionCommandRepositoryLineage(context.Context, CommandRepositoryState, CommandRepositoryProvisioningEvidence) error
	AdvanceCommandRepositoryLineage(context.Context, CommandRepositoryState) error
}

// CommandRepositoryLineageController supplies both the read-only verifier and
// explicit writer capabilities required by a writable command repository.
type CommandRepositoryLineageController interface {
	CommandRepositoryLineageVerifier
	CommandRepositoryLineageWriter
}

// CommandRepositoryLineageVerifierFunc adapts one function for local tests or
// read-only callers. It deliberately cannot satisfy the lineage writer
// interface.
type CommandRepositoryLineageVerifierFunc func(context.Context, CommandRepositoryState) error

// VerifyCommandRepositoryLineage invokes f.
func (f CommandRepositoryLineageVerifierFunc) VerifyCommandRepositoryLineage(ctx context.Context, state CommandRepositoryState) error {
	if f == nil {
		return errors.New("nil command repository lineage verifier function")
	}
	return f(ctx, state)
}

// CommandRepositoryProvisioningEvidence is a one-shot, in-memory capability
// produced by the exact successful all-absent initialization transaction. It
// has no exported fields or serializer, so a restored database cannot replay
// provisioning authority on a later process.
type CommandRepositoryProvisioningEvidence struct {
	nonce [sha256.Size]byte
	store CommandStoreBinding
}

func (e CommandRepositoryProvisioningEvidence) validFor(state CommandRepositoryState) bool {
	return e.store == state.Store && e.nonce != [sha256.Size]byte{}
}

// CommandRepositoryUnsupportedError identifies the concrete store that lacked
// the required atomic capability.
type CommandRepositoryUnsupportedError struct {
	StoreType string
}

// Error describes the unsupported concrete store.
func (e *CommandRepositoryUnsupportedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: store %s", ErrCommandRepositoryUnsupported, e.StoreType)
}

// Is makes CommandRepositoryUnsupportedError errors.Is-compatible.
func (e *CommandRepositoryUnsupportedError) Is(target error) bool {
	return target == ErrCommandRepositoryUnsupported
}

// CommandRepositorySchemaSkewError identifies the incompatible metadata field
// without treating permissive command decoding as proof of store compatibility.
type CommandRepositorySchemaSkewError struct {
	Field string
	Found string
	Want  string
}

// Error describes the incompatible repository metadata.
func (e *CommandRepositorySchemaSkewError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s=%q, want %s", ErrCommandRepositorySchemaSkew, e.Field, e.Found, e.Want)
}

// Is makes CommandRepositorySchemaSkewError errors.Is-compatible.
func (e *CommandRepositorySchemaSkewError) Is(target error) bool {
	return target == ErrCommandRepositorySchemaSkew
}

// CommandRepositoryLineageError reports which operation failed together with
// the transaction-consistent database evidence that was refused.
type CommandRepositoryLineageError struct {
	Operation string
	State     CommandRepositoryState
	Err       error
}

// Error describes the failed lineage operation without inventing identity.
func (e *CommandRepositoryLineageError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s during %s for store %q epoch %d revision %d sequence %d: %v",
		ErrCommandRepositoryLineage,
		e.Operation,
		e.State.Store.StoreUUID,
		e.State.Store.RestoreEpoch,
		e.State.Revision,
		e.State.SequenceHighWater,
		e.Err,
	)
}

// Unwrap preserves both the typed lineage classification and verifier cause.
func (e *CommandRepositoryLineageError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrCommandRepositoryLineage}
	}
	return []error{ErrCommandRepositoryLineage, e.Err}
}

// CommandRepositoryRecordError identifies one poison or wrong-contract row.
type CommandRepositoryRecordError struct {
	CommandID string
	Err       error
}

// Error describes the invalid record without copying command content.
func (e *CommandRepositoryRecordError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s %q: %v", ErrCommandRepositoryRecord, e.CommandID, e.Err)
}

// Unwrap preserves the record classification and safe underlying cause.
func (e *CommandRepositoryRecordError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrCommandRepositoryRecord}
	}
	return []error{ErrCommandRepositoryRecord, e.Err}
}

// CommandRepository persists and reads one durable nudge command ledger. It
// owns no provider effects and never reads the legacy file queue.
type CommandRepository struct {
	reader   commandRepositoryReader
	writer   CommandRepositoryLineageWriter
	preparer beads.AtomicReadSnapshotPreparer
	random   io.Reader
}

// commandRepositoryReader contains no lineage writer capability. Keeping the
// complete State/Get/Snapshot implementations on this type makes durable
// lineage mutation unavailable to read paths by construction.
type commandRepositoryReader struct {
	store     beads.AtomicReadWriteStore
	snapshots beads.AtomicReadSnapshotStore
	verifier  CommandRepositoryLineageVerifier
}

// NewCommandRepository constructs a repository only for a store with the
// required atomic capability and an independent lineage controller. The
// controller is narrowed to separate verifier and writer capabilities inside
// the repository.
func NewCommandRepository(store beads.Store, controller CommandRepositoryLineageController) (*CommandRepository, error) {
	return newCommandRepositoryWithRandom(store, controller, rand.Reader)
}

func newCommandRepositoryWithRandom(store beads.Store, controller CommandRepositoryLineageController, random io.Reader) (*CommandRepository, error) {
	atomicStore, ok := beads.AtomicReadWriteFor(store)
	if !ok {
		storeType := "<nil>"
		if store != nil {
			storeType = reflect.TypeOf(store).String()
		}
		return nil, &CommandRepositoryUnsupportedError{StoreType: storeType}
	}
	snapshotStore, ok := beads.AtomicReadSnapshotFor(store)
	if !ok {
		storeType := "<nil>"
		if store != nil {
			storeType = reflect.TypeOf(store).String()
		}
		return nil, &CommandRepositoryUnsupportedError{StoreType: storeType}
	}
	if isNilRepositoryDependency(controller) {
		return nil, &CommandRepositoryLineageError{Operation: "construction", Err: errors.New("independent lineage controller is required")}
	}
	if random == nil {
		return nil, &CommandRepositoryLineageError{Operation: "construction", Err: errors.New("cryptographic random source is required")}
	}
	return &CommandRepository{
		reader: commandRepositoryReader{store: atomicStore, snapshots: snapshotStore, verifier: controller},
		writer: controller,
		preparer: func() beads.AtomicReadSnapshotPreparer {
			preparer, _ := snapshotStore.(beads.AtomicReadSnapshotPreparer)
			return preparer
		}(),
		random: random,
	}, nil
}

func isNilRepositoryDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// CommandIDForRequest derives the only valid command ID for one request in one
// store lineage. Invalid bindings or request identities return an empty ID.
func CommandIDForRequest(binding CommandStoreBinding, requestID string) string {
	if validateCommandRepositoryBinding(binding) != nil || validateCommandIdentity("request id", requestID) != nil {
		return ""
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, commandRequestIDDomainV1)
	_, _ = digest.Write([]byte{0})
	_, _ = io.WriteString(digest, binding.StoreUUID)
	_, _ = digest.Write([]byte{0})
	var epoch [8]byte
	binary.BigEndian.PutUint64(epoch[:], binding.RestoreEpoch)
	_, _ = digest.Write(epoch[:])
	_, _ = digest.Write([]byte{0})
	_, _ = io.WriteString(digest, requestID)
	return commandIDPrefix + hex.EncodeToString(digest.Sum(nil))
}

// State returns existing, independently verified repository authority without
// initializing metadata or mutating independent lineage evidence. Missing
// metadata or an anchor behind the database fails closed.
func (r *CommandRepository) State(ctx context.Context) (CommandRepositoryState, error) {
	return r.reader.state(ctx)
}

func (r commandRepositoryReader) state(ctx context.Context) (CommandRepositoryState, error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandRepositoryState{}, err
	}
	state, err := r.loadState(ctx, "gc: read durable nudge command repository")
	if err != nil {
		return CommandRepositoryState{}, err
	}
	if err := r.verify(ctx, "verify", state); err != nil {
		return CommandRepositoryState{}, err
	}
	return state, nil
}

// Get resolves one exact command ID from durable authority. A missing ID is a
// successful Found=false result at the returned repository watermark; a row
// with the same ID but the wrong type/metadata/wire is an error.
func (r *CommandRepository) Get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	return r.reader.get(ctx, commandID)
}

func (r commandRepositoryReader) get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	return r.getPartition(ctx, commandID, nil)
}

type commandRepositoryExactValidator func(context.Context, beads.Bead, CommandIndexEntry) (bool, error)

func (r commandRepositoryReader) getPartition(ctx context.Context, commandID string, validate commandRepositoryExactValidator) (CommandIndexResolution, error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandIndexResolution{}, err
	}
	if err := validateCommandIdentity("command id", commandID); err != nil {
		return CommandIndexResolution{}, fmt.Errorf("%w: %w", ErrCommandRepositoryInvalidRequest, err)
	}
	before, err := r.state(ctx)
	if err != nil {
		return CommandIndexResolution{}, err
	}
	var (
		state CommandRepositoryState
		entry CommandIndexEntry
		found bool
	)
	err = atomicCommandRepositoryRead(ctx, r.store, "gc: read durable nudge command", func(tx commandRepositoryReadTx) error {
		var err error
		state, err = readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(before, state); err != nil {
			return err
		}
		row, err := tx.GetIssue(commandID)
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting exact durable nudge command %q: %w", commandID, err)
		}
		entry, _, err = decodeCommandRecord(row, state)
		if err != nil {
			return err
		}
		include := true
		if validate != nil {
			include, err = validate(ctx, row, entry)
			if err != nil {
				return err
			}
		}
		found = include
		if !include {
			entry = CommandIndexEntry{}
		}
		return nil
	})
	if err != nil {
		return CommandIndexResolution{}, err
	}
	if err := r.verify(ctx, "exact read", state); err != nil {
		return CommandIndexResolution{}, err
	}
	return CommandIndexResolution{
		Store:    state.Store,
		Revision: state.Revision,
		Entry:    entry,
		Found:    found,
	}, nil
}

// Snapshot returns a complete transaction-consistent active command set plus
// compacted terminal coverage or a typed bound/catch-up error. It never
// truncates, skips poison, writes a checkpoint, or fabricates tombstones.
func (r *CommandRepository) Snapshot(ctx context.Context, maxCommands int) (CommandIndexSnapshot, error) {
	return r.reader.snapshot(ctx, maxCommands)
}

func (r commandRepositoryReader) snapshot(ctx context.Context, maxCommands int) (CommandIndexSnapshot, error) {
	return r.snapshotPartition(ctx, maxCommands, "", nil)
}

type commandRepositoryPartitionValidator func(context.Context, beads.Bead, CommandIndexEntry) error

func (r commandRepositoryReader) snapshotPartition(ctx context.Context, maxCommands int, partitionRoute string, validate commandRepositoryPartitionValidator) (CommandIndexSnapshot, error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandIndexSnapshot{}, err
	}
	if maxCommands <= 0 || maxCommands > MaxCommandRepositorySnapshotCommands {
		return CommandIndexSnapshot{}, fmt.Errorf("snapshot command bound %d is outside 1..%d: %w", maxCommands, MaxCommandRepositorySnapshotCommands, ErrCommandRepositorySnapshotLimit)
	}
	before, err := r.state(ctx)
	if err != nil {
		return CommandIndexSnapshot{}, err
	}
	var snapshot CommandIndexSnapshot
	err = r.snapshots.AtomicReadSnapshot(ctx, func(tx beads.AtomicReadSnapshotTx) error {
		state, err := readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(before, state); err != nil {
			return err
		}
		var (
			checkpoint commandRepositoryCheckpoint
			found      bool
		)
		if partitionRoute == "" {
			checkpoint, found, err = readCommandRepositoryCheckpointFromSnapshot(tx, state)
			if err != nil {
				return err
			}
			tailQuery := beads.AtomicReadSnapshotPageQuery{
				IDPrefix: commandIDPrefix,
				Status:   "closed",
				Order:    beads.AtomicReadSnapshotOrderUpdatedAtID,
				Limit:    1,
			}
			if found {
				tailQuery.After = beads.AtomicReadSnapshotCursor{UpdatedAt: checkpoint.TerminalCursor.UpdatedAt, ID: checkpoint.TerminalCursor.ID}
			}
			tail, err := tx.ListHistoryPage(tailQuery)
			if err != nil {
				return fmt.Errorf("checking durable nudge command terminal tail: %w", err)
			}
			if len(tail.Rows) != 0 {
				return fmt.Errorf("terminal command %q follows the published checkpoint: %w", tail.Rows[0].ID, ErrCommandRepositoryCheckpointRequired)
			}
		}

		entries := make([]CommandIndexEntry, 0, min(maxCommands, beads.MaxAtomicReadSnapshotPageSize))
		query := beads.AtomicReadSnapshotPageQuery{
			IDPrefix: commandIDPrefix,
			Status:   "open",
			Assignee: partitionRoute,
			Order:    beads.AtomicReadSnapshotOrderID,
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			remainingWithOverflowProbe := maxCommands - len(entries) + 1
			query.Limit = min(beads.MaxAtomicReadSnapshotPageSize, remainingWithOverflowProbe)
			page, err := tx.ListHistoryPage(query)
			if err != nil {
				return fmt.Errorf("listing active durable nudge command snapshot: %w", err)
			}
			for _, row := range page.Rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				entry, _, err := decodeCommandRecord(row, state)
				if err != nil {
					return err
				}
				if validate != nil {
					if err := validate(ctx, row, entry); err != nil {
						return err
					}
				}
				entries = append(entries, entry)
				if len(entries) > maxCommands {
					return fmt.Errorf("snapshot contains more than %d active commands: %w", maxCommands, ErrCommandRepositorySnapshotLimit)
				}
			}
			if page.Next == (beads.AtomicReadSnapshotCursor{}) {
				break
			}
			query.After = page.Next
		}
		var coverage *CommandIndexCompactedCoverage
		if found {
			coverage = &CommandIndexCompactedCoverage{
				PublishedRevision: checkpoint.PublishedRevision,
				Ranges:            append([]CommandIndexSequenceRange(nil), checkpoint.Ranges...),
				TerminalCount:     checkpoint.TerminalCount,
				TombstoneCount:    checkpoint.TombstoneCount,
				FingerprintSHA256: checkpoint.FingerprintSHA256,
			}
		}
		if partitionRoute != "" {
			sort.Slice(entries, func(i, j int) bool {
				return commandIndexEntryRouting(entries[i]).Sequence < commandIndexEntryRouting(entries[j]).Sequence
			})
		}
		snapshot = CommandIndexSnapshot{
			Store:             state.Store,
			Entries:           entries,
			Tombstones:        make([]CommandIndexTombstone, 0),
			Coverage:          coverage,
			Revision:          state.Revision,
			SequenceHighWater: state.SequenceHighWater,
		}
		if partitionRoute == "" {
			if _, err := BuildCommandIndex(snapshot); err != nil {
				return &CommandRepositoryRecordError{Err: err}
			}
		}
		return nil
	})
	if err != nil {
		return CommandIndexSnapshot{}, err
	}
	state := CommandRepositoryState{
		Store:             snapshot.Store,
		SchemaVersion:     CommandRepositorySchemaVersion,
		WriterVersion:     CommandRepositoryWriterVersion,
		Revision:          snapshot.Revision,
		SequenceHighWater: snapshot.SequenceHighWater,
	}
	if err := r.verify(ctx, "snapshot", state); err != nil {
		return CommandIndexSnapshot{}, err
	}
	return snapshot, nil
}

// Provision creates repository metadata only when the complete command ledger
// and metadata set are absent, then provisions the independent lineage anchor
// with one-shot in-memory evidence. It never treats an existing database as
// fresh authority.
func (r *CommandRepository) Provision(ctx context.Context) (CommandRepositoryState, error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandRepositoryState{}, err
	}
	if r.preparer != nil {
		if err := r.preparer.PrepareAtomicReadSnapshot(ctx); err != nil {
			return CommandRepositoryState{}, fmt.Errorf("preparing durable nudge command snapshot capability: %w", err)
		}
	}
	state, initializedHere, evidence, err := r.provisionState(ctx)
	if err != nil {
		return CommandRepositoryState{}, err
	}
	if initializedHere {
		if !evidence.validFor(state) {
			return CommandRepositoryState{}, &CommandRepositoryLineageError{Operation: "provision", State: state, Err: errors.New("invalid one-shot provisioning evidence")}
		}
		if err := r.writer.ProvisionCommandRepositoryLineage(ctx, state, evidence); err != nil {
			return CommandRepositoryState{}, &CommandRepositoryLineageError{Operation: "provision", State: state, Err: err}
		}
	}
	if err := r.reader.verify(ctx, "verify provisioned repository", state); err != nil {
		return CommandRepositoryState{}, err
	}
	return state, nil
}

// RepairLineage explicitly confirms or advances independent lineage evidence
// to the current database high-water within the same store UUID and restore
// epoch. Missing metadata or anchor, rewind, foreign identity, and epoch
// changes fail closed; explicit recovery-epoch changes are a separate flow.
func (r *CommandRepository) RepairLineage(ctx context.Context) (CommandRepositoryState, error) {
	return r.repairLineage(ctx, "repair lineage")
}

// create durably admits an authority-partitioned pending command or returns the
// existing exact command for an idempotent retry. The opaque partition can be
// minted only by trusted ingress; store binding, sequence, and revision are
// allocated with the projected route in one transaction.
func (r *CommandRepository) create(ctx context.Context, requestID string, command Command, partition TrustedCityPartition) (CommandIndexEntry, bool, error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandIndexEntry{}, false, err
	}
	if !partition.valid() {
		return CommandIndexEntry{}, false, fmt.Errorf("trusted city partition is required: %w", ErrCommandRepositoryInvalidRequest)
	}
	before, err := r.State(ctx)
	if err != nil {
		before, err = r.repairLineage(ctx, "pre-create lineage repair")
		if err != nil {
			return CommandIndexEntry{}, false, err
		}
	}
	if err := validateCommandCreateRequest(before.Store, requestID, command); err != nil {
		return CommandIndexEntry{}, false, err
	}

	var (
		result  CommandIndexEntry
		created bool
	)
	err = r.reader.store.AtomicReadWrite(ctx, "gc: create durable nudge command", func(tx beads.AtomicReadWriteTx) error {
		state, err := readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(before, state); err != nil {
			return err
		}
		expectedID := CommandIDForRequest(state.Store, requestID)
		if expectedID == "" || command.ID != expectedID {
			return fmt.Errorf("command id %q does not match current store/request binding: %w", command.ID, ErrCommandRepositoryInvalidRequest)
		}
		existingRow, err := tx.GetIssue(expectedID)
		if err == nil {
			entry, storedRequestID, err := decodeCommandRecord(existingRow, state)
			if err != nil {
				return err
			}
			if err := validateIdempotentCommandRetry(requestID, command, storedRequestID, entry); err != nil {
				return err
			}
			result = entry
			return nil
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("checking durable nudge request %q: %w", requestID, err)
		}
		if state.Revision == math.MaxUint64 || state.SequenceHighWater == math.MaxUint64 {
			return &CommandRepositorySchemaSkewError{Field: "repository high water", Found: fmt.Sprintf("revision=%d sequence=%d", state.Revision, state.SequenceHighWater), Want: "allocatable uint64"}
		}
		stamped := command
		stamped.Store = state.Store
		stamped.Order = CommandOrder{
			Sequence: state.SequenceHighWater + 1,
			Revision: state.Revision + 1,
		}
		wire, err := EncodeCommandV1(stamped)
		if err != nil {
			return fmt.Errorf("validating repository-stamped command: %w", errors.Join(ErrCommandRepositoryInvalidRequest, err))
		}
		partitionRoute := commandPartitionRoute(partition)
		if partitionRoute == "" {
			return fmt.Errorf("deriving durable nudge command partition route: %w", ErrCommandRepositoryInvalidRequest)
		}
		createdRow, err := tx.Create(beads.Bead{
			ID:       stamped.ID,
			Title:    commandRecordTitle,
			Status:   "open",
			Type:     commandRecordBeadType,
			Assignee: partitionRoute,
			Metadata: map[string]string{
				commandRecordKindMetadataKey:            commandRecordKindMetadataValue,
				commandRecordCommandKindMetadataKey:     commandRecordCommandKindMetadataValue,
				commandRecordPartitionKeyMetadataKey:    partitionRoute,
				commandRecordPartitionSchemaMetadataKey: commandPartitionSchemaVersion,
				commandRecordRequestIDMetadataKey:       requestID,
				commandRecordWireMetadataKey:            string(wire),
			},
		})
		if err != nil {
			return fmt.Errorf("creating durable nudge command %q: %w", stamped.ID, err)
		}
		if _, _, err := decodeCommandRecord(createdRow, CommandRepositoryState{
			Store:             state.Store,
			SchemaVersion:     state.SchemaVersion,
			WriterVersion:     state.WriterVersion,
			Revision:          stamped.Order.Revision,
			SequenceHighWater: stamped.Order.Sequence,
		}); err != nil {
			return err
		}
		if err := setCommandRepositoryHighWaters(tx, stamped.Order.Revision, stamped.Order.Sequence); err != nil {
			return err
		}
		owned := stamped
		result = CommandIndexEntry{Command: &owned}
		created = true
		return nil
	})
	if err != nil {
		// A concurrent same-request winner or commit-with-lost-response can make
		// the transaction return an error after durable authority exists. Because
		// this is a writer path, repair the same-epoch anchor before the read-only
		// exact reread; never allocate a replacement identity.
		if _, repairErr := r.repairLineage(ctx, "ambiguous create lineage repair"); repairErr == nil {
			resolved, resolveErr := r.Get(ctx, command.ID)
			if resolveErr == nil && resolved.Found {
				if retryErr := validateIdempotentCommandRetry(requestID, command, requestID, resolved.Entry); retryErr == nil {
					return resolved.Entry, false, nil
				}
			}
		}
		return CommandIndexEntry{}, false, err
	}
	if _, err := r.repairLineage(ctx, "post-create lineage advance"); err != nil {
		return CommandIndexEntry{}, false, err
	}
	return result, created, nil
}

func (r *CommandRepository) provisionState(ctx context.Context) (CommandRepositoryState, bool, CommandRepositoryProvisioningEvidence, error) {
	var existing CommandRepositoryState
	err := atomicCommandRepositoryRead(ctx, r.reader.store, "gc: read durable nudge command repository before provisioning", func(tx commandRepositoryReadTx) error {
		state, present, err := readOptionalCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if present {
			existing = state
		}
		return nil
	})
	if err != nil {
		return CommandRepositoryState{}, false, CommandRepositoryProvisioningEvidence{}, err
	}
	if existing != (CommandRepositoryState{}) {
		return existing, false, CommandRepositoryProvisioningEvidence{}, nil
	}

	storeID, err := uuid.NewRandomFromReader(r.random)
	if err != nil {
		return CommandRepositoryState{}, false, CommandRepositoryProvisioningEvidence{}, fmt.Errorf("generating command repository store UUID: %w", err)
	}
	var nonce [sha256.Size]byte
	if _, err := io.ReadFull(r.random, nonce[:]); err != nil {
		return CommandRepositoryState{}, false, CommandRepositoryProvisioningEvidence{}, fmt.Errorf("generating command repository provisioning evidence: %w", err)
	}
	proposed := CommandRepositoryState{
		Store:         CommandStoreBinding{StoreUUID: storeID.String(), RestoreEpoch: 1},
		SchemaVersion: CommandRepositorySchemaVersion,
		WriterVersion: CommandRepositoryWriterVersion,
	}
	var (
		state           CommandRepositoryState
		initializedHere bool
	)
	err = r.reader.store.AtomicReadWrite(ctx, "gc: initialize durable nudge command repository", func(tx beads.AtomicReadWriteTx) error {
		var present bool
		var err error
		state, present, err = readOptionalCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		rows, err := tx.ListHistory(commandRecordListQuery(1))
		if err != nil {
			return fmt.Errorf("checking uninitialized command repository rows: %w", err)
		}
		if len(rows) != 0 {
			return &CommandRepositorySchemaSkewError{Field: "repository initialization", Found: "command rows without repository metadata", Want: "all command rows and metadata absent"}
		}
		if err := writeCommandRepositoryState(tx, proposed); err != nil {
			return err
		}
		state = proposed
		initializedHere = true
		return nil
	})
	if err != nil {
		return CommandRepositoryState{}, false, CommandRepositoryProvisioningEvidence{}, err
	}
	evidence := CommandRepositoryProvisioningEvidence{}
	if initializedHere {
		evidence = CommandRepositoryProvisioningEvidence{nonce: nonce, store: state.Store}
	}
	return state, initializedHere, evidence, nil
}

func (r *CommandRepository) repairLineage(ctx context.Context, operation string) (CommandRepositoryState, error) {
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandRepositoryState{}, err
	}
	state, err := r.reader.loadState(ctx, "gc: read durable nudge command repository for lineage repair")
	if err != nil {
		return CommandRepositoryState{}, err
	}
	if err := r.writer.AdvanceCommandRepositoryLineage(ctx, state); err != nil {
		return CommandRepositoryState{}, &CommandRepositoryLineageError{Operation: operation, State: state, Err: err}
	}
	if err := r.reader.verify(ctx, operation+" verification", state); err != nil {
		return CommandRepositoryState{}, err
	}
	return state, nil
}

func (r commandRepositoryReader) loadState(ctx context.Context, commitMessage string) (CommandRepositoryState, error) {
	var state CommandRepositoryState
	err := atomicCommandRepositoryRead(ctx, r.store, commitMessage, func(tx commandRepositoryReadTx) error {
		var err error
		state, err = readCommandRepositoryState(tx)
		return err
	})
	return state, err
}

func (r commandRepositoryReader) verify(ctx context.Context, operation string, state CommandRepositoryState) error {
	if err := validateRepositoryContext(ctx); err != nil {
		return err
	}
	if err := r.verifier.VerifyCommandRepositoryLineage(ctx, state); err != nil {
		return &CommandRepositoryLineageError{Operation: operation, State: state, Err: err}
	}
	return nil
}

func validateRepositoryContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("durable nudge command repository: nil context")
	}
	return ctx.Err()
}

func commandRecordListQuery(limit int) beads.AtomicReadWriteList {
	return beads.AtomicReadWriteList{
		IDPrefix: commandIDPrefix,
		Limit:    limit,
	}
}

// commandRepositoryReadTx intentionally omits every mutation exposed by
// beads.AtomicReadWriteTx. Read callbacks receive only this surface, so adding
// a metadata or record write to State/Get/Snapshot is a compile-time error.
type commandRepositoryReadTx interface {
	GetIssue(string) (beads.Bead, error)
	ListHistory(beads.AtomicReadWriteList) ([]beads.Bead, error)
	GetMetadata(string) (string, error)
}

type commandRepositoryMetadataReader interface {
	GetMetadata(string) (string, error)
}

func atomicCommandRepositoryRead(ctx context.Context, store beads.AtomicReadWriteStore, commitMessage string, fn func(commandRepositoryReadTx) error) error {
	return store.AtomicReadWrite(ctx, commitMessage, func(tx beads.AtomicReadWriteTx) error {
		return fn(tx)
	})
}

func readCommandRepositoryState(tx commandRepositoryMetadataReader) (CommandRepositoryState, error) {
	state, present, err := readOptionalCommandRepositoryState(tx)
	if err != nil {
		return CommandRepositoryState{}, err
	}
	if !present {
		return CommandRepositoryState{}, &CommandRepositoryLineageError{Operation: "transaction read", Err: errors.New("repository metadata disappeared")}
	}
	return state, nil
}

func readOptionalCommandRepositoryState(tx commandRepositoryMetadataReader) (CommandRepositoryState, bool, error) {
	keys := []string{
		commandRepositorySchemaVersionMetadataKey,
		commandRepositoryWriterVersionMetadataKey,
		commandRepositoryStoreUUIDMetadataKey,
		commandRepositoryRestoreEpochMetadataKey,
		commandRepositoryRevisionMetadataKey,
		commandRepositorySequenceHighWaterMetadataKey,
		commandRepositoryPartitionSchemaMetadataKey,
	}
	values := make(map[string]string, len(keys))
	present := 0
	hadNonEmptyRaw := false
	for _, key := range keys {
		value, err := tx.GetMetadata(key)
		if err != nil {
			return CommandRepositoryState{}, false, fmt.Errorf("reading command repository metadata %q: %w", key, err)
		}
		values[key] = value
		if value != "" {
			hadNonEmptyRaw = true
		}
		if strings.TrimSpace(value) != "" {
			present++
		}
	}
	if present == 0 {
		if hadNonEmptyRaw {
			return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "repository metadata", Found: "whitespace-only values", Want: "all values exactly absent or canonical"}
		}
		return CommandRepositoryState{}, false, nil
	}
	if present != len(keys) {
		return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "repository metadata", Found: fmt.Sprintf("%d/%d required values", present, len(keys)), Want: "complete metadata set"}
	}
	schema, err := parseRepositoryUint(values[commandRepositorySchemaVersionMetadataKey], 32, "schema_version")
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	writer, err := parseRepositoryUint(values[commandRepositoryWriterVersionMetadataKey], 32, "writer_version")
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	restoreEpoch, err := parseRepositoryUint(values[commandRepositoryRestoreEpochMetadataKey], 64, "restore_epoch")
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	revision, err := parseRepositoryUint(values[commandRepositoryRevisionMetadataKey], 64, "revision")
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	sequence, err := parseRepositoryUint(values[commandRepositorySequenceHighWaterMetadataKey], 64, "sequence_high_water")
	if err != nil {
		return CommandRepositoryState{}, false, err
	}
	state := CommandRepositoryState{
		Store: CommandStoreBinding{
			StoreUUID:    values[commandRepositoryStoreUUIDMetadataKey],
			RestoreEpoch: restoreEpoch,
		},
		SchemaVersion:     uint32(schema),
		WriterVersion:     uint32(writer),
		Revision:          revision,
		SequenceHighWater: sequence,
	}
	if state.SchemaVersion != CommandRepositorySchemaVersion {
		return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "schema_version", Found: fmt.Sprint(state.SchemaVersion), Want: fmt.Sprint(CommandRepositorySchemaVersion)}
	}
	if state.WriterVersion != CommandRepositoryWriterVersion {
		return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "writer_version", Found: fmt.Sprint(state.WriterVersion), Want: fmt.Sprint(CommandRepositoryWriterVersion)}
	}
	if values[commandRepositoryPartitionSchemaMetadataKey] != commandPartitionSchemaVersion {
		return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "command partition schema version", Found: values[commandRepositoryPartitionSchemaMetadataKey], Want: commandPartitionSchemaVersion}
	}
	if err := validateCommandRepositoryBinding(state.Store); err != nil {
		return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "store binding", Found: fmt.Sprintf("uuid=%q epoch=%d", state.Store.StoreUUID, state.Store.RestoreEpoch), Want: "valid UUID and positive restore epoch"}
	}
	if state.SequenceHighWater > state.Revision {
		return CommandRepositoryState{}, false, &CommandRepositorySchemaSkewError{Field: "high waters", Found: fmt.Sprintf("revision=%d sequence=%d", state.Revision, state.SequenceHighWater), Want: "sequence_high_water <= revision"}
	}
	return state, true, nil
}

func validateCommandRepositoryBinding(binding CommandStoreBinding) error {
	if err := ValidateCommandStoreBinding(binding); err != nil {
		return err
	}
	parsed, err := uuid.Parse(binding.StoreUUID)
	if err != nil || parsed.String() != binding.StoreUUID {
		return fmt.Errorf("store uuid %q is not a canonical UUID", binding.StoreUUID)
	}
	return nil
}

func parseRepositoryUint(value string, bits int, field string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, bits)
	if err != nil {
		return 0, &CommandRepositorySchemaSkewError{Field: field, Found: value, Want: fmt.Sprintf("base-10 uint%d", bits)}
	}
	if strconv.FormatUint(parsed, 10) != value {
		return 0, &CommandRepositorySchemaSkewError{Field: field, Found: value, Want: "canonical base-10 unsigned integer"}
	}
	return parsed, nil
}

func writeCommandRepositoryState(tx beads.AtomicReadWriteTx, state CommandRepositoryState) error {
	values := map[string]string{
		commandRepositorySchemaVersionMetadataKey:     strconv.FormatUint(uint64(state.SchemaVersion), 10),
		commandRepositoryWriterVersionMetadataKey:     strconv.FormatUint(uint64(state.WriterVersion), 10),
		commandRepositoryStoreUUIDMetadataKey:         state.Store.StoreUUID,
		commandRepositoryRestoreEpochMetadataKey:      strconv.FormatUint(state.Store.RestoreEpoch, 10),
		commandRepositoryRevisionMetadataKey:          strconv.FormatUint(state.Revision, 10),
		commandRepositorySequenceHighWaterMetadataKey: strconv.FormatUint(state.SequenceHighWater, 10),
		commandRepositoryPartitionSchemaMetadataKey:   commandPartitionSchemaVersion,
	}
	for _, key := range []string{
		commandRepositorySchemaVersionMetadataKey,
		commandRepositoryWriterVersionMetadataKey,
		commandRepositoryStoreUUIDMetadataKey,
		commandRepositoryRestoreEpochMetadataKey,
		commandRepositoryRevisionMetadataKey,
		commandRepositorySequenceHighWaterMetadataKey,
		commandRepositoryPartitionSchemaMetadataKey,
	} {
		if err := tx.SetMetadata(key, values[key]); err != nil {
			return fmt.Errorf("writing command repository metadata %q: %w", key, err)
		}
	}
	return nil
}

func setCommandRepositoryHighWaters(tx beads.AtomicReadWriteTx, revision, sequence uint64) error {
	if err := tx.SetMetadata(commandRepositoryRevisionMetadataKey, strconv.FormatUint(revision, 10)); err != nil {
		return fmt.Errorf("writing command repository revision: %w", err)
	}
	if err := tx.SetMetadata(commandRepositorySequenceHighWaterMetadataKey, strconv.FormatUint(sequence, 10)); err != nil {
		return fmt.Errorf("writing command repository sequence high-water: %w", err)
	}
	return nil
}

func validateCommandRepositoryStateAdvance(before, current CommandRepositoryState) error {
	if current.SchemaVersion != before.SchemaVersion || current.WriterVersion != before.WriterVersion {
		return &CommandRepositorySchemaSkewError{Field: "transaction repository versions", Found: fmt.Sprintf("schema=%d writer=%d", current.SchemaVersion, current.WriterVersion), Want: fmt.Sprintf("schema=%d writer=%d", before.SchemaVersion, before.WriterVersion)}
	}
	if current.Store != before.Store || current.Revision < before.Revision || current.SequenceHighWater < before.SequenceHighWater {
		return &CommandRepositoryLineageError{
			Operation: "transaction state comparison",
			State:     current,
			Err: fmt.Errorf("authority changed or regressed from store %q epoch %d revision %d sequence %d",
				before.Store.StoreUUID, before.Store.RestoreEpoch, before.Revision, before.SequenceHighWater),
		}
	}
	return nil
}

func validateCommandCreateRequest(binding CommandStoreBinding, requestID string, command Command) error {
	if err := validateCommandIdentity("request id", requestID); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandRepositoryInvalidRequest, err)
	}
	expectedID := CommandIDForRequest(binding, requestID)
	if expectedID == "" || command.ID != expectedID {
		return fmt.Errorf("command id %q does not match deterministic id %q: %w", command.ID, expectedID, ErrCommandRepositoryInvalidRequest)
	}
	if command.Store != (CommandStoreBinding{}) || command.Order != (CommandOrder{}) {
		return fmt.Errorf("caller supplied authoritative store/order fields: %w", ErrCommandRepositoryInvalidRequest)
	}
	if command.Version != CommandVersion1 || command.State != CommandStatePending || command.Retry != nil || command.Claim != nil || command.Terminal != nil {
		return fmt.Errorf("create requires a pristine pending v1 command: %w", ErrCommandRepositoryInvalidRequest)
	}
	if command.Target.Policy == TargetPolicyContinuation && command.Binding != nil {
		return fmt.Errorf("continuation create cannot carry a claim-time launch binding: %w", ErrCommandRepositoryInvalidRequest)
	}
	if command.TrustedIngress.PayloadDigest != ComputeCommandPayloadDigest(command) {
		return fmt.Errorf("trusted-ingress payload digest does not cover the authoritative command id: %w", ErrCommandRepositoryInvalidRequest)
	}
	stamped := command
	stamped.Store = binding
	stamped.Order = CommandOrder{Sequence: 1, Revision: 1}
	if _, err := EncodeCommandV1(stamped); err != nil {
		return fmt.Errorf("command fails v1 validation after authoritative stamping: %w", errors.Join(ErrCommandRepositoryInvalidRequest, err))
	}
	return nil
}

func validateIdempotentCommandRetry(requestID string, requested Command, storedRequestID string, entry CommandIndexEntry) error {
	if storedRequestID != requestID {
		return fmt.Errorf("request id %q resolves to command owned by request %q: %w", requestID, storedRequestID, ErrCommandRepositoryIdempotencyConflict)
	}
	if entry.Command == nil {
		return &CommandRepositorySchemaSkewError{Field: "idempotent command version", Found: "newer opaque command", Want: fmt.Sprintf("version %d", CommandVersion1)}
	}
	existing := *entry.Command
	if existing.ID != requested.ID ||
		existing.TrustedIngress != requested.TrustedIngress ||
		ComputeCommandPayloadDigest(existing) != ComputeCommandPayloadDigest(requested) {
		return fmt.Errorf("request %q reuses deterministic id %q for different immutable content: %w", requestID, requested.ID, ErrCommandRepositoryIdempotencyConflict)
	}
	return nil
}

func decodeCommandRecord(row beads.Bead, state CommandRepositoryState) (CommandIndexEntry, string, error) {
	recordErr := func(err error) (CommandIndexEntry, string, error) {
		return CommandIndexEntry{}, "", &CommandRepositoryRecordError{CommandID: row.ID, Err: err}
	}
	if row.Ephemeral || row.NoHistory {
		return recordErr(errors.New("command record is not history-backed"))
	}
	if row.Type != commandRecordBeadType || row.Title != commandRecordTitle {
		return recordErr(fmt.Errorf("record type/title does not match command contract"))
	}
	if row.Metadata[commandRecordKindMetadataKey] != commandRecordKindMetadataValue ||
		row.Metadata[commandRecordCommandKindMetadataKey] != commandRecordCommandKindMetadataValue {
		return recordErr(errors.New("record metadata kind does not match nudge command contract"))
	}
	requestID := row.Metadata[commandRecordRequestIDMetadataKey]
	if err := validateCommandIdentity("request id", requestID); err != nil {
		return recordErr(err)
	}
	wire, ok := row.Metadata[commandRecordWireMetadataKey]
	if !ok || wire == "" {
		return recordErr(errors.New("command wire metadata is missing"))
	}
	decoded := DecodeCommand([]byte(wire))
	var entry CommandIndexEntry
	switch decoded.Disposition {
	case CommandDecodeDecoded:
		command := decoded.Command
		entry.Command = &command
	case CommandDecodeUpgradeRequired:
		entry.Opaque = &OpaqueCommand{
			Version: decoded.Version,
			Routing: decoded.Routing,
			Raw:     append([]byte(nil), decoded.Raw...),
		}
	case CommandDecodeDeadLetter:
		return recordErr(fmt.Errorf("total codec classified record as %s", decoded.DeadLetterReason))
	default:
		return recordErr(fmt.Errorf("total codec returned unknown disposition %q", decoded.Disposition))
	}
	expectedStatus := "open"
	wireState := "opaque"
	if entry.Command != nil {
		wireState = string(entry.Command.State)
		if commandIsTerminalState(entry.Command.State) {
			expectedStatus = "closed"
		}
	}
	if row.Status != expectedStatus {
		return recordErr(fmt.Errorf("record status %q does not mirror wire state %q; want %q", row.Status, wireState, expectedStatus))
	}
	routing := commandIndexEntryRouting(entry)
	if routing.CommandID != row.ID {
		return recordErr(fmt.Errorf("wire command id %q does not match bead id", routing.CommandID))
	}
	expectedID := CommandIDForRequest(state.Store, requestID)
	if expectedID == "" || row.ID != expectedID {
		return recordErr(fmt.Errorf("bead id does not match store/request binding"))
	}
	if routing.Store != state.Store {
		return CommandIndexEntry{}, "", &CommandRepositoryLineageError{Operation: "decode command record", State: state, Err: fmt.Errorf("command %q is bound to store %q epoch %d", row.ID, routing.Store.StoreUUID, routing.Store.RestoreEpoch)}
	}
	if routing.Sequence == 0 || routing.Sequence > state.SequenceHighWater || routing.Revision == 0 || routing.Revision > state.Revision {
		return recordErr(fmt.Errorf("routing order sequence=%d revision=%d exceeds repository sequence=%d revision=%d", routing.Sequence, routing.Revision, state.SequenceHighWater, state.Revision))
	}
	return entry, requestID, nil
}
