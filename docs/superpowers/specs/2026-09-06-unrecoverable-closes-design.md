# Why 90% of recorded closes are unrecoverable — design

Issue: #105. Follow-on from #104 (picker rendering). Observation: ~318 hidden
vs ~30 shown close events against the real store.

## Problem

`buildCloseContexts` (cmd/tmux-remux/main.go) resolves each close event against
the newest snapshot strictly before it (`LatestSnapshotBefore`) via
`closeevent.FindClosed`. A nil item → empty `SubManifest` →
`partitionRecoverable` hides the event. ~90% of recorded closes land there.

Candidate causes (from the issue):

1. **Cadence** — snapshot interval too slow; create-then-close inside one
   interval leaves no prior snapshot containing the entity.
2. **Retention** — `PruneSnapshots` (keep 20 + per-day floor over 7 days)
   evicts snapshots faster than `PruneCloseEvents` (keep 50) evicts closes, so
   old closes lose the prior state they need.
3. **Never-recoverable-in-principle** — the closed pane/window/session never
   appeared in *any* snapshot; recording it at close time was already
   pointless.

## Approach

Investigation first, change second. A scratch Go analysis (inside the module,
not committed) opens a **copy** of the real `state.db` and classifies every
close event:

- recoverable vs hidden, per kind;
- for hidden: the exact `FindClosed` failure mode (no prior snapshot; entity
  still live; id-aware prior lacks the id; session ambiguity; empty diff);
- distribution of `close_ts − prior_snapshot_ts`;
- whether the closed entity id appears in *any* snapshot in the store;
- age clustering of hidden closes (retention check).

The real DB is never opened writable; analysis runs against a scratch copy.

## Fix (evidence-gated)

The numbers pick one of:

- **Cadence** → save more eagerly around entity birth (or accept and record
  less).
- **Retention** → align close-event pruning with snapshot survivability.
- **Never-recoverable** → stop recording closes that cannot be resolved at
  capture time (record less at the source), the honest fix per the issue.

If evidence points at more than one, land the highest-value one and file a
follow-up issue for the rest.

## Acceptance

- PR body states with numbers which cause dominates and how it was measured.
- Landed change matches the evidence, with a test pinning the new behavior.
- Before/after recoverable-vs-hidden counts reported.
- Unsettled parts become a linked follow-up issue.
- `go build ./... && go vet ./... && go test ./...` + repo lint/format clean.

## Out of scope

Close-picker layout/drawing (#104 territory).
