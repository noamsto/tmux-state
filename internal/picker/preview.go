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
	if m.mode == ModeClose && m.closeTree != nil {
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
	return frame.Render(previewWindow(string(content), innerWidth, innerHeight, m.previewScroll, m.previewScrollX))
}

// renderClosePreview draws the panel for whatever close row the cursor is on.
// Unlike snapshot mode there is no second tree to focus first: prefix+U opens
// with this already showing.
func (m PickerModel) renderClosePreview(width int) string {
	frameHeight := m.panelFrameHeight()
	innerHeight := m.previewInnerHeight()
	innerWidth := width - 4
	if innerWidth < 1 {
		innerWidth = 1
	}
	frame := previewFrame.Width(width).Height(frameHeight).MaxHeight(frameHeight)

	vis := m.CloseVisible()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return frame.Render(rowDim.Render("(nothing selected)"))
	}
	n := vis[m.cursor]
	if n.EventID == 0 {
		return frame.Render(rowDim.Render("(a section — ↑↓ to reach a close)"))
	}

	cc := m.CloseContextFor(n.EventID)
	if sha := closeSHAFor(cc); sha != "" {
		if err := m.scrollbackErrors[sha]; err != nil {
			return frame.Render(footerWarn.Render("(scrollback file missing: " + err.Error() + ")"))
		}
		if content, ok := m.scrollbacks[sha]; ok {
			return frame.Render(previewWindow(string(content), innerWidth, innerHeight, m.previewScroll, m.previewScrollX))
		}
		if m.loadingSHAs[sha] {
			return frame.Render(rowDim.Render("(loading scrollback…)"))
		}
		// The pane has scrollback but no load is in flight. Say so rather than
		// falling through to the map, which would silently change content type
		// once a load lands.
		return frame.Render(rowDim.Render("(scrollback pending)"))
	}
	w := closePreviewWindow(cc)
	sentence := restoreSentence(cc.Placement, cc.SubManifest, time.Now(), n.Ts)
	// A lipgloss frame pads short content but does not clip overflow — once the
	// body already has more lines than fit, MaxHeight hard-truncates the line
	// list, which drops the closing border row rather than the excess content.
	// So the body must never exceed innerHeight lines. The sentence is the only
	// thing that says what Enter would do; the map (or its "nothing captured"
	// stand-in) is decoration. So when the frame is too short for both, drop
	// that decoration first, and truncate the sentence itself only once even
	// that alone does not fit.
	if len(sentence) > innerHeight {
		sentence = sentence[:innerHeight]
	}
	remaining := innerHeight - len(sentence)
	var lines []string
	switch {
	case w == nil:
		if remaining >= 1 {
			lines = []string{rowDim.Render("(nothing captured for this close)")}
		}
	case remaining >= 2:
		// renderWindowMap always emits a title line plus at least one map line
		// (art, or its own "no layout" fallback), so it needs 2 rows of budget
		// before it's called — at remaining==1 it would overflow by itself.
		if art := m.renderWindowMap(w, innerWidth, remaining); art != "" {
			lines = strings.Split(art, "\n")
		} else {
			lines = []string{rowDim.Render("(no layout captured for this window)")}
		}
	case remaining == 1:
		lines = []string{rowDim.Render("(no layout captured for this window)")}
	}
	for _, line := range sentence {
		lines = append(lines, rowDim.Render(ansi.Truncate(line, innerWidth, "…")))
	}
	return frame.Render(strings.Join(lines, "\n"))
}

// closePreviewWindow returns the window the preview should draw: the one the
// placement names, or the sub-manifest's first window for a session close,
// where every window came down and the first one stands for the rest.
//
// It prefers the session named by the placement, then falls back to dropping
// the name filter — but only when the sub-manifest holds exactly one session,
// so there is no other candidate it could mean. The two names are
// independently sourced — Placement.Session from the tmux hook, the
// sub-manifest's from the prior snapshot — and can disagree when the session
// was renamed in between (see closeevent.OwnerSession's fallback chain);
// treating the name as a hard filter turned that mismatch into a blank
// preview instead of a degraded one. With more than one session in the
// sub-manifest, dropping the filter would instead risk drawing a window that
// belongs to a different session than the one that closed.
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
			l = ansi.Cut(l, scrollX, scrollX+width)
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

// restoreSentence states what Enter on this close would do, in the terms the
// restore path actually honours: a window goes back to its original index, so
// naming the index here is a promise rather than a guess.
func restoreSentence(p ClosePlacement, man snapshot.Manifest, now time.Time, ts int64) []string {
	var out []string
	switch p.Scope {
	case "window":
		out = append(out,
			"↵ reopens window "+snapshot.StripFormat(p.WindowName),
			fmt.Sprintf("  in %s at index %d", p.Session, p.WindowIndex),
		)
	case "pane":
		out = append(out, fmt.Sprintf("↵ reopens a pane in %s:%d", p.Session, p.WindowIndex))
	case "session":
		out = append(out, fmt.Sprintf("↵ reopens session %s (%s)", p.Session, countPhrase(countWindows(man), "window")))
	default:
		return nil
	}

	age := "closed " + humanAge(now.Sub(time.UnixMilli(ts)))
	if p.PaneCount > 0 {
		age = countPhrase(p.PaneCount, "pane") + " · " + age
	}
	return append(out, age)
}

// humanAge renders a duration as the coarsest unit that still reads true.
// The row already carries the wall-clock time; this is the "and how long ago
// was that" the wall clock cannot answer once a close is a day old.
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
