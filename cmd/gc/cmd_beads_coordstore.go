package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

const (
	coordstoreDefaultRetentionPeriod        = 4 * time.Hour
	coordstoreDefaultRetentionSweepInterval = 30 * time.Second
)

func newBeadsCoordstoreCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "coordstore",
		Short:  "Manage the opt-in SQLite-CGo coordstore backend",
		Hidden: true,
	}
	cmd.AddCommand(
		newBeadsCoordstoreImportCmd(stdout, stderr),
		newBeadsCoordstoreShadowCmd(stdout, stderr),
	)
	return cmd
}

func newBeadsCoordstoreImportCmd(stdout, stderr io.Writer) *cobra.Command {
	var cityPath string
	var dryRun bool
	var full bool
	var retention time.Duration
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import city beads from Dolt/bd into SQLite-CGo coordstore",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doBeadsCoordstoreImport(cityPath, dryRun, full, retention, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cityPath, "city", "", "city root (default: resolve from cwd)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count records without writing coordstore")
	cmd.Flags().BoolVar(&full, "full", false, "include terminal beads older than retention")
	cmd.Flags().DurationVar(&retention, "retention", coordstoreDefaultRetentionPeriod, "terminal bead retention horizon")
	return cmd
}

func newBeadsCoordstoreShadowCmd(stdout, stderr io.Writer) *cobra.Command {
	var cityPath string
	var jsonOut bool
	var full bool
	var retention time.Duration
	cmd := &cobra.Command{
		Use:   "shadow",
		Short: "Diff Dolt/bd against SQLite-CGo coordstore",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doBeadsCoordstoreShadow(cityPath, jsonOut, full, retention, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cityPath, "city", "", "city root (default: resolve from cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	cmd.Flags().BoolVar(&full, "full", false, "include terminal beads older than retention")
	cmd.Flags().DurationVar(&retention, "retention", coordstoreDefaultRetentionPeriod, "terminal bead retention horizon")
	return cmd
}

type coordstoreImportSummary struct {
	SourceCount int  `json:"source_count"`
	Filtered    int  `json:"filtered"`
	Imported    int  `json:"imported"`
	Skipped     int  `json:"skipped"`
	Deps        int  `json:"deps"`
	DryRun      bool `json:"dry_run"`
}

type coordstoreShadowSummary struct {
	SchemaVersion  string   `json:"schema_version"`
	SourceCount    int      `json:"source_count"`
	TargetCount    int      `json:"target_count"`
	FilteredSource int      `json:"filtered_source"`
	FilteredTarget int      `json:"filtered_target"`
	Missing        []string `json:"missing,omitempty"`
	Extra          []string `json:"extra,omitempty"`
	Corrupted      []string `json:"corrupted,omitempty"`
	OK             bool     `json:"ok"`
}

type coordstoreRetentionOptions struct {
	Full      bool
	Retention time.Duration
	Now       time.Time
}

func doBeadsCoordstoreImport(cityFlag string, dryRun, full bool, retention time.Duration, stdout, stderr io.Writer) int {
	cityPath, err := resolveCoordstoreCity(cityFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: %v\n", err) //nolint:errcheck
		return 1
	}
	retentionOpts, err := newCoordstoreRetentionOptions(full, retention)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: %v\n", err) //nolint:errcheck
		return 1
	}
	src, err := openBdStoreAt(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: open bd source: %v\n", err) //nolint:errcheck
		return 1
	}
	var dst beads.Store
	if !dryRun {
		period, sweep := retentionOpts.sqliteRetention()
		dst, err = openCoordStoreAtWithRetention(cityPath, cityPath, period, sweep)
		if err != nil {
			fmt.Fprintf(stderr, "gc beads coordstore import: open coordstore target: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	summary, err := copyBeadsIntoCoordstore(src, dst, dryRun, retentionOpts)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: %v\n", err) //nolint:errcheck
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "coordstore import: source=%d filtered=%d imported=%d skipped=%d deps=%d dry_run=%t\n",
		summary.SourceCount, summary.Filtered, summary.Imported, summary.Skipped, summary.Deps, summary.DryRun)
	return 0
}

func doBeadsCoordstoreShadow(cityFlag string, jsonOut, full bool, retention time.Duration, stdout, stderr io.Writer) int {
	cityPath, err := resolveCoordstoreCity(cityFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
		return 1
	}
	retentionOpts, err := newCoordstoreRetentionOptions(full, retention)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
		return 1
	}
	src, err := openBdStoreAt(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: open bd source: %v\n", err) //nolint:errcheck
		return 1
	}
	period, sweep := retentionOpts.sqliteRetention()
	dst, err := openCoordStoreAtWithRetention(cityPath, cityPath, period, sweep)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: open coordstore target: %v\n", err) //nolint:errcheck
		return 1
	}
	summary, err := diffCoordstoreShadow(src, dst, retentionOpts)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
			return 1
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "coordstore shadow: source=%d target=%d filtered_source=%d filtered_target=%d missing=%d extra=%d corrupted=%d ok=%t\n",
			summary.SourceCount, summary.TargetCount, summary.FilteredSource, summary.FilteredTarget, len(summary.Missing), len(summary.Extra), len(summary.Corrupted), summary.OK)
	}
	if !summary.OK {
		return 1
	}
	return 0
}

