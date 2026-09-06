package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/noamsto/tmux-remux/internal/store"
)

func TestOpenAppliesMigrations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		db, err := store.Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		db.Close()
	}
}

func TestMigrateRespectsUserVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	// First open creates the schema and sets user_version=1.
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var v int
	if err := db.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != 1 {
		t.Errorf("after first open: user_version = %d, want 1", v)
	}
	db.Close()

	// Second open is a no-op for migrations.
	db, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer db.Close()
	if err := db.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != 1 {
		t.Errorf("after second open: user_version = %d, want 1", v)
	}
}

func TestInsertEventReturnsID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	id, err := db.InsertEvent(ctx, store.Event{
		Ts:           1745700000000,
		Kind:         "snapshot",
		Scope:        "server",
		Reason:       "timer",
		Host:         "testhost",
		ManifestJSON: `{"v":1}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
}

func TestLatestSnapshotReturnsMostRecent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i, ts := range []int64{1, 2, 3} {
		_, err := db.InsertEvent(ctx, store.Event{
			Ts:           ts,
			Kind:         "snapshot",
			Scope:        "server",
			Host:         "h",
			ManifestJSON: fmt.Sprintf(`{"i":%d}`, i),
		})
		if err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	ev, err := db.LatestSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if ev == nil {
		t.Fatal("expected event, got nil")
	}
	if ev.Ts != 3 {
		t.Errorf("Ts = %d, want 3", ev.Ts)
	}
}

func TestLatestSnapshotReturnsNilWhenEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ev, err := db.LatestSnapshot(ctx)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil, got %+v", ev)
	}
}

func TestListEventsByKind(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insert := func(ts int64, kind string) {
		t.Helper()
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: kind, Scope: "session", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert(10, "snapshot")
	insert(20, "pane-died")
	insert(30, "snapshot")
	insert(40, "session-closed")

	closes, err := db.ListEvents(ctx, store.ListOpts{ExcludeKinds: []string{"snapshot"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 2 {
		t.Fatalf("got %d events, want 2", len(closes))
	}
	if closes[0].Ts != 40 || closes[1].Ts != 20 {
		t.Errorf("expected ts=40,20 (DESC), got %d,%d", closes[0].Ts, closes[1].Ts)
	}
}

func TestPruneSnapshotsKeepsNewest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for ts := int64(1); ts <= 10; ts++ {
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: "snapshot", Scope: "server", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.PruneSnapshots(ctx, 3, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListEvents(ctx, store.ListOpts{Kinds: []string{"snapshot"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3", len(all))
	}
	if all[0].Ts != 10 || all[1].Ts != 9 || all[2].Ts != 8 {
		t.Errorf("expected newest 3 (10,9,8), got %d,%d,%d", all[0].Ts, all[1].Ts, all[2].Ts)
	}
}

func TestPruneCloseEventsKeepsNewest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for ts := int64(1); ts <= 10; ts++ {
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: "pane-died", Scope: "session", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.PruneCloseEvents(ctx, 3); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListEvents(ctx, store.ListOpts{ExcludeKinds: []string{"snapshot"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3", len(all))
	}
	if all[0].Ts != 10 || all[1].Ts != 9 || all[2].Ts != 8 {
		t.Errorf("expected newest 3 (10,9,8), got %d,%d,%d", all[0].Ts, all[1].Ts, all[2].Ts)
	}
}

func TestPruneCloseEventsDropsUnresolvable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insert := func(ts int64, kind string) {
		t.Helper()
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: kind, Scope: "session", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert(100, "snapshot")
	insert(200, "snapshot")
	insert(50, "pane-died")
	insert(150, "pane-died")
	insert(250, "pane-died")

	if err := db.PruneCloseEvents(ctx, 50); err != nil {
		t.Fatal(err)
	}

	closes, err := db.ListEvents(ctx, store.ListOpts{ExcludeKinds: []string{"snapshot"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 2 {
		t.Fatalf("got %d closes, want 2", len(closes))
	}
	if closes[0].Ts != 250 || closes[1].Ts != 150 {
		t.Errorf("expected ts=250,150, got %d,%d", closes[0].Ts, closes[1].Ts)
	}
}

func TestPruneCloseEventsKeepsAllWhenNoSnapshots(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, ts := range []int64{50, 150, 250} {
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: "pane-died", Scope: "session", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.PruneCloseEvents(ctx, 50); err != nil {
		t.Fatal(err)
	}

	closes, err := db.ListEvents(ctx, store.ListOpts{ExcludeKinds: []string{"snapshot"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 3 {
		t.Fatalf("got %d closes, want 3", len(closes))
	}
}

func TestUpsertScrollbackIncrementsRefcount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertScrollback(ctx, "abc123", 42, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertScrollback(ctx, "abc123", 42, 200); err != nil {
		t.Fatal(err)
	}

	var refcount int
	var lastUsed int64
	err = db.DB().QueryRowContext(ctx, "SELECT refcount, last_used_ts FROM scrollbacks WHERE sha256=?", "abc123").Scan(&refcount, &lastUsed)
	if err != nil {
		t.Fatal(err)
	}
	if refcount != 0 {
		t.Errorf("refcount on upsert should be 0 (linking happens via event_scrollbacks); got %d", refcount)
	}
	if lastUsed != 200 {
		t.Errorf("last_used_ts = %d, want 200", lastUsed)
	}
}

// TestPruneSnapshotsKeepsNewestPerDayWithinWeek verifies the retention
// safety net: besides the keep-N-newest window, the newest snapshot of each
// UTC day in the last 7 days survives — so a pre-shutdown snapshot is not
// evicted by a burst of fresh post-boot saves (the 2026-06-07 data loss).
// UTC-day grouping keeps the test deterministic in any host timezone.
func TestPruneSnapshotsKeepsNewestPerDayWithinWeek(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const day = int64(24 * time.Hour / time.Millisecond)
	now := int64(1780000000000) // fixed anchor
	insert := func(ts int64) {
		t.Helper()
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: "snapshot", Scope: "server", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Two snapshots on each of day-3 and day-2 (the later one per day must
	// survive), one on day-10 (outside the week — must be pruned), and a
	// burst of 4 fresh snapshots that fills the keep-N window.
	insert(now - 10*day)
	insert(now - 3*day)
	insert(now - 3*day + 3_600_000)
	insert(now - 2*day)
	insert(now - 2*day + 3_600_000)
	for i := int64(0); i < 4; i++ {
		insert(now - 3000 + i*1000)
	}

	if err := db.PruneSnapshots(ctx, 3, now); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListEvents(ctx, store.ListOpts{Kinds: []string{"snapshot"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int64, 0, len(all))
	for _, ev := range all { // ListEvents returns ts DESC
		got = append(got, ev.Ts)
	}
	want := []int64{
		now, now - 1000, now - 2000, // 3 newest
		now - 2*day + 3_600_000, // newest of day-2
		now - 3*day + 3_600_000, // newest of day-3
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("survivors mismatch (-want +got):\n%s", diff)
	}
}

func TestLinkEventScrollbackBumpsRefcount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, _ := db.InsertEvent(ctx, store.Event{Ts: 1, Kind: "snapshot", Scope: "server", Host: "h", ManifestJSON: "{}"})
	_ = db.UpsertScrollback(ctx, "sha1", 10, 1)
	if err := db.LinkEventScrollback(ctx, id, "s:1:1", "sha1"); err != nil {
		t.Fatal(err)
	}

	var refcount int
	_ = db.DB().QueryRowContext(ctx, "SELECT refcount FROM scrollbacks WHERE sha256='sha1'").Scan(&refcount)
	if refcount != 1 {
		t.Errorf("refcount = %d, want 1", refcount)
	}
}

func TestDeletingEventDecrementsRefcount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, _ := db.InsertEvent(ctx, store.Event{Ts: 1, Kind: "snapshot", Scope: "server", Host: "h", ManifestJSON: "{}"})
	_ = db.UpsertScrollback(ctx, "sha1", 10, 1)
	_ = db.LinkEventScrollback(ctx, id, "s:1:1", "sha1")

	if _, err := db.DB().ExecContext(ctx, "DELETE FROM events WHERE id=?", id); err != nil {
		t.Fatal(err)
	}

	var refcount int
	_ = db.DB().QueryRowContext(ctx, "SELECT refcount FROM scrollbacks WHERE sha256='sha1'").Scan(&refcount)
	if refcount != 0 {
		t.Errorf("refcount = %d, want 0", refcount)
	}
}

func TestSetGetMeta(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.SetMeta(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMeta(ctx, "k")
	if err != nil || got != "v1" {
		t.Fatalf("GetMeta(k) = %q, %v; want v1, nil", got, err)
	}
	if err := db.SetMeta(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetMeta(ctx, "k")
	if got != "v2" {
		t.Errorf("update did not stick: got %q", got)
	}
	missing, err := db.GetMeta(ctx, "nope")
	if err != nil || missing != "" {
		t.Errorf("missing key: got %q, %v; want \"\", nil", missing, err)
	}
}

// TestLatestSnapshotBeforeIgnoresNewerSnapshots pins the semantics restore
// relies on: a snapshot written at/after the anchor (server start) is never
// selected, only the newest strictly-older one.
func TestLatestSnapshotBeforeIgnoresNewerSnapshots(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, ts := range []int64{100, 200} { // 100 = pre-boot, 200 = post-start save
		if _, err := db.InsertEvent(ctx, store.Event{
			Ts: ts, Kind: "snapshot", Scope: "server", Host: "h", ManifestJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}

	ev, err := db.LatestSnapshotBefore(ctx, 150)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Ts != 100 {
		t.Fatalf("LatestSnapshotBefore(150) = %+v, want Ts=100", ev)
	}

	ev, err = db.LatestSnapshotBefore(ctx, 100) // strict <: equal ts excluded
	if err != nil {
		t.Fatal(err)
	}
	if ev != nil {
		t.Errorf("LatestSnapshotBefore(100) = %+v, want nil", ev)
	}
}
