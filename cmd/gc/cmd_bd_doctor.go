package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

var (
	bdDoctorResolveCity    = resolveBdCity
	bdDoctorLoadCityConfig = func(cityPath string, stderr io.Writer) (*config.City, error) {
		return loadCityConfig(cityPath, stderr)
	}
	bdDoctorResolveScopeTarget  = resolveBdScopeTarget
	bdDoctorReadProjectIdentity = func(scopeRoot string) (string, bool, error) {
		return contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	}
	bdDoctorDialDoltForScope      = dialDoltForScope
	bdDoctorReadDatabaseProjectID = func(ctx context.Context, db *sql.DB) (string, bool, error) {
		return readDatabaseProjectID(ctx, db)
	}
	bdDoctorUpsertDatabaseProjectIDForce = func(ctx context.Context, db *sql.DB, newID string) (int64, error) {
		return upsertDatabaseProjectIDForce(ctx, db, newID)
	}
	bdDoctorRecordProjectIdentityStamped = recordProjectIdentityStamped
	bdDoctorIsInteractive                = isInteractive
)

// doBdDoctor handles gc-owned bd doctor subcommands before passthrough to bd.
func doBdDoctor(cityName, rigName string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc bd doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var reseedIdentity bool
	var assumeYes bool
	var noInput bool
	fs.BoolVar(&reseedIdentity, "reseed-identity", false, "overwrite the dolt L3 project-identity stamp from L1")
	fs.BoolVar(&assumeYes, "yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(&noInput, "no-input", false, "fail rather than prompt; requires --yes for destructive operations")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !reseedIdentity {
		fmt.Fprintln(stderr, "gc bd doctor: no operation specified; pass --reseed-identity to repair L3 drift") //nolint:errcheck
		fs.PrintDefaults()
		return 2
	}
	return runReseedIdentity(cityName, rigName, fs.Args(), assumeYes, noInput, stdin, stdout, stderr)
}

func runReseedIdentity(cityName, rigName string, tail []string, assumeYes, noInput bool, stdin io.Reader, stdout, stderr io.Writer) int {
	cityPath, err := bdDoctorResolveCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: %v\n", err) //nolint:errcheck
		return 1
	}
	cfg, err := bdDoctorLoadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: loading config: %v\n", err) //nolint:errcheck
		return 1
	}
	target, err := bdDoctorResolveScopeTarget(cfg, cityPath, rigName, tail)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: %v\n", err) //nolint:errcheck
		return 1
	}
	l1ID, ok, err := bdDoctorReadProjectIdentity(target.ScopeRoot)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: reading L1 at %s: %v\n", target.ScopeRoot, err) //nolint:errcheck
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "gc bd doctor: L1 identity.toml is absent at %s; nothing to reseed from\n", target.ScopeRoot) //nolint:errcheck
		return 1
	}

	db, doltOK, err := bdDoctorDialDoltForScope(cityPath, target.ScopeRoot)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: connecting to dolt: %v\n", err) //nolint:errcheck
		return 1
	}
	if !doltOK {
		fmt.Fprintf(stderr, "gc bd doctor: dolt server unavailable for %s; cannot reseed\n", target.ScopeRoot) //nolint:errcheck
		return 1
	}
	if db != nil {
		defer func() { _ = db.Close() }()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldID, _, err := bdDoctorReadDatabaseProjectID(ctx, db)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: reading L3: %v\n", err) //nolint:errcheck
		return 1
	}

	fmt.Fprintln(stdout, "gc bd doctor: about to overwrite L3 (dolt metadata._project_id) from L1") //nolint:errcheck
	fmt.Fprintf(stdout, "  scope:      %s\n", target.ScopeRoot)                                     //nolint:errcheck
	fmt.Fprintf(stdout, "  current L3: %s\n", truncateForDisplay(oldID))                            //nolint:errcheck
	fmt.Fprintf(stdout, "  L1:         %s\n", truncateForDisplay(l1ID))                             //nolint:errcheck
	fmt.Fprintln(stdout, "This is destructive; nothing else in gc clobbers a non-empty L3 stamp.")  //nolint:errcheck

	if !assumeYes {
		if noInput || !bdDoctorIsInteractive(stdin) {
			fmt.Fprintln(stderr, "gc bd doctor: refusing to reseed without --yes; rerun with `--yes` to confirm") //nolint:errcheck
			return 1
		}
		if !confirmYes(stdin, stdout, "Continue? [yes/NO]: ") {
			fmt.Fprintln(stdout, "gc bd doctor: refused; L3 unchanged") //nolint:errcheck
			return 0
		}
	}

	if _, err := bdDoctorUpsertDatabaseProjectIDForce(ctx, db, l1ID); err != nil {
		fmt.Fprintf(stderr, "gc bd doctor: writing L3: %v\n", err) //nolint:errcheck
		return 1
	}
	bdDoctorRecordProjectIdentityStamped(stderr, cityPath, target.ScopeRoot, oldID, l1ID)
	fmt.Fprintln(stdout, "reseeded; event project.identity.stamped emitted (Source=cache_repair, Layer=L3)") //nolint:errcheck
	return 0
}

func truncateForDisplay(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 24 {
		return id
	}
	return id[:8] + "..." + id[len(id)-4:]
}

func confirmYes(stdin io.Reader, stdout io.Writer, prompt string) bool {
	fmt.Fprint(stdout, prompt) //nolint:errcheck
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

func isInteractive(r io.Reader) bool {
	file, ok := r.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func dialDoltForScope(cityPath, scopeRoot string) (*sql.DB, bool, error) {
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil || !ok {
		return nil, false, err
	}
	db, err := managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, false, nil
	}
	return db, true, nil
}

func recordProjectIdentityStamped(stderr io.Writer, cityPath, scopeRoot, oldID, newID string) {
	rec, closeRecorder := openProjectIdentityEventRecorder(cityPath, stderr)
	defer closeRecorder()
	emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", oldID, newID)
}
