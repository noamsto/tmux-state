// Command proto-layout is a THROWAWAY PROTOTYPE. It answers one design
// question for the close picker (prefix+U): does a folded tree or a flat
// cwd-primary list read better against real close history? It opens a copy of
// the user's real store read-only, runs the same close pipeline the picker
// does, and prints the list two ways. Not production code — no tests, no error
// handling beyond what makes it run. Delete once the layout is chosen.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/picker"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

func main() {
	layout := flag.String("layout", "tree", "tree|flat")
	cols := flag.Int("cols", 130, "render width")
	rows := flag.Int("rows", 40, "render height")
	session := flag.String("session", "tmux-remux", "session that counts as \"this session\"")
	flag.Parse()

	ctx := context.Background()
	db := mustOpenCopy(ctx)
	evs, err := db.ListEvents(ctx, store.ListOpts{ExcludeKinds: []string{"snapshot"}, Limit: 50})
	if err != nil {
		fatal(err)
	}
	ctxs := buildCloseContexts(ctx, db, evs)
	kept := evs[:0]
	for _, ev := range evs {
		if len(ctxs[ev.ID].SubManifest.Sessions) > 0 {
			kept = append(kept, ev)
		}
	}
	hidden := len(evs) - len(kept)
	evs = kept

	initStyles()
	live := liveSessions(ctx)

	switch *layout {
	case "tree":
		fmt.Print(renderTree(evs, ctxs, *session, live, *cols, *rows, hidden))
	case "flat":
		fmt.Print(renderFlat(evs, ctxs, *session, live, *cols, *rows, hidden))
	default:
		fatal(fmt.Errorf("unknown layout %q", *layout))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "proto-layout:", err)
	os.Exit(1)
}

// mustOpenCopy copies the real store to /tmp and opens the copy, so the
// prototype's migrations and WAL files never touch the user's database.
func mustOpenCopy(ctx context.Context) *store.Store {
	src := filepath.Join(os.Getenv("HOME"), ".local/share/tmux-remux/state.db")
	data, err := os.ReadFile(src) // #nosec G304,G703 -- prototype, path is the fixed store location
	if err != nil {
		fatal(err)
	}
	dst := "/tmp/proto-layout.db"
	if err := os.WriteFile(dst, data, 0o600); err != nil { // #nosec G703 -- fixed /tmp path
		fatal(err)
	}
	db, err := store.Open(ctx, dst)
	if err != nil {
		fatal(err)
	}
	return db
}

func liveSessions(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	b, err := exec.CommandContext(ctx, "tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return out
	}
	for _, name := range strings.Fields(string(b)) {
		out[name] = true
	}
	return out
}

// buildCloseContexts / placementFor mirror cmd/tmux-remux's unexported
// versions; the prototype cannot import package main.
func buildCloseContexts(ctx context.Context, db *store.Store, evs []store.Event) map[int64]picker.CloseContext {
	out := make(map[int64]picker.CloseContext, len(evs))
	priorCache := map[int64]snapshot.Manifest{}
	for _, ev := range evs {
		closeMan, err := closeevent.ParseManifest(ev.ManifestJSON)
		if err != nil {
			continue
		}
		prior, ok := priorCache[ev.Ts]
		if !ok {
			snap, err := db.LatestSnapshotBefore(ctx, ev.Ts)
			if err != nil || snap == nil {
				priorCache[ev.Ts] = snapshot.Manifest{}
				continue
			}
			if err := json.Unmarshal([]byte(snap.ManifestJSON), &prior); err != nil {
				priorCache[ev.Ts] = snapshot.Manifest{}
				continue
			}
			priorCache[ev.Ts] = prior
		}
		item := closeevent.FindClosed(prior, closeMan, ev.Kind)
		if item == nil {
			continue
		}
		out[ev.ID] = picker.CloseContext{
			Label:       item.Describe(),
			Placement:   placementFor(closeMan, item),
			SubManifest: item.SubManifest(prior.Host, prior.SavedAt),
		}
	}
	return out
}

func placementFor(closeMan closeevent.CloseManifest, item *closeevent.ClosedItem) picker.ClosePlacement {
	p := picker.ClosePlacement{Session: closeevent.OwnerSession(closeMan, item)}
	if p.Session == closeevent.UnknownSession {
		p.Session = ""
	}
	p.PaneID = closeMan.PaneID
	switch {
	case item.Session != nil:
		p.Scope = "session"
	case item.Pane != nil:
		p.Scope = "pane"
		p.WindowIndex, p.WindowName = item.WindowIndex, item.Window.Name
	case item.Window != nil:
		p.Scope = "window"
		p.WindowIndex, p.WindowName = item.WindowIndex, item.Window.Name
		p.PaneCount = len(item.Window.Panes)
	}
	return p
}

