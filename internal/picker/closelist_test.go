package picker_test

import (
	"testing"

	"github.com/noamsto/tmux-remux/internal/picker"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

// windowCtx builds a CloseContext for a window-scope close: one window
// holding a single pane with the given command and cwd.
func windowCtx(session string, idx int, winName, cmd, cwd string) picker.CloseContext {
	return picker.CloseContext{
		Placement: picker.ClosePlacement{
			Session: session, WindowIndex: idx, WindowName: winName, Scope: "window", PaneCount: 1,
		},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{
			Name: session,
			Windows: []snapshot.Window{{
				Index: idx, Name: winName,
				Panes: []snapshot.Pane{{Command: cmd, Cwd: cwd}},
			}},
		}}},
	}
}

// paneCtx builds a CloseContext for a pane-scope close: the enclosing window
// holding the died pane, identified by paneID. The sibling is wider than
// closeevent.SubManifest now produces, and is here so the id-based lookup
// stays honest if a caller ever hands the picker the whole window again.
func paneCtx(session string, idx int, winName, paneID, cmd, cwd string) picker.CloseContext {
	return picker.CloseContext{
		Placement: picker.ClosePlacement{
			Session: session, WindowIndex: idx, WindowName: winName, Scope: "pane", PaneID: paneID,
		},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{
			Name: session,
			Windows: []snapshot.Window{{
				Index: idx, Name: winName,
				Panes: []snapshot.Pane{
					{ID: "%sibling", Command: "fish", Cwd: "/home/x"},
					{ID: paneID, Command: cmd, Cwd: cwd},
				},
			}},
		}}},
	}
}

// sessionCtx builds a CloseContext for a session-scope close: the whole
// session, with whatever windows it happened to hold when it closed.
func sessionCtx(session string, windows []snapshot.Window) picker.CloseContext {
	return picker.CloseContext{
		Placement:   picker.ClosePlacement{Session: session, Scope: "session"},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{Name: session, Windows: windows}}},
	}
}

func oneWindow(target string) []snapshot.Window {
	return []snapshot.Window{{
		Index: 1, Name: "1w",
		Panes: []snapshot.Pane{{Command: "claude", Cwd: "/home/" + target}},
	}}
}

// closeRows returns the RowClose rows of rows, in order.
func closeRows(rows []picker.CloseRow) []picker.CloseRow {
	out := make([]picker.CloseRow, 0, len(rows))
	for _, r := range rows {
		if r.Kind == picker.RowClose {
			out = append(out, r)
		}
	}
	return out
}

// sections returns the Section text of every header row, in order.
func sections(rows []picker.CloseRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Kind == picker.RowSectionHeader {
			out = append(out, r.Section)
		}
	}
	return out
}

