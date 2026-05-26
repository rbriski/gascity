package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SupervisorHTTPCheck probes the supervisor HTTP API after the socket check passes.
type SupervisorHTTPCheck struct {
	supervisorRunning bool
	loadConfig        func(string) (supervisor.Config, error)
	configPath        func() string
	client            httpDoer
}

// NewSupervisorHTTPCheck creates a supervisor API reachability check.
func NewSupervisorHTTPCheck(supervisorRunning bool) *SupervisorHTTPCheck {
	return &SupervisorHTTPCheck{
		supervisorRunning: supervisorRunning,
		loadConfig:        supervisor.LoadConfig,
		configPath:        supervisor.ConfigPath,
		client:            &http.Client{Timeout: 2 * time.Second},
	}
}

// Name returns the check identifier.
func (c *SupervisorHTTPCheck) Name() string { return "supervisor-http-api" }

// Run probes the configured supervisor HTTP API port.
func (c *SupervisorHTTPCheck) Run(_ *CheckContext) *CheckResult {
	result := &CheckResult{Name: c.Name()}
	if !c.supervisorRunning {
		result.Status = StatusOK
		result.Message = "supervisor HTTP API check skipped (supervisor socket not reachable)"
		return result
	}

	loadConfig := c.loadConfig
	if loadConfig == nil {
		loadConfig = supervisor.LoadConfig
	}
	configPath := c.configPath
	if configPath == nil {
		configPath = supervisor.ConfigPath
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("supervisor HTTP API config: %v", err)
		return result
	}
	port := cfg.Supervisor.PortOrDefault()
	url := fmt.Sprintf("http://127.0.0.1:%d/v0/cities", port)

	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("supervisor HTTP API probe setup for port %d: %v", port, err)
		return result
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("supervisor socket OK but HTTP API unreachable on port %d (%s)", port, supervisorHTTPProbeFailureReason(err))
		return result
	}
	if resp.Body != nil {
		defer resp.Body.Close() //nolint:errcheck
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.Status = StatusError
		result.Message = fmt.Sprintf("supervisor socket OK but HTTP API unreachable on port %d (non-2xx HTTP %d)", port, resp.StatusCode)
		return result
	}
	result.Status = StatusOK
	result.Message = fmt.Sprintf("supervisor socket OK, HTTP API reachable on port %d", port)
	return result
}

// CanFix returns false because the check is read-only.
func (c *SupervisorHTTPCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *SupervisorHTTPCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this probe is only run by gc doctor.
func (c *SupervisorHTTPCheck) WarmupEligible() bool { return false }

func supervisorHTTPProbeFailureReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	return err.Error()
}
