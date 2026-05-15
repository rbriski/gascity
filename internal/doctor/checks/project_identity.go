package checks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ProjectIdentityCheck reports drift between the canonical L1 identity,
// metadata cache, and Dolt database stamp for city and rig bead scopes.
type ProjectIdentityCheck struct {
	resolveScopes func(cityPath string) ([]projectIdentityScope, error)
	readL3        func(cityPath string, scope projectIdentityScope) (id string, ok bool, reachable bool, err error)
	fs            fsys.FS
}

type projectIdentityScope struct {
	Root string
	Kind string
	Name string
}

type piClass int

const (
	piOK piClass = iota
	piMigrationFixable
	piL2DriftFixable
	piL3DriftUnfixable
	piL3Unverifiable
)

type piOutcome struct {
	Class        piClass
	Message      string
	RepairL2From string
}

// NewProjectIdentityCheck binds the project-identity doctor check to the
// production filesystem, scope resolver, and Dolt reader.
func NewProjectIdentityCheck() *ProjectIdentityCheck {
	return &ProjectIdentityCheck{
		resolveScopes: defaultProjectIdentityScopes,
		readL3:        defaultProjectIdentityL3,
		fs:            fsys.OSFS{},
	}
}

// Name returns the stable doctor check name.
func (c *ProjectIdentityCheck) Name() string { return "project-identity" }

// Run evaluates identity consistency for every configured bead scope.
func (c *ProjectIdentityCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	check := c.withDefaults()
	result := &doctor.CheckResult{Name: check.Name()}
	scopes, err := check.resolveScopes(ctx.CityPath)
	if err != nil {
		result.Status = doctor.StatusError
		result.Message = fmt.Sprintf("resolving scopes: %v", err)
		return result
	}

	var (
		fixableCount     int
		l3MismatchCount  int
		unverifiableHits int
		l3Details        []string
		fixableDetails   []string
		verboseDetails   []string
	)
	for _, scope := range scopes {
		outcome, err := classifyProjectIdentity(check.fs, check.readL3, ctx.CityPath, scope)
		if err != nil {
			result.Status = doctor.StatusError
			result.Message = fmt.Sprintf("checking %s: %v", scope.Root, err)
			return result
		}
		detail := fmt.Sprintf("%s: %s", scope.Root, outcome.Message)
		switch outcome.Class {
		case piOK:
		case piMigrationFixable, piL2DriftFixable:
			fixableCount++
			fixableDetails = append(fixableDetails, detail)
		case piL3DriftUnfixable:
			l3MismatchCount++
			l3Details = append(l3Details, detail)
		case piL3Unverifiable:
			unverifiableHits++
			if ctx.Verbose {
				verboseDetails = append(verboseDetails, detail)
			}
		}
	}
	result.Details = append(result.Details, l3Details...)
	result.Details = append(result.Details, fixableDetails...)
	result.Details = append(result.Details, verboseDetails...)

	switch {
	case l3MismatchCount > 0:
		result.Status = doctor.StatusError
		result.Message = fmt.Sprintf("%d scope(s) with L3 drift (manual reseed required)", l3MismatchCount)
		result.FixHint = `for each scope, run "gc bd doctor --reseed-identity" after confirming L1 is the value to keep`
	case fixableCount > 0:
		result.Status = doctor.StatusWarning
		result.Message = fmt.Sprintf("%d scope(s) with fixable identity drift", fixableCount)
		result.FixHint = `run "gc doctor --fix" to regenerate L2 caches from L1`
	default:
		result.Status = doctor.StatusOK
		switch {
		case len(scopes) == 0:
			result.Message = "no scopes configured"
		case unverifiableHits > 0:
			result.Message = fmt.Sprintf("identity OK in %d scope(s); %d unverifiable (dolt unreachable)", len(scopes)-unverifiableHits, unverifiableHits)
		default:
			result.Message = fmt.Sprintf("identity consistent across %d scope(s)", len(scopes))
		}
	}
	return result
}

// CanFix reports that the check can repair L2 cache drift.
func (c *ProjectIdentityCheck) CanFix() bool { return true }

// Fix regenerates only the L2 metadata cache from L1 for fixable scopes.
func (c *ProjectIdentityCheck) Fix(ctx *doctor.CheckContext) error {
	check := c.withDefaults()
	scopes, err := check.resolveScopes(ctx.CityPath)
	if err != nil {
		return fmt.Errorf("resolving scopes: %w", err)
	}
	for _, scope := range scopes {
		outcome, err := classifyProjectIdentity(check.fs, check.readL3, ctx.CityPath, scope)
		if err != nil {
			return fmt.Errorf("checking %s: %w", scope.Root, err)
		}
		switch outcome.Class {
		case piL2DriftFixable, piMigrationFixable:
			if outcome.RepairL2From == "" {
				continue
			}
			if err := writeProjectIdentityL2Cache(check.fs, scope.Root, outcome.RepairL2From); err != nil {
				return fmt.Errorf("repairing L2 cache for %s: %w", scope.Root, err)
			}
		case piL3DriftUnfixable:
			continue
		}
	}
	return nil
}

func (c *ProjectIdentityCheck) withDefaults() *ProjectIdentityCheck {
	cp := *c
	if cp.resolveScopes == nil {
		cp.resolveScopes = defaultProjectIdentityScopes
	}
	if cp.readL3 == nil {
		cp.readL3 = defaultProjectIdentityL3
	}
	if cp.fs == nil {
		cp.fs = fsys.OSFS{}
	}
	return &cp
}

