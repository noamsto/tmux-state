# Close-picker layout prototype — findings

Throwaway prototype: `cmd/proto-layout/main.go`. Opens a copy of the real store
(`~/.local/share/tmux-remux/state.db` → `/tmp/proto-layout.db`), runs the same
close pipeline `pick --kind=close` runs, and prints the list two ways.

```
nix develop -c go run ./cmd/proto-layout --layout=tree --cols=130 --rows=40
nix develop -c go run ./cmd/proto-layout --layout=flat --cols=130 --rows=40
nix develop -c go run ./cmd/proto-layout --layout=flat --cols=80  --rows=28
```

`internal/` was not modified. `picker.Theme` is fully exported, so the prototype
rebuilds the styles it needs (`nodeSession`, `nodeWindow`, `rowScaffold`, …) from
the same theme rather than needing a new exported helper. `closeGuidePrefix`,
`closeRow` and `buildCloseContexts`/`placementFor` are unexported (the last two
live in `package main` of `cmd/tmux-remux`), so the prototype re-implements them.

## The real data

Newest 50 non-snapshot events → 24 recoverable, 26 unrecoverable/hidden.
23 distinct identities (session + window index + command + cwd).

**The five-way tie is not there.** Exactly one pair collapses:
`pane | lazytmux | window 1 | bash | ~/Data/git/noamsto/lazytmux` (ids 31747,
31700). So `×N` fires once in the whole visible list. Either the five tying
events are among the 26 hidden ones (they never reach the picker), they fall
outside the 50-event window, or the tie was measured on the raw close manifest
rather than on the resolved close context. Worth re-checking before treating
`×N` as a load-bearing feature — on today's history it earns one row.

## Variant A — folded tree

Folding is a clear win. Before folding, a session→window→pane chain burned three
rows to reach one restorable pane. After, it is one row:

```
├─ ● halo-nix-amd-ai · 1: nix-amd-ai · pane: fish       6m
```

The tree went from ~34 rows to 28 for the same 24 events, and — more important —
every row above the fold is now actionable except the two group headers and the
session scaffolds that genuinely hold several closes (`mono`, `dispatcher`,
`lazytmux`). Those surviving scaffolds are the ones you actually want: they group
five closes from the same session.

Annotations: `· 2w 2p` on a session close and `· fish` / `· claude` on a
single-pane window close both read well and cost little. The spec's two examples
resolved to one rule — annotate from the event's own sub-manifest (session →
`Nw Np`; window with exactly one pane → that pane's command).

`(live)`/`(gone)` needed a tweak: carrying a folded scaffold's state onto its
child tagged nearly every row `(live)`, which is the default and pure noise. The
prototype only carries `gone` down.

## Variant B — flat cwd-primary

Columns: `● kind cwd window extra → reopen ×N age`. The `extra` column carries
exactly the non-defaults (command ≠ `fish`, pane count ≠ 1, `(gone)`).

What it buys: the worktree tails are visible at a glance and they are the thing
that actually discriminates —

```
● window  …atcher/feat-90-docs-land-the-orphaned-worker-window-fin  worker-window-g… claude 2p  → dispatcher:2  6m
● window  …atcher/feat-89-dispatch-tier-model-gate-and-budget-awar  󰊤 #89 🧠 󰒲 󰗠 #94 claude     → dispatcher:3  6m
```

In the tree those two are `2: worker-window-geometry-findings 🧠 󰒲 (2p)` and
`3: 󰊤 #89 🧠 󰒲 󰗠 #94 (1p) · claude`, which say nothing about which branch.

What it costs: strict newest-first destroys the grouping. Three rows that the
tree nests as one session close containing one window containing one pane show up
as three unrelated adjacent rows —

```
● session ~/Data/git/factify  1w  (gone)  → factify    3m
● window  ~/Data/git/factify  factify (gone) → factify:1  3m
● pane    ~/Data/git/factify  factify  → mono:1  3m
```

A user scanning that cannot tell that restoring the first also restores the
second. The tree makes containment obvious; the flat list actively misleads.

At 80 columns it degrades acceptably but the window-name and reopen columns both
collapse to `…`, and dropping the session name from the reopen target inside
`THIS SESSION` leaves a bare `→ 2` that reads as garbage until you know the rule.

## My read

**Neither as-is. Take the folded tree and give it the flat list's cwd.**

The fold is the change that matters — it removes the pure-indentation rows, which
were most of the noise. Containment is real information the flat list throws
away, and the sections that survive folding are exactly the ones that carry more
than one close.

But the tree's labels are the wrong labels. Window names on this user's machine
are decorated tmux titles (`󰊤 #89 🧠 󰒲 󰗠 #94`, `󰰍 ENG-8226  󰀨 #3414`) which are
glyph-dense and, across worktrees of the same repo, near-identical. The cwd tail
is what tells `feat-89-…` from `feat-90-…`. The tree row has room: most folded
rows end well before column 60.

Concretely: keep the folded tree, replace the pane-command tail with the
left-truncated cwd tail, keep the age right-aligned, keep `×N`.

## Surprises / not handled by either

- **The 26 hidden closes are more than half the history.** Both variants print a
  one-line `— 26 unrecoverable closes hidden —` footer under a list of 24. That
  ratio suggests the snapshot cadence, not the layout, is the real legibility
  problem. Neither variant addresses it.
- **cwd is often not the closed thing's cwd.** `pane … ~/Data/git/noamsto/lazytmux
  … nix-config → tmux-remux:2` — a pane whose window is named `nix-config` sitting
  in the lazytmux checkout. Real, but it means cwd and window name disagree often
  enough that showing only one of them is a guess.
- **Nerd-font glyphs break column math visually.** `󰊤 #511 🧠 󰒲 󰗠 #…` measures
  narrower than it paints in some terminals; the flat list's fixed columns show
  the drift, the tree (single flexible label) hides it. A point for the tree.
- **Two rows can still be textually identical after `×N`** when they differ only
  in window index — see the two `2: nix-config · pane: fish` / `1: nix-config ·
  pane: fish` rows in this session. The identity key deliberately keeps them
  apart (different windows), but the tree label does not always show the index
  after folding.
- Neither variant has a story for a session close that also parents *later*
  window closes — it renders as an actionable row with actionable children, and
  nothing says whether restoring the parent supersedes the children.
