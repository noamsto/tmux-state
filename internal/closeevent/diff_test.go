package closeevent_test

import (
	"testing"

	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/tmux"
)

func TestFindClosed_WindowUnlinked(t *testing.T) {
	prior := snapshot.Manifest{
		V:    1,
		Host: "h",
		Sessions: []snapshot.Session{
			{Name: "lazytmux", Windows: []snapshot.Window{
				{Index: 1, Name: "main", Panes: []snapshot.Pane{{Index: 1, Command: "claude", Cwd: "/x"}}},
				{Index: 2, Name: "logs", Panes: []snapshot.Pane{{Index: 1, Command: "fish", Cwd: "/y"}}},
			}},
		},
	}
	post := closeevent.CloseManifest{
		WindowID: "@7",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{
				{Session: "lazytmux", Index: 1, Name: "main"},
			},
		},
	}
	got := closeevent.FindClosed(prior, post, "window-unlinked")
	if got == nil {
		t.Fatal("expected ClosedItem, got nil")
	}
	if got.Window == nil || got.Window.Index != 2 {
		t.Errorf("got Window=%+v, want index=2", got.Window)
	}
	if got.SessionName != "lazytmux" {
		t.Errorf("got SessionName=%q, want lazytmux", got.SessionName)
	}
	if got.Describe() != "lazytmux/logs (1p)" {
		t.Errorf("got Describe()=%q, want lazytmux/logs (1p)", got.Describe())
	}
}

func TestFindClosed_SessionClosed(t *testing.T) {
	prior := snapshot.Manifest{
		V:    1,
		Host: "h",
		Sessions: []snapshot.Session{
			{Name: "lazytmux", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "scratch", Windows: []snapshot.Window{{Index: 1, Name: "main"}, {Index: 2, Name: "logs"}}},
		},
	}
	post := closeevent.CloseManifest{
		SessionID: "scratch",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "lazytmux", Index: 1, Name: "main"}},
		},
	}
	got := closeevent.FindClosed(prior, post, "session-closed")
	if got == nil || got.Session == nil {
		t.Fatal("expected closed session, got nil")
	}
	if got.SessionName != "scratch" {
		t.Errorf("got SessionName=%q, want scratch", got.SessionName)
	}
	want := "session: scratch (2w)"
	if got.Describe() != want {
		t.Errorf("got Describe()=%q, want %q", got.Describe(), want)
	}
}

func TestFindClosed_NoDiff(t *testing.T) {
	prior := snapshot.Manifest{
		Sessions: []snapshot.Session{{Name: "s", Windows: []snapshot.Window{{Index: 1, Name: "w"}}}},
	}
	post := closeevent.CloseManifest{
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "w"}},
		},
	}
	if got := closeevent.FindClosed(prior, post, "window-unlinked"); got != nil {
		t.Errorf("expected nil when nothing was lost, got %+v", got)
	}
}

func TestFindClosed_WindowIDDisambiguatesBurstCloses(t *testing.T) {
	// Two windows closed since the prior snapshot; the event names @3. The
	// first-missing heuristic would wrongly pick @2.
	prior := snapshot.Manifest{
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, Name: "keep", ID: "@1"},
				{Index: 2, Name: "first-closed", ID: "@2"},
				{Index: 3, Name: "second-closed", ID: "@3"},
			}},
		},
	}
	post := closeevent.CloseManifest{
		WindowID: "@3",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "keep", ID: "@1"}},
		},
	}
	got := closeevent.FindClosed(prior, post, "window-unlinked")
	if got == nil || got.Window == nil {
		t.Fatal("expected ClosedItem, got nil")
	}
	if got.Window.ID != "@3" {
		t.Errorf("got window %+v, want ID @3", got.Window)
	}
}

