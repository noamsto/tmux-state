// Package closeevent records tmux close hooks (pane/window/session) as events
// and resolves them against pre-close snapshots for undo/restore.
package closeevent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

const dedupWindow = 2000 * time.Millisecond

// Args bundles the parameters of a tmux close hook.
type Args struct {
	Kind        string // "pane-died" | "window-unlinked" | "session-closed"
	SessionID   string
	SessionName string
	WindowID    string
	PaneID      string
	Host        string
	// Index is the live tmux structure queried AFTER the close (the closed
	// entity is already gone when the hook fires). Empty when the server is
	// unreachable — i.e. the last session closed and nothing survived.
	Index IndexPost
}

// Capture inserts a close event into the store unless a fresh outer-scope
// event for the same session exists (cascade dedup). Returns the inserted
// event id, or 0 if deduped.
func Capture(ctx context.Context, db *store.Store, a Args) (int64, error) {
	// after-kill-pane is a command hook, so it carries no hook_pane: a pane
	// killed with prefix+x used to leave no trace at all. Recover its id by
	// diffing the survivors against the last snapshot instead.
	if a.Kind == "pane-died" && a.PaneID == "" {
		resolved, ok, err := resolveKilledPane(ctx, db, a)
		if err != nil || !ok {
			return 0, err
		}
		a = resolved
	}

	// window-unlinked also fires on move-window, where the window survives under
	// another session. When the closed entity's id is still in the post-close
	// index nothing was lost, so drop it at the source rather than storing a row
	// the picker can only render as "no recoverable entity".
	if entityStillLive(a) {
		return 0, nil
	}

	now := time.Now().UnixMilli()
	cutoff := now - dedupWindow.Milliseconds()

	if a.Kind != "session-closed" {
		evs, err := db.ListEvents(ctx, store.ListOpts{
			Kinds: []string{"session-closed"},
			Limit: 5,
		})
		if err != nil {
			return 0, err
		}
		for _, ev := range evs {
			if ev.Ts >= cutoff && eventReferencesSession(ev.ManifestJSON, a.SessionID) {
				return 0, nil
			}
		}
	}
	if a.Kind == "pane-died" {
		evs, err := db.ListEvents(ctx, store.ListOpts{
			Kinds: []string{"window-unlinked"},
			Limit: 5,
		})
		if err != nil {
			return 0, err
		}
		for _, ev := range evs {
			if ev.Ts >= cutoff && eventReferencesWindow(ev.ManifestJSON, a.SessionID, a.WindowID) {
				return 0, nil
			}
		}
	}

	man := CloseManifest{
		SessionID:   a.SessionID,
		SessionName: a.SessionName,
		WindowID:    a.WindowID,
		PaneID:      a.PaneID,
		Index:       a.Index,
	}
	man.Resolved = resolveAtCapture(ctx, db, a, man)

	wrapped, err := json.Marshal(man)
	if err != nil {
		return 0, err
	}

	id, err := db.InsertEvent(ctx, store.Event{
		Ts:           now,
		Kind:         a.Kind,
		Scope:        scopeFor(a.Kind),
		Reason:       "hook",
		Host:         a.Host,
		ManifestJSON: string(wrapped),
	})
	if err != nil {
		return 0, err
	}

	linkResolvedScrollback(ctx, db, id, man.Resolved)
	return id, nil
}

// resolveAtCapture embeds the closed entity in the event at capture time so a
// later read no longer needs a surviving snapshot to resolve it. Returns nil
// whenever embedding isn't safe, leaving the event on the snapshot-diff path.
func resolveAtCapture(ctx context.Context, db *store.Store, a Args, man CloseManifest) *ResolvedClose {
	idKnown := (a.Kind == "pane-died" && a.PaneID != "") || (a.Kind == "window-unlinked" && a.WindowID != "")
	if !idKnown {
		// session-closed is excluded: findClosedSession isn't id-aware and, when
		// no name matches, guesses the sole remaining candidate anyway. Re-derived
		// per read that guess decays as other sessions close; embedded it would be
		// permanent.
		return nil
	}

	snap, err := db.LatestSnapshot(ctx)
	if err != nil || snap == nil {
		return nil
	}
	var prior snapshot.Manifest
	if json.Unmarshal([]byte(snap.ManifestJSON), &prior) != nil {
		return nil
	}

	// findClosedWindow/findClosedPane only refuse to guess inside their id
	// branch when the prior is also id-aware; an old, id-less snapshot falls
	// through to a positional session:index fallback that, under
	// renumber-windows, can return a SURVIVING entity instead of the closed one.
	switch a.Kind {
	case "pane-died":
		if !priorHasPaneIDs(prior) {
			return nil
		}
	case "window-unlinked":
		if !priorHasWindowIDs(prior) {
			return nil
		}
	}

	item := FindClosed(prior, man, a.Kind)
	if item == nil {
		return nil
	}
	return &ResolvedClose{Item: *item, SavedAt: prior.SavedAt}
}