// ---------------------------------------------------------------- shared data

// firstPane returns the pane a close event's sub-manifest leads with, which is
// where the prototype reads cwd and command from.
func firstPane(cc picker.CloseContext) (snapshot.Pane, bool) {
	for _, s := range cc.SubManifest.Sessions {
		for _, w := range s.Windows {
			if len(w.Panes) > 0 {
				return w.Panes[0], true
			}
		}
	}
	return snapshot.Pane{}, false
}

func paneCount(cc picker.CloseContext) int {
	n := 0
	for _, s := range cc.SubManifest.Sessions {
		for _, w := range s.Windows {
			n += len(w.Panes)
		}
	}
	return n
}

func windowCount(cc picker.CloseContext) int {
	n := 0
	for _, s := range cc.SubManifest.Sessions {
		n += len(s.Windows)
	}
	return n
}

// identity is the collapse key: two closes are the same close when they agree
// on session, window index, command and cwd.
func identity(cc picker.CloseContext) string {
	p, _ := firstPane(cc)
	return fmt.Sprintf("%s\x1f%d\x1f%s\x1f%s", cc.Placement.Session, cc.Placement.WindowIndex, p.Command, p.Cwd)
}

func tildify(path string) string {
	home := os.Getenv("HOME")
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// truncLeft keeps the tail of s, which is where a worktree path's
// discriminating segments live.
func truncLeft(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-(width-1):])
}

func relAge(ts int64) string {
	d := time.Since(time.UnixMilli(ts))
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	}
}

// cell renders one flat-list column: truncated to leave a space before the
// next column, then padded out to the full width.
func cell(s string, width int) string {
	return pad(ansi.Truncate(s, width-1, "…"), width)
}

func pad(s string, width int) string {
	s = ansi.Truncate(s, width, "…")
	if gap := width - lipgloss.Width(s); gap > 0 {
		s += strings.Repeat(" ", gap)
	}
	return s
}

// ------------------------------------------------------------------- styling

// The picker's styles are package-private, so the prototype rebuilds the ones
// it needs from the same exported Theme.
var (
	sHeader, sSession, sWindow, sPane, sScaffold, sDim, sActive, sFrame lipgloss.Style
)

func initStyles() {
	t := picker.NewTheme()
	sHeader = lipgloss.NewStyle().Foreground(t.Blue()).Bold(true)
	sSession = lipgloss.NewStyle().Foreground(t.Mauve()).Bold(true)
	sWindow = lipgloss.NewStyle().Foreground(t.Blue())
	sPane = lipgloss.NewStyle().Foreground(t.Text())
	sScaffold = lipgloss.NewStyle().Foreground(t.Subtext())
	sDim = lipgloss.NewStyle().Foreground(t.Overlay())
	sActive = lipgloss.NewStyle().Foreground(t.Base()).Background(t.Mauve()).Bold(true)
	sFrame = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(t.Surface1()).Padding(0, 1)
}

// -------------------------------------------------------------- variant: tree

var inTailRE = regexp.MustCompile(`\s+in\s+\S+$`)

func renderTree(evs []store.Event, ctxs map[int64]picker.CloseContext, current string, live map[string]bool, cols, rows, hidden int) string {
	root := picker.BuildCloseTree(evs, ctxs, current, live)
	for _, g := range root.Children {
		collapseDuplicates(g, ctxs)
	}
	annotate(root, ctxs)
	for _, g := range root.Children {
		g.Children = foldChildren(g)
	}
	// Nothing is collapsed in the prototype: show the whole list.
	expandAll(root)

	inner := cols - sFrame.GetHorizontalFrameSize()
	flat := picker.FlattenClose(root)
	lines := make([]string, 0, len(flat))
	for i, n := range flat {
		lines = append(lines, treeRow(n, inner, i == 1))
	}
	return frame(lines, inner, cols, rows, hidden)
}

func expandAll(n *picker.CloseNode) {
	n.Expanded = true
	for _, c := range n.Children {
		expandAll(c)
	}
}

