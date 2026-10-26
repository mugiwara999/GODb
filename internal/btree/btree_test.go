package btree

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func withIndex(t *testing.T, colName string, fn func(*Index)) {
	t.Helper()
	// path := filepath.Join(colName)
	idx, _ := NewIndex("test", colName)
	defer idx.Close()
	fn(idx)
}

func TestInsertAndFindSequential(t *testing.T) {
	withIndex(t, "test_seq", func(idx *Index) {
		const N = 100000
		// Insert keys 0 .. N-1 with RID = {pageID=key, slotID=key%100}
		for i := 0; i < N; i++ {
			err := idx.Insert(uint64(i), uint32(i), uint16(i%100))
			if err != nil {
				t.Fatalf("Insert key %d: %v", i, err)
			}
		}

		// Look up every key
		for i := 0; i < N; i++ {
			if i%10000 == 0 {
				err := idx.Validate()
				if err != nil {
					t.Fatalf("Invalid Tree")
				}
			}
			rid, err := idx.Find(uint64(i))
			if err != nil {
				t.Fatalf("Find key %d: %v", i, err)
			}
			if rid.pageID != uint32(i) || rid.slotID != uint16(i%100) {
				t.Errorf("key %d: got RID (%d,%d), want (%d,%d)",
					i, rid.pageID, rid.slotID, i, i%100)
			}
		}

	})
}

func TestInsertRandom(t *testing.T) {
	withIndex(t, "test_rand", func(idx *Index) {
		rng := rand.New(rand.NewSource(42))
		expected := make(map[uint64]RID)
		const N = 100000

		// Insert
		for i := 0; i < N; i++ {
			if i%10000 == 0 {
				err := idx.Validate()
				if err != nil {
					t.Fatalf("Invalid Tree")
				}
			}
			key := rng.Uint64() >> 1 // avoid sign issues
			rid := RID{uint32(key % 1000), uint16(i % 100)}
			err := idx.Insert(key, rid.pageID, rid.slotID)
			if err != nil {
				t.Fatalf("Insert key %d: %v", key, err)
			}
			expected[key] = rid
		}

		// Find all
		for key, want := range expected {
			got, err := idx.Find(key)
			if err != nil {
				t.Fatalf("Find key %d: %v", key, err)
			}
			if *got != want {
				t.Errorf("key %d: got %v, want %v", key, got, want)
			}
		}

		// Look for non‑existent key
		if _, err := idx.Find(^uint64(0)); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("Find non-existent key error = %v, want ErrKeyNotFound", err)
		}
	})
}

func TestInsertRejectsDuplicateKeyWithoutChangingTree(t *testing.T) {
	withIndex(t, "test_duplicate", func(idx *Index) {
		const n = 1000
		for i := 0; i < n; i++ {
			if err := idx.Insert(uint64(i), uint32(i), uint16(i%100)); err != nil {
				t.Fatalf("Insert key %d: %v", i, err)
			}
		}

		beforePages := idx.GetNumPages()
		if err := idx.Insert(500, 9999, 99); !errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("duplicate Insert error = %v, want ErrDuplicateKey", err)
		}
		if got := idx.GetNumPages(); got != beforePages {
			t.Fatalf("page count after duplicate insert = %d, want %d", got, beforePages)
		}
		got, err := idx.Find(500)
		if err != nil {
			t.Fatalf("Find key 500: %v", err)
		}
		if got.GetPageID() != 500 || got.GetSlotID() != 0 {
			t.Fatalf("RID after duplicate insert = (%d,%d), want (500,0)", got.GetPageID(), got.GetSlotID())
		}
		if err := idx.Validate(); err != nil {
			t.Fatalf("Validate after duplicate insert: %v", err)
		}
	})
}

