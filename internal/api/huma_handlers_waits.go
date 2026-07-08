package api

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Handlers for the durable-wait wire (GET /v0/city/{cityName}/waits and
// /wait/{id}). Both read through session.Store over SessionsBeadStore(), so a
// [beads.classes.sessions] relocation serves relocated wait beads that the
// generic ListBeads(label=gc:wait) leg (which reads CityBeadStore/BeadStores())
// would miss. Bead serialization is confined to session.Store + waitViewFromInfo.

// humaHandleWaitList serves GET /v0/city/{cityName}/waits?state=&session=. The
// list is created-DESC (the CLI applies its own stable ascending sort); a capped
// lookup surfaces the truncation via body.capped rather than an error.
func (s *Server) humaHandleWaitList(_ context.Context, input *WaitListInput) (*WaitListOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	if err := cacheLiveOr503(store.Store); err != nil {
		return nil, err
	}
	waits, err := session.NewStore(store).ListWaits(input.State, input.Session)
	capped := false
	if err != nil {
		if beads.IsLookupLimitError(err) {
			capped = true
		} else {
			return nil, humaStoreError(err)
		}
	}
	out := &WaitListOutput{CacheAgeS: cacheAgeSeconds(store.Store)}
	out.Body.Capped = capped
	out.Body.Waits = make([]WaitView, 0, len(waits))
	for _, w := range waits {
		out.Body.Waits = append(out.Body.Waits, waitViewFromInfo(w))
	}
	return out, nil
}

// humaHandleWaitGet serves GET /v0/city/{cityName}/wait/{id}. A missing bead
// maps to a problem+json 404 (not_found); a bead that is not a durable wait maps
// to a machine-matchable "not_a_wait: <id>" 404 detail.
func (s *Server) humaHandleWaitGet(_ context.Context, input *WaitGetInput) (*WaitGetOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	if err := cacheLiveOr503(store.Store); err != nil {
		return nil, err
	}
	w, err := session.NewStore(store).GetWait(input.ID)
	if err != nil {
		if errors.Is(err, session.ErrNotAWait) {
			return nil, huma.Error404NotFound("not_a_wait: " + input.ID)
		}
		return nil, humaStoreError(err)
	}
	out := &WaitGetOutput{CacheAgeS: cacheAgeSeconds(store.Store)}
	out.Body = waitViewFromInfo(w)
	return out, nil
}

// waitViewFromInfo projects a session.WaitInfo onto its wire view. CreatedAt is
// rendered exactly like the CLI's formatOptionalTime (zero -> "", else RFC3339
// UTC) so the typed rung and the local fallback rung are byte-identical.
func waitViewFromInfo(w session.WaitInfo) WaitView {
	created := ""
	if !w.CreatedAt.IsZero() {
		created = w.CreatedAt.UTC().Format(time.RFC3339)
	}
	return WaitView{
		ID:              w.ID,
		SessionID:       w.SessionID,
		SessionName:     w.SessionName,
		Kind:            w.Kind,
		State:           w.State,
		DepIDs:          w.DepIDs,
		DepMode:         w.DepMode,
		RegisteredEpoch: w.RegisteredEpoch,
		DeliveryAttempt: w.DeliveryAttempt,
		NudgeID:         w.NudgeID,
		ExpiresAt:       w.ExpiresAt,
		Note:            w.Note,
		Status:          w.Status,
		CreatedAt:       created,
		Labels:          w.Labels,
	}
}
