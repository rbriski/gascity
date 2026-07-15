package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

type recordingSessionNudgeAdmission struct {
	result  nudgequeue.NudgeIngressResult
	err     error
	admit   func(int, context.Context, string, nudgequeue.NudgeIngressRequest) (nudgequeue.NudgeIngressResult, error)
	called  bool
	calls   int
	ctx     context.Context
	city    string
	request nudgequeue.NudgeIngressRequest
}

type sessionNudgeAdmissionFunc func(
	context.Context,
	string,
	nudgequeue.NudgeIngressRequest,
) (nudgequeue.NudgeIngressResult, error)

func (f sessionNudgeAdmissionFunc) AdmitSessionNudge(
	ctx context.Context,
	city string,
	request nudgequeue.NudgeIngressRequest,
) (nudgequeue.NudgeIngressResult, error) {
	return f(ctx, city, request)
}

func (f *recordingSessionNudgeAdmission) AdmitSessionNudge(
	ctx context.Context,
	city string,
	request nudgequeue.NudgeIngressRequest,
) (nudgequeue.NudgeIngressResult, error) {
	f.called = true
	f.calls++
	f.ctx = ctx
	f.city = city
	f.request = request
	if f.admit != nil {
		return f.admit(f.calls, ctx, city, request)
	}
	return f.result, f.err
}

type sessionNudgeContextMarker struct{}

func TestSessionNudgeAdmissionForwardsTypedIntentAndTrustedContext(t *testing.T) {
	state := newFakeState(t)
	deliverAfter := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	expiresAt := deliverAfter.Add(time.Hour)
	admission := &recordingSessionNudgeAdmission{result: admittedSessionNudgeResult(
		"gc-nudge-command-1",
		true,
	)}
	handler := newSessionNudgeAdmissionTestHandler(t, state, admission)

	body := SessionNudgeAdmissionRequestBody{
		RequestID:            "request-1",
		Mode:                 nudgequeue.DeliveryModeWaitIdle,
		IntentGeneration:     7,
		ContinuationIdentity: "continuation-7",
		TargetPolicy:         nudgequeue.TargetPolicyContinuation,
		Message:              "continue with the assigned work",
		DeliverAfter:         &deliverAfter,
		ExpiresAt:            expiresAt,
	}
	req := newSessionNudgeAdmissionRequest(t, state, body)
	req = req.WithContext(context.WithValue(
		nudgequeue.WithAuthenticatedNudgeRequester(req.Context(), nudgequeue.AuthenticatedNudgeRequester{
			PrincipalID:     "principal-1",
			TenantScope:     "tenant-1",
			CityScope:       state.CityName(),
			CredentialClass: "city-write-grant",
			EvidenceID:      "grant-1",
		}),
		sessionNudgeContextMarker{},
		"trusted-request-context",
	))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	if !admission.called {
		t.Fatal("durable admission boundary was not called")
	}
	if admission.city != state.CityName() {
		t.Fatalf("city = %q, want %q", admission.city, state.CityName())
	}
	wantRequest := nudgequeue.NudgeIngressRequest{
		RequestID: "request-1",
		Mode:      nudgequeue.DeliveryModeWaitIdle,
		Target: nudgequeue.CommandTarget{
			SessionID:            "session-1",
			IntentGeneration:     7,
			ContinuationIdentity: "continuation-7",
			Policy:               nudgequeue.TargetPolicyContinuation,
		},
		Source:       nudgequeue.CommandSourceSession,
		Message:      "continue with the assigned work",
		DeliverAfter: deliverAfter,
		ExpiresAt:    expiresAt,
	}
	if admission.request != wantRequest {
		t.Fatalf("admission request = %#v, want %#v", admission.request, wantRequest)
	}
	if got := admission.ctx.Value(sessionNudgeContextMarker{}); got != "trusted-request-context" {
		t.Fatalf("trusted request context marker = %#v, want preserved", got)
	}

	var response SessionNudgeAdmissionResponseBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.CommandID != "gc-nudge-command-1" || response.Status != nudgequeue.CommandStatePending || !response.Created {
		t.Fatalf("response = %#v", response)
	}

	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	for _, forbidden := range []string{"principal", "tenant", "credential", "evidence", "trusted_ingress"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("caller body exposes trusted requester field %q: %s", forbidden, wire)
		}
	}
}

func TestSessionNudgeAdmissionCapabilityAbsentFailsClosed(t *testing.T) {
	state := newFakeState(t)
	handler := wrapTestSupervisorMiddleware(NewSupervisorMux(
		&stateCityResolver{state: state}, nil, false, "test", "", time.Now(),
	))
	req := newSessionNudgeAdmissionRequest(t, state, validSessionNudgeAdmissionBody())
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assertSessionNudgeProblem(t, recorder, http.StatusServiceUnavailable, apierr.ServiceUnavailable.Code)
}