func TestDeleteSeparatorKeyKeepsTreeSearchable(t *testing.T) {
	withIndex(t, "test_delete_separator", func(idx *Index) {
		for i := 0; i < 600; i++ {
			if err := idx.Insert(uint64(i), uint32(i), 0); err != nil {
				t.Fatalf("Insert key %d: %v", i, err)
			}
		}

		root, err := idx.GetRootPage()
		if err != nil {
			t.Fatalf("GetRootPage: %v", err)
		}
		if root.NodeType() != InternalNode || root.GetNumKeys() == 0 {
			t.Fatalf("root = type %v with %d keys, want internal root", root.NodeType(), root.GetNumKeys())
		}
		// The final root separator routes into the rightmost leaf, which has
		// spare capacity after this insert count. This isolates fence-key
		// routing from underflow/rebalancing behavior.
		key := root.GetKey(root.GetNumKeys() - 1)
		if err := idx.Delete(key); err != nil {
			t.Fatalf("Delete separator key %d: %v", key, err)
		}
		if _, err := idx.Find(key); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("Find deleted separator key error = %v, want ErrKeyNotFound", err)
		}
		for _, neighbor := range []uint64{key - 1, key + 1} {
			if _, err := idx.Find(neighbor); err != nil {
				t.Fatalf("Find neighbor %d after deletion: %v", neighbor, err)
			}
		}
		if err := idx.Validate(); err != nil {
			t.Fatalf("Validate after deleting separator key: %v", err)
		}
	})
}

