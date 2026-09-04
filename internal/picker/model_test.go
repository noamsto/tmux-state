package picker_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/noamsto/tmux-remux/internal/picker"
	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

func TestModel_TabSwitchesFocus_SnapshotMode(t *testing.T) {
	events := []store.Event{{ID: 1, Kind: "snapshot", ManifestJSON: `{"v":1,"sessions":[]}`}}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)

	// Initial focus is list. After tab, should be tree.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	pm := updated.(picker.PickerModel)
	if pm.Focus() != picker.FocusTree {
		t.Errorf("after tab: focus=%v, want focusTree", pm.Focus())
	}

	// Tab again returns to list.
	updated, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	pm = updated.(picker.PickerModel)
	if pm.Focus() != picker.FocusList {
		t.Errorf("after second tab: focus=%v, want focusList", pm.Focus())
	}
}

func TestModel_ToggleIdleUpdatesCounter(t *testing.T) {
	events := []store.Event{
		{
			ID: 1, Kind: "snapshot",
			ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"name":"w","panes":[
				{"index":0,"command":"fish","child_count":0},
				{"index":1,"command":"nvim","child_count":0}
			]}]}]}`,
		},
	}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()

	// Before toggle: 2 panes kept.
	if c := m.CurrentCounts(); c.KeptPanes != 2 || c.SkippedPanes != 0 {
		t.Fatalf("before toggle: counts=%+v", c)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: 's'})
	pm := updated.(picker.PickerModel)

	// After "skip idle shells": fish (idle) skipped, nvim kept.
	if c := pm.CurrentCounts(); c.KeptPanes != 1 || c.SkippedPanes != 1 {
		t.Errorf("after toggle: counts=%+v", c)
	}
}

func TestModel_CursorMoveTriggersManifestParse(t *testing.T) {
	events := []store.Event{
		{ID: 1, Kind: "snapshot", ManifestJSON: `{"v":1,"sessions":[{"name":"a","windows":[]}]}`},
		{ID: 2, Kind: "snapshot", ManifestJSON: `{"v":1,"sessions":[{"name":"b","windows":[]}]}`},
	}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	pm := updated.(picker.PickerModel)
	if pm.Cursor() != 1 {
		t.Errorf("after Down: cursor=%d, want 1", pm.Cursor())
	}
	tree := pm.TreeFor(2)
	if tree == nil || len(tree.Children) != 1 || tree.Children[0].Label != "b (0w)" {
		t.Errorf("tree for event 2 not built correctly: %+v", tree)
	}
}

func TestModel_EnterRecordsSelectedID(t *testing.T) {
	events := []store.Event{
		{ID: 7, Kind: "snapshot", ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[]}]}`},
	}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pm := updated.(picker.PickerModel)
	if pm.SelectedID() != 7 {
		t.Errorf("selectedID=%d, want 7", pm.SelectedID())
	}
	if cmd == nil {
		t.Error("expected tea.Quit cmd, got nil")
	}
}

func TestModel_EnterBlockedOnParseError(t *testing.T) {
	events := []store.Event{
		{ID: 9, Kind: "snapshot", ManifestJSON: `{not json`},
	}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pm := updated.(picker.PickerModel)
	if pm.SelectedID() != 0 {
		t.Errorf("selectedID=%d, want 0 (blocked)", pm.SelectedID())
	}
	if cmd != nil {
		t.Error("expected no quit cmd on parse error")
	}
	if pm.FooterNote() == "" {
		t.Error("expected footer warning to be set")
	}
}

