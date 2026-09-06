package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/restore"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
	"github.com/noamsto/tmux-remux/internal/tmux"
)

// seedStore returns an open store with a single snapshot capturing one window
// (mono:4, id @9) plus whatever close events the test inserts on top.
func seedStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	snap := snapshot.Manifest{V: 1, Host: "h", SavedAt: 100, Sessions: []snapshot.Session{{
		Name: "mono",
		Windows: []snapshot.Window{{
			Index: 4, Name: "win", Layout: "L", ID: "@9",
			Panes: []snapshot.Pane{{Index: 1, Cwd: "/m", Command: "fish", ID: "%9"}},
		}},
	}}}
	insertEvent(ctx, t, db, 100, "snapshot", string(mustJSON(t, snap)))
	return db
}

func insertEvent(ctx context.Context, t *testing.T, db *store.Store, ts int64, kind, manifest string) int64 {
	t.Helper()
	id, err := db.InsertEvent(ctx, store.Event{Ts: ts, Kind: kind, Scope: "server", Host: "h", ManifestJSON: manifest})
	if err != nil {
		t.Fatalf("insert %s: %v", kind, err)
	}
	return id
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// closeWindowManifest builds a window-unlinked CloseManifest naming the closed
// window id. The post-close index stays empty — resolution keys off the id.
func closeWindowManifest(t *testing.T, closedID string) string {
	t.Helper()
	return string(mustJSON(t, closeevent.CloseManifest{WindowID: closedID}))
}

func TestRestorableCloseReportsUnrecoverableHead(t *testing.T) {
	ctx := context.Background()
	db := seedStore(ctx, t)

	// Recoverable: @9 is in the snapshot, and it's gone from the post-close index.
	recoverable := insertEvent(ctx, t, db, 200, "window-unlinked", closeWindowManifest(t, "@9"))
	// Newer but unrecoverable: @14 was born+died inside a snapshot gap, so it
	// never made it into the snapshot. It must surface as discarded rather than
	// be stepped over — restoring @9 here would look like undo doing nothing.
	unrecoverable := insertEvent(ctx, t, db, 300, "window-unlinked", closeWindowManifest(t, "@14"))

	target, err := restorableClose(ctx, db, "")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if len(target.Discarded) != 1 || target.Discarded[0].ID != unrecoverable {
		t.Fatalf("Discarded = %+v, want just event %d", target.Discarded, unrecoverable)
	}
	if !target.OK {
		t.Fatal("expected a recoverable event behind the discarded head, got none")
	}
	if target.Event.ID != recoverable {
		t.Errorf("target event %d, want %d", target.Event.ID, recoverable)
	}
	if m := target.Item.SubManifest(target.Prior.Host, target.Prior.SavedAt); len(m.Sessions) != 1 || m.Sessions[0].Name != "mono" {
		t.Errorf("manifest = %+v, want one session 'mono'", m.Sessions)
	}
}

func TestDiscardSummaryMentionsFollowUpPress(t *testing.T) {
	evs := []store.Event{{Ts: time.Now().UnixMilli(), Scope: "window"}}
	if got := discardSummary(evs, true); !strings.Contains(got, "prefix+u again") {
		t.Errorf("summary = %q, want a hint to press again", got)
	}
	if got := discardSummary(evs, false); !strings.Contains(got, "nothing older") {
		t.Errorf("summary = %q, want the exhausted-history wording", got)
	}
}

func TestRestorableClosePicksLonePane(t *testing.T) {
	ctx := context.Background()
	db := seedStore(ctx, t)

	insertEvent(ctx, t, db, 200, "window-unlinked", closeWindowManifest(t, "@9"))
	// A lone pane-died is now recoverable (its parent window @9 is in the
	// snapshot), so it wins the head over the older window close.
	paneMan := string(mustJSON(t, closeevent.CloseManifest{PaneID: "%9", WindowID: "@9"}))
	pane := insertEvent(ctx, t, db, 300, "pane-died", paneMan)

	target, err := restorableClose(ctx, db, "")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if !target.OK || target.Event.ID != pane {
		t.Fatalf("popped event %d ok=%v, want the pane event %d", target.Event.ID, target.OK, pane)
	}
	if len(target.Discarded) != 0 {
		t.Errorf("Discarded = %+v, want none", target.Discarded)
	}
	if target.Item.Pane == nil || target.Item.Pane.ID != "%9" {
		t.Errorf("item.Pane = %+v, want the lost pane %%9", target.Item.Pane)
	}
	if target.Item.Window == nil || target.Item.Window.ID != "@9" {
		t.Errorf("item.Window = %+v, want parent window @9", target.Item.Window)
	}
}

func TestRestorableCloseEmptyWhenNothingRecoverable(t *testing.T) {
	ctx := context.Background()
	db := seedStore(ctx, t)
	unrecoverable := insertEvent(ctx, t, db, 300, "window-unlinked", closeWindowManifest(t, "@14"))

	target, err := restorableClose(ctx, db, "")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if target.OK {
		t.Error("expected no recoverable event, got one")
	}
	if len(target.Discarded) != 1 || target.Discarded[0].ID != unrecoverable {
		t.Errorf("Discarded = %+v, want just event %d", target.Discarded, unrecoverable)
	}
}

// emptyStore returns an open store with no snapshot at all, so any close
// event in it can only resolve through the entity embedded at capture time.
func emptyStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// resolvedWindowManifest builds a window-unlinked CloseManifest carrying an
// embedded entity for a window no prior snapshot ever captured.
func resolvedWindowManifest(t *testing.T) string {
	t.Helper()
	item := closeevent.ClosedItem{
		Window: &snapshot.Window{
			Index: 4, Name: "win", Layout: "L", ID: "@9",
			Panes: []snapshot.Pane{{Index: 1, Cwd: "/m", Command: "fish", ID: "%9"}},
		},
		SessionName: "mono",
		WindowIndex: 4,
	}
	man := closeevent.CloseManifest{
		WindowID:    "@9",
		SessionName: "mono",
		Resolved:    &closeevent.ResolvedClose{Item: item, SavedAt: 100},
	}
	return string(mustJSON(t, man))
}

// TestRestorableCloseUsesEmbeddedEntityWithoutAPriorSnapshot guards the case
// this step exists for: a close event with nothing in any snapshot must still
// be restorable, and — critically — must not be dropped into Discarded, since
// undo --pop deletes discarded rows outright.
func TestRestorableCloseUsesEmbeddedEntityWithoutAPriorSnapshot(t *testing.T) {
	ctx := context.Background()
	db := emptyStore(ctx, t)
	id := insertEvent(ctx, t, db, 200, "window-unlinked", resolvedWindowManifest(t))

	target, err := restorableClose(ctx, db, "")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if !target.OK || target.Event.ID != id {
		t.Fatalf("OK = %v, event = %d, want the embedded close %d restorable", target.OK, target.Event.ID, id)
	}
	if len(target.Discarded) != 0 {
		t.Errorf("Discarded = %+v, want none — the embedded entity resolves it", target.Discarded)
	}
	if target.Item.Window == nil || target.Item.Window.ID != "@9" {
		t.Errorf("item.Window = %+v, want the embedded window @9", target.Item.Window)
	}
}

// TestBuildCloseContextsUsesEmbeddedEntityWithoutAPriorSnapshot mirrors the
// picker's read path: an event with no prior snapshot but an embedded entity
// must still get a picker.CloseContext, not be hidden as unrecoverable.
func TestBuildCloseContextsUsesEmbeddedEntityWithoutAPriorSnapshot(t *testing.T) {
	ctx := context.Background()
	db := emptyStore(ctx, t)
	ev := store.Event{ID: 1, Ts: 200, Kind: "window-unlinked", Host: "h", ManifestJSON: resolvedWindowManifest(t)}

	ctxs := buildCloseContexts(ctx, db, []store.Event{ev})

	got, ok := ctxs[ev.ID]
	if !ok {
		t.Fatal("buildCloseContexts: no context for the embedded-entity event")
	}
	if len(got.SubManifest.Sessions) != 1 || got.SubManifest.Sessions[0].Name != "mono" {
		t.Errorf("SubManifest = %+v, want one session 'mono'", got.SubManifest.Sessions)
	}
}

// TestRestorableCloseStillDiscardsWithNoEmbeddedEntity guards against
// over-reach: an event with neither a prior snapshot nor an embedded entity
// must land in Discarded exactly as it did before this step.
func TestRestorableCloseStillDiscardsWithNoEmbeddedEntity(t *testing.T) {
	ctx := context.Background()
	db := emptyStore(ctx, t)
	id := insertEvent(ctx, t, db, 200, "window-unlinked", closeWindowManifest(t, "@14"))

	target, err := restorableClose(ctx, db, "")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if target.OK {
		t.Error("OK = true, want false — no snapshot and no embedded entity to fall back on")
	}
	if len(target.Discarded) != 1 || target.Discarded[0].ID != id {
		t.Errorf("Discarded = %+v, want just event %d", target.Discarded, id)
	}
}

// A window that was itself restored carries a fresh @id, so matching only on
// the snapshot's id would report "not live" and recreate the window a second
// time. Name and index are the fallbacks.
func TestMatchParentWindow(t *testing.T) {
	live := []tmux.WindowRow{
		{Session: "mono", Index: 1, Name: "shell", ID: "@1"},
		{Session: "mono", Index: 7, Name: "docs", ID: "@42"},
		{Session: "other", Index: 3, Name: "docs", ID: "@50"},
	}
	tests := []struct {
		name    string
		session string
		win     snapshot.Window
		want    string
	}{
		{"id match wins", "mono", snapshot.Window{ID: "@42", Name: "renamed", Index: 99}, "@42"},
		{"stale id falls back to name in session", "mono", snapshot.Window{ID: "@9", Name: "docs", Index: 99}, "@42"},
		{"empty id skips id lookup and falls back to name in session", "mono", snapshot.Window{ID: "", Name: "docs", Index: 99}, "@42"},
		// A live window merely sharing the index must not match: renumbering can
		// shift an unrelated survivor into the exact slot a closed window
		// vacated, and matching on index alone would inject a restore into it.
		{"index-only match is rejected, not live", "mono", snapshot.Window{ID: "@9", Name: "gone", Index: 7}, ""},
		{"never crosses sessions", "mono", snapshot.Window{ID: "@9", Name: "nothing", Index: 3}, ""},
		{"no match is not live", "mono", snapshot.Window{ID: "@9", Name: "nothing", Index: 88}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchParentWindow(live, tc.session, tc.win); got != tc.want {
				t.Errorf("matchParentWindow = %q, want %q", got, tc.want)
			}
		})
	}
}

