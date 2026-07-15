package nudgequeue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityPersistsAuthenticatedGrantAcrossReopen(t *testing.T) {
	cityPath := t.TempDir()
	state := localAuthorityRepositoryState()
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	request := localAuthorityIngressRequest()
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	first, err := authority.AuthorizeNudgeIngress(ctx, request)
	if err != nil || first.Disposition != NudgeAuthorizationAllowed {
		t.Fatalf("AuthorizeNudgeIngress = %#v, err=%v", first, err)
	}
	if first.Reference.PrincipalID != localAuthorityRequester().PrincipalID || first.Reference.CityScope != localAuthorityRequester().CityScope {
		t.Fatalf("persisted requester reference = %#v", first.Reference)
	}
	if !first.CommandCreatedAt.Equal(request.RequestedAt) {
		t.Fatalf("authorized command creation time = %v, want %v", first.CommandCreatedAt, request.RequestedAt)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close first authority: %v", err)
	}

	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("reopen local authority: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	resolved, err := reopened.ResolveTrustedNudgeIngress(t.Context(), first.Reference)
	if err != nil || resolved != first {
		t.Fatalf("ResolveTrustedNudgeIngress = %#v, err=%v; want %#v", resolved, err, first)
	}
	second, err := reopened.AuthorizeNudgeIngress(ctx, request)
	if err != nil || second != first {
		t.Fatalf("idempotent AuthorizeNudgeIngress = %#v, err=%v; want %#v", second, err, first)
	}

	info, err := os.Lstat(LocalNudgeAuthorityPath(cityPath))
	if err != nil {
		t.Fatalf("lstat local authority: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("authority file mode = %v, want regular file", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("authority permissions = %v, want 0600", info.Mode().Perm())
	}
}

func TestLocalNudgeAuthorityRejectsMissingOrMismatchedRequester(t *testing.T) {
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), localAuthorityRepositoryState(), localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	request := localAuthorityIngressRequest()

	for name, ctx := range map[string]context.Context{
		"missing":    t.Context(),
		"wrong city": WithAuthenticatedNudgeRequester(t.Context(), AuthenticatedNudgeRequester{PrincipalID: "principal-a", TenantScope: "tenant-a", CityScope: "city-b", CredentialClass: "city-write-grant", EvidenceID: "grant-b"}),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := authority.AuthorizeNudgeIngress(ctx, request)
			if err != nil || got.Disposition != NudgeAuthorizationDenied {
				t.Fatalf("AuthorizeNudgeIngress = %#v, err=%v; want denied", got, err)
			}
		})
	}
}

