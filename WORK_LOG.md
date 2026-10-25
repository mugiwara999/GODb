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

## Prioritized backlog

1. **Make table insert/update/delete atomic across row pages, ID metadata, and indexes.** Failure between the component writes can leave unreachable rows, stale RIDs, or reused IDs. Depends on a transaction or recovery design. Acceptance: injected I/O failures at each write boundary recover to a consistent table/index state; cover reopen/restart behavior.
2. **Correct variable-length updates.** `Update` currently deletes the old slot and calls `Insert` with the full row (including ID), while the insert API prepends a new ID; its TODO also identifies iterator mutation when appended rows are on a page being scanned. Acceptance: longer and shorter replacement values update once, preserve row identity, and remain selectable through the ID index; verify across reopen.
3. **Validate and harden B+ tree file metadata and duplicate-key semantics.** Index open currently trusts root/page counts and the tests do not clearly specify duplicate handling. Acceptance: malformed/truncated metadata returns errors without panics; duplicate key behavior is explicit and tested.
4. **Define delete/index behavior.** Tombstoning a row leaves its ID index entry pointing at a deleted slot. Acceptance: selecting a deleted ID returns no row, and index validation can identify stale entries or maintain a valid mapping.
5. **Add SQL/parser integration tests and usability documentation.** Parser/lexer lack test files, despite supporting CREATE/INSERT/SELECT/DELETE/UPDATE. Acceptance: representative valid and malformed statements are covered, including EOF and separator edge cases.

## Next action

Commit the verified insert integrity fix on `agent` without staging the supplied `AGENTS.md`; record the resulting commit and continue with the next prioritized storage correctness task.