// seedTwoSessionStore snapshots two sessions so closes in either can resolve:
// mono (window @9) and lazytmux (window @20).
func seedTwoSessionStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	snap := snapshot.Manifest{V: 1, Host: "h", SavedAt: 100, Sessions: []snapshot.Session{
		{Name: "mono", Windows: []snapshot.Window{{
			Index: 4, Name: "win", Layout: "L", ID: "@9",
			Panes: []snapshot.Pane{{Index: 1, Cwd: "/m", Command: "fish", ID: "%9"}},
		}}},
		{Name: "lazytmux", Windows: []snapshot.Window{{
			Index: 2, Name: "docs", Layout: "L", ID: "@20",
			Panes: []snapshot.Pane{{Index: 1, Cwd: "/l", Command: "fish", ID: "%20"}},
		}}},
	}}
	insertEvent(ctx, t, db, 100, "snapshot", string(mustJSON(t, snap)))
	return db
}

// namedCloseManifest builds a window close that carries its own session name,
// the way a post-change hook records it.
func namedCloseManifest(t *testing.T, closedID, session string) string {
	t.Helper()
	return string(mustJSON(t, closeevent.CloseManifest{WindowID: closedID, SessionName: session}))
}

func TestRestorableClosePrefersTheCurrentSession(t *testing.T) {
	ctx := context.Background()
	db := seedTwoSessionStore(ctx, t)

	mine := insertEvent(ctx, t, db, 200, "window-unlinked", namedCloseManifest(t, "@9", "mono"))
	// Newer, but it belongs to another session — pressing u in mono must not
	// reach across and resurrect it.
	insertEvent(ctx, t, db, 300, "window-unlinked", namedCloseManifest(t, "@20", "lazytmux"))

	target, err := restorableClose(ctx, db, "mono")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if !target.OK || target.Event.ID != mine {
		t.Fatalf("popped event %d ok=%v, want mono's event %d", target.Event.ID, target.OK, mine)
	}
	if target.FromSession != "" {
		t.Errorf("FromSession = %q, want empty — this was not a cross-session fallback", target.FromSession)
	}
}

