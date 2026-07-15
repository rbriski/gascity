package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/btree"
)

const (
	// MaxCommandIndexPageSize is the hard upper bound for one session-index
	// page. There is no unbounded or default-zero read mode.
	MaxCommandIndexPageSize = 256
	// MaxCommandIndexReplayHistory bounds the in-memory duplicate-detection
	// window. Older replays fail closed and require an independent audit.
	MaxCommandIndexReplayHistory        = 256
	commandIndexPartitionSnapshotDomain = "gascity.command-index.trusted-partition-snapshot.v1"
)

var (
	// ErrCommandIndexUnsynced reports that a gap or conflicting replay made the
	// derived index unsafe to advance incrementally. An independent audit is the
	// only recovery path.
	ErrCommandIndexUnsynced = errors.New("nudge command index is unsynced")
	// ErrCommandIndexUpgradeBarrier reports that a caller-supplied continuation
	// cursor would cross a command parked for a newer compatible owner.
	ErrCommandIndexUpgradeBarrier = errors.New("nudge command index continuation crosses upgrade barrier")
	// ErrCommandIndexOpaqueBarrier is an alias retained for callers that
	// classified the first supported upgrade barrier as opaque.
	ErrCommandIndexOpaqueBarrier = ErrCommandIndexUpgradeBarrier
)

// CommandIndex is a reconstructable, non-authoritative projection of decoded
// durable commands. It contains no store, runtime, or provider capability.
type CommandIndex struct {
	mu sync.RWMutex

	store          CommandStoreBinding
	entries        map[string]CommandIndexEntry
	ordered        *btree.BTreeG[commandIndexOrderRef]
	barrierOrdered *btree.BTreeG[commandIndexOrderRef]
	tombstones     map[string]CommandIndexTombstone
	replays        map[uint64][sha256.Size]byte
	coverage       *CommandIndexCompactedCoverage

	revision               uint64
	sequenceHighWater      uint64
	completedAuditRevision uint64
	synced                 bool
	unsyncedReason         string
}

// CommandIndexEntry is an exact tagged union. Command contains one validated
// v1 value, or Opaque contains one decode-validated newer value; exactly one
// arm must be non-nil.
type CommandIndexEntry struct {
	Command *Command
	Opaque  *OpaqueCommand
}

// CommandIndexPage is one strictly bounded sequence-ordered page for a
// canonical target session. A page stops at the first upgrade barrier because
// an older owner may not advance that session beyond parked or uninterpretable
// work. NextAfterSequence is zero when no safe continuation is available.
type CommandIndexPage struct {
	Store                  CommandStoreBinding
	Revision               uint64
	CompletedAuditRevision uint64
	Entries                []CommandIndexEntry
	NextAfterSequence      uint64
}

// CommandIndexResolution is one entry lookup together with the exact index
// watermark that produced it.
type CommandIndexResolution struct {
	Store                  CommandStoreBinding
	Revision               uint64
	CompletedAuditRevision uint64
	Entry                  CommandIndexEntry
	Found                  bool
}

// CommandIndexSnapshot is the authoritative input to an independent rebuild.
// Repository snapshots are globally dense. A CommandPartitionReader instead
// returns only its authority-proven active set and seals that exact sparse
// projection with a private witness; global terminal history and foreign
// sequence gaps never enter the city-local hot path.
type CommandIndexSnapshot struct {
	Store             CommandStoreBinding
	Entries           []CommandIndexEntry
	Tombstones        []CommandIndexTombstone
	PartitionGaps     []CommandIndexPartitionGap
	Coverage          *CommandIndexCompactedCoverage
	Revision          uint64
	SequenceHighWater uint64
	partitionWitness  [sha256.Size]byte
}

// CommandIndexPartitionGap certifies one inclusive range of sequences owned by
// foreign trusted city partitions. Ranges are canonical, disjoint from local
// terminal/tombstone coverage, and carry no foreign command identity, target,
// content, or authorization data.
type CommandIndexPartitionGap struct {
	FirstSequence uint64
	LastSequence  uint64
}

// CommandIndexCompactedCoverage is bounded evidence for exact terminal records
// and tombstones intentionally omitted from a rebuild. Ranges contain only
// sequences that are no longer active. Together with the exact Entries and
// Tombstones and trusted PartitionGaps in a snapshot they must partition every
// sequence from one through SequenceHighWater without a gap or overlap.
type CommandIndexCompactedCoverage struct {
	PublishedRevision uint64
	Ranges            []CommandIndexSequenceRange
	TerminalCount     uint64
	TombstoneCount    uint64
	FingerprintSHA256 string
}

// CommandIndexSequenceRange is one inclusive canonical interval of compacted
// command sequences. Adjacent intervals are rejected because a canonical
// checkpoint must merge them.
type CommandIndexSequenceRange struct {
	FirstSequence uint64
	LastSequence  uint64
}

// CommandIndexTombstone is the durable deletion evidence retained by an audit
// snapshot. It prevents a deleted command identity from being resurrected.
type CommandIndexTombstone struct {
	CommandID        string
	Store            CommandStoreBinding
	Revision         uint64
	PriorVersion     uint32
	PriorRevision    uint64
	PriorState       CommandState
	TargetSessionID  string
	IntentGeneration uint64
	Sequence         uint64
}

// CommandIndexMutation is one decoded post-commit index change. Exactly one of
// Entry and Tombstone must be set, and Revision is the global mutation
// revision carried by the durable command ledger.
type CommandIndexMutation struct {
	Store     CommandStoreBinding
	Revision  uint64
	Entry     *CommandIndexEntry
	Tombstone *CommandIndexTombstone
}

// CommandIndexStatus describes the projection's current revision and the last
// independently rebuilt revision. It is diagnostic state, never authority.
type CommandIndexStatus struct {
	Store                  CommandStoreBinding
	Revision               uint64
	SequenceHighWater      uint64
	CompletedAuditRevision uint64
	Synced                 bool
	UnsyncedReason         string
}

type commandIndexOrderRef struct {
	sessionID string
	sequence  uint64
	id        string
}

type commandIndexProjection struct {
	entries        map[string]CommandIndexEntry
	ordered        *btree.BTreeG[commandIndexOrderRef]
	barrierOrdered *btree.BTreeG[commandIndexOrderRef]
	tombstones     map[string]CommandIndexTombstone
	replays        map[uint64][sha256.Size]byte
	coverage       *CommandIndexCompactedCoverage
}

// BuildCommandIndex independently rebuilds an index from the complete decoded
// command snapshot at revision. The successful build itself is a completed
// audit at that revision.
func BuildCommandIndex(snapshot CommandIndexSnapshot) (*CommandIndex, error) {
	projection, err := buildCommandIndexProjection(snapshot)
	if err != nil {
		return nil, err
	}
	return &CommandIndex{
		store:                  snapshot.Store,
		entries:                projection.entries,
		ordered:                projection.ordered,
		barrierOrdered:         projection.barrierOrdered,
		tombstones:             projection.tombstones,
		replays:                projection.replays,
		coverage:               cloneCommandIndexCoverage(projection.coverage),
		revision:               snapshot.Revision,
		sequenceHighWater:      snapshot.SequenceHighWater,
		completedAuditRevision: snapshot.Revision,
		synced:                 true,
	}, nil
}

