package picker

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// View renders the full picker UI. Called by Bubble Tea after every Update.
func (m PickerModel) View() tea.View {
	if m.showHelp {
		// m.keys.mode isn't kept in sync at construction time (it's never set
		// by defaultKeys/NewPickerModel), so stamp the live mode on here —
		// the one place ShortHelp/FullHelp are actually consulted.
		helpKeys := m.keys
		helpKeys.mode = m.mode
		v := tea.NewView(m.help.View(helpKeys))
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
	case m.mode == ModeClose && m.closeTree != nil && m.width < 80:
		content = lipgloss.JoinVertical(lipgloss.Left, renderCloseTree(m, m.width, bodyHeight), footer)
	case m.mode == ModeClose && m.closeTree != nil:
		// Close tree beside the preview of what the cursor's close would
		// reopen. No sub-manifest column: it restated the tree.
		closes := renderCloseTree(m, listWidth, bodyHeight)
		preview := m.renderPreview(previewWidth)
		body := lipgloss.JoinHorizontal(lipgloss.Top, closes, preview)
		content = lipgloss.JoinVertical(lipgloss.Left, body, footer)
	case m.width < 80:
		list := renderList(m, listWidth, bodyHeight)
		content = lipgloss.JoinVertical(lipgloss.Left, list, footer)
	case m.mode == ModeClose:
		list := renderList(m, listWidth, bodyHeight)
		tree := renderTree(m, m.width-listWidth, bodyHeight)
		body := lipgloss.JoinHorizontal(lipgloss.Top, list, tree)
		content = lipgloss.JoinVertical(lipgloss.Left, body, footer)
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

// renderFooter renders the footer bar with toggle indicators, pane counter, and
// an optional transient warning note. Format: `key:value` pairs in lavender +
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

	c := m.CurrentCounts()
	counter := fmt.Sprintf("%d panes / %d skipped", c.KeptPanes, c.SkippedPanes)

	parts := []string{
		toggle(m.filter.SkipIdleShells, m.keys.ToggleIdle),
		toggle(m.filter.SkipRunningSessions, m.keys.ToggleSkipRunning),
		toggle(m.dimOlderThan > 0, m.keys.ToggleAge),
		counter,
		hint(m.keys.Enter),
	}
	if m.mode == ModeClose {
		// The close tree has no other affordance advertising that a
		// collapsed header can be opened — without this hint prefix+U can
		// open on a single "▸ other sessions" row with no clue how to see
		// inside it.
		parts = append(parts, hint(m.keys.Right))
	}
	// Tab reaches snapshot mode's sub-manifest tree; close mode has no second
	// tree, and its preview scrolls with Alt+j/k regardless of focus.
	_, _, previewW := m.paneWidthsThree()
	if previewW > 0 {
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

// stacksPanel reports whether the map/scrollback panel goes under the tree
// rather than beside it — the case for a terminal too narrow for a third
// column. The popup is 90% of the client, so that threshold lands near 120
// columns. Close mode only ever has two columns, so it never stacks.
func (m PickerModel) stacksPanel() bool {
	if m.mode == ModeClose {
		return false
	}
	return m.width >= 80 && m.width < 120
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
		// Two columns: no sub-manifest pane, since it restated the hierarchy
		// the close tree already draws. 40% keeps the tree past its 32-cell
		// floor for deep guide prefixes and long session labels, and leaves
		// the preview at least 30 cells — enough for a pane map and the
		// restore sentence under it. Both bounds hold automatically once
		// width>=80 (the guard above): 2w/5>=32 and w-2w/5>=30, so no clamp
		// is needed here — only if that threshold ever moves does one become
		// necessary again.
		treeW := m.width * 2 / 5
		return treeW, 0, m.width - treeW
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

// renderCloseTree renders the grouped close hierarchy into the list pane.
// One physical row per visible node: every row is truncated to the frame's
// inner width — guide prefix included — because a lipgloss frame wraps
// overflow instead of clipping it, which would desync scrollWindow.
func renderCloseTree(m PickerModel, width, height int) string {
	frame := listFrame.Width(width).Height(height).MaxHeight(height)
	vis := m.CloseVisible()
	if len(vis) == 0 {
		msg := "No close events yet."
		if m.hiddenCount > 0 {
			msg = fmt.Sprintf("No recoverable closes (%d hidden).", m.hiddenCount)
		}
		return frame.Render(rowDim.Render(msg))
	}

	innerWidth := width - listFrame.GetHorizontalFrameSize()
	if innerWidth < 1 {
		innerWidth = 1
	}
	rows := height - 2
	if rows < 1 {
		rows = 1
	}
	showFooter := m.hiddenCount > 0 && rows > 1
	nodeRows := rows
	if showFooter {
		nodeRows--
	}
	start, end := scrollWindow(m.cursor, len(vis), nodeRows)

	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(closeRow(vis[i], innerWidth, i == m.cursor))
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	if showFooter {
		for pad := end - start; pad < nodeRows; pad++ {
			b.WriteString("\n")
		}
		b.WriteString("\n")
		text := ansi.Truncate(fmt.Sprintf("— %s hidden —", hiddenPhrase(m.hiddenCount)), innerWidth, "…")
		b.WriteString(rowDim.Width(innerWidth).Align(lipgloss.Center).Render(text))
	}
	return frame.Render(b.String())
}

// closeMarker precedes every restorable row. Scaffolding gets the same two
// cells of blank so labels stay aligned down the column.
const closeMarker = "● "

// closeRow renders one tree row: guide prefix, expand marker, restore marker,
// label, a parenthesised state tag on scaffolding, and a right-aligned
// timestamp for event rows.
func closeRow(n *CloseNode, innerWidth int, active bool) string {
	left := closeGuidePrefix(n)
	switch {
	case len(n.Children) > 0 && n.Expanded:
		left += "▾ "
	case len(n.Children) > 0:
		left += "▸ "
	}
	if !IsCloseGroup(n) {
		if n.EventID != 0 {
			left += closeMarker
		} else {
			left += strings.Repeat(" ", lipgloss.Width(closeMarker))
		}
	}
	left += n.Label
	if n.State != "" {
		left += " (" + n.State + ")"
	}

	right := ""
	if n.Ts != 0 {
		right = time.UnixMilli(n.Ts).Format("15:04")
	}
	// Reserve the timestamp plus one separating space, then pad the gap.
	budget := innerWidth
	if right != "" {
		budget -= len(right) + 1
	}
	if budget < 1 {
		budget = 1
	}
	left = ansi.Truncate(left, budget, "…")
	line := left
	if right != "" {
		if gap := innerWidth - lipgloss.Width(left) - len(right); gap > 0 {
			line += strings.Repeat(" ", gap)
		} else {
			line += " "
		}
		line += right
	}
	line = ansi.Truncate(line, innerWidth, "…")

	if active {
		// One flat style: lipgloss v2 strips ESC bytes from pre-styled input,
		// so nesting a role color inside rowActive's background can collapse to
		// invisible. Same reason appendNodeRows renders the active row plain.
		return rowActive.Width(innerWidth).Render(line)
	}
	return closeRowStyle(n).Width(innerWidth).Render(line)
}

// closeRowStyle returns the single flat style for a non-cursor close row.
// Scaffolding is separated from restorable rows by foreground alone — never by
// Faint or Italic, which the picker reserves for age.
func closeRowStyle(n *CloseNode) lipgloss.Style {
	if IsCloseGroup(n) {
		return previewHeader
	}
	if n.EventID == 0 {
		return rowScaffold
	}
	switch n.Kind {
	case CSession:
		return nodeSession
	case CWindow:
		return nodeWindow
	}
	return nodePane
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
	idx := 0
	var rows []string
	for _, sess := range tree.Children {
		appendNodeRows(&rows, sess, 0, &idx, highlightIdx)
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
func appendNodeRows(rows *[]string, n *TreeNode, depth int, idx *int, highlightIdx int) {
	indent := strings.Repeat("  ", depth)
	bullet := "•"
	if len(n.Children) > 0 {
		if n.Expanded {
			bullet = "▾"
		} else {
			bullet = "▸"
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
			line = line + "  (" + n.SkipReason + ")"
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
			styled = styled + "  " + skipReason.Render("("+n.SkipReason+")")
		}
		rendered = fmt.Sprintf("%s%s %s", indent, bullet, styled)
	}
	*rows = append(*rows, rendered)
	*idx++
	if n.Expanded {
		for _, c := range n.Children {
			appendNodeRows(rows, c, depth+1, idx, highlightIdx)
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
