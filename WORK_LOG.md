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
- Commit: `e8b5830` (`Preserve IDs in variable-length updates`).

### Treat deleted and missing ID lookups as empty results

- Priority: correctness. DELETE leaves the primary index pointing to a tombstoned slot, and `SELECT ... WHERE id=...` previously surfaced the tombstone as an invalid-slot error. Missing keys also returned a generic error rather than an empty result.
- Changes: pager reads now distinguish `ErrDeletedSlot` from `ErrInvalidSlot`; B+ tree lookups wrap `ErrKeyNotFound`; indexed table SELECT returns its header with no rows for a missing key or tombstone, while invalid slot RIDs still return an error.
- Regression coverage: select a deleted and absent ID and expect a header-only result; inject an invalid slot into the index and verify the corruption remains an error; B+ tree tests assert `errors.Is(..., ErrKeyNotFound)`.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./internal/pager ./internal/btree ./internal/table` passed; full `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Limitation: DELETE still leaves the stale primary-index entry on disk; SELECT handles that tombstone, and the new validator reports it until physical index deletion is implemented.
- Commit: `f30adae` (`Handle deleted row ID lookups`).

### Add a primary-index consistency validator

- Priority: data-integrity diagnostics. Structural B+ tree validation alone cannot prove that leaf RIDs match stored rows or that every live table row is indexed.
- Changes: `Table.ValidatePrimaryIndex` runs structural validation, walks the leaf chain, verifies each entry points to a live row with the matching ID, and checks every live row maps back to its exact RID. It reports duplicate keys, missing entries, bad locations, mismatched IDs, and tombstoned index entries.
- Regression coverage: validation passes on a populated table and after row relocation/reopen; corrupted RIDs and stale entries after DELETE are rejected.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./internal/table` passed; full `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Limitation: this adds diagnosis, not physical B+ tree deletion. A table with deleted rows is expected to fail this strict consistency check until index deletion is implemented.
- Commit: `e5794b1` (`Validate table primary index consistency`).

### Reject duplicate B+ tree keys before mutation

- Priority: correctness. The index stores one RID per key and `Find` returns one RID, so accepting duplicates makes rows unreachable through the index and violates primary-key behavior.
- Changes: `Index.Insert` now returns `ErrDuplicateKey` when the destination leaf already contains the key, before it shifts entries or splits a page.
- Regression coverage: insert 1,000 keys across leaf splits, reject a duplicate, verify the original RID and page count are unchanged, and validate the tree.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./internal/btree` passed; full `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Commit: `7d38e47` (`Reject duplicate B-tree keys`).

### Validate complete slotted pages when reading

- Priority: data integrity. `GetRow` checked the requested payload bounds but did not detect invalid deletion flags or overlapping payloads in other slots on the same page.
- Changes: `Page.Validate` checks every slot flag, active payload boundary, and active-range overlap. `Pager.GetPage` validates data pages and leaves metadata page 0 to its metadata validators. Row iteration caches the loaded page and slot count so whole-page checks run once per page.
- Regression coverage: invalid deletion flags, overlapping active slots, and corrupt pages loaded through the pager all return `ErrCorruptPageData`; valid page replacement, tombstones, and normal table iteration remain covered by the full suite.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./internal/pager ./internal/table` passed; full `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Commit: `aa582ca` (`Validate slotted pages on read`).

### Remove deleted row IDs from the primary B+ tree

- Priority: correctness. Table DELETE previously tombstoned rows but left their ID keys in the primary index, so validation reported stale entries and every delete depended on SELECT special-casing tombstones.
- Changes: added `Index.Delete` to remove a key from its sorted leaf and wired `Table.Delete` to remove each matched row's ID after persisting the tombstone. Internal separator keys remain as lower-bound fences; lookup of the deleted key returns `ErrKeyNotFound`. The structural validator now permits underfull and empty leaves because deletion does not yet merge/rebalance leaves; it continues to check page reachability, key order/ranges, parent links, leaf-chain order, and internal-node occupancy.
- Regression coverage: deleted an internal separator key and verified its neighbors and tree structure; deleted rows across a multi-leaf table and checked primary-index consistency; verified deleted and absent ID lookups return header-only results; closed/reopened a table after deletion and revalidated the index.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./...` passed; `GOCACHE=/tmp/goDB-gocache go build ./...` passed; `GOCACHE=/tmp/goDB-gocache go vet ./...` passed; `git diff --check` passed.
- Limitations: leaf underflow is allowed and merged pages are not reclaimed, so repeated deletions can leave sparse or empty leaves and reduce index efficiency. Table mutation is not crash-atomic: an I/O failure between the row tombstone and index write can leave the two structures inconsistent. WAL/recovery and leaf rebalancing remain separate tasks.
- Commit: `7d9c0d0` (`Remove deleted rows from primary index`).

### Rebalance B+ tree deletions and reuse merged pages

- Priority: storage correctness and sustained index efficiency. Leaf-only deletion left underfull leaves, weakened structural validation, and never reclaimed merged pages. Internal separator keys may also be stale lower-bound fences after deleting subtree minima, so rebalancing must derive new fences from actual subtree minima.
- Changes: leaf and internal underflow now borrow from siblings or merge, update parent separators and child parent IDs, collapse a one-child root, and add merged pages to a free list. The free-list head is stored in four unused metadata bytes immediately after the index name, preserving the existing metadata layout and reading as zero for old indexes. New page allocation consumes free pages before extending the file. `Validate` checks that every page is either reachable from the root or appears once on the free list, and strict leaf/internal minimum occupancy is restored. `SetNumPages` now updates in-memory state only after metadata is written.
- Regression coverage: a 50,000-key tree exercises multi-level internal borrowing/merging and root collapse while deleting from alternating extremes; validation runs throughout. The test empties the index, verifies free pages are reused without growing the page count, reinserts keys, reopens the index, and checks lookups and validation. Existing table tests validate multi-leaf DELETE consistency.
- Verification: baseline `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, and `go vet ./...` passed before edits. Final `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` passed.
- Limitations: rebalancing writes multiple pages and metadata without a WAL, so a process or I/O failure mid-delete can still leave a partially applied tree mutation. Crash-safe table/index mutations remain a separate task.
- Commit: `3b2b181` (`Rebalance B-tree deletions and reuse pages`).

