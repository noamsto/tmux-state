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
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

// closePreviewWidth returns the width View hands the close preview: the
// preview column when it sits beside the list, the whole terminal when the
// list is too narrow for both and it stacks underneath.
func closePreviewWidth(m PickerModel) int {
	if _, _, w := m.paneWidthsThree(); w > 0 {
		return w
	}
	return m.width
}

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

func TestRenderPreview_ScrollbackSkipped(t *testing.T) {
	man := snapshot.Manifest{V: 1, ScrollbackSkipped: true, Sessions: []snapshot.Session{{
		Windows: []snapshot.Window{{Panes: []snapshot.Pane{{}}}},
	}}}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 1, Kind: "snapshot", ManifestJSON: string(raw)}
	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)

	got := m.renderPreview(60)
	if !strings.Contains(stripANSI(got), "min_save_interval") {
		t.Errorf("expected throttle explanation, got: %q", got)
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

// TestRenderPreview_NeverOverflowsFrameWithWideRunesScrolled is the snapshot
// mode counterpart of TestRenderClosePreview_NeverOverflowsFrameWithWideRunesScrolled:
// renderPreview's plain-pane path calls the same previewWindow, so it shares
// the same ansi.Cut overshoot and the same dropped closing border once
// previewScrollX > 0 lands a double-width rune on a cut boundary. Nothing
// about the snapshot path makes it immune — it is just less likely to be hit
// by hand, since it additionally requires focusing a pane node in the tree.
func TestRenderPreview_NeverOverflowsFrameWithWideRunesScrolled(t *testing.T) {
	applyTheme(NewTheme())
	man := snapshot.Manifest{V: 1, Sessions: []snapshot.Session{{
		Windows: []snapshot.Window{{Panes: []snapshot.Pane{{ScrollbackSHA: "abc"}}}},
	}}}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 1, Kind: "snapshot", ManifestJSON: string(raw)}
	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.focus = focusTree
	m.treeCursor = paneNodeIndex(t, m)

	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "行%d 日本語プロジェクトの内容漢字文字列テスト絵文字📁📂🎉 line %d with tail text long enough to overflow a narrow panel\n", i, i)
	}
	m.scrollbacks["abc"] = []byte(b.String())

	widths := []int{60, 80, 86, 100, 120}
	heights := []int{12, 20, 30}
	for _, w := range widths {
		for _, h := range heights {
			for scrollX := 1; scrollX <= 40; scrollX++ {
				m.width, m.height = w, h
				m.previewScrollX = scrollX
				out := m.renderPreview(w)
				if got := lipgloss.Height(out); got != m.panelFrameHeight() {
					t.Errorf("w=%d h=%d scrollX=%d: rendered height=%d, want %d", w, h, scrollX, got, m.panelFrameHeight())
				}
				if got := lipgloss.Width(out); got != w {
					t.Errorf("w=%d h=%d scrollX=%d: rendered width=%d, want %d", w, h, scrollX, got, w)
				}
				rows := strings.Split(out, "\n")
				last := rows[len(rows)-1]
				if !strings.ContainsRune(last, '╰') || !strings.ContainsRune(last, '╯') {
					t.Errorf("w=%d h=%d scrollX=%d: frame did not close, last row is %q\n%s", w, h, scrollX, last, out)
				}
			}
		}
	}
}

// stripANSI removes ANSI escapes for assertion ergonomics.
func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

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

	// A real close list, because in close mode the cursor indexes its rows
	// rather than the event slice — the path production actually takes.
	ctxs := map[int64]CloseContext{7: {
		Label:       "pane",
		Placement:   ClosePlacement{Session: "demo", WindowIndex: 1, Scope: "pane", PaneID: "%1"},
		SubManifest: sub,
	}}
	m := NewPickerModel(ModeClose, []store.Event{ev}, nil, scrollback.New(t.TempDir()))
	m.SetCloseContexts(ctxs)
	m.SetCloseRows(BuildCloseList([]store.Event{ev}, ctxs, "demo"))
	m.Bootstrap()
	m.width, m.height = 130, 20
	m.scrollbacks[sha] = []byte("step 34: reading files…")

	previewWidth := closePreviewWidth(m)
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

