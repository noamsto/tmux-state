// Package main is the tmux-remux CLI entry point.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alecthomas/kong"

	"github.com/noamsto/tmux-remux/internal/applog"
	"github.com/noamsto/tmux-remux/internal/closeevent"
	"github.com/noamsto/tmux-remux/internal/config"
	"github.com/noamsto/tmux-remux/internal/filter"
	"github.com/noamsto/tmux-remux/internal/lockfile"
	"github.com/noamsto/tmux-remux/internal/picker"
	"github.com/noamsto/tmux-remux/internal/restore"
	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
	"github.com/noamsto/tmux-remux/internal/tmux"
	"github.com/noamsto/tmux-remux/internal/triggers"
)

// Version is the released version. Bumped on tagged releases.
const Version = "0.4.0"

var hostname = sync.OnceValue(func() string {
	h, _ := os.Hostname()
	return h
})

// CLI is the full command grammar; kong parses os.Args into it. Field order is
// the order subcommands appear in --help.
type CLI struct {
	Version       VersionCmd       `cmd:"" help:"Print version"`
	Save          SaveCmd          `cmd:"" help:"Save a snapshot of the current tmux server"`
	Restore       RestoreCmd       `cmd:"" help:"Restore the latest snapshot through the smart filter"`
	Undo          UndoCmd          `cmd:"" help:"Restore the most recent close event"`
	Pick          PickCmd          `cmd:"" help:"Open an interactive picker over events"`
	CaptureEvent  CaptureEventCmd  `cmd:"" name:"capture-event" help:"Record a close event (called from tmux hooks)"`
	List          ListCmd          `cmd:"" help:"List events"`
	Prune         PruneCmd         `cmd:"" help:"Apply retention limits to events"`
	GC            GCCmd            `cmd:"" name:"gc" help:"Reap orphan scrollback files"`
	CatScrollback CatScrollbackCmd `cmd:"" name:"cat-scrollback" hidden:"" help:"Stream stored scrollback to stdout (internal helper)"`
	RelaunchStamp RelaunchStampCmd `cmd:"" name:"relaunch-stamp" hidden:"" help:"Stamp @remux_relaunch from an agent start hook (internal helper)"`
	InstallHook   InstallHookCmd   `cmd:"" name:"install-hook" help:"Wire an agent start hook for resume-on-restore"`
	Triggers      TriggersCmd      `cmd:"" help:"Print the tmux.conf fragment that wires tmux-remux (used by tmux-remux.tmux)"`
}

func main() {
	var cli CLI
	kctx := kong.Parse(&cli,
		kong.Name("tmux-remux"),
		kong.Description("Fast, smart tmux state persistence"),
		kong.UsageOnError(),
	)
	if err := kctx.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tmux-remux: error:", err)
		if log, lerr := applog.Open(loadConfig().LogPath); lerr == nil {
			log.Logf("error: %v (args: %v)", err, os.Args[1:])
			_ = log.Close()
		}
		os.Exit(1)
	}
}

// withStore opens the DB after ensuring storage directories exist, takes an
// exclusive flock on cfg.LockPath to serialize writers, runs fn, and closes
// the DB. Used by every subcommand's Run.
func withStore(fn func(ctx context.Context, cfg config.Config, db *store.Store) error) error {
	ctx, cancel := signalCtx()
	defer cancel()
	cfg := loadConfig()
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	lock, err := lockfile.Acquire(ctx, cfg.LockPath)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(ctx, cfg, db)
}

// VersionCmd prints the version.
type VersionCmd struct{}

func (VersionCmd) Run() error {
	fmt.Println(Version)
	return nil
}

// TriggersCmd prints the tmux.conf fragment that wires tmux-remux into tmux.
// tmux-remux.tmux pipes it into `tmux source-file -`; examples/tmux.conf is a
// checked-in copy of the tmux 3.8 output.
type TriggersCmd struct {
	Bin         string `help:"tmux-remux path the hooks invoke (default: this binary)"`
	TmuxVersion string `name:"tmux-version" help:"target tmux version, e.g. 3.8 (default: detected via tmux -V)"`
	AutoRestore string `name:"auto-restore" default:"on" enum:"on,off" help:"emit the restore --auto line"`
}