func resolveCoordstoreCity(cityFlag string) (string, error) {
	if strings.TrimSpace(cityFlag) != "" {
		return filepath.Abs(filepath.Clean(cityFlag))
	}
	return resolveCity()
}

func newCoordstoreRetentionOptions(full bool, retention time.Duration) (coordstoreRetentionOptions, error) {
	if full {
		return coordstoreRetentionOptions{Full: true}, nil
	}
	if retention <= 0 {
		return coordstoreRetentionOptions{}, fmt.Errorf("retention must be positive unless --full is set")
	}
	return coordstoreRetentionOptions{Retention: retention}, nil
}

func (o coordstoreRetentionOptions) sqliteRetention() (time.Duration, time.Duration) {
	if o.Full {
		return 0, 0
	}
	return o.Retention, coordstoreDefaultRetentionSweepInterval
}

func copyBeadsIntoCoordstore(src, dst beads.Store, dryRun bool, retention coordstoreRetentionOptions) (coordstoreImportSummary, error) {
	source, err := loadCoordstoreSnapshot(src, retention, beads.SortCreatedAsc)
	if err != nil {
		return coordstoreImportSummary{}, fmt.Errorf("list source beads: %w", err)
	}
	summary := coordstoreImportSummary{SourceCount: len(source.Beads), Filtered: source.Filtered, DryRun: dryRun}
	for _, b := range source.Beads {
		if !dryRun {
			if dst == nil {
				return summary, fmt.Errorf("coordstore target is required")
			}
			if _, err := dst.Get(b.ID); err == nil {
				summary.Skipped++
			} else if !errors.Is(err, beads.ErrNotFound) {
				return summary, fmt.Errorf("probe target bead %q: %w", b.ID, err)
			} else {
				if _, err := dst.Create(b); err != nil {
					return summary, fmt.Errorf("import bead %q: %w", b.ID, err)
				}
				summary.Imported++
			}
		}
		for _, dep := range source.Deps[b.ID] {
			dep = normalizeCoordstoreDep(dep)
			if !source.IDs[dep.IssueID] || !source.IDs[dep.DependsOnID] {
				continue
			}
			if !dryRun {
				if err := dst.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
					return summary, fmt.Errorf("import dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
				}
			}
			summary.Deps++
		}
	}
	return summary, nil
}

func diffCoordstoreShadow(src, dst beads.Store, retention coordstoreRetentionOptions) (coordstoreShadowSummary, error) {
	source, err := loadCoordstoreSnapshot(src, retention, beads.SortDefault)
	if err != nil {
		return coordstoreShadowSummary{}, fmt.Errorf("list source beads: %w", err)
	}
	target, err := loadCoordstoreSnapshot(dst, retention, beads.SortDefault)
	if err != nil {
		return coordstoreShadowSummary{}, fmt.Errorf("list target beads: %w", err)
	}
	summary := coordstoreShadowSummary{
		SchemaVersion:  "1",
		SourceCount:    len(source.Beads),
		TargetCount:    len(target.Beads),
		FilteredSource: source.Filtered,
		FilteredTarget: target.Filtered,
	}
	corrupted := make(map[string]bool)
	for id, sbead := range source.ByID {
		tbead, ok := target.ByID[id]
		if !ok {
			summary.Missing = append(summary.Missing, id)
			continue
		}
		if coordstoreBeadFingerprint(sbead) != coordstoreBeadFingerprint(tbead) {
			corrupted[id] = true
		}
		srcDeps := coordstoreDepFingerprint(source.Deps[id], source.IDs)
		dstDeps := coordstoreDepFingerprint(target.Deps[id], source.IDs)
		if srcDeps != dstDeps {
			corrupted[id] = true
		}
	}
	for id := range target.ByID {
		if _, ok := source.ByID[id]; !ok {
			summary.Extra = append(summary.Extra, id)
		}
	}
	for id := range corrupted {
		summary.Corrupted = append(summary.Corrupted, id)
	}
	sort.Strings(summary.Missing)
	sort.Strings(summary.Extra)
	sort.Strings(summary.Corrupted)
	summary.OK = len(summary.Missing) == 0 && len(summary.Extra) == 0 && len(summary.Corrupted) == 0
	return summary, nil
}

type coordstoreSnapshot struct {
	Beads    []beads.Bead
	ByID     map[string]beads.Bead
	IDs      map[string]bool
	Deps     map[string][]beads.Dep
	Filtered int
}