// A pane close's preview is about the pane that died: its block names it, and
// the siblings that survived it are nowhere in the panel.
func TestPickerModel_ClosePreviewShowsOnlyTheDiedPane(t *testing.T) {
	sub := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
				// What closeevent.SubManifest now hands the picker for a pane
				// close: the enclosing window, carrying only the dead pane.
				Panes: []snapshot.Pane{{Index: 1, ID: "%1", Command: "agent-work"}},
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
	m.SetCloseRows(BuildCloseList([]store.Event{ev}, ctxs, "demo"))
	m.Bootstrap()
	m.width, m.height = 160, 24

	previewWidth := closePreviewWidth(m)
	got := stripANSI(m.renderPreview(previewWidth))
	if !strings.Contains(got, "1 · agent-work") || !strings.Contains(got, "1 of 1") {
		t.Errorf("the dead pane has no block of its own:\n%s", got)
	}
	if strings.Contains(got, "fish") {
		t.Errorf("a surviving sibling leaked into the close preview:\n%s", got)
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
// live from the first frame, following the close list's cursor.
func TestRenderPreview_CloseModeNeedsNoTab(t *testing.T) {
	m := nestedClosePreviewModel(t)
	previewW := closePreviewWidth(m)
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
	previewW := closePreviewWidth(m)
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
	// The cursor now sits on the pane close, whose sub-manifest carries only
	// %2 — so the panel shows nvim's block and not fish's.
	plain := stripANSI(got)
	if !strings.Contains(plain, "pane close") || !strings.Contains(plain, "1 · nvim") {
		t.Errorf("the preview did not follow the cursor onto the pane close:\n%s", plain)
	}
	if strings.Contains(plain, "fish") {
		t.Errorf("the surviving pane leaked into the pane close's preview:\n%s", plain)
	}
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
	paneSub := snapshot.Manifest{Sessions: []snapshot.Session{{
		Name: "lazytmux",
		Windows: []snapshot.Window{{
			Index: 3, Name: "docs", Layout: man.Sessions[0].Windows[0].Layout,
			Panes: []snapshot.Pane{{Index: 1, Command: "nvim", ID: "%2"}},
		}},
	}}}
	ctxs := map[int64]CloseContext{
		1: {Label: "w", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "window", PaneCount: 2}, SubManifest: man},
		2: {Label: "pane: nvim", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane", PaneID: "%2"}, SubManifest: paneSub},
	}
	m := NewPickerModel(ModeClose, evs, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseRows(BuildCloseList(evs, ctxs, "mono"))
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
	previewW := closePreviewWidth(m)
	if got := m.renderPreview(previewW); !strings.Contains(got, "loading scrollback") {
		t.Errorf("first frame does not say the scrollback is on its way:\n%s", got)
	}
}

// A pane close whose scrollback is neither cached nor in flight must say so.
// Falling through to the window map instead means the panel silently changes
// content type the moment a load lands.
func TestRenderPreview_CloseModePendingScrollback(t *testing.T) {
	m := closeScrollbackModel(t, closeScrollbackSHA)
	previewW := closePreviewWidth(m)
	if got := m.renderPreview(previewW); !strings.Contains(got, "scrollback pending") {
		t.Errorf("an unscheduled scrollback does not read as pending:\n%s", got)
	}
}

// Tab has no second tree to reach in close mode: the preview already follows
// the list cursor, so the key must leave both focus and the panel alone.
func TestPickerModel_CloseModeTabIsInert(t *testing.T) {
	m := nestedClosePreviewModel(t)
	previewW := closePreviewWidth(m)
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

	previewW := closePreviewWidth(m)
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
	m.SetCloseRows(BuildCloseList([]store.Event{ev}, ctxs, "demo"))
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
	if sha := closePreviewWindow(cc).Panes[0].ScrollbackSHA; sha != "aaa" {
		t.Errorf("resolved scrollback = %q, want %q", sha, "aaa")
	}
}

// The rename-tolerance fallback drops the session-name filter entirely, which
// is only safe when the sub-manifest holds exactly one session — with more
// than one, "match nothing" can pick a window that belongs to a session
// other than the one that closed. A session-scope close whose own name isn't
// in the sub-manifest at all (not reachable in production today: each close
// event carries its own single-session sub-manifest) must not fall back to
// drawing an unrelated session's window under its restore sentence.
func TestClosePreviewWindow_MultiSessionSubManifestDoesNotFallBack(t *testing.T) {
	cc := CloseContext{
		Label:     "session: agentdetect-test-2612860",
		Placement: ClosePlacement{Session: "agentdetect-test-2612860", Scope: "session"},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{
			{Name: "dispatcher", Windows: []snapshot.Window{{Index: 2, Name: "bump-model-map"}}},
			{Name: "other", Windows: []snapshot.Window{{Index: 0, Name: "fish"}}},
		}},
	}
	if w := closePreviewWindow(cc); w != nil {
		t.Errorf("closePreviewWindow = %+v, want nil: a multi-session sub-manifest must not fall back to an unrelated session's window", w)
	}
}

// Moving the cursor to another close must reset the preview scroll and
// schedule the scrollback of the pane it lands on — otherwise the new close's
// block opens mid-buffer, or hangs on "(loading scrollback…)" forever.
func TestPickerModel_CloseModeCursorMoveResetsScrollAndLoads(t *testing.T) {
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
	m.SetCloseRows(BuildCloseList(evs, ctxs, "mono"))
	m.Bootstrap()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(PickerModel)

	// Row 0 is the OTHER SESSIONS header; the cursor opens on alpha's close.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(PickerModel)
	if m.cursor != 2 {
		t.Fatalf("cursor landed on %d, want 2 (beta's pane %%2 close)", m.cursor)
	}
	m.previewScroll = 3
	delete(m.loadingSHAs, "aaa") // scheduled by Bootstrap; watch this move re-schedule it

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(PickerModel)

	if m.cursor != 1 {
		t.Fatalf("cursor landed on %d, want 1 (alpha's pane %%1 close)", m.cursor)
	}
	if m.previewScroll != 0 {
		t.Errorf("previewScroll = %d, want 0 after moving to another close", m.previewScroll)
	}
	if !m.loadingSHAs["aaa"] {
		t.Error("the move did not schedule the scrollback load for the close it landed on")
	}
}

// closePreviewFrameFixture builds a single-close ModeClose model whose body is
// controlled by scope: "window" stacks two pane blocks, "one" fills the panel
// with a single one, "pane" is the narrowed single-pane sub-manifest a pane
// close now produces, and "missing" strips the session's windows so
// closePreviewWindow finds nothing and the body takes its "nothing captured"
// branch. The session itself is kept (a zero-session sub-manifest makes
// BuildCloseList drop the row entirely, per its "nothing to restore" rule —
// a different case than this one). Every pane carries far more
// scrollback than any frame here can hold, and the session/window name is long
// enough to need truncation, so both axes of the box math are under load.
func closePreviewFrameFixture(t *testing.T, scope string) PickerModel {
	t.Helper()
	longName := "a-really-long-window-name-that-will-not-fit-in-any-narrow-panel-at-all"
	sub := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo-" + longName,
			Windows: []snapshot.Window{{
				Index:  1,
				Name:   longName,
				Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
				Panes: []snapshot.Pane{
					{Index: 0, ID: "%0", Command: "fish", ScrollbackSHA: "sha0"},
					{Index: 1, ID: "%1", Command: "agent-work", ScrollbackSHA: "sha1"},
				},
			}},
		}},
	}
	placementScope := scope
	switch scope {
	case "missing":
		placementScope = "window"
		sub.Sessions[0].Windows = nil
	case "one", "pane":
		placementScope = "pane"
		sub.Sessions[0].Windows[0].Panes = sub.Sessions[0].Windows[0].Panes[:1]
	}
	ev := store.Event{ID: 1, Ts: time.Now().UnixMilli(), Kind: "window-unlinked"}
	ctxs := map[int64]CloseContext{1: {
		Label: "w",
		Placement: ClosePlacement{
			Session: "demo-" + longName, WindowIndex: 1, WindowName: longName,
			Scope: placementScope, PaneCount: 2,
		},
		SubManifest: sub,
	}}
	m := NewPickerModel(ModeClose, []store.Event{ev}, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseRows(BuildCloseList([]store.Event{ev}, ctxs, "demo-"+longName))
	m.Bootstrap()
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "scrollback line %d with enough text to overflow a narrow panel\n", i)
	}
	m.scrollbacks["sha0"] = []byte(b.String())
	m.scrollbacks["sha1"] = []byte(b.String())
	return m
}

