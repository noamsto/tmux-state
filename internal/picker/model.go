package picker

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/noamsto/tmux-remux/internal/filter"
	"github.com/noamsto/tmux-remux/internal/scrollback"
	"github.com/noamsto/tmux-remux/internal/snapshot"
	"github.com/noamsto/tmux-remux/internal/store"
)

// Mode is "snapshot" (tree pane visible) or "close" (list-only).
type Mode int

// Picker modes select which event kind drives the UI.
const (
	ModeSnapshot Mode = iota // restore-from-snapshot picker (prefix+R)
	ModeClose                // restore-from-close-event picker (prefix+U)
)

type focusZone int

const (
	focusList focusZone = iota
	focusTree
)

// FocusZone aliases focusZone for tests.
type FocusZone = focusZone

// Focus-zone constants exported for tests.
const (
	FocusList = focusList
	FocusTree = focusTree
)

// PickerModel is the Bubble Tea model for the restore picker.
//
//revive:disable-next-line:exported other callers reference picker.PickerModel
type PickerModel struct {
	mode             Mode
	events           []store.Event
	cursor           int
	treeCursor       int                         // index into the flattened visible-node list
	manifests        map[int64]snapshot.Manifest // lazy parse cache
	trees            map[int64]*TreeNode         // lazy build cache
	manifestErrors   map[int64]error             // remember parse failures
	filter           filter.Filter
	dimOlderThan     time.Duration // list-pane only; 0 = no dimming
	runningSet       map[string]bool
	keys             keyMap
	help             help.Model
	width, height    int
	focus            focusZone
	showHelp         bool
	footerNote       string // transient warning text
	selectedID       int64  // 0 = no selection (cancelled)
	scrollbackStore  *scrollback.Store
	scrollbacks      map[string][]byte // sha → bytes
	scrollbackErrors map[string]error  // sha → load error
	loadingSHAs      map[string]bool   // sha → in-flight load
	previewScroll    int               // lines scrolled up from the tail; 0 = bottom
	previewScrollX   int               // visible cells shifted right; 0 = left edge
	// closeContexts holds per-close-event derived data: a short human label
	// and the sub-manifest of what was lost. Populated by SetCloseContexts
	// before Bootstrap. Keys are store.Event IDs. Empty in snapshot mode.
	closeContexts map[int64]CloseContext
	// closeRows is the flat, newest-first close list rendered in close mode.
	// When the mode is ModeClose, m.cursor indexes closeRows rather than
	// m.events. Empty when nothing recoverable was closed.
	closeRows []CloseRow
	// hiddenCount is the number of unrecoverable close events the caller
	// filtered out before constructing the model. Rendered as a footer line so
	// the user knows the list is pruned. Close mode only.
	hiddenCount int
	// demoKeys echoes the last key pressed into the footer, for screen
	// recordings where the viewer can't see the keyboard. Off unless
	// REMUX_DEMO_KEYS is set; never on in normal use.
	demoKeys bool
	lastKey  string
}

// CloseContext is the picker-facing summary of a single close event, used to
// render rich row labels and a preview-pane tree. The Label is shown alongside
// the timestamp; SubManifest is rendered as the close-mode preview tree and
// is also what restore.BuildPlan operates on when Enter is pressed.
type CloseContext struct {
	Label       string
	Placement   ClosePlacement
	SubManifest snapshot.Manifest
}

// NewPickerModel builds the initial state. The caller is responsible for
// fetching events and the running session set before constructing it.
func NewPickerModel(mode Mode, events []store.Event, running map[string]bool, sb *scrollback.Store) PickerModel {
	applyTheme(NewTheme())
	return PickerModel{
		mode:             mode,
		events:           events,
		manifests:        make(map[int64]snapshot.Manifest, len(events)),
		trees:            make(map[int64]*TreeNode, len(events)),
		manifestErrors:   make(map[int64]error),
		filter:           filter.Filter{SkipRunningSessions: true},
		dimOlderThan:     24 * time.Hour,
		runningSet:       running,
		keys:             defaultKeys(),
		help:             help.New(),
		focus:            focusList,
		scrollbackStore:  sb,
		scrollbacks:      make(map[string][]byte),
		scrollbackErrors: make(map[string]error),
		loadingSHAs:      make(map[string]bool),
		demoKeys:         os.Getenv("REMUX_DEMO_KEYS") != "",
	}
}

