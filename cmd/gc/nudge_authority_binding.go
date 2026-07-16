package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout"
)

const (
	productionNudgeAuthorityID     = "gascity-local-controller-v1"
	productionNudgeIssuer          = "gascity-local-controller"
	productionNudgeTenantScope     = "local-single-controller"
	productionNudgeCredentialClass = "city-write-grant"
	productionNudgePolicyVersion   = "gascity-local-policy-v1"
)

// productionNudgeAuthorityBinding is one process-lifetime ownership bundle.
// Every capability in the bundle is derived from the same durable repository
// and independently locked authority journal; callers cannot replace one
// member after startup evidence has been accepted.
type productionNudgeAuthorityBinding struct {
	mu sync.RWMutex

	store           beads.Store
	repository      *nudgequeue.CommandRepository
	authority       *nudgequeue.LocalNudgeAuthority
	partition       nudgequeue.TrustedCityPartition
	resolver        nudgequeue.TrustedCityPartitionResolver
	claimAuthorizer nudgequeue.NudgeClaimAuthorizer
	ingress         *nudgequeue.TrustedNudgeIngress
	source          *productionNudgeCommandSource

	tenantScope     string
	cityScope       string
	credentialClass string
	// commandProducersCovered stays false until every managed CLI/API/direct
	// nudge producer is proven to enter through this durable ingress.
	commandProducersCovered bool
	closed                  bool
	closeErr                error
}

// bindProductionNudgeAuthority binds an already-provisioned repository to one
// canonical controller-selected city identity. Store lineage and city scope
// intentionally remain separate: when two authorities are supplied the same
// already-verified repository, their distinct canonical identities mint
// distinct opaque partitions. The higher-level production opener may still
// fail closed if a second city's independent restore anchor is absent.
func bindProductionNudgeAuthority(
	ctx context.Context,
	cityPath string,
	canonicalCityIdentity string,
	store beads.Store,
	repository *nudgequeue.CommandRepository,
) (_ *productionNudgeAuthorityBinding, retErr error) {
	if ctx == nil {
		_ = closeBeadStoreHandle(store)
		return nil, errors.New("binding production nudge authority: context is nil")
	}
	if store == nil {
		return nil, errors.New("binding production nudge authority: command store is nil")
	}
	if repository == nil {
		_ = closeBeadStoreHandle(store)
		return nil, errors.New("binding production nudge authority: command repository is nil")
	}
	var authority *nudgequeue.LocalNudgeAuthority
	defer func() {
		if retErr == nil {
			return
		}
		var authorityErr error
		if authority != nil {
			authorityErr = authority.Close()
		}
		retErr = errors.Join(retErr, authorityErr, closeBeadStoreHandle(store))
	}()
	state, err := repository.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("binding production nudge authority: reading repository state: %w", err)
	}
	authority, err = nudgequeue.OpenLocalNudgeAuthority(ctx, cityPath, state, nudgequeue.LocalNudgeAuthorityOptions{
		Profile:         nudgequeue.LocalNudgeAuthorityProfileStoreWriterIsController,
		AuthorityID:     productionNudgeAuthorityID,
		Issuer:          productionNudgeIssuer,
		TenantScope:     productionNudgeTenantScope,
		CityScope:       canonicalCityIdentity,
		CredentialClass: productionNudgeCredentialClass,
		PolicyVersion:   productionNudgePolicyVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("binding production nudge authority: opening independent authority journal: %w", err)
	}
	partition, err := authority.TrustedCityPartition(ctx)
	if err != nil {
		return nil, fmt.Errorf("binding production nudge authority: loading trusted city partition: %w", err)
	}
	ingress, err := nudgequeue.NewTrustedNudgeIngress(repository, authority)
	if err != nil {
		return nil, fmt.Errorf("binding production nudge authority: constructing trusted ingress: %w", err)
	}
	if err := authority.RecoverCommandAuthority(ctx, repository); err != nil {
		return nil, fmt.Errorf("binding production nudge authority: recovering command authority: %w", err)
	}
	reader, err := nudgequeue.NewCommandPartitionReader(repository, partition, ingress)
	if err != nil {
		return nil, fmt.Errorf("binding production nudge authority: constructing partition reader: %w", err)
	}
	source := &productionNudgeCommandSource{
		repository:   repository,
		reader:       reader,
		partition:    partition,
		membership:   authority,
		terminal:     authority,
		recovery:     authority,
		recoveryGate: make(chan struct{}, 1),
	}
	source.recoveryGate <- struct{}{}
	return &productionNudgeAuthorityBinding{
		store:           store,
		repository:      repository,
		authority:       authority,
		partition:       partition,
		resolver:        ingress,
		claimAuthorizer: authority,
		ingress:         ingress,
		source:          source,
		tenantScope:     productionNudgeTenantScope,
		cityScope:       canonicalCityIdentity,
		credentialClass: productionNudgeCredentialClass,
	}, nil
}