// TestRenderClosePreview_NeverOverflowsFrame guards renderClosePreview's box
// math the same way TestRenderList/TestRenderCloseTree guard theirs: a
// lipgloss frame pads short content but does not clip overflow, so a body
// with more lines than previewInnerHeight() pushes the closing border past the
// requested height. MaxHeight then hard-truncates the *line list*, so the
// bottom border is what gets dropped and a plain height assertion cannot see
// it — the corners are asserted too. Every fixture's panes carry more
// scrollback than the panel can hold, so each block is always full.
//
// bodyHeight's floor of 5 puts previewInnerHeight()'s reachable minimum at 3,
// where the two-line header leaves the body a single row.
func TestRenderClosePreview_NeverOverflowsFrame(t *testing.T) {
	applyTheme(NewTheme())
	sizes := []struct{ w, h int }{
		{90, 6},   // bodyHeight floor 5 -> previewInnerHeight 3: body budget 1
		{90, 7},   // previewInnerHeight 4: body budget 2 -> one block, one content row
		{90, 8},   // previewInnerHeight 5
		{90, 10},  // previewInnerHeight 7
		{90, 12},  // previewInnerHeight 9
		{90, 30},  // comfortable
		{40, 20},  // narrow: below the two-column threshold
		{160, 30}, // wide and comfortable
	}
	for _, scope := range []string{"window", "one", "pane", "missing"} {
		m := closePreviewFrameFixture(t, scope)
		for _, sz := range sizes {
			m.width, m.height = sz.w, sz.h
			out := m.renderClosePreview(sz.w)
			wantH := m.panelFrameHeight()
			if got := lipgloss.Height(out); got != wantH {
				t.Errorf("scope=%s w=%d h=%d: rendered height=%d, want %d\n%s", scope, sz.w, sz.h, got, wantH, out)
			}
			if got := lipgloss.Width(out); got != sz.w {
				t.Errorf("scope=%s w=%d h=%d: rendered width=%d, want %d", scope, sz.w, sz.h, got, sz.w)
			}
			// MaxHeight truncates the *line list*, not the overflow: when the
			// body already has more lines than the frame's interior, the
			// bottom border row is the one that gets cut. Assert it survives.
			rows := strings.Split(out, "\n")
			last := rows[len(rows)-1]
			if !strings.ContainsRune(last, '╰') || !strings.ContainsRune(last, '╯') {
				t.Errorf("scope=%s w=%d h=%d: frame did not close, last row is %q\n%s", scope, sz.w, sz.h, last, out)
			}
		}
	}
}

