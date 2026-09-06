# Plan: diagnose why 90% of recorded closes are unrecoverable (#105)

Spec: `docs/superpowers/specs/2026-09-06-unrecoverable-closes-design.md`.
Consult: none — survey did not trip (sequential investigation, localized fix
candidates, no shared-interface churn).

## Steps

1. **Scratch analyzer** (not committed): a Go test under
   `internal/closeevent/` (env-gated, e.g. `ANALYZE_DB=/path`) that:
   - copies the real `state.db` to a scratch path, opens the copy;
   - lists all non-snapshot events; for each, parses the close manifest and
     replays `buildCloseContexts`'s resolution (`LatestSnapshotBefore` +
     `FindClosed`);
   - buckets: recoverable vs hidden by kind; hidden by failure mode;
     histogram of `close_ts − prior_snapshot_ts`; whether the closed entity
     id (pane_id / window_id / session name) appears in the prior snapshot,
     and whether it appears in *any* snapshot; hidden-count by event age
     bucket (retention clustering).
   - prints a summary table.
   Run it, capture numbers. `implement: grok-4.6-medium-fast`

2. **Interpret** (worker, no delegation): answer the issue's three questions
   from the numbers; pick the fix the evidence supports.

3. **Implement the fix** + regression test pinning the new behavior.
   Scope unknown until step 2 — candidates:
   `internal/closeevent/capture.go` (record less),
   `internal/store/store.go` + `internal/config/config.go` (retention),
   `internal/snapshot/save.go` + triggers (cadence).
   `implement: grok-4.6-medium-fast`

4. **Re-run analyzer** on the same DB copy for before/after
   recoverable-vs-hidden counts. (Worker runs; analyzer already exists.)

5. **Fast deterministic gate**: `go build ./... && go vet ./... &&
   go test ./...` + repo lint/format (check `.pre-commit-config*` / CI).
   Remove the scratch analyzer before this if it is not earning a place.

6. **Review gate** (full — no PR review gauntlet on this repo): go language
   reviewer + targeted test-runner in one parallel batch; reconcile; deep
   conditional second pass only on non-trivially-fixed HIGH findings.

7. Push, open PR (`Closes #105`), body carries the numbers, before/after
   counts, and any follow-up issue link.

## Validation

- Analyzer output table (evidence).
- New regression test green in `go test ./...`.
- Full gate green.