func TestFindClosed_BornInGapWindowIsUnrecoverable(t *testing.T) {
	// The event names @9, but the id-aware prior snapshot never captured it
	// (created and closed within one snapshot gap). Even though @2 also closed
	// in that gap, the positional fallback must NOT grab @2 — @9 is simply
	// unrecoverable.
	prior := snapshot.Manifest{
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, Name: "keep", ID: "@1"},
				{Index: 2, Name: "also-closed", ID: "@2"},
			}},
		},
	}
	post := closeevent.CloseManifest{
		WindowID: "@9",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "keep", ID: "@1"}},
		},
	}
	if got := closeevent.FindClosed(prior, post, "window-unlinked"); got != nil {
		t.Errorf("expected nil for a window born within a snapshot gap, got %+v", got.Window)
	}
}

func TestFindClosed_MovedWindowIsNotClosed(t *testing.T) {
	// window-unlinked also fires on move-window: the window survives under
	// another session, so the event must resolve to nothing.
	prior := snapshot.Manifest{
		Sessions: []snapshot.Session{
			{Name: "a", Windows: []snapshot.Window{{Index: 1, Name: "w", ID: "@5"}}},
		},
	}
	post := closeevent.CloseManifest{
		WindowID: "@5",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "b", Index: 3, Name: "w", ID: "@5"}},
		},
	}
	if got := closeevent.FindClosed(prior, post, "window-unlinked"); got != nil {
		t.Errorf("expected nil for a moved window, got %+v", got)
	}
}

func TestFindClosed_PaneIDDisambiguatesBurstCloses(t *testing.T) {
	prior := snapshot.Manifest{
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, ID: "@1", Panes: []snapshot.Pane{
					{Index: 1, Command: "fish", ID: "%1"},
					{Index: 2, Command: "nvim", ID: "%2"},
				}},
			}},
		},
	}
	post := closeevent.CloseManifest{
		PaneID: "%2",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, ID: "@1"}},
		},
	}
	got := closeevent.FindClosed(prior, post, "pane-died")
	if got == nil || got.Pane == nil {
		t.Fatal("expected ClosedItem, got nil")
	}
	if got.Pane.ID != "%2" {
		t.Errorf("got pane %+v, want ID %%2", got.Pane)
	}
	// The parent window must ride along so the pane can be split back into it.
	if got.Window == nil || got.Window.ID != "@1" {
		t.Errorf("got window %+v, want parent @1", got.Window)
	}
}

func TestSubManifest_RoundTripsForRestore(t *testing.T) {
	item := &closeevent.ClosedItem{
		SessionName: "lazytmux",
		Window: &snapshot.Window{
			Index: 2, Name: "logs",
			Panes: []snapshot.Pane{{Index: 1, Command: "fish", Cwd: "/y"}},
		},
		WindowIndex: 2,
	}
	m := item.SubManifest("h", 100)
	if len(m.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(m.Sessions))
	}
	if m.Sessions[0].Name != "lazytmux" {
		t.Errorf("got session name %q, want lazytmux", m.Sessions[0].Name)
	}
	if len(m.Sessions[0].Windows) != 1 || m.Sessions[0].Windows[0].Index != 2 {
		t.Errorf("got windows %+v, want one window with Index=2", m.Sessions[0].Windows)
	}
}

func TestSubManifest_PaneScopeCarriesOnlyTheDiedPane(t *testing.T) {
	win := &snapshot.Window{
		Index: 2, Name: "logs", ID: "@1",
		Panes: []snapshot.Pane{
			{Index: 1, ID: "%1", Command: "claude", Cwd: "/x"},
			{Index: 2, ID: "%2", Command: "fish", Cwd: "/y"},
		},
	}
	item := &closeevent.ClosedItem{
		SessionName: "lazytmux",
		Pane:        &win.Panes[1],
		Window:      win,
		WindowIndex: 2,
	}
	m := item.SubManifest("h", 100)
	if len(m.Sessions) != 1 || len(m.Sessions[0].Windows) != 1 {
		t.Fatalf("want one session with one window, got %+v", m.Sessions)
	}
	w := m.Sessions[0].Windows[0]
	if w.Index != 2 || w.Name != "logs" {
		t.Errorf("got window %d/%q, want 2/logs", w.Index, w.Name)
	}
	if len(w.Panes) != 1 {
		t.Fatalf("want only the died pane, got %+v", w.Panes)
	}
	if w.Panes[0].ID != "%2" {
		t.Errorf("got pane %q, want the died pane %%2", w.Panes[0].ID)
	}
	// The prior snapshot is shared with the restore path; narrowing must copy.
	if len(win.Panes) != 2 {
		t.Errorf("SubManifest mutated the source window: %+v", win.Panes)
	}
}

