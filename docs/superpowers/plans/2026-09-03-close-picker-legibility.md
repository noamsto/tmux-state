# Close Picker Legibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the close picker (`prefix+U`) legible — colour rows by role instead of dimming them, stop the cursor landing on rows Enter refuses, and give it a live preview column instead of a dead one.

**Architecture:** All changes live in `internal/picker`. The close tree stays; five independent slices land in order: row styling, cursor navigation, a close-cursor-driven preview, the "what happens on Enter" sentence, and the collapse from three columns to two. Nothing outside `internal/picker` changes, and snapshot mode's behaviour is preserved at every step.

**Tech Stack:** Go, `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`, `github.com/charmbracelet/x/ansi`.

**Spec:** `docs/superpowers/specs/2026-09-03-close-picker-legibility-design.md`

## Global Constraints

- Work in the worktree `~/Data/git/.worktrees/noamsto/tmux-remux/feat-102-close-picker-legibility` on branch `feat/102-close-picker-legibility`. Every command below assumes that directory.
- Run tests as `nix develop -c go test ./internal/picker/...` — the devshell provides the Go toolchain.
- Commit as `nix develop -c git commit -m "..."` — the pre-commit hooks (`gofmt`, `golangci-lint`, `govet`) need the devshell on `PATH` or they skip silently.
- **Never nest a styled string inside another lipgloss style.** lipgloss v2 strips ESC bytes from pre-styled input, so a role colour placed inside `rowActive`'s background collapses to invisible. This is already documented at `internal/picker/view.go:355`. Build each row as plain text and apply exactly one style to the whole line.
- **No `Faint` and no `Italic` anywhere in the close tree** — that is the defect being fixed.
- Every row rendered into a lipgloss frame must be truncated to the frame's inner width with `ansi.Truncate`. A frame *wraps* overflow instead of clipping it, which desyncs `scrollWindow`'s one-row-per-node assumption.
- Snapshot mode (`prefix+R`) keeps its three-column layout and its current Tab semantics. Any change that touches shared code must branch on `m.mode`.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/picker/style.go` | Theme-derived lipgloss style globals | Modify — add `rowScaffold` |
| `internal/picker/view.go` | All frame rendering + width arithmetic | Modify — `closeRow`, `closeRowStyle`, `paneWidthsThree`, `stacksPanel`, `View`, `renderFooter` |
| `internal/picker/model.go` | Elm state + key handling | Modify — `isCloseNavTarget`, `closeNavAt`, Left/Up/Down handlers, Tab guard, `previewSHA` |
| `internal/picker/preview.go` | Right-hand panel | Modify — `renderPreview` dispatch, `renderClosePreview`, `closePreviewWindow`, `restoreSentence`, `humanAge`, `previewMaxScroll` |
| `internal/picker/view_internal_test.go` | White-box render tests | Modify — styling, marker, width tests |
| `internal/picker/model_test.go` | Black-box key-handling tests | Modify — navigation tests |
| `internal/picker/preview_test.go` | Preview tests | Modify — follow-cursor and sentence tests |

`preview.go` grows by roughly 90 lines in Tasks 3 and 4. It stays under 350 lines and keeps one responsibility (the right-hand panel), so it is not split.

---

## Task 1: Row styling — colour by role, not by importance

Removes `Faint`/`Italic` from the close tree and gives restorable rows a leading `●` marker.

**Files:**
- Modify: `internal/picker/style.go:10-31` (style var block), `internal/picker/style.go:37-62` (`applyTheme`)
- Modify: `internal/picker/view.go:316-372` (`closeRow`)
- Test: `internal/picker/view_internal_test.go`

**Interfaces:**
- Consumes: `closeTreeFixture()` (already in `view_internal_test.go:46`), `IsCloseGroup(n *CloseNode) bool` (`closetree.go:56`).
- Produces: `closeRowStyle(n *CloseNode) lipgloss.Style` — the single flat style for one close row, ignoring cursor state. Task 2 does not use it; the tests do.

**Design note (deviates from the spec, deliberately):** the spec put the `(live)` tag in `Overlay` while the row body used `Subtext`. That needs two styles on one line, which the global constraint above forbids. The whole scaffolding row therefore renders in one flat `Subtext`; the parentheses alone distinguish the tag. Also, the spec proposed asserting the absence of `ESC[2m`/`ESC[3m` in rendered bytes. That is unreliable — lipgloss emits combined SGR like `ESC[2;38;2;166;173;200m`, and the literal `2` also appears inside every truecolor triplet. Asserting on `closeRowStyle(...).GetFaint()` is the same guarantee without the false positives, so the tests do that instead.

- [ ] **Step 1: Write the failing tests**

Append to `internal/picker/view_internal_test.go`:

```go
// The close tree must never render faint or italic text: the picker used both
// to mean "not a restore target", which reads as "old / unimportant" instead —
// and dimOlderThan already claims that visual channel for age.
func TestCloseRowStyle_NeverFaintOrItalic(t *testing.T) {
	applyTheme(Theme{})
	for _, n := range FlattenClose(closeTreeFixture()) {
		s := closeRowStyle(n)
		if s.GetFaint() {
			t.Errorf("%q: style is faint", n.Label)
		}
		if s.GetItalic() {
			t.Errorf("%q: style is italic", n.Label)
		}
	}
}

// Scaffolding must still be visually separable from a restorable row — just by
// a different foreground, not by dimming.
func TestCloseRowStyle_ScaffoldingDiffersFromEventRow(t *testing.T) {
	applyTheme(Theme{})
	var event, scaffold *CloseNode
	for _, n := range FlattenClose(closeTreeFixture()) {
		if IsCloseGroup(n) {
			continue
		}
		if n.EventID != 0 && event == nil {
			event = n
		}
		if n.EventID == 0 && scaffold == nil {
			scaffold = n
		}
	}
	if event == nil || scaffold == nil {
		t.Fatalf("fixture lacks both row kinds: event=%v scaffold=%v", event, scaffold)
	}
	if closeRowStyle(event).GetForeground() == closeRowStyle(scaffold).GetForeground() {
		t.Error("event and scaffolding rows share a foreground colour")
	}
}

// The marker is the load-bearing "Enter works here" cue, so it must appear on
// exactly the rows that carry an event id.
func TestCloseRow_MarksOnlyRestorableRows(t *testing.T) {
	applyTheme(Theme{})
	for _, n := range FlattenClose(closeTreeFixture()) {
		if IsCloseGroup(n) {
			continue
		}
		got := strings.Contains(closeRow(n, 60, false), closeMarker)
		if want := n.EventID != 0; got != want {
			t.Errorf("%q: marker present=%v, want %v", n.Label, got, want)
		}
	}
}

// State is a tag, not a suffix: " · live" reads as part of the window name.
func TestCloseRow_StateRendersAsAParenthesisedTag(t *testing.T) {
	applyTheme(Theme{})
	for _, n := range FlattenClose(closeTreeFixture()) {
		if n.State == "" {
			continue
		}
		row := closeRow(n, 60, false)
		if !strings.Contains(row, "("+n.State+")") {
			t.Errorf("%q: want a (%s) tag, got %q", n.Label, n.State, row)
		}
		if strings.Contains(row, " · "+n.State) {
			t.Errorf("%q: still renders the old ' · %s' suffix", n.Label, n.State)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/picker/ -run 'TestCloseRow' -v`
Expected: FAIL to compile — `undefined: closeRowStyle`, `undefined: closeMarker`.

- [ ] **Step 3: Add the scaffolding style**

In `internal/picker/style.go`, add `rowScaffold` to the var block after `rowDim`:

```go
	rowActive  lipgloss.Style
	rowDefault lipgloss.Style
	rowDim     lipgloss.Style
	// rowScaffold styles close-tree rows that exist only to parent something
	// restorable. Quieter than body text, but never faint — dimming is how the
	// picker says "old", and these rows are not old, just not targets.
	rowScaffold lipgloss.Style
```

and in `applyTheme`, after the `rowDim` assignment:

```go
	rowScaffold = lipgloss.NewStyle().Foreground(t.Subtext())
```

- [ ] **Step 4: Rewrite `closeRow` and extract `closeRowStyle`**

Replace `internal/picker/view.go:316-372` (the whole `closeRow` function and its doc comment) with:

```go
// closeMarker precedes every restorable row. Scaffolding gets the same two
// cells of blank so labels stay aligned down the column.
const closeMarker = "● "

// closeRow renders one tree row: guide prefix, expand marker, restore marker,
// label, a parenthesised state tag on scaffolding, and a right-aligned
// timestamp for event rows.
func closeRow(n *CloseNode, innerWidth int, active bool) string {
	left := closeGuidePrefix(n)
	switch {
	case len(n.Children) > 0 && n.Expanded:
		left += "▾ "
	case len(n.Children) > 0:
		left += "▸ "
	}
	if !IsCloseGroup(n) {
		if n.EventID != 0 {
			left += closeMarker
		} else {
			left += strings.Repeat(" ", lipgloss.Width(closeMarker))
		}
	}
	left += n.Label
	if n.State != "" {
		left += " (" + n.State + ")"
	}

	right := ""
	if n.Ts != 0 {
		right = time.UnixMilli(n.Ts).Format("15:04")
	}
	// Reserve the timestamp plus one separating space, then pad the gap.
	budget := innerWidth
	if right != "" {
		budget -= len(right) + 1
	}
	if budget < 1 {
		budget = 1
	}
	left = ansi.Truncate(left, budget, "…")
	line := left
	if right != "" {
		if gap := innerWidth - lipgloss.Width(left) - len(right); gap > 0 {
			line += strings.Repeat(" ", gap)
		} else {
			line += " "
		}
		line += right
	}
	line = ansi.Truncate(line, innerWidth, "…")

	if active {
		// One flat style: lipgloss v2 strips ESC bytes from pre-styled input,
		// so nesting a role color inside rowActive's background can collapse to
		// invisible. Same reason appendNodeRows renders the active row plain.
		return rowActive.Width(innerWidth).Render(line)
	}
	return closeRowStyle(n).Width(innerWidth).Render(line)
}

// closeRowStyle returns the single flat style for a non-cursor close row.
// Scaffolding is separated from restorable rows by foreground alone — never by
// Faint or Italic, which the picker reserves for age.
func closeRowStyle(n *CloseNode) lipgloss.Style {
	if IsCloseGroup(n) {
		return previewHeader
	}
	if n.EventID == 0 {
		return rowScaffold
	}
	switch n.Kind {
	case CSession:
		return nodeSession
	case CWindow:
		return nodeWindow
	}
	return nodePane
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/picker/ -run 'TestCloseRow' -v`
Expected: PASS — four tests.

- [ ] **Step 6: Run the full package to check nothing regressed**

Run: `nix develop -c go test ./internal/picker/...`
Expected: PASS. `TestRenderCloseTree_NeverOverflowsFrame` is the one to watch — the marker adds two cells per row, and it asserts exact frame width and height.

- [ ] **Step 7: Commit**

```bash
git add internal/picker/style.go internal/picker/view.go internal/picker/view_internal_test.go
nix develop -c git commit -m "feat(picker): mark restorable close rows instead of dimming the rest (#102)"
```

---

## Task 2: Navigation — the cursor only lands where Enter works

**Files:**
- Modify: `internal/picker/model.go:654-658` (`isCloseNavTarget`), `internal/picker/model.go:349-365` (the Left handler)
- Test: `internal/picker/model_test.go`

**Interfaces:**
- Consumes: `closeRowStyle` is not used here. `IsCloseGroup(n *CloseNode) bool`, `closeIndexOf(vis []*CloseNode, target *CloseNode) int` (`model.go:688`).
- Produces: `closeNavAt(vis []*CloseNode, idx int) int` — clamps an index onto the nearest navigable row at or above it. Task 3 does not use it.

**No change to the Right handler.** It already expands the cursor row when it has children, which is exactly what the spec asks for once the cursor only ever sits on group headers and event rows.

- [ ] **Step 1: Write the failing tests**

Append to `internal/picker/model_test.go`:

```go
// nestedCloseModel builds a bootstrapped close-mode picker over a session →
// window → pane scaffolding chain. Distinct from the existing closeModel(t,
// hidden) at model_test.go:228, which has no scaffolding to skip: lazytmux is gone, its window 3 is a live header, and
// the only restorable thing inside it is a pane close.
func nestedCloseModel(t *testing.T) picker.PickerModel {
	t.Helper()
	evs := []store.Event{{ID: 1, Ts: 300, Kind: "pane-died"}}
	one := snapshot.Manifest{Sessions: []snapshot.Session{{Name: "lazytmux"}}}
	ctxs := map[int64]picker.CloseContext{
		1: {
			Label:       "pane: fish",
			Placement:   picker.ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane"},
			SubManifest: one,
		},
	}
	m := picker.NewPickerModel(picker.ModeClose, evs, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(picker.BuildCloseTree(evs, ctxs, "mono", map[string]bool{}))
	m.Bootstrap()
	return m
}

// Scaffolding rows exist only to indent something restorable. Stopping on them
// offers a row and then refuses it with "(group — nothing to restore here)".
func TestModel_CloseNavigationSkipsScaffolding(t *testing.T) {
	m := nestedCloseModel(t)
	pm := m
	// Walk the whole tree downward, then all the way back up.
	for _, code := range []rune{'j', 'j', 'j', 'j', 'k', 'k', 'k', 'k'} {
		updated, _ := pm.Update(tea.KeyPressMsg{Code: code})
		pm = updated.(picker.PickerModel)
		vis := pm.CloseVisible()
		n := vis[pm.Cursor()]
		if n.EventID == 0 && !picker.IsCloseGroup(n) {
			t.Fatalf("cursor landed on scaffolding row %q", n.Label)
		}
	}
}

// Enter on a scaffolding row is now unreachable, so the only row that can
// produce the group note is a group header.
func TestModel_CloseEnterOnScaffoldingIsUnreachable(t *testing.T) {
	m := nestedCloseModel(t)
	pm := m
	for i := 0; i < 6; i++ {
		updated, _ := pm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(picker.PickerModel)
		vis := got.CloseVisible()
		if note := got.FooterNote(); note != "" && !picker.IsCloseGroup(vis[got.Cursor()]) {
			t.Fatalf("Enter refused a non-group row %q with %q", vis[got.Cursor()].Label, note)
		}
		updated, _ = got.Update(tea.KeyPressMsg{Code: 'j'})
		pm = updated.(picker.PickerModel)
	}
}

// With scaffolding no longer a cursor stop, Left must collapse the nearest
// collapsible ancestor and leave the cursor somewhere it may legally sit.
func TestModel_CloseLeftCollapsesAncestorAndLandsNavigable(t *testing.T) {
	m := nestedCloseModel(t)
	// Cursor starts on the newest restorable row: the nested pane close.
	pm := m
	before := len(pm.CloseVisible())
	updated, _ := pm.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	pm = updated.(picker.PickerModel)

	if after := len(pm.CloseVisible()); after >= before {
		t.Errorf("Left did not collapse anything: %d rows before, %d after", before, after)
	}
	vis := pm.CloseVisible()
	if pm.Cursor() < 0 || pm.Cursor() >= len(vis) {
		t.Fatalf("cursor %d out of range for %d rows", pm.Cursor(), len(vis))
	}
	n := vis[pm.Cursor()]
	if n.EventID == 0 && !picker.IsCloseGroup(n) {
		t.Errorf("Left left the cursor on scaffolding row %q", n.Label)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/picker/ -run 'TestModel_Close' -v`
Expected: `TestModel_CloseNavigationSkipsScaffolding` FAILs with `cursor landed on scaffolding row "3: docs"`; `TestModel_CloseLeftCollapsesAncestorAndLandsNavigable` FAILs the same way.

- [ ] **Step 3: Narrow the navigation predicate**

Replace `internal/picker/model.go:654-658`:

```go
// isCloseNavTarget reports whether the cursor may land on n: an event row
// (restorable) or one of the two group headers (so a section can be
// collapsed). Scaffolding — the nodes that exist only to parent something
// restorable — is never a stop: Enter would only refuse it.
func isCloseNavTarget(n *CloseNode) bool {
	return n.EventID != 0 || IsCloseGroup(n)
}
```

- [ ] **Step 4: Add the clamp helper**

Add below `closeIndexOf` in `internal/picker/model.go`:

```go
// closeNavAt returns idx when vis[idx] is a cursor stop, else the nearest stop
// above it. Collapsing an ancestor leaves the cursor pointing at a row it may
// no longer land on; walking up always terminates, since row 0 is a group
// header.
func closeNavAt(vis []*CloseNode, idx int) int {
	if idx >= len(vis) {
		idx = len(vis) - 1
	}
	for i := idx; i >= 0; i-- {
		if isCloseNavTarget(vis[i]) {
			return i
		}
	}
	return 0
}
```

- [ ] **Step 5: Rework the Left handler**

Replace the `case key.Matches(msg, m.keys.Left):` block at `internal/picker/model.go:349-365` with:

```go
		case key.Matches(msg, m.keys.Left):
			if m.cursor >= 0 && m.cursor < len(vis) {
				n := vis[m.cursor]
				if n.Expanded && len(n.Children) > 0 {
					n.Expanded = false
					return m, nil
				}
				// Walk up to the nearest collapsible ancestor below the
				// synthetic root and collapse it. The cursor cannot step onto
				// that ancestor when it is scaffolding, so land on the nearest
				// stop at or above where it sits — the enclosing group header
				// in the worst case.
				for a := n.Parent; a != nil && a.Parent != nil; a = a.Parent {
					if !a.Expanded || len(a.Children) == 0 {
						continue
					}
					a.Expanded = false
					next := m.CloseVisible()
					m.cursor = closeNavAt(next, closeIndexOf(next, a))
					(&m).ensureManifest()
					return m, nil
				}
			}
			return m, nil
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/picker/ -run 'TestModel_Close' -v`
Expected: PASS — three tests.

- [ ] **Step 7: Run the full package**

Run: `nix develop -c go test ./internal/picker/...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/picker/model.go internal/picker/model_test.go
nix develop -c git commit -m "feat(picker): stop the close cursor landing on rows Enter refuses (#102)"
```

---

## Task 3: The preview follows the close cursor

Removes the Tab gate. The preview reads the close-tree cursor and draws the pane map or the closed pane's scrollback.

**Files:**
- Modify: `internal/picker/preview.go:57-101` (`renderPreview`), `internal/picker/preview.go:150-174` (`previewMaxScroll`)
- Modify: `internal/picker/model.go:330-344` (close-mode Up/Down), `internal/picker/model.go:733-752` (`focusedPaneSHA`), `internal/picker/model.go:487-500` (the Tab case)
- Test: `internal/picker/preview_test.go`

**Interfaces:**
- Consumes: `renderWindowMap(w *snapshot.Window, innerWidth, innerHeight int) string` (`preview.go:197`) — already reads `Placement` off `CurrentEventID()` to mark which panes died, which is now the close cursor, so it needs no change. `previewWindow(s string, width, height, scroll, scrollX int) string` (`preview.go:105`).
- Produces:
  - `func (m PickerModel) renderClosePreview(width int) string`
  - `func closePreviewWindow(cc CloseContext) *snapshot.Window`
  - `func (m PickerModel) previewSHA() string` — replaces `focusedPaneSHA`; Task 4 does not use it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/picker/preview_test.go`:

`preview_test.go` is package `picker` (internal), so these call the unexported
renderer directly rather than reading the whole view — the close tree column
also contains the window name, so asserting on `View().Content` would pass on
the wrong column.

```go
// prefix+U used to open on "(press Tab to preview panes)". The preview must be
// live from the first frame, following the close-tree cursor.
func TestRenderPreview_CloseModeNeedsNoTab(t *testing.T) {
	m := nestedClosePreviewModel(t)
	_, _, previewW := m.paneWidthsThree()
	out := m.renderPreview(previewW)
	if strings.Contains(out, "press Tab") {
		t.Errorf("close preview still gated behind Tab:\n%s", out)
	}
	if !strings.Contains(out, "docs") {
		t.Errorf("close preview does not name the window it would restore:\n%s", out)
	}
}

// Moving the close cursor must move the preview with it, without a focus change.
func TestRenderPreview_CloseModeTracksTheCursor(t *testing.T) {
	m := nestedClosePreviewModel(t)
	_, _, previewW := m.paneWidthsThree()
	first := m.renderPreview(previewW)

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'j'})
	pm := updated.(PickerModel)
	if pm.focus != focusList {
		t.Fatalf("focus moved to %v; the preview should follow the list cursor", pm.focus)
	}
	if pm.renderPreview(previewW) == first {
		t.Error("preview did not change when the close cursor moved")
	}
}
```

Add the fixture helper to the same file:

```go
// nestedClosePreviewModel builds a close-mode picker whose sub-manifest carries
// a real two-pane layout, so the preview has a map to draw.
func nestedClosePreviewModel(t *testing.T) PickerModel {
	t.Helper()
	man := snapshot.Manifest{Sessions: []snapshot.Session{{
		Name: "lazytmux",
		Windows: []snapshot.Window{{
			Index:  3,
			Name:   "docs",
			Layout: "a1b2,80x24,0,0[80x12,0,0,1,80x11,0,13,2]",
			Panes: []snapshot.Pane{
				{Index: 0, Command: "fish", ID: "%1"},
				{Index: 1, Command: "nvim", ID: "%2"},
			},
		}},
	}}}
	evs := []store.Event{
		{ID: 1, Ts: 300, Kind: "window-unlinked"},
		{ID: 2, Ts: 200, Kind: "pane-died"},
	}
	ctxs := map[int64]CloseContext{
		1: {Label: "w", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "window", PaneCount: 2}, SubManifest: man},
		2: {Label: "pane: nvim", Placement: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane", PaneID: "%2"}, SubManifest: man},
	}
	m := NewPickerModel(ModeClose, evs, nil, nil)
	m.SetCloseContexts(ctxs)
	m.SetCloseTree(BuildCloseTree(evs, ctxs, "mono", map[string]bool{}))
	m.Bootstrap()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return updated.(PickerModel)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/picker/ -run 'TestRenderPreview_CloseMode' -v`
Expected: `TestRenderPreview_CloseModeNeedsNoTab` FAILs with `close preview still gated behind Tab`.

- [ ] **Step 3: Add the close-mode preview**

Add to `internal/picker/preview.go`, after `renderPreview`:

```go
// renderClosePreview draws the panel for whatever close row the cursor is on.
// Unlike snapshot mode there is no second tree to focus first: prefix+U opens
// with this already showing.
func (m PickerModel) renderClosePreview(width int) string {
	frameHeight := m.panelFrameHeight()
	innerHeight := m.previewInnerHeight()
	innerWidth := width - 4
	if innerWidth < 1 {
		innerWidth = 1
	}
	frame := previewFrame.Width(width).Height(frameHeight).MaxHeight(frameHeight)

	vis := m.CloseVisible()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return frame.Render(rowDim.Render("(nothing selected)"))
	}
	n := vis[m.cursor]
	if n.EventID == 0 {
		return frame.Render(rowDim.Render("(a section — ↑↓ to reach a close)"))
	}

	cc := m.CloseContextFor(n.EventID)
	if sha := m.closeCursorSHA(); sha != "" {
		if err := m.scrollbackErrors[sha]; err != nil {
			return frame.Render(footerWarn.Render("(scrollback file missing: " + err.Error() + ")"))
		}
		if content, ok := m.scrollbacks[sha]; ok {
			return frame.Render(previewWindow(string(content), innerWidth, innerHeight, m.previewScroll, m.previewScrollX))
		}
		if m.loadingSHAs[sha] {
			return frame.Render(rowDim.Render("(loading scrollback…)"))
		}
	}
	w := closePreviewWindow(cc)
	if w == nil {
		return frame.Render(rowDim.Render("(nothing captured for this close)"))
	}
	if art := m.renderWindowMap(w, innerWidth, innerHeight); art != "" {
		return frame.Render(art)
	}
	return frame.Render(rowDim.Render("(no layout captured for this window)"))
}

// closePreviewWindow returns the window the preview should draw: the one the
// placement names, or the sub-manifest's first window for a session close,
// where every window came down and the first one stands for the rest.
func closePreviewWindow(cc CloseContext) *snapshot.Window {
	for i := range cc.SubManifest.Sessions {
		s := &cc.SubManifest.Sessions[i]
		for j := range s.Windows {
			if cc.Placement.Scope == "session" {
				return &s.Windows[j]
			}
			if s.Windows[j].Index == cc.Placement.WindowIndex {
				return &s.Windows[j]
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Dispatch to it, and give the SHA lookup a close-mode path**

At the top of `renderPreview` (`internal/picker/preview.go:57`), insert before `frameHeight := ...`:

```go
	if m.mode == ModeClose && m.closeTree != nil {
		return m.renderClosePreview(width)
	}
```

Replace `focusedPaneSHA` (`internal/picker/model.go:733-752`) with:

```go
// previewSHA returns the ScrollbackSHA the preview is currently showing, or ""
// when it is not showing scrollback. Close mode reads the close-tree cursor and
// ignores focus — its preview is live from the first frame; snapshot mode still
// requires Tab to reach its tree.
func (m PickerModel) previewSHA() string {
	if m.mode == ModeClose && m.closeTree != nil {
		return m.closeCursorSHA()
	}
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

// closeCursorSHA returns the scrollback hash of the pane the cursor's close
// took down, or "" when the cursor is not on a pane-scoped close. Only a pane
// close has one pane's worth of output to show; a window or session close is
// previewed as a layout map instead.
func (m PickerModel) closeCursorSHA() string {
	vis := m.CloseVisible()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return ""
	}
	cc := m.CloseContextFor(vis[m.cursor].EventID)
	if cc.Placement.Scope != "pane" || cc.Placement.PaneID == "" {
		return ""
	}
	w := closePreviewWindow(cc)
	if w == nil {
		return ""
	}
	for i := range w.Panes {
		if w.Panes[i].ID == cc.Placement.PaneID {
			return w.Panes[i].ScrollbackSHA
		}
	}
	return ""
}
```

Update the one caller in `PreviewCmd` (`internal/picker/model.go:716`):

```go
	sha := m.previewSHA()
```

- [ ] **Step 5: Make close-mode Up/Down schedule a preview load**

In the close-mode key block, replace the `Up` and `Down` cases (`internal/picker/model.go:331-343`):

```go
		case key.Matches(msg, m.keys.Up):
			if idx := m.nextCloseIdx(m.cursor, -1); idx >= 0 {
				m.cursor = idx
				m.previewScroll = 0
				m.previewScrollX = 0
				(&m).ensureManifest()
			}
			return m, (&m).PreviewCmd()
		case key.Matches(msg, m.keys.Down):
			if idx := m.nextCloseIdx(m.cursor, +1); idx >= 0 {
				m.cursor = idx
				m.previewScroll = 0
				m.previewScrollX = 0
				(&m).ensureManifest()
			}
			return m, (&m).PreviewCmd()
```

- [ ] **Step 6: Clamp scrolling off the same SHA**

Replace the head of `previewMaxScroll` (`internal/picker/preview.go:152-166`) so both modes resolve the same way:

```go
func (m PickerModel) previewMaxScroll(innerHeight int) int {
	sha := m.previewSHA()
	if sha == "" {
		return 0
	}
	content, ok := m.scrollbacks[sha]
	if !ok {
		return 0
	}
```

Leave the rest of the function (the `total := ...` tail) unchanged. Delete the now-unused `nodes`/`n`/`p` lookup above it.

- [ ] **Step 7: Retire Tab in close mode**

Close mode has no second tree to focus, and `Alt+j`/`Alt+k` already scroll the preview without a focus change (the preview-scroll block at `model.go:381` is mode-gated, not focus-gated). Narrow the Tab case at `internal/picker/model.go:489`:

```go
	case key.Matches(msg, m.keys.Tab):
		// Snapshot mode only: close mode is two columns with no second tree to
		// reach, and its preview scrolls with Alt+j/k regardless of focus.
		if m.mode == ModeSnapshot {
```

(the body and the closing brace are unchanged).

- [ ] **Step 8: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/picker/ -run 'TestRenderPreview_CloseMode' -v`
Expected: PASS — two tests.

- [ ] **Step 9: Run the full package**

Run: `nix develop -c go test ./internal/picker/...`
Expected: PASS. If `TestModel_TabSwitchesFocus_*` covers close mode, update it to assert Tab is now inert there and say so in the commit.

- [ ] **Step 10: Commit**

```bash
git add internal/picker/preview.go internal/picker/model.go internal/picker/preview_test.go
nix develop -c git commit -m "feat(picker): make the close preview follow the cursor, not the Tab key (#102)"
```

---

## Task 4: The what-happens line

**Files:**
- Modify: `internal/picker/preview.go` (`renderClosePreview` from Task 3)
- Test: `internal/picker/preview_test.go`

**Interfaces:**
- Consumes: `renderClosePreview`, `closePreviewWindow` (Task 3); `ClosePlacement` (`closetree.go:29`); `snapshot.StripFormat(string) string`.
- Produces: `func restoreSentence(p ClosePlacement, man snapshot.Manifest, now time.Time, ts int64) []string`, `func humanAge(d time.Duration) string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/picker/view_internal_test.go` (white-box — `restoreSentence` is unexported):

```go
func TestRestoreSentence(t *testing.T) {
	man := snapshot.Manifest{Sessions: []snapshot.Session{{
		Name:    "lazytmux",
		Windows: []snapshot.Window{{Index: 3, Name: "docs"}, {Index: 4, Name: "src"}},
	}}}
	now := time.UnixMilli(1_000_000_000)
	ts := now.Add(-22 * time.Minute).UnixMilli()

	cases := []struct {
		name  string
		place ClosePlacement
		want  []string
	}{
		{
			name:  "window",
			place: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "window", PaneCount: 2},
			want: []string{
				"↵ reopens window docs",
				"  in lazytmux at index 3",
				"2 panes · closed 22m ago",
			},
		},
		{
			name:  "pane",
			place: ClosePlacement{Session: "lazytmux", WindowIndex: 3, WindowName: "docs", Scope: "pane"},
			want: []string{
				"↵ reopens a pane in lazytmux:3",
				"closed 22m ago",
			},
		},
		{
			name:  "session",
			place: ClosePlacement{Session: "lazytmux", Scope: "session"},
			want: []string{
				"↵ reopens session lazytmux (2 windows)",
				"closed 22m ago",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreSentence(tc.place, man, now, ts)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{22 * time.Minute, "22m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		if got := humanAge(tc.in); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/picker/ -run 'TestRestoreSentence|TestHumanAge' -v`
Expected: FAIL to compile — `undefined: restoreSentence`, `undefined: humanAge`.

- [ ] **Step 3: Implement the sentence**

Add to `internal/picker/preview.go`:

```go
// restoreSentence states what Enter on this close would do, in the terms the
// restore path actually honours: a window goes back to its original index, so
// naming the index here is a promise rather than a guess.
func restoreSentence(p ClosePlacement, man snapshot.Manifest, now time.Time, ts int64) []string {
	var out []string
	switch p.Scope {
	case "window":
		out = append(out,
			"↵ reopens window "+snapshot.StripFormat(p.WindowName),
			fmt.Sprintf("  in %s at index %d", p.Session, p.WindowIndex),
		)
	case "pane":
		out = append(out, fmt.Sprintf("↵ reopens a pane in %s:%d", p.Session, p.WindowIndex))
	case "session":
		out = append(out, fmt.Sprintf("↵ reopens session %s (%s)", p.Session, countPhrase(countWindows(man), "window")))
	default:
		return nil
	}

	age := "closed " + humanAge(now.Sub(time.UnixMilli(ts)))
	if p.PaneCount > 0 {
		age = countPhrase(p.PaneCount, "pane") + " · " + age
	}
	return append(out, age)
}

// humanAge renders a duration as the coarsest unit that still reads true.
// The row already carries the wall-clock time; this is the "and how long ago
// was that" the wall clock cannot answer once a close is a day old.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// countPhrase renders "1 pane" / "2 panes".
func countPhrase(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// countWindows totals the windows a manifest would restore.
func countWindows(man snapshot.Manifest) int {
	n := 0
	for _, s := range man.Sessions {
		n += len(s.Windows)
	}
	return n
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/picker/ -run 'TestRestoreSentence|TestHumanAge' -v`
Expected: PASS — five subtests plus four age cases.

- [ ] **Step 5: Render it under the map**

In `renderClosePreview`, replace the `renderWindowMap` tail (the last three statements from Task 3, Step 3) with:

```go
	sentence := restoreSentence(cc.Placement, cc.SubManifest, time.Now(), n.Ts)
	// Reserve the sentence's rows out of the map's height so the frame does not
	// wrap — a lipgloss frame pads short content but never clips overflow.
	mapHeight := innerHeight - len(sentence) - 1
	body := rowDim.Render("(no layout captured for this window)")
	if w := closePreviewWindow(cc); w != nil && mapHeight > 0 {
		if art := m.renderWindowMap(w, innerWidth, mapHeight); art != "" {
			body = art
		}
	}
	for _, line := range sentence {
		body += "\n" + rowDim.Render(ansi.Truncate(line, innerWidth, "…"))
	}
	return frame.Render(body)
```

Note the sentence goes under the map for a window or session close. A pane close returns from the scrollback branch above and shows no sentence — the scrollback is what you are there to read, and the row's own label already says which pane it was.

- [ ] **Step 6: Run the full package**

Run: `nix develop -c go test ./internal/picker/...`
Expected: PASS. `TestView_CloseModeStacksPanel` and the frame-size assertions are the ones to watch — the sentence changes the panel's content height.

- [ ] **Step 7: Commit**

```bash
git add internal/picker/preview.go internal/picker/view_internal_test.go
nix develop -c git commit -m "feat(picker): say what Enter would reopen, in the close preview (#102)"
```

---

## Task 5: Two columns

**Files:**
- Modify: `internal/picker/view.go:36-52` (close-mode `View` branches), `internal/picker/view.go:154-160` (`stacksPanel`), `internal/picker/view.go:165-208` (`paneWidthsThree`), `internal/picker/view.go:118-127` (footer hints)
- Test: `internal/picker/view_internal_test.go`

**Interfaces:**
- Consumes: `renderCloseTree(m PickerModel, width, height int) string`, `renderPreview(width int) string`.
- Produces: nothing new. `paneWidthsThree` keeps its signature and returns a zero middle column in close mode.

- [ ] **Step 1: Write the failing tests**

Append to `internal/picker/view_internal_test.go`:

```go
// The sub-manifest column restated the hierarchy the close tree already draws.
// Close mode is two columns at every width that has room for a preview.
func TestPaneWidths_CloseModeHasNoMiddleColumn(t *testing.T) {
	for _, w := range []int{80, 100, 119, 120, 160, 200} {
		m := PickerModel{mode: ModeClose, width: w, height: 40}
		list, tree, preview := m.paneWidthsThree()
		if tree != 0 {
			t.Errorf("width=%d: middle column = %d, want 0", w, tree)
		}
		if preview <= 0 {
			t.Errorf("width=%d: preview = %d, want > 0", w, preview)
		}
		if list+tree+preview != w {
			t.Errorf("width=%d: columns sum to %d", w, list+tree+preview)
		}
		if list < 32 {
			t.Errorf("width=%d: tree column = %d, below the 32-cell floor", w, list)
		}
	}
}

// Snapshot mode is untouched: it keeps three columns at 120 and above.
func TestPaneWidths_SnapshotModeKeepsThreeColumns(t *testing.T) {
	m := PickerModel{mode: ModeSnapshot, width: 160, height: 40}
	if _, tree, _ := m.paneWidthsThree(); tree == 0 {
		t.Error("snapshot mode lost its middle column")
	}
}

// Two columns side by side at every width — close mode never stacks.
func TestView_CloseModeNeverStacks(t *testing.T) {
	for _, w := range []int{90, 100, 119} {
		m := PickerModel{mode: ModeClose, closeTree: closeTreeFixture(), width: w, height: 24}
		if m.stacksPanel() {
			t.Errorf("width=%d: close mode stacked the panel", w)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/picker/ -run 'TestPaneWidths|TestView_CloseModeNeverStacks' -v`
Expected: `TestPaneWidths_CloseModeHasNoMiddleColumn` FAILs with `width=80: middle column = 48, want 0`.

- [ ] **Step 3: Give close mode two columns**

Replace the `if m.mode == ModeClose { ... }` block inside `paneWidthsThree` (`internal/picker/view.go:170-190`) with:

```go
	if m.mode == ModeClose {
		// Two columns. The tree carries the hierarchy the sub-manifest column
		// used to restate, so it takes the larger share; 30 cells is enough
		// preview to read a pane map and the restore sentence beside it.
		treeW := m.width * 2 / 5
		if treeW < 32 {
			treeW = 32
		}
		if max := m.width - 30; treeW > max {
			treeW = max
		}
		return treeW, 0, m.width - treeW
	}
```

- [ ] **Step 4: Stop close mode stacking**

Replace `stacksPanel` (`internal/picker/view.go:154-160`):

```go
// stacksPanel reports whether the map/scrollback panel goes under the tree
// rather than beside it — the case for a terminal too narrow for a third
// column. The popup is 90% of the client, so that threshold lands near 120
// columns. Close mode only ever has two columns, so it never stacks.
func (m PickerModel) stacksPanel() bool {
	if m.mode == ModeClose {
		return false
	}
	return m.width >= 80 && m.width < 120
}
```

- [ ] **Step 5: Collapse the View branches**

Replace the three close-mode cases in `View` (`internal/picker/view.go:34-52`) with two:

```go
	case m.mode == ModeClose && m.closeTree != nil && m.width < 80:
		content = lipgloss.JoinVertical(lipgloss.Left, renderCloseTree(m, m.width, bodyHeight), footer)
	case m.mode == ModeClose && m.closeTree != nil:
		// Close tree beside the preview of what the cursor's close would
		// reopen. No sub-manifest column: it restated the tree.
		closes := renderCloseTree(m, listWidth, bodyHeight)
		preview := m.renderPreview(previewWidth)
		body := lipgloss.JoinHorizontal(lipgloss.Top, closes, preview)
		content = lipgloss.JoinVertical(lipgloss.Left, body, footer)
```

The `case m.mode == ModeClose:` branch further down (close mode with a nil tree) stays as it is — it is the fallback for a caller that never built a tree.

- [ ] **Step 6: Fix the footer hints**

In `renderFooter` (`internal/picker/view.go:118-127`), replace the `m.width >= 120` block:

```go
	// Tab reaches snapshot mode's sub-manifest tree; close mode has no second
	// tree, and its preview scrolls with Alt+j/k regardless of focus.
	_, _, previewW := m.paneWidthsThree()
	if previewW > 0 {
		if m.mode == ModeSnapshot {
			parts = append(parts, hint(m.keys.Tab))
		}
		parts = append(parts, hint(m.keys.PreviewUp))
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/picker/ -run 'TestPaneWidths|TestView_CloseModeNeverStacks' -v`
Expected: PASS — three tests.

- [ ] **Step 8: Run the full package**

Run: `nix develop -c go test ./internal/picker/...`
Expected: PASS. `TestView_CloseModeStacksPanel` asserts the *old* behaviour and will fail — delete it and note the deletion in the commit body; `TestView_CloseModeNeverStacks` replaces it.

- [ ] **Step 9: Run the whole suite and the linters**

Run: `nix develop -c go test ./...`
Expected: PASS.

Run: `nix develop -c golangci-lint run ./internal/picker/...`
Expected: no findings.

- [ ] **Step 10: Commit**

```bash
git add internal/picker/view.go internal/picker/view_internal_test.go
nix develop -c git commit -m "feat(picker): drop the close picker's redundant sub-manifest column (#102)"
```

---

## Task 6: See it running

The installed binary predates `internal/panemap`, so none of this is visible until a local build runs. This task produces no code — it is the verification gate before the PR.

**Files:** none.

- [ ] **Step 1: Build**

```bash
nix develop -c go build -o /tmp/tmux-remux-102 ./cmd/tmux-remux
```

- [ ] **Step 2: Open the close picker against your real close history**

```bash
tmux display-popup -E -w 90% -h 85% "/tmp/tmux-remux-102 pick --kind=close --session=$(tmux display-message -p '#{session_name}')"
```

- [ ] **Step 3: Check each claim by hand**

Confirm, and report what you actually saw rather than what the plan predicts:

- No row is faint or italic. Restorable rows carry `●`; scaffolding does not.
- Holding `↓` never parks the cursor on a `(live)` / `(gone)` row.
- The preview is populated on the first frame, without pressing Tab.
- The preview changes as the cursor moves, and names what Enter would reopen.
- `←` from a nested pane close collapses its session and leaves the cursor usable.
- Two columns, no empty middle pane.

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin feat/102-close-picker-legibility
gh pr create --assignee @me --title "feat(picker): make the close picker legible (#102)" --body "Closes #102. See docs/superpowers/specs/2026-09-03-close-picker-legibility-design.md."
```

---

## Notes for the implementer

- **`renderWindowMap` already does the hard part.** It reads `Placement` off `CurrentEventID()` to decide which panes to mark as dead, and `CurrentEventID()` reads the close cursor. Do not add a parallel marking path.
- **`closeTreeFixture()` already exists** at `internal/picker/view_internal_test.go:46` and builds the deepest real shape (session → window → pane under "other sessions"). Reuse it; do not write a second one.
- **The frame-size tests are the canary.** `TestRenderList_NeverOverflowsFrame` and `TestRenderCloseTree_NeverOverflowsFrame` assert exact width and height for a grid of sizes. Every task in this plan changes row content or column arithmetic, so run the full package after each one, not just the new tests.
