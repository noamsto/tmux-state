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
