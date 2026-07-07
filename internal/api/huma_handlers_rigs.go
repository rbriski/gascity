package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/ssrf"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// rigVisibilityTimeout / rigVisibilityPoll bound the G17 post-provision
// visibility barrier: the async goroutine waits for the freshly-added rig to
// appear in Config() before emitting request.result.rig.create. It is a
// defensive poll — the controllerState refreshes config synchronously inside
// ProvisionRigFromGit — so it must never block the terminal event forever.
const (
	rigVisibilityTimeout = 10 * time.Second
	rigVisibilityPoll    = 50 * time.Millisecond
)

// humaHandleRigList is the Huma-typed handler for GET /v0/rigs.
func (s *Server) humaHandleRigList(ctx context.Context, input *RigListInput) (*ListOutput[rigResponse], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	store := s.state.CityBeadStore()
	if err := cacheLiveOr503(store); err != nil {
		return nil, err
	}
	wantGit := input.Git

	rigs := make([]rigResponse, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		resp := s.buildRigResponse(cfg, rig, sp, cityName, s.state.CityPath())
		if wantGit {
			resp.Git = fetchGitStatus(rig.Path)
		}
		rigs = append(rigs, resp)
	}
	return &ListOutput[rigResponse]{
		Index:     s.latestIndex(),
		CacheAgeS: cacheAgeSeconds(store),
		Body:      ListBody[rigResponse]{Items: rigs, Total: len(rigs)},
	}, nil
}

// humaHandleRigGet is the Huma-typed handler for GET /v0/rig/{name}.
func (s *Server) humaHandleRigGet(_ context.Context, input *RigGetInput) (*IndexOutput[rigResponse], error) {
	name := input.Name
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	wantGit := input.Git

	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			resp := s.buildRigResponse(cfg, rig, sp, s.state.CityName(), s.state.CityPath())
			if wantGit {
				resp.Git = fetchGitStatus(rig.Path)
			}
			return &IndexOutput[rigResponse]{
				Index: s.latestIndex(),
				Body:  resp,
			}, nil
		}
	}
	return nil, huma.Error404NotFound("rig " + name + " not found")
}

// humaHandleRigCreate is the Huma-typed handler for POST /v0/rigs. It branches
// on git_url:
//
//   - git_url absent: today's synchronous config-append create → 201. Byte-
//     identical to the pre-C4b path (path required, mapped via mutationError).
//   - git_url present: async clone+provision. Runs the G13 request_id state
//     machine under the per-rig-name lock, spawns a detached provisioning
//     goroutine, and returns 202 (accepted) / 200 (idempotent replay of a
//     succeeded create) / 409 (request_id or rig_name conflict).
func (s *Server) humaHandleRigCreate(ctx context.Context, input *RigCreateInput) (*RigCreateOutput, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	body := input.Body
	if strings.TrimSpace(body.GitURL) == "" {
		return s.rigCreateSync(sm, body)
	}
	return s.rigCreateAsync(ctx, sm, body)
}

// rigCreateSync is the git_url-absent 201 create. Path is required here (the
// wire schema makes it optional only so a git_url clone can derive it), so a
// missing path is the same 422 the prior required-path schema produced.
func (s *Server) rigCreateSync(sm StateMutator, body RigCreateBody) (*RigCreateOutput, error) {
	if strings.TrimSpace(body.Path) == "" {
		return nil, huma.Error422UnprocessableEntity("path is required")
	}
	rig := config.Rig{
		Name:          body.Name,
		Path:          body.Path,
		Prefix:        body.Prefix,
		DefaultBranch: body.DefaultBranch,
	}
	if err := sm.CreateRig(rig); err != nil {
		return nil, mutationError(err)
	}
	out := &RigCreateOutput{Status: http.StatusCreated}
	out.Body.Status = "created"
	out.Body.Rig = body.Name
	out.Body.RequestID = body.RequestID // echo of the client's id, if any
	return out, nil
}

