package tmux_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/noamsto/tmux-remux/internal/tmux"
)

func TestParseSessions(t *testing.T) {
	input := "lazytmux\x1f1745700000\x1f\nwork\x1f1745699000\x1f\n"
	got, err := tmux.ParseSessions(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []tmux.SessionRow{
		{Name: "lazytmux", LastAttached: 1745700000},
		{Name: "work", LastAttached: 1745699000},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseSessions mismatch (-want +got):\n%s", diff)
	}
}

func TestParseSessionsBridgeHost(t *testing.T) {
	input := "myhost-work\x1f1745700000\x1fmyhost\n"
	got, err := tmux.ParseSessions(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []tmux.SessionRow{
		{Name: "myhost-work", LastAttached: 1745700000, BridgeHost: "myhost"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseSessions mismatch (-want +got):\n%s", diff)
	}
}

func TestParseSessionsEmpty(t *testing.T) {
	got, err := tmux.ParseSessions("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestParseWindows(t *testing.T) {
	input := "lazytmux\x1f1\x1fmain\x1fabcd,200x50,0,0,1\x1f@4\x1f1\nwork\x1f2\x1fbuild\x1fefgh,80x24,0,0,2\x1f@7\x1f0\n"
	got, err := tmux.ParseWindows(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []tmux.WindowRow{
		{Session: "lazytmux", Index: 1, Name: "main", Layout: "abcd,200x50,0,0,1", ID: "@4", AutomaticRename: true},
		{Session: "work", Index: 2, Name: "build", Layout: "efgh,80x24,0,0,2", ID: "@7", AutomaticRename: false},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseWindows mismatch (-want +got):\n%s", diff)
	}
}

func TestParseWindowsDecoration(t *testing.T) {
	opts := []string{"@crew_name", "@crew_color"}
	line := strings.Join([]string{
		"sess", "2", "win", "layout", "@4", "1", "dispatcher", "colour141",
	}, tmux.FieldSep)
	rows, err := tmux.ParseWindows(line+"\n", opts)
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0].Decoration
	want := map[string]string{"@crew_name": "dispatcher", "@crew_color": "colour141"}
	if !maps.Equal(got, want) {
		t.Errorf("Decoration = %v, want %v", got, want)
	}
}

func TestParseWindowsDecorationEmptyDropped(t *testing.T) {
	opts := []string{"@crew_name", "@crew_color"}
	// @crew_color unset -> empty trailing field
	line := strings.Join([]string{
		"sess", "2", "win", "layout", "@4", "1", "dispatcher", "",
	}, tmux.FieldSep)
	rows, err := tmux.ParseWindows(line+"\n", opts)
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	want := map[string]string{"@crew_name": "dispatcher"}
	if !maps.Equal(rows[0].Decoration, want) {
		t.Errorf("Decoration = %v, want %v", rows[0].Decoration, want)
	}
}

func TestParseWindowsNoDecoration(t *testing.T) {
	line := strings.Join([]string{"sess", "2", "win", "layout", "@4", "1"}, tmux.FieldSep)
	rows, err := tmux.ParseWindows(line+"\n", nil)
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	if rows[0].Decoration != nil {
		t.Errorf("Decoration = %v, want nil", rows[0].Decoration)
	}
}

func TestParsePanes(t *testing.T) {
	input := "lazytmux\x1f1\x1f1\x1f/home/me\x1fnvim\x1f12345\x1f1745700000\x1f%3\x1f\nlazytmux\x1f1\x1f2\x1f/tmp\x1fclaude\x1f12346\x1f1745699000\x1f%9\x1fclaude --resume abc-123\n"
	got, err := tmux.ParsePanes(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []tmux.PaneRow{
		{Session: "lazytmux", WindowIndex: 1, PaneIndex: 1, Cwd: "/home/me", Command: "nvim", PID: 12345, LastUsed: 1745700000, ID: "%3"},
		{Session: "lazytmux", WindowIndex: 1, PaneIndex: 2, Cwd: "/tmp", Command: "claude", PID: 12346, LastUsed: 1745699000, ID: "%9", Relaunch: "claude --resume abc-123"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParsePanes mismatch (-want +got):\n%s", diff)
	}
}

func TestParseSessionsEmptyLastAttached(t *testing.T) {
	// tmux emits empty session_last_attached for sessions that have never been attached.
	got, err := tmux.ParseSessions("never-attached\x1f\x1f\nlazytmux\x1f1745700000\x1f\n")
	if err != nil {
		t.Fatalf("ParseSessions: %v", err)
	}
	want := []tmux.SessionRow{
		{Name: "never-attached", LastAttached: 0},
		{Name: "lazytmux", LastAttached: 1745700000},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestParsePanesEmptyLastUsed(t *testing.T) {
	// tmux emits empty pane_last_used for freshly-created panes.
	input := "s1\x1f1\x1f1\x1f/x\x1fbash\x1f1234\x1f\x1f%1\x1f\n"
	got, err := tmux.ParsePanes(input)
	if err != nil {
		t.Fatalf("ParsePanes: %v", err)
	}
	if got[0].LastUsed != 0 {
		t.Errorf("LastUsed = %d, want 0 for empty input", got[0].LastUsed)
	}
}