// openProductionNudgeAuthorityBindingFromStore provisions the durable command
// repository and transfers ownership of store to the returned binding. Every
// failure before that transfer closes store exactly once.
func openProductionNudgeAuthorityBindingFromStore(
	ctx context.Context,
	cityPath string,
	canonicalCityIdentity string,
	store beads.Store,
) (_ *productionNudgeAuthorityBinding, retErr error) {
	if ctx == nil {
		_ = closeBeadStoreHandle(store)
		return nil, errors.New("opening production nudge authority binding: context is nil")
	}
	if store == nil {
		return nil, errors.New("opening production nudge authority binding: command store is nil")
	}
	transferred := false
	defer func() {
		if retErr != nil && !transferred {
			retErr = errors.Join(retErr, closeBeadStoreHandle(store))
		}
	}()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(cityPath))
	if err != nil {
		return nil, fmt.Errorf("opening production nudge authority binding: constructing command repository: %w", err)
	}
	if _, err := repository.Provision(ctx); err != nil {
		if _, repairErr := repository.RepairLineage(ctx); repairErr != nil {
			return nil, errors.Join(
				fmt.Errorf("opening production nudge authority binding: provisioning command repository: %w", err),
				fmt.Errorf("opening production nudge authority binding: repairing command repository lineage: %w", repairErr),
			)
		}
	}
	// bindProductionNudgeAuthority owns store on entry, including its failure
	// paths, so suppress this wrapper's cleanup before transferring the call.
	transferred = true
	return bindProductionNudgeAuthority(ctx, cityPath, canonicalCityIdentity, store, repository)
}

// openProductionNudgeAuthorityBinding opens the boot-latched city store and
// retains that exact handle for the binding lifetime.
func openProductionNudgeAuthorityBinding(
	ctx context.Context,
	cityPath string,
	canonicalCityIdentity string,
	conditionalWrites rollout.Mode,
) (*productionNudgeAuthorityBinding, error) {
	if ctx == nil {
		return nil, errors.New("opening production nudge authority binding: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opened, err := openStoreResultAtForCityWithMode(cityPath, cityPath, conditionalWrites, true)
	if err != nil {
		return nil, fmt.Errorf("opening production nudge authority command store: %w", err)
	}
	return openProductionNudgeAuthorityBindingFromStore(ctx, cityPath, canonicalCityIdentity, opened.Store)
}

// Admit writes through the retained trusted ingress. Close and Admit are
// serialized so shutdown either waits for the in-flight durable admission or
// rejects it before any repository work begins.
func (b *productionNudgeAuthorityBinding) Admit(ctx context.Context, request nudgequeue.NudgeIngressRequest) (nudgequeue.NudgeIngressResult, error) {
	if b == nil {
		return nudgequeue.NudgeIngressResult{}, fmt.Errorf("%w: production binding is nil", nudgequeue.ErrLocalNudgeAuthorityUnavailable)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed || b.ingress == nil || b.authority == nil || b.repository == nil {
		return nudgequeue.NudgeIngressResult{}, fmt.Errorf("%w: production binding is closed or incomplete", nudgequeue.ErrLocalNudgeAuthorityUnavailable)
	}
	return b.ingress.Admit(ctx, request)
}

// RequesterScope returns the immutable server-owned scope expected by the
// local authority. Authentication adapters stamp these values only after
// verifying the transport credential; request bodies never supply them.
func (b *productionNudgeAuthorityBinding) RequesterScope() (tenantScope, cityScope, credentialClass string) {
	if b == nil {
		return "", "", ""
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return "", "", ""
	}
	return b.tenantScope, b.cityScope, b.credentialClass
}

func (b *productionNudgeAuthorityBinding) live() bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.completeLocked()
}

func (b *productionNudgeAuthorityBinding) startupEvidence() (complete, commandProducersCovered bool) {
	if b == nil {
		return false, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	complete = b.completeLocked()
	return complete, complete && b.commandProducersCovered
}

func (b *productionNudgeAuthorityBinding) completeLocked() bool {
	return !b.closed && b.store != nil && b.repository != nil && b.authority != nil &&
		b.partition != (nudgequeue.TrustedCityPartition{}) &&
		b.resolver == b.ingress && b.claimAuthorizer == b.authority && b.ingress != nil &&
		b.source != nil && b.source.repository == b.repository && b.source.reader != nil &&
		b.source.partition == b.partition && b.source.membership == b.authority &&
		b.source.terminal == b.authority && b.source.recovery == b.authority && b.source.recoveryGate != nil &&
		b.tenantScope != "" && b.cityScope != "" && b.credentialClass != ""
}

// Close releases the independent authority lock before the command-store
// handle. It is idempotent and permanently makes admission fail closed.
func (b *productionNudgeAuthorityBinding) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return b.closeErr
	}
	b.closed = true
	b.closeErr = errors.Join(b.authority.Close(), closeBeadStoreHandle(b.store))
	return b.closeErr
}

// loadProductionNudgeAuthorityBinding enforces the no-I/O rollout boundary.
// Off/unset never inspect the security profile or invoke opener. Hosted and
// unknown profiles refuse before any local store or journal is opened.
func loadProductionNudgeAuthorityBinding(
	ctx context.Context,
	mode rollout.Mode,
	profile nudgequeue.CommandSecurityProfile,
	opener func(context.Context) (*productionNudgeAuthorityBinding, error),
) (*productionNudgeAuthorityBinding, error) {
	if mode == rollout.ModeUnset || mode == rollout.Off {
		return nil, nil
	}
	if profile != nudgequeue.CommandSecurityProfileStoreWriterIsController {
		profileErr := fmt.Errorf("%w: profile %q cannot use the local production authority binding", nudgequeue.ErrCommandSecurityProfile, profile)
		return nil, newNudgeEffectStartupRefusal(mode, profile, profileErr.Error(), profileErr)
	}
	if opener == nil {
		return nil, errors.New("production nudge authority binding opener is nil")
	}
	return opener(ctx)
}