// rigCreateAsync runs the G13 admission state machine for a git_url clone and,
// on a fresh/re-clone admission, spawns the detached provisioning goroutine.
func (s *Server) rigCreateAsync(ctx context.Context, sm StateMutator, body RigCreateBody) (*RigCreateOutput, error) {
	gitURL := strings.TrimSpace(body.GitURL)

	// G13 §2 validation, at the handler edge before any lock/store access.
	if body.RequestID != "" {
		if err := validateRequestID(body.RequestID); err != nil {
			return nil, huma.Error400BadRequest("invalid request_id: must be 8-200 chars of [A-Za-z0-9._~:-] and not a bare JSON literal")
		}
	}
	if err := validateRigName(body.Name); err != nil {
		return nil, huma.Error400BadRequest("invalid rig name")
	}

	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	city := s.rigIdemCity()

	var (
		out    *RigCreateOutput
		outErr error
	)
	lockErr := withRigNameLock(ctx, s.state.CityPath(), body.Name, func() error {
		res, err := admitRigCreate(s.rigIdem, store, s.currentCityEventCursor, s.rigInConfig, city, body)
		if err != nil {
			outErr = s.mapRigAdmitError(err)
			return nil
		}
		switch res.outcome {
		case rigAdmitNew, rigAdmitReclone:
			// The cursor was captured under this lock (res.eventCursor) and the
			// live entry is registered; spawn the provision, then return 202.
			s.spawnRigProvision(sm, city, res.entry, body, gitURL)
			out = acceptedRigOutput(res)
		case rigAdmitInflightReplay:
			// A goroutine already owns this request_id; replay its cursor, no spawn.
			out = acceptedRigOutput(res)
		case rigAdmitExisting:
			out = existingRigOutput(res)
		default:
			outErr = huma.Error500InternalServerError("unknown rig admission outcome")
		}
		return nil
	})
	if lockErr != nil {
		if errors.Is(lockErr, context.Canceled) || errors.Is(lockErr, context.DeadlineExceeded) {
			return nil, huma.Error408RequestTimeout("request canceled while awaiting rig-create admission")
		}
		return nil, huma.Error500InternalServerError(lockErr.Error())
	}
	if outErr != nil {
		return nil, outErr
	}
	return out, nil
}

// acceptedRigOutput builds the 202 accepted body from an admission result.
func acceptedRigOutput(res rigAdmitResult) *RigCreateOutput {
	out := &RigCreateOutput{Status: http.StatusAccepted}
	out.Body.Status = "accepted"
	out.Body.RequestID = res.requestID
	out.Body.EventCursor = res.eventCursor
	return out
}

// existingRigOutput builds the 200 idempotent-replay body from a succeeded
// durable record.
func existingRigOutput(res rigAdmitResult) *RigCreateOutput {
	out := &RigCreateOutput{Status: http.StatusOK}
	out.Body.Status = "exists"
	out.Body.RequestID = res.requestID
	if res.record != nil {
		out.Body.Rig = res.record.Metadata[metaIdemResultRig]
		out.Body.Prefix = res.record.Metadata[metaIdemResultPrefix]
		out.Body.DefaultBranch = res.record.Metadata[metaIdemResultBranch]
	}
	return out
}

// mapRigAdmitError renders the two G13 §4 conflicts as structured 409s (the
// sling structured-409 precedent), so a coordinating client can attach to a
// live provision's event stream. Any other admission error is a 500.
func (s *Server) mapRigAdmitError(err error) error {
	var rc *requestIDConflictError
	if errors.As(err, &rc) {
		return &huma.ErrorModel{
			Status: http.StatusConflict,
			Title:  http.StatusText(http.StatusConflict),
			Detail: rc.Error(),
			Errors: []*huma.ErrorDetail{
				{Location: "body.code", Value: "request_id_conflict"},
				{Location: "body.request_id", Value: rc.RequestID},
			},
		}
	}
	var nc *rigNameConflictError
	if errors.As(err, &nc) {
		details := []*huma.ErrorDetail{
			{Location: "body.code", Value: "rig_name_conflict"},
			{Location: "body.name", Value: nc.Rig},
		}
		if nc.InFlightRequestID != "" {
			details = append(details,
				&huma.ErrorDetail{Location: "body.in_flight_request_id", Value: nc.InFlightRequestID},
				&huma.ErrorDetail{Location: "body.event_cursor", Value: nc.InFlightCursor},
			)
		}
		return &huma.ErrorModel{
			Status: http.StatusConflict,
			Title:  http.StatusText(http.StatusConflict),
			Detail: nc.Error(),
			Errors: details,
		}
	}
	return huma.Error500InternalServerError("rig admission failed: " + err.Error())
}

