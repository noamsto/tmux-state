package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/store"
)

// CaptureLister extends Lister with the ability to capture pane scrollback.
type CaptureLister interface {
	Lister
	CapturePane(ctx context.Context, target string) ([]byte, error)
}

// SaverOptions configures Saver behavior.
type SaverOptions struct {
	Host              string
	CaptureScrollback bool
	MinSaveInterval   time.Duration
	ScrollbackWorkers int
	// Logf, when set, receives non-fatal diagnostics (e.g. a pane whose
	// scrollback capture failed). A func hook keeps this package free of an
	// applog dependency. Nil is a no-op.
	Logf func(format string, a ...any)
}

// Saver wires together store, scrollback, and tmux to perform snapshots.
type Saver struct {
	db   *store.Store
	sb   *scrollback.Store
	tmux CaptureLister
	opts SaverOptions
}

// NewSaver returns a Saver. Pass MinSaveInterval=0 to disable throttling.
func NewSaver(db *store.Store, sb *scrollback.Store, t CaptureLister, opts SaverOptions) *Saver {
	if opts.ScrollbackWorkers <= 0 {
		opts.ScrollbackWorkers = 4
	}
	return &Saver{db: db, sb: sb, tmux: t, opts: opts}
}

func (s *Saver) logf(format string, a ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, a...)
	}
}

// Save snapshots the live tmux server. Returns nil if the snapshot was skipped
// (no tmux server running, fingerprint unchanged, or a non-structural change
// inside MinSaveInterval).
func (s *Saver) Save(ctx context.Context, reason string) error {
	now := time.Now()
	manifest, err := Build(ctx, s.tmux, s.opts.Host, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}

	// No sessions ⇒ no tmux server (tmux exits when the last session closes).
	// Writing an empty `sessions:null` event pollutes the history — `restore`
	// picks the latest by timestamp and would no-op on it. Skip silently;
	// the next hook-driven save will capture real state.
	if len(manifest.Sessions) == 0 {
		return nil
	}

	fp := manifest.Fingerprint()
	prevFP, _ := s.db.GetMeta(ctx, "last_save_fingerprint")
	prevTSStr, _ := s.db.GetMeta(ctx, "last_save_ts")
	prevTS, _ := strconv.ParseInt(prevTSStr, 10, 64)
	if fp == prevFP {
		return nil
	}
	// MinSaveInterval may only drop churn — a cwd or command change that the
	// next save will pick up anyway. Dropping a structural change instead
	// loses it forever: a window or pane created inside the throttle window
	// would never reach a snapshot, and undo can only restore what a snapshot
	// recorded. So structure always saves; the throttle downgrades it to a
	// scrollback-free snapshot, which keeps a restore storm's hook-per-window
	// saves cheap.
	structFP := manifest.StructureFingerprint()
	prevStructFP, _ := s.db.GetMeta(ctx, "last_save_structure_fingerprint")
	throttled := time.Since(time.UnixMilli(prevTS)) < s.opts.MinSaveInterval
	if throttled && structFP == prevStructFP {
		return nil
	}

	if s.opts.CaptureScrollback && !throttled {
		if err := s.captureScrollbacks(ctx, &manifest); err != nil {
			return err
		}
	} else if s.opts.CaptureScrollback {
		manifest.ScrollbackSkipped = true
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	id, err := s.db.InsertEvent(ctx, store.Event{
		Ts:           manifest.SavedAt,
		Kind:         "snapshot",
		Scope:        "server",
		Reason:       reason,
		Host:         manifest.Host,
		ManifestJSON: string(manifestJSON),
	})
	if err != nil {
		return err
	}

	for _, sess := range manifest.Sessions {
		for _, w := range sess.Windows {
			for _, p := range w.Panes {
				if p.ScrollbackSHA == "" {
					continue
				}
				key := fmt.Sprintf("%s:%d:%d", sess.Name, w.Index, p.Index)
				if err := s.db.LinkEventScrollback(ctx, id, key, p.ScrollbackSHA); err != nil {
					return err
				}
			}
		}
	}

	if err := s.db.SetMeta(ctx, "last_save_fingerprint", fp); err != nil {
		return err
	}
	if err := s.db.SetMeta(ctx, "last_save_structure_fingerprint", structFP); err != nil {
		return err
	}
	if err := s.db.SetMeta(ctx, "last_save_ts", strconv.FormatInt(manifest.SavedAt, 10)); err != nil {
		return err
	}
	return nil
}

func (s *Saver) captureScrollbacks(ctx context.Context, m *Manifest) error {
	type job struct {
		sessIdx, winIdx, paneIdx int
		target                   string
	}
	var jobs []job
	for si, sess := range m.Sessions {
		for wi, w := range sess.Windows {
			for pi, p := range w.Panes {
				jobs = append(jobs, job{
					sessIdx: si, winIdx: wi, paneIdx: pi,
					target: fmt.Sprintf("%s:%d.%d", sess.Name, w.Index, p.Index),
				})
			}
		}
	}

	sem := make(chan struct{}, s.opts.ScrollbackWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, j := range jobs {
		j := j
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			content, err := s.tmux.CapturePane(ctx, j.target)
			if err != nil {
				// Non-fatal: the pane may have died mid-save, or capture hit a
				// transient failure. Don't abort the snapshot, but log so a
				// systemic failure (e.g. a wedged socket) isn't invisible.
				mu.Lock()
				s.logf("save: capture-pane %s: %v", j.target, err)
				mu.Unlock()
				return
			}
			sha, n, err := s.sb.Put(ctx, content)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("put scrollback: %w", err)
				}
				mu.Unlock()
				return
			}
			if err := s.db.UpsertScrollback(ctx, sha, n, m.SavedAt); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			m.Sessions[j.sessIdx].Windows[j.winIdx].Panes[j.paneIdx].ScrollbackSHA = sha
			mu.Unlock()
		}()
	}
	wg.Wait()
	return firstErr
}
