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

### Exercise the SQL command loop end to end

- Priority: integration coverage. Parser and table tests did not exercise the scanner-to-parser-to-storage path, leaving statement dispatch and column resolution unverified as a unit.
- Changes: extracted `run(io.Reader, io.Writer)` from `main` so the command loop can be driven and its output captured in tests. Removed an index-open debug print that leaked into user-visible CLI output.
- Regression coverage: a command-loop test creates a table, inserts and selects a row, updates it, deletes it, checks missing-column and malformed-SQL errors, then reopens the table and validates persisted row and primary-index state.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./cmd ./internal/btree`, full `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOCACHE=/tmp/goDB-gocache go build ./...`, `GOCACHE=/tmp/goDB-gocache go vet ./...`, and `git diff --check` passed.
- Limitations: the CLI is still a single-line, prompt-oriented interface; this test covers the documented statement subset rather than interactive terminal behavior or multi-line input.
- Commit: `b4a568d` (`Test SQL command loop integration`).

### Reject malformed row payloads at table boundaries

- Priority: storage correctness. Slotted-page validation verifies byte ranges, but table decoding silently ignored trailing bytes without a NUL terminator. SELECT and DELETE also accepted extra serialized fields beyond the table schema.
- Assessment: secondary indexes are premature with the current model. Columns have no types and every value is an arbitrary string; the B+ tree accepts only unique `uint64` keys and stores one RID per key, so repeated string or numeric values cannot be indexed correctly. Typed columns and a duplicate-key or key-to-RID-list design need to precede a secondary-index feature.
- Changes: added strict table-row decoding that requires a final field terminator and exactly the declared number of columns. SELECT (including primary-key lookup), DELETE, UPDATE, and primary-index consistency validation now share this check.
- Regression coverage: malformed persisted payloads with unterminated trailing bytes and with extra terminated fields are rejected by table scans, indexed SELECT, DELETE, UPDATE, and `ValidatePrimaryIndex`.
- Verification: targeted `GOCACHE=/tmp/goDB-gocache go test ./internal/table` and full `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOCACHE=/tmp/goDB-gocache go build ./...`, `GOCACHE=/tmp/goDB-gocache go vet ./...`, and `git diff --check`, `GOCACHE=/tmp/goDB-gocache go test -race ./internal/table`, and `GOOS=windows GOARCH=amd64 GOCACHE=/tmp/goDB-gocache go build ./...` passed.
- Limitations: `DeserializeRow` remains a permissive low-level helper for compatibility; table operations use the strict schema-aware decoder.
- Commit: `ffe0b44` (`Reject malformed serialized table rows`).

### Restore Solaris build coverage for process-local locks

- Priority: portability and platform correctness. `lock_unix.go` excludes Solaris, while `lock_other.go` also excluded it, leaving the package with no buildable implementation on Solaris.
- Changes: made the fallback lock build constraint the complement of the supported `flock` targets, so Solaris and other remaining platforms use the documented process-local mutex. Added fallback-lock serialization coverage and documented the exact advisory-lock targets plus runtime durability verification scope in README.
- Regression coverage: fallback lock test verifies a second same-process acquisition blocks until the first lock closes. Linux tests pass; Solaris full build and filelock test binary cross-compile; Windows full build passes.
- Verification: `GOCACHE=/tmp/goDB-gocache go test ./internal/filelock`, full `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOOS=solaris GOARCH=amd64 GOCACHE=/tmp/goDB-gocache go build ./...`, `GOOS=solaris GOARCH=amd64 GOCACHE=/tmp/goDB-gocache go test -c -o /tmp/goDB-filelock-solaris.test ./internal/filelock`, `GOOS=windows GOARCH=amd64 GOCACHE=/tmp/goDB-gocache go build ./...`, cross-builds for Darwin, FreeBSD, DragonFly, NetBSD, OpenBSD, AIX, and Plan 9, and `git diff --check` passed.
- Limitations: runtime tests were available only on Linux. Cross-compilation proves the lock implementations build across the tested targets but does not validate directory-sync behavior or crash durability outside Linux.
- Commit: `8983652` (`Restore Solaris build using fallback locks`).

### Validate and canonicalize table schemas

- Priority: schema correctness and safe storage paths. The storage API accepted duplicate names, the generated `id` name as a user column, SQL keywords, malformed identifiers, and embedded NULs. Duplicate names made column lookup inconsistent, while invalid table names could produce paths the SQL interface could not address.
- Changes: `Create` and `Open` now accept only identifier tokens supported by the SQL lexer and normalize them to lowercase. Creation rejects duplicate columns (case-insensitively) and reserves `id` for the generated primary key before acquiring locks or creating files. `Open` validates stored schema metadata, normalizes legacy mixed-case columns in memory, and rejects duplicate or invalid persisted columns. README documents these constraints.
- Regression coverage: invalid schemas including duplicate names, `id`, reserved words, whitespace, and NUL are rejected without leaving files; invalid/path-traversal table names are rejected by Create and Open; mixed-case table/column creation persists canonical lowercase metadata and reopens through a different table-name case; on-disk mixed-case metadata is normalized and duplicate primary-key metadata is rejected as corrupt.
- Verification: targeted `GOCACHE=/tmp/goDB-gocache go test ./internal/table ./cmd`, full `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOCACHE=/tmp/goDB-gocache go build ./...`, `GOCACHE=/tmp/goDB-gocache go vet ./...`, `GOCACHE=/tmp/goDB-gocache go test -race ./internal/table`, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: legacy mixed-case metadata is normalized only in memory; older tables containing duplicate, reserved, or otherwise unaddressable columns are rejected and require manual repair.
- Commits: `e21b439` (`Validate SQL table and column identifiers`) and `73ec01f` (persisted-schema validation follow-up).

### Remove outdated row-mutation notes

- Priority: maintainability. A comment still described update relocation as an unresolved iterator-mutation bug even though `Update` now gathers matching rows before applying changes. A second commented-out block suggested index maintenance that could not run with the current schema/index model.
- Changes: removed the stale `Update` TODO and abandoned secondary-index stub; added a short explanation beside the existing two-pass scan.
- Regression coverage: the existing row-relocation update test covers the behavior described by the removed TODO.
- Verification: full `GOCACHE=/tmp/goDB-gocache go test ./...` and `git diff --check` passed.
- Commit: `9799c27` (`Remove obsolete row mutation TODOs`).

### Make pager page I/O safe for concurrent offsets

- Priority: read correctness and storage API robustness. `ReadPage` and `WritePage` used `Seek` plus sequential I/O on a shared descriptor; concurrent calls could move the descriptor between operations and read or write the wrong page. Large page IDs could also overflow the signed file offset and wrap to an unrelated location.
- Changes: switched page reads and writes to offset-based `ReadAt`/`WriteAt` and added a checked page-ID-to-offset conversion that rejects negative and overflowing IDs.
- Regression coverage: concurrent readers verify every page boundary remains associated with the requested page; concurrent writers update distinct pages and verify the final page contents; maximum-int page IDs are rejected without changing file size.
- Verification: pager concurrency/overflow tests passed 20 repeated runs; `GOCACHE=/tmp/goDB-gocache go test ./internal/pager -run '^$' -fuzz FuzzPageAddRowRoundTrip -fuzztime=8s` passed after 1,007,268 executions; `GOCACHE=/tmp/goDB-gocache go test -race ./internal/pager ./internal/table`, full `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOCACHE=/tmp/goDB-gocache go build ./...`, `GOCACHE=/tmp/goDB-gocache go vet ./...`, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: offset-safe I/O does not provide snapshot isolation if a writer changes a page while a reader is reading it; the existing README limitation remains.
- Commit: `69688b0` (`Use offset-based pager page I/O`).

### Bound persisted column counts before allocation

- Priority: corruption resilience. `Pager.GetColumns` converted the persisted 32-bit column count to `int` and allocated a slice before proving the metadata page could contain that many names. A corrupt count could cause excessive allocation or process termination instead of a metadata error.
- Changes: `GetNumCols` now rejects zero columns and counts larger than the metadata page can minimally encode before converting or allocating.
- Regression coverage: corrupt the persisted count to `math.MaxUint32` and verify both `GetNumCols` and `GetColumns` return `ErrMetadataCorrupt`.
- Verification: targeted pager metadata corruption tests, full `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOCACHE=/tmp/goDB-gocache go build ./...`, `GOCACHE=/tmp/goDB-gocache go vet ./...`, `GOCACHE=/tmp/goDB-gocache go test -race ./internal/pager ./internal/table`, and `git diff --check` passed.
- Limitations: the bound uses the current v1 layout where names start at `ColNamesOffset`; a versioned schema layout must apply its own bound accounting for type metadata.
- Commit: `a662200` (`Bound metadata column counts before allocation`).

### Add typed columns with backward-compatible metadata

- Priority: foundational schema support. Untyped text-only columns blocked validation of numeric values and any reliable numeric index semantics. Existing metadata version 1 had no type tags, so typed tables needed a separate version without making old tables unreadable.
- Changes: added metadata v2 with one type tag per column, while v1 reads infer the generated `id` as `UINT64` and user columns as `TEXT`. Added `TEXT` and `UINT64` table columns through `CreateWithTypes` and SQL declarations; typed unsigned values are canonicalized to decimal on write and validated in filters, updates, reads, and primary-index validation. The CLI now accepts declarations such as `CREATE TABLE people (age UINT64, name TEXT)`. Untyped Create and SQL remain v1-compatible text schemas.
- Regression coverage: v1 and v2 metadata round trips (including case-insensitive inference of a legacy `ID`), invalid type tags/types, v2 ID type enforcement, typed insert/update/filter validation, numeric canonicalization, persisted reopen, malformed typed-row detection, SQL parser declarations, and CLI end-to-end typed CRUD.
- Verification: initial targeted testing caught an out-of-memory regression because the new `GetSchema` path bypassed the bounded column-count accessor; it was corrected to use `GetNumCols`. Final `GOCACHE=/tmp/goDB-gocache go test ./...`, `GOCACHE=/tmp/goDB-gocache go build ./...`, `GOCACHE=/tmp/goDB-gocache go vet ./...`, race tests for pager/table/parser/cmd, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: only `TEXT` and `UINT64` are supported; typed columns are not yet indexed, and v1 inference assumes the first stored column is the generated `id`.
- Commit: `f46b882` (`Add backward-compatible typed columns`).

### Add duplicate-key posting lists to the B+ tree

- Priority: index correctness foundation. Secondary indexes need to map repeated typed values to multiple row locations; changing leaf/internal separators to duplicate keys would weaken routing and range invariants. Posting pages preserve one tree entry per distinct key.
- Design and changes: added a posting-page type with packed RIDs and a linked-page pointer. A leaf RID with reserved slot ID `0xffff` points to the chain; ordinary insert/update/delete APIs reject or handle that marker explicitly. Added `InsertDuplicate`, `FindAll`, and `DeleteRID`; duplicate pages use the existing allocator/free list, while validation checks page ownership, chain bounds/cycles, counts, and duplicate RIDs. Existing unique leaf and internal page layouts remain unchanged.
- Regression coverage: duplicate chains spanning multiple pages, duplicate RID rejection, unique-insert duplicate rejection, reopen, `FindAll`, shrink-to-one and delete-to-zero, free-page reuse, whole-key deletion, corrupt-cycle detection, validation, and WAL rollback after a partial leaf write.
- Verification: targeted posting-chain tests; full `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, race tests for B+ tree/WAL/table, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: APIs are currently B+ tree-level only; table secondary-index creation and mutation maintenance are not integrated. The reserved marker relies on the table pager's practical slot-count bound; callers cannot use slot ID `0xffff` in the B+ tree API. TEXT keys still need stable encoding.
- Commit: `5b3cfce` (`Add duplicate key posting lists`).