func classifyProjectIdentity(
	fs fsys.FS,
	readL3 func(cityPath string, scope projectIdentityScope) (string, bool, bool, error),
	cityPath string,
	scope projectIdentityScope,
) (piOutcome, error) {
	l1, l1OK, err := contract.ReadProjectIdentity(fs, scope.Root)
	if err != nil {
		return piOutcome{}, err
	}
	l2, l2OK, err := readProjectIdentityL2Cache(fs, scope.Root)
	if err != nil {
		return piOutcome{}, err
	}
	l3, l3OK, reachable, err := readL3(cityPath, scope)
	if err != nil {
		return piOutcome{}, err
	}
	if !reachable {
		if l1OK {
			return piOutcome{Class: piL3Unverifiable, Message: "dolt unavailable; L3 layer not verified"}, nil
		}
		return piOutcome{Class: piMigrationFixable, Message: "L1 absent; dolt unreachable - migration deferred"}, nil
	}

	if !l1OK {
		switch {
		case !l2OK && !l3OK:
			return piOutcome{Class: piMigrationFixable, Message: "scope not yet migrated to identity.toml"}, nil
		case l2OK && (!l3OK || l2 == l3):
			return piOutcome{Class: piMigrationFixable, Message: "L1 absent; will adopt id from L2 on next gc bd run"}, nil
		case l2OK && l3OK && l2 != l3:
			return piOutcome{Class: piMigrationFixable, Message: "L1 absent; legacy mismatch - see gc bd doctor"}, nil
		default:
			return piOutcome{Class: piMigrationFixable, Message: "L1 absent; will adopt id from L3 on next gc bd run"}, nil
		}
	}

	if l3OK && l3 != l1 {
		if l2OK && l2 != l1 {
			return piOutcome{Class: piL3DriftUnfixable, Message: "L2 and L3 both differ from L1; reseed required"}, nil
		}
		return piOutcome{Class: piL3DriftUnfixable, Message: "L3 dolt stamp differs from L1; reseed required"}, nil
	}
	if !l2OK || l2 != l1 {
		return piOutcome{Class: piL2DriftFixable, Message: "L2 cache differs from L1; run gc doctor --fix", RepairL2From: l1}, nil
	}
	return piOutcome{Class: piOK}, nil
}

func defaultProjectIdentityScopes(cityPath string) ([]projectIdentityScope, error) {
	cityPath = filepath.Clean(strings.TrimSpace(cityPath))
	if cityPath == "." || cityPath == "" {
		return nil, fmt.Errorf("missing city path")
	}
	scopes := []projectIdentityScope{{Root: cityPath, Kind: "city"}}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	for _, rig := range cfg.Rigs {
		root := strings.TrimSpace(rig.Path)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(cityPath, root)
		}
		scopes = append(scopes, projectIdentityScope{
			Root: filepath.Clean(root),
			Kind: "rig",
			Name: rig.Name,
		})
	}
	return scopes, nil
}

func defaultProjectIdentityL3(cityPath string, scope projectIdentityScope) (string, bool, bool, error) {
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scope.Root)
	if err != nil {
		if errors.Is(err, contract.ErrManagedRuntimeUnavailable) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(target.Port))
	if err != nil || port <= 0 {
		return "", false, false, nil
	}
	authRoot := doltauth.AuthScopeRoot(cityPath, scope.Root, target)
	auth := doltauth.Resolve(authRoot, strings.TrimSpace(target.User), strings.TrimSpace(target.Host), port)
	user := strings.TrimSpace(auth.User)
	if user == "" {
		user = "root"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = auth.Password
	cfg.Net = "tcp"
	cfg.Addr = strings.TrimSpace(target.Host) + ":" + strings.TrimSpace(target.Port)
	cfg.DBName = strings.TrimSpace(target.Database)
	cfg.Timeout = 2 * time.Second
	cfg.ReadTimeout = 2 * time.Second
	cfg.WriteTimeout = 2 * time.Second
	cfg.AllowNativePasswords = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return "", false, false, err
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return "", false, false, nil
	}
	id, ok, err := readProjectIdentityL3DB(ctx, db)
	if err != nil {
		if projectIdentityTableMissing(err) {
			return "", false, true, nil
		}
		return "", false, true, err
	}
	return id, ok, true, nil
}

func readProjectIdentityL2Cache(fs fsys.FS, scopeRoot string) (string, bool, error) {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read metadata %s: %w", path, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", false, fmt.Errorf("parse metadata %s: %w", path, err)
	}
	id := strings.TrimSpace(fmt.Sprint(meta["project_id"]))
	if id == "" || id == "<nil>" || strings.EqualFold(id, "null") {
		return "", false, nil
	}
	return id, true, nil
}

func writeProjectIdentityL2Cache(fs fsys.FS, scopeRoot string, id string) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	data, err := fs.ReadFile(path)
	if err != nil {
		return err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parse metadata %s: %w", path, err)
	}
	if strings.TrimSpace(fmt.Sprint(meta["project_id"])) == id {
		return nil
	}
	meta["project_id"] = id
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return fsys.WriteFileIfChangedAtomic(fs, path, encoded, 0o644)
}

func readProjectIdentityL3DB(ctx context.Context, db *sql.DB) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = '_project_id' LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	id = strings.TrimSpace(id)
	return id, id != "", nil
}

func projectIdentityTableMissing(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "unknown table")
}