func (c TriggersCmd) Run() error {
	bin := c.Bin
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		bin = exe
	}

	// A guessed version would be worse than none: a 3.8 fragment sourced by a
	// 3.7 tmux fails on set-hook -B partway through, leaving hooks half-wired.
	var (
		v   tmux.Version
		err error
	)
	if c.TmuxVersion != "" {
		v, err = tmux.ParseVersion(c.TmuxVersion)
	} else {
		v, err = tmux.NewClient("tmux").Version(context.Background())
	}
	if err != nil {
		return err
	}

	fmt.Print(triggers.Render(triggers.Params{
		Bin:         bin,
		Version:     v,
		AutoRestore: c.AutoRestore == "on",
	}))
	return nil
}

// SaveCmd saves a snapshot of the current tmux server.
type SaveCmd struct {
	Reason string `default:"manual" help:"reason for save (e.g. 'timer', 'hook:session-created')"`
}

func (c SaveCmd) Run() error {
	return withStore(func(ctx context.Context, cfg config.Config, db *store.Store) error {
		log, err := applog.Open(cfg.LogPath)
		if err != nil {
			return err
		}
		defer func() { _ = log.Close() }()
		sb := scrollback.New(cfg.ScrollbackDir)
		t := tmux.NewClient("tmux", cfg.DecorationOptions...)
		saver := snapshot.NewSaver(db, sb, t, snapshot.SaverOptions{
			Host:              hostname(),
			CaptureScrollback: cfg.CaptureScrollback,
			MinSaveInterval:   cfg.MinSaveInterval,
			Logf:              log.Logf,
		})
		if err := saver.Save(ctx, c.Reason); err != nil {
			return err
		}
		if err := db.PruneSnapshots(ctx, cfg.SnapshotHistoryLimit, time.Now().UnixMilli()); err != nil {
			return err
		}
		n, err := db.PruneUnresolvableCloseEvents(ctx)
		if err != nil {
			return err
		}
		log.Logf("save: pruned %d unresolvable close events", n)
		return nil
	})
}

// RestoreCmd restores the latest snapshot through the smart filter.
type RestoreCmd struct {
	Auto bool `help:"respect restore_mode=off"`
}

func (c RestoreCmd) Run() error {
	return withStore(func(ctx context.Context, cfg config.Config, db *store.Store) error {
		if cfg.RestoreMode == config.RestoreOff && c.Auto {
			return nil
		}
		log, err := applog.Open(cfg.LogPath)
		if err != nil {
			return err
		}
		defer func() { _ = log.Close() }()

		t := tmux.NewClient("tmux")
		startMs, err := t.ServerStartTime(ctx)
		if err != nil {
			log.Logf("restore: server start time: %v", err)
			return err
		}
		// Anchor selection to before this server existed: snapshots
		// written by the current server's own save hooks (the
		// session-created hook and the systemd timer both race this
		// command at server birth) can never be selected.
		ev, err := db.LatestSnapshotBefore(ctx, startMs)
		if err != nil {
			log.Logf("restore: %v", err)
			return err
		}
		if ev == nil {
			log.Logf("restore: no snapshot before server start — nothing to do")
			return nil
		}

		var m snapshot.Manifest
		if err := json.Unmarshal([]byte(ev.ManifestJSON), &m); err != nil {
			log.Logf("restore: parse snapshot %d: %v", ev.ID, err)
			return err
		}

		f := filter.Filter{
			MaxSessionAge:       cfg.RestoreMaxSessionAge,
			MaxSnapshotAge:      cfg.RestoreMaxSnapshotAge,
			SkipIdleShells:      cfg.RestoreSkipIdleShells,
			SkipIdleWindows:     cfg.RestoreSkipIdleWindows,
			SkipRunningSessions: cfg.SkipRunningSessions,
		}
		age := time.Since(time.UnixMilli(ev.Ts)).Round(time.Second)
		if f.SkipSnapshot(ev.Ts) {
			log.Logf("restore: snapshot %d (age %s) older than max-snapshot-age — skipped", ev.ID, age)
			return nil
		}

		// ListSessions already maps "no server" to (nil, nil); any error here
		// is real. Abort rather than proceed with an empty running-set — an
		// unknown "is it running" state must not be treated as "not running",
		// or BuildPlan recreates a live session and injects windows into it.
		running := map[string]bool{}
		rows, err := t.ListSessions(ctx)
		if err != nil {
			log.Logf("restore: list sessions: %v — aborting to avoid clobbering live sessions", err)
			return err
		}
		for _, s := range rows {
			running[s.Name] = true
		}

		opts := resolveBuildOptions(ctx, t, cfg.CommandAllowList)
		plan, stats := restore.BuildPlan(m, f, running, opts)
		failed, err := restore.Apply(ctx, t, plan)
		if err != nil {
			log.Logf("restore: snapshot %d (age %s): apply failed: %v", ev.ID, age, err)
			return err
		}
		log.Logf("restore: snapshot %d (age %s): %d sessions restored, skipped %d running / %d stale / %d idle (%d idle windows), %d actions, %d failed",
			ev.ID, age, stats.SessionsKept, stats.SessionsSkippedRunning,
			stats.SessionsSkippedStale, stats.SessionsSkippedIdle,
			stats.WindowsSkippedIdle, len(plan), len(failed))
		for _, actionErr := range failed {
			log.Logf("restore: snapshot %d: action failed: %v", ev.ID, actionErr)
		}
		// Launch feedback: make a filtered-to-nothing restore visible
		// at the moment it happens. Best-effort — at server birth
		// there may be no attached client to display to.
		if c.Auto && (stats.SessionsKept > 0 || stats.SessionsSkippedIdle > 0) {
			_, _ = t.Run(ctx, []string{"display-message",
				fmt.Sprintf("tmux-remux: restored %d sessions (%d filtered)",
					stats.SessionsKept, stats.SessionsSkippedIdle)})
		}
		return nil
	})
}

