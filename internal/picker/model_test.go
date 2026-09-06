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

// TestModel_CollapsedRunningSessionExplainsHiddenPanes guards the fix for a
// session auto-collapsed by the running-session filter: the bullet still
// promises expandable content, but every descendant is filtered out too, so
// the row must say why instead of just "(running)".
func TestModel_CollapsedRunningSessionExplainsHiddenPanes(t *testing.T) {
	events := []store.Event{{
		ID: 1, Ts: time.Now().UnixMilli(), Kind: "snapshot",
		ManifestJSON: `{"v":1,"sessions":[{"name":"demo","windows":[{"name":"w","panes":[{"index":0,"command":"nvim"},{"index":1,"command":"fish"}]}]}]}`,
	}}
	running := map[string]bool{"demo": true}
	m := picker.NewPickerModel(picker.ModeSnapshot, events, running, nil)
	m.Bootstrap()
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	pm := upd.(picker.PickerModel)

	out := pm.View().Content
	if !strings.Contains(out, "2 panes hidden — d:ski") {
		t.Errorf("expected hidden-panes note, got:\n%s", out)
	}
	if strings.Contains(out, "(running)") {
		t.Errorf("collapsed running session should not fall back to bare (running), got:\n%s", out)
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
	m.SetCloseRows(picker.BuildCloseList(events, ctxs, "mono"))
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
	if !strings.Contains(out, "win → mono:4") {
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
	m.SetCloseRows(picker.BuildCloseList(nil, nil, ""))
	m.SetHiddenCount(5)
	m.Bootstrap()
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	out := upd.(picker.PickerModel).View().Content
	if !strings.Contains(out, "No recoverable closes (5 hidden)") {
		t.Errorf("expected all-hidden empty state, got:\n%s", out)
	}
}

// closeRowsModel builds a close-mode picker over the flat row list: THIS
// SESSION (newest first: events 1, 2) then OTHER SESSIONS (event 3). Each
// event's context carries a real
// pane with a ScrollbackSHA so the preview-loading tests have something to
// schedule.
func closeRowsModel(t *testing.T, sb *scrollback.Store) picker.PickerModel {
	t.Helper()
	subManifest := func(session, sha string) snapshot.Manifest {
		return snapshot.Manifest{Sessions: []snapshot.Session{{
			Name: session,
			Windows: []snapshot.Window{{
				Index: 1, Name: "win",
				Panes: []snapshot.Pane{{Index: 1, ID: "%1", Command: "fish", ScrollbackSHA: sha}},
			}},
		}}}
	}
	ctxs := map[int64]picker.CloseContext{
		1: {Placement: picker.ClosePlacement{Session: "mono", WindowIndex: 1, Scope: "window"}, SubManifest: subManifest("mono", "sha1")},
		2: {Placement: picker.ClosePlacement{Session: "mono", WindowIndex: 2, Scope: "window"}, SubManifest: subManifest("mono", "sha2")},
		3: {Placement: picker.ClosePlacement{Session: "lazytmux", WindowIndex: 1, Scope: "window"}, SubManifest: subManifest("lazytmux", "sha3")},
	}
	rows := []picker.CloseRow{
		{Kind: picker.RowSectionHeader, Section: "THIS SESSION · mono"},
		{Kind: picker.RowClose, EventID: 1, Ts: 300, Scope: "window", Session: "mono"},
		{Kind: picker.RowClose, EventID: 2, Ts: 200, Scope: "window", Session: "mono"},
		{Kind: picker.RowSectionHeader, Section: "OTHER SESSIONS"},
		{Kind: picker.RowClose, EventID: 3, Ts: 100, Scope: "window", Session: "lazytmux"},
	}
	m := picker.NewPickerModel(picker.ModeClose, nil, nil, sb)
	m.SetCloseContexts(ctxs)
	m.SetCloseRows(rows)
	m.Bootstrap()
	return m
}

func TestModel_CloseRowsCursorStartsOnNewestSelectableClose(t *testing.T) {
	m := closeRowsModel(t, nil)
	if got := m.CurrentEventID(); got != 1 {
		t.Errorf("CurrentEventID = %d, want 1 (newest close)", got)
	}
}

func TestModel_CloseRowsUpDownSkipHeaders(t *testing.T) {
	m := closeRowsModel(t, nil)
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	pm := upd.(picker.PickerModel)
	if got := pm.CurrentEventID(); got != 2 {
		t.Fatalf("after Down, CurrentEventID = %d, want 2", got)
	}
	upd, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	pm = upd.(picker.PickerModel)
	if got := pm.CurrentEventID(); got != 3 {
		t.Fatalf("after second Down, CurrentEventID = %d, want 3 (stepping over the OTHER SESSIONS header)", got)
	}
	upd, _ = pm.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	pm = upd.(picker.PickerModel)
	if got := pm.CurrentEventID(); got != 2 {
		t.Fatalf("after Up, CurrentEventID = %d, want 2 (stepping back over the header)", got)
	}
}

func TestModel_CloseRowsLeftRightAreInert(t *testing.T) {
	m := closeRowsModel(t, nil)
	before := m.Cursor()
	for _, code := range []rune{tea.KeyLeft, tea.KeyRight} {
		upd, cmd := m.Update(tea.KeyPressMsg{Code: code})
		pm := upd.(picker.PickerModel)
		if pm.Cursor() != before {
			t.Errorf("key %v moved the cursor: %d -> %d", code, before, pm.Cursor())
		}
		if cmd != nil {
			t.Errorf("key %v returned a command; the flat list has nothing to expand or collapse", code)
		}
	}
}

func TestModel_CloseRowsEnterSelectsNewestClose(t *testing.T) {
	m := closeRowsModel(t, nil)
	upd, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pm := upd.(picker.PickerModel)
	if pm.SelectedID() != 1 {
		t.Errorf("SelectedID = %d, want 1 (the newest close)", pm.SelectedID())
	}
	if cmd == nil {
		t.Error("Enter on a selectable row should quit the program")
	}
}

// Close mode's key handler claims Up/Down/Enter for the whole mode, not just
// for a non-empty list. m.events in close mode holds every close event
// including the unrecoverable ones BuildCloseList drops, so a fall-through to
// the snapshot handler would page a list the user cannot see and let Enter
// select a row that has nothing to restore.
func TestModel_CloseModeWithNoRowsNeverPagesTheEventSlice(t *testing.T) {
	evs := []store.Event{{ID: 7, Ts: 300, Kind: "pane-died"}, {ID: 8, Ts: 200, Kind: "pane-died"}}
	m := picker.NewPickerModel(picker.ModeClose, evs, nil, nil)
	m.SetCloseRows(picker.BuildCloseList(evs, nil, "mono")) // no contexts: nothing recoverable
	m.SetHiddenCount(len(evs))
	m.Bootstrap()

	for _, code := range []rune{tea.KeyDown, tea.KeyDown, tea.KeyUp} {
		upd, _ := m.Update(tea.KeyPressMsg{Code: code})
		m = upd.(picker.PickerModel)
		if m.Cursor() != 0 {
			t.Fatalf("key %v moved the cursor to %d over an empty list", code, m.Cursor())
		}
		if id := m.CurrentEventID(); id != 0 {
			t.Fatalf("key %v put event %d under the cursor; the list is empty", code, id)
		}
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := upd.(picker.PickerModel).SelectedID(); got != 0 {
		t.Errorf("Enter selected event %d; an empty list has nothing to restore", got)
	}
}

// The cursor sitting one past the last row must read as "nothing selected",
// not index out of range: len(rows) is the first invalid index, so the bound
// has to be >=, not >.
func TestModel_CurrentEventIDAtTheRowJustPastTheEnd(t *testing.T) {
	m := closeRowsModel(t, nil)
	rows := 5 // the fixture's three closes plus its two section headers
	m.SetCursor(rows - 1)
	if got := m.CurrentEventID(); got != 3 {
		t.Fatalf("cursor on the last row: CurrentEventID = %d, want 3", got)
	}
	m.SetCursor(rows)
	if got := m.CurrentEventID(); got != 0 {
		t.Errorf("cursor one past the last row: CurrentEventID = %d, want 0", got)
	}
}

// #103 fixed a regression where nothing scheduled the cursor's scrollback
// load until the first key arrived, leaving the first frame's preview blank
// even though the scrollback existed. Bootstrap+Init must keep scheduling it
// for the flat list too.
func TestModel_CloseRowsInitSchedulesFirstScrollbackLoad(t *testing.T) {
	sb := scrollback.New(t.TempDir())
	m := closeRowsModel(t, sb)
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init scheduled no load for the initial close row's scrollback")
	}
}