func TestSessionNudgeAdmissionTypedNilCapabilityFailsClosed(t *testing.T) {
	state := newFakeState(t)
	var admission *recordingSessionNudgeAdmission
	handler := newSessionNudgeAdmissionTestHandler(t, state, admission)
	req := newSessionNudgeAdmissionRequest(t, state, validSessionNudgeAdmissionBody())
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assertSessionNudgeProblem(t, recorder, http.StatusServiceUnavailable, apierr.ServiceUnavailable.Code)
}

func TestSessionNudgeAdmissionNilFunctionCapabilityFailsClosed(t *testing.T) {
	state := newFakeState(t)
	var admission sessionNudgeAdmissionFunc
	handler := newSessionNudgeAdmissionTestHandler(t, state, admission)
	req := newSessionNudgeAdmissionRequest(t, state, validSessionNudgeAdmissionBody())
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assertSessionNudgeProblem(t, recorder, http.StatusServiceUnavailable, apierr.ServiceUnavailable.Code)
}

func TestSessionNudgeAdmissionRejectsCallerOwnedRequesterEvidence(t *testing.T) {
	state := newFakeState(t)
	admission := &recordingSessionNudgeAdmission{}
	handler := newSessionNudgeAdmissionTestHandler(t, state, admission)
	wire, err := json.Marshal(validSessionNudgeAdmissionBody())
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	forged := strings.TrimSuffix(string(wire), "}") + `,"principal_id":"forged-principal","credential_class":"forged-grant","evidence_id":"forged-evidence"}`
	req := newPostRequest(cityURL(state, "/session/session-1/nudges"), strings.NewReader(forged))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	if admission.called {
		t.Fatal("durable admission ran for caller-supplied requester evidence")
	}
}

func TestSessionNudgeAdmissionDelegatesReplayIdentityToDurableBoundary(t *testing.T) {
	state := newFakeState(t)
	admission := &recordingSessionNudgeAdmission{}
	admission.admit = func(call int, _ context.Context, _ string, _ nudgequeue.NudgeIngressRequest) (nudgequeue.NudgeIngressResult, error) {
		switch call {
		case 1:
			return admittedSessionNudgeResult("gc-nudge-command-1", true), nil
		case 2:
			return admittedSessionNudgeResult("gc-nudge-command-1", false), nil
		case 3:
			return nudgequeue.NudgeIngressResult{}, nudgequeue.ErrCommandRepositoryIdempotencyConflict
		default:
			t.Fatalf("unexpected admission call %d", call)
			return nudgequeue.NudgeIngressResult{}, nil
		}
	}
	handler := newSessionNudgeAdmissionTestHandler(t, state, admission)
	body := validSessionNudgeAdmissionBody()

	first := serveSessionNudgeAdmission(t, handler, newSessionNudgeAdmissionRequest(t, state, body))
	assertAdmittedSessionNudge(t, first, true)

	replay := serveSessionNudgeAdmission(t, handler, newSessionNudgeAdmissionRequest(t, state, body))
	assertAdmittedSessionNudge(t, replay, false)

	body.Message = "different immutable content"
	conflict := serveSessionNudgeAdmission(t, handler, newSessionNudgeAdmissionRequest(t, state, body))
	assertSessionNudgeProblem(t, conflict, http.StatusConflict, apierr.ConflictWrongState.Code)

	if admission.calls != 3 {
		t.Fatalf("durable admission calls = %d, want 3", admission.calls)
	}
	if admission.request.RequestID != body.RequestID || admission.request.Message != body.Message {
		t.Fatalf("conflicting replay was not delegated intact: %#v", admission.request)
	}
}