// ScrollbackStore returns the scrollback store passed to the constructor.
// Exported for tests; production code does not call this.
func (m PickerModel) ScrollbackStore() *scrollback.Store { return m.scrollbackStore }

// ScrollbackFor returns the cached scrollback bytes for sha and whether the
// entry was present.
func (m PickerModel) ScrollbackFor(sha string) ([]byte, bool) {
	b, ok := m.scrollbacks[sha]
	return b, ok
}

// ScrollbackError returns the cached load error for sha, or nil.
func (m PickerModel) ScrollbackError(sha string) error { return m.scrollbackErrors[sha] }

// Init satisfies tea.Model. Close mode's preview is live from the first frame,
// so the cursor's scrollback has to be scheduled before any key arrives —
// otherwise the panel falls back to the window map until the user moves.
func (m PickerModel) Init() tea.Cmd { return (&m).PreviewCmd() }

// Update handles key events. Implementation grows across the next few tasks.
func (m PickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case scrollbackLoadedMsg:
		delete(m.loadingSHAs, msg.sha)
		if msg.err != nil {
			m.scrollbackErrors[msg.sha] = msg.err
		} else {
			m.scrollbacks[msg.sha] = msg.content
		}
		return m, nil
	case tea.KeyPressMsg:
		if m.demoKeys {
			m.lastKey = keyLabel(msg)
		}
		return m.handleKey(msg)
	case tea.MouseWheelMsg:
		if m.mode != ModeSnapshot {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			inner := m.paneScrollbackHeight()
			maxScroll := m.previewMaxScroll(inner)
			m.previewScroll += 3
			if m.previewScroll > maxScroll {
				m.previewScroll = maxScroll
			}
			return m, nil
		case tea.MouseWheelDown:
			m.previewScroll -= 3
			if m.previewScroll < 0 {
				m.previewScroll = 0
			}
			return m, nil
		}
	}
	return m, nil
}

// visibleNodes flattens the current tree honoring Expanded.
func (m PickerModel) visibleNodes() []*TreeNode {
	// Keyed off CurrentEventID, not m.events[m.cursor]: in close mode the
	// cursor indexes closeRows, so indexing the event slice with it looks up
	// the wrong tree (or none) and the preview finds no pane to show.
	id := m.CurrentEventID()
	if id == 0 {
		return nil
	}
	tree := m.trees[id]
	if tree == nil {
		return nil
	}
	var out []*TreeNode
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		out = append(out, n)
		if n.Expanded {
			for _, c := range n.Children {
				walk(c)
			}
		}
	}
	for _, sess := range tree.Children {
		walk(sess)
	}
	return out
}

// VisibleNodes exports visibleNodes for tests.
func (m PickerModel) VisibleNodes() []*TreeNode { return m.visibleNodes() }

// firstPaneIdx returns the index of the first visible NodePane, or 0 if none.
func (m PickerModel) firstPaneIdx() int {
	for i, n := range m.visibleNodes() {
		if n.Kind == NodePane {
			return i
		}
	}
	return 0
}

// isNavTarget reports whether `n` is a valid Up/Down landing spot in tree
// focus: panes (scrollback preview), window nodes (the layout map lives there),
// and collapsed non-leaf nodes (so the user can step onto a collapsed
// window/session and press Right to re-expand). An expanded session is passed
// over — its only preview is a hint — so Up/Down step window → panes → window.
func isNavTarget(n *TreeNode) bool {
	if n.Kind == NodePane || n.Kind == NodeWindow {
		return true
	}
	return !n.Expanded && len(n.Children) > 0
}

