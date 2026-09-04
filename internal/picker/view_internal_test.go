package picker

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

// TestRenderList_NeverOverflowsFrame guards the list-pane box math: a lipgloss
// frame pads short content but does NOT clip overflow, so a wrapped row or
// footer pushes the border past the requested height and desyncs the sibling
// panes. Rendered output must be exactly width×height for every size — even a
// narrow pane with a hidden-count footer pinned to the bottom. Per-row
// truncation of a long label is covered by TestRenderCloseTree_NeverOverflowsFrame
// instead, since renderList's row format no longer reads a close-specific label.
func TestRenderList_NeverOverflowsFrame(t *testing.T) {
	applyTheme(NewTheme())
	m := PickerModel{
		dimOlderThan: 24 * time.Hour,
		events: []store.Event{
			{ID: 1, Ts: time.Now().UnixMilli(), Kind: "window-unlinked"},
			{ID: 2, Ts: time.Now().UnixMilli(), Kind: "pane-died"},
		},
		hiddenCount: 14,
	}
	for _, w := range []int{32, 40, 80, 120} {
		for _, h := range []int{3, 4, 6, 10} {
			out := renderList(m, w, h)
			if got := lipgloss.Height(out); got != h {
				t.Errorf("renderList(w=%d,h=%d): height=%d, want %d\n%s", w, h, got, h, out)
			}
			if got := lipgloss.Width(out); got != w {
				t.Errorf("renderList(w=%d,h=%d): width=%d, want %d", w, h, got, w)
			}
		}
	}
}

// closeTreeFixture builds the tree used by the rendering tests: mono (current)
// with window 2 closed and a pane inside it, plus a gone lazytmux session
// whose window 3 has both its own close and a pane-scoped close nested under
// it — the deepest real shape, session → window → pane under other sessions.
func closeTreeFixture() *CloseNode {
	evs := []store.Event{
		{ID: 1, Ts: 300, Kind: "window-unlinked"},
		{ID: 2, Ts: 200, Kind: "pane-died"},
		{ID: 3, Ts: 100, Kind: "window-unlinked"},
		{ID: 4, Ts: 50, Kind: "pane-died"},
	}
	one := snapshot.Manifest{Sessions: []snapshot.Session{{Name: "mono"}}}
	ctxs := map[int64]CloseContext{
		1: {Label: "w", Placement: ClosePlacement{Session: "mono", WindowIndex: 2, WindowName: "main", Scope: "window", PaneCount: 1}, SubManifest: one},
		2: {Label: "pane: nvim", Placement: ClosePlacement{Session: "mono", WindowIndex: 2, WindowName: "main", Scope: "pane"}, SubManifest: one},
		3: {Label: "w", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "window", PaneCount: 1}, SubManifest: one},
		4: {Label: "pane: fish", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane"}, SubManifest: one},
	}
	return BuildCloseTree(evs, ctxs, "mono", map[string]bool{"mono": true})
}

// Under the three-column threshold the panel stacks rather than vanishing.
func TestView_NarrowStacksPanel(t *testing.T) {
	m := PickerModel{mode: ModeSnapshot, width: 90, height: 24}
	if !m.stacksPanel() {
		t.Error("want a stacked panel at 90 columns")
	}
	wide := PickerModel{mode: ModeSnapshot, width: 160, height: 24}
	if wide.stacksPanel() {
		t.Error("want three columns at 160 columns")
	}
}

// The close picker shares renderPreview with the snapshot picker, so
// narrow-width stacking must cover ModeClose too: at 100 columns (the
// stacksPanel range) the panel goes below the tree rather than being dropped.
// Selects the window node so renderPreview draws the pane-layout map.
func TestView_CloseModeStacksPanel(t *testing.T) {
	applyTheme(NewTheme())
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
	m.width, m.height = 100, 30
	m.focus = focusTree
	m.treeCursor = windowNodeIndex(t, m)

	if !m.stacksPanel() {
		t.Fatal("want stacksPanel() true at 100 columns")
	}
	panel := m.renderPreview(m.width)
	if !strings.ContainsRune(panel, '┌') {
		t.Errorf("panel not rendered in close mode at 100 columns:\n%s", panel)
	}
	if out := m.View().Content; !strings.ContainsRune(out, '┌') {
		t.Errorf("View() dropped the stacked panel in close mode at 100 columns:\n%s", out)
	}
}

func TestCloseGuidePrefixes(t *testing.T) {
	root := closeTreeFixture()
	// Expand the other-sessions group so its subtree is visible.
	for _, g := range root.Children {
		g.Expanded = true
		for _, c := range g.Children {
			c.Expanded = true
		}
	}
	want := map[string]string{
		"this session · mono": "",
		"2: main (1p)":        "└─ ",
		"pane: nvim":          "   └─ ",
		"other sessions":      "",
		"lazytmux":            "└─ ",
		"3: docs (1p)":        "   └─ ",
		"pane: fish":          "      └─ ",
	}
	for _, n := range FlattenClose(root) {
		exp, ok := want[n.Label]
		if !ok {
			t.Errorf("unexpected row %q", n.Label)
			continue
		}
		if got := closeGuidePrefix(n); got != exp {
			t.Errorf("prefix for %q = %q, want %q", n.Label, got, exp)
		}
	}
}

