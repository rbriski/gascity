package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestOpenProductionNudgeCommandSourceProvisionsButRefusesUnverifiedCityPartition(t *testing.T) {
	store := newNudgeCommandSourceAtomicStore()
	cityPath := t.TempDir()

	first, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, nudgequeue.TrustedCityPartition{}, nil)
	if first != nil || !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		t.Fatalf("first open = %T, err=%v; want unverified partition refusal", first, err)
	}
	if writes := store.metadataWriteCount(); writes != 7 {
		t.Fatalf("initial metadata writes = %d, want 7", writes)
	}
	if _, exists, err := nudgequeue.LoadRestoreAnchor(t.Context(), nudgequeue.RestoreAnchorPath(cityPath)); err != nil || !exists {
		t.Fatalf("independent restore anchor after first open: exists=%t err=%v", exists, err)
	}

	second, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, nudgequeue.TrustedCityPartition{}, nil)
	if second != nil || !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		t.Fatalf("second open = %T, err=%v; want stable unverified partition refusal", second, err)
	}
	if writes := store.metadataWriteCount(); writes != 7 {
		t.Fatalf("unverified reopen metadata writes = %d, want 7", writes)
	}
}

func TestOpenProductionNudgeCommandSourceLeavesUnsupportedStoreLegacyOnly(t *testing.T) {
	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), t.TempDir(), beads.NewMemStore(), nudgequeue.TrustedCityPartition{}, nil)
	if source != nil {
		t.Fatalf("unsupported source = %T, want nil", source)
	}
	if !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryUnsupported) {
		t.Fatalf("unsupported error = %v, want unverified + repository unsupported", err)
	}
}

func TestOpenProductionNudgeCommandSourceWrapsKnownTransientProvisionFailure(t *testing.T) {
	store := newNudgeCommandSourceAtomicStore()
	store.failNext = context.DeadlineExceeded

	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), t.TempDir(), store, nudgequeue.TrustedCityPartition{}, nil)
	if source != nil {
		t.Fatalf("transient source = %T, want nil until retry", source)
	}
	var failure nudgeCommandSourceFailure
	if !errors.As(err, &failure) || failure.class != nudgeCommandSourceErrorTransient || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transient open error = %#v (%v), want retryable deadline failure", failure, err)
	}
}

func TestOpenProductionNudgeCommandSourceCatchesUpTerminalCheckpointBeforeSnapshot(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	claimed := fixture.claimDirect(t, "claim-before-reopen", "attempt-before-reopen")
	completed, err := fixture.repository.CompleteProviderAttempt(t.Context(), deliveredNudgeCompletion(claimed, fixture.now.Add(3*time.Second)))
	if err != nil {
		t.Fatalf("CompleteProviderAttempt: %v", err)
	}
	if err := fixture.authority.RecordCommandPartitionTerminal(t.Context(), nudgequeue.CommandPartitionTerminal{
		Store: completed.Command.Store, RepositoryRevision: completed.Command.Order.Revision,
		CommandID: completed.Command.ID, Sequence: completed.Command.Order.Sequence, Partition: fixture.partition,
	}); err != nil {
		t.Fatalf("RecordCommandPartitionTerminal: %v", err)
	}
	if _, err := fixture.repository.Snapshot(t.Context(), 1); !errors.Is(err, nudgequeue.ErrCommandRepositoryCheckpointRequired) {
		t.Fatalf("Snapshot before reopen error = %v, want checkpoint-required", err)
	}

	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	snapshot, err := source.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot after opener checkpoint catch-up: %v", err)
	}
	if len(snapshot.Entries) != 0 || snapshot.Coverage == nil || snapshot.Coverage.TerminalCount != 1 {
		t.Fatalf("snapshot after checkpoint catch-up = %#v, want one compacted terminal", snapshot)
	}
}