func TestRestorableCloseFallsBackAcrossSessions(t *testing.T) {
	ctx := context.Background()
	db := seedTwoSessionStore(ctx, t)

	other := insertEvent(ctx, t, db, 300, "window-unlinked", namedCloseManifest(t, "@20", "lazytmux"))

	target, err := restorableClose(ctx, db, "mono")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if !target.OK || target.Event.ID != other {
		t.Fatalf("popped event %d ok=%v, want the fallback event %d", target.Event.ID, target.OK, other)
	}
	if target.FromSession != "lazytmux" {
		t.Errorf("FromSession = %q, want \"lazytmux\" so the message can name it", target.FromSession)
	}
}

func TestRestorableCloseDiscardsOnlyThisSessionsDeadRows(t *testing.T) {
	ctx := context.Background()
	db := seedTwoSessionStore(ctx, t)

	mine := insertEvent(ctx, t, db, 200, "window-unlinked", namedCloseManifest(t, "@9", "mono"))
	// Unrecoverable and in mono: discard it, since it can never come back.
	dead := insertEvent(ctx, t, db, 400, "window-unlinked", namedCloseManifest(t, "@77", "mono"))
	// Unrecoverable but in lazytmux: leave it for a press over there, so that
	// session still gets its own "never made it into a snapshot" message.
	insertEvent(ctx, t, db, 500, "window-unlinked", namedCloseManifest(t, "@88", "lazytmux"))

	target, err := restorableClose(ctx, db, "mono")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if len(target.Discarded) != 1 || target.Discarded[0].ID != dead {
		t.Fatalf("Discarded = %+v, want just mono's dead event %d", target.Discarded, dead)
	}
	if !target.OK || target.Event.ID != mine {
		t.Errorf("popped event %d, want %d behind the discarded row", target.Event.ID, mine)
	}
}