### Reject malformed B+ tree metadata when opening indexes

- Priority: storage integrity. `OpenIndex` previously trusted root IDs, page counts, free-list heads, and name lengths; `GetPage` also allowed out-of-range IDs and multiplied offsets before widening them.
- Changes: opening now validates metadata bounds, root/free-page IDs, exact file/page-count agreement, root header type/parent/key count, and free-list-head type. `GetPage` and `WritePage` reject page IDs outside the allocated range and calculate offsets in `int64`. Legacy indexes with zeroed unused free-list bytes remain supported. `Table.Open` now surfaces a malformed primary `id` index rather than silently returning a table with no primary index.
- Regression coverage: malformed/truncated metadata, misaligned files, page-count mismatch, invalid root IDs/types/parents/counts, and bad free-list heads are rejected; a legacy no-free-list index opens and remains usable. Table reopen reports corrupt primary-index errors.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` passed.
- Limitations: open-time checks validate the root and free-list head but do not recursively validate the whole tree, avoiding a full index scan on every open. Call `Validate` for a complete structural audit.
- Commit: `0fff3fd` (`Validate B-tree metadata when opening indexes`).

### Make B+ tree validation safe across concurrent indexes

- Priority: verification reliability. `Validate` stored traversal levels in a package-global slice, so independent indexes validating at the same time could overwrite each other's state and race.
- Changes: each `Validate` call now allocates its own visited-page state and passes it through recursive node validation. Other read-only traversal state was already local to the call.
- Regression coverage: four indexes with different page counts and free-list shapes are validated concurrently and repeatedly. The targeted test passed with the race detector.
- Verification: `GOCACHE=/tmp/goDB-gocache go test -race ./internal/btree -run TestValidateConcurrentIndexes -count=1` passed; full `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` passed.
- Limitations: this makes concurrent read-only validation safe across independent indexes. Concurrent mutation of an index while validating it is still unsupported and unsynchronized.
- Commit: `e7b003a` (`Make B-tree validation state local`).

### Add undo logging for table mutations

- Priority: crash consistency and data integrity. Inserts, updates, and deletes change the table file and primary B+ tree in separate page writes; a process interruption could previously leave a live row without an index entry, a tombstone with a stale index entry, or partially rebalanced index pages.
- Changes: added a checksummed undo journal with durable page before-images, original file-size records for newly appended pages, commit records, and recovery before `Table.Open`. Pager and B+ tree writes enlist in the same journal. `Table.Insert`, `Update`, and `Delete` now serialize their mutation callbacks, commit only after syncing touched data files, and undo/reopen the table when an operation fails. A failed rollback poisons that in-memory table handle so later writes require reopening. Journal creation does not overwrite an existing log, and recovery validates records and limits target files to the journal directory.
- Regression coverage: WAL tests restore multiple page images and truncate newly appended data after simulated process exit; committed changes survive; bad checksums and out-of-directory recovery targets are rejected without changing data. Table tests simulate an uncommitted table-metadata plus index change and confirm `Open` restores both, and force insert/delete index failures to verify the row/index state rolls back.
- Verification: baseline `GOCACHE=/tmp/goDB-gocache go test ./...` passed before implementation. Final `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, `go test -race ./internal/wal ./internal/table`, and `git diff --check` passed.
- Limitations: journaling covers mutations to existing tables; initial table/index creation and schema changes are not transactional. Concurrency is serialized only within one `Table` handle, not across multiple handles or processes. Tests exercise representative failures and crash recovery, not injected faults at every individual write/fsync boundary. Recovery intentionally leaves malformed or out-of-directory journals untouched and reports an error.
- Commit: `a306ea7` (`Add undo journal for table mutations`).