func TestFindClosed_SessionClosed_NamedEventPicksItsOwnSession(t *testing.T) {
	// Two sessions are absent from the post-close index — a stale snapshot, or
	// several closes in a row. The event names the second one.
	prior := snapshot.Manifest{
		V:    1,
		Host: "h",
		Sessions: []snapshot.Session{
			{Name: "halo-nix-config", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "noamsto", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "lazytmux", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
		},
	}
	post := closeevent.CloseManifest{
		SessionName: "noamsto",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "lazytmux", Index: 1, Name: "main"}},
		},
	}
	got := closeevent.FindClosed(prior, post, "session-closed")
	if got == nil || got.Session == nil {
		t.Fatal("expected closed session, got nil")
	}
	if got.SessionName != "noamsto" {
		t.Errorf("got SessionName=%q, want noamsto", got.SessionName)
	}
	if got.Session != &prior.Sessions[1] {
		t.Errorf("got Session=%+v, want the noamsto session", got.Session)
	}
}

func TestFindClosed_SessionClosed_UnnamedEventFallsBackToScan(t *testing.T) {
	// Events recorded before SessionName was stored carry no name; the
	// first-missing scan is still the only attribution available.
	prior := snapshot.Manifest{
		V:    1,
		Host: "h",
		Sessions: []snapshot.Session{
			{Name: "halo-nix-config", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "lazytmux", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
		},
	}
	post := closeevent.CloseManifest{
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "lazytmux", Index: 1, Name: "main"}},
		},
	}
	got := closeevent.FindClosed(prior, post, "session-closed")
	if got == nil || got.Session == nil {
		t.Fatal("expected closed session, got nil")
	}
	if got.SessionName != "halo-nix-config" {
		t.Errorf("got SessionName=%q, want halo-nix-config", got.SessionName)
	}
}

func TestFindClosed_SessionClosed_UnknownNameWithOneCandidate(t *testing.T) {
	// The event names a session the prior snapshot doesn't hold, but exactly
	// one session is non-live — the attribution is unambiguous whatever the
	// name says, and a rename is the likely explanation.
	prior := snapshot.Manifest{
		V:    1,
		Host: "h",
		Sessions: []snapshot.Session{
			{Name: "halo-nix-config", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "lazytmux", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
		},
	}
	post := closeevent.CloseManifest{
		SessionName: "halo-nix-config-renamed",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "lazytmux", Index: 1, Name: "main"}},
		},
	}
	got := closeevent.FindClosed(prior, post, "session-closed")
	if got == nil || got.Session == nil {
		t.Fatal("expected closed session, got nil")
	}
	if got.SessionName != "halo-nix-config" {
		t.Errorf("got SessionName=%q, want halo-nix-config", got.SessionName)
	}
}

func TestResolve_SnapshotWinsOverEmbedded(t *testing.T) {
	// Both a snapshot match and an embedded entity are present. prior is
	// id-aware and post.WindowID pins the id match, so the diff is trusted
	// and must win (see Resolve's doc comment for why).
	prior := snapshot.Manifest{
		SavedAt: 100,
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, Name: "keep", ID: "@1"},
				{Index: 2, Name: "from-snapshot", ID: "@2"},
			}},
		},
	}
	post := closeevent.CloseManifest{
		WindowID: "@2",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "keep"}},
		},
		Resolved: &closeevent.ResolvedClose{
			Item:    closeevent.ClosedItem{SessionName: "s", WindowIndex: 2, Window: &snapshot.Window{Index: 2, Name: "from-embedded"}},
			SavedAt: 200,
		},
	}
	item, savedAt, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if !ok {
		t.Fatal("expected resolved, got false")
	}
	if item.Window == nil || item.Window.Name != "from-snapshot" {
		t.Errorf("got window %+v, want the snapshot-diff result", item.Window)
	}
	if savedAt != 100 {
		t.Errorf("got savedAt=%d, want the prior snapshot's SavedAt=100", savedAt)
	}
}