### Integrate typed UINT64 secondary indexes with tables

- Priority: make the B+ tree posting API usable by table operations. Until table-level creation and maintenance existed, secondary indexes could become stale as rows changed and equality filters could not use them.
- Changes: added `Table.CreateIndex` for typed `UINT64` columns. It locks the table, refreshes current files, backfills a staging index, syncs it, then atomically publishes and opens it. Existing handles discover newly published indexes before their next mutation. Insert, update, row relocation, and delete now maintain exact key/RID associations under the table's shared WAL. Indexed equality filters use `FindAll` and validate each pointed row before returning it. `ValidateIndexes` compares every open secondary index to live rows; `Open` now surfaces malformed present secondary index files instead of silently skipping them. README documents the Go API and its current SQL limitation.
- Regression coverage: backfill from populated tables; duplicate-value equality lookup; index create rejection for text, unknown, primary, and repeated columns; index refresh by a handle opened before creation; update/delete/reopen; row relocation; failed backfill staging cleanup; rejection of a corrupt present index on open; rollback of a mutation when a secondary posting chain is corrupt; index consistency validation.
- Verification: targeted table tests; full `GOCACHE=/tmp/goDB-gocache go test ./...`, `go build ./...`, `go vet ./...`, race tests for B+ tree/WAL/table, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: index definitions are represented by the presence of index files, not a catalog; a missing optional index falls back to table scans. `CREATE INDEX` is currently a Go API, not SQL. Only `UINT64` indexes are supported. A crash during backfill may leave a private `.build-tmp` file; it is ignored and replaced on a later index-creation attempt.
- Commit: `029a14f` (`Integrate typed secondary indexes`).

### Add SQL syntax for typed secondary-index creation