type coordstoreDepBatchLister interface {
	DepListBatch(ids []string) (map[string][]beads.Dep, error)
}

func loadCoordstoreSnapshot(store beads.Store, retention coordstoreRetentionOptions, sortOrder beads.SortOrder) (coordstoreSnapshot, error) {
	listed, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth, Sort: sortOrder})
	if err != nil {
		return coordstoreSnapshot{}, err
	}
	snapshot := coordstoreSnapshot{
		Beads: make([]beads.Bead, 0, len(listed)),
		ByID:  make(map[string]beads.Bead, len(listed)),
		IDs:   make(map[string]bool, len(listed)),
	}
	for _, b := range listed {
		if coordstoreSkipForRetention(b, retention) {
			snapshot.Filtered++
			continue
		}
		snapshot.Beads = append(snapshot.Beads, b)
		snapshot.ByID[b.ID] = b
		snapshot.IDs[b.ID] = true
	}
	deps, err := coordstoreDepMap(store, snapshot.Beads)
	if err != nil {
		return coordstoreSnapshot{}, err
	}
	snapshot.Deps = deps
	return snapshot, nil
}

func coordstoreDepMap(store beads.Store, source []beads.Bead) (map[string][]beads.Dep, error) {
	ids := make([]string, 0, len(source))
	for _, b := range source {
		ids = append(ids, b.ID)
	}
	if batch, ok := store.(coordstoreDepBatchLister); ok {
		deps, err := batch.DepListBatch(ids)
		if err != nil {
			return nil, err
		}
		if deps == nil {
			deps = make(map[string][]beads.Dep)
		}
		return deps, nil
	}
	depsByID := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("list deps for %q: %w", id, err)
		}
		depsByID[id] = deps
	}
	return depsByID, nil
}

func coordstoreSkipForRetention(b beads.Bead, retention coordstoreRetentionOptions) bool {
	if retention.Full {
		return false
	}
	now := retention.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-retention.Retention)
	return coordstoreTerminalStatus(b.Status) && coordstoreTerminalReferenceTime(b).Before(cutoff)
}

func coordstoreTerminalStatus(status string) bool {
	if status == "cancel"+"led" {
		return true
	}
	switch status {
	case "closed", "canceled", "expired":
		return true
	default:
		return false
	}
}

func coordstoreTerminalReferenceTime(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

func coordstoreDepFingerprint(deps []beads.Dep, validIDs map[string]bool) string {
	normalized := deps[:0]
	for _, dep := range deps {
		dep = normalizeCoordstoreDep(dep)
		if validIDs != nil && (!validIDs[dep.IssueID] || !validIDs[dep.DependsOnID]) {
			continue
		}
		normalized = append(normalized, dep)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].IssueID != normalized[j].IssueID {
			return normalized[i].IssueID < normalized[j].IssueID
		}
		if normalized[i].DependsOnID != normalized[j].DependsOnID {
			return normalized[i].DependsOnID < normalized[j].DependsOnID
		}
		return normalized[i].Type < normalized[j].Type
	})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(normalized)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func normalizeCoordstoreDep(dep beads.Dep) beads.Dep {
	if dep.Type == "" {
		dep.Type = "blocks"
	}
	return dep
}

func coordstoreBeadFingerprint(b beads.Bead) string {
	type stableBead struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Status      string            `json:"status"`
		Type        string            `json:"type"`
		Priority    *int              `json:"priority,omitempty"`
		CreatedAt   time.Time         `json:"created_at"`
		UpdatedAt   time.Time         `json:"updated_at,omitempty"`
		Assignee    string            `json:"assignee,omitempty"`
		From        string            `json:"from,omitempty"`
		ParentID    string            `json:"parent,omitempty"`
		Ref         string            `json:"ref,omitempty"`
		Needs       []string          `json:"needs,omitempty"`
		Description string            `json:"description,omitempty"`
		Labels      []string          `json:"labels,omitempty"`
		Metadata    map[string]string `json:"metadata,omitempty"`
		Ephemeral   bool              `json:"ephemeral,omitempty"`
	}
	stable := stableBead{
		ID:          b.ID,
		Title:       b.Title,
		Status:      b.Status,
		Type:        b.Type,
		Priority:    cloneIntPtrForCoordstore(b.Priority),
		CreatedAt:   b.CreatedAt,
		UpdatedAt:   b.UpdatedAt,
		Assignee:    b.Assignee,
		From:        b.From,
		ParentID:    b.ParentID,
		Ref:         b.Ref,
		Needs:       append([]string(nil), b.Needs...),
		Description: b.Description,
		Labels:      append([]string(nil), b.Labels...),
		Metadata:    maps.Clone(b.Metadata),
		Ephemeral:   b.Ephemeral,
	}
	sort.Strings(stable.Needs)
	sort.Strings(stable.Labels)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(stable)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func cloneIntPtrForCoordstore(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}
