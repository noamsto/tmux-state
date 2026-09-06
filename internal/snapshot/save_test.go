package snapshot_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
	"github.com/noamsto/tmux-remux/internal/tmux"
)

type captureClient struct {
	*fakeClient
	captured map[string][]byte
	errs     map[string]error
}

func (c *captureClient) CapturePane(_ context.Context, target string) ([]byte, error) {
	if err, ok := c.errs[target]; ok {
		return nil, err
	}
	if v, ok := c.captured[target]; ok {
		return v, nil
	}
	return []byte("default"), nil
}

func TestSaveInsertsEventAndScrollbacks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	scrollDir := filepath.Join(dir, "scrollbacks")
	ctx := context.Background()

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sb := scrollback.New(scrollDir)

	cc := &captureClient{
		fakeClient: &fakeClient{
			sessions: []tmux.SessionRow{{Name: "s1", LastAttached: 100}},
			windows:  []tmux.WindowRow{{Session: "s1", Index: 1, Name: "w1", Layout: "L"}},
			panes:    []tmux.PaneRow{{Session: "s1", WindowIndex: 1, PaneIndex: 1, Cwd: "/x", Command: "nvim", PID: 1, LastUsed: 1}},
		},
		captured: map[string][]byte{"s1:1.1": []byte("hello")},
	}

	saver := snapshot.NewSaver(db, sb, cc, snapshot.SaverOptions{
		Host: "test", CaptureScrollback: true, MinSaveInterval: 0,
	})

	if err := saver.Save(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	ev, err := db.LatestSnapshot(ctx)
	if err != nil || ev == nil {
		t.Fatalf("LatestSnapshot = %v, %v", ev, err)
	}
	rows, err := db.DB().QueryContext(ctx, "SELECT scrollback_sha FROM event_scrollbacks WHERE event_id=?", ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Error("expected at least one event_scrollback row")
	}
}

func TestSaveCapturePaneErrorIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	scrollDir := filepath.Join(dir, "scrollbacks")
	ctx := context.Background()

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sb := scrollback.New(scrollDir)

	cc := &captureClient{
		fakeClient: &fakeClient{
			sessions: []tmux.SessionRow{{Name: "s1", LastAttached: 100}},
			windows: []tmux.WindowRow{
				{Session: "s1", Index: 1, Name: "w1", ID: "@1"},
				{Session: "s1", Index: 2, Name: "w2", ID: "@2"},
			},
			panes: []tmux.PaneRow{
				{Session: "s1", WindowIndex: 1, PaneIndex: 1, Command: "bash", ID: "%1"},
				{Session: "s1", WindowIndex: 2, PaneIndex: 1, Command: "bash", ID: "%2"},
			},
		},
		errs: map[string]error{"s1:2.1": errors.New("can't find pane: 1")},
	}

	var logs []string
	saver := snapshot.NewSaver(db, sb, cc, snapshot.SaverOptions{
		Host: "h", CaptureScrollback: true, MinSaveInterval: 0,
		Logf: func(format string, a ...any) {
			logs = append(logs, fmt.Sprintf(format, a...))
		},
	})

	if err := saver.Save(ctx, "test"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ev, err := db.LatestSnapshot(ctx)
	if err != nil || ev == nil {
		t.Fatalf("LatestSnapshot = %v, %v", ev, err)
	}
	var m snapshot.Manifest
	if err := json.Unmarshal([]byte(ev.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Sessions) != 1 || len(m.Sessions[0].Windows) != 2 {
		t.Fatalf("manifest has %d windows, want 2", len(m.Sessions[0].Windows))
	}
	for _, w := range m.Sessions[0].Windows {
		switch w.Index {
		case 1:
			if w.Panes[0].ScrollbackSHA == "" {
				t.Error("w1 pane has empty ScrollbackSHA, want non-empty")
			}
		case 2:
			if w.Panes[0].ScrollbackSHA != "" {
				t.Errorf("w2 pane has ScrollbackSHA %q, want empty (capture-pane errored)", w.Panes[0].ScrollbackSHA)
			}
		}
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "capture-pane") && strings.Contains(l, "s1:2.1") {
			found = true
		}
	}
	if !found {
		t.Errorf("logs = %v, want an entry mentioning capture-pane and s1:2.1", logs)
	}
}

