package picker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

func TestLoadScrollbackCmd_ReturnsContent(t *testing.T) {
	tmp := t.TempDir()
	sb := scrollback.New(tmp)
	sha, _, err := sb.Put(context.Background(), []byte("hello scrollback"))
	if err != nil {
		t.Fatalf("seed scrollback: %v", err)
	}

	cmd := loadScrollbackCmd(sb, sha)
	if cmd == nil {
		t.Fatal("loadScrollbackCmd returned nil")
	}
	msg := cmd()
	loaded, ok := msg.(scrollbackLoadedMsg)
	if !ok {
		t.Fatalf("expected scrollbackLoadedMsg, got %T", msg)
	}
	if loaded.sha != sha {
		t.Errorf("sha mismatch: got %q want %q", loaded.sha, sha)
	}
	if loaded.err != nil {
		t.Errorf("unexpected err: %v", loaded.err)
	}
	if !strings.Contains(string(loaded.content), "hello scrollback") {
		t.Errorf("content mismatch: got %q", loaded.content)
	}
}

func TestLoadScrollbackCmd_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	sb := scrollback.New(tmp)
	// all-zeros is not a valid sha256 output for any input, so this file is guaranteed absent
	const missing = "0000000000000000000000000000000000000000000000000000000000000000"

	cmd := loadScrollbackCmd(sb, missing)
	msg := cmd()
	loaded, ok := msg.(scrollbackLoadedMsg)
	if !ok {
		t.Fatalf("expected scrollbackLoadedMsg, got %T", msg)
	}
	if loaded.err == nil {
		t.Fatal("expected err for missing scrollback, got nil")
	}
	if !errors.Is(loaded.err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist-chain error, got %v", loaded.err)
	}
}

func TestPickerModel_HandlesScrollbackLoadedMsg(t *testing.T) {
	m := NewPickerModel(ModeSnapshot, nil, nil, nil)
	msg := scrollbackLoadedMsg{sha: "deadbeef", content: []byte("hi"), err: nil}
	updated, _ := m.Update(msg)
	final := updated.(PickerModel)
	got, ok := final.ScrollbackFor("deadbeef")
	if !ok {
		t.Fatalf("cache miss for sha after loaded msg")
	}
	if string(got) != "hi" {
		t.Errorf("content mismatch: got %q want %q", got, "hi")
	}
}

func TestPickerModel_RemembersScrollbackError(t *testing.T) {
	m := NewPickerModel(ModeSnapshot, nil, nil, nil)
	wantErr := errors.New("boom")
	msg := scrollbackLoadedMsg{sha: "deadbeef", err: wantErr}
	updated, _ := m.Update(msg)
	final := updated.(PickerModel)
	if got := final.ScrollbackError("deadbeef"); !errors.Is(got, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, got)
	}
}

func TestPickerModel_FocusedPaneTriggersLoad(t *testing.T) {
	// Build a minimal manifest with one pane carrying a scrollback SHA.
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "s1",
			Windows: []snapshot.Window{{
				Index: 0, Name: "w1",
				Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Command: "bash", ScrollbackSHA: "abc123"}},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}

	tmp := t.TempDir()
	sb := scrollback.New(tmp)

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, sb)
	m.Bootstrap()
	// Focus tree, then walk cursor down session → window → pane.
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)

	cmd := m.PreviewCmd()
	if cmd == nil {
		t.Fatal("PreviewCmd returned nil for a pane with scrollback")
	}
}

func TestPickerModel_NoLoadWhenAlreadyCached(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Windows: []snapshot.Window{{
				Panes: []snapshot.Pane{{ScrollbackSHA: "abc123"}},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}
	tmp := t.TempDir()
	sb := scrollback.New(tmp)

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, sb)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)
	m.scrollbacks["abc123"] = []byte("cached")

	if cmd := m.PreviewCmd(); cmd != nil {
		t.Fatal("PreviewCmd should be nil when SHA already cached")
	}
}

