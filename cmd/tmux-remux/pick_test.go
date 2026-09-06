package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/picker"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
	"github.com/noamsto/tmux-remux/internal/tmux"
)

func TestPartitionRecoverable(t *testing.T) {
	evs := []store.Event{
		{ID: 1}, // recoverable: non-empty sub-manifest
		{ID: 2}, // unrecoverable: no context entry at all
		{ID: 3}, // unrecoverable: context present but empty sub-manifest
	}
	ctxs := map[int64]picker.CloseContext{
		1: {
			Label:       "mono/win (1p)",
			SubManifest: snapshot.Manifest{Sessions: []snapshot.Session{{Name: "mono"}}},
		},
		3: {Label: "window-unlinked"},
	}

	kept, hidden := partitionRecoverable(evs, ctxs)
	if len(kept) != 1 || kept[0].ID != 1 {
		t.Fatalf("kept = %+v, want only event 1", kept)
	}
	if hidden != 2 {
		t.Errorf("hidden = %d, want 2", hidden)
	}
}

// A close event resolved against a throttled (scrollback-skipped) prior
// snapshot must carry that flag onto its sub-manifest — otherwise the close
// picker has no way to tell a throttle-caused empty capture from a genuinely
// unexplained one (see internal/picker/preview.go's closePaneContent).
func TestBuildCloseContextsPropagatesScrollbackSkipped(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snap := snapshot.Manifest{
		V: 1, Host: "h", SavedAt: 1000, ScrollbackSkipped: true,
		Sessions: []snapshot.Session{
			{Name: "gone", Windows: []snapshot.Window{{Index: 1, Panes: []snapshot.Pane{{Index: 1}}}}},
			{Name: "s1", Windows: []snapshot.Window{{Index: 1, Panes: []snapshot.Pane{{Index: 1}}}}},
		},
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertEvent(ctx, store.Event{
		Ts: 1000, Kind: "snapshot", Scope: "server", Host: "h", ManifestJSON: string(snapJSON),
	}); err != nil {
		t.Fatal(err)
	}

	// "gone" is missing from the post-close index; "s1" survives — the diff
	// resolves this as the "gone" session having closed.
	closeMan := closeevent.CloseManifest{
		SessionName: "gone",
		Index:       closeevent.IndexPost{Windows: []tmux.WindowRow{{Session: "s1", Index: 1}}},
	}
	closeJSON, err := json.Marshal(closeMan)
	if err != nil {
		t.Fatal(err)
	}
	evs := []store.Event{{ID: 42, Ts: 2000, Kind: "session-closed", ManifestJSON: string(closeJSON)}}

	ctxs := buildCloseContexts(ctx, db, evs)
	cc, ok := ctxs[42]
	if !ok {
		t.Fatalf("no CloseContext resolved for the close event; ctxs = %+v", ctxs)
	}
	if !cc.SubManifest.ScrollbackSkipped {
		t.Error("CloseContext.SubManifest.ScrollbackSkipped = false, want true (must propagate from the prior snapshot)")
	}
}
