// Package snapshot defines the manifest schema and the save/restore-relevant
// data shape for tmux state.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// tmuxFormatRE matches tmux-format directives like #[fg=#94e2d5] embedded in
// session/window names via automatic-rename-format. tmux evaluates them on
// render in the status bar, but the raw string is what gets stored in
// snapshots — so any UI rendering the stored name outside tmux must strip
// them first.
var tmuxFormatRE = regexp.MustCompile(`#\[[^\]]*\]`)

// StripFormat removes tmux-format directives from `s` and trims surrounding
// whitespace.
func StripFormat(s string) string {
	return strings.TrimSpace(tmuxFormatRE.ReplaceAllString(s, ""))
}

// Manifest is the top-level snapshot envelope persisted in events.manifest_json.
type Manifest struct {
	V       int    `json:"v"`
	Host    string `json:"host"`
	TmuxPID int    `json:"tmux_pid,omitempty"`
	SavedAt int64  `json:"saved_at"`
	// ScrollbackSkipped is true when this snapshot deliberately skipped
	// capturing pane scrollback (min_save_interval throttle), as opposed to
	// a pane simply never having had scrollback captured. The picker uses
	// this to tell "throttled" apart from "nothing captured yet" in the
	// preview.
	ScrollbackSkipped bool      `json:"scrollback_skipped,omitempty"`
	Sessions          []Session `json:"sessions"`
}

// Session captures one tmux session's structure.
type Session struct {
	Name         string   `json:"name"`
	LastAttached int64    `json:"last_attached"`
	Windows      []Window `json:"windows"`
}

// Window captures one tmux window's structure within a session.
type Window struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Layout string `json:"layout"`
	ID     string `json:"id,omitempty"` // tmux window id ("@4"); stable within a server lifetime
	// AutomaticRename records whether the window derived its name from
	// automatic-rename-format at save time. When true, restore re-enables
	// automatic-rename so the live format takes over again — otherwise
	// `new-window -n` would pin the stale stored name and disable the renamer.
	AutomaticRename bool              `json:"automatic_rename,omitempty"`
	Decoration      map[string]string `json:"decoration,omitempty"`
	Panes           []Pane            `json:"panes"`
}

// Pane captures one tmux pane's state, including optional scrollback hash.
type Pane struct {
	Index         int      `json:"index"`
	Cwd           string   `json:"cwd"`
	Command       string   `json:"command"`
	CommandArgs   []string `json:"command_args,omitempty"`
	LastUsed      int64    `json:"last_used"`
	ChildCount    int      `json:"child_count"`
	ScrollbackSHA string   `json:"scrollback_sha,omitempty"`
	ID            string   `json:"id,omitempty"`       // tmux pane id ("%7"); stable within a server lifetime
	Relaunch      string   `json:"relaunch,omitempty"` // @remux_relaunch override; exec'd verbatim on restore, bypassing the allow-list
}

// Fingerprint returns a sha256 hex of the manifest with timestamps zeroed,
// suitable for "did anything change since last save?" checks.
func (m Manifest) Fingerprint() string {
	cp := m
	cp.SavedAt = 0
	cp.Sessions = make([]Session, len(m.Sessions))
	for i, s := range m.Sessions {
		s2 := s
		s2.LastAttached = 0
		s2.Windows = make([]Window, len(s.Windows))
		for j, w := range s.Windows {
			w2 := w
			w2.Decoration = nil
			w2.Panes = make([]Pane, len(w.Panes))
			for k, p := range w.Panes {
				p2 := p
				p2.LastUsed = 0
				w2.Panes[k] = p2
			}
			s2.Windows[j] = w2
		}
		cp.Sessions[i] = s2
	}
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// StructureFingerprint returns a sha256 hex over the identity of every session,
// window, and pane — which entities exist, ignoring what they are doing (cwd,
// command, layout, scrollback). It changes when something is created or
// destroyed, and when a pane's @remux_relaunch stamp changes: both are the
// class of change a save must never drop. An entity that never reaches a
// snapshot can never be restored once it closes, and a relaunch stamped after
// the last snapshot (an agent start hook fires seconds into a fresh pane)
// would restore as a bare shell.
func (m Manifest) StructureFingerprint() string {
	var ids []string
	for _, s := range m.Sessions {
		ids = append(ids, "s\x1f"+s.Name)
		for _, w := range s.Windows {
			ids = append(ids, fmt.Sprintf("w\x1f%s\x1f%d", w.ID, w.Index))
			for _, p := range w.Panes {
				ids = append(ids, fmt.Sprintf("p\x1f%s\x1f%d\x1f%s", p.ID, p.Index, p.Relaunch))
			}
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x1e")))
	return hex.EncodeToString(sum[:])
}