func sealCommandIndexPartitionSnapshot(snapshot CommandIndexSnapshot) (CommandIndexSnapshot, error) {
	if snapshot.Coverage != nil || len(snapshot.Tombstones) != 0 || len(snapshot.PartitionGaps) != 0 {
		return CommandIndexSnapshot{}, errors.New("trusted sparse partition snapshot carries global coverage")
	}
	digest, err := commandIndexPartitionSnapshotDigest(snapshot)
	if err != nil {
		return CommandIndexSnapshot{}, err
	}
	snapshot.partitionWitness = digest
	return snapshot, nil
}

func validateCommandIndexPartitionSnapshot(snapshot CommandIndexSnapshot) (bool, error) {
	if snapshot.partitionWitness == ([sha256.Size]byte{}) {
		return false, nil
	}
	if snapshot.Coverage != nil || len(snapshot.Tombstones) != 0 || len(snapshot.PartitionGaps) != 0 {
		return false, errors.New("trusted sparse partition snapshot carries global coverage")
	}
	digest, err := commandIndexPartitionSnapshotDigest(snapshot)
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare(snapshot.partitionWitness[:], digest[:]) != 1 {
		return false, errors.New("trusted sparse partition snapshot changed after authority verification")
	}
	return true, nil
}

func commandIndexPartitionSnapshotDigest(snapshot CommandIndexSnapshot) ([sha256.Size]byte, error) {
	snapshot.partitionWitness = [sha256.Size]byte{}
	wire, err := json.Marshal(snapshot)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("fingerprinting trusted sparse partition snapshot: %w", err)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(commandIndexPartitionSnapshotDomain))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(wire)
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func buildCommandIndexProjection(snapshot CommandIndexSnapshot) (commandIndexProjection, error) {
	if err := ValidateCommandStoreBinding(snapshot.Store); err != nil {
		return commandIndexProjection{}, fmt.Errorf("building nudge command index: %w", err)
	}
	sparsePartition, err := validateCommandIndexPartitionSnapshot(snapshot)
	if err != nil {
		return commandIndexProjection{}, fmt.Errorf("building nudge command index: %w", err)
	}
	projection := commandIndexProjection{
		entries:        make(map[string]CommandIndexEntry, len(snapshot.Entries)),
		ordered:        btree.NewG(32, commandIndexOrderLess),
		barrierOrdered: btree.NewG(32, commandIndexOrderLess),
		tombstones:     make(map[string]CommandIndexTombstone, len(snapshot.Tombstones)),
		replays:        make(map[uint64][sha256.Size]byte, min(len(snapshot.Entries)+len(snapshot.Tombstones), MaxCommandIndexReplayHistory)),
		coverage:       cloneCommandIndexCoverage(snapshot.Coverage),
	}
	if !sparsePartition {
		_, err := validateCommandIndexCoverage(snapshot.Coverage, snapshot.Revision, snapshot.SequenceHighWater)
		if err != nil {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: %w", err)
		}
	}
	exactSequenceOwner := make(map[uint64]string, len(snapshot.Entries)+len(snapshot.Tombstones))
	intervals := make([]commandIndexSequenceInterval, 0, len(snapshot.Entries)+len(snapshot.Tombstones)+len(snapshot.PartitionGaps)+len(commandIndexCoverageRanges(snapshot.Coverage)))
	for _, sequenceRange := range commandIndexCoverageRanges(snapshot.Coverage) {
		intervals = append(intervals, commandIndexSequenceInterval{first: sequenceRange.FirstSequence, last: sequenceRange.LastSequence, owner: "compacted coverage"})
	}
	var maxSequence uint64
	var priorGapLast uint64
	for index, gap := range snapshot.PartitionGaps {
		if gap.FirstSequence == 0 || gap.LastSequence < gap.FirstSequence || gap.LastSequence > snapshot.SequenceHighWater {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: trusted partition gap range %d [%d,%d] is outside sequence high-water %d", index, gap.FirstSequence, gap.LastSequence, snapshot.SequenceHighWater)
		}
		if index > 0 && gap.FirstSequence <= priorGapLast {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: trusted partition gap range %d overlaps or is out of order", index)
		}
		if index > 0 && priorGapLast != ^uint64(0) && gap.FirstSequence == priorGapLast+1 {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: trusted partition gap range %d is adjacent instead of canonically merged", index)
		}
		intervals = append(intervals, commandIndexSequenceInterval{first: gap.FirstSequence, last: gap.LastSequence, owner: "trusted partition gap"})
		priorGapLast = gap.LastSequence
		maxSequence = max(maxSequence, gap.LastSequence)
	}
	for _, tombstone := range snapshot.Tombstones {
		if err := validateCommandIndexTombstone(tombstone, snapshot.Store, snapshot.Revision); err != nil {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: %w", err)
		}
		if _, duplicate := projection.tombstones[tombstone.CommandID]; duplicate {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: duplicate tombstone id %q", tombstone.CommandID)
		}
		if owner, exists := exactSequenceOwner[tombstone.Sequence]; exists {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: sequence %d belongs to both %q and %q", tombstone.Sequence, owner, tombstone.CommandID)
		}
		if commandIndexCoverageContainsSequence(snapshot.Coverage, tombstone.Sequence) {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: tombstone %q sequence %d overlaps compacted coverage", tombstone.CommandID, tombstone.Sequence)
		}
		exactSequenceOwner[tombstone.Sequence] = tombstone.CommandID
		intervals = append(intervals, commandIndexSequenceInterval{first: tombstone.Sequence, last: tombstone.Sequence, owner: tombstone.CommandID})
		maxSequence = max(maxSequence, tombstone.Sequence)
		projection.tombstones[tombstone.CommandID] = tombstone
		copyForReplay := tombstone
		mutation := CommandIndexMutation{Store: snapshot.Store, Revision: tombstone.Revision, Tombstone: &copyForReplay}
		if err := recordCommandIndexReplay(projection.replays, mutation); err != nil {
			return commandIndexProjection{}, err
		}
	}
	for _, entry := range snapshot.Entries {
		if err := validateCommandIndexEntryRecord(entry, snapshot.Revision); err != nil {
			return commandIndexProjection{}, err
		}
		routing := commandIndexEntryRouting(entry)
		if routing.Store != snapshot.Store {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: command %q store binding %#v does not match snapshot binding %#v", routing.CommandID, routing.Store, snapshot.Store)
		}
		if _, tombstoned := projection.tombstones[routing.CommandID]; tombstoned {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: command id %q is both live and tombstoned", routing.CommandID)
		}
		if _, exists := projection.entries[routing.CommandID]; exists {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: duplicate command id %q", routing.CommandID)
		}
		if owner, exists := exactSequenceOwner[routing.Sequence]; exists {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: sequence %d belongs to both %q and %q", routing.Sequence, owner, routing.CommandID)
		}
		if commandIndexCoverageContainsSequence(snapshot.Coverage, routing.Sequence) {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: command %q sequence %d overlaps compacted coverage", routing.CommandID, routing.Sequence)
		}
		exactSequenceOwner[routing.Sequence] = routing.CommandID
		intervals = append(intervals, commandIndexSequenceInterval{first: routing.Sequence, last: routing.Sequence, owner: routing.CommandID})
		maxSequence = max(maxSequence, routing.Sequence)
		owned := cloneCommandIndexEntry(entry)
		projection.entries[routing.CommandID] = owned
		copyForReplay := cloneCommandIndexEntry(entry)
		if err := recordCommandIndexReplay(projection.replays, CommandIndexMutation{Store: snapshot.Store, Revision: routing.Revision, Entry: &copyForReplay}); err != nil {
			return commandIndexProjection{}, err
		}
		ref := commandIndexOrderRef{sessionID: routing.TargetSessionID, sequence: routing.Sequence, id: routing.CommandID}
		if commandIndexEntryInOrderingDomain(entry) {
			projection.ordered.ReplaceOrInsert(ref)
		}
		if commandIndexEntryIsUpgradeBarrier(entry) {
			projection.barrierOrdered.ReplaceOrInsert(ref)
		}
	}
	if maxSequence > snapshot.SequenceHighWater {
		return commandIndexProjection{}, fmt.Errorf("building nudge command index: record sequence %d exceeds authoritative high-water %d", maxSequence, snapshot.SequenceHighWater)
	}
	if !sparsePartition {
		if err := validateDenseCommandIndexIntervals(intervals, snapshot.SequenceHighWater); err != nil {
			return commandIndexProjection{}, fmt.Errorf("building nudge command index: %w", err)
		}
	}
	trimCommandIndexReplays(projection.replays, snapshot.Revision)
	return projection, nil
}

