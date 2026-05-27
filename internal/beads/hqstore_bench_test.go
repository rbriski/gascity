package beads_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func BenchmarkHQStoreCreate(b *testing.B) {
	store, err := beads.OpenHQStore(b.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		b.Fatalf("OpenHQStore: %v", err)
	}
	b.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			b.Errorf("Shutdown: %v", err)
		}
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Create(beads.Bead{Title: "bench"}); err != nil {
			b.Fatalf("Create: %v", err)
		}
	}
}

func BenchmarkHQStoreRAM10K(b *testing.B) {
	for i := 0; i < b.N; i++ {
		store, err := beads.OpenHQStore(b.TempDir(), beads.WithHQStoreSnapshotInterval(0))
		if err != nil {
			b.Fatalf("OpenHQStore: %v", err)
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		rssBefore := hqBenchRSSBytes()
		for n := range 10000 {
			if _, err := store.Create(beads.Bead{Title: "bench", Assignee: "rig/agent-" + strconv.Itoa(n%10)}); err != nil {
				b.Fatalf("Create: %v", err)
			}
		}
		runtime.ReadMemStats(&after)
		rssAfter := hqBenchRSSBytes()
		b.ReportMetric(float64(after.HeapInuse)/1024/1024, "heap_inuse_mib")
		if rssAfter > 0 {
			b.ReportMetric(float64(rssAfter)/1024/1024, "rss_mib")
		}
		if after.HeapInuse >= before.HeapInuse {
			b.ReportMetric(float64(after.HeapInuse-before.HeapInuse)/1024/1024, "heap_delta_mib")
		}
		if rssAfter >= rssBefore && rssBefore > 0 {
			b.ReportMetric(float64(rssAfter-rssBefore)/1024/1024, "rss_delta_mib")
		}
		if err := store.Shutdown(); err != nil {
			b.Fatalf("Shutdown: %v", err)
		}
	}
}

// BenchmarkHQStoreRecentScan measures the cost of a SortCreatedDesc+Limit scan
// over a 28k-bead corpus — the "before" number (no fast path) establishes the
// baseline; after Phase 1 lands the builder's fast path, this should drop by
// roughly an order of magnitude in both ns/op and allocs/op.
//
// Run with:
//
//	go test -bench=BenchmarkHQStoreRecentScan -benchmem ./internal/beads/
func BenchmarkHQStoreRecentScan(b *testing.B) {
	const corpusSize = 28_000

	store, err := beads.OpenHQStore(b.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		b.Fatalf("OpenHQStore: %v", err)
	}
	b.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			b.Errorf("Shutdown: %v", err)
		}
	})

	for i := range corpusSize {
		status := ""
		if i%3 == 0 {
			status = "closed"
		}
		if _, err := store.Create(beads.Bead{Title: "bench-" + strconv.Itoa(i), Status: status}); err != nil {
			b.Fatalf("Create: %v", err)
		}
	}

	q := beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
		Sort:          beads.SortCreatedDesc,
		Limit:         5,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results, err := store.List(q)
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected non-empty results")
		}
	}
}

func hqBenchRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
