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
- Regression coverage: WAL tests restore multiple page images and truncate newly appended data after simulated process exit; committed changes survive; bad checksums and out-of-directory recovery targets are rejected without changing data. Table tests simulate an uncommitted table-metadata plus index change and confirm `Open` restores both. Closed-index checks reject mutations before table changes; the torn-commit follow-up tests interrupted commit recovery.
- Verification: baseline `GOCACHE=/tmp/goDB-gocache go test ./...` passed before implementation. Final `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, `go test -race ./internal/wal ./internal/table`, and `git diff --check` passed.
- Limitations: journaling covers mutations to existing tables; initial table/index creation and schema changes are not transactional. At this checkpoint, concurrency was serialized only within one `Table` handle, not across multiple handles or processes. Tests exercise representative failures and crash recovery, not injected faults at every individual write/fsync boundary. Recovery intentionally leaves malformed or out-of-directory journals untouched and reports an error.
- Commit: `a306ea7` (`Add undo journal for table mutations`).
- Follow-up recovery test: `8ee6105` (`Test recovery from torn WAL commit`) confirms that a commit frame missing its final byte is treated as uncommitted and restores the original page. `go test ./internal/wal ./internal/table`, full `go test ./...`, and `git diff --check` passed afterward.

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

### Serialize recovery and writes across table handles

- Priority: storage correctness. A second `Table` handle could call `wal.Recover` while another handle was writing an active undo journal, making a live transaction look abandoned. Independent handles also held stale pager page counts and B+ tree root/free-list metadata after another writer committed.
- Changes: added a per-table advisory file lock on Unix (process-local keyed mutex fallback elsewhere). `Create`, `Open` recovery/open, and each mutation hold the same lock. Mutations refresh pager page count, last ID, and index metadata after acquiring the lock and before starting their journal. Added `Pager.Refresh` and `Index.Refresh`; rollback reopens files under the already-held lock without recursively acquiring it. README states the locking scope and lack of read snapshot isolation.
- Regression coverage: two separately opened table handles perform concurrent inserts; after close/reopen, all 100 rows remain live and the primary index validates. Existing recovery and mutation tests also pass with lock acquisition in place.
- Verification: baseline `GOCACHE=/tmp/goDB-gocache go test ./...` and `go build ./...` passed before changes. Final `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, `go test -race ./internal/table ./internal/filelock`, `GOOS=windows GOARCH=amd64 go build ./...`, and `git diff --check` passed.
- Limitations: Unix advisory locks coordinate cooperating processes; non-Unix platforms currently only serialize within the process. Reads are not isolated from concurrent writes. Initial table creation is protected against concurrent open/mutation but is not crash-atomic. WAL failures at every syscall boundary still lack injection coverage.
- Commit: `aa1ab94` (`Serialize mutations across table handles`).

### Inject failures at WAL ordering boundaries

- Priority: crash consistency verification. Representative crash tests did not force failures around the journal's before-image, data-sync, and commit-record boundaries, leaving key rollback ordering assumptions unverified.
- Changes: added package-local journal failpoints for file-start/page-before/commit record appends and data-file sync. Production callers cannot configure these hooks; normal journal behavior keeps the hook nil.
- Regression coverage: an injected page-before record failure leaves data unchanged; injected data-sync and commit-record failures after changing a page return errors, and explicit rollback restores the original page bytes.
- Verification: targeted WAL/table tests and full `go test ./...` passed; `go build ./...`, `go vet ./...`, full `go test -race ./...`, `GOOS=windows GOARCH=amd64 go build ./...`, and `git diff --check` passed.
- Limitations: the new hooks exercise journal record and sync ordering, but do not yet inject low-level table/index page write errors through the full `Table` mutation wrapper. Torn final commit framing is covered separately by `8ee6105`.
- Commit: `9e27e0f` (`Inject WAL ordering failures in tests`).
- Follow-up table integration: `794335f` (`Test rollback after partial table and index writes`) forces an error after the mutation wrapper has written both table metadata and an index leaf, then verifies restored ID/index state and full primary-index consistency. Targeted table/WAL tests, full `go test ./...`, `go build ./...`, `go vet ./...`, and `go test -race ./internal/table ./internal/wal` passed.
- Follow-up page-write coverage: `49093cc` adds package-local partial-write injection for `Pager.Flush` and `Index.WritePage`, then verifies journal rollback restores exact original page bytes. It also fixes `Index.WritePage` to return `io.ErrShortWrite` when `WriteAt` reports a short successful write. Full `go test ./...`, `go build ./...`, `go vet ./...`, and race tests for pager/B+ tree/table/WAL passed. A concurrent verification run once caused the fixed-filename B+ tree tests from two test processes to collide; rerunning `go test ./...` alone passed.
- Follow-up allocation/rebalance coverage: `8013d07` injects a partial table-file append from `Pager.NewPage`, an index-file append after metadata page-count growth, and a short write on the second page of a leaf borrow. Rollback restores the original file length/page count, leaf keys, and tree validity. Full `go test ./...`, `go build ./...`, `go vet ./...`, Windows cross-build, and `git diff --check` passed.
- Follow-up recovery/root-collapse coverage: `b999f00` interrupts recovery after a partial page restore and before file truncation; a second recovery restores bytes and removes the journal. It also injects a short write during root metadata update after leaf merge/root collapse has begun, then verifies the old root and deleted-key state are restored and the tree validates. Full tests/build/vet and race tests for WAL/pager/B+ tree passed.