// keyLabel is a compact, readable name for a key press, shown in the footer
// when demoKeys is on so a screen recording can convey what was pressed.
func keyLabel(msg tea.KeyPressMsg) string {
	switch s := msg.String(); s {
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	case "enter":
		return "↵"
	case "tab":
		return "⇥"
	case " ", "space":
		return "space"
	default:
		return s
	}
}

// nextPaneIdx walks from `start` in `dir` (+1 or -1) and returns the next
// navigable index — see isNavTarget. Returns -1 if none in that direction.
func (m PickerModel) nextPaneIdx(start, dir int) int {
	nodes := m.visibleNodes()
	for i := start + dir; i >= 0 && i < len(nodes); i += dir {
		if isNavTarget(nodes[i]) {
			return i
		}
	}
	return -1
}

// indexOf returns the visible-tree index of `target`, or -1 if `target` isn't
// currently visible (e.g., an ancestor was collapsed).
func (m PickerModel) indexOf(target *TreeNode) int {
	for i, n := range m.visibleNodes() {
		if n == target {
			return i
		}
	}
	return -1
}

// firstPaneIdxIn returns the visible-tree index of the first NodePane that
// descends from `subtree`. Falls back to `indexOf(subtree)` if no pane is
// visible underneath (everything below it is collapsed).
func (m PickerModel) firstPaneIdxIn(subtree *TreeNode) int {
	nodes := m.visibleNodes()
	rootIdx := -1
	seenRoot := false
	for i, n := range nodes {
		if n == subtree {
			rootIdx = i
			seenRoot = true
			continue
		}
		if !seenRoot {
			continue
		}
		// Once we step outside the subtree, stop.
		if !descendsFrom(n, subtree) {
			break
		}
		if n.Kind == NodePane {
			return i
		}
	}
	return rootIdx
}

func descendsFrom(n, ancestor *TreeNode) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