func TestResolve_IDUnawarePriorFallsBackToEmbeddedOverPositionalGuess(t *testing.T) {
	// prior has no window IDs (a pre-id snapshot). renumber-windows shifted
	// "survivor" down from index 2 to index 1 after "closed-me" (index 1)
	// shut, so the positional fallback finds index 2 "missing" and returns
	// the SURVIVING window under a wrong guess. The embedded entity —
	// captured at close time — knows the real one; Resolve must prefer it.
	prior := snapshot.Manifest{
		SavedAt: 100,
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, Name: "closed-me"},
				{Index: 2, Name: "survivor"},
			}},
		},
	}
	post := closeevent.CloseManifest{
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "survivor"}},
		},
		Resolved: &closeevent.ResolvedClose{
			Item:    closeevent.ClosedItem{SessionName: "s", WindowIndex: 1, Window: &snapshot.Window{Index: 1, Name: "closed-me"}},
			SavedAt: 200,
		},
	}
	item, savedAt, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if !ok {
		t.Fatal("expected resolved, got false")
	}
	if item.Window == nil || item.Window.Name != "closed-me" {
		t.Errorf("got window %+v, want the embedded entity closed-me, not the positional guess survivor", item.Window)
	}
	if savedAt != 200 {
		t.Errorf("got savedAt=%d, want the embedded ResolvedClose's SavedAt=200", savedAt)
	}
}

func TestResolve_IDUnawarePriorNoEmbeddedStillUsesPositionalGuess(t *testing.T) {
	// Same id-unaware prior/post as above, but the event carries no embedded
	// entity (recorded before Resolved existed). Resolve must fall back to
	// the same positional guess exactly as it always has — no regression for
	// the pre-existing rows.
	prior := snapshot.Manifest{
		SavedAt: 100,
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, Name: "closed-me"},
				{Index: 2, Name: "survivor"},
			}},
		},
	}
	post := closeevent.CloseManifest{
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "survivor"}},
		},
	}
	item, savedAt, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if !ok {
		t.Fatal("expected resolved, got false")
	}
	if item.Window == nil || item.Window.Name != "survivor" {
		t.Errorf("got window %+v, want the positional guess survivor (index 2)", item.Window)
	}
	if savedAt != 100 {
		t.Errorf("got savedAt=%d, want the prior snapshot's SavedAt=100", savedAt)
	}
}

func TestResolve_EmbeddedPaneWithoutWindowIsUnresolvable(t *testing.T) {
	// Deserialisation boundary: a manifest_json row with "pane" but no
	// "window" unmarshals cleanly into ClosedItem, but SubManifest and
	// buildRestorePlan dereference Window unconditionally once Pane is set.
	// Resolve must report this unresolvable rather than handing it to callers.
	var prior snapshot.Manifest
	post := closeevent.CloseManifest{
		Resolved: &closeevent.ResolvedClose{
			Item:    closeevent.ClosedItem{SessionName: "s", Pane: &snapshot.Pane{Index: 1, Command: "vim"}},
			SavedAt: 200,
		},
	}
	item, savedAt, ok := closeevent.Resolve(prior, post, "pane-died")
	if ok {
		t.Errorf("expected unresolvable, got item=%+v savedAt=%d", item, savedAt)
	}
	if item != nil {
		t.Errorf("expected nil item, got %+v", item)
	}
}

func TestResolve_FallsBackToEmbeddedWhenSnapshotMisses(t *testing.T) {
	// FindClosed returns nil against a non-empty prior (the window isn't in
	// it), so Resolve must fall back to the embedded entity.
	prior := snapshot.Manifest{
		SavedAt: 100,
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{{Index: 1, Name: "keep"}}},
		},
	}
	post := closeevent.CloseManifest{
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "s", Index: 1, Name: "keep"}},
		},
		Resolved: &closeevent.ResolvedClose{
			Item:    closeevent.ClosedItem{SessionName: "s", WindowIndex: 2, Window: &snapshot.Window{Index: 2, Name: "from-embedded"}},
			SavedAt: 200,
		},
	}
	item, savedAt, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if !ok {
		t.Fatal("expected resolved, got false")
	}
	if item.Window == nil || item.Window.Name != "from-embedded" {
		t.Errorf("got window %+v, want the embedded result", item.Window)
	}
	if savedAt != 200 {
		t.Errorf("got savedAt=%d, want the embedded ResolvedClose's SavedAt=200", savedAt)
	}
}