func TestBuildCloseList_SectionAssignment(t *testing.T) {
	evs := []store.Event{
		{ID: 1, Ts: 100}, // mine: current session, window scope
		{ID: 2, Ts: 200}, // other session entirely
		{ID: 3, Ts: 300}, // current session, but scope session -> not "mine"
	}
	ctxs := map[int64]picker.CloseContext{
		1: windowCtx("mono", 1, "main", "claude", "/home/mono"),
		2: windowCtx("lazytmux", 1, "shell", "fish", "/home/lazytmux"),
		3: sessionCtx("mono", oneWindow("recreate")),
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")

	if got, want := sections(rows), []string{"THIS SESSION · mono", "OTHER SESSIONS"}; !equalStrings(got, want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	closes := closeRows(rows)
	if len(closes) != 3 {
		t.Fatalf("close rows = %d, want 3", len(closes))
	}
	if closes[0].EventID != 1 {
		t.Errorf("this-session row EventID = %d, want 1", closes[0].EventID)
	}
	// Newest first within OTHER: event 3 (Ts 300) before event 2 (Ts 200).
	if closes[1].EventID != 3 || closes[2].EventID != 2 {
		t.Errorf("other-session event ids = [%d %d], want [3 2]", closes[1].EventID, closes[2].EventID)
	}
}

func TestBuildCloseList_NewestFirstWithinSection(t *testing.T) {
	evs := []store.Event{
		{ID: 1, Ts: 100},
		{ID: 2, Ts: 300},
		{ID: 3, Ts: 200},
	}
	ctxs := map[int64]picker.CloseContext{
		1: windowCtx("other", 1, "a", "fish", "/a"),
		2: windowCtx("other", 2, "b", "fish", "/b"),
		3: windowCtx("other", 3, "c", "fish", "/c"),
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")
	closes := closeRows(rows)

	ids := []int64{closes[0].EventID, closes[1].EventID, closes[2].EventID}
	if want := []int64{2, 3, 1}; ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Errorf("event ids = %v, want %v (newest ts first)", ids, want)
	}
}

func TestBuildCloseList_HeaderSuppressedWhenSectionEmpty(t *testing.T) {
	evs := []store.Event{{ID: 1, Ts: 100}}
	ctxs := map[int64]picker.CloseContext{1: windowCtx("lazytmux", 1, "shell", "fish", "/home/lazytmux")}

	rows := picker.BuildCloseList(evs, ctxs, "mono")

	if got, want := sections(rows), []string{"OTHER SESSIONS"}; !equalStrings(got, want) {
		t.Fatalf("sections = %v, want %v (no this-session closes)", got, want)
	}
}

func TestBuildCloseList_CollapsesIdenticalPair(t *testing.T) {
	evs := []store.Event{
		{ID: 1, Ts: 100},
		{ID: 2, Ts: 300},
	}
	ctxs := map[int64]picker.CloseContext{
		1: windowCtx("mono", 1, "main", "claude", "/home/mono"),
		2: windowCtx("mono", 1, "main", "claude", "/home/mono"),
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")
	closes := closeRows(rows)

	if len(closes) != 1 {
		t.Fatalf("close rows = %d, want 1 (collapsed)", len(closes))
	}
	if closes[0].Count != 2 {
		t.Errorf("Count = %d, want 2", closes[0].Count)
	}
	if closes[0].Ts != 300 {
		t.Errorf("Ts = %d, want 300 (newest)", closes[0].Ts)
	}
	if closes[0].EventID != 2 {
		t.Errorf("EventID = %d, want 2 (the newest surviving row)", closes[0].EventID)
	}
}

func TestBuildCloseList_DoesNotCollapseDifferingCwd(t *testing.T) {
	evs := []store.Event{
		{ID: 1, Ts: 100},
		{ID: 2, Ts: 300},
	}
	ctxs := map[int64]picker.CloseContext{
		1: windowCtx("mono", 1, "main", "claude", "/home/mono/repo-a"),
		2: windowCtx("mono", 1, "main", "claude", "/home/mono/repo-b"),
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")
	closes := closeRows(rows)

	if len(closes) != 2 {
		t.Fatalf("close rows = %d, want 2 (cwd differs, must not collapse)", len(closes))
	}
	for _, r := range closes {
		if r.Count != 1 {
			t.Errorf("Count = %d, want 1 for uncollapsed row (event %d)", r.Count, r.EventID)
		}
	}
}

// TestBuildCloseList_PaneScopeKeysOnTheDiedPane checks that collapsing a
// pane-scope close reads the died pane (matched by Placement.PaneID), not
// whatever else the enclosing window's sub-manifest happens to carry: two
// closes must collapse when the died pane's command/cwd agree even though a
// sibling pane's fields differ, and must not collapse when the died pane's
// own fields differ even though a sibling's happen to agree.
func TestBuildCloseList_PaneScopeKeysOnTheDiedPane(t *testing.T) {
	evs := []store.Event{
		{ID: 1, Ts: 100},
		{ID: 2, Ts: 200},
		{ID: 3, Ts: 300},
	}
	ctxs := map[int64]picker.CloseContext{
		1: paneCtx("mono", 1, "main", "%died1", "claude", "/home/mono"),
		2: paneCtx("mono", 1, "main", "%died1", "claude", "/home/mono"),
		3: paneCtx("mono", 1, "main", "%died2", "vim", "/home/mono/other"),
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")
	closes := closeRows(rows)

	if len(closes) != 2 {
		t.Fatalf("close rows = %d, want 2 (events 1&2 collapse, event 3 stands alone)", len(closes))
	}
	if closes[0].EventID != 3 || closes[0].Count != 1 {
		t.Errorf("newest row = %+v, want event 3 uncollapsed", closes[0])
	}
	if closes[1].EventID != 2 || closes[1].Count != 2 {
		t.Errorf("older row = %+v, want events 1&2 collapsed (count 2)", closes[1])
	}
}

// TestBuildCloseList_EightSessionCloseCollapse is the known trap: eight
// closes of the same session, recreated and closed again each time with
// different contents (mirroring "session tmux-remux · 1w · (gone) →
// agentdetect-…" repeating with only the target name changing). A session
// close has no single closed pane, so the collapse key must not be derived
// from window/pane contents for scope "session" — otherwise these look like
// eight distinct events instead of one repeated one.
func TestBuildCloseList_EightSessionCloseCollapse(t *testing.T) {
	evs := make([]store.Event, 8)
	ctxs := map[int64]picker.CloseContext{}
	for i := range 8 {
		id := int64(i + 1)
		evs[i] = store.Event{ID: id, Ts: int64(100 * (i + 1))}
		ctxs[id] = sessionCtx("tmux-remux", oneWindow("agentdetect-"+string(rune('a'+i))))
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")
	closes := closeRows(rows)

	if len(closes) != 1 {
		t.Fatalf("close rows = %d, want 1 (all eight collapse)", len(closes))
	}
	if closes[0].Count != 8 {
		t.Errorf("Count = %d, want 8", closes[0].Count)
	}
	if closes[0].Ts != 800 {
		t.Errorf("Ts = %d, want 800 (newest)", closes[0].Ts)
	}
	if closes[0].Scope != "session" {
		t.Errorf("Scope = %q, want %q", closes[0].Scope, "session")
	}
}

func TestBuildCloseList_ExcludesEmptySubManifest(t *testing.T) {
	evs := []store.Event{
		{ID: 1, Ts: 100},
		{ID: 2, Ts: 200},
	}
	ctxs := map[int64]picker.CloseContext{
		1: windowCtx("mono", 1, "main", "claude", "/home/mono"),
		// Event 2 has no context at all: unrecoverable, must be excluded.
	}

	rows := picker.BuildCloseList(evs, ctxs, "mono")
	closes := closeRows(rows)

	if len(closes) != 1 || closes[0].EventID != 1 {
		t.Fatalf("close rows = %+v, want only event 1", closes)
	}
}

func TestCloseRow_Selectable(t *testing.T) {
	header := picker.CloseRow{Kind: picker.RowSectionHeader}
	if header.Selectable() {
		t.Error("section header must not be selectable")
	}
	row := picker.CloseRow{Kind: picker.RowClose}
	if !row.Selectable() {
		t.Error("close row must be selectable")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