func TestDeleteRebalancesCollapsesAndReusesPages(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	t.Setenv("DATA_DIR", dataDir)

	idx, err := NewIndex("delete_rebalance", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	const total = 50000 // enough leaves to force a multi-level internal tree
	for key := 0; key < total; key++ {
		if err := idx.Insert(uint64(key), uint32(key), 0); err != nil {
			t.Fatalf("Insert key %d: %v", key, err)
		}
	}
	allocatedPages := idx.GetNumPages()
	root, err := idx.GetRootPage()
	if err != nil {
		t.Fatalf("GetRootPage before deletes: %v", err)
	}
	if root.NodeType() != InternalNode {
		t.Fatalf("root type before deletes = %v, want internal tree", root.NodeType())
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("Validate before deletes: %v", err)
	}

	for step := 0; step < total; step++ {
		key := step / 2
		if step%2 == 1 {
			key = total - 1 - step/2
		}
		if err := idx.Delete(uint64(key)); err != nil {
			t.Fatalf("Delete key %d: %v", key, err)
		}
		if step > 0 && step%5000 == 0 {
			if err := idx.Validate(); err != nil {
				t.Fatalf("Validate after %d deletes: %v", step+1, err)
			}
		}
	}
	if idx.GetNumPages() != allocatedPages {
		t.Fatalf("page count after deleting all keys = %d, want %d (pages should be reusable, not truncated)", idx.GetNumPages(), allocatedPages)
	}
	if idx.freePageID == 0 {
		t.Fatal("free list is empty after collapsing a multi-page tree")
	}
	root, err = idx.GetRootPage()
	if err != nil {
		t.Fatalf("GetRootPage after deletes: %v", err)
	}
	if root.NodeType() != LeafNode || root.GetNumKeys() != 0 {
		t.Fatalf("root after deleting all keys = type %v with %d keys, want empty leaf", root.NodeType(), root.GetNumKeys())
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("Validate empty tree: %v", err)
	}

	for key := 0; key < 1000; key++ {
		if err := idx.Insert(uint64(total+key), uint32(key), 1); err != nil {
			t.Fatalf("reinsert key %d: %v", total+key, err)
		}
	}
	if idx.GetNumPages() != allocatedPages {
		t.Fatalf("page count after reusing free pages = %d, want %d", idx.GetNumPages(), allocatedPages)
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("Validate after page reuse: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}
	idx, err = OpenIndex("delete_rebalance", "id")
	if err != nil {
		t.Fatalf("OpenIndex after page reuse: %v", err)
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("Validate after reopen: %v", err)
	}
	for _, key := range []uint64{total, total + 1, total + 999} {
		if _, err := idx.Find(key); err != nil {
			t.Fatalf("Find reinserted key %d after reopen: %v", key, err)
		}
	}
}

func TestOpenIndexWithoutFreeListMetadata(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	t.Setenv("DATA_DIR", dataDir)

	meta := &BTreePage{ID: metaPageID}
	root := &BTreePage{ID: 1}
	InitMetaPage(meta, "legacy_id")
	InitLeafPage(root)
	path := filepath.Join(dataDir, "legacy_id.idx")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	if _, err := file.Write(meta.Data[:]); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}
	if _, err := file.Write(root.Data[:]); err != nil {
		t.Fatalf("write legacy root: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close legacy index: %v", err)
	}

	idx, err := OpenIndex("legacy", "id")
	if err != nil {
		t.Fatalf("OpenIndex legacy format: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if idx.freePageID != 0 {
		t.Fatalf("legacy free-page head = %d, want zero", idx.freePageID)
	}
	if err := idx.Insert(7, 1, 0); err != nil {
		t.Fatalf("Insert into legacy index: %v", err)
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("Validate legacy index after insert: %v", err)
	}
}

func TestInvariantsAfterInsert(t *testing.T) {
	withIndex(t, "test_inv", func(idx *Index) {
		// Insert enough keys to guarantee at least 2 levels
		for i := 0; i < 500; i++ {
			idx.Insert(uint64(i), uint32(i), 0)
		}

		// 1. Leaf chain
		leafID := getFirstLeafID(idx) // find leftmost leaf (search for key 0)
		var prevLeaf *BTreePage
		var lastKey uint64
		for leafID != 0 {
			leaf, err := idx.GetPage(leafID)
			if err != nil {
				t.Fatalf("leaf %d: %v", leafID, err)
			}
			if leaf.NodeType() != LeafNode {
				t.Fatalf("page %d expected leaf", leafID)
			}
			// Check key order within leaf
			n := leaf.GetNumKeys()
			if n > (PAGE_SIZE-LeafHeaderSize)/LeafEntrySize {
				t.Errorf("leaf %d overflow: %d keys", leafID, n)
			}
			for i := 0; i < n; i++ {
				key := leaf.GetKey(i)
				if i > 0 && key < lastKey {
					t.Errorf("leaf %d: key %d < previous key %d", leafID, key, lastKey)
				}
				lastKey = key
			}
			prevLeaf = leaf
			leafID = prevLeaf.NextLeaf()
		}

		// 2. Parent pointers & internal node consistency
		// For each internal node, check its children all point back to it
		for pageID := uint32(1); pageID < idx.numPages; pageID++ {
			page, err := idx.GetPage(pageID)
			if err != nil {
				continue // pages might be free; if you have free‑list, adjust
			}
			if page.NodeType() == InternalNode {
				numKeys := page.GetNumKeys()
				for i := 0; i <= int(numKeys); i++ {
					childID := page.GetChild(uint32(i))
					child, err := idx.GetPage(childID)
					if err != nil {
						t.Errorf("internal %d child %d: %v", pageID, childID, err)
						continue
					}
					if child.GetParentID() != pageID {
						t.Errorf("parent mismatch: page %d (parent %d) thinks parent is %d",
							childID, page.GetParentID(), pageID)
					}
				}
			}
		}
	})
}

// Helper to find the leftmost leaf. Assumes key 0 exists and leads there.
func getFirstLeafID(idx *Index) uint32 {
	_, leaf, err := idx.FindLeaf(0)
	if err != nil {
		panic("could not find leaf for key 0")
	}
	return leaf.ID
}

func TestRootSplit(t *testing.T) {
	withIndex(t, "test_root_split", func(idx *Index) {
		// Fill many pages. Starting from 0, the first split is easy.
		for i := 0; i < 600; i++ {
			idx.Insert(uint64(i), uint32(i), 0)
		}
		root, err := idx.GetRootPage()
		if err != nil {
			t.Fatal(err)
		}
		if root.NodeType() != InternalNode {
			t.Error("root should be internal after multiple splits")
		}
		if root.ID != idx.rootPageID {
			t.Errorf("root page ID mismatch: got %d, want %d", root.ID, idx.rootPageID)
		}
	})
}