// Discarding this session's dead rows while another session still holds a
// restorable close must promise a next press, not claim the history is
// exhausted — the next press falls back and restores that close.
func TestRestorableCloseReportsMoreWhenOnlyAFallbackSurvives(t *testing.T) {
	ctx := context.Background()
	db := seedTwoSessionStore(ctx, t)

	dead := insertEvent(ctx, t, db, 400, "window-unlinked", namedCloseManifest(t, "@77", "mono"))
	insertEvent(ctx, t, db, 300, "window-unlinked", namedCloseManifest(t, "@20", "lazytmux"))

	target, err := restorableClose(ctx, db, "mono")
	if err != nil {
		t.Fatalf("restorableClose: %v", err)
	}
	if len(target.Discarded) != 1 || target.Discarded[0].ID != dead {
		t.Fatalf("Discarded = %+v, want just %d", target.Discarded, dead)
	}
	if target.OK {
		t.Error("OK = true, want false — nothing in mono is restorable")
	}
	if !target.MoreAvailable {
		t.Error("MoreAvailable = false, want true — lazytmux's close survives for the next press")
	}
	if got := discardSummary(target.Discarded, target.MoreAvailable); !strings.Contains(got, "prefix+u again") {
		t.Errorf("summary = %q, want a hint to press again", got)
	}
}

func TestDiscardSummaryNamesTheFallbackSession(t *testing.T) {
	if got := undoMessage("lazytmux"); !strings.Contains(got, "lazytmux") {
		t.Errorf("message = %q, want it to name the source session", got)
	}
	if got := undoMessage(""); got != "" {
		t.Errorf("message = %q, want empty for a same-session undo", got)
	}
}

// countCreateWindows returns the CreateWindow actions in plan, in order.
func countCreateWindows(plan []restore.Action) []restore.CreateWindow {
	var out []restore.CreateWindow
	for _, a := range plan {
		if cw, ok := a.(restore.CreateWindow); ok {
			out = append(out, cw)
		}
	}
	return out
}

// TestBuildRestorePlan_WindowCloseInsertsAtItsIndex guards Finding 1's
// inverted guard: a window close's sub-manifest holds exactly one window, so
// it must reclaim the index it was closed at via new-window -b — otherwise
// renumber-windows almost always leaves that index occupied and the plain
// new-window fallback fails with "index in use".
func TestBuildRestorePlan_WindowCloseInsertsAtItsIndex(t *testing.T) {
	ctx := context.Background()
	db := seedStore(ctx, t)
	ev := store.Event{Ts: 200, Kind: "window-unlinked", ManifestJSON: closeWindowManifest(t, "@9")}
	item, prior, ok := resolveEvent(ctx, db, ev)
	if !ok {
		t.Fatal("resolveEvent: expected a recoverable window close")
	}

	plan, _ := buildRestorePlan(ctx, nil, item, prior, restore.BuildOptions{})

	creates := countCreateWindows(plan)
	if len(creates) != 1 {
		t.Fatalf("CreateWindow actions = %d, want exactly 1", len(creates))
	}
	if !creates[0].InsertBefore {
		t.Error("InsertBefore = false, want true — a window undo must reclaim its old index")
	}
}

