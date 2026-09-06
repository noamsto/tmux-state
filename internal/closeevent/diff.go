package closeevent

import (
	"encoding/json"
	"fmt"

	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/tmux"
)

// CloseManifest is the unmarshalled close-event ManifestJSON payload.
// Capture writes it via:
//
//	{"session_id":..., "window_id":..., "pane_id":..., "index": {...}}
//
// where `index` is the live-index of tmux's structure AFTER the close has
// happened (whatever survived).
type CloseManifest struct {
	SessionID string `json:"session_id"`
	// SessionName is the session the closed entity belonged to, captured from
	// #{hook_session_name} at hook time. Empty on events recorded before it was
	// stored, and on after-kill-pane events (a command hook gets no hook_*
	// formats) — both fall back to the snapshot diff for attribution.
	SessionName string    `json:"session_name,omitempty"`
	WindowID    string    `json:"window_id"`
	PaneID      string    `json:"pane_id"`
	Index       IndexPost `json:"index"`
}

// IndexPost holds tmux's list-windows and list-panes output queried right
// after the close hook fired — the surviving structure.
type IndexPost struct {
	Windows []tmux.WindowRow `json:"windows"`
	Panes   []tmux.PaneRow   `json:"panes"`
}

// ParseManifest unmarshals an events.manifest_json string for a close event.
// Returns an empty CloseManifest if the JSON is empty or invalid (callers
// treat both cases the same — close event is no longer actionable).
func ParseManifest(s string) (CloseManifest, error) {
	var m CloseManifest
	if s == "" {
		return m, nil
	}
	err := json.Unmarshal([]byte(s), &m)
	return m, err
}

// ClosedItem identifies the entity lost in a close event, recovered by diffing
// the most recent snapshot from before the event against the event's
// post-close index. SessionName is always populated (root of the lost entity).
// A session-closed sets Session; a window-unlinked sets Window; a pane-died
// sets Pane (the lost pane) AND Window (its parent, needed to split it back in
// or recreate the window).
type ClosedItem struct {
	Session     *snapshot.Session
	Window      *snapshot.Window
	Pane        *snapshot.Pane
	SessionName string
	WindowIndex int // 0 when the whole session was closed
}

// Describe returns a short human label like
//
//	"lazytmux/main 🧠 (1p)"      — for window-unlinked
//	"session: scratch-foo (3w)" — for session-closed
//	"pane: nvim in lazytmux/2"  — for pane-died
//
// Returns "(unrecoverable)" when c is nil (caller couldn't recover the
// entity, e.g., no prior snapshot exists).
func (c *ClosedItem) Describe() string {
	if c == nil {
		return "(unrecoverable)"
	}
	// Pane is checked before Window: a pane-died carries both, and the pane
	// label is the specific one.
	switch {
	case c.Session != nil:
		return fmt.Sprintf("session: %s (%dw)", c.SessionName, len(c.Session.Windows))
	case c.Pane != nil:
		cmd := c.Pane.Command
		if cmd == "" {
			cmd = "(none)"
		}
		return fmt.Sprintf("pane: %s in %s/%d", cmd, c.SessionName, c.WindowIndex)
	case c.Window != nil:
		return fmt.Sprintf("%s/%s (%dp)", c.SessionName, snapshot.StripFormat(c.Window.Name), len(c.Window.Panes))
	}
	return "(unrecoverable)"
}

// SubManifest builds a snapshot.Manifest containing only the closed entity,
// suitable for restore.BuildPlan. A pane close yields its enclosing window
// carrying just the pane that died — the window is what a restore needs to
// place the pane back into, but its surviving siblings were not lost.
// Returns an empty manifest if c is nil.
func (c *ClosedItem) SubManifest(host string, savedAt int64) snapshot.Manifest {
	m := snapshot.Manifest{V: 1, Host: host, SavedAt: savedAt}
	if c == nil {
		return m
	}
	// Pane is checked before Window: a pane-died carries both, and only the
	// pane that died was lost — its siblings are still running, so listing
	// them here would report them as casualties of this close.
	switch {
	case c.Session != nil:
		m.Sessions = []snapshot.Session{*c.Session}
	case c.Pane != nil:
		w := *c.Window
		w.Panes = []snapshot.Pane{*c.Pane}
		m.Sessions = []snapshot.Session{{Name: c.SessionName, Windows: []snapshot.Window{w}}}
	case c.Window != nil:
		m.Sessions = []snapshot.Session{{
			Name:    c.SessionName,
			Windows: []snapshot.Window{*c.Window},
		}}
	}
	return m
}

// FindClosed diffs `prior` (the snapshot taken just before the close event)
// against the close event's post-close index to identify what was lost. The
// `kind` argument is the event Kind ("session-closed" | "window-unlinked" |
// "pane-died"). Returns nil if the diff is ambiguous or empty.
func FindClosed(prior snapshot.Manifest, post CloseManifest, kind string) *ClosedItem {
	switch kind {
	case "session-closed":
		return findClosedSession(prior, post)
	case "window-unlinked":
		return findClosedWindow(prior, post)
	case "pane-died":
		return findClosedPane(prior, post)
	}
	return nil
}