// collapseDuplicates merges sibling rows whose events are the same close,
// keeping the newest and tagging it "×N".
func collapseDuplicates(n *picker.CloseNode, ctxs map[int64]picker.CloseContext) {
	seen := map[string]*picker.CloseNode{}
	counts := map[*picker.CloseNode]int{}
	var kept []*picker.CloseNode
	for _, c := range n.Children {
		collapseDuplicates(c, ctxs)
		if c.EventID == 0 || len(c.Children) > 0 {
			kept = append(kept, c)
			continue
		}
		key := fmt.Sprintf("%d\x1f%s", c.Kind, identity(ctxs[c.EventID]))
		if prev, ok := seen[key]; ok {
			counts[prev]++
			if c.Ts > prev.Ts {
				prev.Ts, prev.EventID = c.Ts, c.EventID
			}
			continue
		}
		seen[key] = c
		counts[c] = 1
		kept = append(kept, c)
	}
	for node, count := range counts {
		if count > 1 {
			node.Label += fmt.Sprintf(" ×%d", count)
		}
	}
	n.Children = kept
}

// annotate strips the redundant "in <session>/<window>" tail from pane labels —
// the tree position already says it — and appends what a row's event contains
// where the label doesn't already show it.
func annotate(n *picker.CloseNode, ctxs map[int64]picker.CloseContext) {
	for _, c := range n.Children {
		annotate(c, ctxs)
	}
	if n.EventID == 0 {
		return
	}
	cc := ctxs[n.EventID]
	switch n.Kind {
	case picker.CPane:
		n.Label = inTailRE.ReplaceAllString(n.Label, "")
	case picker.CSession:
		n.Label += fmt.Sprintf(" · %dw %dp", windowCount(cc), paneCount(cc))
	case picker.CWindow:
		if p, ok := firstPane(cc); ok && paneCount(cc) == 1 {
			n.Label += " · " + p.Command
		}
	}
}

// foldChildren collapses every chain of non-actionable single-child rows into
// the row it exists to indent, joining the labels with " · ".
func foldChildren(parent *picker.CloseNode) []*picker.CloseNode {
	out := make([]*picker.CloseNode, 0, len(parent.Children))
	for _, c := range parent.Children {
		n := c
		for n.EventID == 0 && len(n.Children) == 1 {
			child := n.Children[0]
			child.Label = n.Label + " · " + child.Label
			// Only "gone" is worth carrying down: "live" is the default and
			// would tag nearly every folded row.
			if child.State == "" && n.State == "gone" {
				child.State = n.State
			}
			n = child
		}
		n.Parent = parent
		n.Children = foldChildren(n)
		out = append(out, n)
	}
	return out
}

func treeRow(n *picker.CloseNode, inner int, active bool) string {
	left := guidePrefix(n)
	if !picker.IsCloseGroup(n) {
		if n.EventID != 0 {
			left += "● "
		} else {
			left += "  "
		}
	}
	left += n.Label
	if n.State != "" {
		left += " (" + n.State + ")"
	}
	right := ""
	if n.Ts != 0 {
		right = relAge(n.Ts)
	}
	line := pad(left, inner-len(right)) + right
	if active {
		return sActive.Width(inner).Render(line)
	}
	return treeStyle(n).Width(inner).Render(line)
}

func treeStyle(n *picker.CloseNode) lipgloss.Style {
	switch {
	case picker.IsCloseGroup(n):
		return sHeader
	case n.EventID == 0:
		return sScaffold
	case n.Kind == picker.CSession:
		return sSession
	case n.Kind == picker.CWindow:
		return sWindow
	}
	return sPane
}

func guidePrefix(n *picker.CloseNode) string {
	if n.Parent == nil || picker.IsCloseGroup(n) {
		return ""
	}
	branch := "└─ "
	if laterSibling(n) {
		branch = "├─ "
	}
	out := branch
	for a := n.Parent; a != nil && !picker.IsCloseGroup(a); a = a.Parent {
		seg := "   "
		if laterSibling(a) {
			seg = "│  "
		}
		out = seg + out
	}
	return out
}

func laterSibling(n *picker.CloseNode) bool {
	if n.Parent == nil {
		return false
	}
	sibs := n.Parent.Children
	return len(sibs) > 0 && sibs[len(sibs)-1] != n
}

// -------------------------------------------------------------- variant: flat