type commandIndexSequenceInterval struct {
	first uint64
	last  uint64
	owner string
}

func commandIndexCoverageRanges(coverage *CommandIndexCompactedCoverage) []CommandIndexSequenceRange {
	if coverage == nil {
		return nil
	}
	return coverage.Ranges
}

func validateDenseCommandIndexIntervals(intervals []commandIndexSequenceInterval, sequenceHighWater uint64) error {
	if sequenceHighWater == 0 {
		if len(intervals) != 0 {
			return errors.New("records exist above zero sequence high-water")
		}
		return nil
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].first != intervals[j].first {
			return intervals[i].first < intervals[j].first
		}
		return intervals[i].last < intervals[j].last
	})
	expected := uint64(1)
	for _, interval := range intervals {
		if interval.first < expected {
			return fmt.Errorf("sequence range [%d,%d] owned by %q overlaps prior coverage", interval.first, interval.last, interval.owner)
		}
		if interval.first > expected {
			return fmt.Errorf("sequence %d is not represented before range [%d,%d] owned by %q", expected, interval.first, interval.last, interval.owner)
		}
		if interval.last > sequenceHighWater {
			return fmt.Errorf("sequence range [%d,%d] owned by %q exceeds authoritative high-water %d", interval.first, interval.last, interval.owner, sequenceHighWater)
		}
		if interval.last == ^uint64(0) {
			expected = 0
			continue
		}
		expected = interval.last + 1
	}
	if sequenceHighWater == ^uint64(0) {
		if expected != 0 {
			return fmt.Errorf("sequence %d is not represented through high-water %d", expected, sequenceHighWater)
		}
		return nil
	}
	if expected != sequenceHighWater+1 {
		return fmt.Errorf("sequence %d is not represented through high-water %d", expected, sequenceHighWater)
	}
	return nil
}