func findClosedSession(prior snapshot.Manifest, post CloseManifest) *ClosedItem {
	live := map[string]bool{}
	for _, w := range post.Index.Windows {
		live[w.Session] = true
	}
	var missing []*snapshot.Session
	for i := range prior.Sessions {
		s := &prior.Sessions[i]
		if !live[s.Name] {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// The event's own session name pins the entity when several sessions are
	// absent from the index — a stale snapshot, or closes in a row — where the
	// first-missing scan would hand every one of them the same session.
	if post.SessionName != "" {
		for _, s := range missing {
			if s.Name == post.SessionName {
				return &ClosedItem{Session: s, SessionName: s.Name}
			}
		}
		// A named session the snapshot never captured (born and gone inside a
		// snapshot gap) is only attributable when one candidate remains — then
		// it is unambiguous, and a rename explains the mismatch. With several,
		// any pick is a guess, and a wrong one silently restores a different
		// session's windows under its own name; report it unrecoverable so the
		// picker hides it rather than offering a row it cannot honour.
		if len(missing) > 1 {
			return nil
		}
	}
	// Events recorded before the name was stored have nothing better to go on.
	return &ClosedItem{Session: missing[0], SessionName: missing[0].Name}
}

// priorHasWindowIDs reports whether the snapshot records window ids at all.
// Old snapshots (pre-id) leave them empty, in which case id-based matching is
// meaningless and the positional fallback is the only option. A single save
// writes ids all-or-none, so "any id present" reliably means "id-aware".
func priorHasWindowIDs(prior snapshot.Manifest) bool {
	for i := range prior.Sessions {
		for j := range prior.Sessions[i].Windows {
			if prior.Sessions[i].Windows[j].ID != "" {
				return true
			}
		}
	}
	return false
}

// priorHasPaneIDs reports whether the snapshot records pane ids at all.
func priorHasPaneIDs(prior snapshot.Manifest) bool {
	for i := range prior.Sessions {
		for j := range prior.Sessions[i].Windows {
			for k := range prior.Sessions[i].Windows[j].Panes {
				if prior.Sessions[i].Windows[j].Panes[k].ID != "" {
					return true
				}
			}
		}
	}
	return false
}

func findClosedWindow(prior snapshot.Manifest, post CloseManifest) *ClosedItem {
	// ID match first: the event records the dying window's id, which pins the
	// exact entity when several windows closed since the prior snapshot (a
	// session:index diff would return whichever is missing first). A window
	// whose id still appears in the post-close index wasn't closed at all —
	// window-unlinked also fires on move-window.
	if post.WindowID != "" {
		for _, w := range post.Index.Windows {
			if w.ID == post.WindowID {
				return nil
			}
		}
		for i := range prior.Sessions {
			s := &prior.Sessions[i]
			for j := range s.Windows {
				w := &s.Windows[j]
				if w.ID == post.WindowID {
					return &ClosedItem{Window: w, SessionName: s.Name, WindowIndex: w.Index}
				}
			}
		}
		// The event recorded an id and the prior snapshot is id-aware but
		// doesn't hold it: the window was born and gone within a snapshot gap,
		// so it was never captured — unrecoverable. Falling through to the
		// positional fallback would wrongly grab some other window that closed
		// in the same gap.
		if priorHasWindowIDs(prior) {
			return nil
		}
	}
	// Fallback for snapshots from before ids were recorded: first missing wins.
	live := map[string]bool{}
	for _, w := range post.Index.Windows {
		live[fmt.Sprintf("%s:%d", w.Session, w.Index)] = true
	}
	for i := range prior.Sessions {
		s := &prior.Sessions[i]
		for j := range s.Windows {
			w := &s.Windows[j]
			if !live[fmt.Sprintf("%s:%d", s.Name, w.Index)] {
				return &ClosedItem{Window: w, SessionName: s.Name, WindowIndex: w.Index}
			}
		}
	}
	return nil
}

func findClosedPane(prior snapshot.Manifest, post CloseManifest) *ClosedItem {
	// Same id-first strategy as findClosedWindow.
	if post.PaneID != "" {
		for _, p := range post.Index.Panes {
			if p.ID == post.PaneID {
				return nil
			}
		}
		for i := range prior.Sessions {
			s := &prior.Sessions[i]
			for j := range s.Windows {
				w := &s.Windows[j]
				for k := range w.Panes {
					p := &w.Panes[k]
					if p.ID == post.PaneID {
						return &ClosedItem{Pane: p, Window: w, SessionName: s.Name, WindowIndex: w.Index}
					}
				}
			}
		}
		// Id-aware prior without this pane: born and gone within a snapshot
		// gap, never captured (see findClosedWindow).
		if priorHasPaneIDs(prior) {
			return nil
		}
	}
	live := map[string]bool{}
	for _, p := range post.Index.Panes {
		live[fmt.Sprintf("%s:%d:%d", p.Session, p.WindowIndex, p.PaneIndex)] = true
	}
	for i := range prior.Sessions {
		s := &prior.Sessions[i]
		for j := range s.Windows {
			w := &s.Windows[j]
			for k := range w.Panes {
				p := &w.Panes[k]
				key := fmt.Sprintf("%s:%d:%d", s.Name, w.Index, p.Index)
				if !live[key] {
					return &ClosedItem{Pane: p, Window: w, SessionName: s.Name, WindowIndex: w.Index}
				}
			}
		}
	}
	return nil
}