type flatRow struct {
	kind    string
	cwd     string
	window  string
	reopen  string
	extra   string
	count   int
	ts      int64
	session string
}

func renderFlat(evs []store.Event, ctxs map[int64]picker.CloseContext, current string, live map[string]bool, cols, rows, hidden int) string {
	var mine, other []*flatRow
	byIdentity := map[string]*flatRow{}
	for _, ev := range evs {
		cc, ok := ctxs[ev.ID]
		if !ok {
			continue
		}
		p := cc.Placement
		name := p.Session
		if name == "" {
			name = closeevent.UnknownSession
		}
		key := fmt.Sprintf("%s\x1f%s", p.Scope, identity(cc))
		if prev, ok := byIdentity[key]; ok {
			prev.count++
			if ev.Ts > prev.ts {
				prev.ts = ev.Ts
			}
			continue
		}

		pane, hasPane := firstPane(cc)
		r := &flatRow{kind: p.Scope, count: 1, ts: ev.Ts, session: name}
		if hasPane {
			r.cwd = tildify(pane.Cwd)
		}
		var extra []string
		if hasPane && pane.Command != "" && pane.Command != "fish" {
			extra = append(extra, pane.Command)
		}
		if n := paneCount(cc); p.Scope != "pane" && n != 1 {
			extra = append(extra, fmt.Sprintf("%dp", n))
		}
		if !live[name] {
			extra = append(extra, "(gone)")
		}
		r.extra = strings.Join(extra, " ")

		switch p.Scope {
		case "session":
			r.reopen = name
			r.window = fmt.Sprintf("%dw", windowCount(cc))
		default:
			r.window = snapshot.StripFormat(p.WindowName)
			r.reopen = fmt.Sprintf("%s:%d", name, p.WindowIndex)
		}
		byIdentity[key] = r
		if name == current && p.Scope != "session" {
			mine = append(mine, r)
		} else {
			other = append(other, r)
		}
	}

	inner := cols - sFrame.GetHorizontalFrameSize()
	narrow := cols <= 80
	var lines []string
	section := func(title string, rs []*flatRow) {
		if len(rs) == 0 {
			return
		}
		lines = append(lines, sHeader.Width(inner).Render(pad(title, inner)))
		for _, r := range rs {
			lines = append(lines, flatLine(r, inner, narrow, current))
		}
	}
	section("THIS SESSION · "+current, mine)
	section("OTHER SESSIONS", other)
	return frame(lines, inner, cols, rows, hidden)
}

func flatLine(r *flatRow, inner int, narrow bool, current string) string {
	kindW, winW, reopenW, cntW, ageW := 8, 17, 20, 4, 4
	if narrow {
		winW, reopenW = 10, 12
	}
	reopen := r.reopen
	if narrow {
		reopen = strings.TrimPrefix(reopen, current+":")
	}
	extraW := 0
	if !narrow {
		extraW = 14
	}
	cwdW := inner - 2 - kindW - winW - extraW - reopenW - cntW - ageW
	if cwdW < 8 {
		cwdW = 8
	}

	count := ""
	if r.count > 1 {
		count = fmt.Sprintf("×%d", r.count)
	}
	var b strings.Builder
	b.WriteString("● ")
	b.WriteString(cell(sDim.Render(r.kind), kindW))
	b.WriteString(cell(sPane.Render(truncLeft(r.cwd, cwdW-1)), cwdW))
	b.WriteString(cell(sWindow.Render(r.window), winW))
	if extraW > 0 {
		b.WriteString(cell(sScaffold.Render(r.extra), extraW))
	}
	b.WriteString(cell(sSession.Render("→ "+reopen), reopenW))
	b.WriteString(cell(sScaffold.Render(count), cntW))
	b.WriteString(pad(sDim.Render(relAge(r.ts)), ageW))
	return ansi.Truncate(b.String(), inner, "…")
}

// ---------------------------------------------------------------- the frame

func frame(lines []string, inner, cols, rows, hidden int) string {
	body := rows - 2
	if hidden > 0 {
		body--
	}
	if len(lines) > body {
		lines = lines[:body]
	}
	if hidden > 0 {
		lines = append(lines, sDim.Width(inner).Align(lipgloss.Center).
			Render(fmt.Sprintf("— %d unrecoverable closes hidden —", hidden)))
	}
	return sFrame.Width(cols).Height(rows-2).MaxHeight(rows).Render(strings.Join(lines, "\n")) + "\n"
}
