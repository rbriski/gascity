package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

type builtinPackRegistryMigrationCheck struct {
	cityPath string
}

func newBuiltinPackRegistryMigrationCheck(cityPath string) *builtinPackRegistryMigrationCheck {
	return &builtinPackRegistryMigrationCheck{cityPath: cityPath}
}

func (c *builtinPackRegistryMigrationCheck) Name() string { return "builtin-pack-registry" }

func (c *builtinPackRegistryMigrationCheck) CanFix() bool { return true }

func (c *builtinPackRegistryMigrationCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	issues, err := inspectBuiltinPackRegistryMigration(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("inspecting bundled pack imports: %v", err)
		return r
	}
	if len(issues.legacyRefs) == 0 && len(issues.missingDefaults) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "bundled pack imports use the pack registry"
		return r
	}
	r.Status = doctor.StatusError
	r.Message = "bundled pack imports need registry migration"
	for _, detail := range issues.legacyRefs {
		r.Details = append(r.Details, "legacy source: "+detail)
	}
	for _, name := range issues.missingDefaults {
		r.Details = append(r.Details, "missing explicit default import: "+name)
	}
	r.FixHint = `run "gc doctor --fix" to rewrite .gc/system/packs sources and add missing bundled imports`
	return r
}

func (c *builtinPackRegistryMigrationCheck) Fix(_ *doctor.CheckContext) error {
	cityCfg, err := loadCityConfigForBuiltinPackMigration(c.cityPath)
	if err != nil {
		return err
	}
	packCfg, packExists, err := loadPackConfigForBuiltinPackMigration(c.cityPath)
	if err != nil {
		return err
	}

	rewriteImportSources(c.cityPath, packCfg.Imports)
	rewriteImportSources(c.cityPath, packCfg.Defaults.Rig.Imports)
	rewriteImportSources(c.cityPath, cityCfg.Imports)
	for i := range cityCfg.Rigs {
		rewriteImportSources(c.cityPath, cityCfg.Rigs[i].Imports)
	}

	present := bundledImportNames(cityCfg, packCfg)
	includeGastown := present["gastown"]
	required := requiredDefaultBuiltinImportNames(c.cityPath, cityCfg, includeGastown)
	if packCfg.Imports == nil {
		packCfg.Imports = make(map[string]config.Import)
	}
	for _, name := range required {
		if present[name] {
			continue
		}
		packCfg.Imports[name] = config.Import{Source: builtinpacks.MustSource(name)}
		present[name] = true
	}

	if err := writeBuiltinMigrationCityConfig(c.cityPath, cityCfg); err != nil {
		return err
	}
	if packExists || len(packCfg.Imports) > 0 || len(packCfg.Defaults.Rig.Imports) > 0 {
		if err := writeBuiltinMigrationPackConfig(c.cityPath, packCfg); err != nil {
			return err
		}
	}
	return ensureBundledImportLockEntries(c.cityPath, collectBundledImportsForLock(cityCfg, packCfg))
}

type builtinPackRegistryIssues struct {
	legacyRefs      []string
	missingDefaults []string
}

func inspectBuiltinPackRegistryMigration(cityPath string) (builtinPackRegistryIssues, error) {
	cityCfg, err := loadCityConfigForBuiltinPackMigration(cityPath)
	if err != nil {
		return builtinPackRegistryIssues{}, err
	}
	packCfg, packExists, err := loadPackConfigForBuiltinPackMigration(cityPath)
	if err != nil {
		return builtinPackRegistryIssues{}, err
	}

	var issues builtinPackRegistryIssues
	issues.legacyRefs = append(issues.legacyRefs, legacyImportDetails(cityPath, "pack.toml imports", packCfg.Imports)...)
	issues.legacyRefs = append(issues.legacyRefs, legacyImportDetails(cityPath, "pack.toml defaults.rig.imports", packCfg.Defaults.Rig.Imports)...)
	issues.legacyRefs = append(issues.legacyRefs, legacyImportDetails(cityPath, "city.toml imports", cityCfg.Imports)...)
	for _, rig := range cityCfg.Rigs {
		label := fmt.Sprintf("city.toml rig %q imports", rig.Name)
		issues.legacyRefs = append(issues.legacyRefs, legacyImportDetails(cityPath, label, rig.Imports)...)
	}
	sort.Strings(issues.legacyRefs)

	present := bundledImportNames(cityCfg, packCfg)
	if !packExists && len(present) == 0 && len(issues.legacyRefs) == 0 {
		return issues, nil
	}
	required := requiredDefaultBuiltinImportNames(cityPath, cityCfg, present["gastown"])
	for _, name := range required {
		if !present[name] {
			issues.missingDefaults = append(issues.missingDefaults, name)
		}
	}
	sort.Strings(issues.missingDefaults)
	return issues, nil
}