- Priority: usability. The table API could create and maintain indexes, but CLI users had no route to request one.
- Changes: parser and command loop now accept `CREATE INDEX ON table(column)` and delegate to `Table.CreateIndex`. The syntax omits a user-provided index name because the current storage identity is derived from the table and column. README examples now use a typed UINT64 column for index creation and describe the current limit.
- Regression coverage: parser accepts the statement and rejects missing columns, extra indexed columns, misplaced names, and trailing tokens; CLI test creates the index over duplicate values then runs indexed equality queries through subsequent updates and reopen.
- Verification: targeted parser/CLI tests, full suite/build/vet, table/B+ tree/WAL race tests, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: index files are still the only record that a secondary index exists; missing files cause scan fallback. Durable index catalog semantics remain the next task.
- Commit: `c1c689e` (`Add SQL secondary index creation`).

### Persist secondary-index declarations in metadata v3

- Priority: index lifecycle correctness. File presence alone could not distinguish an intentionally unindexed column from a lost or corrupt declared index. That made `Open` silently drop an index declaration and fall back to scans.
- Design and changes: added metadata version 3 with a packed bitset after the typed-column tags. `GetSchema` validates flag bounds, unused bits, ID exclusion, and UINT64-only declarations. `SetIndexCatalog` writes the complete catalog in one metadata-page update; the table WAL covers upgrades and flag changes. Index creation initializes/upgrades the catalog before staging, publishes a synced complete index, then commits the declared bit. Open fails when a cataloged index is absent or malformed; unlisted orphan files are ignored and removed if that index is recreated. Legacy v1/v2 metadata remains readable; v2 indexes discovered during migration are cataloged together.
- Regression coverage: v2-to-v3 round trip and WAL rollback; invalid ID/TEXT catalog flags; missing declared index rejection; orphan ignore/replacement; migration preserving a preexisting v2 secondary index; corrupt present index rejection; CLI and table secondary-index flows.
- Verification: full `GOCACHE=/tmp/goDB-gocache go test ./...`, build, vet, race tests for pager/B+ tree/WAL/table, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: the catalog has no `DROP INDEX` operation yet. If a process crashes before catalog commit, the complete published index remains an unlisted orphan and is ignored; recreation removes it safely. Native Windows/Solaris durability remains unverified.
- Commit: `5d0e704` (`Persist secondary index catalog`).

### Add safe secondary-index removal and refresh reads

- Priority: complete index lifecycle without leaving old table handles on stale index state. Removing a catalog bit/file is unsafe while an older handle can still query through its cached index; its later mutations could stop maintaining that cache.
- Changes: added `Table.DropIndex` and SQL `DROP INDEX ON table(column)`. Drop clears the catalog flag through the table WAL, closes the local handle, unlinks the file, and syncs the directory; a crash after catalog commit leaves an ignored orphan. `SELECT` now takes the table writer lock, recovers and refreshes table/index metadata before reading. Existing handles therefore discard dropped indexes before their next query or mutation. Reads from cooperating table handles now serialize with writes.
- Regression coverage: create/drop/reopen and scan fallback; old handle reads and writes after drop; primary and absent-index rejection; parser/CLI DDL; existing closed-index recovery behavior; catalog and table consistency.
- Verification: full `GOCACHE=/tmp/goDB-gocache go test ./...`, build, vet, race tests for pager/B+ tree/WAL/table/parser/CLI, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: SELECT uses an exclusive table lock, so readers serialize with each other as well as writers. Direct pager access and `ValidateIndexes` do not acquire that lock. Native Windows/Solaris runtime behavior remains unverified.
- Commit: `c1bf366` (`Add SQL secondary index removal`).

### Inject index-removal filesystem failures

- Priority: storage lifecycle verification. The catalog-first drop order was structurally safe, but unlink and directory-sync errors had no deterministic regression coverage.
- Changes: routed index removal through an internal filesystem-operation seam and added tests for unlink failure and post-unlink directory-sync failure. The tests confirm that reopen ignores the unlisted orphan or missing file and scans correctly; index recreation also replaces an orphan safely.
- Environment assessment: Docker is available only for Linux (`docker info` reports `linux x86_64`); no Windows/Solaris images are present. QEMU is installed but no guest images or boot setup are available. Native runtime testing on those targets therefore remains unavailable here.
- Verification: targeted failure-injection tests, full `GOCACHE=/tmp/goDB-gocache go test ./...`, build, vet, race tests for table/WAL/B+ tree/pager, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: injected tests verify application recovery semantics on Linux, not each target filesystem's actual unlink and directory-sync guarantees.
- Commit: `2bc9cd0` (`Test secondary index removal failures`).

### Benchmark and cache secondary-index reads

- Priority: query performance. No table-level secondary-index benchmarks existed, and indexed lookup loaded and validated a table page separately for every RID in a posting list.
- Changes: added indexed-versus-scan benchmarks for single-match and 512-match posting chains, plus parallel-reader benchmarks. Indexed SELECT now caches each touched table page for the duration of the query; the table lock keeps that image stable while the cache is used.
- Benchmark evidence on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=100ms -cpu=1,4`: the 512-match indexed case improved from 6.46 ms and 9.23 MB/op to 0.42 ms and 279 KB/op. The single-match case measured 46 µs / 83 KB versus a table scan at 0.83 ms / 320 KB. Four-CPU parallel indexed reads measured 108 µs/op versus 57 µs/op at one CPU, so contention remains measurable; these short local runs are directional rather than stable performance claims.
- Verification: full tests, build, vet, race tests for table/WAL/B+ tree/pager, Windows and Solaris cross-builds, targeted benchmarks, and `git diff --check` passed.
- Limitations: SELECT remains exclusive and readers serialize. Benchmark setup is excluded from operation timing; environment and 100 ms sample durations limit precision.
- Commit: `f688ab6` (`Cache pages during indexed selects`).

### Serialize table close with API operations

- Priority: resource and concurrency correctness. `Table.Close` bypassed the mutex used by SELECT, mutations, index DDL, and validators, so closing a table concurrently could race with file I/O and partially close owned handles. Repeated close also returned descriptor-closed errors.
- Changes: `Close` now waits on the table operation mutex, closes the pager and every index while collecting errors, and becomes idempotent. Added `ErrTableClosed` checks to operations and validators so calls queued after close return a stable error.
- Regression coverage: race a SELECT and `ValidateIndexes` against `Close`; either the operation completes first or reports `ErrTableClosed`. Verify repeated close and post-close API behavior.
- Verification: targeted test repeated 100 times and under race detection 25 times; full tests, build, vet, race tests for table/WAL/B+ tree/pager, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: callers using the publicly exposed `Table.Pager` directly remain responsible for synchronizing that access with `Close`.
- Commit: `d393111` (`Serialize table close with operations`).

### Shared-read-lock assessment

- Finding: changing the OS lock from exclusive to shared is not sufficient. Every SELECT runs WAL recovery and refreshes pager/index metadata and the index map. Recovery can write files, while refresh mutates state shared by queries; allowing concurrent readers without changing this lifecycle would introduce races or read partially recovered state.
- Direction: a safe shared-read design likely needs a durable table generation/read epoch plus a distinct recovery path for a pending WAL, or immutable per-query state. Benchmarking confirms lock contention but does not justify weakening storage consistency before that foundation exists.

### Add exact TEXT secondary indexes

- Priority: useful indexing support. The B+ tree stores `uint64` keys, which previously limited table secondary indexes to numeric columns even though TEXT is the default user-column type.
- Design and changes: TEXT values map to the first 64 bits of SHA-256 as a stable bucket key. Indexed SELECT verifies the original stored text and returns only exact matches; distinct strings sharing a hash bucket are valid candidates, while an entry whose value hashes to a different bucket is reported as stale. Inserts, backfill, update, delete, validation, create/drop, reopen, and SQL use the same encoding. Metadata v4 permits TEXT catalog flags; v1-v3 remain readable, and creating a TEXT index upgrades old metadata as needed.
- Regression coverage: empty, duplicate, Unicode, and long text; equality and missing-value queries; update/delete/reopen/drop; deterministic forced hash collisions; v1 table migration; metadata v4 catalog round trip; SQL create/query/drop.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/CLI, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: the current B+ tree remains a `uint64` bucket index, so collisions can increase candidate scans and TEXT indexes provide equality lookup only; rows are compared exactly before returning results.
- Commit: `04b1157` (`Add exact text secondary indexes`).

### Choose the most selective index for equality filters

- Priority: indexed query cost. SELECT previously used the first indexed predicate in input order. A benchmark with 2,048 rows, one broad posting list, and one single-row match measured 1.47 ms, 664 KB/op when broad came first versus 46 µs, 86 KB/op when selective came first.
- Changes: added `Index.CountAll`, which counts posting-list RIDs without allocating a RID slice and rejects cycles, invalid posting pages/counts, and out-of-range links. For multiple indexed predicates, SELECT counts candidates and fetches the shortest list; single-index queries retain the prior single `FindAll` path.
- Regression coverage: B+ tree counts for unique, duplicate, shrunk, missing, and cyclic keys; table queries return identical rows under reversed filter orders and return header-only results for absent matches. Added a skewed two-index benchmark.
- Benchmark evidence on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=100ms -cpu=1`: after planning both filter orders measured about 55 µs/op and 121 KB/op. Broad-first improved from 1.47 ms / 664 KB; selective-first changed from 46 µs / 86 KB. These short runs are directional.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/CLI, Windows and Solaris cross-builds, benchmark run, and `git diff --check` passed. An initial verification caught a test assertion comparing `uint64` with `int`; the assertion was corrected and the full checks rerun successfully.
- Limitations: candidate counts require traversing each posting chain, so multi-index planning has additional index I/O; the shortest list is selected by RID count, without accounting for row-page locality.
- Commit: `5b7937b` (`Choose selective index for equality filters`).

