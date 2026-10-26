# GODb

> **A database for the gods, by the gods.**

GODb is a simple relational database engine written from scratch in Go as a learning project to understand how modern databases work internally.

<!-- The goal of the project is **not** to compete with production databases like PostgreSQL or SQLite, but to build the core components yourself—starting from file storage and ending with a working B+ Tree index. -->

---

## Features

Current features include:

* Fixed-size page storage (4096-byte pages)
* Slotted page layout
* Record insertion and retrieval
* Persistent storage on disk
* Table metadata page
* Insert, update, and delete operations
* B+ Tree index
* Leaf and internal nodes
* Binary search within nodes
* Leaf splitting
* Internal node splitting
* Root creation and split propagation
* Parent pointers
* Leaf sibling pointers for range scans
* B+ Tree deletion rebalancing and page reuse
* Undo journal recovery for table mutations
* Structural and primary-index consistency validators

---

# Architecture

The project is divided into a few small components.

```text
                SQL
                 │
                 ▼
             Parser/Lexer
                 │
                 ▼
          Storage Engine
          (Tables/Records)
                 │
                 ▼
          Pager (4096-byte pages)
                 │
                 ▼
             Disk Files
```

Indexes are implemented separately using a B+ Tree.

```text
            Insert/Search
                  │
                  ▼
             B+ Tree Index
                  │
                  ▼
             Pager Pages
                  │
                  ▼
                Disk
```

---

# Storage Engine

Each table is stored in its own file.

A table file consists of:

```text
+----------------+
| Metadata Page  |
+----------------+
| Page 1         |
+----------------+
| Page 2         |
+----------------+
| Page 3         |
+----------------+
        ...
```

The metadata page stores information such as:

* column names
* number of pages
* other table metadata

The remaining pages store records.

---

# Pages

All data is stored in fixed-size **4096-byte pages**.

Using pages instead of variable-length files allows:

* random access
* efficient disk I/O
* compatibility with B+ Trees
* easier future extensions like caching and transactions

---

# Slotted Pages

Records are stored using a slotted-page layout.

```
+------------------------------------------------------+
| Header | Slot Directory | Free Space | Record Data   |
+------------------------------------------------------+
```

The header stores:

* number of slots
* start of free space
* end of free space

The slot directory stores:

* record offset
* record length

Actual record bytes are packed from the end of the page toward the beginning.

This design allows records of different sizes while avoiding expensive page rewrites.

---

# B+ Tree Index

Indexes are implemented using a B+ Tree.

Leaf nodes contain:

```
key -> Record ID
```

where a Record ID (RID) consists of:

```
(pageID, slotID)
```

Internal nodes only contain separator keys and child pointers.

```
          [50]
         /    \
      <50     >=50
```

Searching proceeds by recursively selecting the appropriate child until a leaf is reached.

---

## Leaf Node Layout

```
Byte 0      : Node Type
Bytes 1-2   : Number of Keys
Bytes 3-6   : Parent Page ID
Bytes 7-10  : Next Leaf Page ID

Repeated:
+---------+---------+--------+
|  Key    | Page ID | SlotID |
+---------+---------+--------+
```

---

## Internal Node Layout

```
Byte 0      : Node Type
Bytes 1-2   : Number of Keys
Bytes 3-6   : Parent Page ID
Bytes 7-10  : Leftmost Child

Repeated:
+---------+--------------+
|  Key    | Right Child  |
+---------+--------------+
```

The implementation follows the rule:

* keys **less than** a separator go left
* keys **greater than or equal** to a separator go right

---

# Searching

Searching starts at the root.

```
Root
 │
 ▼
Internal Node
 │
 ▼
Internal Node
 │
 ▼
Leaf
```

Each internal node performs a binary search over its separator keys to determine which child page to visit next.

Once a leaf is reached, another binary search locates the desired key.

---

# Inserting

Insertion works as follows:

1. Traverse from the root to the target leaf.
2. Insert the key into the leaf while keeping it sorted.
3. If the leaf overflows:

   * split the leaf
   * promote a separator key
4. Insert the separator into the parent.
5. If the parent overflows:

   * split the parent
   * continue propagating upward.
6. If the root splits:

   * create a new root.

---

# Validation

The project contains a recursive validator that checks important B+ Tree invariants, including:

* every leaf is at the same depth
* leaf keys are sorted
* internal separator keys are sorted
* subtree key ranges are valid
* parent pointers are correct
* page IDs are valid
* node occupancy constraints
* leaf chain ordering
* unreachable pages
* cycles in the tree

This validator is heavily used while developing the storage engine.

`Table.ValidatePrimaryIndex` also checks that each live row has the correct ID-index entry and that every index entry points to a matching live row.

## SQL subset

The command-line program reads one statement per input line. Keywords and identifiers are case-insensitive. String values use single quotes; the current lexer does not support escaping an apostrophe inside a string. Statements do not use a trailing semicolon.

```sql
CREATE TABLE users (name, age)
INSERT INTO users VALUES 'Ada Lovelace', 36
SELECT id, name FROM users WHERE id=0 AND name='Ada Lovelace'
UPDATE users SET age=37 WHERE id=0
DELETE FROM users WHERE id=0
```

`SELECT * FROM users` is also supported. `UPDATE` and `DELETE` without a `WHERE` clause apply to every row. `INSERT` takes values in the table's non-ID column order; IDs are generated by the database. This is a small command subset: there are no types, expressions, joins, transaction statements, or query planner.

Mutations to an existing table use an undo journal and are recovered when the table is opened. Initial table and index creation are not crash-atomic.

---

# Current Limitations

This project is intentionally small and educational.

Some features commonly found in production databases are not yet implemented:

* SQL planner and optimizer
* Cross-handle and cross-process concurrency control
* Buffer pool
* Atomic table and index creation
* Secondary indexes

These are possible future additions.

---