func TestLocalNudgeAuthorityMaintainsHistoricalMembershipAndTerminalDigest(t *testing.T) {
	state := localAuthorityRepositoryState()
	cityPath := t.TempDir()
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	request := localAuthorityIngressRequest()
	authorized, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), request)
	if err != nil || authorized.Disposition != NudgeAuthorizationAllowed {
		t.Fatalf("AuthorizeNudgeIngress = %#v, err=%v", authorized, err)
	}
	partition := trustedCityPartitionFromAuthority(authorized.Reference)
	commandID := CommandIDForRequest(state.Store, request.RequestID)
	admission := CommandPartitionAdmission{Store: state.Store, RepositoryRevision: 1, CommandID: commandID, Sequence: 1, Partition: partition}
	if err := authority.RecordCommandPartitionAdmission(t.Context(), admission); err != nil {
		t.Fatalf("RecordCommandPartitionAdmission: %v", err)
	}

	active, err := authority.ResolveCommandPartitionCoverage(t.Context(), CommandPartitionCoverageRequest{
		Store: state.Store, RepositoryRevision: 1, SequenceHighWater: 1, Partition: partition,
	})
	if err != nil || active.AdmittedCount != 1 || len(active.ActiveEntries) != 1 || active.ActiveEntries[0] != (CommandPartitionCoverageEntry{CommandID: commandID, Sequence: 1}) {
		t.Fatalf("active coverage = %#v, err=%v", active, err)
	}
	intent := CommandPartitionTerminalIntent{
		Store: state.Store, RepositoryBeforeRevision: 1, RepositoryRevision: 2,
		CommandID: commandID, Sequence: 1, Partition: partition,
		BeforeCommandDigest: [32]byte{1}, CommandDigest: [32]byte{2},
	}
	if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
	}
	resolution := CommandPartitionTerminalResolution{
		Store: state.Store, RepositoryRevision: 2, CommandID: commandID, Sequence: 1,
		Partition: partition, CommandDigest: intent.CommandDigest,
	}
	if err := authority.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
		t.Fatalf("VerifyCommandPartitionTerminal prepared: %v", err)
	}
	terminal := CommandPartitionTerminal{Store: state.Store, RepositoryRevision: 2, CommandID: commandID, Sequence: 1, Partition: partition}
	if err := authority.RecordCommandPartitionTerminal(t.Context(), terminal); err != nil {
		t.Fatalf("RecordCommandPartitionTerminal: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close first authority: %v", err)
	}
	if rewound, err := OpenLocalNudgeAuthority(t.Context(), cityPath, CommandRepositoryState{
		Store: state.Store, SchemaVersion: state.SchemaVersion, WriterVersion: state.WriterVersion, Revision: 1, SequenceHighWater: 1,
	}, localAuthorityOptions()); rewound != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		if rewound != nil {
			_ = rewound.Close()
		}
		t.Fatalf("reopen against same-epoch repository rewind = %v, err=%v; want conflict", rewound, err)
	}

	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, CommandRepositoryState{
		Store: state.Store, SchemaVersion: state.SchemaVersion, WriterVersion: state.WriterVersion, Revision: 2, SequenceHighWater: 1,
	}, localAuthorityOptions())
	if err != nil {
		t.Fatalf("reopen local authority: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
		t.Fatalf("VerifyCommandPartitionTerminal finalized after reopen: %v", err)
	}
	atAdmission, err := reopened.ResolveCommandPartitionCoverage(t.Context(), CommandPartitionCoverageRequest{
		Store: state.Store, RepositoryRevision: 1, SequenceHighWater: 1, Partition: partition,
	})
	if err != nil || len(atAdmission.ActiveEntries) != 1 {
		t.Fatalf("historical admission coverage = %#v, err=%v", atAdmission, err)
	}
	afterTerminal, err := reopened.ResolveCommandPartitionCoverage(t.Context(), CommandPartitionCoverageRequest{
		Store: state.Store, RepositoryRevision: 2, SequenceHighWater: 1, Partition: partition,
	})
	if err != nil || len(afterTerminal.ActiveEntries) != 0 {
		t.Fatalf("terminal coverage = %#v, err=%v", afterTerminal, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened authority: %v", err)
	}
	advanced, err := OpenLocalNudgeAuthority(t.Context(), cityPath, CommandRepositoryState{
		Store: state.Store, SchemaVersion: state.SchemaVersion, WriterVersion: state.WriterVersion, Revision: 5, SequenceHighWater: 1,
	}, localAuthorityOptions())
	if err != nil {
		t.Fatalf("reopen against advanced repository: %v", err)
	}
	if err := advanced.Close(); err != nil {
		t.Fatalf("Close advanced authority: %v", err)
	}
	if rewound, err := OpenLocalNudgeAuthority(t.Context(), cityPath, CommandRepositoryState{
		Store: state.Store, SchemaVersion: state.SchemaVersion, WriterVersion: state.WriterVersion, Revision: 4, SequenceHighWater: 1,
	}, localAuthorityOptions()); rewound != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		if rewound != nil {
			_ = rewound.Close()
		}
		t.Fatalf("reopen after observed repository rewind = %v, err=%v; want conflict", rewound, err)
	}
}

func TestLocalNudgeAuthorityRefusesBootstrapAgainstNonemptyRepository(t *testing.T) {
	state := localAuthorityRepositoryState()
	state.Revision = 1
	state.SequenceHighWater = 1
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if authority != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("OpenLocalNudgeAuthority = %v, err=%v; want nonempty bootstrap conflict", authority, err)
	}
}

func TestLocalNudgeAuthorityHoldsExclusiveLifetimeLock(t *testing.T) {
	cityPath := t.TempDir()
	first, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority first: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
	if second != nil || !errors.Is(err, ErrRestoreAnchorBusy) {
		t.Fatalf("OpenLocalNudgeAuthority second = %v, err=%v; want lifetime lock refusal", second, err)
	}
}

func TestLocalNudgeAuthorityTreatsRotatedTransportEvidenceAsIdempotent(t *testing.T) {
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), localAuthorityRepositoryState(), localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	request := localAuthorityIngressRequest()
	firstRequester := localAuthorityRequester()
	first, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), firstRequester), request)
	if err != nil {
		t.Fatalf("AuthorizeNudgeIngress first: %v", err)
	}
	rotatedRequester := firstRequester
	rotatedRequester.EvidenceID = "write-grant-b"
	second, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), rotatedRequester), request)
	if err != nil || second != first {
		t.Fatalf("AuthorizeNudgeIngress after evidence rotation = %#v, err=%v; want %#v", second, err, first)
	}
}

func TestLocalNudgeAuthorityRejectsChangedRequesterOrPayloadForReusedRequestID(t *testing.T) {
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), localAuthorityRepositoryState(), localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	request := localAuthorityIngressRequest()
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	if _, err := authority.AuthorizeNudgeIngress(ctx, request); err != nil {
		t.Fatalf("AuthorizeNudgeIngress first: %v", err)
	}

	changedRequester := localAuthorityRequester()
	changedRequester.PrincipalID = "principal-b"
	if _, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), changedRequester), request); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("changed requester error = %v, want conflict", err)
	}
	changedPayload := request
	changedPayload.IntentDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	changedPayload.PayloadDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := authority.AuthorizeNudgeIngress(ctx, changedPayload); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("changed payload error = %v, want conflict", err)
	}
}