func TestPickerModel_NoLoadWhenAlreadyInFlight(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Windows: []snapshot.Window{{
				Panes: []snapshot.Pane{{ScrollbackSHA: "abc123"}},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}
	tmp := t.TempDir()
	sb := scrollback.New(tmp)

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, sb)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)

	if cmd := m.PreviewCmd(); cmd == nil {
		t.Fatal("first PreviewCmd should return a Cmd")
	}
	if cmd := m.PreviewCmd(); cmd != nil {
		t.Fatal("second PreviewCmd should be nil when SHA is in-flight")
	}
}

// paneNodeIndex returns the index of the first NodePane in m's visible tree,
// failing the test if none exists. Use this instead of hard-coded cursor indices.
func paneNodeIndex(t *testing.T, m PickerModel) int {
	t.Helper()
	for i, n := range m.VisibleNodes() {
		if n.Kind == NodePane {
			return i
		}
	}
	t.Fatal("no pane node in visible tree")
	return -1
}

func TestPaneWidths_ThreePane(t *testing.T) {
	m := PickerModel{mode: ModeSnapshot, width: 160}
	l, tr, pv := m.paneWidthsThree()
	if l+tr+pv != 160 {
		t.Errorf("widths must sum to total: got %d+%d+%d != 160", l, tr, pv)
	}
	if l < 28 || tr < 32 || pv < 40 {
		t.Errorf("min widths violated: l=%d tr=%d pv=%d", l, tr, pv)
	}
}

func TestPaneWidths_NarrowFallsBackToTwoPane(t *testing.T) {
	m := PickerModel{mode: ModeSnapshot, width: 100}
	l, tr, pv := m.paneWidthsThree()
	if pv != 0 {
		t.Errorf("preview should be 0 at width=100, got %d", pv)
	}
	if l+tr != 100 {
		t.Errorf("widths must sum to total: got %d+%d != 100", l, tr)
	}
}

func TestRenderPreview_NoPaneFocused(t *testing.T) {
	m := PickerModel{mode: ModeSnapshot, width: 160, height: 30, focus: focusList}
	got := m.renderPreview(60)
	if !strings.Contains(stripANSI(got), "press Tab") {
		t.Errorf("expected hint, got: %q", got)
	}
}

func TestRenderPreview_PaneWithoutSHA(t *testing.T) {
	man := snapshot.Manifest{V: 1, Sessions: []snapshot.Session{{
		Windows: []snapshot.Window{{Panes: []snapshot.Pane{{}}}},
	}}}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 1, Kind: "snapshot", ManifestJSON: string(raw)}
	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)

	got := m.renderPreview(60)
	if !strings.Contains(stripANSI(got), "no scrollback captured") {
		t.Errorf("expected hint, got: %q", got)
	}
}

func TestRenderPreview_Loaded(t *testing.T) {
	man := snapshot.Manifest{V: 1, Sessions: []snapshot.Session{{
		Windows: []snapshot.Window{{Panes: []snapshot.Pane{{ScrollbackSHA: "abc"}}}},
	}}}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 1, Kind: "snapshot", ManifestJSON: string(raw)}
	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)
	m.scrollbacks["abc"] = []byte("$ echo hi\nhi\n$ ")

	got := stripANSI(m.renderPreview(60))
	if !strings.Contains(got, "echo hi") {
		t.Errorf("expected content, got: %q", got)
	}
}

func TestRenderPreview_Error(t *testing.T) {
	man := snapshot.Manifest{V: 1, Sessions: []snapshot.Session{{
		Windows: []snapshot.Window{{Panes: []snapshot.Pane{{ScrollbackSHA: "abc"}}}},
	}}}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 1, Kind: "snapshot", ManifestJSON: string(raw)}
	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)
	m.scrollbackErrors["abc"] = errors.New("file gone")

	got := stripANSI(m.renderPreview(60))
	if !strings.Contains(got, "missing") {
		t.Errorf("expected error label, got: %q", got)
	}
}