func (m PickerModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Footer notes are transient by design: clear on any key so a stale
	// "(invalid manifest)" doesn't follow the user as they navigate to a
	// recoverable row. Handlers that want to surface a fresh note set it
	// after this clear.
	m.footerNote = ""
	// Close mode: the cursor walks closeRows, skipping section headers. There
	// is no hierarchy to expand or collapse, so Left/Right fall through
	// unhandled — Up/Down/Enter is the whole nav story. The branch claims
	// every close-mode Up/Down/Enter even with no rows to walk, so an empty
	// list can never fall through to the snapshot handler below and start
	// paging m.events, which in close mode holds the unrecoverable events the
	// list deliberately does not show.
	if m.mode == ModeClose {
		switch {
		case key.Matches(msg, m.keys.Up):
			if idx := m.nextCloseRowIdx(m.cursor, -1); idx >= 0 {
				m.cursor = idx
				m.previewScroll = 0
				m.previewScrollX = 0
				(&m).ensureManifest()
			}
			return m, (&m).PreviewCmd()
		case key.Matches(msg, m.keys.Down):
			if idx := m.nextCloseRowIdx(m.cursor, +1); idx >= 0 {
				m.cursor = idx
				m.previewScroll = 0
				m.previewScrollX = 0
				(&m).ensureManifest()
			}
			return m, (&m).PreviewCmd()
		case key.Matches(msg, m.keys.Enter):
			if m.cursor < 0 || m.cursor >= len(m.closeRows) {
				return m, nil
			}
			// Every selectable row carries an EventID and Up/Down never stop
			// on a header, so there's nothing left for Enter to refuse.
			if id := m.closeRows[m.cursor].EventID; id != 0 {
				m.selectedID = id
				return m, tea.Quit
			}
			return m, nil
		}
	}
	// Preview scroll: Alt+J/K / PgUp/PgDn — scroll up to read past output
	// without leaving the cursor pane. Both modes preview scrollback.
	if m.mode == ModeSnapshot || m.mode == ModeClose {
		switch {
		case key.Matches(msg, m.keys.PreviewUp):
			inner := m.paneScrollbackHeight()
			maxScroll := m.previewMaxScroll(inner)
			if m.previewScroll < maxScroll {
				m.previewScroll++
				if m.previewScroll > maxScroll {
					m.previewScroll = maxScroll
				}
			}
			return m, nil
		case key.Matches(msg, m.keys.PreviewDown):
			if m.previewScroll > 0 {
				m.previewScroll--
			}
			return m, nil
		case key.Matches(msg, m.keys.PreviewLeft):
			if m.previewScrollX > 0 {
				m.previewScrollX -= 8
				if m.previewScrollX < 0 {
					m.previewScrollX = 0
				}
			}
			return m, nil
		case key.Matches(msg, m.keys.PreviewRight):
			m.previewScrollX += 8
			return m, nil
		}
	}
	// Focus-tree key handling: intercept Up/Down/Left/Right so they walk the
	// manifest tree's panes. Snapshot mode only: nothing moves focus off
	// focusList in close mode.
	if m.mode == ModeSnapshot && m.focus == focusTree {
		switch {
		case key.Matches(msg, m.keys.Up):
			if idx := m.nextPaneIdx(m.treeCursor, -1); idx >= 0 {
				m.treeCursor = idx
				m.previewScroll = 0
				m.previewScrollX = 0
			}
			return m, (&m).PreviewCmd()
		case key.Matches(msg, m.keys.Down):
			if idx := m.nextPaneIdx(m.treeCursor, +1); idx >= 0 {
				m.treeCursor = idx
				m.previewScroll = 0
				m.previewScrollX = 0
			}
			return m, (&m).PreviewCmd()
		case key.Matches(msg, m.keys.Right):
			nodes := m.visibleNodes()
			if m.treeCursor >= 0 && m.treeCursor < len(nodes) {
				n := nodes[m.treeCursor]
				switch {
				case !n.Expanded && len(n.Children) > 0:
					// Cursor on a collapsed parent: expand and dive to the first
					// pane within.
					n.Expanded = true
					m.treeCursor = m.firstPaneIdxIn(n)
				case n.Expanded && len(n.Children) > 0:
					// On an already-expanded window/session (the map is showing):
					// dive to the first pane within, without collapsing.
					m.treeCursor = m.firstPaneIdxIn(n)
				case n.Kind == NodePane:
					// On a leaf pane: nothing to expand. No-op.
				}
			}
			return m, (&m).PreviewCmd()
		case key.Matches(msg, m.keys.Left):
			nodes := m.visibleNodes()
			if m.treeCursor >= 0 && m.treeCursor < len(nodes) {
				// Walk up from cursor to the nearest expanded ancestor with
				// children, collapse it, and move the cursor to it. From a pane
				// this collapses the parent window; pressing Left again on the
				// window collapses the parent session.
				for n := nodes[m.treeCursor]; n != nil; n = n.Parent {
					if n.Parent == nil {
						break // don't collapse the synthetic root
					}
					if n.Expanded && len(n.Children) > 0 {
						n.Expanded = false
						m.treeCursor = m.indexOf(n)
						break
					}
				}
			}
			return m, (&m).PreviewCmd()
		}
	}

	switch {
	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.events)-1 {
			m.cursor++
			(&m).ensureManifest()
			m.treeCursor = m.firstPaneIdx()
			m.previewScroll = 0
		}
		return m, (&m).PreviewCmd()
	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
			(&m).ensureManifest()
			m.treeCursor = m.firstPaneIdx()
			m.previewScroll = 0
		}
		return m, (&m).PreviewCmd()
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Tab):
		// Snapshot mode only: close mode is two columns with no second tree to
		// reach, and its preview scrolls with Alt+j/k regardless of focus.
		if m.mode == ModeSnapshot {
			if m.focus == focusList {
				m.focus = focusTree
				nodes := m.visibleNodes()
				if m.treeCursor < 0 || m.treeCursor >= len(nodes) || nodes[m.treeCursor].Kind != NodePane {
					m.treeCursor = m.firstPaneIdx()
				}
			} else {
				m.focus = focusList
			}
		}
		return m, (&m).PreviewCmd()
	case key.Matches(msg, m.keys.ToggleIdle):
		m.filter.SkipIdleShells = !m.filter.SkipIdleShells
		(&m).redecorate()
		return m, nil
	case key.Matches(msg, m.keys.ToggleSkipRunning):
		m.filter.SkipRunningSessions = !m.filter.SkipRunningSessions
		(&m).redecorate()
		return m, nil
	case key.Matches(msg, m.keys.ToggleAge):
		if m.dimOlderThan == 0 {
			m.dimOlderThan = 24 * time.Hour
		} else {
			m.dimOlderThan = 0
		}
		return m, nil
	case key.Matches(msg, m.keys.Help):
		m.showHelp = !m.showHelp
		return m, nil
	case key.Matches(msg, m.keys.Enter):
		if m.cursor < 0 || m.cursor >= len(m.events) {
			return m, nil
		}
		ev := m.events[m.cursor]
		if _, bad := m.manifestErrors[ev.ID]; bad {
			m.footerNote = "(invalid manifest — cannot restore)"
			return m, nil
		}
		if _, ok := m.manifests[ev.ID]; !ok {
			m.footerNote = "(manifest not loaded yet)"
			return m, nil
		}
		m.selectedID = ev.ID
		return m, tea.Quit
	}
	return m, nil
}