func validateCommandIndexCoverage(coverage *CommandIndexCompactedCoverage, snapshotRevision, sequenceHighWater uint64) (uint64, error) {
	if coverage == nil {
		return 0, nil
	}
	if coverage.PublishedRevision == 0 || coverage.PublishedRevision > snapshotRevision {
		return 0, fmt.Errorf("compacted coverage publication revision %d is outside snapshot revision %d", coverage.PublishedRevision, snapshotRevision)
	}
	digest, err := hex.DecodeString(coverage.FingerprintSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != coverage.FingerprintSHA256 {
		return 0, errors.New("compacted coverage fingerprint is not a canonical SHA-256 digest")
	}
	var covered uint64
	var priorLast uint64
	for i, sequenceRange := range coverage.Ranges {
		if sequenceRange.FirstSequence == 0 || sequenceRange.LastSequence < sequenceRange.FirstSequence || sequenceRange.LastSequence > sequenceHighWater {
			return 0, fmt.Errorf("compacted coverage range %d [%d,%d] is outside sequence high-water %d", i, sequenceRange.FirstSequence, sequenceRange.LastSequence, sequenceHighWater)
		}
		if i > 0 {
			if sequenceRange.FirstSequence <= priorLast {
				return 0, fmt.Errorf("compacted coverage range %d overlaps or is out of order", i)
			}
			if priorLast != ^uint64(0) && sequenceRange.FirstSequence == priorLast+1 {
				return 0, fmt.Errorf("compacted coverage range %d is adjacent instead of canonically merged", i)
			}
		}
		length := sequenceRange.LastSequence - sequenceRange.FirstSequence + 1
		var overflow bool
		covered, overflow = addCommandIndexUint64(covered, length)
		if overflow {
			return 0, errors.New("compacted coverage sequence count overflows uint64")
		}
		priorLast = sequenceRange.LastSequence
	}
	conserved, overflow := addCommandIndexUint64(coverage.TerminalCount, coverage.TombstoneCount)
	if overflow || conserved != covered {
		return 0, fmt.Errorf("compacted coverage conserves %d terminal plus %d tombstone records, want %d covered sequences", coverage.TerminalCount, coverage.TombstoneCount, covered)
	}
	return covered, nil
}

func addCommandIndexUint64(left, right uint64) (uint64, bool) {
	result := left + right
	return result, result < left
}

func commandIndexCoverageContainsSequence(coverage *CommandIndexCompactedCoverage, sequence uint64) bool {
	if coverage == nil || sequence == 0 {
		return false
	}
	position := sort.Search(len(coverage.Ranges), func(i int) bool {
		return coverage.Ranges[i].LastSequence >= sequence
	})
	return position < len(coverage.Ranges) && coverage.Ranges[position].FirstSequence <= sequence
}

func commandIndexCoverageContainsRange(coverage *CommandIndexCompactedCoverage, sequenceRange CommandIndexSequenceRange) bool {
	if coverage == nil || sequenceRange.FirstSequence == 0 || sequenceRange.LastSequence < sequenceRange.FirstSequence {
		return false
	}
	position := sort.Search(len(coverage.Ranges), func(i int) bool {
		return coverage.Ranges[i].LastSequence >= sequenceRange.FirstSequence
	})
	return position < len(coverage.Ranges) && coverage.Ranges[position].FirstSequence <= sequenceRange.FirstSequence && coverage.Ranges[position].LastSequence >= sequenceRange.LastSequence
}

func cloneCommandIndexCoverage(coverage *CommandIndexCompactedCoverage) *CommandIndexCompactedCoverage {
	if coverage == nil {
		return nil
	}
	owned := *coverage
	owned.Ranges = append([]CommandIndexSequenceRange(nil), coverage.Ranges...)
	return &owned
}

func validateCommandIndexTombstone(tombstone CommandIndexTombstone, store CommandStoreBinding, snapshotRevision uint64) error {
	if err := validateCommandIdentity("tombstone command id", tombstone.CommandID); err != nil {
		return err
	}
	if err := ValidateCommandStoreBinding(tombstone.Store); err != nil {
		return fmt.Errorf("tombstone %q: %w", tombstone.CommandID, err)
	}
	if tombstone.Store != store {
		return fmt.Errorf("tombstone %q store binding %#v does not match index binding %#v", tombstone.CommandID, tombstone.Store, store)
	}
	if tombstone.Revision == 0 || tombstone.Revision > snapshotRevision {
		return fmt.Errorf("tombstone %q revision %d is outside snapshot revision %d", tombstone.CommandID, tombstone.Revision, snapshotRevision)
	}
	if tombstone.PriorRevision == 0 || tombstone.PriorRevision >= tombstone.Revision {
		return fmt.Errorf("tombstone %q prior revision %d must be positive and precede deletion revision %d", tombstone.CommandID, tombstone.PriorRevision, tombstone.Revision)
	}
	if tombstone.PriorVersion != CommandVersion1 {
		return fmt.Errorf("tombstone %q prior version %d is not owned by this index", tombstone.CommandID, tombstone.PriorVersion)
	}
	if !commandIsTerminalState(tombstone.PriorState) {
		return fmt.Errorf("tombstone %q prior state %q is not terminal", tombstone.CommandID, tombstone.PriorState)
	}
	if err := validateCommandIdentity("tombstone target session id", tombstone.TargetSessionID); err != nil {
		return err
	}
	if tombstone.IntentGeneration == 0 {
		return fmt.Errorf("tombstone %q intent generation must be positive", tombstone.CommandID)
	}
	if tombstone.Sequence == 0 {
		return fmt.Errorf("tombstone %q sequence must be positive", tombstone.CommandID)
	}
	return nil
}

func commandIndexOrderLess(left, right commandIndexOrderRef) bool {
	if left.sessionID != right.sessionID {
		return left.sessionID < right.sessionID
	}
	if left.sequence != right.sequence {
		return left.sequence < right.sequence
	}
	return left.id < right.id
}

func validateCommandIndexEntryRecord(entry CommandIndexEntry, snapshotRevision uint64) error {
	routing, err := validateCommandIndexEntry(entry)
	if err != nil {
		return fmt.Errorf("building nudge command index: %w", err)
	}
	if routing.Revision > snapshotRevision {
		return fmt.Errorf("building nudge command index: command %q revision %d exceeds snapshot revision %d", routing.CommandID, routing.Revision, snapshotRevision)
	}
	return nil
}

func validateCommandIndexEntry(entry CommandIndexEntry) (CommandRoutingHeader, error) {
	hasCommand := entry.Command != nil
	hasOpaque := entry.Opaque != nil
	if hasCommand == hasOpaque {
		return CommandRoutingHeader{}, errors.New("command index entry requires exactly one known command or opaque command")
	}
	if hasCommand {
		if err := validateCommandV1(*entry.Command); err != nil {
			return CommandRoutingHeader{}, fmt.Errorf("command %q is invalid: %w", entry.Command.ID, err)
		}
		return commandRoutingHeader(*entry.Command), nil
	}
	decoded := DecodeCommand(entry.Opaque.Raw)
	if decoded.Disposition != CommandDecodeUpgradeRequired {
		return CommandRoutingHeader{}, fmt.Errorf("opaque command must decode as upgrade-required, got %q", decoded.Disposition)
	}
	if decoded.Version != entry.Opaque.Version {
		return CommandRoutingHeader{}, fmt.Errorf("opaque command version %d does not match decoded version %d", entry.Opaque.Version, decoded.Version)
	}
	if decoded.Routing != entry.Opaque.Routing {
		return CommandRoutingHeader{}, errors.New("opaque command routing does not match decoded raw bytes")
	}
	if !bytes.Equal(decoded.Raw, entry.Opaque.Raw) {
		return CommandRoutingHeader{}, errors.New("opaque command raw bytes changed during decode")
	}
	return entry.Opaque.Routing, nil
}

func commandIndexEntryRouting(entry CommandIndexEntry) CommandRoutingHeader {
	if entry.Command != nil {
		return commandRoutingHeader(*entry.Command)
	}
	if entry.Opaque != nil {
		return entry.Opaque.Routing
	}
	return CommandRoutingHeader{}
}

func commandIndexEntryInOrderingDomain(entry CommandIndexEntry) bool {
	return entry.Opaque != nil || entry.Command != nil && commandInOrderingDomain(entry.Command.State)
}

func commandIndexEntryIsUpgradeBarrier(entry CommandIndexEntry) bool {
	return entry.Opaque != nil || entry.Command != nil && entry.Command.State == CommandStateUpgradeRequired
}

func commandInOrderingDomain(state CommandState) bool {
	switch state {
	case CommandStatePending, CommandStateInFlight, CommandStateUpgradeRequired:
		return true
	default:
		return false
	}
}

// Apply advances the derived index by one contiguous durable mutation. An
// identical replay is a no-op. A gap, unknown stale replay, conflicting replay,
// immutable identity change, or sequence reuse fails the index closed until an
// independent audit replaces it.
func (idx *CommandIndex) Apply(mutation CommandIndexMutation) error {
	if idx == nil {
		return errors.New("applying nudge command index mutation: index is nil")
	}
	mutation = cloneCommandIndexMutation(mutation)
	digest, err := validateCommandIndexMutation(mutation)
	if err != nil {
		return err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !idx.synced {
		return fmt.Errorf("%w: %s", ErrCommandIndexUnsynced, idx.unsyncedReason)
	}
	if mutation.Store != idx.store {
		return idx.markUnsyncedLocked(fmt.Sprintf("mutation store binding %#v does not match index binding %#v", mutation.Store, idx.store))
	}
	if mutation.Revision <= idx.revision {
		if prior, ok := idx.replays[mutation.Revision]; ok && prior == digest {
			return nil
		}
		return idx.markUnsyncedLocked(fmt.Sprintf("conflicting or unknown stale mutation at revision %d", mutation.Revision))
	}
	if mutation.Revision != idx.revision+1 {
		return idx.markUnsyncedLocked(fmt.Sprintf("mutation revision gap: current=%d received=%d", idx.revision, mutation.Revision))
	}

	if mutation.Entry != nil {
		if err := idx.applyEntryLocked(*mutation.Entry); err != nil {
			return idx.markUnsyncedLocked(err.Error())
		}
	} else {
		if err := idx.applyTombstoneLocked(*mutation.Tombstone); err != nil {
			return idx.markUnsyncedLocked(err.Error())
		}
	}
	idx.revision = mutation.Revision
	idx.replays[mutation.Revision] = digest
	trimCommandIndexReplays(idx.replays, mutation.Revision)
	return nil
}

func trimCommandIndexReplays(replays map[uint64][sha256.Size]byte, newestRevision uint64) {
	if newestRevision < MaxCommandIndexReplayHistory {
		return
	}
	oldestRetained := newestRevision - MaxCommandIndexReplayHistory + 1
	for revision := range replays {
		if revision < oldestRetained {
			delete(replays, revision)
		}
	}
}

func validateCommandIndexMutation(mutation CommandIndexMutation) ([sha256.Size]byte, error) {
	if err := ValidateCommandStoreBinding(mutation.Store); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("applying nudge command index mutation: %w", err)
	}
	if mutation.Revision == 0 {
		return [sha256.Size]byte{}, errors.New("applying nudge command index mutation: revision must be positive")
	}
	hasEntry := mutation.Entry != nil
	hasTombstone := mutation.Tombstone != nil
	if hasEntry == hasTombstone {
		return [sha256.Size]byte{}, errors.New("applying nudge command index mutation: exactly one entry or tombstone is required")
	}
	if hasEntry {
		routing, err := validateCommandIndexEntry(*mutation.Entry)
		if err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("applying nudge command index mutation: %w", err)
		}
		if routing.Store != mutation.Store {
			return [sha256.Size]byte{}, fmt.Errorf("applying nudge command index mutation: command store binding %#v does not match mutation binding %#v", routing.Store, mutation.Store)
		}
		if routing.Revision != mutation.Revision {
			return [sha256.Size]byte{}, fmt.Errorf("applying nudge command index mutation: envelope revision %d does not match command revision %d", mutation.Revision, routing.Revision)
		}
	} else {
		if mutation.Tombstone.Revision != mutation.Revision {
			return [sha256.Size]byte{}, fmt.Errorf("applying nudge command index mutation: envelope revision %d does not match tombstone revision %d", mutation.Revision, mutation.Tombstone.Revision)
		}
		if err := validateCommandIndexTombstone(*mutation.Tombstone, mutation.Store, mutation.Revision); err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("applying nudge command index mutation: %w", err)
		}
	}
	return commandIndexMutationDigest(mutation)
}