// closePreviewWideRuneFixture is closePreviewFrameFixture's "window" scope
// with scrollback stuffed with CJK and emoji runes in place of plain ASCII —
// a stand-in for a real editor/agent pane, and the shape previewWindow's
// horizontal-scroll path needs to overshoot its budget.
func closePreviewWideRuneFixture(t *testing.T) PickerModel {
	t.Helper()
	m := closePreviewFrameFixture(t, "window")
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "行%d 日本語プロジェクトの内容漢字文字列テスト絵文字📁📂🎉 line %d with tail text long enough to overflow a narrow panel\n", i, i)
	}
	m.scrollbacks["sha0"] = []byte(b.String())
	m.scrollbacks["sha1"] = []byte(b.String())
	return m
}

// TestRenderClosePreview_NeverOverflowsFrameWithWideRunesScrolled guards the
// same frame-closing invariant as TestRenderClosePreview_NeverOverflowsFrame,
// through the one path that test's ASCII fixture can't reach: previewWindow
// calls ansi.Cut instead of ansi.Truncate whenever previewScrollX > 0 (i.e.
// after Alt+L), and ansi.Cut overshoots its budget by one cell when a
// double-width rune straddles either boundary of the cut. The resulting
// over-wide line wraps inside the lipgloss frame, and MaxHeight's
// hard-truncation of the rendered line list drops the closing border row —
// invisible to a height assertion, since the reported height stays exact.
// Reachable in production with CJK/emoji scrollback and any non-zero
// previewScrollX.
func TestRenderClosePreview_NeverOverflowsFrameWithWideRunesScrolled(t *testing.T) {
	applyTheme(NewTheme())
	m := closePreviewWideRuneFixture(t)
	widths := []int{60, 80, 86, 100, 120}
	heights := []int{12, 20, 30}
	for _, w := range widths {
		for _, h := range heights {
			for scrollX := 1; scrollX <= 40; scrollX++ {
				m.width, m.height = w, h
				m.previewScrollX = scrollX
				out := m.renderClosePreview(w)
				if got := lipgloss.Height(out); got != m.panelFrameHeight() {
					t.Errorf("w=%d h=%d scrollX=%d: rendered height=%d, want %d", w, h, scrollX, got, m.panelFrameHeight())
				}
				if got := lipgloss.Width(out); got != w {
					t.Errorf("w=%d h=%d scrollX=%d: rendered width=%d, want %d", w, h, scrollX, got, w)
				}
				rows := strings.Split(out, "\n")
				last := rows[len(rows)-1]
				if !strings.ContainsRune(last, '╰') || !strings.ContainsRune(last, '╯') {
					t.Errorf("w=%d h=%d scrollX=%d: frame did not close, last row is %q\n%s", w, h, scrollX, last, out)
				}
			}
		}
	}
}