func TestLocalNudgeAuthorityConcurrentIdenticalAuthorizationReturnsOneGrant(t *testing.T) {
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), localAuthorityRepositoryState(), localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	request := localAuthorityIngressRequest()

	const callers = 64
	start := make(chan struct{})
	results := make(chan struct {
		authorization NudgeAuthorization
		err           error
	}, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			authorization, err := authority.AuthorizeNudgeIngress(ctx, request)
			results <- struct {
				authorization NudgeAuthorization
				err           error
			}{authorization: authorization, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var want *NudgeAuthorization
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent AuthorizeNudgeIngress: %v", result.err)
		}
		if want == nil {
			value := result.authorization
			want = &value
			continue
		}
		if result.authorization != *want {
			t.Fatalf("concurrent authorization = %#v, want %#v", result.authorization, *want)
		}
	}
}

func TestLocalNudgeAuthorityRecoversGrantOnlyIngressWithOriginalCreationTime(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("repository State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	firstNow := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	currentNow := firstNow
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return currentNow })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(firstNow)
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	store.failNext = errors.New("injected repository commit failure")
	if _, err := ingress.Admit(ctx, request); err == nil {
		t.Fatal("first Admit error = nil, want injected repository failure after authority grant")
	}

	currentNow = firstNow.Add(time.Second)
	recovered, err := ingress.Admit(ctx, request)
	if err != nil {
		t.Fatalf("Admit retry after grant-only failure: %v", err)
	}
	if !recovered.Created || recovered.Entry.Command == nil {
		t.Fatalf("recovered admission = %#v, want created command", recovered)
	}
	if !recovered.Entry.Command.CreatedAt.Equal(firstNow) || !recovered.Entry.Command.TrustedIngress.IssuedAt.Equal(firstNow) {
		t.Fatalf("recovered times = created %v issued %v, want %v", recovered.Entry.Command.CreatedAt, recovered.Entry.Command.TrustedIngress.IssuedAt, firstNow)
	}
}

func TestLocalNudgeAuthorityRecoversDeliverAtCreationAfterGrantOnlyFailure(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("repository State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	firstNow := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	currentNow := firstNow
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return currentNow })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(firstNow)
	request.DeliverAfter = time.Time{}
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	store.failNext = errors.New("injected repository commit failure")
	if _, err := ingress.Admit(ctx, request); err == nil {
		t.Fatal("first Admit error = nil, want injected repository failure after authority grant")
	}

	currentNow = firstNow.Add(time.Second)
	recovered, err := ingress.Admit(ctx, request)
	if err != nil || !recovered.Created || recovered.Entry.Command == nil {
		t.Fatalf("Admit retry after grant-only failure = %#v, err=%v", recovered, err)
	}
	if !recovered.Entry.Command.CreatedAt.Equal(firstNow) || !recovered.Entry.Command.DeliverAfter.Equal(firstNow) {
		t.Fatalf("recovered effective times = created %v deliver %v, want %v", recovered.Entry.Command.CreatedAt, recovered.Entry.Command.DeliverAfter, firstNow)
	}
}

func TestLocalNudgeAuthorityDoesNotPoisonRequestIDForMalformedIngress(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("repository State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	now := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(now)
	request.Message = ""
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())

	if _, err := ingress.Admit(ctx, request); !errors.Is(err, ErrNudgeAuthorizationInvalid) {
		t.Fatalf("malformed Admit error = %v, want invalid authorization request", err)
	}
	request.Message = "wake up"
	recovered, err := ingress.Admit(ctx, request)
	if err != nil || !recovered.Created || recovered.Entry.Command == nil {
		t.Fatalf("corrected Admit = %#v, err=%v; want newly admitted command", recovered, err)
	}
}

func TestLocalNudgeAuthorityClassifiesInvalidNewDeliveryWindowWithoutPoisoningRequestID(t *testing.T) {
	for _, scenario := range []string{"past absolute delivery", "expired deliver at creation"} {
		t.Run(scenario, func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			repository := newVerifiedCommandRepository(t, store)
			state, err := repository.State(t.Context())
			if err != nil {
				t.Fatalf("repository State: %v", err)
			}
			authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
			if err != nil {
				t.Fatalf("OpenLocalNudgeAuthority: %v", err)
			}
			t.Cleanup(func() { _ = authority.Close() })
			builtAt := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
			admittedAt := builtAt.Add(time.Second)
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return admittedAt })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			request := validNudgeIngressRequest(builtAt)
			if scenario == "expired deliver at creation" {
				request.DeliverAfter = time.Time{}
				request.ExpiresAt = builtAt.Add(time.Millisecond)
			}
			ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())

			if _, err := ingress.Admit(ctx, request); !errors.Is(err, ErrNudgeAuthorizationInvalid) || errors.Is(err, ErrNudgeAuthorizationUnknown) {
				t.Fatalf("invalid new delivery window error = %v, want definitive invalid", err)
			}
			request.DeliverAfter = time.Time{}
			request.ExpiresAt = admittedAt.Add(time.Hour)
			recovered, err := ingress.Admit(ctx, request)
			if err != nil || !recovered.Created || recovered.Entry.Command == nil {
				t.Fatalf("corrected delivery window Admit = %#v, err=%v", recovered, err)
			}
		})
	}
}

