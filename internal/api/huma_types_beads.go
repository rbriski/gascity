package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Per-domain Huma input/output types for the beads handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_beads.go.

// --- Bead types ---

// BeadListInput is the Huma input for GET /v0/city/{cityName}/beads.
type BeadListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Status   string `query:"status" required:"false" doc:"Filter by bead status."`
	Type     string `query:"type" required:"false" doc:"Filter by bead type."`
	Label    string `query:"label" required:"false" doc:"Filter by label."`
	Assignee string `query:"assignee" required:"false" doc:"Filter by assignee."`
	Rig      string `query:"rig" required:"false" doc:"Filter by rig."`
	All      bool   `query:"all" required:"false" doc:"Include closed beads."`
}

// BeadReadyInput is the Huma input for GET /v0/city/{cityName}/beads/ready.
type BeadReadyInput struct {
	CityScope
	BlockingParam
}

// BeadEphemeralInput is the Huma input for GET
// /v0/city/{cityName}/beads/ephemeral — the wisp/ephemeral discovery tier that
// `bd query 'ephemeral=true AND <clauses>'` reads. Only the ephemeral tier is
// returned (the issues tier is excluded); the bare predicate clauses map to
// these filters. Under graph_store=sqlite the wisp tier lives in the SQLite
// graph backend, so this route reaches it through the controller's Router (the
// work-only bd cannot see SQLite wisps, which is why the shim refuses bare
// `bd query` in the split phase and routes it here instead).
type BeadEphemeralInput struct {
	CityScope
	Status   string `query:"status" required:"false" doc:"Filter by status."`
	Type     string `query:"type" required:"false" doc:"Filter by bead type."`
	Label    string `query:"label" required:"false" doc:"Filter by label."`
	Assignee string `query:"assignee" required:"false" doc:"Filter by assignee."`
	Parent   string `query:"parent" required:"false" doc:"Filter by parent bead id."`
	All      bool   `query:"all" required:"false" doc:"Include closed ephemeral beads."`
	Limit    int    `query:"limit" required:"false" doc:"Max rows (0 = unbounded)."`
}

// BeadGraphInput is the Huma input for GET /v0/city/{cityName}/beads/graph/{rootID}.
type BeadGraphInput struct {
	CityScope
	RootID string `path:"rootID" doc:"Root bead ID for the graph."`
}

// BeadGetInput is the Huma input for GET /v0/city/{cityName}/bead/{id}.
type BeadGetInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadDepsInput is the Huma input for GET /v0/city/{cityName}/bead/{id}/deps.
type BeadDepsInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadCreateInput is the Huma input for POST /v0/city/{cityName}/beads.
type BeadCreateInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig         string            `json:"rig,omitempty" doc:"Rig name."`
		Title       string            `json:"title" doc:"Bead title." minLength:"1"`
		Type        string            `json:"type,omitempty" doc:"Bead type."`
		Priority    *int              `json:"priority,omitempty" doc:"Bead priority."`
		Assignee    string            `json:"assignee,omitempty" doc:"Assigned agent."`
		Description string            `json:"description,omitempty" doc:"Bead description."`
		Labels      []string          `json:"labels,omitempty" doc:"Bead labels."`
		Parent      string            `json:"parent,omitempty" doc:"Parent bead ID."`
		Metadata    map[string]string `json:"metadata,omitempty" doc:"Metadata key-value pairs to set at create time."`
		DeferUntil  *time.Time        `json:"defer_until,omitempty" doc:"Hide the bead from ready views until this time."`
	}
}

// BeadCloseInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/close.
type BeadCloseInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadReopenInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/reopen.
type BeadReopenInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadUpdateInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/update and PATCH /v0/city/{cityName}/bead/{id}.
type BeadUpdateInput struct {
	CityScope
	ID   string `path:"id" doc:"Bead ID."`
	Body beadUpdateBody
}

// beadUpdateBody is the request body for bead update/patch endpoints.
type beadUpdateBody struct {
	Title        *string           `json:"title,omitempty" doc:"Bead title."`
	Status       *string           `json:"status,omitempty" doc:"Bead status."`
	Type         *string           `json:"type,omitempty" doc:"Bead type."`
	Priority     *int              `json:"priority,omitempty" doc:"Bead priority."`
	Assignee     *string           `json:"assignee,omitempty" doc:"Assigned agent."`
	Description  *string           `json:"description,omitempty" doc:"Bead description."`
	Labels       []string          `json:"labels,omitempty" doc:"Bead labels."`
	RemoveLabels []string          `json:"remove_labels,omitempty" doc:"Labels to remove."`
	Parent       *string           `json:"parent,omitempty" nullable:"true" doc:"Parent bead ID. Use null or an empty string to clear."`
	Metadata     map[string]string `json:"metadata,omitempty" doc:"Metadata key-value pairs to set."`
	parentSet    bool
}

// UnmarshalJSON rejects `"priority": null` explicitly. Standard Go JSON decoding
// folds null and absent into a nil pointer, which silently drops clear-intent
// requests. Clients that want to clear priority must use a dedicated endpoint
// (not yet available); until then, null is a 400.
func (b *beadUpdateBody) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if p, ok := raw["priority"]; ok {
		trimmed := bytes.TrimSpace(p)
		if bytes.Equal(trimmed, []byte("null")) {
			return fmt.Errorf("clearing priority via null is not supported; omit the field to leave it unchanged")
		}
	}
	type alias beadUpdateBody
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*b = beadUpdateBody(a)
	if p, ok := raw["parent"]; ok {
		b.parentSet = true
		if bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
			parent := ""
			b.Parent = &parent
		}
	}
	return nil
}

// BeadAssignInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/assign.
type BeadAssignInput struct {
	CityScope
	ID   string `path:"id" doc:"Bead ID."`
	Body struct {
		Assignee string `json:"assignee,omitempty" doc:"Assignee name."`
	}
}

// BeadReleaseIfCurrentInput is the Huma input for
// POST /v0/city/{cityName}/bead/{id}/release-if-current — an atomic
// compare-and-swap release of a bead's assignment.
type BeadReleaseIfCurrentInput struct {
	CityScope
	ID   string `path:"id" doc:"Bead ID."`
	Body struct {
		ExpectedAssignee string `json:"expected_assignee,omitempty" doc:"Release the assignment only if the bead is currently assigned to this agent (compare-and-swap)."`
	}
}

// BeadClaimInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/claim —
// an atomic claim of a bead's assignment for the given agent.
type BeadClaimInput struct {
	CityScope
	ID   string `path:"id" doc:"Bead ID."`
	Body struct {
		Assignee string `json:"assignee,omitempty" doc:"Agent to claim the bead for. The claim is atomic (rejected if the bead is already assigned to a different agent)."`
	}
}

// BeadClaimResult is the body of a claim response.
type BeadClaimResult struct {
	Claimed bool        `json:"claimed" doc:"Whether the claim succeeded (false if the bead was not claimable, e.g. already assigned)."`
	Bead    *beads.Bead `json:"bead,omitempty" doc:"The claimed bead, populated when claimed=true."`
}

// BeadDeleteInput is the Huma input for DELETE /v0/city/{cityName}/bead/{id}.
type BeadDeleteInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}