// TestBuildRestorePlan_SessionCloseNeverInserts covers the other half of
// Finding 1: restoring a whole session recreates several windows in one
// plan, and new-window -b on any but the first would shift the indexes that
// later actions in the same plan target.
func TestBuildRestorePlan_SessionCloseNeverInserts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	snap := snapshot.Manifest{V: 1, Host: "h", SavedAt: 100, Sessions: []snapshot.Session{{
		Name: "mono",
		Windows: []snapshot.Window{
			{Index: 0, Name: "shell", Layout: "L", ID: "@1", Panes: []snapshot.Pane{{Index: 1, Cwd: "/a", Command: "fish", ID: "%1"}}},
			{Index: 1, Name: "logs", Layout: "L", ID: "@2", Panes: []snapshot.Pane{{Index: 1, Cwd: "/b", Command: "fish", ID: "%2"}}},
		},
	}}}
	insertEvent(ctx, t, db, 100, "snapshot", string(mustJSON(t, snap)))

	ev := store.Event{Ts: 200, Kind: "session-closed", ManifestJSON: string(mustJSON(t, closeevent.CloseManifest{}))}
	item, prior, ok := resolveEvent(ctx, db, ev)
	if !ok {
		t.Fatal("resolveEvent: expected a recoverable session close")
	}

	plan, _ := buildRestorePlan(ctx, nil, item, prior, restore.BuildOptions{})

	creates := countCreateWindows(plan)
	if len(creates) != 2 {
		t.Fatalf("CreateWindow actions = %d, want 2", len(creates))
	}
	for i, cw := range creates {
		if cw.InsertBefore {
			t.Errorf("creates[%d].InsertBefore = true, want false on every window of a session close", i)
		}
	}
}

// TestBuildRestorePlan_PaneCloseNeverCreatesAWindow covers the third case: a
// lost pane whose parent window is still live splits back into it and must
// never go through the window-recreating path.
func TestBuildRestorePlan_PaneCloseNeverCreatesAWindow(t *testing.T) {
	ctx := context.Background()
	db := seedStore(ctx, t)
	ev := store.Event{Ts: 200, Kind: "pane-died", ManifestJSON: string(mustJSON(t, closeevent.CloseManifest{PaneID: "%9", WindowID: "@9"}))}
	item, prior, ok := resolveEvent(ctx, db, ev)
	if !ok {
		t.Fatal("resolveEvent: expected a recoverable pane close")
	}

	// Fake tmux stands in for a live server so the parent window (mono:4, @9)
	// resolves as live, driving buildRestorePlan down the split-back-in path
	// rather than the window-recreating one that would emit a CreateWindow.
	live := strings.Join([]string{"mono", "4", "win", "L", "@9", "0"}, tmux.FieldSep)
	plan, _ := buildRestorePlan(ctx, tmux.NewClient(fakeTmuxEmitting(t, live)), item, prior, restore.BuildOptions{})

	if creates := countCreateWindows(plan); len(creates) != 0 {
		t.Fatalf("plan = %+v, want no CreateWindow — the parent window is live", plan)
	}
}

// fakeTmuxEmitting writes a stand-in tmux that prints `out` verbatim, so
// ListWindows can be driven without a real server.
//
// Two portability constraints shape it, both of which silently produce a
// working-looking fake whose output is wrong: the nix build sandbox has no
// /usr/bin/env, so that shebang cannot exec there, and \x escapes in printf are
// not portable across shells, so the \x1f field separators may not survive. The
// separator bytes therefore ride a quoted heredoc, already embedded by the
// caller via tmux.FieldSep.
func fakeTmuxEmitting(t *testing.T, out string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-tmux")
	script := "#!/bin/sh\ncat <<'REMUX_ROWS'\n" + out + "\nREMUX_ROWS\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
