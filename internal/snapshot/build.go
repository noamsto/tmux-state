package snapshot

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/noamsto/tmux-remux/internal/tmux"
)

// Lister is the subset of tmux.Client used by Build. Lets tests inject a fake.
type Lister interface {
	ListSessions(context.Context) ([]tmux.SessionRow, error)
	ListWindows(context.Context) ([]tmux.WindowRow, error)
	ListPanes(context.Context) ([]tmux.PaneRow, error)
}

// Build queries the live tmux server via l and returns a Manifest. ChildCount
// is populated best-effort from /proc; errors are ignored (missing PID just
// leaves it zero).
func Build(ctx context.Context, l Lister, host string, savedAt int64) (Manifest, error) {
	var sessions []tmux.SessionRow
	var windows []tmux.WindowRow
	var panes []tmux.PaneRow

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		s, err := l.ListSessions(gctx)
		sessions = s
		return err
	})
	g.Go(func() error {
		w, err := l.ListWindows(gctx)
		windows = w
		return err
	})
	g.Go(func() error {
		p, err := l.ListPanes(gctx)
		panes = p
		return err
	})
	if err := g.Wait(); err != nil {
		return Manifest{}, err
	}

	m := Manifest{V: 1, Host: host, SavedAt: savedAt}

	winsBySess := map[string][]tmux.WindowRow{}
	for _, w := range windows {
		winsBySess[w.Session] = append(winsBySess[w.Session], w)
	}
	pansByWin := map[string]map[int][]tmux.PaneRow{}
	for _, p := range panes {
		if pansByWin[p.Session] == nil {
			pansByWin[p.Session] = map[int][]tmux.PaneRow{}
		}
		pansByWin[p.Session][p.WindowIndex] = append(pansByWin[p.Session][p.WindowIndex], p)
	}

	for _, s := range sessions {
		if s.BridgeHost != "" {
			continue
		}
		sess := Session{Name: s.Name, LastAttached: s.LastAttached}
		for _, w := range winsBySess[s.Name] {
			win := Window{Index: w.Index, Name: w.Name, Layout: w.Layout, ID: w.ID, AutomaticRename: w.AutomaticRename, Decoration: w.Decoration}
			for _, p := range pansByWin[s.Name][w.Index] {
				cc, _ := ChildCount(p.PID)
				win.Panes = append(win.Panes, Pane{
					Index: p.PaneIndex, Cwd: p.Cwd, Command: p.Command,
					LastUsed:   p.LastUsed,
					ChildCount: cc,
					ID:         p.ID,
					Relaunch:   p.Relaunch,
				})
			}
			sess.Windows = append(sess.Windows, win)
		}
		m.Sessions = append(m.Sessions, sess)
	}
	return m, nil
}