// stripANSI removes ANSI escapes for assertion ergonomics.
func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func TestPaneWidths_ClosePreviewNeedsWidth(t *testing.T) {
	narrow := PickerModel{mode: ModeClose, width: 100}
	if _, _, pv := narrow.paneWidthsThree(); pv != 0 {
		t.Errorf("close mode at 100 cols: got preview width %d, want 0", pv)
	}
	wide := PickerModel{mode: ModeClose, width: 130}
	l, tr, pv := wide.paneWidthsThree()
	if pv == 0 {
		t.Error("close mode at 130 cols: got no preview column, want one")
	}
	if l+tr+pv != 130 {
		t.Errorf("widths %d+%d+%d do not sum to 130", l, tr, pv)
	}
	if l < 32 {
		t.Errorf("list width %d is under the 32-cell floor", l)
	}
}

// A closed pane is read out of the snapshot the close was diffed against, so it
// carries that snapshot's ScrollbackSHA and the close picker can preview it.
func TestPickerModel_ClosePreviewsClosedPaneScrollback(t *testing.T) {
	const sha = "deadbeef"
	sub := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index: 1, Name: "tmux-remux",
				Panes: []snapshot.Pane{{Index: 1, ID: "%1", Cwd: "/tmp", Command: "fish", ScrollbackSHA: sha}},
			}},
		}},
	}
	ev := store.Event{ID: 7, Kind: "pane-died", ManifestJSON: `{"pane_id":"%1"}`}

	// A real close tree, because with one the cursor indexes close-tree rows
	// rather than the event slice — the path production actually takes.
	ctxs := map[int64]CloseContext{7: {
		Label:       "pane",
		Placement:   ClosePlacement{Session: "demo", WindowIndex: 1, Scope: "pane", PaneID: "%1"},
		SubManifest: sub,
	}}
	m := NewPickerModel(ModeClose, []store.Event{ev}, nil, scrollback.New(t.TempDir()))
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(BuildCloseTree([]store.Event{ev}, ctxs, "demo", nil))
	m.Bootstrap()
	m.width, m.height = 130, 20
	m.scrollbacks[sha] = []byte("step 34: reading files…")

	_, _, previewWidth := m.paneWidthsThree()
	got := m.renderPreview(previewWidth)
	if !strings.Contains(got, "step 34") {
		t.Errorf("close-mode preview does not show the pane's scrollback:\n%s", got)
	}
}

// A window node's preview shows the map, not the old "press →" hint.
func TestPickerModel_WindowNodeShowsMap(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Name:   "tmux-remux",
				Layout: "1cb4,145x36,0,0[145x18,0,0,0,145x17,0,19{72x17,0,19,1,72x17,73,19,2}]",
				Panes: []snapshot.Pane{
					{Index: 0, ID: "%0", Command: "nvim"},
					{Index: 1, ID: "%1", Command: "agent-work"},
					{Index: 2, ID: "%2", Command: "fish"},
				},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.width, m.height = 160, 24
	m.focus = focusTree
	m.treeCursor = windowNodeIndex(t, m)

	_, _, previewWidth := m.paneWidthsThree()
	got := m.renderPreview(previewWidth)
	if strings.Contains(got, "press → to expand") {
		t.Errorf("still showing the hint instead of a map:\n%s", got)
	}
	if !strings.ContainsRune(got, '┌') || !strings.Contains(got, "agent-work") {
		t.Errorf("no labelled box art in:\n%s", got)
	}
}

// With pane-base-index set, the layout string still numbers panes by tmux id
// (0,1,2) while the snapshot records pane_index 1,2,3. The map must match boxes
// to panes by id, so every box — including the top one the old index match left
// blank — is labelled with its own pane index and command, not a shifted one.
func TestPickerModel_WindowMapLabelsByPaneID(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Name:   "editor",
				Layout: "1cb4,145x36,0,0[145x18,0,0,0,145x17,0,19{72x17,0,19,1,72x17,73,19,2}]",
				Panes: []snapshot.Pane{
					{Index: 1, ID: "%0", Command: "nvim"},
					{Index: 2, ID: "%1", Command: "agentwork"},
					{Index: 3, ID: "%2", Command: "shellfish"},
				},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.width, m.height = 160, 24
	m.focus = focusTree
	m.treeCursor = windowNodeIndex(t, m)

	_, _, previewWidth := m.paneWidthsThree()
	got := m.renderPreview(previewWidth)
	// The top box is layout pane 0 → id %0 → pane index 1: it must read "1 nvim",
	// never blank and never a neighbour's command.
	for _, want := range []string{"1 nvim", "2 agentwork", "3 shellfish"} {
		if !strings.Contains(got, want) {
			t.Errorf("map missing %q:\n%s", want, got)
		}
	}
}