### Add streaming UINT64 range selection

- Priority: ordered lookup capability. The B+ tree stored sorted keys and duplicate posting chains, but callers had no supported way to scan a key interval.
- Changes: added `Index.RangeIterator` to stream distinct keys and their RIDs in key order with optional inclusive/exclusive bounds, unbounded endpoints, posting-chain reads, and leaf-cycle detection. Added `Table.SelectRange`, which uses the index when available and falls back to a scan; additional equality filters work with either path. README documents the API and unspecified row order.
- Regression coverage: scans across multiple leaf pages, boundary inclusivity, empty point ranges, duplicate postings, unbounded scans, indexed-versus-scan equivalence, empty matches, additional filters, update/delete, and reopen.
- Verification: targeted range tests repeated 10 times; full tests, build, vet, race tests for table/pager/B+ tree/WAL/CLI, Windows and Solaris cross-builds, and `git diff --check` passed.
- Limitations: SQL range predicates are not yet implemented; range selection is available through the Go API. The iterator requires callers to exclude concurrent index mutation; `Table.SelectRange` provides that coordination.
- Commit: `c65594b` (`Add UINT64 range selection`).

### Add SQL range predicates for SELECT

- Priority: make the new range API usable from the command line. The SQL parser previously accepted equality filters only.
- Changes: lexer recognizes `<`, `<=`, `>`, and `>=`; SELECT parsing records range filters separately from equality filters. The CLI combines bounds for one `UINT64` column and dispatches to `Table.SelectRange`. Equality predicates can accompany the range. README documents operators and unspecified result ordering.
- Regression coverage: lexer tokenization; parser range query, malformed bounds, and rejection in UPDATE/DELETE; command-loop scan and indexed range queries with equality filters.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, and `git diff --check` passed. Initial parser verification found equality-only queries represented an empty nonnil range slice; this was corrected to preserve existing parse output and all checks were rerun.
- Limitations: SQL supports one ranged column per SELECT; range predicates are not yet supported in UPDATE or DELETE.
- Commit: `92501a7` (`Add SQL SELECT range predicates`).

### Add range predicates to UPDATE and DELETE

- Priority: complete the SQL range feature. Mutations previously accepted equality filters only, even though the table API could select UINT64 ranges.
- Changes: added `Table.UpdateRange` and `Table.DeleteRange` inside the existing WAL mutation wrapper. They validate the range column, select matching rows during the mutation scan, and retain the existing collect-before-apply update flow so row relocation cannot invalidate later matches. SQL UPDATE/DELETE now parse and resolve range filters; equality predicates can accompany the range. README documents range support for all three statements.
- Regression coverage: parser cases for update/delete ranges; a command-loop test covering indexed range updates, range deletes, equality plus range predicates, reopen, and index validation; WAL rollback test injecting failure after range update page/index writes and verifying rows and indexes are restored.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, and `git diff --check` passed. Parser verification initially caught empty equality filters represented as nonnil on range-only statements; the parser now preserves nil for absent equality filters, and all checks passed after that correction.
- Limitations: range mutations currently scan rows even when the ranged column has an index. This keeps matching and mutation behavior simple; range-index candidate collection should be benchmarked before optimizing.
- Commit: `11448e1` (`Add range predicates to mutations`).

### Measure range UPDATE selection cost