// redecorate runs FilterDecorate over every cached tree with the current
// filter state. Cheap — O(nodes) and only over what's been viewed.
func (m *PickerModel) redecorate() {
	for _, tree := range m.trees {
		FilterDecorate(tree, m.filter, m.runningSet)
	}
}

// Bootstrap parses the manifest for the initial cursor position. Call once
// after construction; the cobra wiring does this so View has data on first
// render. Idempotent.
func (m *PickerModel) Bootstrap() {
	if m.mode == ModeClose {
		m.cursor = firstSelectableCloseRow(m.closeRows)
	}
	m.ensureManifest()
}

// CurrentCounts returns FilterDecorate's most recent output for the cursor's
// event. Used by the footer and by tests.
func (m PickerModel) CurrentCounts() Counts {
	id := m.CurrentEventID()
	if id == 0 {
		return Counts{}
	}
	tree := m.trees[id]
	if tree == nil {
		return Counts{}
	}
	// FilterDecorate mutates in place; rerun to read counts cheaply.
	return FilterDecorate(tree, m.filter, m.runningSet)
}

// Filter returns the current filter for caller-side BuildPlan.
func (m PickerModel) Filter() filter.Filter { return m.filter }

// SelectedID returns the event ID of the row the user confirmed, or 0 on cancel.
func (m PickerModel) SelectedID() int64 { return m.selectedID }

// SelectedManifest returns the parsed manifest of the selected event. Snapshot
// mode hands it to restore.BuildPlan. Close mode does not: buildRestorePlan
// restores from the ClosedItem's own Pane/Window pointers, and reads the
// sub-manifest only for shape checks and to name the session to focus.
func (m PickerModel) SelectedManifest() snapshot.Manifest {
	if m.selectedID == 0 {
		return snapshot.Manifest{}
	}
	if m.mode == ModeClose {
		return m.closeContexts[m.selectedID].SubManifest
	}
	return m.manifests[m.selectedID]
}

// Focus returns the current focus zone (exported for tests).
func (m PickerModel) Focus() FocusZone { return m.focus }

// Cursor returns the current cursor position (exported for tests).
func (m PickerModel) Cursor() int { return m.cursor }