func cloneCommandIndexMutation(mutation CommandIndexMutation) CommandIndexMutation {
	if mutation.Entry != nil {
		entry := cloneCommandIndexEntry(*mutation.Entry)
		mutation.Entry = &entry
	}
	if mutation.Tombstone != nil {
		tombstone := *mutation.Tombstone
		mutation.Tombstone = &tombstone
	}
	return mutation
}

func commandIndexMutationDigest(mutation CommandIndexMutation) ([sha256.Size]byte, error) {
	owned := cloneCommandIndexMutation(mutation)
	wire, err := json.Marshal(owned)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("fingerprinting nudge command index mutation: %w", err)
	}
	return sha256.Sum256(wire), nil
}

func recordCommandIndexReplay(replays map[uint64][sha256.Size]byte, mutation CommandIndexMutation) error {
	digest, err := commandIndexMutationDigest(mutation)
	if err != nil {
		return err
	}
	if prior, exists := replays[mutation.Revision]; exists && prior != digest {
		return fmt.Errorf("building nudge command index: revision %d describes conflicting records", mutation.Revision)
	}
	replays[mutation.Revision] = digest
	return nil
}

func (idx *CommandIndex) applyEntryLocked(entry CommandIndexEntry) error {
	routing := commandIndexEntryRouting(entry)
	if _, tombstoned := idx.tombstones[routing.CommandID]; tombstoned {
		return fmt.Errorf("command id %q was tombstoned and cannot be resurrected", routing.CommandID)
	}
	existing, exists := idx.entries[routing.CommandID]
	if exists {
		if err := validateCommandIndexEntryUpdate(existing, entry); err != nil {
			return err
		}
		existingRouting := commandIndexEntryRouting(existing)
		wasOrdered := commandIndexEntryInOrderingDomain(existing)
		isOrdered := commandIndexEntryInOrderingDomain(entry)
		switch {
		case wasOrdered && !isOrdered:
			idx.removeOrderedCommandLocked(existingRouting.TargetSessionID, existingRouting.Sequence, existingRouting.CommandID)
		case !wasOrdered && isOrdered:
			idx.insertOrderedCommandLocked(routing.TargetSessionID, commandIndexOrderRef{sequence: routing.Sequence, id: routing.CommandID})
		}
		wasBarrier := commandIndexEntryIsUpgradeBarrier(existing)
		isBarrier := commandIndexEntryIsUpgradeBarrier(entry)
		switch {
		case wasBarrier && !isBarrier:
			idx.barrierOrdered.Delete(commandIndexOrderRef{sessionID: existingRouting.TargetSessionID, sequence: existingRouting.Sequence, id: existingRouting.CommandID})
		case !wasBarrier && isBarrier:
			idx.barrierOrdered.ReplaceOrInsert(commandIndexOrderRef{sessionID: routing.TargetSessionID, sequence: routing.Sequence, id: routing.CommandID})
		}
		idx.entries[routing.CommandID] = cloneCommandIndexEntry(entry)
		return nil
	}

	wantSequence := idx.sequenceHighWater + 1
	if routing.Sequence != wantSequence {
		return fmt.Errorf("new command %q sequence %d does not densely advance high-water %d (want %d)", routing.CommandID, routing.Sequence, idx.sequenceHighWater, wantSequence)
	}
	idx.sequenceHighWater = routing.Sequence
	idx.entries[routing.CommandID] = cloneCommandIndexEntry(entry)
	ref := commandIndexOrderRef{sessionID: routing.TargetSessionID, sequence: routing.Sequence, id: routing.CommandID}
	if commandIndexEntryInOrderingDomain(entry) {
		// A genuinely new command must advance the global high-water, so its
		// per-session position is necessarily an append.
		idx.ordered.ReplaceOrInsert(ref)
	}
	if commandIndexEntryIsUpgradeBarrier(entry) {
		idx.barrierOrdered.ReplaceOrInsert(ref)
	}
	return nil
}

func validateCommandIndexEntryUpdate(existing, updated CommandIndexEntry) error {
	switch {
	case existing.Command != nil && updated.Command != nil:
		return validateCommandIndexUpdate(*existing.Command, *updated.Command)
	case existing.Opaque != nil && updated.Opaque != nil:
		return validateOpaqueCommandIndexUpdate(*existing.Opaque, *updated.Opaque)
	default:
		routing := commandIndexEntryRouting(existing)
		return fmt.Errorf("command %q changed known/opaque representation", routing.CommandID)
	}
}

func validateOpaqueCommandIndexUpdate(existing, updated OpaqueCommand) error {
	if existing.Version != updated.Version {
		return fmt.Errorf("command %q changed immutable opaque version", existing.Routing.CommandID)
	}
	if existing.Routing.CommandID != updated.Routing.CommandID ||
		existing.Routing.Store != updated.Routing.Store ||
		existing.Routing.TargetSessionID != updated.Routing.TargetSessionID ||
		existing.Routing.IntentGeneration != updated.Routing.IntentGeneration ||
		existing.Routing.Sequence != updated.Routing.Sequence {
		return fmt.Errorf("command %q changed immutable opaque routing identity", existing.Routing.CommandID)
	}
	if updated.Routing.Revision <= existing.Routing.Revision {
		return fmt.Errorf("command %q record revision %d does not advance prior revision %d", existing.Routing.CommandID, updated.Routing.Revision, existing.Routing.Revision)
	}
	return nil
}

func validateCommandIndexUpdate(existing, updated Command) error {
	if err := validateCommandIndexImmutableEnvelope(existing, updated); err != nil {
		return err
	}
	if updated.Order.Revision <= existing.Order.Revision {
		return fmt.Errorf("command %q record revision %d does not advance prior revision %d", existing.ID, updated.Order.Revision, existing.Order.Revision)
	}
	if err := validateCommandIndexEvidenceTransition(existing, updated); err != nil {
		return err
	}
	return validateCommandIndexTransition(existing, updated)
}