// spawnRigProvision launches the detached provisioning goroutine for a
// fresh/re-clone admission. The live entry is already registered; this owns its
// terminal drop + durable mark + terminal event.
func (s *Server) spawnRigProvision(sm StateMutator, city string, entry *liveProvision, body RigCreateBody, gitURL string) {
	store := s.state.CityBeadStore()
	reqID := entry.requestID
	name := body.Name

	// Progress emitter: non-blocking + panic-safe. The recover lives INSIDE the
	// closure — an observability emit hiccup must never roll back a healthy
	// provision (which the goroutine's recoverAsRequestFailed would do) or mark
	// the request failed.
	onStep := func(step, detail string, warn bool) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("api: rig.provision.progress emit panic (rig %s, step %s): %v", name, step, r)
			}
		}()
		s.emitRigProvisionProgress(reqID, name, step, detail, warn)
	}

	rigCfg := config.Rig{
		Name:          name,
		Path:          body.Path,
		Prefix:        body.Prefix,
		DefaultBranch: body.DefaultBranch,
	}

	go func() {
		terminalized := false
		defer s.recoverAsRequestFailed(reqID, RequestOperationRigCreate) // runs LAST (LIFO)
		defer func() {                                                   // runs FIRST: panic backstop
			if !terminalized {
				s.rigIdem.remove(city, entry) // never wedge the name on a panic
			}
		}()

		provisioned, err := sm.ProvisionRigFromGit(context.Background(), rigCfg, gitURL, onStep)
		if err != nil {
			// Rollback (G13 §6 drop-then-mark). The full staging dir/DB teardown
			// is TODO(C4c); here we mark the durable record re-executable and drop
			// the live entry so a retry re-clones.
			if entry.beadID != "" {
				if mErr := markIdemRolledBack(store, entry.beadID); mErr != nil {
					log.Printf("api: rig create %s: marking rolled_back: %v", reqID, mErr)
				}
			}
			s.rigIdem.remove(city, entry)
			terminalized = true
			s.emitRequestFailed(reqID, RequestOperationRigCreate, rigProvisionFailureCode(err), err.Error())
			return
		}

		// Success: wait for the G17 visibility barrier before the terminal step.
		s.waitRigVisible(name)
		prefix := provisioned.EffectivePrefix()
		branch := provisioned.EffectiveDefaultBranch()
		if entry.beadID != "" {
			if mErr := markIdemSucceeded(store, entry.beadID, name, prefix, branch); mErr != nil {
				log.Printf("api: rig create %s: marking succeeded: %v", reqID, mErr)
			}
		}
		s.rigIdem.remove(city, entry)
		terminalized = true
		s.emitRigCreateSucceeded(reqID, name, prefix, branch)
	}()
}

// waitRigVisible polls Config() until the rig appears (the G17 visibility
// barrier) or the bounded deadline elapses. Best-effort: it logs and returns on
// timeout rather than stranding the terminal event, since the controllerState
// already refreshes config synchronously inside ProvisionRigFromGit.
func (s *Server) waitRigVisible(name string) {
	deadline := time.Now().Add(rigVisibilityTimeout)
	for {
		if s.rigInConfig(name) {
			return
		}
		if !time.Now().Before(deadline) {
			log.Printf("api: rig create: %q not visible in config after %s; emitting result anyway", name, rigVisibilityTimeout)
			return
		}
		time.Sleep(rigVisibilityPoll)
	}
}