### Verify process locks and internal-node merge rollback

- Priority: storage reliability. The writer lock had only been exercised with separate handles in one process, and rollback tests had not injected a failure while moving child pages during internal-node merge.
- Changes: added a Unix child-process test that holds the table lock while the parent proves a second acquisition blocks, then releases it and verifies acquisition proceeds. Added a deterministic 3-level B+ tree fixture where a partial write during internal-node merge follows a leaf merge and root-collapse preparation; the journal must restore child parents, page count, separators, and root metadata.
- Regression coverage: after retrying the lock, the child exits cleanly; after the injected internal merge error, `Index.Validate`, root checks, and lookup of the rolled-back key all pass.
- Verification: full `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, Windows cross-build, and `git diff --check` passed. `go test -race ./internal/filelock ./internal/btree ./internal/wal` passed.
- Limitations: cross-process locking was runtime-tested on Linux; Windows was cross-built and uses only process-local locking. Table reads still do not acquire shared locks or provide snapshots.
- Commit: `8825454` (`Test process lock and internal merge rollback`).

### Make table creation recoverable and non-destructive

- Priority: storage safety. `Table.Create` previously truncated a same-name table file and created its index separately; a duplicate create could destroy data, and a crash between files could leave an incomplete table/index pair.
- Changes: creation now rejects an existing table or primary-index file with `ErrTableExists`, stages both files in the target directory, syncs them, publishes a durable creation marker, renames both files while holding the table lock, then removes and syncs the marker. `Open` and `Create` recover an interrupted marker by removing only the incomplete pair. B+ tree creation gained `IndexPath`/`NewIndexAt` for staging with stable logical metadata. Default table/index data paths are now consistently relative to the process working directory. Index initial writes close descriptors and report short writes.
- Regression coverage: duplicate create leaves existing table and index bytes unchanged; simulated failures after marker creation, table publication, and index publication clean up all artifacts and permit a later successful create; `Open` cleans a simulated interrupted marker/pair before reporting the table absent.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, `GOOS=windows GOARCH=amd64 go build ./...`, `go test -race ./internal/table ./internal/btree`, and `git diff --check` passed.
- Limitations: recovery of interrupted creation intentionally leaves the name absent for a later retry; it does not replace an existing table. The guarantee depends on filesystem rename and directory-sync behavior; runtime behavior was exercised on Linux, while Windows was cross-built only.
- Commit: `cc55747` (`Make table creation recoverable and non-destructive`).

## Prioritized backlog

1. **Add CLI integration coverage.** Parser and table layers have unit coverage, but the scanner-to-parser-to-storage path has no integration tests. This matters because statement dispatch and column resolution can diverge from documented grammar or fail to close/reopen storage correctly. Dependencies: isolate `DATA_DIR` and refactor command handling into a callable function only if that materially improves testability. Risks: avoid turning the CLI into an unnecessary framework or coupling output tests to prompts. Acceptance: representative create/insert/select/update/delete statements execute through the command path and verify output/errors plus persisted state after reopen.
2. **Assess secondary-index support.** Current table creation only builds the primary `id` index and non-ID filters use scans. This matters for lookup performance, but key encoding and maintenance must be designed before adding indexes. Dependencies: define supported value types and uniqueness semantics, then update/delete/relocation hooks. Risks: current B+ tree keys are `uint64`, arbitrary strings need a stable ordered encoding, and extra indexes multiply WAL writes. Acceptance: either document scans as the current behavior or implement typed secondary indexes with WAL-backed consistency tests and query-selection coverage.
3. **Exercise filesystem durability boundaries on each supported OS.** Table creation and WAL startup depend on syncing containing directories; Unix uses advisory `flock`, other platforms use a process-local fallback. This matters because build success does not prove runtime durability behavior. Dependencies: platform-specific test environments. Risks: directory sync and lock APIs differ. Acceptance: document supported durability guarantees per OS and run create/recovery tests on each supported platform, or fail explicitly where required primitives are unavailable.

## Next action

Next task: add CLI integration coverage for the documented statement subset, then reassess secondary-index and cross-platform durability priorities. Preserve the supplied untracked `AGENTS.md`.