func TestModel_TreeLeftCollapsesAncestorRightReExpands(t *testing.T) {
	events := []store.Event{{
		ID: 1, Kind: "snapshot",
		ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"name":"w","panes":[
			{"index":0,"command":"fish"}
		]}]}]}`,
	}}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()
	// Tab into tree: cursor lands on the pane.
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	pm := upd.(picker.PickerModel)
	// Left from the pane collapses the parent window; cursor moves to the window.
	upd, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	pm = upd.(picker.PickerModel)
	nodes := pm.VisibleNodes()
	if len(nodes) != 2 {
		t.Fatalf("after Left from pane: visible=%d, want 2 (session + collapsed window)", len(nodes))
	}
	if nodes[pm.TreeCursor()].Kind != picker.NodeWindow {
		t.Errorf("after Left from pane: cursor on %v, want NodeWindow", nodes[pm.TreeCursor()].Kind)
	}
	// Right re-expands and snaps back to the pane within.
	upd, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	pm = upd.(picker.PickerModel)
	nodes = pm.VisibleNodes()
	if nodes[pm.TreeCursor()].Kind != picker.NodePane {
		t.Errorf("after Right re-expand: cursor on %v, want NodePane", nodes[pm.TreeCursor()].Kind)
	}
}

func TestModel_TreeFocusLandsOnFirstPane(t *testing.T) {
	events := []store.Event{{
		ID: 1, Kind: "snapshot",
		ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"name":"w","panes":[
			{"index":0,"command":"fish"},
			{"index":1,"command":"vim"}
		]}]}]}`,
	}}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()
	// Tab into tree focus. Cursor should snap to the first pane, skipping
	// session/window nodes that have no preview.
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	pm := upd.(picker.PickerModel)
	nodes := pm.VisibleNodes()
	cur := pm.TreeCursor()
	if cur < 0 || cur >= len(nodes) || nodes[cur].Kind != picker.NodePane {
		t.Fatalf("after Tab: cursor=%d, want a NodePane", cur)
	}
	// Down should move to the next pane, skipping over any non-pane between.
	upd, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	pm = upd.(picker.PickerModel)
	nodes = pm.VisibleNodes()
	cur = pm.TreeCursor()
	if cur < 0 || cur >= len(nodes) || nodes[cur].Kind != picker.NodePane {
		t.Fatalf("after Down: cursor=%d, want a NodePane", cur)
	}
}

func TestModel_ViewRendersWithoutPanic(t *testing.T) {
	events := []store.Event{
		{ID: 1, Ts: time.Now().UnixMilli(), Kind: "snapshot",
			ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"name":"w","panes":[{"index":0,"command":"fish"}]}]}]}`},
	}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()
	// Simulate a sane terminal size.
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	pm := upd.(picker.PickerModel)
	view := pm.View()
	out := view.Content // tea.View exposes rendered content via the Content field
	if out == "" {
		t.Fatal("View() returned empty string")
	}
	if !strings.Contains(out, "s (1w)") {
		t.Errorf("expected session label in view, got:\n%s", out)
	}
}

func TestNewPickerModel_AcceptsScrollbackStore(t *testing.T) {
	tmp := t.TempDir()
	sb := scrollback.New(tmp)
	m := picker.NewPickerModel(picker.ModeSnapshot, nil, nil, sb)
	if m.ScrollbackStore() != sb {
		t.Fatalf("scrollback store not threaded through constructor")
	}
}

func TestModel_ViewHighlightsTreeCursor(t *testing.T) {
	events := []store.Event{{
		ID: 1, Ts: time.Now().UnixMilli(), Kind: "snapshot",
		ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"name":"w","panes":[{"index":0,"command":"fish"}]}]}]}`,
	}}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, nil, nil)
	m.Bootstrap()
	// Resize so two-pane mode kicks in.
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	pm := upd.(picker.PickerModel)
	// Move focus to tree.
	upd, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	pm = upd.(picker.PickerModel)
	out := pm.View().Content
	// Active row style sets a mauve background (#cba6f7 = 203;166;247 in 24-bit SGR).
	// When focus is on the tree pane, the first visible node must be highlighted.
	if !strings.Contains(out, "203;166;247") {
		t.Errorf("expected mauve-background highlight in tree pane, got:\n%s", out)
	}
}

