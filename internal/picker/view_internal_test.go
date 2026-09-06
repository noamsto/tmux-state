package picker

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

// TestRenderList_NeverOverflowsFrame guards the list-pane box math: a lipgloss
// frame pads short content but does NOT clip overflow, so a wrapped row or
// footer pushes the border past the requested height — MaxHeight then cuts the
// line list, dropping the closing border rather than the excess — and desyncs
// the sibling panes. Rendered output must be exactly width×height for every size — even a
// narrow pane with a hidden-count footer pinned to the bottom. Per-row
// truncation of a long label is covered by TestRenderCloseList_NeverOverflowsFrame
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
			assertFrameCloses(t, out)
		}
	}
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

// Close mode is two frames side by side once the terminal is wide enough for
// both columns. Asserts the layout, not just stacksPanel(): every frame's
// top-left corner sits on the same row, and there are two of them, so the
// preview is beside the list and no third column survives.
func TestView_CloseModeDrawsTwoFramesSideBySide(t *testing.T) {
	applyTheme(NewTheme())
	sub := snapshot.Manifest{
		V: 1,
		Sessions: []snapshot.Session{{
			Name: "demo",
			Windows: []snapshot.Window{{
				Index:  1,
				Layout: "1cb4,80x24,0,0[80x11,0,0,0,80x12,0,12,1]",
				// A pane close's sub-manifest carries only the pane that died.
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
	m.width, m.height = 180, 30

	out := m.View().Content
	if !strings.Contains(out, "1 · agent-work") {
		t.Errorf("View() dropped the close preview's pane block:\n%s", out)
	}
	var rows []int
	for i, line := range strings.Split(out, "\n") {
		if n := strings.Count(line, "╭"); n > 0 {
			rows = append(rows, n)
			if i != 0 {
				t.Errorf("a frame opens on row %d; want every frame to open on row 0:\n%s", i, out)
			}
		}
	}
	if len(rows) != 1 || rows[0] != 2 {
		t.Errorf("frame corners per row = %v, want exactly one row carrying 2:\n%s", rows, out)
	}
}

// Snapshot mode's three-column View has been the single most important
// invariant on this branch — every task had to avoid breaking it — but
// nothing pinned its geometry the way TestView_CloseModeDrawsTwoFramesSideBySide
// pins close mode's. Checks, at several widths and heights: the render is
// exactly terminal-sized, all three frames open on row 0, and the bottom
// border row survives intact. A lipgloss.Height(out) == h check alone would
// not catch a dropped border — lipgloss v2's MaxHeight hard-truncates the
// line list rather than clipping content — so this asserts the corners
// directly, the same way TestRenderClosePreview_NeverOverflowsFrame does.
func TestView_SnapshotModeThreeColumnGeometry(t *testing.T) {
	applyTheme(NewTheme())
	events := []store.Event{{
		ID: 1, Ts: time.Now().UnixMilli(), Kind: "snapshot",
		ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"index":1,"name":"w","panes":[{"index":0,"command":"fish"}]}]}]}`,
	}}
	sizes := []struct{ w, h int }{
		{120, 24}, {120, 40}, {160, 30}, {200, 50},
	}
	for _, sz := range sizes {
		m := NewPickerModel(ModeSnapshot, events, nil, nil)
		m.Bootstrap()
		m.width, m.height = sz.w, sz.h

		out := m.View().Content
		if got := lipgloss.Height(out); got != sz.h {
			t.Errorf("w=%d h=%d: rendered height=%d, want %d\n%s", sz.w, sz.h, got, sz.h, out)
		}
		if got := lipgloss.Width(out); got != sz.w {
			t.Errorf("w=%d h=%d: rendered width=%d, want %d", sz.w, sz.h, got, sz.w)
		}

		rows := strings.Split(out, "\n")
		var topRows []int
		for i, line := range rows {
			if n := strings.Count(line, "╭"); n > 0 {
				topRows = append(topRows, n)
				if i != 0 {
					t.Errorf("w=%d h=%d: a frame opens on row %d; want every frame to open on row 0:\n%s", sz.w, sz.h, i, out)
				}
			}
		}
		if len(topRows) != 1 || topRows[0] != 3 {
			t.Errorf("w=%d h=%d: frame corners per row = %v, want exactly one row carrying 3:\n%s", sz.w, sz.h, topRows, out)
		}

		var closed bool
		for _, line := range rows {
			if strings.Count(line, "╰") == 3 && strings.Count(line, "╯") == 3 {
				closed = true
				break
			}
		}
		if !closed {
			t.Errorf("w=%d h=%d: no row closes all three frames (missing ╰/╯):\n%s", sz.w, sz.h, out)
		}
	}
}

// assertFrameCloses checks that a single frame's last row is its bottom
// border. Height alone cannot: lipgloss v2's MaxHeight truncates the line
// list, so an over-tall body drops the closing border and still measures the
// height that was asked for.
func assertFrameCloses(t *testing.T, frame string) {
	t.Helper()
	rows := strings.Split(frame, "\n")
	last := rows[len(rows)-1]
	if !strings.Contains(last, "╰") || !strings.Contains(last, "╯") {
		t.Errorf("frame's last row carries no bottom corners:\n%s", frame)
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

// A close row must never render faint or italic text: the picker used both to
// mean "not a restore target", which reads as "old / unimportant" instead —
// and dimOlderThan already claims that visual channel for age.
func TestCloseListRowStyle_NeverFaintOrItalic(t *testing.T) {
	applyTheme(Theme{})
	styles := map[string]lipgloss.Style{"section header": previewHeader}
	for _, scope := range []string{"session", "window", "pane"} {
		styles[scope] = closeRowScopeStyle(scope)
	}
	for name, st := range styles {
		if st.GetFaint() {
			t.Errorf("%s: style is faint", name)
		}
		if st.GetItalic() {
			t.Errorf("%s: style is italic", name)
		}
	}
}

func TestClosePreviewHeader(t *testing.T) {
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
				"lazytmux:3 · docs",
				"window close · 2 panes · closed 22m ago",
			},
		},
		{
			name:  "pane",
			place: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane"},
			want: []string{
				"lazytmux:3 · docs",
				"pane close · 1 pane · closed 22m ago",
			},
		},
		{
			name:  "session",
			place: ClosePlacement{Session: "lazytmux", Scope: "session"},
			want: []string{
				"lazytmux",
				"session close · 2 windows · closed 22m ago",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := closePreviewHeader(CloseContext{Placement: tc.place, SubManifest: man}, 80, now, ts)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if stripANSI(got[i]) != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, stripANSI(got[i]), tc.want[i])
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

// Snapshot mode is untouched: it keeps three columns at 120 and above.
func TestPaneWidths_SnapshotModeKeepsThreeColumns(t *testing.T) {
	m := PickerModel{mode: ModeSnapshot, width: 160, height: 40}
	if _, tree, _ := m.paneWidthsThree(); tree == 0 {
		t.Error("snapshot mode lost its middle column")
	}
}

// Tab is snapshot-only, so close mode's footer must not advertise it — while
// both modes still advertise the preview scroll wherever a preview column
// exists. Reads the keys off the bindings so a rebind can't fake a pass.
func TestRenderFooter_TabHintIsSnapshotOnly(t *testing.T) {
	applyTheme(NewTheme())
	tabKey := defaultKeys().Tab.Help().Key
	scrollKey := defaultKeys().PreviewUp.Help().Key

	closeM := PickerModel{mode: ModeClose, keys: defaultKeys(), width: 160, height: 40}
	foot := stripANSI(closeM.renderFooter(closeM.width))
	if strings.Contains(foot, tabKey+":") {
		t.Errorf("close footer advertises Tab (%q):\n%s", tabKey, foot)
	}
	if !strings.Contains(foot, scrollKey+":") {
		t.Errorf("close footer dropped the preview-scroll hint (%q):\n%s", scrollKey, foot)
	}

	snapM := PickerModel{mode: ModeSnapshot, keys: defaultKeys(), width: 160, height: 40}
	foot = stripANSI(snapM.renderFooter(snapM.width))
	if !strings.Contains(foot, tabKey+":") {
		t.Errorf("snapshot footer dropped the Tab hint (%q):\n%s", tabKey, foot)
	}
	if !strings.Contains(foot, scrollKey+":") {
		t.Errorf("snapshot footer dropped the preview-scroll hint (%q):\n%s", scrollKey, foot)
	}
}

// Tab is snapshot-only in the footer (see above); the `?` help overlay must
// agree, since it lists what a key does independent of the footer's width
// gating. Close mode has no second tree for Tab to reach — it must not appear
// in close mode's help, and must still appear in snapshot mode's.
func TestPickerModel_HelpOverlayTabIsSnapshotOnly(t *testing.T) {
	applyTheme(NewTheme())
	// "switch pane" is Tab's help description and appears nowhere else in the
	// keymap, so its presence pins Tab specifically rather than any binding
	// whose key label happens to be a common substring.
	tabDesc := defaultKeys().Tab.Help().Desc

	closeM := closeListModel(t, 4)
	closeM.help, closeM.showHelp = help.New(), true
	closeM.width, closeM.height = 160, 40
	out := stripANSI(closeM.View().Content)
	if strings.Contains(out, tabDesc) {
		t.Errorf("close mode help overlay advertises Tab (%q):\n%s", tabDesc, out)
	}

	snapM := PickerModel{mode: ModeSnapshot, keys: defaultKeys(), help: help.New(), showHelp: true, width: 160, height: 40}
	out = stripANSI(snapM.View().Content)
	if !strings.Contains(out, tabDesc) {
		t.Errorf("snapshot mode help overlay dropped the Tab hint (%q):\n%s", tabDesc, out)
	}
}

// TestPickerModel_HelpOverlayShowsFullKeymap pins that the `?` overlay renders
// bubbles' FullHelp, not ShortHelp: help.Model gates on its own ShowAll field,
// which nothing set, so the overlay always fell back to ShortHelp regardless
// of keyMap.FullHelp's content. PreviewUp's description never appears in
// ShortHelp, so its presence here is only possible through FullHelp.
func TestPickerModel_HelpOverlayShowsFullKeymap(t *testing.T) {
	applyTheme(NewTheme())
	previewDesc := defaultKeys().PreviewUp.Help().Desc

	m := PickerModel{mode: ModeSnapshot, keys: defaultKeys(), help: help.New(), showHelp: true, width: 160, height: 40}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, previewDesc) {
		t.Errorf("help overlay did not render FullHelp (missing %q):\n%s", previewDesc, out)
	}
}

// closeListFixture builds the rows and contexts the flat-row rendering tests
// share: the current session `mono` with three closes (two in its own repo,
// one in a worktree of it) plus a session close of a gone session whose name
// does not match its repo.
func closeListFixture(now time.Time) ([]CloseRow, map[int64]CloseContext, map[string]bool) {
	const repo = "/home/noams/git/tmux-remux"
	const worktree = "/home/noams/git/wt/feat-104"
	evs := []store.Event{
		{ID: 1, Ts: now.Add(-4 * time.Minute).UnixMilli()},
		{ID: 2, Ts: now.Add(-18 * time.Hour).UnixMilli()},
		{ID: 3, Ts: now.Add(-19 * time.Hour).UnixMilli()},
		{ID: 4, Ts: now.Add(-48 * time.Hour).UnixMilli()},
		{ID: 5, Ts: now.Add(-45 * time.Minute).UnixMilli()},
	}
	ctxs := map[int64]CloseContext{
		1: paneCloseCtx("mono", 2, "main", "claude", repo),
		2: windowCloseCtx("mono", 3, "docs", "fish", repo, 2),
		3: windowCloseCtx("mono", 3, "docs", "fish", repo, 2),
		4: paneCloseCtx("mono", 4, "code", "claude", worktree),
		5: sessionCloseCtx("tp-g6-nix-config", 3),
	}
	return BuildCloseList(evs, ctxs, "mono"), ctxs, map[string]bool{"mono": true}
}

func paneCloseCtx(session string, idx int, name, cmd, cwd string) CloseContext {
	return CloseContext{
		Placement: ClosePlacement{Session: session, WindowIndex: idx, WindowName: name, Scope: "pane", PaneID: "%1"},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{Name: session, Windows: []snapshot.Window{{
			Index: idx, Name: name, Panes: []snapshot.Pane{{ID: "%1", Command: cmd, Cwd: cwd}},
		}}}}},
	}
}

func windowCloseCtx(session string, idx int, name, cmd, cwd string, panes int) CloseContext {
	return CloseContext{
		Placement: ClosePlacement{Session: session, WindowIndex: idx, WindowName: name, Scope: "window", PaneCount: panes},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{Name: session, Windows: []snapshot.Window{{
			Index: idx, Name: name, Panes: []snapshot.Pane{{ID: "%1", Command: cmd, Cwd: cwd}},
		}}}}},
	}
}

func sessionCloseCtx(session string, windows int) CloseContext {
	ws := make([]snapshot.Window, windows)
	for i := range ws {
		ws[i] = snapshot.Window{Index: i + 1, Name: "w", Panes: []snapshot.Pane{{ID: "%1", Command: "fish", Cwd: "/home/noams"}}}
	}
	return CloseContext{
		Placement:   ClosePlacement{Session: session, Scope: "session"},
		SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{Name: session, Windows: ws}}},
	}
}

// rowByEvent returns the close row carrying id.
func rowByEvent(t *testing.T, rows []CloseRow, id int64) CloseRow {
	t.Helper()
	for _, r := range rows {
		if r.EventID == id {
			return r
		}
	}
	t.Fatalf("no row for event %d", id)
	return CloseRow{}
}

// TestCloseListRow_Columns pins the exact column layout at a comfortable
// width: marker, kind, the cwd column (blank where the session's modal cwd is
// what the close was in), name, extra, target, then a right-aligned count and
// age. The cwd column is as wide as the widest tail in the list — 11 for
// "wt/feat-104" — so a row that elides it still pads to keep names aligned.
func TestCloseListRow_Columns(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	rows, ctxs, live := closeListFixture(now)
	v := newCloseListView(rows, ctxs, live, now)

	blankCwd := strings.Repeat(" ", 11)
	tests := []struct {
		name string
		id   int64
		want string
	}{
		{
			name: "pane close in the session's modal cwd",
			id:   1,
			want: "● pane    " + blankCwd + " main claude → mono:2" + strings.Repeat(" ", 32) + "4m",
		},
		{
			name: "collapsed two-pane window close",
			id:   2,
			want: "● window  " + blankCwd + " docs 2p → mono:3" + strings.Repeat(" ", 32) + "×2 18h",
		},
		{
			name: "worktree close shows the discriminating tail",
			id:   4,
			want: "● pane    wt/feat-104 code claude → mono:4" + strings.Repeat(" ", 32) + "2d",
		},
		{
			name: "session close names its window count and targets the session",
			id:   5,
			want: "● session " + blankCwd + " 3w (gone) → tp-g6-nix-config" + strings.Repeat(" ", 23) + "45m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ansi.Strip(v.renderRow(rowByEvent(t, rows, tt.id), 76, false))
			if got != tt.want {
				t.Errorf("renderRow:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestCloseListRow_HeaderCarriesNoMarker keeps section headers structural:
// they render their text and nothing else, so the ● column reads as "this is
// restorable".
func TestCloseListRow_HeaderCarriesNoMarker(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	rows, ctxs, live := closeListFixture(now)
	v := newCloseListView(rows, ctxs, live, now)
	got := strings.TrimRight(ansi.Strip(v.renderRow(rows[0], 76, false)), " ")
	if got != "THIS SESSION · mono" {
		t.Errorf("header row = %q, want %q", got, "THIS SESSION · mono")
	}
}

// TestCloseListRow_ModalCwdIsNotTheSessionName is the prototype's bug: keying
// elision on "does the cwd's basename match the session name" prints
// "nix-config" on every row of session tp-g6-nix-config. Keying on the
// session's modal cwd prints nothing, because there is nothing to
// discriminate.
func TestCloseListRow_ModalCwdIsNotTheSessionName(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	evs := []store.Event{{ID: 1, Ts: now.UnixMilli()}, {ID: 2, Ts: now.UnixMilli() - 1000}}
	ctxs := map[int64]CloseContext{
		1: paneCloseCtx("tp-g6-nix-config", 1, "edit", "nvim", "/home/noams/nix-config"),
		2: paneCloseCtx("tp-g6-nix-config", 2, "shell", "claude", "/home/noams/nix-config"),
	}
	rows := BuildCloseList(evs, ctxs, "tp-g6-nix-config")
	v := newCloseListView(rows, ctxs, map[string]bool{"tp-g6-nix-config": true}, now)
	for _, r := range rows {
		if !r.Selectable() {
			continue
		}
		if got := ansi.Strip(v.renderRow(r, 76, false)); strings.Contains(got, "nix-config →") || strings.Contains(got, "nix-config edit") || strings.Contains(got, "nix-config shell") {
			t.Errorf("cwd column should be elided, got %q", got)
		}
	}
}

// TestCloseListRow_TieHasNoModalCwd: two closes, two cwds, no majority. Neither
// is the boring background, so both rows show their path rather than one being
// arbitrarily declared normal — but they show the segments that differ, not two
// near-identical absolute paths. This is the most common list shape (one close
// in a repo, one in a worktree of it), so it is where a cwd column that repeats
// itself would hurt most.
func TestCloseListRow_TieHasNoModalCwd(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	evs := []store.Event{{ID: 1, Ts: now.UnixMilli()}, {ID: 2, Ts: now.UnixMilli() - 1000}}
	ctxs := map[int64]CloseContext{
		1: paneCloseCtx("duo", 1, "main", "claude", "/home/noams/git/tmux-remux"),
		2: paneCloseCtx("duo", 2, "feat", "claude", "/home/noams/git/wt/feat-104"),
	}
	rows := BuildCloseList(evs, ctxs, "duo")
	v := newCloseListView(rows, ctxs, map[string]bool{"duo": true}, now)
	want := map[int64]string{
		1: "● pane    tmux-remux  main claude → duo:1",
		2: "● pane    wt/feat-104 feat claude → duo:2",
	}
	for id, prefix := range want {
		got := ansi.Strip(v.renderRow(rowByEvent(t, rows, id), 76, false))
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("row %d = %q, want prefix %q", id, got, prefix)
		}
		if strings.Contains(got, "/home/noams") {
			t.Errorf("row %d spends the column on the shared prefix: %q", id, got)
		}
	}
}

// TestCloseListRow_SubMinuteAgeIsSeconds keeps the age column numeric.
// humanAge says "just now", which is right in the preview pane's prose and
// eight cells of prose in a column sized for "4m".
func TestCloseListRow_SubMinuteAgeIsSeconds(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	evs := []store.Event{{ID: 1, Ts: now.Add(-47 * time.Second).UnixMilli()}}
	ctxs := map[int64]CloseContext{1: paneCloseCtx("mono", 1, "edit", "nvim", "/home/noams")}
	rows := BuildCloseList(evs, ctxs, "mono")
	v := newCloseListView(rows, ctxs, map[string]bool{"mono": true}, now)
	got := ansi.Strip(v.renderRow(rowByEvent(t, rows, 1), 76, false))
	if !strings.HasSuffix(got, "47s") || strings.Contains(got, "just") {
		t.Errorf("age column = %q, want it to end in %q", got, "47s")
	}
}

// TestCloseListRow_WidthLadder walks one row down the widths a real pane
// passes through, pinning what survives at each step: the cwd column is
// truncated from the left (the tail is what discriminates), then dropped
// whole, then the extra column goes, and the name is clipped only after that
// — never below the eight cells that still identify a window.
func TestCloseListRow_WidthLadder(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	evs := []store.Event{
		{ID: 1, Ts: now.Add(-4 * time.Minute).UnixMilli()},
		{ID: 2, Ts: now.Add(-5 * time.Minute).UnixMilli()},
		{ID: 3, Ts: now.Add(-6 * time.Minute).UnixMilli()},
	}
	ctxs := map[int64]CloseContext{
		1: paneCloseCtx("solo", 1, "release-notes-editor", "claude", "/srv/app/wt/topic-branch-long"),
		2: paneCloseCtx("solo", 2, "sh", "claude", "/srv/app/main"),
		3: paneCloseCtx("solo", 3, "sh", "claude", "/srv/app/main"),
	}
	rows := BuildCloseList(evs, ctxs, "solo")
	v := newCloseListView(rows, ctxs, map[string]bool{"solo": true}, now)
	r := rowByEvent(t, rows, 1)

	tests := []struct {
		width int
		want  string
	}{
		// 20-cell tail, quarter-row budget 30 capped at 24: the tail fits whole.
		{120, "● pane    wt/topic-branch-long release-notes-editor claude → solo:1"},
		// Quarter-row budget 19: one cell short, so the head goes, not the tail.
		{76, "● pane    …/topic-branch-long release-notes-editor claude → solo:1"},
		// The column no longer fits beside the name, and yields whole.
		{60, "● pane    release-notes-editor claude → solo:1"},
		{46, "● pane    release-notes-ed… claude → solo:1"},
		// The extra column goes before the name is cut past its floor.
		{34, "● pane    release… → solo:1"},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.width), func(t *testing.T) {
			got := strings.TrimRight(ansi.Strip(v.renderRow(r, tt.width, false)), " ")
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("w=%d:\n got %q\nwant prefix %q", tt.width, got, tt.want)
			}
		})
	}
}

// TestCloseListRow_CwdColumnNeedsRoomToMeanAnything: below eight cells a path
// fragment says nothing a reader can act on, so the column is dropped rather
// than shown as an ellipsis and a syllable.
func TestCloseListRow_CwdColumnNeedsRoomToMeanAnything(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	evs := []store.Event{
		{ID: 1, Ts: now.Add(-4 * time.Minute).UnixMilli()},
		{ID: 2, Ts: now.Add(-5 * time.Minute).UnixMilli()},
		{ID: 3, Ts: now.Add(-6 * time.Minute).UnixMilli()},
	}
	ctxs := map[int64]CloseContext{
		1: paneCloseCtx("s", 1, "a", "fish", "/srv/app/wt/topic"),
		2: paneCloseCtx("s", 2, "b", "fish", "/srv/app/main"),
		3: paneCloseCtx("s", 3, "c", "fish", "/srv/app/main"),
	}
	rows := BuildCloseList(evs, ctxs, "s")
	v := newCloseListView(rows, ctxs, map[string]bool{"s": true}, now)
	r := rowByEvent(t, rows, 1)

	// A quarter of 32 is exactly the floor, and the whole 8-cell tail fits.
	if got := ansi.Strip(v.renderRow(r, 32, false)); !strings.HasPrefix(got, "● pane    wt/topic a → s:1") {
		t.Errorf("at 32 the column should hold the tail, got %q", got)
	}
	// A quarter of 30 is under it. The row has room to spare, so this is the
	// floor talking, not the layout running out of width.
	if got := ansi.Strip(v.renderRow(r, 30, false)); !strings.HasPrefix(got, "● pane    a → s:1") {
		t.Errorf("at 30 the column should be dropped, got %q", got)
	}
}

// TestCloseListRow_GlyphDenseNameTruncatesCleanly: real window names are runs
// of nerd-font glyphs. Truncating one must leave a whole grapheme and a row
// that is still exactly one line of the requested width.
func TestCloseListRow_GlyphDenseNameTruncatesCleanly(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	const glyphy = "#[fg=#94e2d5]󰊤 #511 🧠 󰒲 󰗠 #517"
	evs := []store.Event{{ID: 1, Ts: now.UnixMilli()}}
	ctxs := map[int64]CloseContext{1: paneCloseCtx("mono", 7, glyphy, "claude", "/home/noams/git/tmux-remux")}
	rows := BuildCloseList(evs, ctxs, "mono")
	v := newCloseListView(rows, ctxs, map[string]bool{"mono": true}, now)
	r := rowByEvent(t, rows, 1)
	for _, w := range []int{30, 40, 50, 76} {
		out := v.renderRow(r, w, false)
		plain := ansi.Strip(out)
		if strings.Contains(plain, "#[") {
			t.Errorf("w=%d: tmux format directive survived StripFormat: %q", w, plain)
		}
		if !utf8.ValidString(plain) {
			t.Errorf("w=%d: row is not valid UTF-8: %q", w, plain)
		}
		for _, dangling := range []rune{'️', '︎', '‍'} {
			if strings.ContainsRune(plain, dangling) && []rune(strings.TrimRight(plain, " "))[len([]rune(strings.TrimRight(plain, " ")))-1] == dangling {
				t.Errorf("w=%d: row ends on a dangling combining rune: %q", w, plain)
			}
		}
		if !strings.Contains(plain, "→ mono:7") {
			t.Errorf("w=%d: target column lost to the name: %q", w, plain)
		}
	}
}

// TestCloseListRow_AlwaysOneLineOfExactWidth is the invariant the scroll
// window depends on: every row, at every width, is one physical line of
// exactly innerWidth cells.
func TestCloseListRow_AlwaysOneLineOfExactWidth(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	rows, ctxs, live := closeListFixture(now)
	v := newCloseListView(rows, ctxs, live, now)
	for _, w := range []int{8, 12, 20, 30, 46, 60, 76, 120} {
		for _, r := range rows {
			for _, active := range []bool{false, true} {
				out := v.renderRow(r, w, active)
				if got := lipgloss.Height(out); got != 1 {
					t.Errorf("w=%d active=%v: height=%d, want 1: %q", w, active, got, out)
				}
				if got := lipgloss.Width(out); got != w {
					t.Errorf("w=%d active=%v: width=%d, want %d: %q", w, active, got, w, out)
				}
			}
		}
	}
}

// TestCloseListRow_ElidesDefaults: a fish shell, a one-pane window and a
// session that is still running are what the column would say on nearly every
// row, so it says nothing instead. Anything else is worth the width.
func TestCloseListRow_ElidesDefaults(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	evs := []store.Event{{ID: 1, Ts: now.UnixMilli()}, {ID: 2, Ts: now.UnixMilli() - 1000}}
	ctxs := map[int64]CloseContext{
		1: windowCloseCtx("mono", 1, "shell", "fish", "/home/noams", 1),
		2: windowCloseCtx("gone-one", 2, "work", "claude", "/home/noams", 2),
	}
	rows := BuildCloseList(evs, ctxs, "mono")
	v := newCloseListView(rows, ctxs, map[string]bool{"mono": true}, now)

	defaults := ansi.Strip(v.renderRow(rowByEvent(t, rows, 1), 76, false))
	for _, unwanted := range []string{"fish", "1p", "(gone)"} {
		if strings.Contains(defaults, unwanted) {
			t.Errorf("default %q should be elided, got %q", unwanted, defaults)
		}
	}
	if want := "● window  shell → mono:1"; !strings.HasPrefix(defaults, want) {
		t.Errorf("row = %q, want prefix %q", defaults, want)
	}

	notable := ansi.Strip(v.renderRow(rowByEvent(t, rows, 2), 76, false))
	for _, wanted := range []string{"claude", "2p", "(gone)"} {
		if !strings.Contains(notable, wanted) {
			t.Errorf("%q should be printed, got %q", wanted, notable)
		}
	}
}

// TestCloseListRow_CwdYieldsBeforeTheName: when the row will not fit, the cwd
// column is given up whole before a single cell is taken from the name. A
// name clipped mid-glyph-run loses the words that identify the window; a
// dropped cwd column loses a path the preview still shows.
func TestCloseListRow_CwdYieldsBeforeTheName(t *testing.T) {
	applyTheme(NewTheme())
	now := time.Now()
	const name = "release-notes-editor"
	evs := []store.Event{
		{ID: 1, Ts: now.UnixMilli()},
		{ID: 2, Ts: now.UnixMilli() - 1000},
		{ID: 3, Ts: now.UnixMilli() - 2000},
	}
	ctxs := map[int64]CloseContext{
		1: paneCloseCtx("solo", 1, name, "claude", "/srv/app/wt/topic"),
		2: paneCloseCtx("solo", 2, "sh", "claude", "/srv/app/main"),
		3: paneCloseCtx("solo", 3, "sh", "claude", "/srv/app/main"),
	}
	rows := BuildCloseList(evs, ctxs, "solo")
	v := newCloseListView(rows, ctxs, map[string]bool{"solo": true}, now)
	r := rowByEvent(t, rows, 1)

	wide := ansi.Strip(v.renderRow(r, 76, false))
	if !strings.Contains(wide, "wt/topic") || !strings.Contains(wide, name) {
		t.Fatalf("at 76 both columns fit; got %q", wide)
	}
	// At 56 they do not both fit. The cwd column is the one that goes.
	tight := ansi.Strip(v.renderRow(r, 56, false))
	if strings.Contains(tight, "wt/topic") {
		t.Errorf("cwd column should be dropped at 56, got %q", tight)
	}
	if !strings.Contains(tight, name) {
		t.Errorf("name should survive intact at 56, got %q", tight)
	}
}

// A pane close recorded without a pane id (the positional-fallback path in
// closeevent.findClosedPane) has no id to look the died pane up by, so the row
// falls back to the sub-manifest's first pane. That is only the right answer
// because SubManifest now narrows a pane close to the pane that died.
func TestClosedPaneInfo_NoPaneIDReportsTheDiedPane(t *testing.T) {
	win := &snapshot.Window{
		Index: 2, Name: "logs", ID: "@1",
		Panes: []snapshot.Pane{
			{Index: 1, ID: "%1", Command: "claude", Cwd: "/x"},
			{Index: 2, ID: "%2", Command: "fish", Cwd: "/y"},
		},
	}
	item := &closeevent.ClosedItem{
		SessionName: "lazytmux", Pane: &win.Panes[1], Window: win, WindowIndex: 2,
	}
	cc := CloseContext{
		Placement:   ClosePlacement{Session: "lazytmux", WindowIndex: 2, WindowName: "logs", Scope: "pane"},
		SubManifest: item.SubManifest("h", 100),
	}
	cmd, cwd := closedPaneInfo(cc)
	if cmd != "fish" || cwd != "/y" {
		t.Errorf("got %q %q, want fish /y (the pane that died)", cmd, cwd)
	}
}

// closeListModel builds a close-mode model driven by the flat row list, with
// `n` closes in a second session so the list outruns any frame height the
// tests use. The names and cwds are the shape real ones have — agent and
// runner names long enough to compete for the row, and enough distinct
// directories that newCloseListView emits a cwd tail rather than eliding one
// modal cwd down the whole list. A fixture without those measures a row that
// is missing the columns the layout has to fit.
func closeListModel(t *testing.T, n int) PickerModel {
	t.Helper()
	applyTheme(NewTheme())
	now := time.Now()
	names := []string{"editor", "logs", "nvim-notes", "claude-session-1", "test-runner-long-5"}
	cwds := []string{
		"/home/noams/Data/git/noamsto/tmux-remux",
		"/home/noams/Data/git/factify/services/document",
		"/home/noams/Data/git/noamsto/nix-config/home/ai",
		"/home/noams/Data/git/noamsto/tmux-remux/internal/picker",
	}
	evs := []store.Event{{ID: 1, Ts: now.Add(-4 * time.Minute).UnixMilli()}}
	ctxs := map[int64]CloseContext{1: paneCloseCtx("mono", 2, "main", "claude", cwds[0])}
	for i := 2; i <= n+1; i++ {
		evs = append(evs, store.Event{ID: int64(i), Ts: now.Add(-time.Duration(i) * time.Hour).UnixMilli()})
		ctxs[int64(i)] = paneCloseCtx("nix-config", i, names[(i-2)%len(names)], "fish", cwds[(i-2)%len(cwds)])
	}
	m := NewPickerModel(ModeClose, evs, map[string]bool{"mono": true}, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseRows(BuildCloseList(evs, ctxs, "mono"))
	m.Bootstrap()
	return m
}

// The flat list and its preview split the width so that the preview — the
// column carrying the scrollback that tells two similar closes apart — is
// never the one that gives way: the list stops growing at a width that fits
// its columns and every cell past that goes to the preview. Below
// closeSideBySideMin there is no preview column at all; it stacks underneath
// instead, which is why widening a terminal never costs the list a column.
// Widths are spelled out rather than read off the constants, which would make
// the assertion agree with whatever the code says.
func TestPaneWidths_CloseListSplit(t *testing.T) {
	m := closeListModel(t, 12)
	// wantList of 0 means "stacked": one full-width column, no preview beside it.
	for _, tc := range []struct{ w, wantList int }{
		{80, 0}, {100, 0}, {130, 0}, {160, 0}, {162, 0}, {163, 78}, {180, 78}, {200, 80}, {250, 100}, {300, 100},
	} {
		m.width, m.height = tc.w, 40
		list, tree, preview := m.paneWidthsThree()
		if tree != 0 {
			t.Errorf("width=%d: middle column = %d, want 0", tc.w, tree)
		}
		if list+tree+preview != tc.w {
			t.Errorf("width=%d: columns sum to %d", tc.w, list+tree+preview)
		}
		if tc.wantList == 0 {
			if preview != 0 || list != tc.w {
				t.Errorf("width=%d: got list=%d preview=%d, want the whole width stacked", tc.w, list, preview)
			}
			if !m.stacksPanel() {
				t.Errorf("width=%d: preview neither beside the list nor stacked under it", tc.w)
			}
			continue
		}
		if m.stacksPanel() {
			t.Errorf("width=%d: stacked despite room for two columns", tc.w)
		}
		if list != tc.wantList {
			t.Errorf("width=%d: list = %d, want %d", tc.w, list, tc.wantList)
		}
		if preview < list {
			t.Errorf("width=%d: preview = %d, narrower than the list (%d)", tc.w, preview, list)
		}
	}
	// Past the list's ceiling the extra cells are all the preview's: fifty
	// more columns of terminal buy the list nothing and the preview fifty.
	m.width = 250
	list, _, preview := m.paneWidthsThree()
	m.width = 300
	wideList, _, widePreview := m.paneWidthsThree()
	if wideList != list || widePreview-preview != 50 {
		t.Errorf("250→300 columns: list %d→%d, preview %d→%d — growth is not all going to the preview",
			list, wideList, preview, widePreview)
	}
}

// The narrowest side-by-side width is where the list is tightest, so it is
// what sets the list's floor. A row there must still carry every column that
// changes what Enter does: the cwd tail that says which of a session's
// several directories this close was in, the name, the reopen target, and the
// "(gone)" tag that says the target session has to be recreated rather than
// reopened. layoutRow sheds columns in order as a row runs out of room, so a
// floor set a few cells lower silently drops one of them and the row reads as
// a live session, or as the only close in its directory. Widths narrower than
// this were rendered and looked at: below 78 the longest-named row loses its
// cwd, 44 loses "(gone)", 36 loses the name, 30 loses the target. The check
// is on the longest row in the fixture, since that is the one that sheds
// first.
func TestRenderCloseList_KeepsEveryDecidingColumnAtTheNarrowestSplit(t *testing.T) {
	m := closeListModel(t, 12)
	m.width, m.height = closeSideBySideMin, 40
	listW, _, _ := m.paneWidthsThree()
	lines := innerLines(t, renderCloseList(m, listW, 38))
	for _, want := range []string{
		"main claude → mono:2",
		"services/document test-runner-long-5 (gone) → nix-config:11",
	} {
		found := false
		for _, l := range lines {
			if strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no row reads %q in a %d-cell list:\n%s", want, listW, strings.Join(lines, "\n"))
		}
	}
}

// The cwd column is cut from the left, since the end of a path is what
// discriminates. A cut that lands mid-segment reads as a mangled word rather
// than a path, so it is nudged forward to the next "/" — but only while that
// is cheap: giving up a long leading segment whole costs more than the ragged
// edge does. Either way the column is exactly as wide as it was asked for,
// which is what keeps the row's other columns where layoutRow put them.
func TestFitCwd_PrefersAPathBoundary(t *testing.T) {
	for _, tc := range []struct {
		tail  string
		width int
		want  string
	}{
		{"noamsto/tmux-remux", 22, "noamsto/tmux-remux    "},
		{"noamsto/tmux-remux", 14, "…/tmux-remux  "},
		{"noamsto/tmux-remux/internal/picker", 18, "…/internal/picker "},
		// Snapping here would drop "services" as well — eight of eighteen
		// cells — so the ragged cut is the lesser loss.
		{"factify/services/document", 18, "…services/document"},
	} {
		if got := fitCwd(tc.tail, tc.width); got != tc.want {
			t.Errorf("fitCwd(%q, %d) = %q, want %q", tc.tail, tc.width, got, tc.want)
		}
	}
}

// A double-width rune straddling the truncation cut can leave the cut one
// cell over budget; fitCwd must still land on exactly width cells rather than
// panicking on a negative strings.Repeat count. Covers cwdColumnWidth's whole
// production range (8-24) against tails with CJK and emoji runes.
func TestFitCwd_ExactWidthAcrossWideRunes(t *testing.T) {
	for _, tail := range []string{
		"git/日本語プロジェクト/internal",
		"emoji/📁folder/sub",
		"noamsto/tmux-remux/internal/picker",
		"factify/services/document",
	} {
		for width := 8; width <= 24; width++ {
			got := fitCwd(tail, width)
			if w := lipgloss.Width(got); w != width {
				t.Errorf("fitCwd(%q, %d) = %q, width %d, want %d", tail, width, got, w, width)
			}
		}
	}
}

// TestView_CloseListFrameGeometry pins the flat list's frames across a size
// matrix. lipgloss v2's MaxHeight hard-truncates the rendered line list, so an
// over-tall body silently loses its closing border rather than overflowing:
// an exact height alone cannot see that, and the bottom corners can.
func TestView_CloseListFrameGeometry(t *testing.T) {
	for _, w := range []int{40, 60, 80, 100, 110, 130, 160, 163, 180, 200} {
		for _, h := range []int{10, 24, 30, 40} {
			m := closeListModel(t, 40)
			m.width, m.height = w, h
			m.SetHiddenCount(3)

			out := m.View().Content
			if got := lipgloss.Height(out); got != h {
				t.Errorf("w=%d h=%d: rendered height=%d, want %d\n%s", w, h, got, h, out)
			}
			if got := lipgloss.Width(out); got != w {
				t.Errorf("w=%d h=%d: rendered width=%d, want %d", w, h, got, w)
			}

			// One frame per row when the preview stacks (or is dropped), two
			// when it sits beside the list.
			want := 1
			if w >= closeSideBySideMin {
				want = 2
			}
			rows := strings.Split(out, "\n")
			if got := strings.Count(rows[0], "╭"); got != want {
				t.Errorf("w=%d h=%d: row 0 opens %d frames, want %d\n%s", w, h, got, want, out)
			}
			// Row 0 cannot see a frame stacked *under* the list, so count the
			// whole output too: below 80 the preview is gone entirely (README
			// promises the list gets the terminal), above it the preview
			// exists either beside the list or under it, never both.
			wantTotal := 2
			if w < 80 {
				wantTotal = 1
			}
			if got := strings.Count(out, "╭"); got != wantTotal {
				t.Errorf("w=%d h=%d: frame opens %d times overall, want %d\n%s", w, h, got, wantTotal, out)
			}
			last := len(rows) - 1 - lipgloss.Height(m.renderFooter(w))
			if got := strings.Count(rows[last], "╰"); got != want {
				t.Errorf("w=%d h=%d: last body row closes %d frames (╰), want %d\n%s", w, h, got, want, out)
			}
			if got := strings.Count(rows[last], "╯"); got != want {
				t.Errorf("w=%d h=%d: last body row closes %d frames (╯), want %d\n%s", w, h, got, want, out)
			}
		}
	}
}

// Past a frame's worth of rows a section header scrolls out of view and
// nothing says whose closes are on screen. The active section's header is
// pinned to the first row instead — and only then, so a cursor near the top
// never sees its header twice.
func TestRenderCloseList_PinsTheActiveSectionHeader(t *testing.T) {
	m := closeListModel(t, 40)
	m.width, m.height = 60, 20
	header := sectionOther

	m.SetCursor(30)
	lines := innerLines(t, renderCloseList(m, 60, 18))
	if !strings.HasPrefix(lines[0], header) {
		t.Errorf("cursor deep in %q: first row = %q, want the section header pinned\n%s", header, lines[0], strings.Join(lines, "\n"))
	}
	if n := countPrefixed(lines, header); n != 1 {
		t.Errorf("section header appears %d times, want exactly the pinned one:\n%s", n, strings.Join(lines, "\n"))
	}
	// The pinned row is a header the window itself no longer holds — not the
	// list's own header row shown in place.
	if strings.HasPrefix(lines[1], header) {
		t.Errorf("pinned header duplicates the row below it:\n%s", strings.Join(lines, "\n"))
	}

	m.SetCursor(1)
	lines = innerLines(t, renderCloseList(m, 60, 18))
	if strings.HasPrefix(lines[0], header) {
		t.Errorf("cursor at the top pinned %q over its own section:\n%s", header, strings.Join(lines, "\n"))
	}
	if n := countPrefixed(lines, sectionThis("mono")); n != 1 {
		t.Errorf("this-session header appears %d times, want 1:\n%s", n, strings.Join(lines, "\n"))
	}
}

// innerLines returns a frame's content rows, stripped of border, padding and
// styling, so a test can assert on the text a row carries.
func innerLines(t *testing.T, frame string) []string {
	t.Helper()
	rows := strings.Split(ansi.Strip(frame), "\n")
	if len(rows) < 3 {
		t.Fatalf("frame has %d rows, too few to have content:\n%s", len(rows), frame)
	}
	out := make([]string, 0, len(rows)-2)
	for _, r := range rows[1 : len(rows)-1] {
		out = append(out, strings.TrimSpace(strings.Trim(r, "│")))
	}
	return out
}

// countPrefixed counts the lines starting with prefix.
func countPrefixed(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// The hidden-count line survives the move to the flat list.
func TestRenderCloseList_KeepsTheHiddenCountFooter(t *testing.T) {
	m := closeListModel(t, 40)
	m.SetHiddenCount(3)
	lines := innerLines(t, renderCloseList(m, 60, 18))
	if got := lines[len(lines)-1]; got != "— 3 unrecoverable closes hidden —" {
		t.Errorf("last row = %q, want the hidden-count footer", got)
	}
}

// Close mode has nothing to expand or collapse, so neither the footer nor the
// `?` overlay may advertise those keys — while snapshot mode, whose tree they
// still drive, must keep them. Checked against what the keys do, not against
// the keymap: Left and Right are pressed and must move nothing.
func TestCloseMode_NeverAdvertisesExpandOrCollapse(t *testing.T) {
	applyTheme(NewTheme())
	left, right := defaultKeys().Left.Help(), defaultKeys().Right.Help()

	m := closeListModel(t, 4)
	m.width, m.height = 160, 40
	for _, surface := range []struct {
		name string
		text string
	}{
		{"footer", stripANSI(m.renderFooter(m.width))},
		{"help overlay", stripANSI(withHelpShown(m).View().Content)},
	} {
		for _, b := range []struct {
			key, desc string
		}{{left.Key, left.Desc}, {right.Key, right.Desc}} {
			if strings.Contains(surface.text, b.key+":") || strings.Contains(surface.text, b.desc) {
				t.Errorf("close mode %s advertises %q (%s):\n%s", surface.name, b.key, b.desc, surface.text)
			}
		}
	}

	// FullHelp is what the overlay renders; ShortHelp reaches no frame today,
	// so pin it directly rather than leave close mode's copy free to drift.
	short := keyMap{mode: ModeClose, Left: defaultKeys().Left, Right: defaultKeys().Right}.ShortHelp()
	for _, b := range short {
		if h := b.Help(); h.Desc == left.Desc || h.Desc == right.Desc {
			t.Errorf("close mode ShortHelp lists %q", h.Desc)
		}
	}

	snap := PickerModel{mode: ModeSnapshot, keys: defaultKeys(), help: help.New(), showHelp: true, width: 160, height: 40}
	out := stripANSI(snap.View().Content)
	for _, desc := range []string{left.Desc, right.Desc} {
		if !strings.Contains(out, desc) {
			t.Errorf("snapshot help overlay dropped %q:\n%s", desc, out)
		}
	}
}

// Close mode restores the closed entity whole: nothing on that path reads the
// filter, and only the snapshot list dims by age — so the three toggles and
// the pane counter they drive must appear on no close-mode surface, and the
// keys must move nothing. Snapshot mode, which they do drive, keeps all four.
func TestCloseMode_NeverAdvertisesFiltersOrTheCounter(t *testing.T) {
	applyTheme(NewTheme())
	toggles := []key.Binding{defaultKeys().ToggleIdle, defaultKeys().ToggleSkipRunning, defaultKeys().ToggleAge}

	m := closeListModel(t, 4)
	m.width, m.height = 160, 40
	for _, surface := range []struct{ name, text string }{
		{"footer", stripANSI(m.renderFooter(m.width))},
		{"help overlay", stripANSI(withHelpShown(m).View().Content)},
	} {
		for _, b := range toggles {
			if h := b.Help(); strings.Contains(surface.text, h.Key+":") || strings.Contains(surface.text, h.Desc) {
				t.Errorf("close mode %s advertises %q (%s):\n%s", surface.name, h.Key, h.Desc, surface.text)
			}
		}
		if strings.Contains(surface.text, "panes /") {
			t.Errorf("close mode %s carries the pane counter:\n%s", surface.name, surface.text)
		}
	}

	// Checked against what the keys do, not just what the surfaces say.
	before := m
	for _, b := range toggles {
		u, _ := m.Update(tea.KeyPressMsg{Code: rune(b.Keys()[0][0])})
		m = u.(PickerModel)
	}
	if m.filter.SkipIdleShells != before.filter.SkipIdleShells ||
		m.filter.SkipRunningSessions != before.filter.SkipRunningSessions ||
		m.dimOlderThan != before.dimOlderThan {
		t.Errorf("close mode toggles moved state: filter %+v→%+v, dim %v→%v",
			before.filter, m.filter, before.dimOlderThan, m.dimOlderThan)
	}

	snap := NewPickerModel(ModeSnapshot, []store.Event{{ID: 1, Kind: "snapshot",
		ManifestJSON: `{"v":1,"sessions":[{"name":"s","windows":[{"name":"w","panes":[{"index":0,"command":"fish"}]}]}]}`}}, nil, nil)
	snap.width, snap.height = 160, 40
	snap.Bootstrap()
	foot := stripANSI(snap.renderFooter(snap.width))
	overlay := stripANSI(withHelpShown(snap).View().Content)
	for _, b := range toggles {
		h := b.Help()
		if !strings.Contains(foot, h.Key+":") {
			t.Errorf("snapshot footer dropped %q (%s):\n%s", h.Key, h.Desc, foot)
		}
		if !strings.Contains(overlay, h.Desc) {
			t.Errorf("snapshot help overlay dropped %q:\n%s", h.Desc, overlay)
		}
	}
	if !strings.Contains(foot, "1 panes / 0 skipped") {
		t.Errorf("snapshot footer dropped the pane counter:\n%s", foot)
	}
}

// withHelpShown returns m with the `?` overlay open.
func withHelpShown(m PickerModel) PickerModel {
	m.help, m.showHelp = help.New(), true
	return m
}

// A stacked preview is still a preview: the scroll hint must survive the
// widths where the flat list has no preview column beside it.
func TestRenderFooter_StackedCloseListKeepsTheScrollHint(t *testing.T) {
	applyTheme(NewTheme())
	scrollKey := defaultKeys().PreviewUp.Help().Key
	m := closeListModel(t, 4)
	m.width, m.height = 100, 40
	foot := stripANSI(m.renderFooter(m.width))
	if !strings.Contains(foot, scrollKey+":") {
		t.Errorf("stacked close footer dropped the preview-scroll hint (%q):\n%s", scrollKey, foot)
	}
}

// The list frame's own box math, independent of View's composition: exact
// size and an intact bottom border at every pane size, with the hidden-count
// footer competing for the last row.
func TestRenderCloseList_NeverOverflowsFrame(t *testing.T) {
	// A long, wide-glyph window name and an oversized section header put both
	// row kinds past every pane width here, so the truncation budget — not the
	// fixture's own modest labels — is what keeps the frame square.
	long := closeListModel(t, 40)
	for i := range long.closeRows {
		long.closeRows[i].Section = "OTHER SESSIONS — a header far too long for any narrow pane 🧠"
		long.closeRows[i].Placement.WindowName = "a-really-long-window-name-that-will-not-fit-in-any-narrow-pane 🧠"
	}
	for _, m := range []PickerModel{closeListModel(t, 40), long} {
		m.SetHiddenCount(14)
		for _, size := range []struct{ w, h int }{{28, 5}, {32, 8}, {50, 6}, {64, 12}, {80, 30}} {
			m.width, m.height = size.w, size.h
			for _, cursor := range []int{1, 25} {
				m.SetCursor(cursor)
				out := renderCloseList(m, size.w, size.h)
				if got := lipgloss.Width(out); got != size.w {
					t.Errorf("w=%d h=%d cursor=%d: width %d, want %d", size.w, size.h, cursor, got, size.w)
				}
				if got := lipgloss.Height(out); got != size.h {
					t.Errorf("w=%d h=%d cursor=%d: height %d, want %d", size.w, size.h, cursor, got, size.h)
				}
				assertFrameCloses(t, out)
			}
		}
	}
}