func validateCommandIndexEvidenceTransition(existing, updated Command) error {
	switch {
	case existing.Binding != nil && updated.Binding == nil:
		return fmt.Errorf("command %q cleared its durable launch binding", existing.ID)
	case existing.Binding != nil && updated.Binding != nil && *existing.Binding != *updated.Binding:
		return fmt.Errorf("command %q changed its durable launch binding", existing.ID)
	case existing.Binding == nil && updated.Binding != nil && updated.Retry == nil:
		return fmt.Errorf("command %q acquired a launch binding without attempt evidence", existing.ID)
	}
	if existing.Claim != nil && updated.Claim != nil {
		if existing.Claim.ID != updated.Claim.ID ||
			existing.Claim.OwnerID != updated.Claim.OwnerID ||
			existing.Claim.ClaimedAt != updated.Claim.ClaimedAt {
			return fmt.Errorf("command %q rewrote its active claim identity or owner", existing.ID)
		}
		if updated.Claim.LeaseUntil.Before(existing.Claim.LeaseUntil) {
			return fmt.Errorf("command %q rewound its active claim lease", existing.ID)
		}
	}

	if existing.Retry == nil {
		if updated.Retry == nil {
			return nil
		}
		if existing.State != CommandStatePending || updated.State != CommandStateInFlight {
			return fmt.Errorf("command %q introduced attempt evidence outside pending -> in_flight", existing.ID)
		}
		if updated.Retry.AttemptCount != 1 {
			return fmt.Errorf("command %q first attempt count is %d, want 1", existing.ID, updated.Retry.AttemptCount)
		}
		return nil
	}
	if updated.Retry == nil {
		return fmt.Errorf("command %q cleared its last-attempt evidence", existing.ID)
	}
	if updated.Retry.AttemptCount < existing.Retry.AttemptCount || updated.Retry.AttemptCount > existing.Retry.AttemptCount+1 {
		return fmt.Errorf(
			"command %q attempt count changed from %d to %d without one contiguous attempt",
			existing.ID,
			existing.Retry.AttemptCount,
			updated.Retry.AttemptCount,
		)
	}
	if updated.Retry.AttemptCount == existing.Retry.AttemptCount {
		if !sameCommandIndexAttempt(*existing.Retry, *updated.Retry) {
			return fmt.Errorf("command %q rewrote its current attempt identity or authorization evidence", existing.ID)
		}
		return nil
	}
	if existing.State != CommandStatePending || updated.State != CommandStateInFlight {
		return fmt.Errorf("command %q advanced attempt count outside pending -> in_flight", existing.ID)
	}
	if !updated.Retry.LastAttemptAt.After(existing.Retry.LastAttemptAt) {
		return fmt.Errorf("command %q advanced attempt count without advancing attempt time", existing.ID)
	}
	return nil
}

func sameCommandIndexAttempt(existing, updated CommandRetry) bool {
	return existing.LastAttemptAt.Equal(updated.LastAttemptAt) &&
		existing.ClaimID == updated.ClaimID &&
		existing.OperationID == updated.OperationID &&
		existing.AttemptID == updated.AttemptID &&
		existing.BoundLaunchIdentity == updated.BoundLaunchIdentity &&
		existing.AuthorizationDecisionID == updated.AuthorizationDecisionID &&
		existing.AuthorizationPolicyVersion == updated.AuthorizationPolicyVersion
}

func validateCommandIndexImmutableEnvelope(existing, updated Command) error {
	if existing.Version != updated.Version {
		return fmt.Errorf("command %q changed immutable version", existing.ID)
	}
	if existing.ID != updated.ID {
		return fmt.Errorf("command %q changed immutable id to %q", existing.ID, updated.ID)
	}
	if existing.Mode != updated.Mode {
		return fmt.Errorf("command %q changed immutable delivery mode", existing.ID)
	}
	if existing.Target != updated.Target {
		return fmt.Errorf("command %q changed immutable target", existing.ID)
	}
	if existing.Store != updated.Store {
		return fmt.Errorf("command %q changed immutable store binding", existing.ID)
	}
	if existing.Order.Sequence != updated.Order.Sequence {
		return fmt.Errorf("command %q changed immutable sequence from %d to %d", existing.ID, existing.Order.Sequence, updated.Order.Sequence)
	}
	if existing.TrustedIngress != updated.TrustedIngress {
		return fmt.Errorf("command %q changed immutable trusted-ingress evidence", existing.ID)
	}
	if existing.Source != updated.Source {
		return fmt.Errorf("command %q changed immutable source", existing.ID)
	}
	if existing.Message != updated.Message {
		return fmt.Errorf("command %q changed immutable message", existing.ID)
	}
	if !equalCommandIndexReference(existing.Reference, updated.Reference) {
		return fmt.Errorf("command %q changed immutable reference", existing.ID)
	}
	if existing.CreatedAt != updated.CreatedAt || existing.DeliverAfter != updated.DeliverAfter || existing.ExpiresAt != updated.ExpiresAt {
		return fmt.Errorf("command %q changed immutable delivery window", existing.ID)
	}
	return nil
}

func equalCommandIndexReference(left, right *Reference) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validateCommandIndexTransition(existing, updated Command) error {
	switch existing.State {
	case CommandStatePending:
		switch updated.State {
		case CommandStatePending, CommandStateInFlight:
			return nil
		case CommandStateExpired, CommandStateSuperseded, CommandStateDeadLettered:
			if updated.Terminal != nil {
				if updated.Terminal.ProviderStage == ProviderStageNotEntered {
					return nil
				}
				if existing.Retry != nil &&
					updated.State == CommandStateDeadLettered &&
					updated.Terminal.ActionResult == CommandActionResultRetryExhausted &&
					updated.Terminal.ProviderStage == ProviderStageRejected {
					return nil
				}
			}
		}
	case CommandStateInFlight:
		if updated.State == CommandStatePending || updated.State == CommandStateInFlight || commandIsTerminalState(updated.State) {
			return nil
		}
	case CommandStateUpgradeRequired:
		if updated.State == CommandStateUpgradeRequired {
			return nil
		}
	default:
		if commandIsTerminalState(existing.State) {
			return fmt.Errorf("terminal command %q cannot change without a tombstone", existing.ID)
		}
	}
	return fmt.Errorf("command %q lifecycle transition %q -> %q is forbidden", existing.ID, existing.State, updated.State)
}

func commandIsTerminalState(state CommandState) bool {
	switch state {
	case CommandStateDelivered,
		CommandStateInjectedUnconfirmed,
		CommandStateDeliveryUnknown,
		CommandStateExpired,
		CommandStateSuperseded,
		CommandStateDeadLettered:
		return true
	default:
		return false
	}
}

func (idx *CommandIndex) applyTombstoneLocked(tombstone CommandIndexTombstone) error {
	if prior, exists := idx.tombstones[tombstone.CommandID]; exists {
		return fmt.Errorf("command %q already has tombstone evidence at revision %d", tombstone.CommandID, prior.Revision)
	}
	if existing, ok := idx.entries[tombstone.CommandID]; ok {
		if existing.Opaque != nil {
			return fmt.Errorf("tombstone for opaque command %q is forbidden to this owner", tombstone.CommandID)
		}
		command := *existing.Command
		if !commandIsTerminalState(command.State) {
			return fmt.Errorf("tombstone for command %q would erase active state %q", command.ID, command.State)
		}
		if !commandIndexTombstoneMatchesCommand(tombstone, command) {
			return fmt.Errorf("tombstone for command %q does not match its terminal record", command.ID)
		}
		delete(idx.entries, tombstone.CommandID)
	} else if tombstone.Sequence != idx.sequenceHighWater+1 {
		return fmt.Errorf("unknown command tombstone %q sequence %d does not densely advance high-water %d", tombstone.CommandID, tombstone.Sequence, idx.sequenceHighWater)
	}
	idx.sequenceHighWater = max(idx.sequenceHighWater, tombstone.Sequence)
	idx.tombstones[tombstone.CommandID] = tombstone
	return nil
}

