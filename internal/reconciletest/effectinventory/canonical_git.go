package effectinventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type gitSnapshot struct {
	repoRoot     string
	revision     string
	headIdentity string
	dirty        bool
}

func probeGitRepository(ctx context.Context, repoRoot string) (gitSnapshot, error) {
	absoluteRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return gitSnapshot{}, fmt.Errorf("resolving repository root: %w", err)
	}
	absoluteRoot = filepath.Clean(absoluteRoot)

	rootOutput, err := gitOutput(ctx, absoluteRoot, "resolve worktree root", "rev-parse", "--show-toplevel")
	if err != nil {
		return gitSnapshot{}, err
	}
	resolvedRoot := filepath.Clean(strings.TrimSpace(rootOutput))
	if resolvedRoot == "" || !filepath.IsAbs(resolvedRoot) {
		return gitSnapshot{}, fmt.Errorf("git returned an invalid worktree root")
	}

	revision, err := gitOutput(ctx, resolvedRoot, "resolve HEAD revision", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return gitSnapshot{}, err
	}
	revision = strings.TrimSpace(revision)
	if !lowerHex(revision, 40) {
		return gitSnapshot{}, fmt.Errorf("git returned a non-canonical HEAD revision")
	}

	reference, exitCode, err := gitOutputWithExit(ctx, resolvedRoot, "symbolic-ref", "-q", "HEAD")
	if err != nil && exitCode != 1 {
		return gitSnapshot{}, fmt.Errorf("resolve HEAD identity: %w", err)
	}
	reference = strings.TrimSpace(reference)
	headIdentity := "detached@" + revision
	if exitCode == 0 {
		if !strings.HasPrefix(reference, "refs/") || strings.ContainsAny(reference, "\r\n") {
			return gitSnapshot{}, fmt.Errorf("git returned an invalid symbolic HEAD")
		}
		headIdentity = "ref:" + reference + "@" + revision
	}

	status, err := gitOutput(ctx, resolvedRoot, "inspect worktree status", "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return gitSnapshot{}, err
	}
	return gitSnapshot{
		repoRoot:     resolvedRoot,
		revision:     revision,
		headIdentity: headIdentity,
		dirty:        strings.TrimSpace(status) != "",
	}, nil
}

func gitOutput(ctx context.Context, repoRoot, operation string, args ...string) (string, error) {
	output, exitCode, err := gitOutputWithExit(ctx, repoRoot, args...)
	if err == nil {
		return output, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return "", fmt.Errorf("git %s exited with status %d", operation, exitCode)
}

func gitOutputWithExit(ctx context.Context, repoRoot string, args ...string) (string, int, error) {
	commandArgs := make([]string, 0, len(args)+2)
	commandArgs = append(commandArgs, "-c", "safe.directory="+repoRoot)
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Dir = repoRoot
	command.Env = deterministicGitEnvironment(os.Environ())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", -1, ctxErr
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return "", -1, fmt.Errorf("starting git: %w", err)
	}
	return stdout.String(), exitError.ExitCode(), fmt.Errorf("git exited with status %s", strconv.Itoa(exitError.ExitCode()))
}

func deterministicGitEnvironment(inherited []string) []string {
	environment := make([]string, 0, len(inherited)+10)
	for _, item := range inherited {
		name, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(name, "GIT_") || name == "LANG" || name == "LC_ALL" {
			continue
		}
		environment = append(environment, item)
	}
	return append(environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_ATTR_NOSYSTEM=1",
		"LANG=C",
		"LC_ALL=C",
	)
}