func TestSessionNudgeAdmissionMapsDomainFailuresToRegisteredProblems(t *testing.T) {
	wrapped := func(name string, err error) error {
		return fmt.Errorf("sensitive %s diagnostic: %w", name, err)
	}
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "authorization denied",
			err:        wrapped("authorization", nudgequeue.ErrNudgeAuthorizationDenied),
			wantStatus: http.StatusForbidden,
			wantCode:   apierr.Forbidden.Code,
		},
		{
			name:       "invalid authorization evidence",
			err:        wrapped("authorization evidence", nudgequeue.ErrNudgeAuthorizationInvalid),
			wantStatus: http.StatusBadRequest,
			wantCode:   apierr.InvalidRequest.Code,
		},
		{
			name:       "invalid repository request",
			err:        wrapped("repository request", nudgequeue.ErrCommandRepositoryInvalidRequest),
			wantStatus: http.StatusBadRequest,
			wantCode:   apierr.InvalidRequest.Code,
		},
		{
			name:       "request identity conflict",
			err:        wrapped("request identity", nudgequeue.ErrCommandRepositoryIdempotencyConflict),
			wantStatus: http.StatusConflict,
			wantCode:   apierr.ConflictWrongState.Code,
		},
		{
			name:       "deadline exceeded",
			err:        wrapped("deadline", context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   apierr.GatewayTimeout.Code,
		},
		{
			name:       "request canceled",
			err:        wrapped("cancellation", context.Canceled),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "authorization unknown",
			err:        wrapped("authorization", nudgequeue.ErrNudgeAuthorizationUnknown),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "authority conflict",
			err:        wrapped("authority conflict", nudgequeue.ErrLocalNudgeAuthorityConflict),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "authority unavailable",
			err:        wrapped("authority unavailable", nudgequeue.ErrLocalNudgeAuthorityUnavailable),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository unsupported",
			err:        wrapped("repository unsupported", nudgequeue.ErrCommandRepositoryUnsupported),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository schema skew",
			err:        wrapped("repository schema", nudgequeue.ErrCommandRepositorySchemaSkew),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository lineage mismatch",
			err:        wrapped("repository lineage", nudgequeue.ErrCommandRepositoryLineage),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository partition unverified",
			err:        wrapped("repository partition", nudgequeue.ErrCommandRepositoryPartition),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository snapshot limit",
			err:        wrapped("repository snapshot", nudgequeue.ErrCommandRepositorySnapshotLimit),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository checkpoint required",
			err:        wrapped("repository checkpoint", nudgequeue.ErrCommandRepositoryCheckpointRequired),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository record invalid",
			err:        wrapped("repository record", nudgequeue.ErrCommandRepositoryRecord),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "repository checkpoint conflict",
			err:        wrapped("checkpoint conflict", nudgequeue.ErrCommandRepositoryCheckpointConflict),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "restore admission denied",
			err:        wrapped("restore admission", nudgequeue.ErrRestoreAnchorAdmission),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "restore anchor conflict",
			err:        wrapped("restore conflict", nudgequeue.ErrRestoreAnchorConflict),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "restore anchor busy",
			err:        wrapped("restore busy", nudgequeue.ErrRestoreAnchorBusy),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "restore anchor unsafe path",
			err:        wrapped("restore path", nudgequeue.ErrRestoreAnchorUnsafePath),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "restore anchor durability uncertain",
			err:        wrapped("restore durability", nudgequeue.ErrRestoreAnchorDurabilityUncertain),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.ServiceUnavailable.Code,
		},
		{
			name:       "unexpected failure",
			err:        errors.New("sensitive backend detail"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierr.Internal.Code,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newFakeState(t)
			admission := &recordingSessionNudgeAdmission{err: test.err}
			handler := newSessionNudgeAdmissionTestHandler(t, state, admission)
			req := newSessionNudgeAdmissionRequest(t, state, validSessionNudgeAdmissionBody())
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assertSessionNudgeProblem(t, recorder, test.wantStatus, test.wantCode)
			if strings.Contains(recorder.Body.String(), "sensitive") {
				t.Fatalf("response leaked backend error: %s", recorder.Body.String())
			}
		})
	}
}

