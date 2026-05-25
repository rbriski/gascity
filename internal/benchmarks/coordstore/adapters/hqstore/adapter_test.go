package hqstore

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

func TestStatsNilStore(t *testing.T) {
	adapter := New()

	if got := adapter.Stats(context.Background()); got != nil {
		t.Fatalf("Stats() = %v, want nil for unopened adapter", got)
	}
}

func TestStatsReportsLiveObjectsAcrossTiers(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := adapter.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	main, err := adapter.Create(ctx, coordstore.Record{Title: "main"})
	if err != nil {
		t.Fatalf("Create main: %v", err)
	}
	wisp, err := adapter.Create(ctx, coordstore.Record{Title: "wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}

	assertLiveObjects(t, adapter, 2)

	if err := adapter.Delete(ctx, main.ID); err != nil {
		t.Fatalf("Delete main: %v", err)
	}
	assertLiveObjects(t, adapter, 1)

	if err := adapter.Delete(ctx, wisp.ID); err != nil {
		t.Fatalf("Delete wisp: %v", err)
	}
	assertLiveObjects(t, adapter, 0)
}

func assertLiveObjects(t *testing.T, adapter *Adapter, want int64) {
	t.Helper()

	stats := adapter.Stats(context.Background())
	if stats == nil {
		t.Fatalf("Stats() = nil, want live_objects=%d", want)
	}
	if got := stats["live_objects"]; got != want {
		t.Fatalf("Stats()[live_objects] = %d, want %d (stats=%v)", got, want, stats)
	}
	if len(stats) != 1 {
		t.Fatalf("Stats() = %v, want only live_objects", stats)
	}
}