func TestLocalNudgeAuthorityConcurrentIngressConvergesAcrossCreationTimes(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("repository State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	firstNow := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	nextNow := firstNow
	var clockMu sync.Mutex
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		result := nextNow
		nextNow = nextNow.Add(time.Nanosecond)
		return result
	})
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(firstNow)
	request.DeliverAfter = firstNow.Add(time.Minute)
	request.ExpiresAt = firstNow.Add(time.Hour)
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())

	const callers = 32
	start := make(chan struct{})
	results := make(chan struct {
		result NudgeIngressResult
		err    error
	}, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := ingress.Admit(ctx, request)
			results <- struct {
				result NudgeIngressResult
				err    error
			}{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var wantCommand *Command
	createdCount := 0
	for admission := range results {
		if admission.err != nil {
			t.Fatalf("concurrent Admit: %v", admission.err)
		}
		if admission.result.Created {
			createdCount++
		}
		if admission.result.Entry.Command == nil {
			t.Fatalf("concurrent admission has no command: %#v", admission.result)
		}
		if wantCommand == nil {
			command := *admission.result.Entry.Command
			wantCommand = &command
			continue
		}
		if *admission.result.Entry.Command != *wantCommand {
			t.Fatalf("concurrent command = %#v, want %#v", *admission.result.Entry.Command, *wantCommand)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created admissions = %d, want exactly one", createdCount)
	}
}

func TestLocalNudgeAuthorityAdvancesDensePrefixAfterOutOfOrderAdmissions(t *testing.T) {
	state := localAuthorityRepositoryState()
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	admissions := make([]CommandPartitionAdmission, 3)
	for i := range admissions {
		sequence := uint64(i + 1)
		request := localAuthorityIngressRequest()
		request.RequestID = fmt.Sprintf("request-out-of-order-%d", sequence)
		request.IntentDigest = fmt.Sprintf("%064x", sequence)
		request.PayloadDigest = fmt.Sprintf("%064x", sequence)
		authorized, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), request)
		if err != nil {
			t.Fatalf("AuthorizeNudgeIngress(%d): %v", sequence, err)
		}
		admissions[i] = CommandPartitionAdmission{
			Store: state.Store, RepositoryRevision: sequence,
			CommandID: CommandIDForRequest(state.Store, request.RequestID), Sequence: sequence,
			Partition: trustedCityPartitionFromAuthority(authorized.Reference),
		}
	}
	coverageRequest := CommandPartitionCoverageRequest{
		Store: state.Store, RepositoryRevision: 3, SequenceHighWater: 3, Partition: admissions[0].Partition,
	}
	for _, index := range []int{2, 0} {
		if err := authority.RecordCommandPartitionAdmission(t.Context(), admissions[index]); err != nil {
			t.Fatalf("RecordCommandPartitionAdmission(%d): %v", admissions[index].Sequence, err)
		}
		if _, err := authority.ResolveCommandPartitionCoverage(t.Context(), coverageRequest); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("coverage before closing sequence gap error = %v, want conflict", err)
		}
	}
	if err := authority.RecordCommandPartitionAdmission(t.Context(), admissions[1]); err != nil {
		t.Fatalf("RecordCommandPartitionAdmission(2): %v", err)
	}
	coverage, err := authority.ResolveCommandPartitionCoverage(t.Context(), coverageRequest)
	if err != nil || coverage.AdmittedCount != 3 || len(coverage.ActiveEntries) != 3 {
		t.Fatalf("coverage after closing sequence gap = %#v, err=%v", coverage, err)
	}
}

func TestLocalNudgeAuthorityRejectsCompetingTerminalPreparations(t *testing.T) {
	authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-competition")
	after := localAuthorityDeadLetteredCommand(t, pending)
	intent, err := terminalIntentForTransition(state.Revision, pending, after, partition)
	if err != nil {
		t.Fatalf("terminalIntentForTransition: %v", err)
	}
	if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandPartitionTerminal first: %v", err)
	}
	competing := intent
	competing.CommandDigest = sha256.Sum256([]byte("competing terminal state"))
	if err := authority.PrepareCommandPartitionTerminal(t.Context(), competing); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("competing terminal preparation error = %v, want conflict", err)
	}
}