- Finding: added `BenchmarkTableRangeUpdate` for 2,048 rows and narrow (four-row) and broad ranges; the update changes an unrelated TEXT column so iterations select the same rows. The first benchmark draft used the default range column index and was discarded; the corrected benchmark explicitly ranges over the indexed user column.
- Corrected pre-optimization baseline on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=200ms -cpu=1`: narrow scan 1.37–1.45 ms/op and about 667 KB/op; narrow with secondary index present 1.36–1.37 ms/op and about 672 KB/op. Broad scan 63.7–70.4 ms/op and about 43.9 MB/op; broad with the index present 64.3–65.1 ms/op and about 43.9 MB/op.
- Verification: `go test ./internal/table` and `git diff --check` passed; pre-optimization benchmark ran on commit `babae85` with a separate Go build cache.
- Next: use the range index to collect mutation candidates before changing pages/indexes, retain scan fallback, and compare against this baseline. Validate indexed entries against stored rows and test partial-write rollback.
- Commit: `9e09c71` (`Benchmark range update scans`).

### Use range indexes for mutation candidates

- Finding: the corrected benchmark showed a four-row range update scanning all 2,048 rows took about 1.4 ms and allocated 667 KB even when a secondary index was available.
- Changes: range UPDATE/DELETE now read matching RIDs from the ranged-column index before modifying pages, validate each candidate against the stored row and index key, reject duplicate RIDs, then apply changes under the existing WAL transaction. Unindexed range mutations retain the table scan. UPDATE still collects all changes before page writes, and DELETE collects all index candidates before changing index pages.
- Regression coverage: added stale-secondary-key rejection with rollback and cleanup, plus unindexed scan fallback for range UPDATE and DELETE. Existing SQL range mutation, reopen/`ValidateIndexes`, and WAL rollback tests exercise the indexed path.
- Benchmark comparison on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=200ms -count=2 -cpu=1`: narrow scan 1.40–1.44 ms/op, 667 KB/op; indexed candidate path 0.205–0.211 ms/op, 210 KB/op. Broad scan 63.1–63.3 ms/op, 43.9 MB/op; indexed path 61.8–62.9 ms/op, 44.4 MB/op. Narrow indexed updates were about 7x faster and allocated about 69% less; broad ranges were similar in time and used about 0.5 MB more allocations.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, corrected narrow/broad benchmark, and `git diff --check` passed.
- Limitations: the indexed path validates each candidate but relies on secondary-index completeness, which normal table mutations maintain and `ValidateIndexes` can check; broad ranges retain a small memory overhead. Native Windows/Solaris durability behavior remains unverified.
- Commit: `3fc6ccc` (`Use range indexes for mutations`).

### Bound broad range mutation candidates

- Finding: indexed candidates improved narrow ranges substantially, but broad ranges retained every candidate alongside the pending update and allocated about 0.5 MB more than a scan.
- Changes: candidate collection now stops retaining RIDs after 128 entries and asks UPDATE/DELETE to use the existing scan path. The cap counts all range-index RIDs, including those later rejected by equality filters. Added a medium-range benchmark and an explicit test for the bounded fallback path.
- Benchmark on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=200ms -count=2 -cpu=1`: narrow (4 rows) scan 1.29–1.47 ms/op and 667 KB/op; indexed 0.211–0.215 ms/op and 210 KB/op. Medium (128 rows) scan 4.94–5.23 ms/op and 3.47 MB/op; indexed 3.89–3.98 ms/op and 3.07 MB/op. Broad (2,048 rows) scan 62.8–81.1 ms/op and 43.9–44.0 MB/op; capped index path 62.7–63.1 ms/op and 43.98 MB/op. The broad path's extra allocations fell from about 0.5 MB to about 70 KB while narrow and medium ranges retain the index benefit.
- Regression coverage: candidate collection falls back after the cap and the resulting scan update affects all rows; existing tests cover indexed and unindexed update/delete, stale candidate rejection, rollback, reopen, and index validation.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, narrow/medium/broad benchmark, and `git diff --check` passed.
- Limitations: the mutation candidate cap bounds retained row candidates; exact duplicate validation still needs a per-key RID set while a posting chain is fully consumed. Docker is Linux-only here; QEMU is installed, but no Windows or Solaris guest image is present under `/home/rahul`.
- Commit: `ad1d4b4` (`Bound broad range mutation candidates`).

### Stream RIDs for range mutations

- Finding: even after candidate capping, `RangeIterator.Next` loaded the complete posting chain for a key before returning its first RID. A hot key could therefore allocate well beyond the 128-RID mutation cap.
- Changes: added `Index.RangeRIDIterator`, which returns one key/RID pair at a time and validates posting page types, counts, bounds, cycles, and duplicate RIDs within each consumed posting chain. Range mutation candidate collection now uses it and can stop after 128 RIDs, falling back to the table scan. The existing batch `RangeIterator` remains available to callers that need all RIDs for a key.
- Regression coverage: streamed 10,000 RIDs across posting pages, checked bounded ranges across leaves and duplicate postings, detected a posting cycle, and exercised the mutation fallback for one hot key with 129 rows.
- Benchmark on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=200ms -count=3 -cpu=1`: batch range iteration for 10,000 duplicate RIDs 1.52–1.93 ms/op and 872 KB/op; RID streaming 77–81 µs/op and 78.7 KB/op.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, duplicate-RID and range mutation benchmarks, and `git diff --check` passed.
- Limitations: exact duplicate-RID validation uses a per-key set that grows with the posting chain; mutations stop after 128 RIDs, but full range SELECT consumes and validates the entire chain.
- Commit: `4397ef5` (`Stream range mutation RIDs`).

### Stream RIDs in indexed range SELECT

- Finding: `Table.SelectRange` still used the batch iterator, which held both a RID slice and duplicate-check set for a large posting chain. The streamed iterator now validates duplicate RIDs per key without materializing the RID slice.
- Changes: indexed range selection now consumes `RangeRIDIterator` and retains the existing row/key validation, page cache, and exact duplicate-RID error behavior. Added a table benchmark for a duplicate-heavy range and a corruption test that injects a duplicate posting RID.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, duplicate-posting benchmark, and `git diff --check` passed.
- Benchmark on Linux/amd64, 13th Gen Intel Core i5-13500H: B+ tree iteration over 10,000 duplicate RIDs improved allocation from 872 KB/op (batch) to 514 KB/op (stream), about 41% less; time was similar (batch 1.55–1.67 ms/op, stream 1.49–1.70 ms/op). Table SELECT over 1,024 duplicate rows measured about 0.60 ms/op and 319 KB/op on both baseline and streamed paths; the projected rows dominate allocation at this size.
- Limitations: the per-key duplicate-check set still grows with the posting chain, so memory is reduced rather than strictly bounded. Removing exact validation would risk duplicate rows from corrupted indexes.
- Commit: `c03a50f` (`Stream indexed range selection`).

### Reduce duplicate-RID validation allocation

- Finding: the streamed iterator removed the materialized batch but retained a Go map for exact duplicate detection. On a 10,000-RID posting chain, this map still accounted for about 514 KB/op.
- Changes: `RangeRIDIterator` now stores the current chain's RIDs in a compact slice, sorts them when the chain ends, and checks adjacent entries for duplicates. It still returns each RID incrementally and preserves exact duplicate detection after a chain is fully consumed.
- Verification: full tests, build, vet, race tests for table/pager/B+ tree/WAL/parser/lexer/CLI, Windows and Solaris cross-builds, duplicate-chain and table range-select benchmarks, and `git diff --check` passed.
- Benchmark on Linux/amd64, 13th Gen Intel Core i5-13500H, `-benchtime=200ms -count=2 -cpu=1`, with RIDs clustered at 256 slots per page: for 10,000 duplicate RIDs, batch iteration 1.79–2.23 ms/op and 872 KB/op; streamed slice validation 0.209–0.220 ms/op and 436 KB/op. For 100,000 duplicate RIDs, batch 17.08–17.76 ms/op and 8.31 MB/op; streamed slice validation 2.12–2.25 ms/op and 4.83 MB/op. Table SELECT over 1,024 duplicate rows measured 0.462–0.472 ms/op and 289 KB/op, compared with the prior 0.598–0.600 ms/op and 319 KB/op.
- Assessment: the compact slice is substantially faster and uses about half the allocation of the previous batch path at both chain sizes. External sorting or a persistent uniqueness structure would add I/O or format/write-path complexity. Keep the compact exact check for now and revisit if production workloads show chains materially larger than 100,000 RIDs or a tighter memory target.
- Limitations: the validation slice still grows linearly with one posting chain; this halves allocation relative to the previous streaming map and avoids the batch RID slice, but does not give a strict constant-memory bound. Exact validation for arbitrary unsorted RIDs would need external storage or a persistent uniqueness structure.
- Commit: `64098c2` (`Compact range RID duplicate checks`).
- Scaling benchmark follow-up: `766bb57` (`Benchmark duplicate RID scaling`).