// The map lives on the window node, so Up/Down must be able to land there while
// it is expanded — not only by collapsing it with Left. Up from the first pane
// lands on the window node; Down steps back into the panes.
func TestPickerModel_WindowNodeReachableWhenExpanded(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Name:   "editor",
				Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
				Panes: []snapshot.Pane{
					{Index: 1, ID: "%0", Command: "nvim"},
					{Index: 2, ID: "%1", Command: "fish"},
				},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.width, m.height = 160, 24

	u, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = u.(PickerModel)
	u, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = u.(PickerModel)
	if got := m.VisibleNodes()[m.treeCursor].Kind; got != NodeWindow {
		t.Fatalf("Up from the first pane landed on %v, want the window node (the map)", got)
	}
	u, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = u.(PickerModel)
	if got := m.VisibleNodes()[m.treeCursor].Kind; got != NodePane {
		t.Errorf("Down from the window node landed on %v, want a pane", got)
	}
}

// A snapshot written before layouts were stored must not regress.
func TestPickerModel_WindowNodeWithoutLayoutKeepsHint(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name:    "demo",
			Windows: []snapshot.Window{{Index: 1, Panes: []snapshot.Pane{{Index: 0}}}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}

	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.width, m.height = 160, 24
	m.focus = focusTree
	m.treeCursor = windowNodeIndex(t, m)

	_, _, previewWidth := m.paneWidthsThree()
	if got := m.renderPreview(previewWidth); !strings.Contains(got, "press → to expand") {
		t.Errorf("want the hint for a layout-less window, got:\n%s", got)
	}
}

// In close mode the map dashes the pane the event took down.
func TestPickerModel_CloseMapMarksDeadPane(t *testing.T) {
	sub := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
				Panes: []snapshot.Pane{
					{Index: 0, ID: "%0", Command: "fish"},
					{Index: 1, ID: "%1", Command: "agent-work"},
				},
			}},
		}},
	}
	ev := store.Event{ID: 7, Kind: "pane-died", ManifestJSON: `{"pane_id":"%1"}`}
	ctxs := map[int64]CloseContext{7: {
		Label:       "pane",
		Placement:   ClosePlacement{Session: "demo", WindowIndex: 1, Scope: "pane", PaneID: "%1"},
		SubManifest: sub,
	}}

	m := NewPickerModel(ModeClose, []store.Event{ev}, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(BuildCloseTree([]store.Event{ev}, ctxs, "demo", nil))
	m.Bootstrap()
	m.width, m.height = 160, 24

	_, _, previewWidth := m.paneWidthsThree()
	got := m.renderPreview(previewWidth)
	if !strings.ContainsRune(got, '┄') && !strings.ContainsRune(got, '┆') {
		t.Errorf("dead pane not dashed in:\n%s", got)
	}
}

func windowNodeIndex(t *testing.T, m PickerModel) int {
	t.Helper()
	for i, n := range m.VisibleNodes() {
		if n.Kind == NodeWindow {
			return i
		}
	}
	t.Fatal("no window node in visible tree")
	return -1
}

// With demoKeys on, each key press is echoed into the footer so a screen
// recording can show what was pressed; off, nothing is echoed.
func TestPickerModel_DemoKeysEchoesLastKey(t *testing.T) {
	ev := store.Event{ID: 1, Kind: "snapshot", ManifestJSON: `{"v":1,"sessions":[]}`}

	on := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	on.demoKeys = true
	on.width, on.height = 160, 24
	u, _ := on.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	on = u.(PickerModel)
	if on.lastKey != "↓" {
		t.Errorf("lastKey = %q, want ↓", on.lastKey)
	}
	if foot := on.renderFooter(on.width); !strings.ContainsRune(foot, '↓') {
		t.Errorf("footer missing the key cast:\n%s", foot)
	}

	off := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	off.width, off.height = 160, 24
	u, _ = off.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	off = u.(PickerModel)
	if off.lastKey != "" {
		t.Errorf("lastKey = %q with demoKeys off, want empty", off.lastKey)
	}
}