func TestProductionNudgeCommandSourceInjectsBoundPartitionAndMaintainsCheckpoint(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source, ok := opened.(*productionNudgeCommandSource)
	if !ok {
		t.Fatalf("opened source = %T, want *productionNudgeCommandSource", opened)
	}
	claimRequest := nudgeEffectClaimRequest{
		commandID:           fixture.command.ID,
		claimID:             "claim-through-bound-source",
		ownerID:             "owner-through-bound-source",
		attemptID:           "attempt-through-bound-source",
		boundLaunchIdentity: "production-launch",
		claimedAt:           fixture.now.Add(2 * time.Second),
		leaseUntil:          fixture.now.Add(time.Minute),
	}
	claim, err := source.ClaimAuthorized(t.Context(), claimRequest, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	if got := fixture.authority.lastClaimPartition(); got != fixture.partition {
		t.Fatalf("claim partition = %#v, want opener-bound partition", got)
	}
	if _, err := source.CompleteProviderAttempt(t.Context(), deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second))); err != nil {
		t.Fatalf("CompleteProviderAttempt: %v", err)
	}
	if _, err := source.Snapshot(t.Context(), 1); err != nil {
		t.Fatalf("Snapshot after bounded checkpoint maintenance: %v", err)
	}
}

func TestProductionNudgeCommandSourceClaimTerminalMaintainsCheckpoint(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claimedAt := fixture.now.Add(2 * time.Hour)
	result, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-expired-through-source",
		ownerID: "owner-expired-through-source", attemptID: "attempt-expired-through-source",
		boundLaunchIdentity: "production-launch", claimedAt: claimedAt, leaseUntil: claimedAt.Add(time.Minute),
	}, fixture.authority)
	if err != nil || result.Disposition != nudgequeue.CommandClaimDenied || result.Command.Terminal == nil {
		t.Fatalf("ClaimAuthorized expired = %#v, err=%v", result, err)
	}
	snapshot, err := source.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot after claim-time terminal: %v", err)
	}
	if len(snapshot.Entries) != 0 || snapshot.Coverage == nil || snapshot.Coverage.TerminalCount != 1 {
		t.Fatalf("snapshot after claim-time terminal = %#v, want one compacted terminal", snapshot)
	}
}

func TestProductionNudgeCommandSourceSeparatesReadCapabilityFromCityBoundWrites(t *testing.T) {
	typ := reflect.TypeOf(productionNudgeCommandSource{})
	for name, want := range map[string]reflect.Type{
		"repository": reflect.TypeOf((*nudgequeue.CommandRepository)(nil)),
		"reader":     reflect.TypeOf((*nudgequeue.CommandPartitionReader)(nil)),
		"partition":  reflect.TypeOf(nudgequeue.TrustedCityPartition{}),
	} {
		field, ok := typ.FieldByName(name)
		if !ok || field.Type != want {
			t.Errorf("production source %s field = %#v, want %v", name, field, want)
		}
	}
}

func TestProductionNudgeCommandSourceClassifiesOnlyKnownRetryableFailures(t *testing.T) {
	source := &productionNudgeCommandSource{}
	for _, err := range []error{context.DeadlineExceeded, nudgequeue.ErrRestoreAnchorBusy, nudgequeue.ErrRestoreAnchorConflict, nudgequeue.ErrRestoreAnchorDurabilityUncertain} {
		if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorTransient {
			t.Errorf("ClassifyNudgeCommandSourceError(%v) = %d, want transient", err, got)
		}
	}
	for _, err := range []error{errors.New("unknown"), nudgequeue.ErrCommandRepositoryLineage, nudgequeue.ErrCommandRepositorySchemaSkew, nudgequeue.ErrCommandRepositoryRecord} {
		if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorInvariant {
			t.Errorf("ClassifyNudgeCommandSourceError(%v) = %d, want invariant", err, got)
		}
	}
}

type productionNudgeCommandFixture struct {
	cityPath   string
	store      *nudgeCommandSourceAtomicStore
	repository *nudgequeue.CommandRepository
	authority  *productionNudgeTestAuthority
	ingress    *nudgequeue.TrustedNudgeIngress
	partition  nudgequeue.TrustedCityPartition
	command    nudgequeue.Command
	now        time.Time
}

