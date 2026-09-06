//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/filter"
	"github.com/noamsto/tmux-remux/internal/restore"
	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
	"github.com/noamsto/tmux-remux/internal/tmux"
	"github.com/noamsto/tmux-remux/internal/triggers"
	"github.com/noamsto/tmux-remux/testutil"
)

// scopedTmux runs tmux against a specific socket. Implements both the Lister
// and CaptureLister interfaces consumed by snapshot.Saver.
type scopedTmux struct {
	socket string
}

func (s scopedTmux) Run(ctx context.Context, args []string) (string, error) {
	full := append([]string{"-f", "/dev/null", "-u", "-S", s.socket}, args...)
	cmd := exec.CommandContext(ctx, "tmux", full...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}
func (s scopedTmux) ListSessions(ctx context.Context) ([]tmux.SessionRow, error) {
	out, err := s.Run(ctx, []string{"list-sessions", "-F", "#{session_name}\x1f#{session_last_attached}\x1f#{@bridge_host}"})
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	return tmux.ParseSessions(out)
}
func (s scopedTmux) ListWindows(ctx context.Context) ([]tmux.WindowRow, error) {
	out, err := s.Run(ctx, []string{"list-windows", "-a", "-F", "#{session_name}\x1f#{window_index}\x1f#{window_name}\x1f#{window_layout}\x1f#{window_id}\x1f#{E:automatic-rename}"})
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	return tmux.ParseWindows(out, nil)
}
func (s scopedTmux) ListPanes(ctx context.Context) ([]tmux.PaneRow, error) {
	out, err := s.Run(ctx, []string{"list-panes", "-a", "-F", "#{session_name}\x1f#{window_index}\x1f#{pane_index}\x1f#{pane_current_path}\x1f#{pane_current_command}\x1f#{pane_pid}\x1f#{pane_last_used}\x1f#{pane_id}\x1f#{@remux_relaunch}"})
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	return tmux.ParsePanes(out)
}
func (s scopedTmux) CapturePane(ctx context.Context, target string) ([]byte, error) {
	out, err := s.Run(ctx, []string{"capture-pane", "-pJ", "-t", target, "-S", "-"})
	return []byte(out), err
}

func TestSaveRestoreRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := testutil.StartServer(t)
	st := scopedTmux{socket: srv.Socket}

	if _, err := srv.Tmux("rename-session", "-t", "init", "lazytmux"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Tmux("new-window", "-t", "lazytmux", "-n", "build"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Tmux("split-window", "-t", "lazytmux:1"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	scrollDir := filepath.Join(dir, "sb")
	ctx := context.Background()

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sb := scrollback.New(scrollDir)
	saver := snapshot.NewSaver(db, sb, st, snapshot.SaverOptions{Host: "test", CaptureScrollback: true})
	if err := saver.Save(ctx, "integration"); err != nil {
		t.Fatalf("save: %v", err)
	}

	ev, _ := db.LatestSnapshot(ctx)
	if ev == nil {
		t.Fatal("no snapshot")
	}

	var m snapshot.Manifest
	if err := json.Unmarshal([]byte(ev.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Sessions) == 0 {
		t.Error("manifest missing sessions")
	}
	hasLazytmux := false
	for _, s := range m.Sessions {
		if s.Name == "lazytmux" {
			hasLazytmux = true
		}
	}
	if !hasLazytmux {
		t.Error("manifest missing lazytmux session")
	}
}

func TestPaneRestoreSplitsIntoLiveWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := testutil.StartServer(t)
	st := scopedTmux{socket: srv.Socket}

	// The default window starts with one pane; split to two.
	if _, err := srv.Tmux("split-window", "-t", "init"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sb := scrollback.New(filepath.Join(dir, "sb"))
	saver := snapshot.NewSaver(db, sb, st, snapshot.SaverOptions{Host: "test"})
	if err := saver.Save(ctx, "integration"); err != nil {
		t.Fatalf("save: %v", err)
	}

	ev, _ := db.LatestSnapshot(ctx)
	var m snapshot.Manifest
	if err := json.Unmarshal([]byte(ev.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	win := m.Sessions[0].Windows[0]
	if len(win.Panes) != 2 {
		t.Fatalf("snapshot window has %d panes, want 2", len(win.Panes))
	}
	lost := win.Panes[1]

	// Kill the second pane; the window stays live with its first pane.
	if _, err := srv.Tmux("kill-pane", "-t", fmt.Sprintf("init:%d.%d", win.Index, lost.Index)); err != nil {
		t.Fatal(err)
	}
	if n := panesInWindow(t, st, win.Index); n != 1 {
		t.Fatalf("after kill: %d panes, want 1", n)
	}

	plan := restore.BuildPaneRestore(lost, win, "init", win.ID, restore.BuildOptions{DefaultShell: "/bin/sh"})
	if _, err := restore.Apply(ctx, st, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := panesInWindow(t, st, win.Index); n != 2 {
		t.Errorf("after restore: %d panes, want 2 (the lost pane split back in)", n)
	}
}

func panesInWindow(t *testing.T, st scopedTmux, windowIndex int) int {
	t.Helper()
	panes, err := st.ListPanes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range panes {
		if p.WindowIndex == windowIndex {
			n++
		}
	}
	return n
}

// TestDecorationRestoreRoundtrip captures decoration options (@crew_name,
// @crew_color) from a real tmux server into a manifest, then replays the
// resulting restore plan against a fresh server and confirms the options
// land back on the recreated window. tmux.Client has no -S flag of its own —
// it resolves its target socket from $TMUX (see withSynthesizedTmuxEnv in
// internal/tmux/client.go) — so each phase points Client at the right server
// by setting TMUX to that server's socket.
func TestDecorationRestoreRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	src := testutil.StartServer(t)
	if _, err := src.Tmux("rename-session", "-t", "init", "deco"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Tmux("set-window-option", "-t", "deco:0", "@crew_color", "colour141"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Tmux("set-window-option", "-t", "deco:0", "@crew_name", "dispatcher"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMUX", src.Socket+",0,0")
	srcClient := tmux.NewClient("tmux", "@crew_name", "@crew_color")

	ctx := context.Background()
	m, err := snapshot.Build(ctx, srcClient, "test", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}

	var win *snapshot.Window
	for i := range m.Sessions {
		if m.Sessions[i].Name != "deco" {
			continue
		}
		for j := range m.Sessions[i].Windows {
			if m.Sessions[i].Windows[j].Index == 0 {
				win = &m.Sessions[i].Windows[j]
				break
			}
		}
	}
	if win == nil {
		t.Fatal("deco window missing from manifest")
	}
	wantDecoration := map[string]string{"@crew_name": "dispatcher", "@crew_color": "colour141"}
	if !reflect.DeepEqual(win.Decoration, wantDecoration) {
		t.Errorf("captured Decoration = %#v, want %#v", win.Decoration, wantDecoration)
	}

	plan, _ := restore.BuildPlan(m, filter.Filter{}, nil, restore.BuildOptions{})

	dst := testutil.StartServer(t)
	t.Setenv("TMUX", dst.Socket+",0,0")
	dstClient := tmux.NewClient("tmux")
	if _, err := restore.Apply(ctx, dstClient, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	out, err := dst.Tmux("show-options", "-w", "-v", "-t", "deco:0", "@crew_color")
	if err != nil {
		t.Fatalf("show-options: %v", err)
	}
	if got := strings.TrimSpace(out); got != "colour141" {
		t.Errorf("restored @crew_color = %q, want %q", got, "colour141")
	}
}

// TestRestoreFirstWindowAtBaseIndex restores a session's first window against a
// server with base-index 1, where the window tmux hands every new session lands
// on the very index the restored window wants. Creating session and window
// separately loses the window's startup command to "index in use".
func TestRestoreFirstWindowAtBaseIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dst := testutil.StartServer(t)
	if _, err := dst.Tmux("set-option", "-g", "base-index", "1"); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	plan := []restore.Action{
		restore.CreateWindow{
			Session:        "s1",
			Index:          1,
			Name:           "dispatcher",
			Cwd:            "/tmp",
			StartupCommand: "touch " + marker + "; exec /bin/sh",
			NewSession:     true,
		},
	}

	t.Setenv("TMUX", dst.Socket+",0,0")
	failed, err := restore.Apply(context.Background(), tmux.NewClient("tmux"), plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("apply reported failed actions: %v", failed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		out, _ := dst.Tmux("list-windows", "-t", "s1", "-F", "#{window_index} #{window_name}")
		t.Fatalf("restored window never ran its startup command; s1 windows:\n%s", strings.TrimSpace(out))
	}
}

// buildRemux compiles the CLI into t.TempDir() and returns its path. Hooks need
// a real binary; the other integration tests call packages directly.
func buildRemux(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tmux-remux")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/tmux-remux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// wireTriggers renders the fragment for the running tmux and sources it into
// srv. Storage lands under XDG_DATA_HOME, which the caller must have pointed at
// a temp dir before the server started.
func wireTriggers(t *testing.T, srv *testutil.Server, bin string) tmux.Version {
	t.Helper()
	v, err := tmux.NewClient("tmux").Version(context.Background())
	if err != nil {
		t.Fatalf("detect tmux version: %v", err)
	}
	frag := triggers.Render(triggers.Params{Bin: bin, Version: v, AutoRestore: false})
	path := filepath.Join(t.TempDir(), "triggers.conf")
	if err := os.WriteFile(path, []byte(frag), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := srv.Tmux("source-file", path); err != nil {
		t.Fatalf("source-file: %v\n%s", err, out)
	}
	return v
}

// waitForEvent polls the store for up to 5s for an event of kind whose manifest
// satisfies match, and returns it.
func waitForEvent(t *testing.T, dbPath, kind string, match func(closeevent.CloseManifest) bool) closeevent.CloseManifest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var seen []string
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		db, err := store.Open(context.Background(), dbPath)
		if err != nil {
			continue // hook may not have created the DB yet
		}
		evs, err := db.ListEvents(context.Background(), store.ListOpts{Kinds: []string{kind}, Limit: 20})
		_ = db.Close()
		if err != nil {
			continue
		}
		seen = seen[:0]
		for _, ev := range evs {
			var m closeevent.CloseManifest
			if json.Unmarshal([]byte(ev.ManifestJSON), &m) != nil {
				continue
			}
			seen = append(seen, ev.ManifestJSON)
			if match(m) {
				return m
			}
		}
	}
	t.Fatalf("no %s event matched within 5s; saw: %v", kind, seen)
	return closeevent.CloseManifest{}
}

// remuxEnv points storage at a temp dir and returns the resulting DB path. Must
// run before the tmux server starts so the server (and its hook jobs) inherit it.
func remuxEnv(t *testing.T) string {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_RUNTIME_DIR", dataHome)
	return filepath.Join(dataHome, "tmux-remux", "state.db")
}

func TestTriggersCloseEventsCarrySession(t *testing.T) {
	dbPath := remuxEnv(t)
	bin := buildRemux(t)
	srv := testutil.StartServer(t)
	wireTriggers(t, srv, bin)

	// A snapshot has to exist before the close, or capture-event has nothing to
	// diff against.
	if out, err := srv.Tmux("new-session", "-d", "-s", "work", "/bin/sh"); err != nil {
		t.Fatalf("new-session: %v\n%s", err, out)
	}
	if out, err := srv.Tmux("run-shell", bin+" save --reason=test"); err != nil {
		t.Fatalf("save: %v\n%s", err, out)
	}

	// A pane whose program exits is what fires pane-exited.
	if out, err := srv.Tmux("split-window", "-d", "-t", "work", "sh", "-c", "exit 0"); err != nil {
		t.Fatalf("split-window: %v\n%s", err, out)
	}

	m := waitForEvent(t, dbPath, "pane-died", func(m closeevent.CloseManifest) bool {
		return m.PaneID != ""
	})
	if m.SessionID == "" {
		t.Error("pane-died event has no session id — the hook passed --session empty")
	}
	if m.SessionName != "work" {
		t.Errorf("pane-died SessionName = %q, want \"work\"", m.SessionName)
	}
}

func TestTriggersWindowCloseCarriesSession(t *testing.T) {
	dbPath := remuxEnv(t)
	bin := buildRemux(t)
	srv := testutil.StartServer(t)
	wireTriggers(t, srv, bin)

	if out, err := srv.Tmux("new-session", "-d", "-s", "work", "/bin/sh"); err != nil {
		t.Fatalf("new-session: %v\n%s", err, out)
	}
	if out, err := srv.Tmux("new-window", "-d", "-t", "work", "-n", "doomed", "/bin/sh"); err != nil {
		t.Fatalf("new-window: %v\n%s", err, out)
	}
	if out, err := srv.Tmux("run-shell", bin+" save --reason=test"); err != nil {
		t.Fatalf("save: %v\n%s", err, out)
	}
	if out, err := srv.Tmux("kill-window", "-t", "work:doomed"); err != nil {
		t.Fatalf("kill-window: %v\n%s", err, out)
	}

	m := waitForEvent(t, dbPath, "window-unlinked", func(m closeevent.CloseManifest) bool {
		return m.WindowID != ""
	})
	if m.SessionID == "" {
		t.Error("window-unlinked event has no session id")
	}
	if m.SessionName != "work" {
		t.Errorf("window-unlinked SessionName = %q, want \"work\"", m.SessionName)
	}
}

// prefix+x runs `kill-pane`, and no tmux release gives that command hook the
// pane it killed, so closeevent.resolveKilledPane recovers the id by diffing
// survivors against the last snapshot.
func TestTriggersKillPaneResolvesViaSurvivorDiff(t *testing.T) {
	dbPath := remuxEnv(t)
	bin := buildRemux(t)
	srv := testutil.StartServer(t)
	wireTriggers(t, srv, bin)

	if out, err := srv.Tmux("new-session", "-d", "-s", "work", "/bin/sh"); err != nil {
		t.Fatalf("new-session: %v\n%s", err, out)
	}
	if out, err := srv.Tmux("split-window", "-d", "-t", "work", "/bin/sh"); err != nil {
		t.Fatalf("split-window: %v\n%s", err, out)
	}
	panes, err := srv.Tmux("list-panes", "-t", "work", "-F", "#{pane_id}")
	if err != nil {
		t.Fatalf("list-panes: %v\n%s", err, panes)
	}
	ids := strings.Fields(panes)
	if len(ids) != 2 {
		t.Fatalf("want 2 panes, got %v", ids)
	}
	victim := ids[1]

	if out, err := srv.Tmux("run-shell", bin+" save --reason=test"); err != nil {
		t.Fatalf("save: %v\n%s", err, out)
	}
	// -t names the victim, but the hook is after-kill-pane, which sees no pane
	// id regardless — exactly the prefix+x situation.
	if out, err := srv.Tmux("kill-pane", "-t", victim); err != nil {
		t.Fatalf("kill-pane: %v\n%s", err, out)
	}

	m := waitForEvent(t, dbPath, "pane-died", func(m closeevent.CloseManifest) bool {
		return m.PaneID == victim
	})
	if m.WindowID == "" {
		t.Error("survivor diff resolved the pane but not its window")
	}
}

// The monitor hook watches #{T:@remux_save_tick}, whose format string lives in
// an option precisely so a test can drive it at second granularity instead of
// waiting a minute.
func TestTriggersMonitorSaveTick(t *testing.T) {
	dbPath := remuxEnv(t)
	bin := buildRemux(t)
	srv := testutil.StartServer(t)
	if v := wireTriggers(t, srv, bin); !v.AtLeast(3, 8) {
		t.Skipf("monitor hooks need tmux 3.8, have %s", v)
	}

	if out, err := srv.Tmux("set", "-g", "@remux_save_tick", "%S"); err != nil {
		t.Fatalf("set @remux_save_tick: %v\n%s", err, out)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		db, err := store.Open(context.Background(), dbPath)
		if err != nil {
			continue
		}
		snaps, err := db.ListEvents(context.Background(), store.ListOpts{
			Kinds: []string{"snapshot"},
			Limit: 20,
		})
		_ = db.Close()
		if err != nil {
			continue
		}
		for _, ev := range snaps {
			if ev.Reason == "timer" {
				return
			}
		}
	}
	t.Fatal("no snapshot with reason=timer within 10s — the monitor hook did not fire")
}