// A close whose sub-manifest holds no window has no scrollback to stack, but
// the header still has to name what closed — the panel is otherwise blank
// exactly where the user has least other context.
func TestRenderClosePreview_NothingCapturedStillNamesTheClose(t *testing.T) {
	applyTheme(NewTheme())
	m := closePreviewFrameFixture(t, "missing")
	m.width, m.height = 90, 30
	out := stripANSI(m.renderClosePreview(90))
	if !strings.Contains(out, "nothing captured for this close") {
		t.Errorf("missing the placeholder line entirely:\n%s", out)
	}
	if !strings.Contains(out, "window close") {
		t.Errorf("renderClosePreview did not name the close when nothing was captured:\n%s", out)
	}
}

// A pane's preview leads with a mini-map of its window when the panel is tall
// enough, the focused pane marked, with scrollback below; a short panel drops
// the map and shows scrollback alone.
func TestPickerModel_PaneViewShowsContextMap(t *testing.T) {
	man := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Name:   "editor",
				Layout: "1cb4,145x36,0,0[145x18,0,0,0,145x17,0,19{72x17,0,19,1,72x17,73,19,2}]",
				Panes: []snapshot.Pane{
					{Index: 1, ID: "%0", Command: "nvim", ScrollbackSHA: "a"},
					{Index: 2, ID: "%1", Command: "agent", ScrollbackSHA: "b"},
					{Index: 3, ID: "%2", Command: "fish", ScrollbackSHA: "c"},
				},
			}},
		}},
	}
	raw, _ := json.Marshal(man)
	ev := store.Event{ID: 7, Kind: "snapshot", ManifestJSON: string(raw)}
	m := NewPickerModel(ModeSnapshot, []store.Event{ev}, nil, nil)
	m.Bootstrap()
	m.focus = focusTree
	m.scrollbacks["b"] = []byte("agent output line one\nagent output line two")
	// Focus the agent pane (%1).
	for i, n := range m.VisibleNodes() {
		if p, ok := n.Ref.(*snapshot.Pane); ok && p.ID == "%1" {
			m.treeCursor = i
		}
	}
	m.width, m.height = 160, 40
	_, _, pw := m.paneWidthsThree()
	if !m.paneHintShows() {
		t.Fatal("hint should show on a tall panel")
	}
	got := stripANSI(m.renderPreview(pw))
	if !strings.ContainsRune(got, '┌') {
		t.Errorf("no mini-map art:\n%s", got)
	}
	if !strings.Contains(got, "▸2 agent") {
		t.Errorf("focused pane not marked in the map:\n%s", got)
	}
	if !strings.Contains(got, "agent output line one") {
		t.Errorf("scrollback missing below the map:\n%s", got)
	}

	// A short panel drops the map and shows scrollback alone.
	m.width, m.height = 160, 12
	if m.paneHintShows() {
		t.Error("hint should not show on a short panel")
	}
	if short := stripANSI(m.renderPreview(pw)); strings.ContainsRune(short, '┌') {
		t.Errorf("short panel should not draw the map:\n%s", short)
	}
}

