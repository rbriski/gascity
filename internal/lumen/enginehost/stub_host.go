package enginehost

import (
	"context"
	"fmt"
	"sync"
)

// StubHost is a deterministic [AgentHost] whose results are scripted per do
// node id. It performs no I/O and spawns no processes, so it is the substrate
// for goldens, engine unit tests, the crash harness, and any test that needs a
// do step to fold a known outcome with zero non-determinism.
//
// A request whose NodeID has no scripted entry (in Results or Errs) is a test
// wiring error: RunDo returns a non-nil error naming the node. Fill Results for
// every do node the formula under test executes.
type StubHost struct {
	// Results maps a do node id to the result RunDo returns for it.
	Results map[string]DoResult
	// Errs maps a do node id to an internal error RunDo returns for it,
	// modeling a host that could not produce an outcome. Takes precedence over
	// Results for the same id.
	Errs map[string]error

	mu    sync.Mutex
	calls []DoRequest
}

var _ AgentHost = (*StubHost)(nil)

// RunDo records the request and returns the scripted result (or error) for
// req.NodeID.
func (h *StubHost) RunDo(_ context.Context, req DoRequest) (DoResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, req)
	h.mu.Unlock()

	if err, ok := h.Errs[req.NodeID]; ok {
		return DoResult{}, err
	}
	res, ok := h.Results[req.NodeID]
	if !ok {
		return DoResult{}, fmt.Errorf("enginehost: StubHost has no scripted result for do node %q", req.NodeID)
	}
	if res.SessionRef == "" {
		res.SessionRef = "stub:" + req.NodeID
	}
	return res, nil
}

// Calls returns a copy of the recorded requests in invocation order.
func (h *StubHost) Calls() []DoRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]DoRequest, len(h.calls))
	copy(out, h.calls)
	return out
}