### Tighten SQL syntax parsing and document the supported subset

- Priority: user-facing correctness and regression coverage. The parser previously skipped repeated/missing list commas, accepted empty or repeated `AND` separators, and ignored tokens after `CREATE TABLE`, allowing malformed input to be interpreted as a valid statement.
- Changes: SELECT and INSERT lists now require comma-separated values; CREATE columns and UPDATE assignments require comma separation; WHERE filters require one or more predicates joined by single `AND` tokens; CREATE rejects trailing tokens. Added lexer and parser test suites. README now documents the actual one-line SQL subset, no-semicolon behavior, string quoting limitation, all-row UPDATE/DELETE behavior, and mutation recovery scope; obsolete claims that deletion and recovery are absent were removed.
- Regression coverage: lexer keyword normalization, punctuation, quoted strings, illegal characters, and EOF; parser output for each supported statement and rejection of malformed separators, empty predicates, and trailing tokens.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` passed.
- Limitations: the lexer still supports only the current small grammar; no escaping inside quoted strings, semicolons, negative numeric literals, expressions, or typed columns were added.
- Commit: `3a9af85` (`Validate SQL list and filter syntax`).

### Add storage benchmarks and honor `DATA_DIR` without dotenv

- Priority: measurable performance and usable storage configuration. The repository had large correctness tests but no benchmarks for index/table operations. The first table benchmark attempt also exposed that `tablePath` ignored an already-set `DATA_DIR` when no `.env` file was present, unlike the B+ tree path handling.
- Changes: added B+ tree insert/find and table insert/indexed-select benchmarks with temporary storage directories and allocation reporting; table insertion reports allocated page count. `tablePath` now loads dotenv opportunistically, honors the process environment directly, and uses `filepath.Join`. Added a path regression test and documented the benchmark command and journal-sync scope in README.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` passed. Benchmarks ran with `-benchtime=2s -benchmem` on Linux/amd64, Intel Core i5-13500H: index insert 8.67 µs/op (2,696 pages at end), index find 4.69 µs/op, table insert 96.96 µs/op (including journal sync; 100 pages at end), and indexed table select 11.65 µs/op. These are local reference numbers, not portable performance claims.
- Limitations: results include filesystem and cache effects and only measure one local machine. The index insert benchmark grows one tree during its timed run; find uses a fixed 10,000-key tree; point select uses 100 preloaded rows. No optimization is proposed from this initial baseline.
- Commit: `65abef1` (`Add storage performance benchmarks`).

## Prioritized backlog

1. **Expand WAL fault-injection coverage and multi-handle safety.** The undo journal currently has representative rollback and crash tests but not an injected failure at every journal/data write and sync boundary; separate `Table` handles can also mutate the same files without a shared lock. This matters because recovery ordering is central to storage integrity. Dependencies: add injectable file operations or a narrow test seam without changing production semantics; decide whether to lock per canonical table path or explicitly reject multiple writers. Risks: a faulty test seam could hide real I/O behavior, and cross-process locking is platform-dependent. Acceptance: deterministic failures before/after every before-image, data-page, data-sync, commit-record, and journal-removal boundary either recover to the last committed state or return an explicit poisoned/recovery-required error; independent handles cannot silently interleave writes. Tests cover insert/update/delete and B+ tree root collapse/merge, then reopen and validate primary-index consistency.
2. **Make table creation crash-atomic.** `Table.Create` creates/truncates a table file and separately creates its primary index; interruption can leave an incomplete pair. This matters because a failed DDL operation can make a table unopenable or destroy prior data. Dependencies: define existing-name behavior and a temp-file/rename protocol compatible with current paths. Risks: atomic rename guarantees vary by filesystem, and replacement must not destroy existing user data. Acceptance: create either publishes a complete table/index pair or leaves prior files intact; reopening after simulated interruption reports a clear absent/complete state. Tests inject failures between metadata, index creation, and publication.
3. **Add CLI integration coverage.** Parser and table layers have unit coverage, but the scanner-to-parser-to-storage path has no integration tests. This matters because statement dispatch and column resolution can diverge from documented grammar. Dependencies: isolate `DATA_DIR` and test the command loop without coupling tests to global stdin/stdout where possible. Risks: refactoring CLI code solely for tests could add unnecessary abstraction. Acceptance: representative create/insert/select/update/delete statements are executed through dispatch helpers and verify output/errors and persisted state.

## Next action

Next task: add a narrow injectable I/O seam to exercise undo-journal failure ordering, starting with failures during table-page and index-page writes; verify rollback/reopen consistency before considering cross-handle locking. Preserve the supplied untracked `AGENTS.md`.