// prefix+U used to open on "(press Tab to preview panes)". The preview must be
// live from the first frame, following the close-tree cursor.
func TestRenderPreview_CloseModeNeedsNoTab(t *testing.T) {
	m := nestedClosePreviewModel(t)
	_, _, previewW := m.paneWidthsThree()
	out := m.renderPreview(previewW)
	if strings.Contains(out, "press Tab") {
		t.Errorf("close preview still gated behind Tab:\n%s", out)
	}
	if !strings.Contains(out, "docs") {
		t.Errorf("close preview does not name the window it would restore:\n%s", out)
	}
}

// Moving the close cursor must move the preview with it, without a focus change.
func TestRenderPreview_CloseModeTracksTheCursor(t *testing.T) {
	m := nestedClosePreviewModel(t)
	_, _, previewW := m.paneWidthsThree()
	first := m.renderPreview(previewW)

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'j'})
	pm := updated.(PickerModel)
	if pm.focus != focusList {
		t.Fatalf("focus moved to %v; the preview should follow the list cursor", pm.focus)
	}
	got := pm.renderPreview(previewW)
	if got == first {
		t.Error("preview did not change when the close cursor moved")
	}
	// The cursor now sits on the pane close, so the map must dash %2's box —
	// the pane that died — and leave %1's alone.
	dead, alive := boxLine(got, "1 nvim"), boxLine(got, "0 fish")
	if !strings.ContainsRune(dead, '\u2506') {
		t.Errorf("the closed pane %%2 is not marked dead:\n%s", got)
	}
	if strings.ContainsRune(alive, '\u2506') {
		t.Errorf("the surviving pane %%1 is marked dead:\n%s", got)
	}
}

// boxLine returns the rendered map line carrying label, where the pane box's
// own side borders show whether that pane is marked dead.
func boxLine(render, label string) string {
	for _, l := range strings.Split(stripANSI(render), "\n") {
		if strings.Contains(l, label) {
			return l
		}
	}
	return ""
}

// nestedClosePreviewModel builds a close-mode picker whose sub-manifest carries
// a real two-pane layout, so the preview has a map to draw.
func nestedClosePreviewModel(t *testing.T) PickerModel {
	t.Helper()
	man := snapshot.Manifest{Sessions: []snapshot.Session{{
		Name: "lazytmux",
		Windows: []snapshot.Window{{
			Index:  3,
			Name:   "docs",
			Layout: "a1b2,80x24,0,0[80x12,0,0,1,80x11,0,13,2]",
			Panes: []snapshot.Pane{
				{Index: 0, Command: "fish", ID: "%1"},
				{Index: 1, Command: "nvim", ID: "%2"},
			},
		}},
	}}}
	evs := []store.Event{
		{ID: 1, Ts: 300, Kind: "window-unlinked"},
		{ID: 2, Ts: 200, Kind: "pane-died"},
	}
	ctxs := map[int64]CloseContext{
		1: {Label: "w", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "window", PaneCount: 2}, SubManifest: man},
		2: {Label: "pane: nvim", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane", PaneID: "%2"}, SubManifest: man},
	}
	m := NewPickerModel(ModeClose, evs, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(BuildCloseTree(evs, ctxs, "mono", map[string]bool{}))
	m.Bootstrap()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return updated.(PickerModel)
}

// prefix+U opens straight onto the preview, so the cursor's scrollback has to
// be scheduled before the first key — otherwise the panel shows the window map
// and silently swaps to scrollback once the user nudges the cursor.
func TestPickerModel_CloseModeSchedulesScrollbackAtStartup(t *testing.T) {
	m := closeScrollbackModel(t, closeScrollbackSHA)
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init scheduled no scrollback load for the close cursor's pane")
	}
	if !m.loadingSHAs[closeScrollbackSHA] {
		t.Errorf("Init did not mark %q in flight", closeScrollbackSHA)
	}
	_, _, previewW := m.paneWidthsThree()
	if got := m.renderPreview(previewW); !strings.Contains(got, "loading scrollback") {
		t.Errorf("first frame does not say the scrollback is on its way:\n%s", got)
	}
}