// closeStackFixture builds a close-mode picker sitting on a window close whose
// window holds len(cmds) panes. Each pane gets a scrollback SHA only where
// bodies[i] is non-empty; the rest carry none, so the block has to say so.
func closeStackFixture(t *testing.T, cmds []string, bodies []string) PickerModel {
	t.Helper()
	applyTheme(NewTheme())
	panes := make([]snapshot.Pane, len(cmds))
	for i, c := range cmds {
		panes[i] = snapshot.Pane{Index: i, ID: fmt.Sprintf("%%%d", i), Command: c}
		if bodies[i] != "" {
			panes[i].ScrollbackSHA = fmt.Sprintf("sha%d", i)
		}
	}
	sub := snapshot.Manifest{V: 1, Sessions: []snapshot.Session{{
		Name: "halo-nix-amd-ai",
		Windows: []snapshot.Window{{
			Index: 1, Name: "nix-amd-ai", ID: "@1",
			Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
			Panes:  panes,
		}},
	}}}
	ev := store.Event{ID: 7, Ts: time.Now().Add(-48 * time.Hour).UnixMilli(), Kind: "window-unlinked"}
	ctxs := map[int64]CloseContext{7: {
		Label: "w",
		Placement: ClosePlacement{
			Session: "halo-nix-amd-ai", WindowIndex: 1, WindowName: "nix-amd-ai",
			Scope: "window", PaneCount: len(cmds),
		},
		SubManifest: sub,
	}}
	m := NewPickerModel(ModeClose, []store.Event{ev}, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseRows(BuildCloseList([]store.Event{ev}, ctxs, "halo-nix-amd-ai"))
	m.Bootstrap()
	m.width, m.height = 120, 30
	for i, b := range bodies {
		if b != "" {
			m.scrollbacks[fmt.Sprintf("sha%d", i)] = []byte(b)
		}
	}
	return m
}

// The header names the close and when it happened, and the pane map is gone:
// close mode shows output, not geometry.
func TestRenderClosePreview_HeaderNamesTheClose(t *testing.T) {
	m := closeStackFixture(t, []string{"claude", "fish"}, []string{"alpha output", "beta output"})
	got := stripANSI(m.renderClosePreview(120))
	for _, want := range []string{"halo-nix-amd-ai:1 · nix-amd-ai", "window close", "2 panes", "closed 2d ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '┌') {
		t.Errorf("close preview still draws the pane map:\n%s", got)
	}
	if strings.Contains(got, "↵ reopens") {
		t.Errorf("close preview still carries the reopens sentence:\n%s", got)
	}
}

// One pane's block fills the panel: its label bar sits directly under the
// header and its scrollback runs to the bottom of the frame.
func TestRenderClosePreview_OnePaneFillsThePanel(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	m := closeStackFixture(t, []string{"claude"}, []string{b.String()})
	got := stripANSI(m.renderClosePreview(120))
	if !strings.Contains(got, "0 · claude") {
		t.Errorf("block is not labelled with its pane:\n%s", got)
	}
	if !strings.Contains(got, "1 of 1") {
		t.Errorf("block carries no position marker:\n%s", got)
	}
	if !strings.Contains(got, "line 60") {
		t.Errorf("block does not show the tail of the scrollback:\n%s", got)
	}
	// The panel's interior is header (2) + label (1) + content; one pane means
	// the content takes every remaining row.
	body := m.previewInnerHeight() - 3
	if !strings.Contains(got, fmt.Sprintf("line %d", 60-body+1)) {
		t.Errorf("one pane does not fill the panel (want %d content rows):\n%s", body, got)
	}
}

// Two panes stack, each under its own label bar, each showing its own output.
func TestRenderClosePreview_TwoPanesStack(t *testing.T) {
	m := closeStackFixture(t, []string{"claude", "fish"}, []string{"alpha output", "beta output"})
	got := stripANSI(m.renderClosePreview(120))
	for _, want := range []string{"0 · claude", "1 of 2", "1 · fish", "2 of 2", "alpha output", "beta output"} {
		if !strings.Contains(got, want) {
			t.Errorf("stacked preview missing %q:\n%s", want, got)
		}
	}
	// The second block's label must sit below the first block's content, not
	// beside it.
	lines := strings.Split(got, "\n")
	first, alpha, second := indexOfLine(lines, "0 · claude"), indexOfLine(lines, "alpha output"), indexOfLine(lines, "1 · fish")
	if first >= alpha || alpha >= second {
		t.Errorf("blocks are not stacked: label %d, content %d, next label %d\n%s", first, alpha, second, got)
	}
}

func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if strings.Contains(l, want) {
			return i
		}
	}
	return -1
}

// A pane the close took down but whose scrollback was never captured says so
// in its own slot rather than leaving the block blank.
func TestRenderClosePreview_PaneWithoutScrollbackSaysSo(t *testing.T) {
	m := closeStackFixture(t, []string{"claude", "fish"}, []string{"alpha output", ""})
	got := stripANSI(m.renderClosePreview(120))
	lines := strings.Split(got, "\n")
	label := indexOfLine(lines, "1 · fish")
	note := indexOfLine(lines, "no scrollback captured")
	if label < 0 || note < 0 {
		t.Fatalf("second block does not report its missing scrollback:\n%s", got)
	}
	if note != label+1 {
		t.Errorf("the note is at row %d, want directly under the label at %d:\n%s", note, label, got)
	}
}

