package picker

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/noamsto/tmux-remux/internal/snapshot"
)

// View renders the full picker UI. Called by Bubble Tea after every Update.
func (m PickerModel) View() tea.View {
	if m.showHelp {
		// keyMap.mode is never set at construction, so stamp the live mode on
		// here — the one place ShortHelp/FullHelp are consulted.
		helpKeys := m.keys
		helpKeys.mode = m.mode
		hm := m.help
		hm.ShowAll = true // the overlay is the full keymap; renderFooter writes its own hints.
		v := tea.NewView(hm.View(helpKeys))
		v.AltScreen = true
		return v
	}

	if m.width == 0 {
		// First frame, before Bubble Tea delivers the initial WindowSizeMsg.
		return tea.NewView("")
	}

	listWidth, treeWidth, previewWidth := m.paneWidthsThree()
	// Render the footer first and reserve its measured height; the body frames
	// take the rest. Measuring (rather than assuming exactly one row) keeps the
	// body from overflowing the popup and pushing the footer off-screen.
	footer := m.renderFooter(m.width)
	bodyHeight := m.bodyHeight()
	var content string
	switch {
	case m.mode == ModeClose && previewWidth == 0 && !m.stacksPanel():
		// Too narrow for a preview at all — the list gets the whole width.
		content = lipgloss.JoinVertical(lipgloss.Left, renderCloseList(m, m.width, bodyHeight), footer)
	case m.mode == ModeClose && previewWidth == 0:
		// Below closeSideBySideMin the preview goes under the list, full
		// width: a column narrow enough to fit beside the list truncates the
		// scrollback that is the whole reason to show it.
		topHeight := bodyHeight - m.panelFrameHeight()
		list := renderCloseList(m, m.width, topHeight)
		panel := m.renderPreview(m.width)
		content = lipgloss.JoinVertical(lipgloss.Left, list, panel, footer)
	case m.mode == ModeClose:
		list := renderCloseList(m, listWidth, bodyHeight)
		preview := m.renderPreview(previewWidth)
		body := lipgloss.JoinHorizontal(lipgloss.Top, list, preview)
		content = lipgloss.JoinVertical(lipgloss.Left, body, footer)
	case m.width < 80:
		list := renderList(m, listWidth, bodyHeight)
		content = lipgloss.JoinVertical(lipgloss.Left, list, footer)
	case previewWidth == 0:
		// previewWidth==0 here means exactly the stacksPanel() range, since
		// the width<80 case above already claimed anything narrower.
		topHeight := bodyHeight - m.panelFrameHeight()
		list := renderList(m, listWidth, topHeight)
		tree := renderTree(m, treeWidth, topHeight)
		top := lipgloss.JoinHorizontal(lipgloss.Top, list, tree)
		panel := m.renderPreview(m.width)
		content = lipgloss.JoinVertical(lipgloss.Left, top, panel, footer)
	default:
		list := renderList(m, listWidth, bodyHeight)
		tree := renderTree(m, treeWidth, bodyHeight)
		preview := m.renderPreview(previewWidth)
		body := lipgloss.JoinHorizontal(lipgloss.Top, list, tree, preview)
		content = lipgloss.JoinVertical(lipgloss.Left, body, footer)
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// renderFooter renders the footer bar with the keys that act on the current
// mode and an optional transient warning note. Format: `key:value` pairs in lavender +
// state color, separated by a dim "·" so the eye can lock onto each pair.
func (m PickerModel) renderFooter(width int) string {
	// Key/desc come from the bindings themselves, so a rebind can't leave the
	// footer advertising a stale key.
	toggle := func(b bool, bind key.Binding) string {
		state := footerOff.Render("off")
		if b {
			state = footerOn.Render("on")
		}
		h := bind.Help()
		return footerKey.Render(h.Key) + footerSep.Render(":") + state + footerSep.Render(" "+h.Desc)
	}
	hint := func(bind key.Binding) string {
		h := bind.Help()
		return footerKey.Render(h.Key) + footerSep.Render(":"+h.Desc)
	}
	sep := footerSep.Render(" · ")

	var parts []string
	// The three filter toggles and the pane counter are snapshot mode's alone.
	// Close mode restores the closed entity whole — nothing on that path reads
	// m.filter, and the age toggle only dims snapshot rows — so in close mode
	// all four would sit in front of ↵:restore describing nothing. The close
	// preview's header carries the truthful count instead.
	if m.mode == ModeSnapshot {
		c := m.CurrentCounts()
		parts = append(parts,
			toggle(m.filter.SkipIdleShells, m.keys.ToggleIdle),
			toggle(m.filter.SkipRunningSessions, m.keys.ToggleSkipRunning),
			toggle(m.dimOlderThan > 0, m.keys.ToggleAge),
			fmt.Sprintf("%d panes / %d skipped", c.KeptPanes, c.SkippedPanes),
		)
	}
	parts = append(parts, hint(m.keys.Enter))
	// Tab reaches snapshot mode's sub-manifest tree; close mode has no second
	// tree, and its preview scrolls with Alt+j/k regardless of focus. A
	// stacked preview is still a preview, so close mode advertises the scroll
	// at widths where paneWidthsThree reports no preview column.
	_, _, previewW := m.paneWidthsThree()
	if previewW > 0 || (m.mode == ModeClose && m.stacksPanel()) {
		if m.mode == ModeSnapshot {
			parts = append(parts, hint(m.keys.Tab))
		}
		parts = append(parts, hint(m.keys.PreviewUp))
	}
	line := strings.Join(parts, sep)
	if m.footerNote != "" {
		line = footerWarn.Render(m.footerNote) + sep + line
	}
	if m.demoKeys && m.lastKey != "" {
		line = keyCast.Render(" "+m.lastKey+" ") + sep + line
	}
	// Truncate: footerBar.Width wraps overflow to a second row otherwise, which
	// would break the single-row height View() reserves for the footer.
	innerWidth := width - footerBar.GetHorizontalFrameSize()
	if innerWidth < 1 {
		innerWidth = 1
	}
	line = ansi.Truncate(line, innerWidth, "…")
	return footerBar.Width(width).Render(line)
}

// bodyHeight is the height available to the panes above the footer. View and
// previewInnerHeight must agree on it, so both read it from here.
func (m PickerModel) bodyHeight() int {
	h := m.height - lipgloss.Height(m.renderFooter(m.width))
	if h < 5 {
		h = 5
	}
	return h
}

// closeSideBySideMin is the narrowest terminal that puts the flat close list
// and its preview side by side: it is not a number of its own but the sum of
// what the two columns each need. Below it one of them would have to give up
// something the other cannot replace, so the preview goes underneath at full
// width instead — where even an 80-column terminal can read it, and where the
// list keeps every column the whole terminal can hold.
const closeSideBySideMin = closeListMin + closePreviewMin

// closePreviewMin is the narrowest preview that shows a full-width tmux pane
// line whole. previewWindow cuts scrollback to the column rather than
// wrapping it, so a preview below this silently clips the right-hand end of
// every line: 85 cells is border, padding and the block rail (5) plus the 80
// columns a default pane wraps at.
const closePreviewMin = 85

// stacksPanel reports whether the map/scrollback panel goes under the list
// rather than beside it — the case for a terminal too narrow for both. The
// popup is 90% of the client, so snapshot mode's threshold lands near 120
// columns; the close list needs one column less furniture and holds out to
// closeSideBySideMin.
func (m PickerModel) stacksPanel() bool {
	if m.width < 80 {
		return false
	}
	if m.mode == ModeClose {
		return m.width < closeSideBySideMin
	}
	return m.width < 120
}

// panelFrameHeight is the frame height of the map/scrollback panel: the whole
// body beside the tree, or the lower half beneath it.
func (m PickerModel) panelFrameHeight() int {
	body := m.bodyHeight()
	if !m.stacksPanel() {
		return body
	}
	return body - body/2
}

// paneWidthsThree splits the available width between list, tree, and preview.
// Returns (list, tree, preview) where preview==0 means the preview pane is
// hidden at this width.
func (m PickerModel) paneWidthsThree() (int, int, int) {
	if m.width < 80 {
		return m.width, 0, 0
	}
	if m.mode == ModeClose {
		if m.width < closeSideBySideMin {
			// Stacked: both halves span the terminal.
			return m.width, 0, 0
		}
		listW := closeListWidth(m.width)
		return listW, 0, m.width - listW
	}
	if m.width < 120 {
		// Two-pane fallback (current behavior).
		listW := m.width / 3
		if listW < 28 {
			listW = 28
		}
		return listW, m.width - listW, 0
	}
	// Three-pane: 1/4 list, 1/3 tree, remainder preview. At width ≥ 120 the
	// proportions guarantee previewW ≥ 50 and both min-clamps (28/32) are
	// already satisfied by the proportional values, so no squeeze is needed.
	listW := m.width / 4
	treeW := m.width / 3
	return listW, treeW, m.width - listW - treeW
}

// closeListWidth splits a side-by-side close layout between the list and the
// preview. The list's appetite is bounded — its columns are a marker, a kind,
// a path tail, a name and a reopen target, and past closeListMax the extra
// cells only pad the gap before the age — while the preview's is not: it
// shows scrollback, which is what tells two otherwise identical closes apart.
// So the list takes a fixed share between its floor and that ceiling, and
// every cell beyond goes to the preview. The floor binds until the terminal
// is around 195 columns wide; the proportion is what keeps the two growing
// together past that.
func closeListWidth(width int) int {
	w := width * 2 / 5
	if w < closeListMin {
		w = closeListMin
	}
	if w > closeListMax {
		w = closeListMax
	}
	return w
}

// Bounds on the close list's column, both read off rendered output for a list
// whose rows carry every column — including the cwd tail, which only appears
// when a session's closes are not all in one directory. The floor is where
// layoutRow stops shedding: below 78 the cwd goes first, then at 44 the
// "(gone)" tag that says the session must be recreated, at 36 the window
// name, at 30 the reopen target. The ceiling is where the cwd column reaches
// its 24-cell cap and further cells only pad the gap before the age.
const (
	closeListMin = 78
	closeListMax = 100
)

func renderList(m PickerModel, width, height int) string {
	frame := listFrame.Width(width).Height(height).MaxHeight(height)
	if len(m.events) == 0 {
		return frame.Render(rowDim.Render("No snapshots yet — run `tmux-remux save`."))
	}
	// Inner content width excludes border + padding; rows and the footer must be
	// truncated to it, since lipgloss .Width() wraps overflow onto extra physical
	// lines and breaks the one-row-per-event assumption scrollWindow depends on.
	innerWidth := width - listFrame.GetHorizontalFrameSize()
	if innerWidth < 1 {
		innerWidth = 1
	}
	// Inner content height = frame height − 2 (top+bottom border). Reserve the
	// bottom line for the hidden-count footer, but only when there is more than
	// one row — at the minimum height the lone row goes to events.
	rows := height - 2
	if rows < 1 {
		rows = 1
	}
	showFooter := m.hiddenCount > 0 && rows > 1
	eventRows := rows
	if showFooter {
		eventRows--
	}
	start, end := scrollWindow(m.cursor, len(m.events), eventRows)

	var b strings.Builder
	now := time.Now()
	for i := start; i < end; i++ {
		ev := m.events[i]
		ts := time.UnixMilli(ev.Ts).Format("01-02 15:04")
		line := fmt.Sprintf("#%d %s %s", ev.ID, ts, shortReason(ev.Reason))
		dim := m.dimOlderThan > 0 && now.Sub(time.UnixMilli(ev.Ts)) > m.dimOlderThan
		style := rowDefault
		switch {
		case i == m.cursor:
			style = rowActive
		case dim:
			style = rowDim
		}
		line = ansi.Truncate(line, innerWidth, "…")
		b.WriteString(style.Width(innerWidth).Render(line))
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	if showFooter {
		// Pin the footer to the bottom of the inner area, blank-padding the gap
		// between the last event row and the footer line.
		for pad := end - start; pad < eventRows; pad++ {
			b.WriteString("\n")
		}
		b.WriteString("\n")
		text := ansi.Truncate(fmt.Sprintf("— %s hidden —", hiddenPhrase(m.hiddenCount)), innerWidth, "…")
		b.WriteString(rowDim.Width(innerWidth).Align(lipgloss.Center).Render(text))
	}
	return frame.Render(b.String())
}

// renderCloseList renders the flat close list into the list pane: one physical
// row per CloseRow, the hidden-count footer pinned to the bottom, and — once
// the cursor's own section header has scrolled off the top — that header
// pinned to the first row, so a long list never leaves the reader guessing
// whose closes they are looking at.
func renderCloseList(m PickerModel, width, height int) string {
	frame := listFrame.Width(width).Height(height).MaxHeight(height)
	innerWidth := width - listFrame.GetHorizontalFrameSize()
	if innerWidth < 1 {
		innerWidth = 1
	}
	if len(m.closeRows) == 0 {
		msg := "No close events yet."
		if m.hiddenCount > 0 {
			msg = fmt.Sprintf("No recoverable closes (%d hidden).", m.hiddenCount)
		}
		return frame.Render(rowDim.Render(msg))
	}

	rows := height - 2
	if rows < 1 {
		rows = 1
	}
	showFooter := m.hiddenCount > 0 && rows > 1
	rowBudget := rows
	if showFooter {
		rowBudget--
	}

	start, end := scrollWindow(m.cursor, len(m.closeRows), rowBudget)
	// Shrinking the window can only push start further down, so the pinned
	// header cannot come back into view and this settles in one pass.
	pin := sectionHeaderIdx(m.closeRows, m.cursor)
	if pin >= start || rowBudget < 2 {
		pin = -1
	} else {
		rowBudget--
		start, end = scrollWindow(m.cursor, len(m.closeRows), rowBudget)
	}

	v := newCloseListView(m.closeRows, m.closeContexts, m.runningSet, time.Now())
	var b strings.Builder
	if pin >= 0 {
		b.WriteString(v.renderRow(m.closeRows[pin], innerWidth, false))
		b.WriteString("\n")
	}
	for i := start; i < end; i++ {
		b.WriteString(v.renderRow(m.closeRows[i], innerWidth, i == m.cursor))
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	if showFooter {
		// Pin the footer to the bottom, blank-padding the gap above it.
		for pad := end - start; pad < rowBudget; pad++ {
			b.WriteString("\n")
		}
		b.WriteString("\n")
		text := ansi.Truncate(fmt.Sprintf("— %s hidden —", hiddenPhrase(m.hiddenCount)), innerWidth, "…")
		b.WriteString(rowDim.Width(innerWidth).Align(lipgloss.Center).Render(text))
	}
	return frame.Render(b.String())
}

// sectionHeaderIdx returns the index of the section header governing rows[i],
// or -1 when nothing above it is one.
func sectionHeaderIdx(rows []CloseRow, i int) int {
	if i >= len(rows) {
		i = len(rows) - 1
	}
	for ; i >= 0; i-- {
		if rows[i].Kind == RowSectionHeader {
			return i
		}
	}
	return -1
}

// hiddenPhrase renders the pluralized "N unrecoverable close(s)" fragment.
func hiddenPhrase(n int) string {
	noun := "closes"
	if n == 1 {
		noun = "close"
	}
	return fmt.Sprintf("%d unrecoverable %s", n, noun)
}

// scrollWindow returns [start,end) such that `cursor` falls inside and the
// window length ≤ rows. Tries to keep the cursor centered; clamps at the ends
// of the list so a small list with many rows stays anchored at index 0.
func scrollWindow(cursor, total, rows int) (int, int) {
	if total <= rows {
		return 0, total
	}
	half := rows / 2
	start := cursor - half
	if start < 0 {
		start = 0
	}
	end := start + rows
	if end > total {
		end = total
		start = end - rows
	}
	return start, end
}

func renderTree(m PickerModel, width, height int) string {
	frame := treeFrame.Width(width).Height(height).MaxHeight(height)
	id := m.CurrentEventID()
	if id == 0 {
		return frame.Render("")
	}
	if err, bad := m.manifestErrors[id]; bad {
		return frame.Render(footerWarn.Render("(invalid manifest)") + "\n" + skipReason.Render(err.Error()))
	}
	tree := m.trees[id]
	if tree == nil {
		return frame.Render(rowDim.Render("(loading...)"))
	}
	if len(tree.Children) == 0 {
		return frame.Render(rowDim.Render("(empty snapshot)"))
	}

	// Inner content width excludes border + padding; every node row (and the
	// header) is truncated to it — lipgloss frames wrap overflow, which would
	// desync the one-row-per-node windowing below.
	innerWidth := width - treeFrame.GetHorizontalFrameSize()
	if innerWidth < 1 {
		innerWidth = 1
	}

	var b strings.Builder
	header := fmt.Sprintf("Contents (#%d)", id)
	b.WriteString(ansi.Truncate(previewHeader.Render(header), innerWidth, "…"))
	b.WriteString("\n")

	highlightIdx := -1
	if m.focus == focusTree {
		highlightIdx = m.treeCursor
	}
	skipRunningHint := m.keys.ToggleSkipRunning.Help()
	toggleHint := skipRunningHint.Key + ":" + skipRunningHint.Desc

	idx := 0
	var rows []string
	for _, sess := range tree.Children {
		appendNodeRows(&rows, sess, 0, &idx, highlightIdx, toggleHint)
	}

	// Header (1) + border (2) consume 3 rows inside the frame's height.
	visible := height - 3
	if visible < 1 {
		visible = 1
	}
	start, end := scrollWindow(highlightIdx, len(rows), visible)
	for i := start; i < end; i++ {
		b.WriteString(ansi.Truncate(rows[i], innerWidth, "…"))
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return frame.Render(b.String())
}

// appendNodeRows appends one rendered string per visible node of the subtree
// rooted at n. idx tracks the position in the flat visible-node list and is
// incremented for each row appended. highlightIdx is the row to mark active
// (−1 = none). Caller windows the returned slice for scrolling.
func appendNodeRows(rows *[]string, n *TreeNode, depth int, idx *int, highlightIdx int, toggleHint string) {
	indent := strings.Repeat("  ", depth)
	bullet := "•"
	if len(n.Children) > 0 {
		if n.Expanded {
			bullet = "▾"
		} else {
			bullet = "▸"
		}
	}
	// A session collapsed by the running-session filter has nothing new to
	// reveal on expand, unlike other skip reasons — name what's hidden.
	note := "(" + n.SkipReason + ")"
	if n.Skipped && n.SkipReason == "running" && !n.Expanded {
		if hidden := countPanes(n); hidden > 0 {
			note = fmt.Sprintf("%d panes hidden — %s", hidden, toggleHint)
		}
	}
	active := *idx == highlightIdx
	var rendered string
	if active {
		// Active row gets a single flat style: lipgloss v2 strips ESC bytes
		// from pre-styled input, so nesting role-color inside rowActive's
		// mauve background can collapse to mauve-on-mauve = invisible. Render
		// once, plain.
		line := fmt.Sprintf("%s%s %s", indent, bullet, n.Label)
		if n.Skipped && n.SkipReason != "" {
			line = line + "  " + note
		}
		rendered = rowActive.Render(line)
	} else {
		var style lipgloss.Style
		switch n.Kind {
		case NodeSession:
			style = nodeSession
		case NodeWindow:
			style = nodeWindow
		default:
			style = nodePane
		}
		if n.Skipped {
			// Keep the role color so the tree shape stays legible when
			// skip-running marks everything skipped; just dim it.
			style = style.Faint(true).Italic(true)
		}
		styled := style.Render(n.Label)
		if n.Skipped && n.SkipReason != "" {
			styled = styled + "  " + skipReason.Render(note)
		}
		rendered = fmt.Sprintf("%s%s %s", indent, bullet, styled)
	}
	*rows = append(*rows, rendered)
	*idx++
	if n.Expanded {
		for _, c := range n.Children {
			appendNodeRows(rows, c, depth+1, idx, highlightIdx, toggleHint)
		}
	}
}

// shortReason names a save reason in words the reader can act on. The stored
// strings are internal hook names, and abbreviating them ("screat") or cutting
// them to 8 characters ("hook:after-split-window" → "hook:aft") said nothing
// about what the snapshot caught. renderList already truncates to the pane
// width with an ellipsis, so there is no second length cut here.
func shortReason(r string) string {
	switch r {
	case "keybinding":
		return "key"
	case "hook:after-split-window":
		return "split"
	case "hook:window-linked":
		return "new window"
	case "hook:session-created":
		return "new session"
	case "hook:client-detached":
		return "detach"
	}
	// An unmapped reason keeps its own words; only the hook: prefix goes, since
	// every row in this list is a save and the prefix distinguishes nothing.
	return strings.TrimPrefix(r, "hook:")
}

// closeListView holds the facts a single flat close row cannot work out on its
// own: the contexts its columns read, which sessions are still alive, and —
// the reason this type exists — each session's modal cwd, which decides
// whether the cwd column earns its width on a given row.
type closeListView struct {
	ctxs  map[int64]CloseContext
	live  map[string]bool
	now   time.Time
	tails map[int64]string // EventID → cwd tail; absent when elided
	// widest is the widest tail in the list, so rows that elide theirs still
	// pad to keep the columns right of it aligned. 0 drops the column.
	widest int
}

// newCloseListView precomputes the per-list column facts for rows.
func newCloseListView(rows []CloseRow, ctxs map[int64]CloseContext, live map[string]bool, now time.Time) closeListView {
	v := closeListView{ctxs: ctxs, live: live, now: now, tails: map[int64]string{}}

	counts := map[string]map[string]int{}
	cwds := map[int64]string{}
	for _, r := range rows {
		if !r.Selectable() {
			continue
		}
		_, cwd := closedPaneInfo(ctxs[r.EventID])
		if cwd == "" {
			continue
		}
		cwds[r.EventID] = cwd
		if counts[r.Session] == nil {
			counts[r.Session] = map[string]int{}
		}
		counts[r.Session][cwd]++
	}

	modal := make(map[string]string, len(counts))
	base := make(map[string]string, len(counts))
	for session, byCwd := range counts {
		modal[session] = modalCwd(byCwd)
		base[session] = commonPathPrefix(byCwd)
	}
	for _, r := range rows {
		cwd, ok := cwds[r.EventID]
		if !ok || cwd == modal[r.Session] {
			continue
		}
		tail := cwdTail(cwd, base[r.Session])
		v.tails[r.EventID] = tail
		if w := lipgloss.Width(tail); w > v.widest {
			v.widest = w
		}
	}
	return v
}

// modalCwd returns the cwd a session's closes are usually in — the one the
// column would repeat down the list, and therefore the one worth eliding.
// A tie has no such background: both cwds discriminate, so neither is elided
// and the function returns "". A session with a single close ties with
// nothing and elides, since there is nothing to tell apart.
func modalCwd(byCwd map[string]int) string {
	best, bestN, tied := "", 0, false
	for cwd, n := range byCwd {
		switch {
		case n > bestN:
			best, bestN, tied = cwd, n, false
		case n == bestN:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// commonPathPrefix returns the deepest directory every one of a session's
// cwds sits under. It is the strip base rather than the modal cwd because the
// two answer different questions: modal decides whether a row says anything
// at all, while the base decides how much of what it says is already implied
// by its neighbours. Keying the strip on modal would print full absolute
// paths whenever the session has no modal cwd — the common two-close case.
func commonPathPrefix(byCwd map[string]int) string {
	var base []string
	first := true
	for cwd := range byCwd {
		segs := strings.Split(cwd, "/")
		if first {
			base, first = segs, false
			continue
		}
		i := 0
		for i < len(base) && i < len(segs) && base[i] == segs[i] {
			i++
		}
		base = base[:i]
	}
	return strings.Join(base, "/")
}

// cwdTail returns the part of cwd that base does not already say: the path
// segments after the two share a prefix. Falls back to the whole path when
// the paths share no prefix, and to the last segment when cwd is base itself
// and so has no tail of its own.
func cwdTail(cwd, base string) string {
	if base == "" {
		return cwd
	}
	have, want := strings.Split(cwd, "/"), strings.Split(base, "/")
	i := 0
	for i < len(have) && i < len(want) && have[i] == want[i] {
		i++
	}
	if i >= len(have) {
		return have[len(have)-1]
	}
	return strings.Join(have[i:], "/")
}

// columnAge renders an age for the right-aligned age column, which is two or
// three cells wide at every scale. humanAge's "just now" is prose written for
// the preview pane's sentences; eight cells of it here costs the name column
// its width and truncates into a false value ("just …") on a narrow row.
func columnAge(d time.Duration) string {
	if d < time.Minute {
		if d < 0 {
			d = 0
		}
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return strings.TrimSuffix(humanAge(d), " ago")
}

// closeMarker opens every restorable row. Section headers carry no marker,
// which is what the column means: there is something here to restore.
const closeMarker = "● "

// closeKindWidth pads the kind column so every row's cwd starts in the same
// column. "session" is the longest of the three.
const closeKindWidth = 7

// renderRow renders one flat close row as a single line of exactly innerWidth
// cells. Section headers render their text alone — no marker, since the marker
// column means "restorable".
func (v closeListView) renderRow(r CloseRow, innerWidth int, active bool) string {
	if innerWidth < 1 {
		innerWidth = 1
	}
	if !r.Selectable() {
		return previewHeader.Width(innerWidth).Render(ansi.Truncate(r.Section, innerWidth, "…"))
	}

	cc := v.ctxs[r.EventID]
	cmd, _ := closedPaneInfo(cc)
	name := snapshot.StripFormat(r.Placement.WindowName)
	target := "→ " + r.Session
	if r.Scope == "session" {
		name = fmt.Sprintf("%dw", countWindows(cc.SubManifest))
	} else {
		target += ":" + strconv.Itoa(r.Placement.WindowIndex)
	}

	// Defaults say nothing, so they are not printed: a shell that is fish, a
	// window that held one pane, a session that is still running.
	var extra []string
	if cmd != "" && cmd != "fish" {
		extra = append(extra, cmd)
	}
	if !v.live[r.Session] {
		extra = append(extra, "(gone)")
	}
	if r.Placement.PaneCount > 1 {
		extra = append(extra, fmt.Sprintf("%dp", r.Placement.PaneCount))
	}

	var right []string
	if r.Count > 1 {
		right = append(right, fmt.Sprintf("×%d", r.Count))
	}
	right = append(right, columnAge(v.now.Sub(time.UnixMilli(r.Ts))))
	tail := strings.Join(right, " ")

	line := v.layoutRow(r, name, extra, target, tail, innerWidth)
	if active {
		// One flat style over plain text: lipgloss v2 strips ESC bytes from
		// pre-styled input, so a role colour nested inside rowActive's
		// background collapses to invisible.
		return rowActive.Width(innerWidth).Render(line)
	}
	return closeRowScopeStyle(r.Scope).Width(innerWidth).Render(line)
}

// layoutRow fits the columns into innerWidth by giving them up in order of
// how little they say. The cwd column goes first — it is the one that most
// often has nothing to say — then the name is clipped to a readable floor,
// then the extra column, and only then is the name cut to the bone. The name
// is defended this far because nerd-font glyph runs measure narrower than
// they paint, so a name cut mid-run loses the words that identify it.
func (v closeListView) layoutRow(r CloseRow, name string, extra []string, target, tail string, innerWidth int) string {
	avail := innerWidth - lipgloss.Width(tail) - 1
	if avail < 1 {
		avail = 1
	}

	build := func(name string, cwdWidth int, extra []string) string {
		cols := []string{closeMarker + fmt.Sprintf("%-*s", closeKindWidth, r.Scope)}
		if cwdWidth > 0 {
			cols = append(cols, fitCwd(v.tails[r.EventID], cwdWidth))
		}
		cols = append(cols, name)
		cols = append(cols, extra...)
		return strings.Join(append(cols, target), " ")
	}
	clip := func(line string, floor int) string {
		budget := lipgloss.Width(name) - (lipgloss.Width(line) - avail)
		if budget < floor {
			budget = floor
		}
		return ansi.Truncate(name, budget, "…")
	}

	left := build(name, v.cwdColumnWidth(innerWidth), extra)
	if lipgloss.Width(left) > avail {
		left = build(name, 0, extra)
	}
	if lipgloss.Width(left) > avail {
		name = clip(left, 8)
		left = build(name, 0, extra)
	}
	if lipgloss.Width(left) > avail && len(extra) > 0 {
		left = build(name, 0, nil)
		extra = nil
	}
	if lipgloss.Width(left) > avail {
		left = build(clip(left, 4), 0, extra)
	}

	line := left
	if gap := innerWidth - lipgloss.Width(left) - lipgloss.Width(tail); gap > 0 {
		line += strings.Repeat(" ", gap)
	} else {
		line += " "
	}
	return ansi.Truncate(line+tail, innerWidth, "…")
}

// cwdColumnWidth budgets the cwd column: as wide as the widest tail in the
// list, but never more than a quarter of the row, and dropped entirely when
// that quarter is too narrow to hold a meaningful path fragment.
func (v closeListView) cwdColumnWidth(innerWidth int) int {
	if v.widest == 0 {
		return 0
	}
	budget := innerWidth / 4
	if budget > 24 {
		budget = 24
	}
	if budget < 8 {
		return 0
	}
	if v.widest < budget {
		return v.widest
	}
	return budget
}

// fitCwd pads or left-truncates a tail to exactly width cells. Truncation is
// from the left, since the tail is what discriminates. A cut that lands
// mid-segment ("…sto/tmux-remux") reads as a mangled word rather than a path,
// so the cut is nudged forward to the next "/" when that costs only a few
// more cells — past that the segment is long enough that losing it whole
// gives up more than the ragged edge does.
func fitCwd(tail string, width int) string {
	w := lipgloss.Width(tail)
	if w <= width {
		return tail + strings.Repeat(" ", width-w)
	}
	cut := ansi.TruncateLeft(tail, w-width+1, "…")
	if i := strings.IndexByte(cut, '/'); i > 0 && lipgloss.Width(cut[:i]) <= 6 {
		cut = "…" + cut[i:]
	}
	// A double-width rune straddling the TruncateLeft cut can leave it one
	// cell over width; clamp before padding so the Repeat count never goes
	// negative.
	cut = ansi.Truncate(cut, width, "")
	if pad := width - lipgloss.Width(cut); pad > 0 {
		cut += strings.Repeat(" ", pad)
	}
	return cut
}

// closeRowScopeStyle colours a close row by what it would restore, matching
// the palette the preview tree uses for the same three levels.
func closeRowScopeStyle(scope string) lipgloss.Style {
	switch scope {
	case "session":
		return nodeSession
	case "window":
		return nodeWindow
	}
	return nodePane
}
