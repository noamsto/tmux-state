package picker

import (
	"sort"

	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/store"
)

// CloseRowKind identifies what a CloseRow represents in the flat list.
type CloseRowKind int

// Close-row kinds. RowSectionHeader introduces a section; RowClose is one
// close event (or a collapsed run of duplicates).
const (
	RowSectionHeader CloseRowKind = iota
	RowClose
)

// CloseRow is one row of the flat close list.
type CloseRow struct {
	Kind      CloseRowKind
	Section   string // header text; empty on a close row
	EventID   int64  // 0 on a header
	Ts        int64  // newest timestamp in the group
	Count     int    // 1, or N for a collapsed group
	Scope     string // "session" | "window" | "pane"
	Session   string
	Placement ClosePlacement
}

// Selectable reports whether r can be navigated to and restored. Section
// headers are structural only.
func (r CloseRow) Selectable() bool {
	return r.Kind == RowClose
}

const sectionOther = "OTHER SESSIONS"

// sectionThis returns the "this session" header text for current.
func sectionThis(current string) string {
	return "THIS SESSION · " + current
}

// collapseKey identifies closes a user would read as one repeated event.
// cmd and cwd describe the closed pane and are scope-defined: a session
// close has no single closed pane — the whole session, however it was
// recreated and closed again, is what's repeating — so they're left zero,
// which lets every close of the same session collapse regardless of what
// that incarnation happened to contain.
type collapseKey struct {
	scope, session string
	windowIndex    int
	cmd, cwd       string
}

// closedPaneInfo returns the command and cwd of the pane a close event took
// down, for use as part of a collapseKey. For a pane-scope close it looks up
// the died pane by Placement.PaneID rather than assuming it's first in the
// window, since the sub-manifest also carries the died pane's siblings. For
// a window-scope close it uses the window's first pane — the representative
// case, since 3+-pane windows don't occur in practice. A session-scope close
// has no single closed pane and always returns the zero value.
func closedPaneInfo(cc CloseContext) (cmd, cwd string) {
	if cc.Placement.Scope == "session" {
		return "", ""
	}
	for _, s := range cc.SubManifest.Sessions {
		for _, w := range s.Windows {
			if cc.Placement.Scope == "pane" && cc.Placement.PaneID != "" {
				for _, p := range w.Panes {
					if p.ID == cc.Placement.PaneID {
						return p.Command, p.Cwd
					}
				}
				continue
			}
			if len(w.Panes) > 0 {
				return w.Panes[0].Command, w.Panes[0].Cwd
			}
		}
	}
	return "", ""
}

// closeGroup accumulates one collapsed row as later duplicates are folded in.
type closeGroup struct {
	row   CloseRow
	count int
}

// BuildCloseList flattens recoverable close events into a two-section,
// newest-first list: THIS SESSION (the current session's own closes, save
// its own session-scope closes — see below) then OTHER SESSIONS. Each
// section header is emitted only when it has at least one close under it.
//
// Events with an empty SubManifest.Sessions are excluded, exactly as
// BuildCloseTree does today — the caller counts them as hidden rather than
// listing a dead row.
//
// evs is expected newest-first (store.ListEvents orders by ts DESC), but the
// result does not depend on that — every section is sorted by Ts at the end.
func BuildCloseList(evs []store.Event, ctxs map[int64]CloseContext, current string, _ map[string]bool) []CloseRow {
	var thisGroups, otherGroups []*closeGroup
	thisIdx := map[collapseKey]*closeGroup{}
	otherIdx := map[collapseKey]*closeGroup{}

	for _, ev := range evs {
		cc, ok := ctxs[ev.ID]
		if !ok || len(cc.SubManifest.Sessions) == 0 {
			continue
		}
		p := cc.Placement
		name := p.Session
		if name == "" {
			name = closeevent.UnknownSession
		}
		// A session close means that session is not the one we are sitting
		// in, so it always belongs to OTHER SESSIONS — matches BuildCloseTree.
		mine := current != "" && name == current && p.Scope != "session"

		cmd, cwd := closedPaneInfo(cc)
		key := collapseKey{scope: p.Scope, session: name, windowIndex: p.WindowIndex, cmd: cmd, cwd: cwd}

		idx, groups := otherIdx, &otherGroups
		if mine {
			idx, groups = thisIdx, &thisGroups
		}
		if g, ok := idx[key]; ok {
			g.count++
			if ev.Ts > g.row.Ts {
				g.row.Ts, g.row.EventID = ev.Ts, ev.ID
			}
			continue
		}
		g := &closeGroup{count: 1, row: CloseRow{
			Kind: RowClose, EventID: ev.ID, Ts: ev.Ts, Scope: p.Scope, Session: name, Placement: p,
		}}
		idx[key] = g
		*groups = append(*groups, g)
	}

	out := make([]CloseRow, 0, len(thisGroups)+len(otherGroups)+2)
	if len(thisGroups) > 0 {
		out = append(out, CloseRow{Kind: RowSectionHeader, Section: sectionThis(current)})
		out = append(out, finishCloseGroups(thisGroups)...)
	}
	if len(otherGroups) > 0 {
		out = append(out, CloseRow{Kind: RowSectionHeader, Section: sectionOther})
		out = append(out, finishCloseGroups(otherGroups)...)
	}
	return out
}

// finishCloseGroups sorts groups newest-first and stamps each row's Count.
func finishCloseGroups(groups []*closeGroup) []CloseRow {
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].row.Ts > groups[j].row.Ts })
	out := make([]CloseRow, len(groups))
	for i, g := range groups {
		g.row.Count = g.count
		out[i] = g.row
	}
	return out
}