// The rail is only trustworthy if content cannot reach it: scrollback is full
// of its own box-drawing, so a line of rules must still start one column in.
func TestRenderClosePreview_ContentCannotWriteIntoTheRail(t *testing.T) {
	rules := strings.Repeat(strings.Repeat("─", 200)+"\n", 40)
	m := closeStackFixture(t, []string{"claude", "fish"}, []string{rules, rules})
	got := stripANSI(m.renderClosePreview(120))
	lines := strings.Split(got, "\n")
	// Skip the frame's border rows and its one column of padding.
	seen := 0
	for _, l := range lines[1 : len(lines)-1] {
		r := []rune(l)
		if len(r) < 3 {
			continue
		}
		if r[2] == '─' {
			t.Fatalf("content reached the rail column: %q", l)
		}
		if r[2] == '▌' {
			seen++
		}
	}
	if seen == 0 {
		t.Fatalf("no rail glyph on any content row:\n%s", got)
	}
}

// closePreviewBody is what stands between the stacked body and a dropped
// bottom border, and the frame test can only reach previewInnerHeight() >= 3.
// Drive the budget directly, one and two panes, from nothing to comfortable.
func TestClosePreviewBody_NeverExceedsItsBudget(t *testing.T) {
	applyTheme(NewTheme())
	for _, panes := range []int{1, 2} {
		cmds := []string{"claude", "fish"}[:panes]
		bodies := []string{"alpha", "beta"}[:panes]
		m := closeStackFixture(t, cmds, bodies)
		cc := m.CloseContextFor(7)
		for h := 0; h <= 5; h++ {
			got := m.closePreviewBody(cc, 40, h)
			if len(got) > h {
				t.Errorf("panes=%d budget=%d: body is %d rows\n%s", panes, h, len(got), strings.Join(got, "\n"))
			}
			if h >= 2 && len(got) != h {
				t.Errorf("panes=%d budget=%d: body is %d rows, want the whole budget filled", panes, h, len(got))
			}
			for _, l := range got {
				if w := lipgloss.Width(l); w > 40 {
					t.Errorf("panes=%d budget=%d: row is %d cells wide, want <= 40: %q", panes, h, w, l)
				}
			}
		}
	}
}

// A pane whose scrollback was never captured has no hash to load. Scheduling
// its empty string would hand loadScrollbackCmd a path that cannot exist and
// stamp an error against "" that every later empty-SHA pane would inherit.
func TestPickerModel_CloseModeSkipsPanesWithNoScrollback(t *testing.T) {
	m := closeStackFixture(t, []string{"claude", "fish"}, []string{"alpha", ""})
	if got := m.closeCursorSHAs(); len(got) != 1 || got[0] != "sha0" {
		t.Fatalf("closeCursorSHAs = %q, want just the captured pane's hash", got)
	}

	m.scrollbackStore = scrollback.New(t.TempDir())
	delete(m.scrollbacks, "sha0")
	m.PreviewCmd()
	if m.loadingSHAs[""] {
		t.Error("PreviewCmd scheduled a load for the pane with no scrollback")
	}
}

// Both panes of a two-pane close need their scrollback scheduled: a single
// hash leaves the second block on "(scrollback pending)" forever, since
// nothing else ever schedules it.
func TestPickerModel_CloseModeSchedulesEveryPanesScrollback(t *testing.T) {
	m := closeStackFixture(t, []string{"claude", "fish"}, []string{"alpha", "beta"})
	m.scrollbackStore = scrollback.New(t.TempDir())
	delete(m.scrollbacks, "sha0")
	delete(m.scrollbacks, "sha1")

	if cmd := m.PreviewCmd(); cmd == nil {
		t.Fatal("PreviewCmd scheduled nothing for a two-pane close")
	}
	for _, sha := range []string{"sha0", "sha1"} {
		if !m.loadingSHAs[sha] {
			t.Errorf("PreviewCmd left %q unscheduled; its block can never leave 'pending'", sha)
		}
	}
	if cmd := m.PreviewCmd(); cmd != nil {
		t.Error("PreviewCmd re-scheduled hashes already in flight")
	}
}
