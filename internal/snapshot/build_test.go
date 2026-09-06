package snapshot_test

import (
	"context"
	"maps"
	"os"
	"testing"

	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/tmux"
)

type fakeClient struct {
	sessions []tmux.SessionRow
	windows  []tmux.WindowRow
	panes    []tmux.PaneRow
}

func (f *fakeClient) ListSessions(context.Context) ([]tmux.SessionRow, error) {
	return f.sessions, nil
}
func (f *fakeClient) ListWindows(context.Context) ([]tmux.WindowRow, error) { return f.windows, nil }
func (f *fakeClient) ListPanes(context.Context) ([]tmux.PaneRow, error)     { return f.panes, nil }

func TestBuildAssemblesTree(t *testing.T) {
	fc := &fakeClient{
		sessions: []tmux.SessionRow{
			{Name: "s1", LastAttached: 100},
		},
		windows: []tmux.WindowRow{
			{Session: "s1", Index: 1, Name: "main", Layout: "L"},
		},
		panes: []tmux.PaneRow{
			{Session: "s1", WindowIndex: 1, PaneIndex: 1, Cwd: "/home", Command: "nvim", PID: 1234, LastUsed: 99},
			{Session: "s1", WindowIndex: 1, PaneIndex: 2, Cwd: "/tmp", Command: "bash", PID: 1235, LastUsed: 50},
		},
	}
	m, err := snapshot.Build(context.Background(), fc, "host1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if m.Host != "host1" || m.SavedAt != 200 {
		t.Errorf("envelope wrong: %+v", m)
	}
	if len(m.Sessions) != 1 || m.Sessions[0].Name != "s1" {
		t.Fatalf("sessions: %+v", m.Sessions)
	}
	if len(m.Sessions[0].Windows) != 1 {
		t.Fatalf("windows: %+v", m.Sessions[0].Windows)
	}
	if len(m.Sessions[0].Windows[0].Panes) != 2 {
		t.Fatalf("panes: %+v", m.Sessions[0].Windows[0].Panes)
	}
}

func TestBuildCarriesWindowAndPaneIDs(t *testing.T) {
	fc := &fakeClient{
		sessions: []tmux.SessionRow{{Name: "s1"}},
		windows:  []tmux.WindowRow{{Session: "s1", Index: 1, ID: "@4"}},
		panes:    []tmux.PaneRow{{Session: "s1", WindowIndex: 1, PaneIndex: 1, ID: "%7"}},
	}
	m, err := snapshot.Build(context.Background(), fc, "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	w := m.Sessions[0].Windows[0]
	if w.ID != "@4" {
		t.Errorf("window ID = %q, want @4", w.ID)
	}
	if w.Panes[0].ID != "%7" {
		t.Errorf("pane ID = %q, want %%7", w.Panes[0].ID)
	}
}

func TestBuildCarriesDecoration(t *testing.T) {
	fc := &fakeClient{
		sessions: []tmux.SessionRow{{Name: "s1"}},
		windows: []tmux.WindowRow{
			{Session: "s1", Index: 1, Decoration: map[string]string{"@crew_color": "colour141"}},
		},
	}
	m, err := snapshot.Build(context.Background(), fc, "h", 1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"@crew_color": "colour141"}
	got := m.Sessions[0].Windows[0].Decoration
	if !maps.Equal(got, want) {
		t.Errorf("Decoration = %v, want %v", got, want)
	}
}

func TestBuildSkipsBridgeSessions(t *testing.T) {
	tests := []struct {
		name     string
		sessions []tmux.SessionRow
		windows  []tmux.WindowRow
		panes    []tmux.PaneRow
		want     []string // session names expected in the manifest
	}{
		{
			name:     "bridge session alone",
			sessions: []tmux.SessionRow{{Name: "host-remote", BridgeHost: "host"}},
			windows:  []tmux.WindowRow{{Session: "host-remote", Index: 1}},
			panes:    []tmux.PaneRow{{Session: "host-remote", WindowIndex: 1, PaneIndex: 1}},
			want:     nil,
		},
		{
			name: "bridge session mixed with normal",
			sessions: []tmux.SessionRow{
				{Name: "host-remote", BridgeHost: "host"},
				{Name: "local", LastAttached: 5},
			},
			windows: []tmux.WindowRow{
				{Session: "host-remote", Index: 1},
				{Session: "local", Index: 1},
			},
			panes: []tmux.PaneRow{
				{Session: "host-remote", WindowIndex: 1, PaneIndex: 1},
				{Session: "local", WindowIndex: 1, PaneIndex: 1},
			},
			want: []string{"local"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{sessions: tt.sessions, windows: tt.windows, panes: tt.panes}
			m, err := snapshot.Build(context.Background(), fc, "h", 1)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, s := range m.Sessions {
				got = append(got, s.Name)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("sessions = %v, want %v", got, tt.want)
			}
			for i, name := range tt.want {
				if got[i] != name {
					t.Errorf("sessions[%d] = %q, want %q", i, got[i], name)
				}
			}
			for _, s := range m.Sessions {
				if s.Name == "host-remote" {
					t.Errorf("bridge session leaked into manifest: %+v", s)
				}
			}
			for _, s := range m.Sessions {
				if len(s.Windows) == 0 {
					t.Errorf("session %q has no windows, want its own windows/panes intact", s.Name)
				}
			}
		})
	}
}

func TestBuildPopulatesChildCountFromPID(t *testing.T) {
	// Use the current process PID as a sentinel — it has at least 0 children
	// and we can verify the field is set (not whatever the zero value is from
	// an uninitialized PID).
	selfPID := os.Getpid()
	fc := &fakeClient{
		sessions: []tmux.SessionRow{{Name: "s"}},
		windows:  []tmux.WindowRow{{Session: "s", Index: 1}},
		panes:    []tmux.PaneRow{{Session: "s", WindowIndex: 1, PaneIndex: 1, PID: selfPID}},
	}
	m, err := snapshot.Build(context.Background(), fc, "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	// ChildCount should equal the actual count for this PID (>=0, deterministic).
	expected, _ := snapshot.ChildCount(selfPID)
	if m.Sessions[0].Windows[0].Panes[0].ChildCount != expected {
		t.Errorf("ChildCount = %d, want %d", m.Sessions[0].Windows[0].Panes[0].ChildCount, expected)
	}
}