func TestLocalNudgeAuthorityRepairsExactTerminalAfterState(t *testing.T) {
	authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-finalize")
	after := localAuthorityDeadLetteredCommand(t, pending)
	intent, err := terminalIntentForTransition(state.Revision, pending, after, partition)
	if err != nil {
		t.Fatalf("terminalIntentForTransition: %v", err)
	}
	if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
	}
	reader := localAuthorityRecoveryReaderFor(state, after)
	if err := authority.RepairCommandPartitionTerminals(t.Context(), reader); err != nil {
		t.Fatalf("RepairCommandPartitionTerminals: %v", err)
	}
	resolution, err := terminalResolutionForCommand(after, partition)
	if err != nil {
		t.Fatalf("terminalResolutionForCommand: %v", err)
	}
	if err := authority.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
		t.Fatalf("VerifyCommandPartitionTerminal finalized recovery: %v", err)
	}
}

func TestLocalNudgeAuthorityRepairsOnlyExactPristineAdmission(t *testing.T) {
	t.Run("committed pristine command publishes membership", func(t *testing.T) {
		authority, state, pending, partition := localAuthorityPendingCommandWithAdmission(t, "request-admission-recovery", false)
		reader := localAuthorityRecoveryReaderFor(state, pending)
		if err := authority.RepairCommandPartitionAdmissions(t.Context(), reader); err != nil {
			t.Fatalf("RepairCommandPartitionAdmissions: %v", err)
		}
		coverage, err := authority.ResolveCommandPartitionCoverage(t.Context(), CommandPartitionCoverageRequest{
			Store: state.Store, RepositoryRevision: 1, SequenceHighWater: 1, Partition: partition,
		})
		if err != nil || coverage.AdmittedCount != 1 || len(coverage.ActiveEntries) != 1 || coverage.ActiveEntries[0].CommandID != pending.ID {
			t.Fatalf("recovered admission coverage = %#v, err=%v", coverage, err)
		}
	})

	t.Run("terminal command cannot mint missing admission", func(t *testing.T) {
		authority, state, pending, _ := localAuthorityPendingCommandWithAdmission(t, "request-admission-terminal", false)
		terminal := localAuthorityDeadLetteredCommand(t, pending)
		reader := localAuthorityRecoveryReaderFor(state, terminal)
		if err := authority.RepairCommandPartitionAdmissions(t.Context(), reader); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("terminal admission recovery error = %v, want conflict", err)
		}
	})
}

func TestLocalNudgeAuthorityAbortsOnlyExactUnadvancedTerminalBeforeState(t *testing.T) {
	t.Run("exact unchanged before-state aborts", func(t *testing.T) {
		authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-abort")
		after := localAuthorityDeadLetteredCommand(t, pending)
		intent, err := terminalIntentForTransition(state.Revision, pending, after, partition)
		if err != nil {
			t.Fatalf("terminalIntentForTransition: %v", err)
		}
		if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
			t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
		}
		if err := authority.RepairCommandPartitionTerminals(t.Context(), localAuthorityRecoveryReaderFor(state, pending)); err != nil {
			t.Fatalf("RepairCommandPartitionTerminals: %v", err)
		}
		if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
			t.Fatalf("terminal preparation was not safely aborted: %v", err)
		}
	})

	t.Run("advanced repository refuses abort", func(t *testing.T) {
		authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-advanced")
		after := localAuthorityDeadLetteredCommand(t, pending)
		intent, err := terminalIntentForTransition(state.Revision, pending, after, partition)
		if err != nil {
			t.Fatalf("terminalIntentForTransition: %v", err)
		}
		if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
			t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
		}
		reader := localAuthorityRecoveryReaderFor(state, pending)
		reader.state.Revision++
		resolution := reader.entries[pending.ID]
		resolution.Revision++
		reader.entries[pending.ID] = resolution
		if err := authority.RepairCommandPartitionTerminals(t.Context(), reader); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("advanced before-state recovery error = %v, want conflict", err)
		}
		terminalResolution, err := terminalResolutionForCommand(after, partition)
		if err != nil {
			t.Fatalf("terminalResolutionForCommand: %v", err)
		}
		if err := authority.VerifyCommandPartitionTerminal(t.Context(), terminalResolution); err != nil {
			t.Fatalf("advanced refusal did not retain terminal preparation: %v", err)
		}
	})

	t.Run("advanced exact-read watermark refuses abort", func(t *testing.T) {
		authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-read-advanced")
		after := localAuthorityDeadLetteredCommand(t, pending)
		intent, err := terminalIntentForTransition(state.Revision, pending, after, partition)
		if err != nil {
			t.Fatalf("terminalIntentForTransition: %v", err)
		}
		if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
			t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
		}
		reader := localAuthorityRecoveryReaderFor(state, pending)
		resolution := reader.entries[pending.ID]
		resolution.Revision++
		reader.entries[pending.ID] = resolution
		if err := authority.RepairCommandPartitionTerminals(t.Context(), reader); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("advanced exact-read recovery error = %v, want conflict", err)
		}
	})
}

