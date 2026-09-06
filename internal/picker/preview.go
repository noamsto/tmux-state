package picker

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/tmux-remux/internal/panemap"
	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
)

// structuralANSI matches CSI sequences whose final byte is NOT 'm' (i.e.,
// everything except SGR color/style). These cursor-movement, erase, scroll,
// bracketed-paste-mode toggles confuse lipgloss when embedded in framed
// content — strip them but keep the SGR codes so previews stay colored.
var structuralANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-ln-~]`)

// oscANSI matches OSC sequences (\x1b]…\x07 or \x1b]…\x1b\). These set
// terminal title / hyperlinks; they also break frame rendering when raw.
var oscANSI = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// otherESC matches stray ESC-then-single-letter sequences (e.g., ESC=, ESC>,
// ESC(B) that escape lipgloss's parser.
var otherESC = regexp.MustCompile(`\x1b[()=>NMc78]`)

// sanitizeScrollback removes non-SGR escape sequences and control characters
// (NUL, BS, CR, VT, FF) that would otherwise break preview-frame rendering.
// SGR colors (\x1b[...m) and tab are left intact.
func sanitizeScrollback(s string) string {
	s = oscANSI.ReplaceAllString(s, "")
	s = structuralANSI.ReplaceAllString(s, "")
	s = otherESC.ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"\x00", "", "\x08", "", "\x0b", "", "\x0c", "", "\r", "",
	).Replace(s)
	return s
}

// scrollbackLoadedMsg is emitted by loadScrollbackCmd when the scrollback read
// completes (successfully or not). The model handles it by populating the
// scrollback cache and refreshing the viewport if the cursor still points at
// the same SHA.
type scrollbackLoadedMsg struct {
	sha     string
	content []byte
	err     error
}

// renderPreview renders the right-most preview pane. width is the cell budget
// (including the rounded border). Height comes from m.height.
func (m PickerModel) renderPreview(width int) string {
	if m.hasCloseUI() {
		return m.renderClosePreview(width)
	}
	frameHeight := m.panelFrameHeight()
	innerHeight := m.previewInnerHeight()
	innerWidth := width - 4
	if innerWidth < 1 {
		innerWidth = 1
	}
	frame := previewFrame.Width(width).Height(frameHeight).MaxHeight(frameHeight)

	if m.focus != focusTree {
		return frame.Render(rowDim.Render("(press Tab to preview panes)"))
	}
	nodes := m.visibleNodes()
	if m.treeCursor < 0 || m.treeCursor >= len(nodes) {
		return frame.Render(rowDim.Render("(no pane selected)"))
	}
	n := nodes[m.treeCursor]
	if n.Kind == NodeWindow {
		if w, ok := n.Ref.(*snapshot.Window); ok {
			if art := m.renderWindowMap(w, innerWidth, innerHeight); art != "" {
				return frame.Render(art)
			}
		}
	}
	if n.Kind != NodePane {
		// Reachable after Left collapses to a window/session node, and for a
		// window whose layout predates layout capture.
		return frame.Render(rowDim.Render("(press → to expand, ↑↓ to find a pane)"))
	}
	p, _ := n.Ref.(*snapshot.Pane)
	if p == nil || p.ScrollbackSHA == "" {
		if p != nil && m.manifests[m.CurrentEventID()].ScrollbackSkipped {
			return frame.Render(rowDim.Render("(scrollback skipped — saved within min_save_interval)"))
		}
		return frame.Render(rowDim.Render("(no scrollback captured for this pane)"))
	}
	sha := p.ScrollbackSHA
	if err := m.scrollbackErrors[sha]; err != nil {
		return frame.Render(footerWarn.Render("(scrollback file missing: " + err.Error() + ")"))
	}
	content, ok := m.scrollbacks[sha]
	if !ok {
		if m.loadingSHAs[sha] {
			return frame.Render(rowDim.Render("(loading scrollback…)"))
		}
		// Not loading yet — PreviewCmd will schedule on next key event.
		return frame.Render(rowDim.Render("(scrollback pending)"))
	}
	if m.paneHintShows() {
		w, _ := n.Parent.Ref.(*snapshot.Window) // paneHintShows verified the type
		hint := m.paneContextMap(w, p.ID, innerWidth)
		sep := rowDim.Render(strings.Repeat("─", innerWidth))
		sb := previewWindow(string(content), innerWidth, m.paneScrollbackHeight(), m.previewScroll, m.previewScrollX)
		return frame.Render(hint + "\n" + sep + "\n" + sb)
	}
	return frame.Render(previewWindow(string(content), innerWidth, innerHeight, m.previewScroll, m.previewScrollX))
}

// closeRail is the gutter glyph drawn down the left edge of every content row
// of a pane's block. Captured scrollback prints its own horizontal rules and
// box-drawn status bars, so chrome that is only a thin rule reads as content;
// the rail is trustworthy instead because content is inset past it and
// truncated to the remaining width, and so can never reach that column.
const closeRail = "▌"

// closeHeaderLines is the height of the close preview's header. Fixed, so the
// body's budget can be computed without rendering it.
const closeHeaderLines = 2

// renderClosePreview draws the panel for whatever close row the cursor is on:
// a two-line header naming the close, then the scrollback of every pane it
// took down, stacked one block per pane.
func (m PickerModel) renderClosePreview(width int) string {
	frameHeight := m.panelFrameHeight()
	innerHeight := m.previewInnerHeight()
	innerWidth := width - 4
	if innerWidth < 1 {
		innerWidth = 1
	}
	frame := previewFrame.Width(width).Height(frameHeight).MaxHeight(frameHeight)

	var cc CloseContext
	var ts int64
	if m.usesCloseRows() {
		if m.cursor < 0 || m.cursor >= len(m.closeRows) {
			return frame.Render(rowDim.Render("(nothing selected)"))
		}
		r := m.closeRows[m.cursor]
		if !r.Selectable() {
			return frame.Render(rowDim.Render("(a section — ↑↓ to reach a close)"))
		}
		cc, ts = m.CloseContextFor(r.EventID), r.Ts
	} else {
		vis := m.CloseVisible()
		if m.cursor < 0 || m.cursor >= len(vis) {
			return frame.Render(rowDim.Render("(nothing selected)"))
		}
		n := vis[m.cursor]
		if n.EventID == 0 {
			return frame.Render(rowDim.Render("(a section — ↑↓ to reach a close)"))
		}
		cc, ts = m.CloseContextFor(n.EventID), n.Ts
	}

	// A lipgloss frame pads short content but does not clip overflow — once the
	// body has more lines than fit, MaxHeight hard-truncates the line list,
	// dropping the closing border row rather than the excess. Hence the body
	// only ever gets what the header leaves, and the header itself is clipped:
	// the flat list stacks the panel under it below closeSideBySideMin, where
	// a short terminal can leave fewer than the header's two rows.
	lines := closePreviewHeader(cc, innerWidth, time.Now(), ts)
	if len(lines) > innerHeight {
		lines = lines[:innerHeight]
	}
	lines = append(lines, m.closePreviewBody(cc, innerWidth, innerHeight-len(lines))...)
	return frame.Render(strings.Join(lines, "\n"))
}

// closePreviewHeader names what this close was and when it happened, e.g.
//
//	halo-nix-amd-ai:1 · nix-amd-ai
//	window close · 2 panes · closed 2d ago
func closePreviewHeader(cc CloseContext, innerWidth int, now time.Time, ts int64) []string {
	p := cc.Placement
	title := p.Session
	if p.Scope != "session" {
		title = fmt.Sprintf("%s:%d · %s", p.Session, p.WindowIndex, snapshot.StripFormat(p.WindowName))
	}
	size := countPhrase(1, "pane")
	switch p.Scope {
	case "session":
		size = countPhrase(countWindows(cc.SubManifest), "window")
	case "window":
		n := p.PaneCount
		if w := closePreviewWindow(cc); n == 0 && w != nil {
			n = len(w.Panes)
		}
		size = countPhrase(n, "pane")
	}
	sub := strings.Join([]string{p.Scope + " close", size, "closed " + humanAge(now.Sub(time.UnixMilli(ts)))}, " · ")
	return []string{
		previewHeader.Render(ansi.Truncate(title, innerWidth, "…")),
		rowDim.Render(ansi.Truncate(sub, innerWidth, "…")),
	}
}

// closePreviewBody stacks one block per pane the close took down into height
// rows, or says so when the close captured nothing.
func (m PickerModel) closePreviewBody(cc CloseContext, innerWidth, height int) []string {
	if height < 1 {
		return nil
	}
	w := closePreviewWindow(cc)
	if w == nil || len(w.Panes) == 0 {
		return []string{rowDim.Render(ansi.Truncate("(nothing captured for this close)", innerWidth, "…"))}
	}
	heights := closeBlockHeights(len(w.Panes), height)
	if len(heights) == 0 {
		// One row left: a label bar alone still says whose output is missing.
		return []string{closePaneLabel(w.Panes[0], 0, len(w.Panes), innerWidth)}
	}
	var out []string
	for i, h := range heights {
		out = append(out, m.closePaneBlock(w.Panes[i], i, len(w.Panes), innerWidth, h)...)
	}
	return out
}

// closeBlockHeights divides body rows between the panes a close took down,
// giving every block a label bar plus at least one content row. Panes past
// what fits get no block — the label bar's "k of n" marker is what says the
// list is longer than the panel.
func closeBlockHeights(panes, body int) []int {
	blocks := panes
	if fits := body / 2; blocks > fits {
		blocks = fits
	}
	if blocks < 1 {
		return nil
	}
	base, extra := body/blocks, body%blocks
	out := make([]int, blocks)
	for i := range out {
		out[i] = base
		if i < extra {
			out[i]++
		}
	}
	return out
}

// closePaneBlock renders one pane's slot: its label bar, then exactly height-1
// content rows each led by the block's rail.
func (m PickerModel) closePaneBlock(p snapshot.Pane, i, total, innerWidth, height int) []string {
	rail := closeRailStyles[i%len(closeRailStyles)].Render(closeRail)
	contentWidth := innerWidth - 1
	if contentWidth < 1 {
		contentWidth = 1
	}
	out := make([]string, 0, height)
	out = append(out, closePaneLabel(p, i, total, innerWidth))
	for _, l := range m.closePaneContent(p, contentWidth, height-1) {
		// Concatenated, never nested: lipgloss strips the ESC bytes out of
		// pre-styled input, so wrapping coloured scrollback in the rail's
		// style would flatten it.
		out = append(out, rail+l)
	}
	return out
}

// closePaneLabel is the filled bar introducing a pane's block: which pane it
// was, and where it sits in the run of panes this close took down.
func closePaneLabel(p snapshot.Pane, i, total, innerWidth int) string {
	cmd := p.Command
	if cmd == "" {
		cmd = "(none)"
	}
	left := fmt.Sprintf("%d · %s", p.Index, cmd)
	right := fmt.Sprintf("%d of %d", i+1, total)
	line := left
	if gap := innerWidth - lipgloss.Width(left) - lipgloss.Width(right); gap >= 1 {
		line = left + strings.Repeat(" ", gap) + right
	}
	return closeLabelStyles[i%len(closeLabelStyles)].Width(innerWidth).Render(ansi.Truncate(line, innerWidth, "…"))
}

// closePaneContent returns exactly height rows of a pane's scrollback, or the
// one-line reason there is none, padded so the block's rail runs its full
// height. Every row is already cut to width: the caller prefixes the rail and
// content must not be able to reach that column.
func (m PickerModel) closePaneContent(p snapshot.Pane, width, height int) []string {
	if height < 1 {
		return nil
	}
	note := func(style lipgloss.Style, text string) []string {
		return []string{style.Render(ansi.Truncate(text, width, "…"))}
	}
	var lines []string
	switch sha := p.ScrollbackSHA; {
	case sha == "":
		lines = note(rowDim, "(no scrollback captured for this pane)")
	case m.scrollbackErrors[sha] != nil:
		lines = note(footerWarn, "(scrollback file missing: "+m.scrollbackErrors[sha].Error()+")")
	default:
		content, ok := m.scrollbacks[sha]
		switch {
		case ok:
			lines = strings.Split(previewWindow(string(content), width, height, m.previewScroll, m.previewScrollX), "\n")
		case m.loadingSHAs[sha]:
			lines = note(rowDim, "(loading scrollback…)")
		default:
			// PreviewCmd schedules on the next key event.
			lines = note(rowDim, "(scrollback pending)")
		}
	}
	out := make([]string, height)
	copy(out, lines)
	return out
}

// closePreviewWindow returns the window the preview should draw: the one the
// placement names, or the sub-manifest's first window for a session close,
// where every window came down and the first one stands for the rest.
//
// The two session names are independently sourced — Placement.Session from the
// tmux hook, the sub-manifest's from the prior snapshot — and can disagree when
// the session was renamed in between (see closeevent.OwnerSession's fallback
// chain), so a name mismatch falls back to ignoring the name. Only with exactly
// one session in the sub-manifest, though: past that, ignoring it could draw a
// window belonging to a session other than the one that closed.
func closePreviewWindow(cc CloseContext) *snapshot.Window {
	if w := closePreviewWindowIn(cc, true); w != nil {
		return w
	}
	if len(cc.SubManifest.Sessions) != 1 {
		return nil
	}
	return closePreviewWindowIn(cc, false)
}

// closePreviewWindowIn searches cc.SubManifest.Sessions for the placement's
// window. With matchSession, a session whose name differs from
// cc.Placement.Session is skipped — window indexes are only unique within a
// session, so a sub-manifest carrying more than one needs the name to pick
// the right one.
func closePreviewWindowIn(cc CloseContext, matchSession bool) *snapshot.Window {
	for i := range cc.SubManifest.Sessions {
		s := &cc.SubManifest.Sessions[i]
		if matchSession && cc.Placement.Session != "" && s.Name != cc.Placement.Session {
			continue
		}
		for j := range s.Windows {
			if cc.Placement.Scope == "session" {
				return &s.Windows[j]
			}
			if s.Windows[j].Index == cc.Placement.WindowIndex {
				return &s.Windows[j]
			}
		}
	}
	return nil
}

// previewWindow returns the slice of scrollback to display: structural ANSI
// removed (so cursor moves and erase codes don't break the lipgloss frame),
// each logical line horizontally windowed to [scrollX, scrollX+width) and the
// vertical window offset `scroll` lines from the tail. SGR color escapes are
// preserved through ansi.Cut so the preview stays colored where possible.
func previewWindow(s string, width, height, scroll, scrollX int) string {
	if height <= 0 {
		return ""
	}
	cleaned := sanitizeScrollback(s)
	raw := strings.Split(strings.TrimRight(cleaned, "\n"), "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		if scrollX > 0 {
			// ansi.Cut can overshoot its upper bound by one cell when a
			// double-width rune straddles it; re-truncating clamps that back
			// to width without touching the common case where Cut already
			// landed exactly on budget.
			l = ansi.Truncate(ansi.Cut(l, scrollX, scrollX+width), width, "")
		} else {
			l = ansi.Truncate(l, width, "")
		}
		lines[i] = l
	}
	end := len(lines) - scroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end], "\n")
}

// previewInnerHeight is the number of scrollback rows the preview pane shows:
// the panel's frame height minus its border. Single source of truth for
// renderPreview and the scroll-clamp math in Update/handleKey, which otherwise
// drifted apart at very small terminal heights and in the stacked layout.
func (m PickerModel) previewInnerHeight() int {
	if inner := m.panelFrameHeight() - 2; inner > 1 {
		return inner
	}
	return 1
}

// previewMaxScroll returns the largest valid m.previewScroll for the current
// pane's scrollback. Used to clamp Alt+K scroll-up at the top of the buffer.
func (m PickerModel) previewMaxScroll(innerHeight int) int {
	if m.hasCloseUI() {
		return m.closeMaxScroll()
	}
	sha := m.previewSHA()
	if sha == "" {
		return 0
	}
	content, ok := m.scrollbacks[sha]
	if !ok {
		return 0
	}
	total := strings.Count(strings.TrimRight(string(content), "\n"), "\n") + 1
	if total <= innerHeight {
		return 0
	}
	return total - innerHeight
}

// closeMaxScroll is the largest valid previewScroll for the stacked close
// preview. One scroll offset drives every block, so the limit is the tallest
// overflow across them — stopping at the shortest would strand the deepest
// pane's earliest output out of reach.
func (m PickerModel) closeMaxScroll() int {
	cc := m.closeCursorContext()
	w := closePreviewWindow(cc)
	if w == nil {
		return 0
	}
	worst := 0
	for i, h := range closeBlockHeights(len(w.Panes), m.previewInnerHeight()-closeHeaderLines) {
		content, ok := m.scrollbacks[w.Panes[i].ScrollbackSHA]
		if !ok {
			continue
		}
		total := strings.Count(strings.TrimRight(string(content), "\n"), "\n") + 1
		if over := total - (h - 1); over > worst {
			worst = over
		}
	}
	return worst
}

// loadScrollbackCmd returns a tea.Cmd that reads the scrollback for sha off
// the UI goroutine. Returns nil if sb is nil or sha is empty (caller short-
// circuits and never schedules a load).
func loadScrollbackCmd(sb *scrollback.Store, sha string) tea.Cmd {
	if sb == nil || sha == "" {
		return nil
	}
	return func() tea.Msg {
		rc, err := sb.Stream(context.Background(), sha)
		if err != nil {
			return scrollbackLoadedMsg{sha: sha, err: err}
		}
		defer func() { _ = rc.Close() }()
		buf, err := io.ReadAll(rc)
		return scrollbackLoadedMsg{sha: sha, content: buf, err: err}
	}
}

// renderWindowMap draws w's pane layout, titled with the window. Returns "" when
// the layout is absent or unparsable so the caller can fall back to its hint.
func (m PickerModel) renderWindowMap(w *snapshot.Window, innerWidth, innerHeight int) string {
	g, err := panemap.Parse(w.Layout)
	if err != nil {
		return ""
	}
	title := fmt.Sprintf("%d: %s  (%d×%d)", w.Index, snapshot.StripFormat(w.Name), g.W, g.H)
	// The layout string names each pane by its tmux id (%N), not its pane index —
	// the two diverge whenever pane-base-index is set or a pane has been closed.
	// So the map's box number is a pane id; look the pane up by id and label it
	// with its friendlier pane index and command.
	label := func(idx int) string {
		if p := paneByID(w, idx); p != nil {
			return fmt.Sprintf("%d %s", p.Index, p.Command)
		}
		return ""
	}
	place := m.CloseContextFor(m.CurrentEventID()).Placement
	marked := func(idx int) bool {
		switch place.Scope {
		case "":
			return false // snapshot mode: nothing died
		case "pane":
			p := paneByID(w, idx)
			return p != nil && p.ID == place.PaneID
		default:
			return true // window or session close: all of it came down
		}
	}
	art := panemap.Render(g, innerWidth, innerHeight-1, label, marked)
	return rowDim.Render(ansi.Truncate(title, innerWidth, "…")) + "\n" + art
}

// humanAge renders a duration as the coarsest unit that still reads true.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// countPhrase renders "1 pane" / "2 panes".
func countPhrase(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// countWindows totals the windows a manifest would restore.
func countWindows(man snapshot.Manifest) int {
	n := 0
	for _, s := range man.Sessions {
		n += len(s.Windows)
	}
	return n
}

// paneMapHintHeight is the box-art height of the mini-map shown above a pane's
// scrollback. panemap needs minMapHeight (6) to draw art rather than a summary.
const paneMapHintHeight = 7

// paneHintShows reports whether the focused pane's preview leads with a mini-map
// of its window. It gates on a parsable layout and enough panel height to keep a
// useful amount of scrollback below the map. renderPreview and the scroll-clamp
// math both consult it so the visible scrollback height they assume agrees; the
// preview column is never narrower than the map's minimum, so when it shows the
// strip is always paneMapHintHeight rows plus one separator.
func (m PickerModel) paneHintShows() bool {
	// Snapshot mode only. Close mode renders its own preview and never reaches
	// the mini-map, but it still carries a treeCursor no key drives — without
	// this guard a stale cursor landing on a pane node would silently charge
	// close mode for a strip it does not draw.
	if m.mode != ModeSnapshot {
		return false
	}
	if m.previewInnerHeight() < paneMapHintHeight+1+6 { // map + separator + scrollback
		return false
	}
	nodes := m.visibleNodes()
	if m.treeCursor < 0 || m.treeCursor >= len(nodes) {
		return false
	}
	n := nodes[m.treeCursor]
	if n.Kind != NodePane {
		return false
	}
	w, ok := n.Parent.Ref.(*snapshot.Window)
	if !ok {
		return false
	}
	_, err := panemap.Parse(w.Layout)
	return err == nil
}

// paneScrollbackHeight is the rows the scrollback occupies in the preview: the
// panel's inner height, less the mini-map strip when paneHintShows.
func (m PickerModel) paneScrollbackHeight() int {
	h := m.previewInnerHeight()
	if m.paneHintShows() {
		h -= paneMapHintHeight + 1
	}
	if h < 1 {
		h = 1
	}
	return h
}

// paneContextMap draws w's layout with the focused pane (focusedID) marked, for
// the strip above a pane's scrollback — "you are here" context while reading the
// output. Callers gate on paneHintShows first.
func (m PickerModel) paneContextMap(w *snapshot.Window, focusedID string, width int) string {
	g, err := panemap.Parse(w.Layout)
	if err != nil {
		return ""
	}
	label := func(idx int) string {
		p := paneByID(w, idx)
		if p == nil {
			return ""
		}
		if p.ID == focusedID {
			return fmt.Sprintf("▸%d %s", p.Index, p.Command)
		}
		return fmt.Sprintf("%d %s", p.Index, p.Command)
	}
	return panemap.Render(g, width, paneMapHintHeight, label, nil)
}

// paneByID returns the pane whose tmux id is "%<n>", the handle the layout
// string uses to name each pane. Returns nil if no such pane is in the window.
func paneByID(w *snapshot.Window, n int) *snapshot.Pane {
	id := "%" + strconv.Itoa(n)
	for i := range w.Panes {
		if w.Panes[i].ID == id {
			return &w.Panes[i]
		}
	}
	return nil
}
