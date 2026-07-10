package btree

import (
	"math/rand"
	// "path/filepath"
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
		if _, err := idx.Find(^uint64(0)); err == nil {
			t.Error("expected error for non‑existent key")
		}
	})
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