func loadCityConfigForBuiltinPackMigration(cityPath string) (*config.City, error) {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadPackConfigForBuiltinPackMigration(cityPath string) (initPackConfig, bool, error) {
	path := filepath.Join(cityPath, "pack.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newInitPackConfig(filepath.Base(filepath.Clean(cityPath))), false, nil
		}
		return initPackConfig{}, false, fmt.Errorf("reading pack.toml: %w", err)
	}
	cfg := newInitPackConfig(filepath.Base(filepath.Clean(cityPath)))
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return initPackConfig{}, true, fmt.Errorf("parsing pack.toml: %w", err)
	}
	if cfg.Pack.Name == "" {
		cfg.Pack.Name = filepath.Base(filepath.Clean(cityPath))
	}
	if cfg.Pack.Schema == 0 {
		cfg.Pack.Schema = initPackSchemaVersion
	}
	return cfg, true, nil
}

func legacyImportDetails(cityPath, label string, imports map[string]config.Import) []string {
	var details []string
	for name, imp := range imports {
		if packName, ok := legacySystemPackSourceName(cityPath, imp.Source); ok {
			details = append(details, fmt.Sprintf("%s.%s %q -> %s", label, name, imp.Source, builtinpacks.MustSource(packName)))
		}
	}
	return details
}

func rewriteImportSources(cityPath string, imports map[string]config.Import) {
	for name, imp := range imports {
		packName, ok := legacySystemPackSourceName(cityPath, imp.Source)
		if !ok {
			continue
		}
		imp.Source = builtinpacks.MustSource(packName)
		imports[name] = imp
	}
}

func bundledImportNames(cityCfg *config.City, packCfg initPackConfig) map[string]bool {
	present := make(map[string]bool)
	add := func(imports map[string]config.Import) {
		for _, imp := range imports {
			if name, ok := builtinpacks.NameForSource(imp.Source); ok {
				present[name] = true
			}
			if name, ok := legacySystemPackSourceName("", imp.Source); ok {
				present[name] = true
			}
		}
	}
	add(packCfg.Imports)
	add(packCfg.Defaults.Rig.Imports)
	if cityCfg != nil {
		add(cityCfg.Imports)
		for _, rig := range cityCfg.Rigs {
			add(rig.Imports)
		}
	}
	return present
}

func requiredDefaultBuiltinImportNames(cityPath string, cfg *config.City, includeGastown bool) []string {
	var names []string
	names = append(names, "core")
	if initShouldIncludeBDImport(cityPath, cfg) {
		names = append(names, "bd")
	}
	if includeGastown {
		names = append(names, "gastown")
		return names
	}
	names = append(names, "maintenance")
	return names
}

func collectBundledImportsForLock(cityCfg *config.City, packCfg initPackConfig) map[string]config.Import {
	out := make(map[string]config.Import)
	add := func(imports map[string]config.Import) {
		for name, imp := range imports {
			if builtinpacks.IsSource(imp.Source) {
				out[name] = imp
			}
		}
	}
	add(packCfg.Imports)
	add(packCfg.Defaults.Rig.Imports)
	if cityCfg != nil {
		add(cityCfg.Imports)
		for _, rig := range cityCfg.Rigs {
			add(rig.Imports)
		}
	}
	return out
}

func legacySystemPackSourceName(cityPath, source string) (string, bool) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", false
	}
	if filepath.IsAbs(source) && cityPath != "" {
		rel, err := filepath.Rel(cityPath, source)
		if err == nil {
			source = rel
		}
	}
	clean := filepath.ToSlash(filepath.Clean(source))
	clean = strings.TrimPrefix(clean, "./")
	prefix := filepath.ToSlash(citylayout.SystemPacksRoot) + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(clean, prefix)
	name := strings.Split(rest, "/")[0]
	if _, ok := builtinpacks.Source(name); !ok {
		return "", false
	}
	return name, true
}

func writeBuiltinMigrationCityConfig(cityPath string, cfg *config.City) error {
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), data, 0o644)
}

func writeBuiltinMigrationPackConfig(cityPath string, cfg initPackConfig) error {
	data, err := marshalInitPackConfig(cfg)
	if err != nil {
		return err
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, filepath.Join(cityPath, "pack.toml"), data, 0o644)
}

func ensureBundledImportLockEntries(cityPath string, imports map[string]config.Import) error {
	if len(imports) == 0 {
		return nil
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, imp := range imports {
		resolved, err := packman.ResolveVersion(imp.Source, imp.Version)
		if err != nil {
			return fmt.Errorf("resolving bundled import %s: %w", imp.Source, err)
		}
		if _, err := packman.EnsureRepoInCache(imp.Source, resolved.Commit); err != nil {
			return fmt.Errorf("installing bundled import %s: %w", imp.Source, err)
		}
		lock.Packs[imp.Source] = packman.LockedPack{
			Version: resolved.Version,
			Commit:  resolved.Commit,
			Fetched: now,
		}
	}
	return packman.WriteLockfile(fsys.OSFS{}, cityPath, lock)
}
