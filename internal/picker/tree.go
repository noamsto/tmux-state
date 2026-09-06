// Package picker renders a Bubble Tea TUI over tmux-remux events.
//
// Width arithmetic here has one trap worth knowing before adding a call site.
// Of the truncation primitives this package uses, only ansi.Truncate is safe:
// measured over one probe set, ansi.Truncate never exceeded its budget (0/90
// cases), while ansi.TruncateLeft (10/36) and ansi.Cut (6/36) both overshoot by
// a cell when a double-width rune straddles the cut. Anything computed from the
// result — a pad count, a remaining budget — can then go negative on CJK or
// emoji input, which panics in strings.Repeat and otherwise drops a lipgloss
// frame's closing border. Both existing call sites clamp with ansi.Truncate; a
// new one must too.
package picker

import (
	"fmt"
	"os"
	"strings"

	"github.com/noamsto/tmux-remux/internal/filter"
	"github.com/noamsto/tmux-remux/internal/snapshot"
)

// NodeKind identifies the level of a TreeNode.
type NodeKind int

// Node-kind constants identify the level of each TreeNode.
const (
	NodeSession NodeKind = iota
	NodeWindow
	NodePane
)

// TreeNode is one entry in the rendered tree. BuildTree returns the root with
// Kind=NodeSession at depth 0; the spec mockup wraps these in a synthetic
// "Snapshot #N" header which lives in the View, not here.
type TreeNode struct {
	Kind       NodeKind
	Label      string    // display label without skip styling
	Ref        any       // *snapshot.Session | *snapshot.Window | *snapshot.Pane
	Parent     *TreeNode // nil for root; set by BuildTree
	Children   []*TreeNode
	Expanded   bool   // initial state set by BuildTree (sessions+windows true, panes false)
	Skipped    bool   // set by FilterDecorate
	SkipReason string // "idle shell" | "running" | "" (empty when kept)
}

// Counts is what FilterDecorate returns so the View can render
// "<KeptPanes> panes / <SkippedPanes> skipped" in the footer.
type Counts struct {
	KeptSessions, KeptWindows, KeptPanes          int
	SkippedSessions, SkippedWindows, SkippedPanes int
}

// BuildTree converts a manifest into a virtual root whose Children are session
// nodes. The root itself has Kind=NodeSession and Label="" — callers iterate
// root.Children rather than rendering the root.
func BuildTree(m snapshot.Manifest) *TreeNode {
	root := &TreeNode{Kind: NodeSession, Expanded: true}
	for i := range m.Sessions {
		s := &m.Sessions[i]
		sessionNode := &TreeNode{
			Kind:     NodeSession,
			Label:    sessionLabel(s),
			Ref:      s,
			Parent:   root,
			Expanded: true,
		}
		for j := range s.Windows {
			w := &s.Windows[j]
			windowNode := &TreeNode{
				Kind:     NodeWindow,
				Label:    windowLabel(w),
				Ref:      w,
				Parent:   sessionNode,
				Expanded: true,
			}
			for k := range w.Panes {
				p := &w.Panes[k]
				windowNode.Children = append(windowNode.Children, &TreeNode{
					Kind:     NodePane,
					Label:    paneLabel(p),
					Ref:      p,
					Parent:   windowNode,
					Expanded: false,
				})
			}
			sessionNode.Children = append(sessionNode.Children, windowNode)
		}
		root.Children = append(root.Children, sessionNode)
	}
	return root
}

func sessionLabel(s *snapshot.Session) string {
	return fmt.Sprintf("%s (%dw)", snapshot.StripFormat(s.Name), len(s.Windows))
}

func windowLabel(w *snapshot.Window) string {
	return fmt.Sprintf("%d: %s (%dp)", w.Index, snapshot.StripFormat(w.Name), len(w.Panes))
}

func paneLabel(p *snapshot.Pane) string {
	cwd := p.Cwd
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	cmd := p.Command
	if cmd == "" {
		cmd = "(none)"
	}
	// Lead with the pane index so a tree row matches the numbered box in the map.
	return fmt.Sprintf("%d %-7s %s", p.Index, cmd, cwd)
}

// countPanes returns the number of NodePane descendants of n. Only called on
// a session auto-collapsed by the running-session filter, where every
// descendant is filtered too — it doesn't account for a subtree with a mix
// of shown and filtered panes.
func countPanes(n *TreeNode) int {
	count := 0
	for _, c := range n.Children {
		if c.Kind == NodePane {
			count++
		} else {
			count += countPanes(c)
		}
	}
	return count
}

// FilterDecorate walks the tree and marks each node Skipped/SkipReason based on
// the filter predicates. It mutates the tree in place. Returns counts for the
// footer counter. The runningSessions argument is consulted only when
// f.SkipRunningSessions is true.
func FilterDecorate(root *TreeNode, f filter.Filter, runningSessions map[string]bool) Counts {
	var c Counts
	if root == nil {
		return c
	}
	for _, sess := range root.Children {
		s, _ := sess.Ref.(*snapshot.Session)
		if s == nil {
			continue
		}
		wasRunning := sess.SkipReason == "running"
		sess.Skipped = false
		sess.SkipReason = ""
		if reason := f.SessionSkipReason(*s, runningSessions); reason != "" {
			sess.Skipped = true
			sess.SkipReason = reason
			c.SkippedSessions++
		} else {
			c.KeptSessions++
		}
		sessionSkipped := sess.Skipped
		// Collapse already-running sessions so they read as a single
		// "name (running)" line; only on the transition, so a manual
		// re-expand survives later decorate passes.
		isRunning := sess.SkipReason == "running"
		if isRunning != wasRunning {
			sess.Expanded = !isRunning
		}

		for _, win := range sess.Children {
			w, _ := win.Ref.(*snapshot.Window)
			if w == nil {
				continue
			}
			win.Skipped = false
			win.SkipReason = ""
			// A window is "skipped" if its session is skipped OR every pane in it
			// would be skipped. filter.Filter.SkipWindow already encodes the
			// all-panes-idle check; we OR with session-skipped here.
			windowSkipped := sessionSkipped || f.SkipWindow(*w)
			if windowSkipped {
				win.Skipped = true
				if sessionSkipped {
					win.SkipReason = sess.SkipReason
				} else {
					win.SkipReason = "all panes idle"
				}
				c.SkippedWindows++
			} else {
				c.KeptWindows++
			}

			for _, pane := range win.Children {
				p, _ := pane.Ref.(*snapshot.Pane)
				if p == nil {
					continue
				}
				pane.Skipped = false
				pane.SkipReason = ""
				paneSkipped := windowSkipped || f.SkipPane(*p)
				if paneSkipped {
					pane.Skipped = true
					switch {
					case sessionSkipped:
						pane.SkipReason = sess.SkipReason
					case f.SkipPane(*p):
						pane.SkipReason = "idle shell"
					default:
						pane.SkipReason = "all panes idle"
					}
					c.SkippedPanes++
				} else {
					c.KeptPanes++
				}
			}
		}
	}
	return c
}