// A pane close whose scrollback is neither cached nor in flight must say so.
// Falling through to the window map instead means the panel silently changes
// content type the moment a load lands.
func TestRenderPreview_CloseModePendingScrollback(t *testing.T) {
	m := closeScrollbackModel(t, closeScrollbackSHA)
	_, _, previewW := m.paneWidthsThree()
	if got := m.renderPreview(previewW); !strings.Contains(got, "scrollback pending") {
		t.Errorf("an unscheduled scrollback does not read as pending:\n%s", got)
	}
}

// Tab has no second tree to reach in close mode: the preview already follows
// the list cursor, so the key must leave both focus and the panel alone.
func TestPickerModel_CloseModeTabIsInert(t *testing.T) {
	m := nestedClosePreviewModel(t)
	_, _, previewW := m.paneWidthsThree()
	before := m.renderPreview(previewW)

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	pm := updated.(PickerModel)
	if pm.focus != focusList {
		t.Errorf("Tab moved focus to %v; close mode has no second tree to reach", pm.focus)
	}
	if got := pm.renderPreview(previewW); got != before {
		t.Errorf("Tab changed the close preview:\n%s", got)
	}
}

// Alt+j/k scroll the close preview without a focus change — the reason Tab
// could be retired there.
func TestPickerModel_CloseModeAltScrollsPreview(t *testing.T) {
	m := closeScrollbackModel(t, closeScrollbackSHA)
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	m.scrollbacks[closeScrollbackSHA] = []byte(b.String())

	_, _, previewW := m.paneWidthsThree()
	maxScroll := m.previewMaxScroll(m.previewInnerHeight())
	if maxScroll == 0 {
		t.Fatal("fixture scrollback is not taller than the preview panel")
	}
	tail := m.renderPreview(previewW)
	if !strings.Contains(tail, "line 200") {
		t.Fatalf("close preview does not open at the tail:\n%s", tail)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModAlt})
	m = updated.(PickerModel)
	if m.previewScroll != 1 {
		t.Fatalf("alt+k left previewScroll at %d, want 1", m.previewScroll)
	}
	if got := m.renderPreview(previewW); got == tail {
		t.Error("alt+k did not change the rendered close preview")
	}

	for i := 0; i < maxScroll+50; i++ {
		updated, _ = m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModAlt})
		m = updated.(PickerModel)
	}
	if m.previewScroll != maxScroll {
		t.Errorf("alt+k ran past the top of the buffer: previewScroll %d, want %d", m.previewScroll, maxScroll)
	}
	if got := m.renderPreview(previewW); !strings.Contains(got, "line 1 ") && !strings.Contains(got, "line 1\n") {
		t.Errorf("scrolled to the top but the first line is not on screen:\n%s", got)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModAlt})
	m = updated.(PickerModel)
	if m.previewScroll != maxScroll-1 {
		t.Errorf("alt+j left previewScroll at %d, want %d", m.previewScroll, maxScroll-1)
	}
}

const closeScrollbackSHA = "cafebabe"

// closeScrollbackModel builds a close-mode picker sitting on a pane close whose
// pane carries sha, so the preview resolves to scrollback rather than the map.
func closeScrollbackModel(t *testing.T, sha string) PickerModel {
	t.Helper()
	sub := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index: 1, Name: "tmux-remux",
				Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
				Panes: []snapshot.Pane{
					{Index: 0, ID: "%0", Command: "fish"},
					{Index: 1, ID: "%1", Command: "nvim", ScrollbackSHA: sha},
				},
			}},
		}},
	}
	ev := store.Event{ID: 7, Kind: "pane-died", ManifestJSON: `{"pane_id":"%1"}`}
	ctxs := map[int64]CloseContext{7: {
		Label:       "pane: nvim",
		Placement:   ClosePlacement{Session: "demo", WindowIndex: 1, Scope: "pane", PaneID: "%1"},
		SubManifest: sub,
	}}
	m := NewPickerModel(ModeClose, []store.Event{ev}, nil, scrollback.New(t.TempDir()))
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(BuildCloseTree([]store.Event{ev}, ctxs, "demo", nil))
	m.Bootstrap()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 20})
	return updated.(PickerModel)
}

