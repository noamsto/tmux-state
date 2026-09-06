// Package tmux wraps shelling out to the tmux CLI.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ErrNoServer is returned by Run (and surfaced via ListSessions/Windows/Panes
// as a nil result with no error) when tmux reports that no server is running
// on the target socket. Two phrasings exist in tmux 3.x:
//
//   - "no server running on /path"        socket file exists but no server
//   - "error connecting to /path (...)"  socket file does not exist
//
// Callers that want to handle "no server" as an error rather than empty state
// can match with errors.Is.
var ErrNoServer = errors.New("tmux: no server running")

// Client invokes a tmux-compatible binary. Defaults to the "tmux" command
// when binary is empty.
type Client struct {
	binary         string
	decorationOpts []string
}

// NewClient returns a Client that invokes binary; if empty, "tmux" is used.
// Each decorationOpts entry is wrapped as a #{opt} field appended to the
// list-windows format and captured into WindowRow.Decoration (names are
// typically @-prefixed user options, e.g. "@crew_color").
func NewClient(binary string, decorationOpts ...string) *Client {
	if binary == "" {
		binary = "tmux"
	}
	return &Client{binary: binary, decorationOpts: decorationOpts}
}

// Run executes the binary with the given args and returns stdout. Non-zero
// exit with a "no server" stderr is mapped to ErrNoServer; other failures are
// wrapped with stderr.
//
// When TMUX is not set in the calling environment, Run synthesizes a TMUX
// value pointing at the default socket. tmux 3.x rewrites control bytes
// (0x01-0x1f) in -F format output to "_" when invoked outside a client,
// which silently corrupts the \x1f field separator used by ParseSessions and
// friends. Setting TMUX to any non-empty value with a real socket path makes
// tmux preserve those bytes. See parse.go FieldSep.
func (c *Client) Run(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binary, args...) //nolint:gosec // binary and args are trusted callers
	cmd.Env = withSynthesizedTmuxEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if isNoServerStderr(msg) {
			return "", ErrNoServer
		}
		return "", fmt.Errorf("%s %v: %w (stderr: %s)", c.binary, args, err, msg)
	}
	return stdout.String(), nil
}

func isNoServerStderr(s string) bool {
	return strings.Contains(s, "no server running on ") ||
		strings.Contains(s, "error connecting to ")
}

// withSynthesizedTmuxEnv returns env unchanged when TMUX is already set,
// otherwise appends a synthesized TMUX=<socket>,0,0 entry. The socket path
// follows tmux's own default-socket logic: $TMUX_TMPDIR/tmux-<UID>/default,
// with /tmp as the TMUX_TMPDIR fallback. The pid/session-id components are
// dummies — tmux only checks that TMUX is non-empty and that the socket path
// resolves to a running server.
func withSynthesizedTmuxEnv(env []string) []string {
	tmpdir := ""
	for _, e := range env {
		if strings.HasPrefix(e, "TMUX=") {
			return env
		}
		if v, ok := strings.CutPrefix(e, "TMUX_TMPDIR="); ok {
			tmpdir = v
		}
	}
	if tmpdir == "" {
		tmpdir = "/tmp"
	}
	return append(env, fmt.Sprintf("TMUX=%s/tmux-%d/default,0,0", tmpdir, os.Getuid()))
}

const (
	sessionFormat    = "#{session_name}" + FieldSep + "#{session_last_attached}" + FieldSep + "#{@bridge_host}"
	baseWindowFormat = "#{session_name}" + FieldSep + "#{window_index}" + FieldSep + "#{window_name}" + FieldSep + "#{window_layout}" + FieldSep + "#{window_id}" + FieldSep + "#{E:automatic-rename}"
	paneFormat       = "#{session_name}" + FieldSep + "#{window_index}" + FieldSep + "#{pane_index}" + FieldSep + "#{pane_current_path}" + FieldSep + "#{pane_current_command}" + FieldSep + "#{pane_pid}" + FieldSep + "#{pane_last_used}" + FieldSep + "#{pane_id}" + FieldSep + "#{@remux_relaunch}"
)

// WindowFormat returns the list-windows -F format, with one #{@opt} field per
// decoration option appended in order.
func (c *Client) WindowFormat() string {
	f := baseWindowFormat
	for _, o := range c.decorationOpts {
		f += FieldSep + "#{" + o + "}"
	}
	return f
}

// ListSessions runs `tmux list-sessions -F …` and parses the result.
// Returns (nil, nil) when no tmux server is running; propagates other errors.
func (c *Client) ListSessions(ctx context.Context) ([]SessionRow, error) {
	out, err := c.Run(ctx, []string{"list-sessions", "-F", sessionFormat})
	if errors.Is(err, ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseSessions(out)
}

// ListWindows runs `tmux list-windows -a -F …` and parses the result.
// Returns (nil, nil) when no tmux server is running; propagates other errors.
func (c *Client) ListWindows(ctx context.Context) ([]WindowRow, error) {
	out, err := c.Run(ctx, []string{"list-windows", "-a", "-F", c.WindowFormat()})
	if errors.Is(err, ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseWindows(out, c.decorationOpts)
}

// ListPanes runs `tmux list-panes -a -F …` and parses the result.
// Returns (nil, nil) when no tmux server is running; propagates other errors.
func (c *Client) ListPanes(ctx context.Context) ([]PaneRow, error) {
	out, err := c.Run(ctx, []string{"list-panes", "-a", "-F", paneFormat})
	if errors.Is(err, ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePanes(out)
}

// CapturePane returns the scrollback contents of a pane as raw bytes.
// target format: <session>:<window_index>.<pane_index>
func (c *Client) CapturePane(ctx context.Context, target string) ([]byte, error) {
	out, err := c.Run(ctx, []string{"capture-pane", "-pJ", "-t", target, "-S", "-"})
	if err != nil {
		return nil, fmt.Errorf("capture-pane %q: %w", target, err)
	}
	return []byte(out), nil
}

// ServerStartTime returns the running server's start time in Unix
// milliseconds, via the server-scoped #{start_time} format (epoch seconds).
// Restore uses it to anchor snapshot selection to "before this server
// existed", so snapshots written by the current server's own save hooks can
// never be selected (the save/restore race at server birth).
func (c *Client) ServerStartTime(ctx context.Context) (int64, error) {
	out, err := c.Run(ctx, []string{"display-message", "-p", "#{start_time}"})
	if err != nil {
		return 0, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse start_time %q: %w", strings.TrimSpace(out), err)
	}
	return secs * 1000, nil
}

// SetPaneOption sets a pane-scoped option (tmux set-option -p). The -q flag
// keeps it quiet: a pane that vanished between a hook firing and this call must
// not surface an error to the caller. Pass value="" to unset.
func (c *Client) SetPaneOption(ctx context.Context, pane, name, value string) error {
	_, err := c.Run(ctx, []string{"set-option", "-pq", "-t", pane, name, value})
	return err
}