// UndoCmd restores the most recent close event.
type UndoCmd struct {
	Pop     bool   `help:"restore most recent close event and remove it from history"`
	Session string `help:"session to prefer (#{session_name}); falls back to the attached client's"`
}

func (c UndoCmd) Run() error {
	if !c.Pop {
		return fmt.Errorf("only --pop is supported in v0.1.0")
	}
	return withStore(func(ctx context.Context, cfg config.Config, db *store.Store) error {
		t := tmux.NewClient("tmux")
		target, err := restorableClose(ctx, db, currentSession(ctx, t, c.Session))
		if err != nil {
			return err
		}
		if len(target.Discarded) > 0 {
			if err := deleteEvents(ctx, db, target.Discarded); err != nil {
				return err
			}
			return fmt.Errorf("%s", discardSummary(target.Discarded, target.MoreAvailable))
		}
		if !target.OK {
			return fmt.Errorf("nothing to undo — no recoverable close event")
		}
		opts := resolveBuildOptions(ctx, t, cfg.CommandAllowList)
		plan, m := buildRestorePlan(ctx, t, target.Item, target.Prior, opts)
		failed, err := restore.Apply(ctx, t, plan)
		if err != nil {
			return err
		}
		if len(failed) > 0 {
			// A partial or total restore failure must not delete the close
			// event — erasing history for a restore that didn't happen would
			// make the window unrecoverable even on a second undo.
			return fmt.Errorf("restore failed, close event kept for retry: %w", errors.Join(failed...))
		}
		focusRestored(ctx, t, m)
		if err := deleteEvents(ctx, db, []store.Event{target.Event}); err != nil {
			return err
		}
		if note := undoMessage(target.FromSession); note != "" {
			_, _ = t.Run(ctx, []string{"display-message", note})
		}
		return nil
	})
}

// undoScanLimit bounds how far back undo --pop scans for a recoverable close
// event. Generous enough to step past a run of unrecoverable heads, bounded so
// a corrupt history can't turn undo into a full-table scan.
const undoScanLimit = 50

// undoTarget is the result of scanning the close history for undo: the newest
// restorable close, plus the unrecoverable ones sitting in front of it.
type undoTarget struct {
	Event store.Event
	Item  *closeevent.ClosedItem
	Prior snapshot.Manifest
	OK    bool
	// Discarded is the leading run of close events no snapshot ever captured.
	// Recoverability only decays (snapshots get pruned, never added behind a
	// timestamp), so these can never become restorable and undo drops them.
	Discarded []store.Event
	// FromSession names the session an event was borrowed from when the current
	// session had nothing restorable. Empty for a same-session undo.
	FromSession string
	// MoreAvailable reports whether anything restorable survives behind the
	// discarded run — in this session or, via the cross-session fallback, in
	// another. Distinct from OK, which covers only this session.
	MoreAvailable bool
}