func TestSaveSkipsWhenFingerprintUnchangedAndThrottled(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(dir, "test.db"))
	defer db.Close()
	sb := scrollback.New(filepath.Join(dir, "scrollbacks"))
	cc := &captureClient{
		fakeClient: &fakeClient{
			sessions: []tmux.SessionRow{{Name: "s1"}},
			windows:  []tmux.WindowRow{{Session: "s1", Index: 1, Name: "w"}},
			panes:    []tmux.PaneRow{{Session: "s1", WindowIndex: 1, PaneIndex: 1, Command: "bash"}},
		},
		captured: map[string][]byte{},
	}
	saver := snapshot.NewSaver(db, sb, cc, snapshot.SaverOptions{
		Host: "h", CaptureScrollback: false, MinSaveInterval: time.Hour,
	})
	if err := saver.Save(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := saver.Save(ctx, "second"); err != nil {
		t.Fatal(err)
	}

	all, _ := db.ListEvents(ctx, store.ListOpts{Kinds: []string{"snapshot"}, Limit: 100})
	if len(all) != 1 {
		t.Errorf("expected 1 event (second was throttled), got %d", len(all))
	}
}

// A window created inside the throttle window used to be dropped along with the
// save, so it existed in no snapshot at all — and undo can only restore what a
// snapshot recorded. Structure must outrank the throttle.
func TestSaveThrottleNeverDropsANewWindow(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(dir, "test.db"))
	defer db.Close()
	sb := scrollback.New(filepath.Join(dir, "scrollbacks"))
	cc := &captureClient{
		fakeClient: &fakeClient{
			sessions: []tmux.SessionRow{{Name: "s1"}},
			windows:  []tmux.WindowRow{{Session: "s1", Index: 1, Name: "w", ID: "@1"}},
			panes:    []tmux.PaneRow{{Session: "s1", WindowIndex: 1, PaneIndex: 1, Command: "bash", ID: "%1"}},
		},
		captured: map[string][]byte{},
	}
	saver := snapshot.NewSaver(db, sb, cc, snapshot.SaverOptions{
		Host: "h", CaptureScrollback: true, MinSaveInterval: time.Hour,
	})
	if err := saver.Save(ctx, "baseline"); err != nil {
		t.Fatal(err)
	}

	cc.windows = append(cc.windows, tmux.WindowRow{Session: "s1", Index: 2, Name: "new", ID: "@2"})
	cc.panes = append(cc.panes, tmux.PaneRow{Session: "s1", WindowIndex: 2, PaneIndex: 1, Command: "fish", ID: "%2"})
	if err := saver.Save(ctx, "hook:window-linked"); err != nil {
		t.Fatal(err)
	}

	ev, err := db.LatestSnapshot(ctx)
	if err != nil || ev == nil {
		t.Fatalf("LatestSnapshot = %v, %v", ev, err)
	}
	var m snapshot.Manifest
	if err := json.Unmarshal([]byte(ev.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Sessions) != 1 || len(m.Sessions[0].Windows) != 2 {
		t.Fatalf("latest snapshot has %d windows, want 2 (the new window must be captured despite the throttle)", len(m.Sessions[0].Windows))
	}

	// The throttle still buys something: the forced snapshot skips the
	// expensive scrollback capture.
	var linked int
	if err := db.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM event_scrollbacks WHERE event_id=?", ev.ID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 0 {
		t.Errorf("throttled snapshot linked %d scrollbacks, want 0", linked)
	}
	if !m.ScrollbackSkipped {
		t.Error("throttled snapshot manifest has ScrollbackSkipped = false, want true")
	}

	// A pure churn change (same entities, different command) stays throttled.
	cc.panes[0].Command = "nvim"
	if err := saver.Save(ctx, "churn"); err != nil {
		t.Fatal(err)
	}
	all, _ := db.ListEvents(ctx, store.ListOpts{Kinds: []string{"snapshot"}, Limit: 100})
	if len(all) != 2 {
		t.Errorf("got %d snapshots, want 2 (churn inside the throttle window must not save)", len(all))
	}
}

// TestSaveSkipsWhenNoSessions covers the "no tmux server running" case:
// Build returns an empty manifest, and Save must not insert an event.
// Without this guard the systemd save timer pollutes the event log with
// sessions:null rows, which `restore` then picks as "latest snapshot".
func TestSaveSkipsWhenNoSessions(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(dir, "test.db"))
	defer db.Close()
	sb := scrollback.New(filepath.Join(dir, "scrollbacks"))

	empty := &captureClient{fakeClient: &fakeClient{}, captured: map[string][]byte{}}
	saver := snapshot.NewSaver(db, sb, empty, snapshot.SaverOptions{Host: "h"})

	if err := saver.Save(ctx, "timer"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all, _ := db.ListEvents(ctx, store.ListOpts{Kinds: []string{"snapshot"}, Limit: 10})
	if len(all) != 0 {
		t.Errorf("expected 0 snapshot events, got %d", len(all))
	}
}