func TestLocalNudgeAuthorityRejectsUnsafeOrReplacedDatabasePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode, symlink, and open-file replacement semantics are Unix-specific")
	}
	t.Run("symlink", func(t *testing.T) {
		cityPath := t.TempDir()
		path := prepareLocalAuthorityParent(t, cityPath)
		target := filepath.Join(t.TempDir(), "authority.sqlite")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("write symlink target: %v", err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("symlink authority path: %v", err)
		}
		if authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions()); authority != nil || !errors.Is(err, ErrRestoreAnchorUnsafePath) {
			t.Fatalf("OpenLocalNudgeAuthority(symlink) = %v, err=%v; want unsafe-path refusal", authority, err)
		}
	})

	t.Run("wrong mode", func(t *testing.T) {
		cityPath := t.TempDir()
		path := prepareLocalAuthorityParent(t, cityPath)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write authority fixture: %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod authority fixture: %v", err)
		}
		if authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions()); authority != nil || !errors.Is(err, ErrRestoreAnchorUnsafePath) {
			t.Fatalf("OpenLocalNudgeAuthority(wrong mode) = %v, err=%v; want unsafe-path refusal", authority, err)
		}
	})

	t.Run("replacement after open", func(t *testing.T) {
		cityPath := t.TempDir()
		authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
		if err != nil {
			t.Fatalf("OpenLocalNudgeAuthority: %v", err)
		}
		t.Cleanup(func() { _ = authority.Close() })
		path := LocalNudgeAuthorityPath(cityPath)
		if err := os.Rename(path, path+".replaced"); err != nil {
			t.Fatalf("rename authority database: %v", err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("replace authority database: %v", err)
		}
		if _, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), localAuthorityIngressRequest()); !errors.Is(err, ErrLocalNudgeAuthorityUnavailable) {
			t.Fatalf("authorization after path replacement error = %v, want unavailable", err)
		}
	})
}

func TestLocalNudgeAuthorityRejectsStoreIdentityOrRestoreEpochMismatch(t *testing.T) {
	cityPath := t.TempDir()
	state := localAuthorityRepositoryState()
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	foreign := state
	foreign.Store.StoreUUID = "22222222-2222-4222-8222-222222222222"
	if reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, foreign, localAuthorityOptions()); reopened != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("foreign store reopen = %v, err=%v; want conflict", reopened, err)
	}
	newEpoch := state
	newEpoch.Store.RestoreEpoch++
	if reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, newEpoch, localAuthorityOptions()); reopened != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("new restore epoch reopen = %v, err=%v; want conflict", reopened, err)
	}
}

func TestLocalNudgeAuthorityRejectsCorruptOrPartialSQLite(t *testing.T) {
	t.Run("corrupt database", func(t *testing.T) {
		cityPath := t.TempDir()
		path := prepareLocalAuthorityParent(t, cityPath)
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatalf("write corrupt database: %v", err)
		}
		if authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions()); authority != nil || !errors.Is(err, ErrLocalNudgeAuthorityUnavailable) {
			t.Fatalf("OpenLocalNudgeAuthority(corrupt) = %v, err=%v; want unavailable", authority, err)
		}
	})

	t.Run("partial schema", func(t *testing.T) {
		cityPath := t.TempDir()
		authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
		if err != nil {
			t.Fatalf("OpenLocalNudgeAuthority: %v", err)
		}
		if err := authority.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		dsn := (&url.URL{Scheme: "file", Path: LocalNudgeAuthorityPath(cityPath)}).String()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open partial-schema fixture: %v", err)
		}
		if _, err := db.Exec(`DROP TABLE terminal_preparations`); err != nil {
			_ = db.Close()
			t.Fatalf("drop authority table: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close partial-schema fixture: %v", err)
		}
		if reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions()); reopened != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("OpenLocalNudgeAuthority(partial) = %v, err=%v; want conflict", reopened, err)
		}
	})

	t.Run("missing required index", func(t *testing.T) {
		cityPath := t.TempDir()
		authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
		if err != nil {
			t.Fatalf("OpenLocalNudgeAuthority: %v", err)
		}
		if err := authority.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		dsn := (&url.URL{Scheme: "file", Path: LocalNudgeAuthorityPath(cityPath)}).String()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open missing-index fixture: %v", err)
		}
		if _, err := db.Exec(`DROP INDEX memberships_partition_active`); err != nil {
			_ = db.Close()
			t.Fatalf("drop authority index: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close missing-index fixture: %v", err)
		}
		if reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions()); reopened != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("OpenLocalNudgeAuthority(missing index) = %v, err=%v; want conflict", reopened, err)
		}
	})

	t.Run("unexpected trigger", func(t *testing.T) {
		cityPath := t.TempDir()
		authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
		if err != nil {
			t.Fatalf("OpenLocalNudgeAuthority: %v", err)
		}
		if err := authority.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		dsn := (&url.URL{Scheme: "file", Path: LocalNudgeAuthorityPath(cityPath)}).String()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open trigger fixture: %v", err)
		}
		if _, err := db.Exec(`CREATE TRIGGER unexpected_membership_trigger AFTER INSERT ON memberships BEGIN UPDATE authority_meta SET dense_admission_high_water = dense_admission_high_water WHERE singleton = 1; END`); err != nil {
			_ = db.Close()
			t.Fatalf("create unexpected authority trigger: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close trigger fixture: %v", err)
		}
		if reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions()); reopened != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			t.Fatalf("OpenLocalNudgeAuthority(unexpected trigger) = %v, err=%v; want conflict", reopened, err)
		}
	})
}

