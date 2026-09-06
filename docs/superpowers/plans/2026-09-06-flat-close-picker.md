# Flat Close Picker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the close picker's tree with a flat, newest-first list plus a scrollback preview.

**Architecture:** `internal/picker` gains a flat row model (`closelist.go`) that replaces the grouped tree (`closetree.go`). The preview stops drawing pane maps and shows the scrollback of every pane a close took down. Navigation loses expand/collapse. Snapshot mode (`prefix+R`) is untouched throughout.

**Tech Stack:** Go, `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`, `github.com/charmbracelet/x/ansi`.

**Issue:** [#104](https://github.com/noamsto/tmux-remux/issues/104). **Builds on:** [#103](https://github.com/noamsto/tmux-remux/pull/103), merged to main as `7c60986`.
**Prototype (evidence, do not copy wholesale):** branch `proto/close-picker-layout`, `cmd/proto-layout/main.go`.

---

## A note on how this plan is written

The previous plan for this area pasted verbatim implementation code into its steps. Three of the defects review caught came directly out of those blocks: a test that could not fail, a frame-height formula that dropped the panel's bottom border, and a session filter strict enough to blank the preview. **This plan gives you exact interfaces, exact expected outputs, and the facts you need — and leaves the implementation to you.** Where a literal string is the spec, it is marked as such. Everything else is yours to write, and yours to be responsible for.

---

## Global Constraints

- Work in `~/Data/git/.worktrees/noamsto/tmux-remux/feat-104-flat-close-picker` on branch `feat/104-flat-close-picker`.
- Tests: `nix develop -c go test ./...`. Lint: `nix develop -c golangci-lint run ./...`. Commit: `nix develop -c git commit` — the pre-commit hooks need the devshell on PATH.
- **Snapshot mode (`prefix+R`) must keep its current behaviour exactly.** `renderPreview`, `previewMaxScroll`, `previewSHA`, `PreviewCmd`, `paneWidthsThree`, `stacksPanel`, `View` and `keys.go` are all shared. Branch on `m.mode` wherever behaviour differs. #103 verified snapshot mode byte-identical to its merge base by differential rendering; do not erode that.
- **lipgloss v2.0.6's `MaxHeight` hard-truncates the rendered line list.** Over-tall content silently loses its closing border rather than pushing it out. A `lipgloss.Height(out) == h` assertion therefore **cannot** detect a dropped border — a frame test must also assert the last row carries the `╰`/`╯` corners. `TestRenderClosePreview_NeverOverflowsFrame` is the model to follow.
- **Never nest a styled string inside a style that sets a background** — lipgloss strips ESC bytes from pre-styled input and the inner colour collapses to invisible. Build each row as plain text and apply one style to the whole line.
- Every row rendered into a frame must be truncated to the inner width with `ansi.Truncate`, so one row is always exactly one physical line.
- A test that passes before the change it guards is not a regression test. For every new test, confirm it fails first for the right reason and report that evidence. If a test cannot be made to fail, say so rather than shipping it.

## Measured facts this design rests on

Verified against the real store; do not re-derive or contradict without new measurement.

| Fact | Value |
|---|---|
| Pane counts per window | **1 pane: 95%, 2 panes: 5%, 3+: none** (77/4/0 of 81 distinct windows) |
| Consequence | There is no layout worth drawing. No pane map, no expansion, no minimap. At two panes, show both. |
| The discriminator | **Scrollback.** Closes routinely tie on session, window, command and cwd. |
| Nerd-font window names | Glyph-dense (`3: 󰊤 #511 🧠 󰒲 󰗠 #517`) and measure narrower than they paint. Fixed columns expose the drift. |
| Unrecoverable closes | ~318 of ~348 are filtered out before display. Tracked separately in #105 — **out of scope here.** |

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/picker/closelist.go` | Flat row model: build, group, sort, collapse duplicates | **Create** |
| `internal/picker/closelist_test.go` | Its tests | **Create** |
| `internal/picker/closetree.go` | The grouped tree | **Delete** (Task 6) |
| `internal/picker/closetree_test.go` | Its tests | **Delete** (Task 6) |
| `internal/picker/view.go` | Row rendering, widths, layout | Modify |
| `internal/picker/preview.go` | Stacked scrollback preview | Modify |
| `internal/picker/model.go` | Cursor, key handling | Modify |
| `internal/closeevent/diff.go` | `SubManifest` pane-scope fix | Modify (Task 3) |

---

## Task 1: The flat row model

Pure data. No rendering. This is the task most worth getting exactly right, because every later task reads its output.

**Files:** create `internal/picker/closelist.go`, `internal/picker/closelist_test.go`.

**Produces** — later tasks depend on these names, so keep them:

```go
type CloseRowKind int // RowSectionHeader, RowClose

type CloseRow struct {
    Kind      CloseRowKind
    Section   string   // header text; empty on a close row
    EventID   int64    // 0 on a header
    Ts        int64    // newest timestamp in the group
    Count     int      // 1, or N for a collapsed group
    Scope     string   // "session" | "window" | "pane"
    Session   string
    Placement ClosePlacement
}

func BuildCloseList(evs []store.Event, ctxs map[int64]CloseContext, current string, live map[string]bool) []CloseRow
func (r CloseRow) Selectable() bool
```

**Behaviour:**

- One row per close event, except where collapsed. Events with an empty `SubManifest.Sessions` are excluded, exactly as `BuildCloseTree` does today — the caller counts them as hidden.
- Exactly two section headers, in this order, each emitted **only when it has at least one close under it**. The literal formats are the spec:
  - `THIS SESSION · <current>`
  - `OTHER SESSIONS`
- A close belongs to `THIS SESSION` when its placement session equals `current` **and** its scope is not `session` — a session close is by definition not the session you are sitting in. This matches `BuildCloseTree`'s existing rule; read it rather than reinventing it.
- Within each section, newest first.
- **Duplicate collapse.** Two closes collapse into one row when they agree on **scope, session, window index, the closed pane's command, and the closed pane's cwd**. The surviving row carries the newest `Ts` and `Count` = the number collapsed. Collapsing is within a section only.
- Headers are never selectable; close rows always are.

**Known trap:** the prototype's collapse missed the obvious real case — eight consecutive `session tmux-remux · 1w · (gone) → agentdetect-…` rows differing only in a truncated target name. Work out why before you write the rule (they are session-scope closes, so "window index" and "the closed pane" need a defined meaning for that scope), and cover it with a test built from that shape.

- [ ] **Step 1: Write the tests first.** Cover, at minimum: section assignment including the session-scope exception; newest-first ordering within a section; a header suppressed when its section is empty; collapse of an identical pair; **non**-collapse of two closes differing only in cwd; the eight-session-close shape above; and exclusion of an empty-sub-manifest event.
- [ ] **Step 2: Run them and confirm they fail** for the right reason (undefined symbols is acceptable here, since the file is new).
- [ ] **Step 3: Implement `closelist.go`.** Read `closetree.go` first — its section rule, its newest-first sort and its empty-sub-manifest filter are all correct and worth carrying over. Do not delete it yet; Task 6 does that.
- [ ] **Step 4: Run the package.** Expected: your new tests pass, everything else still passes — nothing consumes `CloseRow` yet.
- [ ] **Step 5: Commit.**

---

## Task 2: Row labels and columns

**Files:** modify `internal/picker/view.go`; tests in `internal/picker/view_internal_test.go`.

**Consumes:** `CloseRow` from Task 1.
**Produces:** a renderer for one row at a given inner width, and the column-budget helper it uses. Name them as you see fit; Task 5 calls the row renderer.

**Columns, left to right.** Widths are yours to budget; the order and content are the spec.

| Column | Content | Notes |
|---|---|---|
| marker | `●` on every selectable row | Headers carry no marker |
| kind | `pane` / `window` / `session` | |
| cwd | the discriminating tail only | see elision rule below |
| name | window name, `StripFormat`ed | `<Nw>` for a session close |
| extra | the closed pane's command; `(gone)`; `Np` | see elide-defaults below |
| target | `→ <session>:<index>`, or `→ <session>` for a session close | where Enter puts it back |
| count | `×N` | omitted when N is 1 |
| age | relative, e.g. `4m`, `18h`, `2d` | reuse `humanAge` from `preview.go`; drop its ` ago` suffix here |

**cwd elision — the rule that decides whether this column earns its width.** Compute each session's **modal cwd** across the closes in this list, and elide the column for any row whose cwd equals it; show the discriminating tail otherwise, left-truncated with a leading `…`.

Do **not** key this on comparing the cwd's basename to the session name. The prototype did, and it fails twice: it leaves a bare repo name repeating down the column nearly as monotonously as the full path, and it misfires whenever session and repo names differ — session `tp-g6-nix-config` in `~/nix-config` printed `nix-config` where it should have printed nothing.

**Elide defaults.** Do not print `fish`, `1p`, or `(live)`. Do print `claude`, `2p`, `(gone)`.

**Truncation.** Nerd-font glyphs measure narrower than they paint, so a name column budgeted on `lipgloss.Width` will clip mid-glyph-run and show icons with the words cut off. Prefer giving the name column slack over the cwd column, and truncate with `ansi.Truncate`.

- [ ] **Step 1: Write the tests first** — a table over real-shaped rows asserting the exact rendered text at a couple of widths. Include: a row whose cwd is its session's modal cwd (column elided), a worktree row (tail shown), `×2`, `(gone)`, a session close's `<Nw>` and `→ <session>` forms, and a glyph-dense name that must truncate without leaving a dangling partial escape.
- [ ] **Step 2: Run them; confirm they fail for the right reason.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the package.**
- [ ] **Step 5: Commit.**

---

## Task 3: The preview — stacked scrollback

**Files:** modify `internal/picker/preview.go`, `internal/closeevent/diff.go`; tests in `internal/picker/preview_test.go` and `internal/closeevent`'s tests.

**Two things happen here, and the bug comes first.**

**3a. Fix `ClosedItem.SubManifest` for pane scope.** It currently returns the **whole enclosing window**, so a pane close reports its siblings as things it took down. This is latent in #103 and becomes visible here. It is also the root cause of a second symptom: the prototype's list showed `claude` while its preview rule showed `fish` **for the same close**, because the list took `w.Panes[0]` (a sibling) and the preview filtered by `Placement.PaneID` (the pane that died). Fix the sub-manifest so pane scope carries only the pane that died, then confirm both symptoms are gone — do not assume the second one follows.

Check every consumer of `SubManifest` before changing it: `restoreSentence`, `paneCount`, the restore path in `cmd/tmux-remux`, and `BuildCloseList` from Task 1. A narrower sub-manifest must not silently change what a restore actually restores. If it would, say so and stop.

**3b. Rewrite the close preview.** Delete the pane map from close mode. In its place:

**Do not touch snapshot mode's map machinery.** Main landed #97 while this branch was in flight, which added a mini-map *above* pane scrollback in snapshot mode: `paneHintShows`, `paneScrollbackHeight`, `paneContextMap` and `paneMapHintHeight`, alongside the existing `renderWindowMap`. All of it is snapshot-only — `paneHintShows` is guarded on `ModeSnapshot` — and all of it must survive this task intact, with `TestPickerModel_PaneViewShowsContextMap` still passing. Snapshot mode is the mode that actually has multi-pane windows; the map earns its place there.


- Header: what this close was and when. Two lines, e.g. `halo-nix-amd-ai:1 · nix-amd-ai` then `window close · 2 panes · closed 2d ago`.
- Body: the scrollback of **every pane the close took down**. One pane fills the panel. Two stack, with the available height divided between them.
- A pane with no captured scrollback says so in its own slot rather than leaving it blank.

**Pane boundaries — a thin rule is not good enough, and this is a measured requirement, not a preference.** The captured scrollback contains its own horizontal rules and box-drawn status bars (visible in the real renders: the agent sessions print `──────────` separators). Chrome that is also a thin rule is indistinguishable from content. Each pane's block therefore gets:

- A **filled label bar** across the panel width carrying `<index> · <command>` and a position marker (`1 of 2`).
- A **coloured left rail** — a gutter glyph on *every* content row of that pane's block, not just at the boundary — so the block identity is readable from any row, including mid-scroll and where the content prints its own rules.
- A **different accent colour per pane**, so two blocks are distinguishable without reading the labels.
- Content **inset one column** and truncated to the remaining width, so it can never write into the rail. This is the property that makes the rail trustworthy: content can fake any glyph, but it cannot reach a column it is not given.

Compose the rail and the content as separate styled strings concatenated together — do **not** wrap a line that already carries the content's own SGR inside another style, per the global constraint above.
- Drop the `↵ reopens …` sentence added in #103. The row's `→ target` column says it now.
- Keep the existing sanitisation and the scrollback loading lifecycle (`previewSHA` / `PreviewCmd` / `scrollbackLoadedMsg`). Both panes of a two-pane close need their scrollback scheduled, so `previewSHA` returning a single hash is no longer sufficient — widen it, and keep the in-flight bookkeeping correct so no path can hang on `(loading scrollback…)`.

- [ ] **Step 1: Write the failing test for 3a** — a pane-scoped close whose enclosing window has two panes must yield a sub-manifest containing exactly the pane that died.
- [ ] **Step 2: Confirm it fails; implement; confirm it passes; run the whole suite** — this touches shared code, so watch for consumers that depended on the old breadth.
- [ ] **Step 3: Commit 3a on its own.** It is a bug fix and should be reviewable separately.
- [ ] **Step 4: Write the failing tests for 3b** — one-pane fills the panel; two panes both appear under correctly-named rules; a pane without scrollback says so; and a frame guard in the shape of `TestRenderClosePreview_NeverOverflowsFrame` covering both the one- and two-pane bodies at inner heights 1-4 and a comfortable height, asserting the `╰`/`╯` corners survive.
- [ ] **Step 5: Confirm they fail; implement; confirm they pass.**
- [ ] **Step 6: Run the whole suite and lint. Commit.**

---

## Task 4: Navigation

**Files:** modify `internal/picker/model.go`; tests in `internal/picker/model_test.go`.

The flat list has no hierarchy, so expand/collapse goes away entirely in close mode.

- `m.cursor` indexes the `[]CloseRow` from Task 1.
- `↑`/`↓` move between **selectable** rows, skipping section headers.
- `←`/`→` do nothing in close mode. Remove the close-mode expand/collapse handling and the helpers only it used (`closeNavAt`, the ancestor walk, `isCloseNavTarget`, `CloseVisible`, `FlattenClose`) — but only once nothing references them.
- `Enter` restores the row under the cursor. Every selectable row carries an `EventID`, so the `(group — nothing to restore here)` footer note has no remaining trigger; remove it and the test that covers it.
- Cursor movement resets `previewScroll`/`previewScrollX` and returns `PreviewCmd()`, as the current Up/Down cases do.
- The initial cursor lands on the newest close — the first selectable row.
- `Bootstrap`/`Init` must still schedule the first scrollback load, as #103 fixed.

**Snapshot mode keeps `focusTree`, `Tab`, and its own `←`/`→` tree navigation untouched.**

- [ ] **Step 1: Write the failing tests** — `↑`/`↓` never land on a header; `←`/`→` are inert in close mode; `Enter` on the initial cursor selects the newest close; the first scrollback load is scheduled before any key is pressed; and a snapshot-mode test asserting `Tab` and tree navigation still work.
- [ ] **Step 2: Confirm they fail; implement; confirm they pass.**
- [ ] **Step 3: Run the whole suite and lint. Commit.**

---

## Task 5: Layout

**Files:** modify `internal/picker/view.go`; tests in `internal/picker/view_internal_test.go`.

- **≥110 columns:** list and preview side by side. Give the preview enough width to read a wrapped line of scrollback — the prototype's split was too list-heavy and the preview suffered. Pick the split by rendering it and looking, then say in your report what you chose and why.
- **<110 columns:** the side-by-side preview stops working — the target column truncates to uselessness. Stack the preview below the list, or drop it. Choose, justify, and make sure the list stays fully legible at 80.
- **Pin the current section header.** Past ~35 rows the headers scroll out of view and nothing says which section you are in. Keep the active section's header visible at the top of the list.
- Keep the `— N unrecoverable closes hidden —` footer.
- Footer hints must match reality: no `→/l:expand` in close mode any more.

Frame guards: assert exact width and height **and** the `╰`/`╯` corners, across a size matrix, for close mode. Add the same corner assertion to `TestRenderList_NeverOverflowsFrame` and `TestRenderCloseTree_NeverOverflowsFrame` if they still exist — #103's review found they assert only height and width, which cannot catch a dropped border.

- [ ] **Step 1: Write the failing tests** — column geometry at 80/100/110/130/160/200; both frames close; the pinned header is present when scrolled; the expand hint is gone from close mode's footer; snapshot mode still renders three columns.
- [ ] **Step 2: Confirm they fail; implement; confirm they pass.**
- [ ] **Step 3: Run the whole suite and lint. Commit.**

---

## Task 6: Remove the tree, and tell the truth about it

**Files:** delete `internal/picker/closetree.go` and `internal/picker/closetree_test.go`; modify `cmd/tmux-remux/main.go`, `README.md`, `demo/*.tape` as needed.

- Delete `BuildCloseTree`, `CloseNode`, `FlattenClose`, `closeGuidePrefix`, `closeRow`, `closeRowStyle`, `renderCloseTree` and anything else only the tree used. Let the compiler and `golangci-lint` find the orphans; do not guess.
- Update `cmd/tmux-remux/main.go` to build the list instead of the tree.
- `README.md` describes the close picker's layout — update it to match, including the width behaviour.
- `demo/pick.tape` drives the picker with keys that may no longer do anything (`→` to expand). Check it and fix it; regenerating the GIF is optional and can be a follow-up.
- Search the tree-era comments for claims that are no longer true.

- [ ] **Step 1: Delete, and let the build tell you what breaks.**
- [ ] **Step 2: Fix every break. Run `nix develop -c go build ./...`, the whole suite, and lint.**
- [ ] **Step 3: Update README and the demo tape.**
- [ ] **Step 4: Commit.**

---

## Task 7: See it running

No code. The verification gate before the PR.

- [ ] **Step 1:** `nix develop -c go build -o /tmp/tmux-remux-104 ./cmd/tmux-remux`
- [ ] **Step 2:** Open it against the real close history:
  `tmux display-popup -E -w 90% -h 85% "/tmp/tmux-remux-104 pick --kind=close --session=$(tmux display-message -p '#{session_name}')"`
- [ ] **Step 3:** Check each claim by hand and **report what you actually saw**, not what the plan predicts: every visible row is actionable; the cwd column is mostly empty and speaks only on worktree rows; the preview shows readable scrollback for the row under the cursor; a two-pane close stacks both; the eight repeated `agentdetect` closes are one row with `×8`; the list and preview never disagree about which pane; nothing is left of the pane map in close mode.
- [ ] **Step 4:** Hand the binary to the user for their own look before opening the PR.
