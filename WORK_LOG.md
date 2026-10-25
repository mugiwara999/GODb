# goDB Work Log

## Current state

- Branch: `agent` (already active when this session began).
- Starting commit: `deb1cff` (`add README`).
- The worktree contained an untracked `AGENTS.md` supplied for this task; it has been preserved.
- No prior `WORK_LOG.md` existed.
- Baseline: `GOCACHE=/tmp/goDB-gocache go test ./...` passed all packages; `GOCACHE=/tmp/goDB-gocache go build ./...` passed. The first attempts without a temporary cache failed because `/home/rahul/.cache/go-build` is read only in this environment.

## Completed

### Reject malformed and oversized inserts before changing table state

- Priority: correctness and data integrity. `Table.Insert` previously advanced persisted ID metadata before validating value count, ignored `SetLastID` failures, and could allocate a page before discovering a row could never fit.
- Changes: validate value count and reject NUL bytes before reading or mutating storage; reject ID exhaustion and missing primary index; serialize once and reject rows larger than a page can hold; reserve the ID only after validation and propagate reservation failures; tombstone and flush a newly written row if primary-index insertion fails.
- Regression coverage: malformed, NUL-containing, and oversized inserts must leave the persisted ID and page count unchanged, and a subsequent valid insert must be retrievable by ID 0. A closed-index failure test verifies that an unindexed row stays invisible and the next successful insert uses the next ID. Test setup explicitly scopes `DATA_DIR` per test so tests do not reuse a deleted temporary directory.
- Verification on the final code: `GOCACHE=/tmp/goDB-gocache go test ./internal/table` passed; `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Limitations: table writes are not transactional. A failure after writing the row or index can still leave components out of sync; WAL/recovery is a separate foundational task.
- Commit: `66dfb66` (`Harden table insert integrity`).

### Preserve row identity during variable-length updates

- Priority: correctness. The previous implementation called `Insert` with a full row, which prepended a second ID and failed arity validation; it also extended the iterator as it appended rows.
- Changes: collect the matched rows before mutation; preflight replacement capacity; repack page payloads while preserving slot IDs; relocate rows that do not fit and update the existing primary-index RID. Oversized values and invalid update columns are rejected before writes.
- Regression coverage: grow and shrink a multi-row table through both in-page replacement and relocation; verify row count, stable IDs, ID-index lookups, and reopen behavior. Oversized update leaves the original row unchanged. Pager test verifies repack slot preservation and no page mutation on `ErrPageFull`.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./internal/pager ./internal/btree ./internal/table` passed; full `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Limitations: multi-row updates are preflighted for capacity and index locations, but are not crash-atomic if a later disk write fails. The engine still has no WAL or transaction recovery.

## Prioritized backlog

1. **Define delete/index behavior.** Tombstoning a row leaves its ID index entry pointing at a deleted slot, so indexed lookup can fail for a row that a table scan treats as deleted. Acceptance: selecting a deleted ID returns no row; index consistency checks distinguish valid live RIDs from tombstones; delete/reopen behavior is covered.
2. **Add crash-safe table mutations.** Insert/update/delete write table pages, metadata, and indexes separately, so a process or I/O failure between writes can leave inconsistent state. Depends on a WAL or another recovery design. Acceptance: injected failures at each write boundary recover to a consistent table/index state across reopen.
3. **Validate and harden B+ tree file metadata and duplicate-key semantics.** Index open currently trusts some root/page counts and the tests do not clearly specify duplicate handling. Acceptance: malformed/truncated metadata returns errors without panics; duplicate key behavior is explicit and tested.
4. **Add SQL/parser integration tests and usability documentation.** Parser/lexer lack test files, despite supporting CREATE/INSERT/SELECT/DELETE/UPDATE. Acceptance: representative valid and malformed statements are covered, including EOF and separator edge cases.
5. **Measure storage and index performance.** Existing insertion tests exercise large inputs but provide no benchmark baseline. Acceptance: add reproducible benchmarks for table insert/select and B+ tree insert/find before proposing optimization.

## Next action

Next task: fix deleted-row ID lookup semantics and add a consistency check for stale index RIDs. Preserve the supplied untracked `AGENTS.md`.