func newProductionNudgeCommandFixture(t *testing.T) productionNudgeCommandFixture {
	t.Helper()
	cityPath := t.TempDir()
	store := newNudgeCommandSourceAtomicStore()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(cityPath))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	authority := &productionNudgeTestAuthority{
		references: make(map[string]nudgequeue.NudgeAuthorization),
		admissions: make(map[string]nudgequeue.CommandPartitionAdmission),
		terminals:  make(map[string]nudgequeue.CommandPartitionTerminal),
	}
	ingress, err := nudgequeue.NewTrustedNudgeIngress(repository, authority)
	if err != nil {
		t.Fatalf("NewTrustedNudgeIngress: %v", err)
	}
	now := time.Now().UTC()
	admitted, err := ingress.Admit(t.Context(), nudgequeue.NudgeIngressRequest{
		RequestID: "production-source-request",
		Mode:      nudgequeue.DeliveryModeQueue,
		Target: nudgequeue.CommandTarget{
			SessionID:            "production-session",
			IntentGeneration:     1,
			ContinuationIdentity: "production-continuation",
			Policy:               nudgequeue.TargetPolicyContinuation,
		},
		Source:       nudgequeue.CommandSourceSession,
		Message:      "production adapter proof",
		DeliverAfter: now.Add(time.Second),
		ExpiresAt:    now.Add(time.Hour),
	})
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	return productionNudgeCommandFixture{
		cityPath: cityPath, store: store, repository: repository, authority: authority,
		ingress: ingress, partition: admitted.Partition, command: *admitted.Entry.Command, now: now,
	}
}

func (f productionNudgeCommandFixture) claimDirect(t *testing.T, claimID, attemptID string) nudgequeue.Command {
	t.Helper()
	result, err := f.repository.ClaimAuthorized(t.Context(), nudgequeue.CommandClaimRequest{
		CommandID: f.command.ID, ClaimID: claimID, OwnerID: "direct-owner", AttemptID: attemptID,
		BoundLaunchIdentity: "production-launch", Partition: f.partition,
		ClaimedAt: f.now.Add(2 * time.Second), LeaseUntil: f.now.Add(time.Minute),
	}, f.authority)
	if err != nil || result.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", result, err)
	}
	return result.Command
}

func deliveredNudgeCompletion(command nudgequeue.Command, completedAt time.Time) nudgequeue.CommandCompletionRequest {
	return nudgequeue.CommandCompletionRequest{
		CommandID: command.ID, ClaimID: command.Claim.ID, OperationID: command.Claim.OperationID,
		AttemptID: command.Claim.AttemptID, CompletedAt: completedAt,
		ActionResult:  nudgequeue.CommandActionResultDelivered,
		ProviderStage: nudgequeue.ProviderStageAccepted, Completion: nudgequeue.CompletionStateCompleted,
	}
}

type productionNudgeTestAuthority struct {
	mu         sync.Mutex
	references map[string]nudgequeue.NudgeAuthorization
	partition  nudgequeue.TrustedCityPartition
	admissions map[string]nudgequeue.CommandPartitionAdmission
	terminals  map[string]nudgequeue.CommandPartitionTerminal
}

func (a *productionNudgeTestAuthority) AuthorizeNudgeIngress(_ context.Context, request nudgequeue.NudgeIngressAuthorizationRequest) (nudgequeue.NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	authorization := nudgequeue.NudgeAuthorization{
		Disposition: nudgequeue.NudgeAuthorizationAllowed, PrincipalSchemaVersion: nudgequeue.NudgePrincipalSchemaVersion,
		Reference: nudgequeue.TrustedIngressReference{
			Issuer: "production-test-authority", ReferenceID: "authority/" + request.RequestID,
			PrincipalID: "principal-1", TenantScope: "tenant-1", CityScope: "city-1",
			CredentialClass: "controller", PolicyVersion: "policy-v1", PolicyDecisionID: "ingress-decision-1",
			Action: request.Action, TargetSessionID: request.Target.SessionID, PayloadDigest: request.PayloadDigest,
			IssuedAt: request.RequestedAt.Add(-time.Second), ExpiresAt: request.RequestedAt.Add(time.Hour),
		},
	}
	a.references[authorization.Reference.ReferenceID] = authorization
	return authorization, nil
}