// restorableClose finds the close event to undo. It prefers the newest
// restorable close owned by `session`, falling back to the newest anywhere when
// that session has none — reaching across is better than refusing to restore
// something the user can see is gone, as long as the message says where it came
// from. An empty `session` means no session context and scans server-wide.
//
// Unrecoverable events are discarded only when they belong to `session`.
// Discarding is garbage collection — a close no snapshot captured can never
// become restorable — but scoping it keeps the message honest: consuming another
// session's dead rows here would rob that session of its own explanation.
func restorableClose(ctx context.Context, db *store.Store, session string) (undoTarget, error) {
	evs, err := db.ListEvents(ctx, store.ListOpts{ExcludeKinds: []string{"snapshot"}, Limit: undoScanLimit})
	if err != nil {
		return undoTarget{}, err
	}
	var t undoTarget
	var fallback *undoTarget
	for _, ev := range evs {
		item, prior, ok := resolveEvent(ctx, db, ev)
		owner := eventOwner(ev, item)
		mine := session == "" || owner == session
		// Defense-in-depth on the sub-manifest: every item FindClosed returns
		// now yields a non-empty one, but guard against a future resolver that
		// can't build a restore plan rather than popping an un-restorable head.
		if !ok || len(item.SubManifest(prior.Host, prior.SavedAt).Sessions) == 0 {
			if mine {
				t.Discarded = append(t.Discarded, ev)
			}
			continue
		}
		if mine {
			t.Event, t.Item, t.Prior, t.OK, t.MoreAvailable = ev, item, prior, true, true
			return t, nil
		}
		if fallback == nil {
			fallback = &undoTarget{Event: ev, Item: item, Prior: prior, OK: true, FromSession: owner}
		}
	}
	// Nothing restorable in this session. Discarded rows are reported first —
	// this press explains them and the next one restores — so a pending fallback
	// only sets MoreAvailable here rather than being returned.
	if len(t.Discarded) > 0 {
		t.MoreAvailable = fallback != nil
		return t, nil
	}
	if fallback != nil {
		return *fallback, nil
	}
	return t, nil
}

// eventOwner reports which session a close event belonged to.
func eventOwner(ev store.Event, item *closeevent.ClosedItem) string {
	closeMan, err := closeevent.ParseManifest(ev.ManifestJSON)
	if err != nil {
		return closeevent.UnknownSession
	}
	return closeevent.OwnerSession(closeMan, item)
}

// undoMessage returns the note to print after a cross-session undo, or "" when
// the restore came from the session the user is in.
func undoMessage(fromSession string) string {
	if fromSession == "" {
		return ""
	}
	return fmt.Sprintf("restored from session %s — nothing was closed in this one", fromSession)
}