// TreeCursor returns the index of the focused node in the visible-tree list.
// Exported for tests.
func (m PickerModel) TreeCursor() int { return m.treeCursor }

// SetCloseContexts attaches the diff-derived ClosedItem summaries (label + sub-
// manifest) for each close event. Call between NewPickerModel and Bootstrap.
// No-op when ctx is nil.
func (m *PickerModel) SetCloseContexts(ctx map[int64]CloseContext) {
	if ctx == nil {
		return
	}
	m.closeContexts = ctx
}

// SetHiddenCount records how many unrecoverable close events the caller
// filtered out. Rendered as a footer line in the list pane. Close mode only.
func (m *PickerModel) SetHiddenCount(n int) {
	m.hiddenCount = n
}

// SetCloseRows attaches the flat, newest-first close list. Call between
// NewPickerModel and Bootstrap. Close mode only.
func (m *PickerModel) SetCloseRows(rows []CloseRow) {
	m.closeRows = rows
}

// SetCursor moves the cursor. Exported for tests; production code moves the
// cursor through key handling.
func (m *PickerModel) SetCursor(i int) { m.cursor = i }

// CurrentEventID returns the event id under the cursor, or 0 when the cursor
// is on a section header or there is nothing to point at.
func (m PickerModel) CurrentEventID() int64 {
	if m.mode == ModeClose {
		if m.cursor < 0 || m.cursor >= len(m.closeRows) {
			return 0
		}
		return m.closeRows[m.cursor].EventID
	}
	if m.cursor < 0 || m.cursor >= len(m.events) {
		return 0
	}
	return m.events[m.cursor].ID
}

// nextCloseRowIdx walks from start in dir (+1/-1) over m.closeRows to the
// next selectable row, or -1 when there is none that way.
func (m PickerModel) nextCloseRowIdx(start, dir int) int {
	for i := start + dir; i >= 0 && i < len(m.closeRows); i += dir {
		if m.closeRows[i].Selectable() {
			return i
		}
	}
	return -1
}

// firstSelectableCloseRow returns the index of the first selectable row in
// rows — the newest close, since BuildCloseList sorts each section
// newest-first — or 0 when rows has no selectable row at all (an empty list
// included, where 0 is out of range and every reader treats it as nothing
// selected).
func firstSelectableCloseRow(rows []CloseRow) int {
	for i, r := range rows {
		if r.Selectable() {
			return i
		}
	}
	return 0
}

// CloseContextFor returns the cached close context for the given event ID,
// or the zero-value CloseContext if absent.
func (m PickerModel) CloseContextFor(id int64) CloseContext {
	return m.closeContexts[id]
}

// FooterNote returns the transient warning text (exported for tests + view).
func (m PickerModel) FooterNote() string { return m.footerNote }

// TreeFor returns the cached tree for the event with the given ID, or nil.
func (m PickerModel) TreeFor(id int64) *TreeNode { return m.trees[id] }