### Enable native Windows CI and assess Solaris guest coverage

- Finding: the Windows file-lock fallback and filesystem durability paths are only cross-built locally; native execution is needed to expose platform-specific behavior. A Solaris-targeted package test binary can cross-compile, but no Oracle Solaris runtime was present.
- Changes: added `.github/workflows/ci.yml` with Linux and Windows runners that execute the full test suite, build, and vet on pushes and pull requests. Downloaded the official OpenIndiana Hipster 2026.04 minimal ISO to `/tmp` and verified its published SHA-256. QEMU reached the illumos boot loader, but the kernel did not reach an install console in either TCG or KVM attempts. This is not counted as Solaris runtime coverage; OpenIndiana is an illumos distribution, and Oracle's Solaris downloads require license acceptance and account sign-in.
- Verification: local full tests, build, vet, and `git diff --check` passed before edits. Windows and Solaris test binaries cross-compiled for all Go packages. `actionlint` passed on the new workflow. The GitHub workflow itself has not run from this local environment.
- Limitations: Windows runtime coverage will be available after this workflow is pushed and run by GitHub Actions. Native Solaris and OpenIndiana runtime tests remain unavailable here. The downloaded ISO and VM disk are temporary files outside the repository.
- Commit: `e9c501c` (`Add native Windows CI coverage`).

### Bounds-check B-tree leaf entry setters

- Finding: `BTreePage.SetKey` and `SetRID` used caller-supplied signed indexes without validation and assumed a leaf layout. Negative or oversized indexes could panic, and calling them on an internal page could overwrite child-pointer bytes.
- Changes: both setters now return errors and validate a nonnil leaf page, an in-range physical slot, and a key count that does not exceed leaf capacity before writing. `InsertDuplicate`, `DeleteRID`, and `UpdateRID` propagate `SetRID` failures. The setter audit pattern was informed by the `correctness/storage-validation` reference branch.
- Regression coverage: rejected nil pages, internal and invalid node types, negative/past-capacity/MaxInt indexes, and corrupt key counts; each failure is checked for no page mutation. The last physical leaf slot still round-trips key and RID data.
- Verification: targeted B+ tree tests passed; full `GOCACHE=/tmp/goDB-gocache go test ./... -count=1`, build, vet, and `git diff --check` passed. `go test -race ./internal/btree ./internal/table -count=1` passed. Windows and Solaris test binaries for all packages cross-compiled with `CGO_ENABLED=0`.
- Limitations: key/RID getters and the overlapping leaf/internal pointer setters remain unchecked; continue the page accessor audit before treating malformed direct page access as panic-safe.
- Commit: `245fb5b` (`Bounds-check B-tree leaf setters`).

### Guard B-tree leaf/internal pointer setters

- Finding: `SetNextLeaf` and `SetLeftmostChild` update the same serialized bytes but represent different fields for leaf and internal nodes. They previously accepted the wrong layout and could overwrite a child pointer or leaf link; nil receivers could panic.
- Changes: both setters now return errors and validate nil/page type before mutation. The internal split helper, leaf split/rebalance paths, new-root creation, and internal-node rebuild propagate failures. This follows the checked-pointer-setter pattern reviewed in `correctness/storage-validation`.
- Regression coverage: nil receivers; wrong node types and invalid type tags; failure leaves page bytes unchanged; valid leaf-link and internal-leftmost-child round trips.
- Verification: targeted B+ tree tests passed; full `GOCACHE=/tmp/goDB-gocache go test ./... -count=1`, build, vet, and `git diff --check` passed. `go test -race ./internal/btree ./internal/table -count=1` passed. Windows and Solaris test binaries for all packages cross-compiled with `CGO_ENABLED=0`.
- Limitations: `GetKey`, `GetRID`, and `GetChild` still trust caller indexes and node layouts; continue the page accessor audit.
- Commit: `92bfb1d` (`Guard B-tree pointer setters`).

### Make Windows table and WAL creation usable