func (a *productionNudgeTestAuthority) ResolveTrustedNudgeIngress(_ context.Context, reference nudgequeue.TrustedIngressReference) (nudgequeue.NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.references[reference.ReferenceID], nil
}

func (a *productionNudgeTestAuthority) RecordCommandPartitionAdmission(_ context.Context, admission nudgequeue.CommandPartitionAdmission) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, found := a.admissions[admission.CommandID]; found && existing != admission {
		return errors.New("conflicting production-test partition admission")
	}
	a.admissions[admission.CommandID] = admission
	return nil
}

func (a *productionNudgeTestAuthority) RecordCommandPartitionTerminal(_ context.Context, terminal nudgequeue.CommandPartitionTerminal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	admission, found := a.admissions[terminal.CommandID]
	if !found || admission.Store != terminal.Store || admission.Sequence != terminal.Sequence || admission.Partition != terminal.Partition {
		return errors.New("terminal has no matching production-test partition admission")
	}
	if existing, found := a.terminals[terminal.CommandID]; found && existing != terminal {
		return errors.New("conflicting production-test partition terminal")
	}
	a.terminals[terminal.CommandID] = terminal
	return nil
}

func (a *productionNudgeTestAuthority) ResolveCommandPartitionCoverage(_ context.Context, request nudgequeue.CommandPartitionCoverageRequest) (nudgequeue.CommandPartitionCoverage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var (
		highWater         uint64
		admittedCount     uint64
		terminalSequences []uint64
		active            []nudgequeue.CommandPartitionCoverageEntry
	)
	for id, admission := range a.admissions {
		if admission.Store != request.Store || admission.RepositoryRevision > request.RepositoryRevision {
			continue
		}
		admittedCount++
		highWater = max(highWater, admission.Sequence)
		terminal, closed := a.terminals[id]
		if closed && terminal.RepositoryRevision <= request.RepositoryRevision {
			terminalSequences = append(terminalSequences, admission.Sequence)
			continue
		}
		if admission.Partition == request.Partition {
			active = append(active, nudgequeue.CommandPartitionCoverageEntry{CommandID: id, Sequence: admission.Sequence})
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Sequence < active[j].Sequence })
	sort.Slice(terminalSequences, func(i, j int) bool { return terminalSequences[i] < terminalSequences[j] })
	ranges := productionNudgeTerminalRanges(terminalSequences)
	if highWater != request.SequenceHighWater || admittedCount != request.SequenceHighWater || uint64(len(terminalSequences)) != request.TerminalCount || !reflect.DeepEqual(ranges, request.TerminalRanges) {
		return nudgequeue.CommandPartitionCoverage{}, errors.New("production-test authority coverage differs from repository snapshot")
	}
	return nudgequeue.CommandPartitionCoverage{
		Store: request.Store, RepositoryRevision: request.RepositoryRevision, SequenceHighWater: request.SequenceHighWater, AdmittedCount: admittedCount,
		TerminalRanges: append([]nudgequeue.CommandIndexSequenceRange(nil), request.TerminalRanges...),
		TerminalCount:  request.TerminalCount, Partition: request.Partition, ActiveEntries: active,
	}, nil
}

func (a *productionNudgeTestAuthority) ResolveCommandPartitionMembership(_ context.Context, request nudgequeue.CommandPartitionMembershipRequest) (nudgequeue.CommandPartitionMembership, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := nudgequeue.CommandPartitionMembership{
		Store: request.Store, RepositoryRevision: request.RepositoryRevision,
		CommandID: request.CommandID, Partition: request.Partition,
	}
	admission, found := a.admissions[request.CommandID]
	if !found || admission.Store != request.Store || admission.RepositoryRevision > request.RepositoryRevision || admission.Partition != request.Partition {
		return result, nil
	}
	result.Found = true
	result.Sequence = admission.Sequence
	terminal, closed := a.terminals[request.CommandID]
	result.Active = !closed || terminal.RepositoryRevision > request.RepositoryRevision
	return result, nil
}