// linkResolvedScrollback links the embedded entity's panes to the event so
// their scrollback blobs survive the source snapshot being pruned. Failures
// are swallowed: the event row is already committed by this point, and
// capture-event runs from a `run-shell -b` tmux hook, so surfacing an error
// here would print a tmux-remux: error: banner on an ordinary pane close that
// actually succeeded — e.g. a gc racing between the snapshot read and this
// link trips the scrollback_sha foreign key.
func linkResolvedScrollback(ctx context.Context, db *store.Store, id int64, resolved *ResolvedClose) {
	if resolved == nil {
		return
	}
	item := resolved.Item
	// The pane key includes the window index, so a missing window leaves
	// nothing to link regardless of which branch item.Pane took.
	if item.Window == nil {
		return
	}
	panes := item.Window.Panes
	if item.Pane != nil {
		panes = []snapshot.Pane{*item.Pane}
	}
	for _, p := range panes {
		if p.ScrollbackSHA == "" {
			continue
		}
		key := fmt.Sprintf("%s:%d:%d", item.SessionName, item.Window.Index, p.Index)
		_ = db.LinkEventScrollback(ctx, id, key, p.ScrollbackSHA)
	}
}

// resolveKilledPane fills in the pane and window ids of an id-less pane-died
// event by finding the pane the latest snapshot knows about but the post-close
// index no longer lists. It reports false unless exactly one pane went missing
// AND its window survived: more than one missing pane means the snapshot is
// stale or a whole window/session came down (which window-unlinked and
// session-closed already record), and guessing there would restore the wrong
// pane. Recording nothing beats recording a lie.
func resolveKilledPane(ctx context.Context, db *store.Store, a Args) (Args, bool, error) {
	snap, err := db.LatestSnapshot(ctx)
	if err != nil || snap == nil {
		return a, false, err
	}
	var prior snapshot.Manifest
	if json.Unmarshal([]byte(snap.ManifestJSON), &prior) != nil {
		return a, false, nil
	}

	live := map[string]bool{}
	for _, p := range a.Index.Panes {
		live[p.ID] = true
	}
	liveWindows := map[string]bool{}
	for _, w := range a.Index.Windows {
		liveWindows[w.ID] = true
	}

	missing := 0
	lost := a
	for i := range prior.Sessions {
		for j := range prior.Sessions[i].Windows {
			w := &prior.Sessions[i].Windows[j]
			for k := range w.Panes {
				p := &w.Panes[k]
				if p.ID == "" || live[p.ID] {
					continue
				}
				missing++
				lost.PaneID, lost.WindowID = p.ID, w.ID
			}
		}
	}
	if missing != 1 || !liveWindows[lost.WindowID] {
		return a, false, nil
	}
	return lost, true, nil
}

// entityStillLive reports whether the close event's target still appears in the
// post-close index, meaning it was not actually closed (e.g. a moved window).
// Mirrors the id-still-present checks in findClosedWindow/findClosedPane.
func entityStillLive(a Args) bool {
	switch a.Kind {
	case "window-unlinked":
		if a.WindowID == "" {
			return false
		}
		for _, w := range a.Index.Windows {
			if w.ID == a.WindowID {
				return true
			}
		}
	case "pane-died":
		if a.PaneID == "" {
			return false
		}
		for _, p := range a.Index.Panes {
			if p.ID == a.PaneID {
				return true
			}
		}
	}
	return false
}

func scopeFor(kind string) string {
	switch kind {
	case "session-closed":
		return "session"
	case "window-unlinked":
		return "window"
	default:
		return "pane"
	}
}

type envelope struct {
	SessionID string `json:"session_id"`
	WindowID  string `json:"window_id"`
}

func eventReferencesSession(manifest, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	var e envelope
	if json.Unmarshal([]byte(manifest), &e) != nil {
		return false
	}
	return e.SessionID == sessionID
}

func eventReferencesWindow(manifest, sessionID, windowID string) bool {
	if windowID == "" {
		return false
	}
	var e envelope
	if json.Unmarshal([]byte(manifest), &e) != nil {
		return false
	}
	return e.SessionID == sessionID && e.WindowID == windowID
}
