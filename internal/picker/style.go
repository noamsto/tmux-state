package picker

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Style globals are initialized by applyTheme. NewPickerModel calls applyTheme
// with NewTheme() so consumers get sensible defaults without wiring; tests can
// call applyTheme(Theme{}) for deterministic Mocha colors.
var (
	listFrame    lipgloss.Style
	treeFrame    lipgloss.Style
	previewFrame lipgloss.Style

	rowActive  lipgloss.Style
	rowDefault lipgloss.Style
	rowDim     lipgloss.Style
	// rowScaffold styles close-tree rows that exist only to parent something
	// restorable. Quieter than body text, but never faint — dimming is how the
	// picker says "old", and these rows are not old, just not targets.
	rowScaffold lipgloss.Style

	nodeSession lipgloss.Style
	nodeWindow  lipgloss.Style
	nodePane    lipgloss.Style
	skipReason  lipgloss.Style

	footerBar  lipgloss.Style
	footerWarn lipgloss.Style
	footerOn   lipgloss.Style
	footerOff  lipgloss.Style
	footerKey  lipgloss.Style
	footerSep  lipgloss.Style
	keyCast    lipgloss.Style

	previewHeader lipgloss.Style

	// closeRailStyles and closeLabelStyles colour the stacked close preview's
	// pane blocks. Indexed by block position and cycled, so two blocks are
	// told apart by colour without reading their labels.
	closeRailStyles  []lipgloss.Style
	closeLabelStyles []lipgloss.Style
)

func init() { applyTheme(Theme{}) }

func applyTheme(t Theme) {
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Surface1()).
		Padding(0, 1)
	listFrame = border
	treeFrame = border
	previewFrame = border

	rowActive = lipgloss.NewStyle().Foreground(t.Base()).Background(t.Mauve()).Bold(true)
	rowDefault = lipgloss.NewStyle().Foreground(t.Text())
	rowDim = lipgloss.NewStyle().Foreground(t.Overlay())
	rowScaffold = lipgloss.NewStyle().Foreground(t.Subtext())

	nodeSession = lipgloss.NewStyle().Foreground(t.Mauve()).Bold(true)
	nodeWindow = lipgloss.NewStyle().Foreground(t.Blue())
	nodePane = lipgloss.NewStyle().Foreground(t.Text())
	skipReason = lipgloss.NewStyle().Foreground(t.Subtext()).Italic(true)

	footerBar = lipgloss.NewStyle().Foreground(t.Subtext()).Padding(0, 1)
	footerWarn = lipgloss.NewStyle().Foreground(t.Red()).Bold(true)
	footerOn = lipgloss.NewStyle().Foreground(t.Green())
	footerOff = lipgloss.NewStyle().Foreground(t.Overlay())
	footerKey = lipgloss.NewStyle().Foreground(t.Lavender())
	footerSep = lipgloss.NewStyle().Foreground(t.Overlay())
	keyCast = lipgloss.NewStyle().Foreground(t.Base()).Background(t.Mauve()).Bold(true)

	previewHeader = lipgloss.NewStyle().Foreground(t.Blue()).Bold(true)

	accents := []color.Color{t.Blue(), t.Yellow()}
	closeRailStyles = make([]lipgloss.Style, len(accents))
	closeLabelStyles = make([]lipgloss.Style, len(accents))
	for i, a := range accents {
		closeRailStyles[i] = lipgloss.NewStyle().Foreground(a)
		closeLabelStyles[i] = lipgloss.NewStyle().Foreground(t.Base()).Background(a).Bold(true)
	}
}