func productionNudgeTerminalRanges(sequences []uint64) []nudgequeue.CommandIndexSequenceRange {
	var ranges []nudgequeue.CommandIndexSequenceRange
	for _, sequence := range sequences {
		last := len(ranges) - 1
		if last >= 0 && ranges[last].LastSequence+1 == sequence {
			ranges[last].LastSequence = sequence
			continue
		}
		ranges = append(ranges, nudgequeue.CommandIndexSequenceRange{FirstSequence: sequence, LastSequence: sequence})
	}
	return ranges
}

func (a *productionNudgeTestAuthority) AuthorizeNudgeClaim(_ context.Context, request nudgequeue.NudgeClaimAuthorizationRequest) (nudgequeue.NudgeClaimAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.partition = request.Partition
	return nudgequeue.NudgeClaimAuthorization{
		Disposition: nudgequeue.NudgeAuthorizationAllowed, PrincipalSchemaVersion: nudgequeue.NudgePrincipalSchemaVersion,
		DecisionID: "claim-decision-1", PolicyVersion: "policy-v2", Reference: request.Command.TrustedIngress,
	}, nil
}

func (a *productionNudgeTestAuthority) lastClaimPartition() nudgequeue.TrustedCityPartition {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.partition
}

type nudgeCommandSourceAtomicStore struct {
	beads.Store

	mu             sync.Mutex
	metadata       map[string]string
	rows           map[string]beads.Bead
	metadataWrites int
	failNext       error
}

func newNudgeCommandSourceAtomicStore() *nudgeCommandSourceAtomicStore {
	return &nudgeCommandSourceAtomicStore{
		Store:    beads.NewMemStore(),
		metadata: make(map[string]string),
		rows:     make(map[string]beads.Bead),
	}
}

func (s *nudgeCommandSourceAtomicStore) AtomicReadWrite(ctx context.Context, _ string, fn func(beads.AtomicReadWriteTx) error) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	tx := &nudgeCommandSourceAtomicTx{
		metadata:       cloneNudgeCommandSourceStrings(s.metadata),
		rows:           cloneNudgeCommandSourceRows(s.rows),
		metadataWrites: s.metadataWrites,
	}
	if err := fn(tx); err != nil {
		return err
	}
	s.metadata = tx.metadata
	s.rows = tx.rows
	s.metadataWrites = tx.metadataWrites
	return nil
}

func (s *nudgeCommandSourceAtomicStore) AtomicReadSnapshot(ctx context.Context, fn func(beads.AtomicReadSnapshotTx) error) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&nudgeCommandSourceAtomicTx{
		metadata: cloneNudgeCommandSourceStrings(s.metadata),
		rows:     cloneNudgeCommandSourceRows(s.rows),
	})
}

func (s *nudgeCommandSourceAtomicStore) metadataWriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadataWrites
}

type nudgeCommandSourceAtomicTx struct {
	metadata       map[string]string
	rows           map[string]beads.Bead
	metadataWrites int
}