// PreviewCmd returns a tea.Cmd that loads the scrollback the preview needs, or
// nil if no load is needed (nothing to show, no SHA, cached, already loading,
// or no scrollback store).
//
// Side effect: marks the SHA as in-flight in m.loadingSHAs before returning.
// Pointer receiver is required to write through to that map.
func (m *PickerModel) PreviewCmd() tea.Cmd {
	if m.scrollbackStore == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, sha := range m.previewSHAs() {
		if _, cached := m.scrollbacks[sha]; cached {
			continue
		}
		if _, errored := m.scrollbackErrors[sha]; errored {
			continue
		}
		if m.loadingSHAs[sha] {
			continue
		}
		m.loadingSHAs[sha] = true
		cmds = append(cmds, loadScrollbackCmd(m.scrollbackStore, sha))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// previewSHAs returns every ScrollbackSHA the preview needs loaded. Close mode
// stacks one block per pane the close took down, so a single hash would leave
// the second block stuck on "(scrollback pending)"; snapshot mode shows one
// pane and yields at most one.
func (m PickerModel) previewSHAs() []string {
	if m.mode == ModeClose {
		return m.closeCursorSHAs()
	}
	if sha := m.previewSHA(); sha != "" {
		return []string{sha}
	}
	return nil
}

// previewSHA returns the ScrollbackSHA snapshot mode's preview is showing, or
// "" when it is not showing scrollback — Tab has to have reached the tree.
func (m PickerModel) previewSHA() string {
	if m.focus != focusTree {
		return ""
	}
	nodes := m.visibleNodes()
	if m.treeCursor < 0 || m.treeCursor >= len(nodes) {
		return ""
	}
	n := nodes[m.treeCursor]
	if n.Kind != NodePane {
		return ""
	}
	p, ok := n.Ref.(*snapshot.Pane)
	if !ok || p == nil {
		return ""
	}
	return p.ScrollbackSHA
}

// closeCursorContext resolves the close context the cursor sits on, or the
// zero value when it sits on a section header or out of range.
func (m PickerModel) closeCursorContext() CloseContext {
	return m.CloseContextFor(m.CurrentEventID())
}

// closeCursorSHAs returns the scrollback hashes of every pane the cursor's
// close took down. All of them are scheduled even when the panel is too short
// to draw every block — loading a hash the frame does not show costs a read,
// where missing one leaves that block hanging on "(loading scrollback…)".
func (m PickerModel) closeCursorSHAs() []string {
	w := closePreviewWindow(m.closeCursorContext())
	if w == nil {
		return nil
	}
	out := make([]string, 0, len(w.Panes))
	for i := range w.Panes {
		if sha := w.Panes[i].ScrollbackSHA; sha != "" {
			out = append(out, sha)
		}
	}
	return out
}

// ensureManifest parses + builds + decorates the tree for the cursor's event,
// caching the result. No-op on cache hit. Records parse errors in
// m.manifestErrors so View can render "(invalid manifest)".
func (m *PickerModel) ensureManifest() {
	id := m.CurrentEventID()
	if id == 0 {
		return
	}
	if _, ok := m.manifests[id]; ok {
		return
	}
	if _, bad := m.manifestErrors[id]; bad {
		return
	}
	var man snapshot.Manifest
	if m.mode == ModeClose {
		// Close events store their post-close index, not a snapshot manifest.
		// Use the diff-derived sub-manifest (set via SetCloseContexts) so the
		// tree pane shows what was lost rather than an empty event.
		man = m.closeContexts[id].SubManifest
		if len(man.Sessions) == 0 {
			m.manifestErrors[id] = fmt.Errorf("close event has no recoverable entity")
			return
		}
	} else {
		ev := m.events[m.cursor]
		var err error
		man, err = parseEventManifest(ev)
		if err != nil {
			m.manifestErrors[id] = err
			return
		}
	}
	m.manifests[id] = man
	tree := BuildTree(man)
	FilterDecorate(tree, m.filter, m.runningSet)
	m.trees[id] = tree
}

func parseEventManifest(ev store.Event) (snapshot.Manifest, error) {
	var m snapshot.Manifest
	if ev.Kind == "snapshot" {
		if err := json.Unmarshal([]byte(ev.ManifestJSON), &m); err != nil {
			return snapshot.Manifest{}, err
		}
		return m, nil
	}
	// Close events wrap the index inside an "index" key.
	var wrapped struct {
		Index json.RawMessage `json:"index"`
	}
	if err := json.Unmarshal([]byte(ev.ManifestJSON), &wrapped); err != nil {
		return snapshot.Manifest{}, err
	}
	if len(wrapped.Index) == 0 {
		return snapshot.Manifest{}, fmt.Errorf("close event has no index")
	}
	if err := json.Unmarshal(wrapped.Index, &m); err != nil {
		return snapshot.Manifest{}, err
	}
	return m, nil
}