// currentSession resolves the session the user is acting from: the flag when the
// keybinding passed one, else the attached client's session. A config that wires
// tmux-remux by hand rather than through tmux-remux.tmux passes no flag, so the
// client lookup is what keeps those installs session-aware. Empty means no
// context, which scans server-wide.
func currentSession(ctx context.Context, t *tmux.Client, flag string) string {
	if flag != "" {
		return flag
	}
	out, err := t.Run(ctx, []string{"display-message", "-p", "#{client_session}"})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func deleteEvents(ctx context.Context, db *store.Store, evs []store.Event) error {
	for _, ev := range evs {
		if _, err := db.DB().ExecContext(ctx, "DELETE FROM events WHERE id = ?", ev.ID); err != nil {
			return err
		}
	}
	return nil
}

// discardSummary explains why undo restored nothing this press. `more` reports
// whether a recoverable close survives behind the discarded run.
func discardSummary(evs []store.Event, more bool) string {
	head := evs[0]
	age := time.Since(time.UnixMilli(head.Ts)).Round(time.Second)
	what := fmt.Sprintf("last %s close (%s ago)", head.Scope, age)
	if len(evs) > 1 {
		what = fmt.Sprintf("last %d closes (newest: %s, %s ago)", len(evs), head.Scope, age)
	}
	tail := "nothing older is recoverable either"
	if more {
		tail = "press prefix+u again to undo the one before it"
	}
	return fmt.Sprintf("%s never made it into a snapshot — discarded; %s", what, tail)
}

// resolveEvent resolves a close event to its lost entity against the most
// recent pre-close snapshot. ok is false when the event isn't a recoverable
// close: unparsable, no prior snapshot, or the entity was never captured.
func resolveEvent(ctx context.Context, db *store.Store, ev store.Event) (*closeevent.ClosedItem, snapshot.Manifest, bool) {
	closeMan, err := closeevent.ParseManifest(ev.ManifestJSON)
	if err != nil {
		return nil, snapshot.Manifest{}, false
	}
	snap, err := db.LatestSnapshotBefore(ctx, ev.Ts)
	if err != nil || snap == nil {
		return nil, snapshot.Manifest{}, false
	}
	var prior snapshot.Manifest
	if err := json.Unmarshal([]byte(snap.ManifestJSON), &prior); err != nil {
		return nil, snapshot.Manifest{}, false
	}
	item := closeevent.FindClosed(prior, closeMan, ev.Kind)
	if item == nil {
		return nil, snapshot.Manifest{}, false
	}
	return item, prior, true
}

// buildRestorePlan turns a resolved close into a restore plan. A lost pane is
// split back into its live parent window (or the window is recreated if it's
// gone); a window or session is rebuilt via BuildPlan. Returns the plan and the
// sub-manifest used for post-restore focus.
func buildRestorePlan(ctx context.Context, t *tmux.Client, item *closeevent.ClosedItem, prior snapshot.Manifest, opts restore.BuildOptions) ([]restore.Action, snapshot.Manifest) {
	m := item.SubManifest(prior.Host, prior.SavedAt)
	if item.Pane != nil {
		target := parentWindowTarget(ctx, t, item.SessionName, *item.Window)
		return restore.BuildPaneRestore(*item.Pane, *item.Window, item.SessionName, target, opts), m
	}
	plan, _ := restore.BuildPlan(m, filter.Filter{}, nil, opts)
	// A window close's sub-manifest holds exactly one window, so -b puts it back
	// at the index it was closed at, shifting whatever renumbering moved into
	// that slot. A whole-session close must not: inserting mid-plan shifts
	// windows that later actions target by index. Discriminate on item.Session,
	// NOT on CreateWindow.NewSession — BuildPlan sets that on every session's
	// first window, so it is true for the single-window case too.
	if item.Session == nil {
		for i, a := range plan {
			if cw, ok := a.(restore.CreateWindow); ok {
				cw.InsertBefore = true
				plan[i] = cw
			}
		}
	}
	return plan, m
}

// eventByID returns the event with the given id from evs, or a zero Event
// (which resolveEvent rejects) when absent.
func eventByID(evs []store.Event, id int64) store.Event {
	for _, ev := range evs {
		if ev.ID == id {
			return ev
		}
	}
	return store.Event{}
}

// parentWindowTarget resolves the live tmux target of a lost pane's parent
// window, or "" when no live window matches. Returns a window id, which is
// unambiguous for split-window -t.
func parentWindowTarget(ctx context.Context, t *tmux.Client, session string, win snapshot.Window) string {
	live, err := t.ListWindows(ctx)
	if err != nil {
		return ""
	}
	return matchParentWindow(live, session, win)
}

// matchParentWindow picks the live window that is `win`, trying id then name
// within the session. A window id is stable only within one server lifetime and
// a restored window carries a fresh one, so an id miss must not be read as "the
// window is gone" — that would recreate a window sitting right there. Name is
// scoped to the session so a same-named window elsewhere can never match.
//
// There is deliberately no index fallback: renumber-windows shifts a survivor
// into the exact index a closed window vacated, so an index match can resolve to
// a live window that merely landed there, splitting the lost pane into it and
// overwriting its layout.
func matchParentWindow(live []tmux.WindowRow, session string, win snapshot.Window) string {
	if win.ID != "" {
		for _, w := range live {
			if w.ID == win.ID {
				return w.ID
			}
		}
	}
	if name := snapshot.StripFormat(win.Name); name != "" {
		for _, w := range live {
			if w.Session == session && snapshot.StripFormat(w.Name) == name {
				return w.ID
			}
		}
	}
	return ""
}

// PickCmd opens an interactive picker over events.
type PickCmd struct {
	Kind    string `default:"snapshot" enum:"snapshot,close" help:"snapshot|close"`
	Session string `help:"session to group by (#{session_name}); falls back to the attached client's"`
}

func (c PickCmd) Run() error {
	return withStore(func(ctx context.Context, cfg config.Config, db *store.Store) error {
		opts := store.ListOpts{Limit: 50}
		mode := picker.ModeSnapshot
		switch c.Kind {
		case "snapshot":
			opts.Kinds = []string{"snapshot"}
		case "close":
			opts.ExcludeKinds = []string{"snapshot"}
			mode = picker.ModeClose
		}
		evs, err := db.ListEvents(ctx, opts)
		if err != nil {
			return err
		}

		t := tmux.NewClient("tmux")
		// A real ListSessions error (no-server is already (nil, nil)) leaves us
		// unable to tell which sessions are live; restoring anyway could inject
		// windows into an attached session, so abort before opening the picker.
		runningSet := map[string]bool{}
		sessions, err := t.ListSessions(ctx)
		if err != nil {
			return fmt.Errorf("list running sessions: %w", err)
		}
		for _, s := range sessions {
			runningSet[s.Name] = true
		}

		sb := scrollback.New(cfg.ScrollbackDir)
		var ctxs map[int64]picker.CloseContext
		hidden := 0
		if mode == picker.ModeClose {
			ctxs = buildCloseContexts(ctx, db, evs)
			evs, hidden = partitionRecoverable(evs, ctxs)
		}
		m := picker.NewPickerModel(mode, evs, runningSet, sb)
		if mode == picker.ModeClose {
			m.SetCloseContexts(ctxs)
			m.SetHiddenCount(hidden)
			m.SetCloseRows(picker.BuildCloseList(evs, ctxs, currentSession(ctx, t, c.Session)))
		}
		m.Bootstrap()

		prog := tea.NewProgram(m)
		finalModel, err := prog.Run()
		if err != nil {
			return fmt.Errorf("picker: %w", err)
		}
		final, ok := finalModel.(picker.PickerModel)
		if !ok || final.SelectedID() == 0 {
			return nil // cancelled
		}

		buildOpts := resolveBuildOptions(ctx, t, cfg.CommandAllowList)

		// Close mode restores one lost entity (the same split-or-recreate
		// path as undo); snapshot mode replays a whole snapshot.
		if mode == picker.ModeClose {
			item, prior, ok := resolveEvent(ctx, db, eventByID(evs, final.SelectedID()))
			if !ok {
				return nil
			}
			plan, m := buildRestorePlan(ctx, t, item, prior, buildOpts)
			failed, err := restore.Apply(ctx, t, plan)
			if err != nil {
				return err
			}
			if len(failed) > 0 {
				return fmt.Errorf("restore failed: %w", errors.Join(failed...))
			}
			focusRestored(ctx, t, m)
			return nil
		}

		manifest := final.SelectedManifest()
		plan, _ := restore.BuildPlan(manifest, final.Filter(), runningSet, buildOpts)
		_, err = restore.Apply(ctx, t, plan)
		return err
	})
}

// partitionRecoverable splits close events into those with a recoverable entity
// (a non-empty sub-manifest in ctxs) and a count of those without. An
// unrecoverable close — entity born-and-died inside a snapshot gap, or a window
// moved rather than closed — carries nothing to restore, so the picker hides it
// behind the returned count instead of listing a dead "(invalid manifest)" row.
func partitionRecoverable(evs []store.Event, ctxs map[int64]picker.CloseContext) (kept []store.Event, hidden int) {
	kept = make([]store.Event, 0, len(evs))
	for _, ev := range evs {
		if len(ctxs[ev.ID].SubManifest.Sessions) == 0 {
			hidden++
			continue
		}
		kept = append(kept, ev)
	}
	return kept, hidden
}

// buildCloseContexts resolves each close event against its parent snapshot
// (most recent snapshot < event.Ts) to derive a short label + sub-manifest of
// the lost entity. Best-effort: events without a recoverable parent get no map
// entry, and partitionRecoverable filters those out of the picker as hidden.
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

// placementFor locates a resolved close in the tmux hierarchy for the picker's
// tree. Scope is read off which field of the item is set — Pane before Window,
// since a pane-died carries both.
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

// focusRestored selects the first restored session/window so the user
// immediately lands on what they un-closed, instead of staying on whatever
// session was attached when they pressed Enter.
func focusRestored(ctx context.Context, t *tmux.Client, m snapshot.Manifest) {
	if len(m.Sessions) == 0 {
		return
	}
	s := m.Sessions[0]
	if len(s.Windows) == 0 {
		_, _ = t.Run(ctx, []string{"switch-client", "-t", s.Name})
		return
	}
	target := fmt.Sprintf("%s:%d", s.Name, s.Windows[0].Index)
	_, _ = t.Run(ctx, []string{"switch-client", "-t", target})
	_, _ = t.Run(ctx, []string{"select-window", "-t", target})
}

// CaptureEventCmd records a close event (called from tmux hooks).
type CaptureEventCmd struct {
	Kind        string `arg:"" help:"event kind"`
	Session     string `help:"tmux session id ($N)"`
	SessionName string `name:"session-name" help:"tmux session name (#{hook_session_name})"`
	Window      string `help:"tmux window id (@N)"`
	Pane        string `help:"tmux pane id (%N)"`
}

func (c CaptureEventCmd) Run() error {
	return withStore(func(ctx context.Context, _ config.Config, db *store.Store) error {
		// The closed entity is already gone when the hook fires, so a
		// live query yields the true post-close survivor set. Errors
		// (last session closed, server gone) leave the index empty —
		// which is also the truth: nothing survived.
		t := tmux.NewClient("tmux")
		var post closeevent.IndexPost
		post.Windows, _ = t.ListWindows(ctx)
		post.Panes, _ = t.ListPanes(ctx)
		_, err := closeevent.Capture(ctx, db, closeevent.Args{
			Kind:        c.Kind,
			SessionID:   c.Session,
			SessionName: c.SessionName,
			WindowID:    c.Window,
			PaneID:      c.Pane,
			Host:        hostname(),
			Index:       post,
		})
		return err
	})
}

// ListCmd lists events.
type ListCmd struct {
	JSON bool `name:"json" help:"emit one JSON object per line (newline-delimited)"`
}

func (c ListCmd) Run() error {
	return withStore(func(ctx context.Context, _ config.Config, db *store.Store) error {
		evs, err := db.ListEvents(ctx, store.ListOpts{Limit: 100})
		if err != nil {
			return err
		}
		if c.JSON {
			enc := json.NewEncoder(os.Stdout)
			for _, ev := range evs {
				if err := enc.Encode(ev); err != nil {
					return err
				}
			}
			return nil
		}
		for _, ev := range evs {
			t := time.UnixMilli(ev.Ts).Format("2006-01-02 15:04:05")
			fmt.Printf("%d\t%s  %-15s  %s\n", ev.ID, t, ev.Kind, ev.Reason)
		}
		return nil
	})
}

// PruneCmd applies retention limits to events.
type PruneCmd struct{}

func (PruneCmd) Run() error {
	return withStore(func(ctx context.Context, cfg config.Config, db *store.Store) error {
		if err := db.PruneSnapshots(ctx, cfg.SnapshotHistoryLimit, time.Now().UnixMilli()); err != nil {
			return err
		}
		_, err := db.PruneCloseEvents(ctx, cfg.CloseEventLimit)
		return err
	})
}

// GCCmd reaps orphan scrollback files.
type GCCmd struct{}

func (GCCmd) Run() error {
	return withStore(func(ctx context.Context, cfg config.Config, db *store.Store) error {
		log, err := applog.Open(cfg.LogPath)
		if err != nil {
			return err
		}
		defer func() { _ = log.Close() }()
		sb := scrollback.New(cfg.ScrollbackDir)
		orphans, err := db.ScrollbacksWithZeroRef(ctx)
		if err != nil {
			return err
		}
		for _, sha := range orphans {
			if err := sb.Delete(ctx, sha); err != nil {
				continue
			}
			// File is gone; drop the row. The next gc retries on failure
			// (sb.Delete of a missing file is a no-op), so this self-heals —
			// log a persistently failing delete so it's not silently dangling.
			if err := db.DeleteScrollback(ctx, sha); err != nil {
				log.Logf("gc: delete scrollback row %s: %v", sha, err)
			}
		}
		return nil
	})
}

func signalCtx() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func loadConfig() config.Config { return config.Default() }

// resolveBuildOptions builds the BuildOptions consumed by restore.BuildPlan.
// Errors are silently swallowed in favor of reasonable defaults: an
// empty Self disables scrollback rendering in emitted startup commands,
// and /bin/sh is the ultimate shell fallback. Resolved once per restore.
func resolveBuildOptions(ctx context.Context, t restore.Runner, allowList []string) restore.BuildOptions {
	self, err := os.Executable()
	if err != nil {
		self = ""
	}
	shell, isBash := restore.DefaultShell(ctx, t, os.Getenv("SHELL"))
	return restore.BuildOptions{
		Self:         self,
		DefaultShell: shell,
		IsBash:       isBash,
		AllowList:    allowList,
	}
}