func (tx *nudgeCommandSourceAtomicTx) GetIssue(id string) (beads.Bead, error) {
	row, ok := tx.rows[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return cloneNudgeCommandSourceRow(row), nil
}

func (tx *nudgeCommandSourceAtomicTx) ListHistory(query beads.AtomicReadWriteList) ([]beads.Bead, error) {
	ids := make(map[string]struct{}, len(query.IDs))
	for _, id := range query.IDs {
		ids[id] = struct{}{}
	}
	var rows []beads.Bead
	for _, row := range tx.rows {
		if len(ids) > 0 {
			if _, ok := ids[row.ID]; !ok {
				continue
			}
		}
		if query.IDPrefix != "" && !strings.HasPrefix(row.ID, query.IDPrefix) {
			continue
		}
		if query.IssueType != "" && row.Type != query.IssueType {
			continue
		}
		matches := true
		for key, value := range query.Metadata {
			if row.Metadata[key] != value {
				matches = false
				break
			}
		}
		if matches {
			rows = append(rows, cloneNudgeCommandSourceRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (tx *nudgeCommandSourceAtomicTx) ListHistoryPage(query beads.AtomicReadSnapshotPageQuery) (beads.AtomicReadSnapshotPage, error) {
	rows := make([]beads.Bead, 0, len(tx.rows))
	for _, row := range tx.rows {
		if row.Status != query.Status || !strings.HasPrefix(row.ID, query.IDPrefix) || query.Assignee != "" && row.Assignee != query.Assignee {
			continue
		}
		after := query.After == (beads.AtomicReadSnapshotCursor{})
		switch query.Order {
		case beads.AtomicReadSnapshotOrderID:
			after = after || row.ID > query.After.ID
		case beads.AtomicReadSnapshotOrderUpdatedAtID:
			after = after || row.UpdatedAt.After(query.After.UpdatedAt) ||
				(row.UpdatedAt.Equal(query.After.UpdatedAt) && row.ID > query.After.ID)
		default:
			return beads.AtomicReadSnapshotPage{}, errors.New("unsupported snapshot order")
		}
		if after {
			rows = append(rows, cloneNudgeCommandSourceRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if query.Order == beads.AtomicReadSnapshotOrderUpdatedAtID && !rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) {
			return rows[i].UpdatedAt.Before(rows[j].UpdatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	page := beads.AtomicReadSnapshotPage{}
	if len(rows) <= query.Limit {
		page.Rows = rows
		return page, nil
	}
	page.Rows = rows[:query.Limit]
	last := page.Rows[len(page.Rows)-1]
	page.Next.ID = last.ID
	if query.Order == beads.AtomicReadSnapshotOrderUpdatedAtID {
		page.Next.UpdatedAt = last.UpdatedAt
	}
	return page, nil
}

func (tx *nudgeCommandSourceAtomicTx) Create(row beads.Bead) (beads.Bead, error) {
	if _, exists := tx.rows[row.ID]; exists {
		return beads.Bead{}, errors.New("duplicate row")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	row.UpdatedAt = row.CreatedAt
	tx.rows[row.ID] = cloneNudgeCommandSourceRow(row)
	return cloneNudgeCommandSourceRow(row), nil
}

func (tx *nudgeCommandSourceAtomicTx) Update(id string, opts beads.UpdateOpts) error {
	row, ok := tx.rows[id]
	if !ok {
		return beads.ErrNotFound
	}
	if opts.Status != nil {
		row.Status = *opts.Status
	}
	if opts.Metadata != nil {
		if row.Metadata == nil {
			row.Metadata = make(map[string]string)
		}
		for key, value := range opts.Metadata {
			row.Metadata[key] = value
		}
	}
	row.UpdatedAt = time.Now().UTC()
	tx.rows[id] = row
	return nil
}

func (tx *nudgeCommandSourceAtomicTx) GetMetadata(key string) (string, error) {
	return tx.metadata[key], nil
}

func (tx *nudgeCommandSourceAtomicTx) SetMetadata(key, value string) error {
	tx.metadata[key] = value
	tx.metadataWrites++
	return nil
}

func cloneNudgeCommandSourceStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneNudgeCommandSourceRows(source map[string]beads.Bead) map[string]beads.Bead {
	result := make(map[string]beads.Bead, len(source))
	for id, row := range source {
		result[id] = cloneNudgeCommandSourceRow(row)
	}
	return result
}

func cloneNudgeCommandSourceRow(row beads.Bead) beads.Bead {
	row.Metadata = cloneNudgeCommandSourceStrings(row.Metadata)
	return row
}