// closeModel builds a close-mode picker with one recoverable event whose
// context carries a label + a one-session sub-manifest, plus a hidden count.
func closeModel(t *testing.T, hidden int) picker.PickerModel {
	t.Helper()
	events := []store.Event{{ID: 1, Ts: time.Now().UnixMilli(), Kind: "window-unlinked"}}
	ctxs := map[int64]picker.CloseContext{
		1: {
			Label:     "mono/win (1p)",
			Placement: picker.ClosePlacement{Session: "mono", WindowIndex: 4, WindowName: "win", Scope: "pane"},
			SubManifest: snapshot.Manifest{V: 1, Sessions: []snapshot.Session{{
				Name: "mono", Windows: []snapshot.Window{{Index: 4, Name: "win"}},
			}}},
		},
	}
	running := map[string]bool{"mono": true}
	m := picker.NewPickerModel(picker.ModeClose, events, running, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(picker.BuildCloseTree(events, ctxs, "mono", running))
	m.SetHiddenCount(hidden)
	m.Bootstrap()
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return upd.(picker.PickerModel)
}

func TestModel_CloseModeShowsHiddenCountLine(t *testing.T) {
	out := closeModel(t, 14).View().Content
	if !strings.Contains(out, "14 unrecoverable closes hidden") {
		t.Errorf("expected hidden-count line, got:\n%s", out)
	}
	if !strings.Contains(out, "mono/win (1p)") {
		t.Errorf("recoverable row should still render, got:\n%s", out)
	}
}

func TestModel_CloseModeHiddenCountSingular(t *testing.T) {
	out := closeModel(t, 1).View().Content
	if !strings.Contains(out, "1 unrecoverable close hidden") {
		t.Errorf("expected singular phrasing, got:\n%s", out)
	}
	if strings.Contains(out, "closes hidden") {
		t.Errorf("singular count must not use plural noun, got:\n%s", out)
	}
}

func TestModel_CloseModeNoHiddenLineWhenZero(t *testing.T) {
	out := closeModel(t, 0).View().Content
	if strings.Contains(out, "hidden") {
		t.Errorf("no hidden line expected when count is 0, got:\n%s", out)
	}
}

func TestModel_CloseModeAllHiddenEmptyState(t *testing.T) {
	m := picker.NewPickerModel(picker.ModeClose, nil, nil, nil)
	m.SetCloseTree(picker.BuildCloseTree(nil, nil, "", nil))
	m.SetHiddenCount(5)
	m.Bootstrap()
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	out := upd.(picker.PickerModel).View().Content
	if !strings.Contains(out, "No recoverable closes (5 hidden)") {
		t.Errorf("expected all-hidden empty state, got:\n%s", out)
	}
}

// closeTreeModel builds a close-mode model over two sessions: mono (this
// session) with window 2 holding a dead pane, and lazytmux with its own close.
func closeTreeModel() picker.PickerModel {
	evs := []store.Event{
		{ID: 1, Ts: 300, Kind: "window-unlinked"},
		{ID: 2, Ts: 200, Kind: "pane-died"},
		{ID: 3, Ts: 100, Kind: "window-unlinked"},
	}
	ctxs := map[int64]picker.CloseContext{
		1: closeCtx("mono", 2, "main", "window", 1),
		2: closeCtx("mono", 2, "main", "pane", 0),
		3: closeCtx("lazytmux", 3, "docs", "window", 1),
	}
	m := picker.NewPickerModel(picker.ModeClose, evs, map[string]bool{"mono": true}, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(picker.BuildCloseTree(evs, ctxs, "mono", map[string]bool{"mono": true}))
	m.Bootstrap()
	return m
}

func TestCloseTreeCursorStartsOnNewestSelectableClose(t *testing.T) {
	m := closeTreeModel()
	if got := m.CurrentEventID(); got != 1 {
		t.Errorf("CurrentEventID = %d, want 1 (this session's newest close)", got)
	}
}

func TestCloseTreeDownSkipsGroupHeaders(t *testing.T) {
	m := closeTreeModel()
	// From the window close, Down must land on the nested pane, not on a header.
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	got := next.(picker.PickerModel)
	if id := got.CurrentEventID(); id != 2 {
		t.Errorf("after Down, CurrentEventID = %d, want the nested pane event 2", id)
	}
}

func TestCloseTreeEnterOnHeaderDoesNotSelect(t *testing.T) {
	m := closeTreeModel()
	// Collapse this-session so the cursor sits on the group header itself.
	vis := m.CloseVisible()
	if len(vis) == 0 || !picker.IsCloseGroup(vis[0]) {
		t.Fatalf("expected a group header first, got %+v", vis)
	}
	m.SetCursor(0)
	after, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := after.(picker.PickerModel)
	if got.SelectedID() != 0 {
		t.Errorf("SelectedID = %d, want 0 — a header carries nothing to restore", got.SelectedID())
	}
	if got.FooterNote() == "" {
		t.Error("expected a footer note explaining the header is not restorable")
	}
}

func TestCloseTreeRightExpandsCollapsedGroup(t *testing.T) {
	m := closeTreeModel()
	// The other-sessions group starts collapsed; step onto it and expand.
	vis := m.CloseVisible()
	idx := -1
	for i, n := range vis {
		if n.Kind == picker.GroupOther {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("expected an other-sessions group header")
	}
	m.SetCursor(idx)
	after, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	newVis := after.(picker.PickerModel).CloseVisible()

	// The lazytmux fixture nests a session header over one window close, and
	// both come back expanded by default, so both rows should surface at once
	// right after the group header.
	revealed := newVis[idx+1:]
	wantLabels := []string{"lazytmux", "3: docs (1p)"}
	if len(revealed) != len(wantLabels) {
		t.Fatalf("revealed rows = %v, want %v", revealedLabels(revealed), wantLabels)
	}
	for i, want := range wantLabels {
		if got := revealed[i].Label; got != want {
			t.Errorf("revealed[%d] = %q, want %q", i, got, want)
		}
	}
}

// revealedLabels renders a []*CloseNode's labels for a failure message.
func revealedLabels(nodes []*picker.CloseNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label
	}
	return out
}

// nestedCloseModel builds a bootstrapped close-mode picker over a session →
// window → pane scaffolding chain. Distinct from the existing closeModel(t,
// hidden) at model_test.go:228, which has no scaffolding to skip: lazytmux is gone, its window 3 is a live header, and
// the only restorable thing inside it is a pane close.
func nestedCloseModel(t *testing.T) picker.PickerModel {
	t.Helper()
	evs := []store.Event{{ID: 1, Ts: 300, Kind: "pane-died"}}
	one := snapshot.Manifest{Sessions: []snapshot.Session{{Name: "lazytmux"}}}
	ctxs := map[int64]picker.CloseContext{
		1: {
			Label:       "pane: fish",
			Placement:   picker.ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane"},
			SubManifest: one,
		},
	}
	m := picker.NewPickerModel(picker.ModeClose, evs, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(picker.BuildCloseTree(evs, ctxs, "mono", map[string]bool{}))
	m.Bootstrap()
	return m
}

// Scaffolding rows exist only to indent something restorable. Stopping on them
// offers a row and then refuses it with "(group — nothing to restore here)".
func TestModel_CloseNavigationSkipsScaffolding(t *testing.T) {
	m := nestedCloseModel(t)
	pm := m
	// Walk the whole tree downward, then all the way back up.
	for _, code := range []rune{'j', 'j', 'j', 'j', 'k', 'k', 'k', 'k'} {
		updated, _ := pm.Update(tea.KeyPressMsg{Code: code})
		pm = updated.(picker.PickerModel)
		vis := pm.CloseVisible()
		n := vis[pm.Cursor()]
		if n.EventID == 0 && !picker.IsCloseGroup(n) {
			t.Fatalf("cursor landed on scaffolding row %q", n.Label)
		}
	}
}

// Enter on a scaffolding row is now unreachable, so the only row that can
// produce the group note is a group header.
func TestModel_CloseEnterOnScaffoldingIsUnreachable(t *testing.T) {
	m := nestedCloseModel(t)
	pm := m
	for i := 0; i < 6; i++ {
		updated, _ := pm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(picker.PickerModel)
		vis := got.CloseVisible()
		if note := got.FooterNote(); note != "" && !picker.IsCloseGroup(vis[got.Cursor()]) {
			t.Fatalf("Enter refused a non-group row %q with %q", vis[got.Cursor()].Label, note)
		}
		updated, _ = got.Update(tea.KeyPressMsg{Code: 'j'})
		pm = updated.(picker.PickerModel)
	}
}

// With scaffolding no longer a cursor stop, Left must collapse the nearest
// collapsible ancestor and leave the cursor somewhere it may legally sit.
func TestModel_CloseLeftCollapsesAncestorAndLandsNavigable(t *testing.T) {
	m := nestedCloseModel(t)
	// Cursor starts on the newest restorable row: the nested pane close.
	pm := m
	before := len(pm.CloseVisible())
	updated, _ := pm.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	pm = updated.(picker.PickerModel)

	if after := len(pm.CloseVisible()); after >= before {
		t.Errorf("Left did not collapse anything: %d rows before, %d after", before, after)
	}
	vis := pm.CloseVisible()
	if pm.Cursor() < 0 || pm.Cursor() >= len(vis) {
		t.Fatalf("cursor %d out of range for %d rows", pm.Cursor(), len(vis))
	}
	n := vis[pm.Cursor()]
	if n.EventID == 0 && !picker.IsCloseGroup(n) {
		t.Errorf("Left left the cursor on scaffolding row %q", n.Label)
	}
}