// A close hook can report a different session name than the prior snapshot
// when the session was renamed in between (see closeevent.OwnerSession's
// three-step fallback). closePreviewWindow must still resolve the window —
// and, for a pane close, its scrollback — rather than treating the mismatch
// as "nothing captured for this close."
func TestClosePreviewWindow_SessionNameMismatchFallsBack(t *testing.T) {
	cc := CloseContext{
		Label:     "pane: nvim",
		Placement: ClosePlacement{Session: "renamed", WindowIndex: 1, WindowName: "docs", Scope: "pane", PaneID: "%1"},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{
			Name: "original",
			Windows: []snapshot.Window{{
				Index: 1, Name: "docs",
				Panes: []snapshot.Pane{{Index: 0, ID: "%1", Command: "nvim", ScrollbackSHA: "aaa"}},
			}},
		}}},
	}
	if w := closePreviewWindow(cc); w == nil {
		t.Fatal("closePreviewWindow returned nil for a session name mismatch; the map/scrollback goes blank")
	}
	if sha := closeSHAFor(cc); sha != "aaa" {
		t.Errorf("closeSHAFor = %q, want %q", sha, "aaa")
	}
}

// Left can collapse a window row and land the cursor on an *event* row above
// it — not just on a pane row it collapsed from. Two pane closes in two
// windows of one other-session group produce that shape: collapsing the
// second window's header walks the cursor up past its own pure-header row to
// the first window's pane close, which must still reset the preview scroll
// and schedule that pane's scrollback.
func TestPickerModel_CloseModeLeftLandsOnEventRowAboveCollapsedWindow(t *testing.T) {
	man := snapshot.Manifest{Sessions: []snapshot.Session{{
		Name: "lazytmux",
		Windows: []snapshot.Window{
			{Index: 1, Name: "alpha", Panes: []snapshot.Pane{{Index: 0, ID: "%1", Command: "fish", ScrollbackSHA: "aaa"}}},
			{Index: 2, Name: "beta", Panes: []snapshot.Pane{{Index: 0, ID: "%2", Command: "nvim"}}},
		},
	}}}
	evs := []store.Event{
		{ID: 1, Ts: 300, Kind: "pane-died"},
		{ID: 2, Ts: 200, Kind: "pane-died"},
	}
	ctxs := map[int64]CloseContext{
		1: {Label: "pane: fish", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 1, WindowName: "alpha", Scope: "pane", PaneID: "%1"}, SubManifest: man},
		2: {Label: "pane: nvim", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 2, WindowName: "beta", Scope: "pane", PaneID: "%2"}, SubManifest: man},
	}
	m := NewPickerModel(ModeClose, evs, nil, scrollback.New(t.TempDir()))
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(BuildCloseTree(evs, ctxs, "mono", nil))
	m.Bootstrap()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(PickerModel)

	vis := m.CloseVisible()
	if len(vis) != 6 {
		t.Fatalf("flattened close tree has %d rows, want 6: %+v", len(vis), vis)
	}
	m.cursor = 5 // beta's pane %2 close
	m.previewScroll = 3

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m = updated.(PickerModel)

	if m.cursor != 3 {
		t.Fatalf("cursor landed on %d, want 3 (alpha's pane %%1 close)", m.cursor)
	}
	if m.previewScroll != 0 {
		t.Errorf("previewScroll = %d, want 0 after Left", m.previewScroll)
	}
	if !m.loadingSHAs["aaa"] {
		t.Error("Left did not schedule the scrollback load for the event row it landed on")
	}
}