func TestLocalNudgeAuthorityClaimBindsExactCommandAndStore(t *testing.T) {
	authority, _, pending, partition := localAuthorityPendingCommand(t, "request-claim-binding")
	request := NudgeClaimAuthorizationRequest{
		Command: pending, Partition: partition, ClaimID: "claim-a", OwnerID: "owner-a", AttemptID: "attempt-a",
		BoundLaunchIdentity: "launch-a", ClaimedAt: pending.CreatedAt.Add(time.Minute), LeaseUntil: pending.CreatedAt.Add(2 * time.Minute),
	}
	allowed, err := authority.AuthorizeNudgeClaim(t.Context(), request)
	if err != nil || allowed.Disposition != NudgeAuthorizationAllowed {
		t.Fatalf("AuthorizeNudgeClaim exact = %#v, err=%v; want allowed", allowed, err)
	}

	changedID := request
	changedID.Command.ID = "different-command-id"
	denied, err := authority.AuthorizeNudgeClaim(t.Context(), changedID)
	if err != nil || denied.Disposition != NudgeAuthorizationDenied {
		t.Fatalf("AuthorizeNudgeClaim changed command id = %#v, err=%v; want denied", denied, err)
	}
	changedStore := request
	changedStore.Command.Store.StoreUUID = "22222222-2222-4222-8222-222222222222"
	denied, err = authority.AuthorizeNudgeClaim(t.Context(), changedStore)
	if err != nil || denied.Disposition != NudgeAuthorizationDenied {
		t.Fatalf("AuthorizeNudgeClaim changed store = %#v, err=%v; want denied", denied, err)
	}
	changedTarget := request
	changedTarget.Command.Target.SessionID = "session-b"
	denied, err = authority.AuthorizeNudgeClaim(t.Context(), changedTarget)
	if err != nil || denied.Disposition != NudgeAuthorizationDenied {
		t.Fatalf("AuthorizeNudgeClaim changed target = %#v, err=%v; want denied", denied, err)
	}
	changedPayload := request
	changedPayload.Command.Message = "substituted payload"
	denied, err = authority.AuthorizeNudgeClaim(t.Context(), changedPayload)
	if err != nil || denied.Disposition != NudgeAuthorizationDenied {
		t.Fatalf("AuthorizeNudgeClaim changed payload = %#v, err=%v; want denied", denied, err)
	}
	changedLaunch := request
	changedLaunch.BoundLaunchIdentity = "launch-b"
	launchDecision, err := authority.AuthorizeNudgeClaim(t.Context(), changedLaunch)
	if err != nil || launchDecision.Disposition != NudgeAuthorizationAllowed || launchDecision.DecisionID == allowed.DecisionID {
		t.Fatalf("AuthorizeNudgeClaim changed launch = %#v, err=%v; want distinct allowed decision from %#v", launchDecision, err, allowed)
	}
}

type localAuthorityRecoveryReader struct {
	state   CommandRepositoryState
	entries map[string]CommandIndexResolution
}

func (r localAuthorityRecoveryReader) State(context.Context) (CommandRepositoryState, error) {
	return r.state, nil
}

func (r localAuthorityRecoveryReader) Get(_ context.Context, commandID string) (CommandIndexResolution, error) {
	if resolution, ok := r.entries[commandID]; ok {
		return resolution, nil
	}
	return CommandIndexResolution{Store: r.state.Store, Revision: r.state.Revision}, nil
}

func localAuthorityRecoveryReaderFor(state CommandRepositoryState, command Command) localAuthorityRecoveryReader {
	state.Revision = command.Order.Revision
	state.SequenceHighWater = command.Order.Sequence
	return localAuthorityRecoveryReader{
		state: state,
		entries: map[string]CommandIndexResolution{
			command.ID: {Store: state.Store, Revision: state.Revision, CompletedAuditRevision: state.Revision, Entry: CommandIndexEntry{Command: &command}, Found: true},
		},
	}
}

func localAuthorityPendingCommand(t *testing.T, requestID string) (*LocalNudgeAuthority, CommandRepositoryState, Command, TrustedCityPartition) {
	return localAuthorityPendingCommandWithAdmission(t, requestID, true)
}

