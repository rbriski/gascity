//go:build integration && windows

package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/windows"
)

func TestCredentialProviderWindowsJobKillsDescendants(t *testing.T) {
	pidPath := t.TempDir() + `\descendant.pid`
	escapedPIDPath := strings.ReplaceAll(pidPath, `'`, `''`)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	response := fmt.Sprintf(
		`{"version":"%s","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"%s","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]}`,
		ProtocolVersion,
		expiresAt,
	)
	script := strings.Join([]string{
		`$child = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 30') -WindowStyle Hidden -PassThru`,
		`[System.IO.File]::WriteAllText('` + escapedPIDPath + `', [string]$child.Id)`,
		`[Console]::Out.WriteLine('` + response + `')`,
		`[Console]::Out.Flush()`,
		`$child.WaitForExit()`,
	}, "; ")
	provider, err := New([]string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, mintErr := provider.Mint(ctx, validCredentialRequest())
		done <- mintErr
	}()

	pid := waitForWindowsPIDFile(t, pidPath)
	process, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		t.Fatalf("open descendant process %d: %v", pid, err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(process)
	})
	cancel()

	select {
	case mintErr := <-done:
		if !errors.Is(mintErr, context.Canceled) {
			t.Fatalf("Mint error = %v, want context cancellation", mintErr)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("Mint did not return after cancellation")
	}
	event, err := windows.WaitForSingleObject(process, uint32(testutil.ExecRaceTimeout/time.Millisecond))
	if err != nil {
		t.Fatalf("wait for descendant process %d: %v", pid, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant wait result = %#x, want WAIT_OBJECT_0", event)
	}
}

func TestCredentialProviderWindowsJobCloseKillsDescendantsAfterParentExit(t *testing.T) {
	dir := t.TempDir()
	pidPath := dir + `\descendant.pid`
	releasePath := dir + `\release-parent`
	escapedPIDPath := strings.ReplaceAll(pidPath, `'`, `''`)
	escapedReleasePath := strings.ReplaceAll(releasePath, `'`, `''`)
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	response := fmt.Sprintf(
		`{"version":"%s","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"%s","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]}`,
		ProtocolVersion,
		expiresAt,
	)
	script := strings.Join([]string{
		`$child = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 30') -NoNewWindow -PassThru`,
		`[System.IO.File]::WriteAllText('` + escapedPIDPath + `', [string]$child.Id)`,
		`while (-not [System.IO.File]::Exists('` + escapedReleasePath + `')) { Start-Sleep -Milliseconds 10 }`,
		`[Console]::Out.WriteLine('` + response + `')`,
		`[Console]::Out.Flush()`,
		`exit 0`,
	}, "; ")
	done := make(chan struct {
		output commandOutput
		err    error
	}, 1)
	go func() {
		output, runErr := runCommand(
			context.Background(),
			[]string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script},
			nil,
			minimalEnvironment(os.Environ()),
		)
		done <- struct {
			output commandOutput
			err    error
		}{output: output, err: runErr}
	}()

	pid := waitForWindowsPIDFile(t, pidPath)
	process, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		t.Fatalf("open descendant process %d: %v", pid, err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(process)
	})
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release provider parent: %v", err)
	}

	select {
	case result := <-done:
		if !errors.Is(result.err, exec.ErrWaitDelay) {
			t.Fatalf("runCommand error = %v, want exec.ErrWaitDelay", result.err)
		}
		if got, want := string(result.output.stdout), response+"\r\n"; got != want {
			t.Fatalf("stdout = %q, want exact response %q", got, want)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("runCommand did not bound descendant-held response pipes after the provider parent exited")
	}
	event, err := windows.WaitForSingleObject(process, uint32(testutil.ExecRaceTimeout/time.Millisecond))
	if err != nil {
		t.Fatalf("wait for descendant process %d: %v", pid, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant wait result = %#x, want WAIT_OBJECT_0", event)
	}
}

func waitForWindowsPIDFile(t *testing.T, path string) int {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(testutil.ExecRaceTimeout)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr != nil || pid <= 1 {
				t.Fatalf("descendant pid = %q: %v", raw, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read descendant pid: %v", err)
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for descendant pid file %s", path)
		}
	}
}