func commandIndexTombstoneMatchesCommand(tombstone CommandIndexTombstone, command Command) bool {
	return tombstone.CommandID == command.ID &&
		tombstone.Store == command.Store &&
		tombstone.PriorVersion == command.Version &&
		tombstone.PriorRevision == command.Order.Revision &&
		tombstone.PriorState == command.State &&
		tombstone.TargetSessionID == command.Target.SessionID &&
		tombstone.IntentGeneration == command.Target.IntentGeneration &&
		tombstone.Sequence == command.Order.Sequence
}

func (idx *CommandIndex) insertOrderedCommandLocked(sessionID string, ref commandIndexOrderRef) {
	ref.sessionID = sessionID
	idx.ordered.ReplaceOrInsert(ref)
}

func (idx *CommandIndex) removeOrderedCommandLocked(sessionID string, sequence uint64, commandID string) {
	idx.ordered.Delete(commandIndexOrderRef{sessionID: sessionID, sequence: sequence, id: commandID})
}

func (idx *CommandIndex) markUnsyncedLocked(reason string) error {
	idx.synced = false
	idx.unsyncedReason = reason
	return fmt.Errorf("%w: %s", ErrCommandIndexUnsynced, reason)
}

// Resolve returns an owned record and the exact projection watermark that
// produced it. An unsynced index never serves a normal resolution.
func (idx *CommandIndex) Resolve(commandID string) (CommandIndexResolution, error) {
	if idx == nil {
		return CommandIndexResolution{}, errors.New("resolving nudge command index: index is nil")
	}
	if err := validateCommandIdentity("command id", commandID); err != nil {
		return CommandIndexResolution{}, fmt.Errorf("resolving nudge command index: %w", err)
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if !idx.synced {
		return CommandIndexResolution{}, fmt.Errorf("%w: %s", ErrCommandIndexUnsynced, idx.unsyncedReason)
	}
	result := CommandIndexResolution{
		Store:                  idx.store,
		Revision:               idx.revision,
		CompletedAuditRevision: idx.completedAuditRevision,
	}
	entry, ok := idx.entries[commandID]
	if !ok {
		return result, nil
	}
	result.Entry = cloneCommandIndexEntry(entry)
	result.Found = true
	return result, nil
}

func (idx *CommandIndex) diagnosticResolve(commandID string) (Command, bool) {
	if idx == nil {
		return Command{}, false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	entry, ok := idx.entries[commandID]
	if !ok || entry.Command == nil {
		return Command{}, false
	}
	return cloneIndexedCommand(*entry.Command), true
}

// Page returns entries in the session's active ordering domain with sequence
// strictly greater than afterSequence. Every successful call copies at most
// limit entries and limit must be explicitly within the hard bound. An upgrade
// barrier is returned as the last entry in a page and disables continuation.
func (idx *CommandIndex) Page(sessionID string, afterSequence uint64, limit int) (CommandIndexPage, error) {
	if idx == nil {
		return CommandIndexPage{}, errors.New("paging nudge command index: index is nil")
	}
	if err := validateCommandIdentity("session id", sessionID); err != nil {
		return CommandIndexPage{}, fmt.Errorf("paging nudge command index: %w", err)
	}
	if limit < 1 || limit > MaxCommandIndexPageSize {
		return CommandIndexPage{}, fmt.Errorf("paging nudge command index: limit %d is outside [1,%d]", limit, MaxCommandIndexPageSize)
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if !idx.synced {
		return CommandIndexPage{}, fmt.Errorf("%w: %s", ErrCommandIndexUnsynced, idx.unsyncedReason)
	}
	upgradeBarrier, hasUpgradeBarrier := idx.firstUpgradeBarrierRefLocked(sessionID)
	if hasUpgradeBarrier && upgradeBarrier.sequence <= afterSequence {
		return CommandIndexPage{}, fmt.Errorf("%w at sequence %d", ErrCommandIndexUpgradeBarrier, upgradeBarrier.sequence)
	}
	refs := make([]commandIndexOrderRef, 0, limit+1)
	pivot := commandIndexOrderRef{sessionID: sessionID, sequence: afterSequence}
	idx.ordered.AscendGreaterOrEqual(pivot, func(ref commandIndexOrderRef) bool {
		if ref.sessionID != sessionID {
			return false
		}
		if ref.sequence <= afterSequence {
			return true
		}
		refs = append(refs, ref)
		if hasUpgradeBarrier && ref.sequence == upgradeBarrier.sequence && ref.id == upgradeBarrier.id {
			return false
		}
		return len(refs) <= limit
	})
	resultCount := min(len(refs), limit)
	page := CommandIndexPage{
		Store:                  idx.store,
		Revision:               idx.revision,
		CompletedAuditRevision: idx.completedAuditRevision,
		Entries:                make([]CommandIndexEntry, 0, resultCount),
	}
	for _, ref := range refs[:resultCount] {
		page.Entries = append(page.Entries, cloneCommandIndexEntry(idx.entries[ref.id]))
	}
	resultEndsAtBarrier := resultCount > 0 && hasUpgradeBarrier && refs[resultCount-1] == upgradeBarrier
	if len(refs) > limit && resultCount > 0 && !resultEndsAtBarrier {
		page.NextAfterSequence = refs[resultCount-1].sequence
	}
	return page, nil
}

func (idx *CommandIndex) firstUpgradeBarrierRefLocked(sessionID string) (commandIndexOrderRef, bool) {
	var result commandIndexOrderRef
	found := false
	idx.barrierOrdered.AscendGreaterOrEqual(commandIndexOrderRef{sessionID: sessionID}, func(ref commandIndexOrderRef) bool {
		if ref.sessionID != sessionID {
			return false
		}
		result = ref
		found = true
		return false
	})
	return result, found
}

// Status returns a value snapshot of the index's synchronization state.
func (idx *CommandIndex) Status() CommandIndexStatus {
	if idx == nil {
		return CommandIndexStatus{}
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return CommandIndexStatus{
		Store:                  idx.store,
		Revision:               idx.revision,
		SequenceHighWater:      idx.sequenceHighWater,
		CompletedAuditRevision: idx.completedAuditRevision,
		Synced:                 idx.synced,
		UnsyncedReason:         idx.unsyncedReason,
	}
}

// CompleteAudit installs an independently rebuilt snapshot only if no
// mutation advanced the index after expectedRevision was observed. Invalid or
// rewound snapshots never replace the current projection.
func (idx *CommandIndex) CompleteAudit(expectedRevision uint64, snapshot CommandIndexSnapshot) (bool, error) {
	if idx == nil {
		return false, errors.New("completing nudge command index audit: index is nil")
	}
	if snapshot.Revision < expectedRevision {
		return false, fmt.Errorf("completing nudge command index audit: snapshot revision %d rewinds expected revision %d", snapshot.Revision, expectedRevision)
	}
	projection, err := buildCommandIndexProjection(snapshot)
	if err != nil {
		return false, fmt.Errorf("completing nudge command index audit: %w", err)
	}
	sparsePartition := snapshot.partitionWitness != ([sha256.Size]byte{})

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.revision != expectedRevision {
		return false, nil
	}
	if snapshot.Store != idx.store {
		return false, fmt.Errorf("completing nudge command index audit: snapshot store binding %#v does not match index binding %#v", snapshot.Store, idx.store)
	}
	if snapshot.Revision < idx.revision {
		return false, fmt.Errorf("completing nudge command index audit: snapshot revision %d rewinds current revision %d", snapshot.Revision, idx.revision)
	}
	if snapshot.SequenceHighWater < idx.sequenceHighWater {
		return false, fmt.Errorf("completing nudge command index audit: snapshot sequence high-water %d rewinds current high-water %d", snapshot.SequenceHighWater, idx.sequenceHighWater)
	}
	for commandID, currentTombstone := range idx.tombstones {
		replacement, retained := projection.tombstones[commandID]
		if !retained && commandIndexCoverageContainsSequence(projection.coverage, currentTombstone.Sequence) {
			continue
		}
		if !retained {
			return false, fmt.Errorf("completing nudge command index audit: snapshot dropped tombstone %q", commandID)
		}
		if replacement != currentTombstone {
			return false, fmt.Errorf("completing nudge command index audit: snapshot rewrote tombstone %q", commandID)
		}
	}
	for commandID, current := range idx.entries {
		if replacement, retained := projection.entries[commandID]; retained {
			if err := validateCommandIndexEntryAuditReplacement(current, replacement); err != nil {
				return false, fmt.Errorf("completing nudge command index audit: %w", err)
			}
			continue
		}
		if tombstone, tombstoned := projection.tombstones[commandID]; tombstoned {
			if current.Opaque != nil || !commandIsTerminalState(current.Command.State) || !commandIndexTombstoneMatchesCommand(tombstone, *current.Command) {
				return false, fmt.Errorf("completing nudge command index audit: tombstone for command %q does not prove deletion of its current state", commandID)
			}
			continue
		}
		if current.Command != nil && commandIsTerminalState(current.Command.State) && commandIndexCoverageContainsSequence(projection.coverage, current.Command.Order.Sequence) {
			continue
		}
		if sparsePartition {
			continue
		}
		return false, fmt.Errorf("completing nudge command index audit: command %q disappeared without a tombstone", commandID)
	}
	if idx.coverage != nil {
		if projection.coverage == nil || projection.coverage.PublishedRevision < idx.coverage.PublishedRevision {
			return false, errors.New("completing nudge command index audit: snapshot dropped or rewound compacted coverage")
		}
		for _, sequenceRange := range idx.coverage.Ranges {
			if !commandIndexCoverageContainsRange(projection.coverage, sequenceRange) {
				return false, fmt.Errorf("completing nudge command index audit: snapshot resurrected compacted range [%d,%d]", sequenceRange.FirstSequence, sequenceRange.LastSequence)
			}
		}
	}

	idx.entries = projection.entries
	idx.ordered = projection.ordered
	idx.barrierOrdered = projection.barrierOrdered
	idx.tombstones = projection.tombstones
	idx.replays = projection.replays
	idx.coverage = cloneCommandIndexCoverage(projection.coverage)
	idx.revision = snapshot.Revision
	idx.sequenceHighWater = snapshot.SequenceHighWater
	idx.completedAuditRevision = snapshot.Revision
	idx.synced = true
	idx.unsyncedReason = ""
	return true, nil
}

func validateCommandIndexAuditReplacement(current, replacement Command) error {
	if err := validateCommandIndexImmutableEnvelope(current, replacement); err != nil {
		return err
	}
	if replacement.Order.Revision < current.Order.Revision {
		return fmt.Errorf("command %q record revision %d rewinds current record revision %d", current.ID, replacement.Order.Revision, current.Order.Revision)
	}
	if replacement.Order.Revision == current.Order.Revision {
		if !equalIndexedCommands(current, replacement) {
			return fmt.Errorf("command %q changed without advancing its record revision", current.ID)
		}
		return nil
	}
	if err := validateCommandIndexEvidenceTransition(current, replacement); err != nil {
		return err
	}
	return validateCommandIndexTransition(current, replacement)
}

func validateCommandIndexEntryAuditReplacement(current, replacement CommandIndexEntry) error {
	switch {
	case current.Command != nil && replacement.Command != nil:
		return validateCommandIndexAuditReplacement(*current.Command, *replacement.Command)
	case current.Opaque != nil && replacement.Opaque != nil:
		if replacement.Opaque.Routing.Revision < current.Opaque.Routing.Revision {
			return fmt.Errorf("command %q record revision %d rewinds current record revision %d", current.Opaque.Routing.CommandID, replacement.Opaque.Routing.Revision, current.Opaque.Routing.Revision)
		}
		if replacement.Opaque.Routing.Revision == current.Opaque.Routing.Revision {
			if !equalCommandIndexEntries(current, replacement) {
				return fmt.Errorf("command %q changed without advancing its record revision", current.Opaque.Routing.CommandID)
			}
			return nil
		}
		return validateOpaqueCommandIndexUpdate(*current.Opaque, *replacement.Opaque)
	default:
		routing := commandIndexEntryRouting(current)
		return fmt.Errorf("command %q changed known/opaque representation", routing.CommandID)
	}
}

func equalIndexedCommands(left, right Command) bool {
	leftEntry := CommandIndexEntry{Command: &left}
	rightEntry := CommandIndexEntry{Command: &right}
	return equalCommandIndexEntries(leftEntry, rightEntry)
}

func equalCommandIndexEntries(left, right CommandIndexEntry) bool {
	leftRouting := commandIndexEntryRouting(left)
	rightRouting := commandIndexEntryRouting(right)
	leftMutation := CommandIndexMutation{Store: leftRouting.Store, Revision: leftRouting.Revision, Entry: &left}
	rightMutation := CommandIndexMutation{Store: rightRouting.Store, Revision: rightRouting.Revision, Entry: &right}
	leftDigest, leftErr := commandIndexMutationDigest(leftMutation)
	rightDigest, rightErr := commandIndexMutationDigest(rightMutation)
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}

func cloneCommandIndexEntry(entry CommandIndexEntry) CommandIndexEntry {
	if entry.Command != nil {
		command := cloneIndexedCommand(*entry.Command)
		entry.Command = &command
	}
	if entry.Opaque != nil {
		opaque := *entry.Opaque
		opaque.Raw = append([]byte(nil), opaque.Raw...)
		entry.Opaque = &opaque
	}
	return entry
}

func cloneIndexedCommand(command Command) Command {
	if command.Binding != nil {
		binding := *command.Binding
		command.Binding = &binding
	}
	if command.Reference != nil {
		reference := *command.Reference
		command.Reference = &reference
	}
	if command.Retry != nil {
		retry := *command.Retry
		if retry.NextEligibleAt != nil {
			nextEligibleAt := *retry.NextEligibleAt
			retry.NextEligibleAt = &nextEligibleAt
		}
		command.Retry = &retry
	}
	if command.Claim != nil {
		claim := *command.Claim
		command.Claim = &claim
	}
	if command.Terminal != nil {
		terminal := *command.Terminal
		command.Terminal = &terminal
	}
	return command
}