func localAuthorityPendingCommandWithAdmission(t *testing.T, requestID string, recordAdmission bool) (*LocalNudgeAuthority, CommandRepositoryState, Command, TrustedCityPartition) {
	t.Helper()
	state := localAuthorityRepositoryState()
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	requestedAt := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	pending := validCommandV1(CommandStatePending)
	pending.ID = CommandIDForRequest(state.Store, requestID)
	pending.Store = state.Store
	pending.Order = CommandOrder{Sequence: 1, Revision: 1}
	pending.Target = localAuthorityIngressRequest().Target
	pending.Mode = DeliveryModeQueue
	pending.Source = CommandSourceSession
	pending.Message = "authorized local nudge"
	pending.Reference = nil
	pending.CreatedAt = requestedAt
	pending.DeliverAfter = requestedAt
	pending.ExpiresAt = requestedAt.Add(time.Hour)
	pending.Binding = nil
	pending.Claim = nil
	pending.Retry = nil
	pending.Terminal = nil
	pending.TrustedIngress = TrustedIngressReference{}
	payloadDigest := ComputeCommandPayloadDigest(pending)
	authorized, err := authority.AuthorizeNudgeIngress(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), NudgeIngressAuthorizationRequest{
		RequestID: requestID, Action: NudgeCommandAction, Mode: pending.Mode, Target: pending.Target,
		IntentDigest: computeNudgeIngressIntentDigest(pending), PayloadDigest: payloadDigest,
		DeliverAfter: pending.DeliverAfter, ExpiresAt: pending.ExpiresAt, RequestedAt: requestedAt,
	})
	if err != nil || authorized.Disposition != NudgeAuthorizationAllowed {
		t.Fatalf("AuthorizeNudgeIngress = %#v, err=%v", authorized, err)
	}
	pending.TrustedIngress = authorized.Reference
	if _, err := EncodeCommandV1(pending); err != nil {
		t.Fatalf("EncodeCommandV1(pending): %v", err)
	}
	partition := trustedCityPartitionFromAuthority(authorized.Reference)
	if recordAdmission {
		if err := authority.RecordCommandPartitionAdmission(t.Context(), CommandPartitionAdmission{
			Store: state.Store, RepositoryRevision: 1, CommandID: pending.ID, Sequence: 1, Partition: partition,
		}); err != nil {
			t.Fatalf("RecordCommandPartitionAdmission: %v", err)
		}
	}
	state.Revision = 1
	state.SequenceHighWater = 1
	return authority, state, pending, partition
}

func localAuthorityDeadLetteredCommand(t *testing.T, pending Command) Command {
	t.Helper()
	after := pending
	after.State = CommandStateDeadLettered
	after.Order.Revision++
	after.Terminal = &CommandTerminal{
		At: pending.CreatedAt.Add(time.Minute), ActionResult: CommandActionResultDeadLettered,
		ErrorClass: CommandErrorClassInvalidCommand, Detail: "invalid command",
		ProviderStage: ProviderStageNotEntered, Completion: CompletionStateNotCompleted,
	}
	if _, err := EncodeCommandV1(after); err != nil {
		t.Fatalf("EncodeCommandV1(dead-lettered): %v", err)
	}
	return after
}

func prepareLocalAuthorityParent(t *testing.T, cityPath string) string {
	t.Helper()
	path := LocalNudgeAuthorityPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create authority parent: %v", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("chmod authority parent: %v", err)
	}
	return path
}

func localAuthorityRepositoryState() CommandRepositoryState {
	return CommandRepositoryState{
		Store:         CommandStoreBinding{StoreUUID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: 1},
		SchemaVersion: CommandRepositorySchemaVersion, WriterVersion: CommandRepositoryWriterVersion,
	}
}

func localAuthorityOptions() LocalNudgeAuthorityOptions {
	return LocalNudgeAuthorityOptions{
		Profile: LocalNudgeAuthorityProfileStoreWriterIsController, AuthorityID: "authority-local-a", Issuer: "local-controller",
		TenantScope: "tenant-a", CityScope: "city-a", CredentialClass: "city-write-grant", PolicyVersion: "local-policy-v1",
	}
}

func localAuthorityRequester() AuthenticatedNudgeRequester {
	return AuthenticatedNudgeRequester{
		PrincipalID: "principal-a", TenantScope: "tenant-a", CityScope: "city-a",
		CredentialClass: "city-write-grant", EvidenceID: "write-grant-a",
	}
}

func localAuthorityIngressRequest() NudgeIngressAuthorizationRequest {
	now := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	return NudgeIngressAuthorizationRequest{
		RequestID: "request-local-authority", Action: NudgeCommandAction, Mode: DeliveryModeQueue,
		Target:        CommandTarget{SessionID: "session-a", IntentGeneration: 1, ContinuationIdentity: "continuation-a", Policy: TargetPolicyContinuation},
		IntentDigest:  "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		PayloadDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeliverAfter:  now,
		ExpiresAt:     now.Add(time.Hour),
		RequestedAt:   now,
	}
}