// rigInConfig reports whether the city config currently holds a rig by name.
func (s *Server) rigInConfig(name string) bool {
	cfg := s.state.Config()
	if cfg == nil {
		return false
	}
	for _, r := range cfg.Rigs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// rigIdemCity is the stable per-city key for the live index and durable
// records: the cleaned city path (unique per city, stable across reboots so a
// crash-recovery scan of metaIdemCity matches).
func (s *Server) rigIdemCity() string {
	return filepath.Clean(strings.TrimSpace(s.state.CityPath()))
}

// rigProvisionFailureCode maps an async provisioning error to a stable
// request.failed error_code. clone failures currently ride the provision_failed
// catch-all; a dedicated clone_failed code is TODO(C4c) once the clone step
// carries a matchable sentinel across the StateMutator boundary.
func rigProvisionFailureCode(err error) string {
	switch {
	case errors.Is(err, ssrf.ErrBlockedHost):
		return "blocked_host"
	case errors.Is(err, configedit.ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, configedit.ErrValidation):
		return "invalid_request"
	default:
		return "provision_failed"
	}
}

// humaHandleRigUpdate is the Huma-typed handler for PATCH /v0/rig/{name}.
func (s *Server) humaHandleRigUpdate(_ context.Context, input *RigUpdateInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	patch := RigUpdate{
		Path:          input.Body.Path,
		Prefix:        input.Body.Prefix,
		DefaultBranch: input.Body.DefaultBranch,
		Suspended:     input.Body.Suspended,
	}

	if err := sm.UpdateRig(input.Name, patch); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "updated"
	return resp, nil
}

// humaHandleRigDelete is the Huma-typed handler for DELETE /v0/rig/{name}.
func (s *Server) humaHandleRigDelete(_ context.Context, input *RigDeleteInput) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	if err := sm.DeleteRig(input.Name); err != nil {
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// humaHandleRigAction is the Huma-typed handler for POST /v0/rig/{name}/{action}.
func (s *Server) humaHandleRigAction(_ context.Context, input *RigActionInput) (*RigActionResponse, error) {
	name := input.Name
	action := input.Action

	switch action {
	case "suspend", "resume":
		sm, ok := s.state.(StateMutator)
		if !ok {
			return nil, errMutationsNotSupported
		}
		var err error
		if action == "suspend" {
			err = sm.SuspendRig(name)
		} else {
			err = sm.ResumeRig(name)
		}
		if err != nil {
			return nil, mutationError(err)
		}
		resp := &RigActionResponse{}
		resp.Body.Status = "ok"
		resp.Body.Action = action
		resp.Body.Rig = name
		return resp, nil

	case "restart":
		return s.humaHandleRigRestart(name)

	default:
		return nil, huma.Error404NotFound("unknown rig action: " + action)
	}
}

// humaHandleRigRestart kills all agents in a rig so the reconciler restarts them.
// Uses sp.Stop() directly — no StateMutator dependency for runtime kills.
func (s *Server) humaHandleRigRestart(name string) (*RigActionResponse, error) {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()

	// Verify rig exists.
	rigFound := false
	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			rigFound = true
			break
		}
	}
	if !rigFound {
		return nil, huma.Error404NotFound("rig " + name + " not found")
	}

	// Best-effort kill: the agent set may change between config read and each
	// Stop call (pool scaling, config reload). The reconciler is the
	// convergence mechanism — survivors will be caught on its next tick.
	killed := make([]string, 0)
	failed := make([]string, 0)
	for _, a := range cfg.Agents {
		if workdirutil.ConfiguredRigName(s.state.CityPath(), a, cfg.Rigs) != name {
			continue
		}
		expanded := expandAgent(a, cityName, cfg.Workspace.SessionTemplate, sp)
		for _, ea := range expanded {
			sessionName := agentSessionName(cityName, ea.qualifiedName, cfg.Workspace.SessionTemplate)
			if err := sp.Stop(sessionName); err != nil {
				// "session gone" is benign — agent wasn't running.
				if !runtime.IsSessionGone(err) {
					failed = append(failed, ea.qualifiedName)
				}
			} else {
				killed = append(killed, ea.qualifiedName)
			}
		}
	}

	resp := &RigActionResponse{}
	resp.Body.Action = "restart"
	resp.Body.Rig = name
	resp.Body.Killed = killed

	if len(failed) > 0 {
		resp.Body.Failed = failed
		if len(killed) == 0 {
			// Total failure: return 200 with Status="failed" + the
			// populated Failed list. Huma's 5xx path would discard
			// the typed body and emit Problem Details, which strips
			// the agent names operators need to diagnose. The 200
			// carries the full per-agent detail; callers dispatch
			// on Body.Status.
			resp.Body.Status = "failed"
		} else {
			resp.Body.Status = "partial"
		}
	} else {
		resp.Body.Status = "ok"
	}
	return resp, nil
}
