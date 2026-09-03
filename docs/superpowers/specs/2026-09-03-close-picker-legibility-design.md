# Close picker legibility — design

Issue: [#102](https://github.com/noamsto/tmux-remux/issues/102)
Date: 2026-09-03

## Problem

The close picker (`prefix+U`) is harder to read than the snapshot picker
(`prefix+R`), despite showing less. Three causes, all in `internal/picker`.

### 1. The tree signals importance where it means eligibility

`BuildCloseTree` produces two kinds of node. An **event node** carries an
`EventID` and can be restored. A **scaffolding node** carries no event — it
exists only because something below it needs a parent to indent from: the
window a dead pane lived in, the session that window belongs to. Both kinds
share the left column, interleaved.

`closeRow` separates them with `style.Faint(true).Italic(true)`
(`internal/picker/view.go:369`). On a dark Catppuccin ground, faint-over-dim is
close to unreadable, and what legibility survives says *old* or *unimportant* —
not *not a target*. Age is already spoken for: `dimOlderThan` dims genuinely
old rows in snapshot mode with the same visual move. Two different meanings,
one visual channel.

### 2. The cursor lands on rows that cannot act

`isCloseNavTarget` (`internal/picker/model.go:656`) admits any row that has an
`EventID` **or** has children:

```go
return n.EventID != 0 || len(n.Children) > 0
```

Leaf scaffolding is already skipped. But scaffolding almost always has
children — that is the entire reason it exists — so `↑`/`↓` stop on it anyway.
Pressing Enter there is answered with `(group — nothing to restore here)`. The
picker offers a row, then refuses it.

### 3. The right two columns do not pay for their width

Close mode renders three columns: close tree, sub-manifest tree, preview
(`view.go:44-52`).

- The **preview** returns `(press Tab to preview panes)` until
  `m.focus == focusTree` (`preview.go:69`). `prefix+U` therefore opens on a
  dead panel, and nothing on screen except a footer hint says Tab is what
  revives it.
- The **sub-manifest tree** restates the hierarchy the close tree already
  draws. For a window close it shows that window and its panes — which the
  close tree shows one column to the left, in the same shape.

## Non-goals

- Changing what is restorable, or how `restore.BuildPlan` executes a restore.
- Touching snapshot mode's three-column layout.
- Reworking the close-event capture path (`internal/closeevent`).

## Design

Close mode keeps its tree and becomes two columns: **close tree | preview**.

```
┌ closes ─────────────────────────────────┐┌ preview ────────────────────┐
│ ▾ other sessions                        ││  ┌───────┬─────────┐        │
│  ├─ dispatcher  (live)                  ││  │   1   │    2    │        │
│  │  ├─ ● 2: bump-model-map #79   16:39  ││  │ fish  │ claude  │        │
│  │  └─ 1: dispatcher  (live)            ││  └───────┴─────────┘        │
│  │     └─ ● pane: fish          16:36   ││                             │
│  └─ ● agentdetect-test-2612860  16:25   ││  ↵ reopens window 2         │
│                                         ││    in dispatcher at index 2 │
│                                         ││  2 panes · closed 22m ago   │
└─────────────────────────────────────────┘└─────────────────────────────┘
```

### D1. Colour by role, never by importance

`closeRow` drops `Faint` and `Italic` entirely. Role is carried by three
independent channels:

| Channel | Meaning | Value |
|---|---|---|
| Leading marker | restorable | `● ` on event rows; two spaces on scaffolding |
| Foreground | what kind of thing | existing `nodeSession` / `nodeWindow` / `nodePane` for event rows; `Subtext` for scaffolding |
| Trailing tag | liveness of scaffolding | ` (live)` / ` (gone)`, in `Overlay` |

The marker is the load-bearing cue; colour reinforces it. `State` moves from
the bare `" · live"` suffix `closeRow` appends today to a parenthesised tag, so
it cannot be mistaken for part of the window name.

The active row keeps the existing single flat `rowActive` style — lipgloss v2
strips ESC bytes from pre-styled input, so nesting a role colour inside
`rowActive`'s background can collapse to invisible. That constraint is already
documented at `view.go:355` and this design does not disturb it.

### D2. The cursor only lands where Enter works

```go
func isCloseNavTarget(n *CloseNode) bool {
    return n.EventID != 0 || IsCloseGroup(n)
}
```

Group headers stay navigable so they can be collapsed; every other stop is
restorable. Enter's `(group — nothing to restore here)` branch becomes
reachable only on the two group headers, where it is a true statement about a
section rather than a refusal of a row that looked selectable.

Collapsing must survive the cursor no longer visiting scaffolding:

- `→` on a group header expands it. On an event row it expands that row if it
  has children (a window close parenting an older pane close), otherwise it is
  a no-op.
- `←` collapses the cursor row when it is an open parent. Otherwise it walks up
  to the **nearest collapsible ancestor**, collapses it, and moves the cursor to
  the nearest navigable row at or above that ancestor's position — the group
  header when the ancestor was a top-level session. The existing handler
  (`model.go:349-360`) already walks to the parent; it needs to keep walking
  past scaffolding rather than stepping onto it.

### D3. The preview follows the left cursor

`renderPreview` stops gating on `m.focus == focusTree`. In close mode it reads
the close-tree cursor row and renders, by the row's `Placement.Scope`:

| Scope | Preview body |
|---|---|
| `window` | `renderWindowMap` of that window from `SubManifest`; every pane marked dead |
| `pane` | scrollback of the closed pane, with `renderWindowMap` unavailable — the pane's `ScrollbackSHA` drives the existing scrollback path |
| `session` | `renderWindowMap` of the session's first window, titled with the session's window count |
| group header | the section's close count and time span |

`renderWindowMap` (`preview.go:197`) already reads `Placement` to mark which
panes came down, and already handles the pane/window/session distinction in its
`marked` closure. It needs a caller driven by the close cursor rather than by
`m.treeCursor`.

Tab keeps its meaning of "let me scrub inside the preview" — in close mode it
moves focus to the preview so `↑`/`↓` scroll scrollback, rather than to a tree
that no longer exists. `focusTree` is reused for that state rather than a third
zone being added: snapshot mode keeps its current meaning of "the sub-manifest
tree", close mode reads it as "the preview". `focusedPaneSHA`
(`model.go:736`) and `PreviewCmd` branch on mode accordingly.

### D4. The what-happens line

Below the map, in `Subtext`, two or three lines built from `Placement` and
`SubManifest`:

```
↵ reopens window 2
  in dispatcher at index 2
2 panes · closed 22m ago
```

Per scope:

- `window` — `reopens window <WindowName> in <Session> at index <WindowIndex>`
- `pane` — `reopens a pane in <Session>:<WindowIndex>`
- `session` — `reopens session <Session> (<n> windows)`

`WindowIndex` restoring to its original slot is behaviour #63 already
guarantees, so naming the index here is a promise the restore path keeps.

The relative age (`closed 22m ago`) replaces nothing — it complements the
absolute `16:39` the row already carries, which is what you need when the
close was yesterday.

### D5. Two columns

`paneWidthsThree` returns `(list, 0, preview)` for close mode. The width bands:

- `< 80`: tree only, as today.
- `≥ 80`: tree and preview, split so the tree keeps its 32-cell floor and the
  preview gets the rest, with the preview stacking below the tree under 120
  rather than disappearing.

The `stacksPanel` / `panelFrameHeight` machinery already expresses this; close
mode's three-column branches in `View()` collapse into the two-column ones.

## What this rejects, and why

**Folding single-child scaffolding chains into their child** (`1: dispatcher` +
`pane: fish` → `1/fish`) was the leading alternative. It is rejected because
"scaffolding row with exactly one child" is a property of *close history*, not
of the tmux layout. The moment a second pane dies in `dispatcher:1`, the chain
un-folds and the row you learned as `1/fish` becomes two rows again. The tree
would change shape between invocations for reasons the user did not cause and
cannot see — and a tree navigated by muscle memory must not do that. It also
saves one row in the observed case, while dropping the window's name and
introducing a second label grammar alongside `2: bump-model-map #79`.

If depth still reads as noise once the dimming is gone, the follow-up is
**collapsing below the session level by default** — a stable rule the user
opens with `→` — not a fold driven by history.

## Testing

Existing suites to extend rather than replace: `closetree_test.go`,
`model_test.go`, `view_internal_test.go`, `preview_test.go`.

- **Navigation** — from a tree with a session→window→pane scaffolding chain,
  assert `↑`/`↓` visits exactly the group headers and the event rows, in
  newest-first order, and never a scaffolding row.
- **Collapse** — with the cursor on a pane-close nested two scaffolding levels
  deep, assert `←` collapses the top-level session node and leaves the cursor on
  a navigable row, not on the collapsed scaffolding.
- **Styling** — assert the rendered close tree contains no faint (`ESC[2m`) or
  italic (`ESC[3m`) sequence, and that every event row carries the `●` marker
  while no scaffolding row does. This is the regression test for the actual
  complaint, so it is asserted on the byte level rather than by eye.
- **Preview without Tab** — construct the model, deliver the initial
  `WindowSizeMsg`, and assert the preview pane renders a map plus a
  `↵ reopens` line with `m.focus == focusList`.
- **What-happens line** — table test over the three scopes, asserting the exact
  sentence for each.
- **Widths** — assert `paneWidthsThree` returns a zero middle column in close
  mode at 80, 119, 120 and 200 columns, and that the rendered frames sum to the
  terminal width at each (the existing width-sum assertions cover the
  arithmetic that border bleed shows up in).

## Rollout note

The binary currently installed via Nix predates `internal/panemap` — it has
`internal/picker/closetree.go` but no pane map, no `"new session"` reason
words, and no `"press → to expand"` string. Whatever this design produces will
not be visible until that package is rebuilt, so the change should be verified
against a locally built binary, not against the installed one.
