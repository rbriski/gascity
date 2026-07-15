package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/google/uuid"
)

// nudgeEffectOwnership cold-selects the only provider-effect owner for durable
// nudges. Ownership is immutable for a CityRuntime; changing it requires the
// old runtime to exit and a new runtime to start.
type nudgeEffectOwnership uint8

const (
	// nudgeEffectOwnershipLegacy is deliberately zero so every existing caller
	// and restart retains the characterized legacy dispatcher by default.
	nudgeEffectOwnershipLegacy nudgeEffectOwnership = iota
	// nudgeEffectOwnershipKeyed selects the durable keyed command owner and
	// makes the legacy dispatcher unreachable for this runtime.
	nudgeEffectOwnershipKeyed
)

// nudgeSessionTargetStore is the persisted session read needed at the final
// effect fence. It intentionally excludes live-enriched Manager reads.
type nudgeSessionTargetStore interface {
	Get(string) (session.Info, error)
}

// productionNudgeEffectTargets rereads the exact persisted session generation
// and runtime identity immediately before claim and again before provider entry.
type productionNudgeEffectTargets struct {
	store nudgeSessionTargetStore
}

// Read returns one exact persisted target projection or fails closed without a
// partial identity.
func (r *productionNudgeEffectTargets) Read(ctx context.Context, sessionID string) (nudgeEffectTarget, error) {
	if ctx == nil {
		return nudgeEffectTarget{}, errors.New("reading production nudge target: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nudgeEffectTarget{}, err
	}
	if r == nil || r.store == nil {
		return nudgeEffectTarget{}, errors.New("reading production nudge target: session store is nil")
	}
	if !canonicalNudgeEffectIdentity(sessionID) {
		return nudgeEffectTarget{}, errors.New("reading production nudge target: requested session id is not canonical")
	}

	info, err := r.store.Get(sessionID)
	if err != nil {
		return nudgeEffectTarget{}, fmt.Errorf("reading production nudge target %q: %w", sessionID, err)
	}
	if err := ctx.Err(); err != nil {
		return nudgeEffectTarget{}, err
	}
	if info.ID != sessionID ||
		!canonicalNudgeEffectIdentity(info.ID) ||
		!canonicalNudgeEffectIdentity(info.SessionName) ||
		!canonicalNudgeEffectIdentity(info.SessionKey) ||
		!canonicalNudgeEffectIdentity(info.InstanceToken) ||
		!canonicalNudgeEffectIdentity(info.Provider) ||
		!canonicalNudgeEffectIdentity(info.Transport) {
		return nudgeEffectTarget{}, errors.New("reading production nudge target: persisted identity is incomplete or non-canonical")
	}
	generation, err := strconv.ParseUint(info.Generation, 10, 64)
	if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != info.Generation {
		return nudgeEffectTarget{}, errors.New("reading production nudge target: persisted generation is not canonical")
	}

	return nudgeEffectTarget{
		sessionID:            info.ID,
		sessionName:          info.SessionName,
		intentGeneration:     generation,
		continuationIdentity: info.SessionKey,
		launchIdentity:       info.InstanceToken,
		provider:             info.Provider,
		transport:            info.Transport,
		closed:               info.Closed,
	}, nil
}

// productionNudgeEffectHandles constructs the canonical worker boundary over
// the current city runtime provider while retaining persisted provider and
// transport identity for operation evidence.
type productionNudgeEffectHandles struct {
	provider runtime.Provider
	recorder events.Recorder
}

// Handle returns a worker boundary for one fully validated persisted target.
func (f *productionNudgeEffectHandles) Handle(target nudgeEffectTarget) (worker.Handle, error) {
	if f == nil || f.provider == nil {
		return nil, errors.New("constructing production nudge effect handle: runtime provider is nil")
	}
	if target.closed || target.intentGeneration == 0 ||
		!canonicalNudgeEffectIdentity(target.sessionID) ||
		!canonicalNudgeEffectIdentity(target.sessionName) ||
		!canonicalNudgeEffectIdentity(target.continuationIdentity) ||
		!canonicalNudgeEffectIdentity(target.launchIdentity) ||
		!canonicalNudgeEffectIdentity(target.provider) ||
		!canonicalNudgeEffectIdentity(target.transport) {
		return nil, errors.New("constructing production nudge effect handle: target identity is incomplete or non-canonical")
	}
	recorder := f.recorder
	if recorder == nil {
		recorder = events.Discard
	}
	return worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
		Provider:     f.provider,
		SessionName:  target.sessionName,
		ProviderName: target.provider,
		Transport:    target.transport,
		Recorder:     recorder,
	})
}

func canonicalNudgeEffectIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func newProductionNudgeEffectID(kind string) (string, error) {
	if !canonicalNudgeEffectIdentity(kind) {
		return "", errors.New("allocating production nudge effect id: kind is not canonical")
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("allocating production nudge effect %s id: %w", kind, err)
	}
	return kind + "-" + id.String(), nil
}

var (
	_ nudgeEffectTargetReader  = (*productionNudgeEffectTargets)(nil)
	_ nudgeEffectHandleFactory = (*productionNudgeEffectHandles)(nil)
)