// The guide prefix is part of the row, so it must be inside the truncation
// budget: a deep row with a long, wide-glyph label must not widen the frame.
// hiddenCount is also set so the footer-pinning pad loop runs, matching the
// two conditions TestRenderList_NeverOverflowsFrame proves matter.
func TestRenderCloseTree_NeverOverflowsFrame(t *testing.T) {
	applyTheme(NewTheme())
	root := closeTreeFixture()
	for _, g := range root.Children {
		g.Expanded = true
		for _, c := range g.Children {
			c.Expanded = true
			c.Label = "a-really-long-window-name-that-will-not-fit-in-any-narrow-pane 🧠 (3p)"
		}
	}
	m := PickerModel{mode: ModeClose, closeTree: root, hiddenCount: 14}
	for _, size := range []struct{ w, h int }{{32, 8}, {40, 6}, {80, 12}, {28, 5}} {
		m.width, m.height = size.w, size.h
		out := renderCloseTree(m, size.w, size.h)
		if got := lipgloss.Width(out); got != size.w {
			t.Errorf("width %d height %d: rendered width %d, want %d", size.w, size.h, got, size.w)
		}
		if got := lipgloss.Height(out); got != size.h {
			t.Errorf("width %d height %d: rendered height %d, want %d", size.w, size.h, got, size.h)
		}
	}
}

func TestShortReason(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"timer", "timer"},
		{"manual", "manual"},
		{"keybinding", "key"},
		{"hook:after-split-window", "split"},
		{"hook:window-linked", "new window"},
		{"hook:session-created", "new session"},
		{"hook:client-detached", "detach"},
		// Unmapped: keeps its words, loses only the prefix. The old fallback
		// cut this to "hook:pan".
		{"hook:pane-exited", "pane-exited"},
	} {
		if got := shortReason(tc.in); got != tc.want {
			t.Errorf("shortReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// allCloseNodes returns every node in the tree, ignoring expansion state.
// The style and marker rules are properties of a node, not of whether it
// happens to be on screen, so these tests must not go through FlattenClose —
// it stops at a collapsed parent and would silently skip whole row kinds.
func allCloseNodes(root *CloseNode) []*CloseNode {
	var out []*CloseNode
	var walk func(n *CloseNode)
	walk = func(n *CloseNode) {
		out = append(out, n)
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, g := range root.Children {
		walk(g)
	}
	return out
}

// The close tree must never render faint or italic text: the picker used both
// to mean "not a restore target", which reads as "old / unimportant" instead —
// and dimOlderThan already claims that visual channel for age.
func TestCloseRowStyle_NeverFaintOrItalic(t *testing.T) {
	applyTheme(Theme{})
	for _, n := range allCloseNodes(closeTreeFixture()) {
		s := closeRowStyle(n)
		if s.GetFaint() {
			t.Errorf("%q: style is faint", n.Label)
		}
		if s.GetItalic() {
			t.Errorf("%q: style is italic", n.Label)
		}
	}
}

// Scaffolding must still be visually separable from a restorable row — just by
// a different foreground, not by dimming.
func TestCloseRowStyle_ScaffoldingDiffersFromEventRow(t *testing.T) {
	applyTheme(Theme{})
	var event, scaffold *CloseNode
	for _, n := range allCloseNodes(closeTreeFixture()) {
		if IsCloseGroup(n) {
			continue
		}
		if n.EventID != 0 && event == nil {
			event = n
		}
		if n.EventID == 0 && scaffold == nil {
			scaffold = n
		}
	}
	if event == nil || scaffold == nil {
		t.Fatalf("fixture lacks both row kinds: event=%v scaffold=%v", event, scaffold)
	}
	if closeRowStyle(event).GetForeground() == closeRowStyle(scaffold).GetForeground() {
		t.Error("event and scaffolding rows share a foreground colour")
	}
}

// The marker is the load-bearing "Enter works here" cue, so it must appear on
// exactly the rows that carry an event id.
func TestCloseRow_MarksOnlyRestorableRows(t *testing.T) {
	applyTheme(Theme{})
	for _, n := range allCloseNodes(closeTreeFixture()) {
		if IsCloseGroup(n) {
			continue
		}
		got := strings.Contains(closeRow(n, 60, false), closeMarker)
		if want := n.EventID != 0; got != want {
			t.Errorf("%q: marker present=%v, want %v", n.Label, got, want)
		}
	}
}

// State is a tag, not a suffix: " · live" reads as part of the window name.
func TestCloseRow_StateRendersAsAParenthesisedTag(t *testing.T) {
	applyTheme(Theme{})
	for _, n := range allCloseNodes(closeTreeFixture()) {
		if n.State == "" {
			continue
		}
		row := closeRow(n, 60, false)
		if !strings.Contains(row, "("+n.State+")") {
			t.Errorf("%q: want a (%s) tag, got %q", n.Label, n.State, row)
		}
		if strings.Contains(row, " · "+n.State) {
			t.Errorf("%q: still renders the old ' · %s' suffix", n.Label, n.State)
		}
	}
}

func TestRestoreSentence(t *testing.T) {
	man := snapshot.Manifest{Sessions: []snapshot.Session{{
		Name:    "lazytmux",
		Windows: []snapshot.Window{{Index: 3, Name: "docs"}, {Index: 4, Name: "src"}},
	}}}
	now := time.UnixMilli(1_000_000_000)
	ts := now.Add(-22 * time.Minute).UnixMilli()

	cases := []struct {
		name  string
		place ClosePlacement
		want  []string
	}{
		{
			name:  "window",
			place: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "window", PaneCount: 2},
			want: []string{
				"↵ reopens window docs",
				"  in lazytmux at index 3",
				"2 panes · closed 22m ago",
			},
		},
		{
			name:  "pane",
			place: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane"},
			want: []string{
				"↵ reopens a pane in lazytmux:3",
				"closed 22m ago",
			},
		},
		{
			name:  "session",
			place: ClosePlacement{Session: "lazytmux", Scope: "session"},
			want: []string{
				"↵ reopens session lazytmux (2 windows)",
				"closed 22m ago",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreSentence(tc.place, man, now, ts)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{22 * time.Minute, "22m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		if got := humanAge(tc.in); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