func TestResolve_ZeroPriorFallsBackToEmbedded(t *testing.T) {
	// A zero snapshot.Manifest is a legal prior (e.g. no snapshot ever
	// existed); FindClosed returns nil on it and Resolve must still recover
	// the embedded entity.
	var prior snapshot.Manifest
	post := closeevent.CloseManifest{
		Index: closeevent.IndexPost{},
		Resolved: &closeevent.ResolvedClose{
			Item:    closeevent.ClosedItem{SessionName: "s", WindowIndex: 2, Window: &snapshot.Window{Index: 2, Name: "from-embedded"}},
			SavedAt: 200,
		},
	}
	item, savedAt, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if !ok {
		t.Fatal("expected resolved, got false")
	}
	if item.Window == nil || item.Window.Name != "from-embedded" {
		t.Errorf("got window %+v, want the embedded result", item.Window)
	}
	if savedAt != 200 {
		t.Errorf("got savedAt=%d, want 200", savedAt)
	}
}

func TestResolve_NeitherResolves(t *testing.T) {
	var prior snapshot.Manifest
	post := closeevent.CloseManifest{Index: closeevent.IndexPost{}}
	item, savedAt, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if ok {
		t.Errorf("expected false, got true (item=%+v, savedAt=%d)", item, savedAt)
	}
	if item != nil {
		t.Errorf("expected nil item, got %+v", item)
	}
}

func TestParseManifest_NoResolvedKeyStillParses(t *testing.T) {
	// Events recorded before Resolved existed carry no "resolved" key; parsing
	// must succeed and Resolve must behave exactly as before (snapshot diff,
	// no embedded fallback).
	s := `{"session_id":"s","window_id":"@2","index":{"windows":[{"session":"s","index":1,"name":"keep"}]}}`
	post, err := closeevent.ParseManifest(s)
	if err != nil {
		t.Fatalf("ParseManifest returned error: %v", err)
	}
	if post.Resolved != nil {
		t.Fatalf("expected nil Resolved, got %+v", post.Resolved)
	}
	prior := snapshot.Manifest{
		Sessions: []snapshot.Session{
			{Name: "s", Windows: []snapshot.Window{
				{Index: 1, Name: "keep", ID: "@1"},
				{Index: 2, Name: "gone", ID: "@2"},
			}},
		},
	}
	item, _, ok := closeevent.Resolve(prior, post, "window-unlinked")
	if !ok || item == nil || item.Window == nil || item.Window.ID != "@2" {
		t.Fatalf("got item=%+v ok=%v, want the snapshot-diff result for @2", item, ok)
	}
}

func TestFindClosed_SessionClosed_UnknownNameAmbiguousIsUnrecoverable(t *testing.T) {
	// A scratch session created and killed inside a snapshot gap: no snapshot
	// holds it, and two sessions are non-live. Guessing the first would offer
	// a row labelled "remux-shot2" whose restore rebuilds halo-nix-config, so
	// the close is reported unrecoverable and the picker hides it.
	prior := snapshot.Manifest{
		V:    1,
		Host: "h",
		Sessions: []snapshot.Session{
			{Name: "halo-nix-config", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "noamsto", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
			{Name: "lazytmux", Windows: []snapshot.Window{{Index: 1, Name: "main"}}},
		},
	}
	post := closeevent.CloseManifest{
		SessionName: "remux-shot2",
		Index: closeevent.IndexPost{
			Windows: []tmux.WindowRow{{Session: "lazytmux", Index: 1, Name: "main"}},
		},
	}
	if got := closeevent.FindClosed(prior, post, "session-closed"); got != nil {
		t.Errorf("got %+v (Session=%+v), want nil for an ambiguous unmatched name", got, got.Session)
	}
}