func TestSessionNudgeAdmissionOpenAPIContract(t *testing.T) {
	sm := NewSupervisorMux(emptyRoundtripResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	recorder := httptest.NewRecorder()
	sm.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", recorder.Code, recorder.Body.String())
	}

	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
			Responses   map[string]json.RawMessage `json:"responses"`
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				AdditionalProperties *bool                      `json:"additionalProperties"`
				Properties           map[string]json.RawMessage `json:"properties"`
				Required             []string                   `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode live OpenAPI spec: %v", err)
	}

	const path = "/v0/city/{cityName}/session/{id}/nudges"
	op, ok := spec.Paths[path]["post"]
	if !ok {
		t.Fatalf("POST %s is absent from live OpenAPI", path)
	}
	if op.OperationID != "admit-session-nudge" {
		t.Fatalf("operationId = %q, want admit-session-nudge", op.OperationID)
	}
	hasCSRFHeader := false
	for _, parameter := range op.Parameters {
		if parameter.In != "header" {
			continue
		}
		switch parameter.Name {
		case csrfHeaderName:
			hasCSRFHeader = true
		case "Idempotency-Key":
			t.Error("admit-session-nudge must use durable body request_id as its sole retry identity")
		}
	}
	if !hasCSRFHeader {
		t.Errorf("OpenAPI is missing required %s mutation header", csrfHeaderName)
	}
	for _, status := range []int{
		http.StatusAccepted,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		if _, ok := op.Responses[strconv.Itoa(status)]; !ok {
			t.Errorf("OpenAPI response %d is not declared", status)
		}
	}

	requestSchema := op.RequestBody.Content["application/json"].Schema.Ref
	const schemaPrefix = "#/components/schemas/"
	if !strings.HasPrefix(requestSchema, schemaPrefix) {
		t.Fatalf("request schema ref = %q, want component ref", requestSchema)
	}
	schema, ok := spec.Components.Schemas[strings.TrimPrefix(requestSchema, schemaPrefix)]
	if !ok {
		t.Fatalf("request schema %q is absent", requestSchema)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatal("session nudge request schema must reject unknown properties")
	}
	wantProperties := map[string]bool{
		"request_id":            true,
		"mode":                  true,
		"intent_generation":     true,
		"continuation_identity": true,
		"launch_identity":       true,
		"target_policy":         true,
		"message":               true,
		"deliver_after":         true,
		"expires_at":            true,
	}
	for property := range schema.Properties {
		if !wantProperties[property] {
			t.Errorf("request schema exposes unexpected property %q", property)
		}
	}
	for property := range wantProperties {
		if _, ok := schema.Properties[property]; !ok {
			t.Errorf("request schema is missing property %q", property)
		}
	}
	wantRequired := map[string]bool{
		"request_id":        true,
		"mode":              true,
		"intent_generation": true,
		"target_policy":     true,
		"message":           true,
		"expires_at":        true,
	}
	if len(schema.Required) != len(wantRequired) {
		t.Errorf("required request properties = %v, want exactly %v", schema.Required, wantRequired)
	}
	for _, property := range schema.Required {
		if !wantRequired[property] {
			t.Errorf("request schema unexpectedly requires %q", property)
		}
	}
}

func TestSessionNudgeAdmissionHandlerCannotReachRuntimeEffects(t *testing.T) {
	const source = "handler_session_nudge_admission.go"
	file, err := parser.ParseFile(token.NewFileSet(), source, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s: %v", imported.Path.Value, err)
		}
		for _, forbidden := range []string{
			"github.com/gastownhall/gascity/internal/runtime",
			"github.com/gastownhall/gascity/internal/worker",
		} {
			if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
				t.Fatalf("%s imports effect capability %q", source, path)
			}
		}
	}
}

func newSessionNudgeAdmissionTestHandler(
	t *testing.T,
	state State,
	admission SessionNudgeAdmission,
) http.Handler {
	t.Helper()
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", time.Now())
	sm.WithSessionNudgeAdmission(admission)
	return wrapTestSupervisorMiddleware(sm)
}

func newSessionNudgeAdmissionRequest(
	t *testing.T,
	state State,
	body SessionNudgeAdmissionRequestBody,
) *http.Request {
	t.Helper()
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	request := newPostRequest(cityURL(state, "/session/session-1/nudges"), strings.NewReader(string(wire)))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func validSessionNudgeAdmissionBody() SessionNudgeAdmissionRequestBody {
	return SessionNudgeAdmissionRequestBody{
		RequestID:            "request-1",
		Mode:                 nudgequeue.DeliveryModeQueue,
		IntentGeneration:     1,
		ContinuationIdentity: "continuation-1",
		TargetPolicy:         nudgequeue.TargetPolicyContinuation,
		Message:              "continue",
		ExpiresAt:            time.Now().UTC().Add(time.Hour),
	}
}

func admittedSessionNudgeResult(id string, created bool) nudgequeue.NudgeIngressResult {
	return nudgequeue.NudgeIngressResult{
		Entry: nudgequeue.CommandIndexEntry{Command: &nudgequeue.Command{
			ID:    id,
			State: nudgequeue.CommandStatePending,
		}},
		Created: created,
	}
}

func serveSessionNudgeAdmission(
	t *testing.T,
	handler http.Handler,
	request *http.Request,
) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func assertAdmittedSessionNudge(t *testing.T, recorder *httptest.ResponseRecorder, created bool) {
	t.Helper()
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var response SessionNudgeAdmissionResponseBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.CommandID != "gc-nudge-command-1" || response.Status != nudgequeue.CommandStatePending || response.Created != created {
		t.Fatalf("response = %#v, want command gc-nudge-command-1 pending created=%t", response, created)
	}
}

func assertSessionNudgeProblem(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, status, recorder.Body.String())
	}
	var problem apierr.ErrorModel
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != code {
		t.Fatalf("problem code = %q, want %q; body=%s", problem.Code, code, recorder.Body.String())
	}
}
