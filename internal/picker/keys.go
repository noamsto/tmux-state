package picker

import "charm.land/bubbles/v2/key"

type keyMap struct {
	// mode drops the bindings that are inert in close mode from the help
	// listings. The zero value, ModeSnapshot, lists them all.
	mode                      Mode
	Up, Down                  key.Binding
	Left, Right               key.Binding
	Tab                       key.Binding
	ToggleIdle                key.Binding
	ToggleSkipRunning         key.Binding
	ToggleAge                 key.Binding
	PreviewUp, PreviewDown    key.Binding
	PreviewLeft, PreviewRight key.Binding
	Enter                     key.Binding
	Help                      key.Binding
	Quit                      key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:                key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:              key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Left:              key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "collapse")),
		Right:             key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "expand")),
		Tab:               key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch pane")),
		ToggleIdle:        key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "skip idle")),
		ToggleSkipRunning: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "skip running")),
		ToggleAge:         key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "age ≤24h")),
		PreviewUp:         key.NewBinding(key.WithKeys("alt+k", "pgup"), key.WithHelp("M-k/PgUp", "preview ↑")),
		PreviewDown:       key.NewBinding(key.WithKeys("alt+j", "pgdown"), key.WithHelp("M-j/PgDn", "preview ↓")),
		PreviewLeft:       key.NewBinding(key.WithKeys("alt+h"), key.WithHelp("M-h", "preview ←")),
		PreviewRight:      key.NewBinding(key.WithKeys("alt+l"), key.WithHelp("M-l", "preview →")),
		Enter:             key.NewBinding(key.WithKeys("enter"), key.WithHelp("↵", "restore")),
		Help:              key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:              key.NewBinding(key.WithKeys("esc", "ctrl+c", "q"), key.WithHelp("q/esc", "quit")),
	}
}

// ShortHelp / FullHelp satisfy bubbles' help.KeyMap. Only FullHelp reaches a
// frame — the `?` overlay forces ShowAll and the footer writes its own hints —
// but both must agree, so both drop the same bindings in close mode.
func (k keyMap) ShortHelp() []key.Binding {
	if k.mode == ModeClose {
		return []key.Binding{k.Up, k.Down, k.Enter, k.Help, k.Quit}
	}
	return []key.Binding{k.Up, k.Down, k.Right, k.Tab, k.Enter, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	nav := []key.Binding{k.Up, k.Down}
	if k.mode != ModeClose {
		nav = append(nav, k.Left, k.Right, k.Tab)
	}
	groups := [][]key.Binding{
		nav,
		{k.PreviewUp, k.PreviewDown, k.PreviewLeft, k.PreviewRight},
	}
	if k.mode != ModeClose {
		groups = append(groups, []key.Binding{k.ToggleIdle, k.ToggleSkipRunning, k.ToggleAge})
	}
	return append(groups, []key.Binding{k.Enter, k.Help, k.Quit})
}