- Finding: the first hosted Windows CI run (`40266422366`) exposed that `os.File.Sync` on an `os.Open` directory handle returns `Access is denied`. This caused table creation and WAL startup to fail; the earlier Linux-only run had not exercised this behavior.
- Changes: added `internal/dirsync` and routed table and WAL directory sync through it. Non-Windows targets retain `os.File.Sync`. Windows opens the directory with write access and `FILE_FLAG_BACKUP_SEMANTICS`, then calls `FlushFileBuffers`; known unsupported/access-denied responses are treated as best-effort success, while other errors still propagate. README now states that directory-entry power-loss durability is not guaranteed on Windows filesystems/configurations that reject directory flushing.
- Regression coverage: shared helper tests verify a valid directory and rejection of a missing directory. Existing table, WAL, and command tests exercise creation/recovery through this helper.
- Verification: full local tests, build, vet, and diff check passed; race tests for dirsync/WAL/table/CLI passed; Windows and Solaris package builds passed; affected Windows test binaries cross-compiled. Microsoft documentation confirms `FlushFileBuffers` requires a `GENERIC_WRITE` handle and that directory handles require `FILE_FLAG_BACKUP_SEMANTICS` ([FlushFileBuffers](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers), [directory handles](https://learn.microsoft.com/en-us/windows/win32/fileio/obtaining-a-handle-to-a-directory)). Native verification followed after the index-handle sharing fix: GitHub Actions run `40267182421` passed the full Windows and Ubuntu test suites, builds, and vet at `b55cb31`.
- Limitations: when Windows/filesystem access denies directory flushing, parent-directory metadata durability remains best-effort; Solaris runtime behavior remains unverified.
- Commit: `726aa61` (`Handle Windows directory sync limitations`).

### Permit safe Windows deletion of open index files

- Finding: after the directory-sync fix, hosted run `40266746157` passed Windows tests for command, B+ tree, pager, WAL, and file locking, but table tests exposed index deletion failures while older handles remained open. Go's Windows `os.OpenFile` opens with `FILE_SHARE_READ|FILE_SHARE_WRITE` and omits `FILE_SHARE_DELETE`, so another table handle prevents the documented drop-and-refresh flow.
- Changes: added `internal/openfile.OpenReadWrite`; Windows opens existing index files with read/write/delete sharing, and other platforms keep `os.OpenFile`. `btree.OpenIndex` uses the helper. This lets an old handle remain valid until its table refresh closes it while the index name is removed.
- Regression coverage: verifies an opened index file can be removed while its handle remains open, and that the handle then closes cleanly. Existing table tests cover dropping an index with an older handle and reopening after missing-index conditions.
- Verification: full local tests, build, vet, and diff check passed; race tests for openfile/B+ tree/table passed; Windows and Solaris builds passed; Windows test binaries for openfile and B+ tree cross-compiled. GitHub Actions run `40267182421` passed the full Windows and Ubuntu test suites, builds, and vet at `b55cb31`, including the index deletion regression.
- Limitations: Windows directory-entry power-loss durability remains best-effort where parent-directory flushing is rejected. Native Windows confirmation passed in GitHub Actions run `40267182421`; Solaris runtime behavior remains unverified.
- Commit: `828a0c0` (`Allow Windows index handles to be deleted`).

### Bounds-check B-tree key reads

- Finding: `GetKey` trusted the node layout and caller index. Invalid types were treated as internal nodes, and negative or oversized indexes could panic or read bytes outside the live key range.
- Changes: `GetKey` now returns `(uint64, error)` and validates a nonnil leaf/internal page, physical key capacity, and logical index range. Tree descent, insertion/splitting, lookup, rebalancing, validation, range iterators, and table index validation propagate read errors. `SearchLeafPage` and `findChild` now surface malformed-page errors. The checked-accessor approach follows the `correctness/storage-validation` reference branch.
- Regression coverage: nil page; invalid and posting node types; negative, past-count, and MaxInt indexes; corrupt leaf/internal counts; valid leaf/internal reads; and no mutation after failed access.
- Verification: baseline full tests/build/vet passed before changes. After changes, `go test ./... -count=1`, `go build ./...`, `go vet ./...`, and `git diff --check` passed. B-tree and table tests passed; B-tree and table race tests passed; Windows and Solaris package test binaries cross-compiled and were exercised with `go test -exec=true`; `git diff --check` passed.
- Limitations: `GetRID` and `GetChild` remain unchecked and are the next accessor audit. Native Solaris runtime coverage is still unavailable.
- Commit: `0fcfcd5` (`Bounds-check B-tree key reads`).

### Bounds-check B-tree RID reads

- Finding: `GetRID` read leaf-entry bytes without checking the page type, stored key count, or requested index, so malformed pages could panic or expose bytes from a different layout.
- Changes: `GetRID` now returns `(*RID, error)` and validates a nonnil leaf page, its physical capacity, and the logical slot. Lookup, posting operations, deletion/rebalancing, range iterators, index validation, and table validation propagate accessor errors.
- Regression coverage: nil page; internal and invalid node types; negative, past-count, and MaxInt indexes; corrupt count; no mutation on failure; last-slot RID round trip remains covered.
- Verification: focused B-tree/table tests, full `go test ./... -count=1`, build, vet, and diff check passed. B-tree/table race tests passed; Windows and Solaris package test binaries cross-compiled with `go test -exec=true`. Hosted run `40267970951` passed Windows and Ubuntu tests/build/vet, including this accessor change.
- Limitations: `GetChild` remains unchecked and is the next accessor audit. Native Solaris runtime coverage is unavailable.
- Commit: `7b991cb` (`Bounds-check B-tree RID reads`).

### Bounds-check B-tree child reads

- Finding: `GetChild` assumed an internal-node layout and trusted a uint32 child index. A malformed page could return bytes from an unrelated field or panic on an out-of-range slice.
- Changes: `GetChild` now returns `(uint32, error)` and validates page type, key count, and the valid child range `[0, numKeys]`. Tree descent, splits, subtree lookup, rebalancing, validation, and `GetChildren` propagate errors; `PrintTree` now reports child-read failures.
- Regression coverage: nil page; leaf and invalid node types; past-count and MaxUint32 indexes; corrupt count; unchanged page bytes on failure; valid leftmost and right-child round trips.
- Verification: full tests/build/vet, B-tree/table race tests, and Windows/Solaris package cross-target checks passed.
- Native CI: run `40268129348` passed Windows and Ubuntu tests/build/vet.
- Limitations: native Solaris runtime coverage remains unavailable.
- Commit: `31158be` (`Bounds-check B-tree child reads`).

### Guard B-tree key-count increments

- Finding: `IncrementNumKeys` incremented the serialized uint16 without checking the page type or node-specific capacity, so a full or malformed page could silently become structurally invalid or wrap.
- Changes: the helper now returns an error for nil, non-leaf/internal, and full/corrupt pages. Leaf and internal insertion paths propagate failures and reject already-full pages before moving entry bytes.
- Regression coverage: full leaf/internal counts, corrupt uint16 count, posting/invalid types, nil receiver, failed-call no mutation, and valid increments to each page capacity.
- Verification: focused tests passed; full tests/build/vet, B-tree/table race tests, and Windows/Solaris package cross-target checks passed. Hosted run `40268329831` passed Windows and Ubuntu tests/build/vet.
- Limitations: `SetNumKeys` remains a deliberately raw serialized-field writer for split/rebalance logic and corruption tests; its call sites need a separate invariant audit.
- Commit: `fc85cdc` (`Guard B-tree key count increments`).

### Bounds-check posting-page RID access

- Finding: posting-page RID helpers used caller indexes directly against a serialized page. Invalid node types, counts, or indexes could panic or interpret unrelated bytes as a RID.
- Changes: `GetPostingRID` now returns `(RID, error)` and validates a nonnil posting page, count, and live slot. `SetPostingRID` returns an error and validates page type, count, append position, and physical capacity. Duplicate insertion, compaction, chain reads, range streaming, benchmark setup, and corruption tests propagate errors.
- Regression coverage: nil/wrong page types, empty/corrupt counts, negative, skipped, past-count and out-of-capacity indexes, no mutation on failure, append-at-count and final physical slot round trips.
- Verification: focused B-tree/table tests and full tests/build/vet passed; B-tree/table race tests passed; Windows and Solaris package test binaries cross-compiled with `go test -exec=true`. Hosted run `40268530725` has Ubuntu green; Windows is still in progress.
- Limitations: `SetNumKeys` and `SetPostingCount` remain raw serialized-field writers; their call-site invariants need review.
- Commit: `48db65f` (`Bounds-check posting page RID access`).

### Bounds-check B-tree entry insertion

- Finding: `InsertIntoLeafAt` and `InsertIntoInternalAt` trusted caller positions and counts before copying serialized entries, which could panic or write across page layouts.
- Changes: both helpers now return errors and validate a nonnil matching node type, agreement between supplied and stored counts, available capacity, and insertion position before mutation. Index insert/split and leaf-rebalance callers propagate errors. `SetNumKeys` is documented as a raw serialized-field write used by validated split/rebalance paths and corruption tests.
- Regression coverage: nil pages; wrong node types; negative/past positions; mismatched, full, and corrupt counts; failed-call no mutation; valid leaf/internal insert-and-read round trips.
- Verification: focused tests passed; full `go test ./... -count=1`, build, vet, diff check, B-tree/table race tests, and Windows/Solaris package test-binary cross-builds passed.
- Limitations: `SetNumKeys` and `SetPostingCount` remain raw count writers; review their call-site invariants next.
- Commit: `e65c1bb` (`Check B-tree entry insertion bounds`).

### Validate B-tree page count setters

- Finding: `SetNumKeys` and `SetPostingCount` wrote serialized counts without checking the page type or node-specific capacity. Production callers relied on local invariants, so malformed values could still be written when a new call path missed those assumptions.
- Changes: both setters now return errors and validate nil receivers, node type, and maximum count before mutation. Split, posting-chain, deletion, and rebalance callers propagate errors. Split helpers validate source and destination layouts/counts before copying. Corruption fixtures write raw serialized counts through a test-only helper.
- Regression coverage: invalid node types, nil receivers, over-capacity counts, no mutation after failure, and valid leaf/internal/posting capacity boundaries.
- Verification: focused B-tree/table tests and full tests/build/vet passed. B-tree/table race tests passed. Windows and Solaris package test binaries cross-compiled with `CGO_ENABLED=0` and were exercised with `go test -exec=true`. Hosted run `40268748666` passed Ubuntu and Windows tests/build/vet.
- Limitations: native Solaris runtime coverage remains unavailable. Remaining fixed-offset posting getters/setters and other direct page field accessors still need review.
- Commit: `c4c5612` (`Validate B-tree page count setters`).

### Validate B-tree leaf and posting links

- Finding: `PostingCount`, `NextPostingPage`, `NextLeaf`, and the posting-link setter read or wrote shared serialized bytes without checking nil receivers or page types. A wrong-layout page could therefore expose unrelated fields as counts or links.
- Changes: posting count reads now reject non-posting pages and over-capacity counts; posting-link reads/writes reject nil or non-posting pages; next-leaf reads reject nil or non-leaf pages. Posting-chain traversal, range iteration, leaf split/rebalance, index validation, and table validation propagate accessor errors.
- Regression coverage: nil and wrong-type pages, corrupt posting counts, valid link round trips, and no mutation after failed posting-link writes.
- Verification: focused and full tests, build, vet, B-tree/table race tests, Windows and Solaris test-binary cross-target checks, and `git diff --check` passed.
- Limitations: native Solaris runtime coverage remains unavailable. Parent pointer accessors and `GetNumKeys` still read/write fixed offsets without checked APIs; page ID validity is checked by chain traversal context.
- Commit: `dca4a37` (`Validate B-tree link accessors`).

### Validate B-tree parent pointer accessors

- Finding: `ParentID`, `GetParentID`, and `SetParentID` used bytes that are the posting-chain link field on posting pages. Calls on nil, free, posting, or invalid pages could interpret or overwrite unrelated serialized fields.
- Changes: parent reads now return errors for nil and non-tree pages; parent writes validate leaf/internal node type before mutation. Index open/refresh, splits, root changes, rebalancing, validation, and parent consistency checks now propagate errors.
- Regression coverage: nil receivers, posting/free/invalid node types, no mutation after failed writes, and valid leaf/internal parent round trips.
- Verification: focused and full tests, build, vet, B-tree/table race tests, Windows/Solaris test-binary cross-target checks, and `git diff --check` passed.
- Limitations: native Solaris runtime coverage remains unavailable. `GetNumKeys` is the remaining unchecked fixed-offset page accessor; its 82 call sites need a deliberate checked-API migration.
- Commit: pending.

## Prioritized backlog

1. **Run storage durability tests on Windows and Solaris.** Creation, WAL, table/index lifecycle, and directory sync now pass native Linux and Windows CI. Windows exposed directory-flush rejection and open-index deletion sharing; both were addressed, and run `40267182421` passed tests, build, and vet on both OSes. Solaris runtime coverage remains unavailable. QEMU could not boot the checksum-verified OpenIndiana guest to a usable console, and no Oracle Solaris runtime is available. Solaris uses a process-local lock fallback. Dependencies: native or emulated test environments. Risks: directory-sync, rename, unlink, and lock behavior differs; cross-builds and injected Linux failures do not prove power-loss guarantees. Acceptance: native Windows coverage is satisfied by the hosted matrix; obtain a usable Solaris/illumos runtime and run table/index creation and removal, interrupted-create recovery, WAL begin/rollback/recovery, and lock tests there. Document unsupported semantics or return clear errors if required primitives fail. Tests needed: native runtime coverage of those paths on Solaris.
2. **Audit remaining B-tree page accessors and mutators.** Leaf/internal entry writes, child reads, posting RID access, key-count increments, page count setters, leaf/posting link accessors, and parent pointer accessors now validate type, count, and bounds. `GetNumKeys` remains unchecked. Dependencies: none. Risks: changing this common count API affects 82 call sites across tree operations, validation, and tests. Acceptance: count reads reject nil, non-tree nodes, and node-specific overflow; production callers propagate errors, while tests use explicit raw fixtures only for corruption cases. Tests needed: malformed-page boundary cases, no-mutation checks for setters, full suite, race tests, and platform cross-builds.
3. **Design a durable read epoch before shared read locking.** SELECT recovery and refresh currently mutate WAL, pager, and index state under an exclusive lock. A shared-lock change alone is unsafe. Dependencies: define a persisted generation or immutable read-state design and a way for readers to detect pending recovery. Acceptance: concurrent reads overlap while writers/recovery remain excluded, stale handles refresh safely, and readers never observe partially recovered state. Tests/benchmarks needed: process-level readers and writers, race tests, crash-recovery interaction, and before/after parallel query throughput.
4. **Reassess duplicate-check memory if posting chains exceed 100,000 RIDs.** Exact validation currently retains a compact RID slice; at 100,000 RIDs it uses 4.83 MB/op and is much faster and smaller than batch iteration. Further reduction would require external I/O or a persistent structure. Dependencies: production evidence of larger chains or a stricter memory target. Risks: external sorting adds temporary-file lifecycle and latency; persistent metadata complicates index writes and migration. Acceptance: benchmark a representative workload above 100,000 RIDs and implement another approach only if current memory is material. Tests/benchmarks needed: multi-page chains, injected duplicate and cycle corruption, index/scan equivalence, time and peak allocation.

## Next action

Next task: finish verification and commit the checked parent pointer accessors, then migrate `GetNumKeys` callers to a checked API. Validated count setters are committed at `c4c5612`; leaf/posting link accessors are committed at `dca4a37`. Hosted run `40269369747` passed on Ubuntu and Windows for `b526b7f`. Preserve the supplied untracked `AGENTS.md`. Separately continue seeking a usable Solaris/illumos runtime; Windows native storage tests pass in hosted CI.
