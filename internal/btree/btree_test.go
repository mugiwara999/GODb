package btree

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mugiwara999/goDB/internal/wal"
)

func TestBTreeLeafSettersValidateBoundsAndPageType(t *testing.T) {
	tests := []struct {
		name  string
		page  *BTreePage
		index int
	}{
		{name: "nil page", page: nil, index: 0},
		{name: "internal page", page: func() *BTreePage {
			page := &BTreePage{ID: 1}
			InitInternalPage(page)
			return page
		}(), index: 0},
		{name: "invalid node type", page: func() *BTreePage {
			page := &BTreePage{ID: 2}
			page.Data[0] = 0xff
			return page
		}(), index: 0},
		{name: "negative index", page: func() *BTreePage {
			page := &BTreePage{ID: 3}
			InitLeafPage(page)
			return page
		}(), index: -1},
		{name: "index past capacity", page: func() *BTreePage {
			page := &BTreePage{ID: 4}
			InitLeafPage(page)
			return page
		}(), index: MaxKeysPerLeaf},
		{name: "maximum integer index", page: func() *BTreePage {
			page := &BTreePage{ID: 5}
			InitLeafPage(page)
			return page
		}(), index: math.MaxInt},
		{name: "corrupt key count", page: func() *BTreePage {
			page := &BTreePage{ID: 6}
			InitLeafPage(page)
			page.SetNumKeys(uint16(MaxKeysPerLeaf + 1))
			return page
		}(), index: 0},
	}

	for _, test := range tests {
		t.Run(test.name+"/SetKey", func(t *testing.T) {
			var before [PAGE_SIZE]byte
			if test.page != nil {
				before = test.page.Data
			}
			if err := test.page.SetKey(test.index, 42); err == nil {
				t.Fatal("SetKey succeeded for invalid page or index")
			}
			if test.page != nil && test.page.Data != before {
				t.Fatal("failed SetKey mutated page")
			}
		})
		t.Run(test.name+"/SetRID", func(t *testing.T) {
			var before [PAGE_SIZE]byte
			if test.page != nil {
				before = test.page.Data
			}
			if err := test.page.SetRID(test.index, RID{pageID: 42, slotID: 7}); err == nil {
				t.Fatal("SetRID succeeded for invalid page or index")
			}
			if test.page != nil && test.page.Data != before {
				t.Fatal("failed SetRID mutated page")
			}
		})
	}
}

func TestBTreeLeafSettersWriteLastPhysicalEntry(t *testing.T) {
	page := &BTreePage{ID: 7}
	InitLeafPage(page)
	index := MaxKeysPerLeaf - 1
	if err := page.SetKey(index, 99); err != nil {
		t.Fatalf("SetKey at last physical entry: %v", err)
	}
	wantRID := RID{pageID: 123, slotID: 17}
	if err := page.SetRID(index, wantRID); err != nil {
		t.Fatalf("SetRID at last physical entry: %v", err)
	}
	if got := page.GetKey(index); got != 99 {
		t.Fatalf("last entry key = %d, want 99", got)
	}
	if got := page.GetRID(index); *got != wantRID {
		t.Fatalf("last entry RID = %+v, want %+v", *got, wantRID)
	}
}

func withIndex(t *testing.T, colName string, fn func(*Index)) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	t.Setenv("DATA_DIR", dataDir)
	idx, err := NewIndex("test", colName)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
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

func TestOpenIndexRejectsCorruptMetadataAndPages(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "truncated metadata", mutate: func(data []byte) []byte { return data[:100] }},
		{name: "unaligned file size", mutate: func(data []byte) []byte { return append(data, 1) }},
		{name: "page count mismatch", mutate: func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[4:8], 3)
			return data
		}},
		{name: "root outside page count", mutate: func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0:4], 2)
			return data
		}},
		{name: "name length overflow", mutate: func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[8:12], ^uint32(0))
			return data
		}},
		{name: "free-list head out of range", mutate: func(data []byte) []byte {
			offset := 12 + int(binary.LittleEndian.Uint32(data[8:12]))
			binary.LittleEndian.PutUint32(data[offset:offset+4], 2)
			return data
		}},
		{name: "invalid root type", mutate: func(data []byte) []byte {
			data[PAGE_SIZE] = 9
			return data
		}},
		{name: "root has parent", mutate: func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[PAGE_SIZE+3:PAGE_SIZE+7], 9)
			return data
		}},
		{name: "root key count overflow", mutate: func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[PAGE_SIZE+1:PAGE_SIZE+3], uint16(MaxKeysPerLeaf+1))
			return data
		}},
		{name: "free-list head is not free", mutate: func(data []byte) []byte {
			data = append(data, make([]byte, PAGE_SIZE)...)
			binary.LittleEndian.PutUint32(data[4:8], 3)
			offset := 12 + int(binary.LittleEndian.Uint32(data[8:12]))
			binary.LittleEndian.PutUint32(data[offset:offset+4], 2)
			data[2*PAGE_SIZE] = byte(LeafNode)
			return data
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatalf("create data directory: %v", err)
			}
			t.Setenv("DATA_DIR", dataDir)
			idx, err := NewIndex("corrupt", "id")
			if err != nil {
				t.Fatalf("NewIndex: %v", err)
			}
			if err := idx.Close(); err != nil {
				t.Fatalf("close fixture index: %v", err)
			}
			path := filepath.Join(dataDir, "corrupt_id.idx")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture index: %v", err)
			}
			if err := os.WriteFile(path, tt.mutate(data), 0o644); err != nil {
				t.Fatalf("write corrupted fixture: %v", err)
			}
			opened, err := OpenIndex("corrupt", "id")
			if err == nil {
				_ = opened.Close()
				t.Fatal("OpenIndex succeeded for corrupted file")
			}
		})
	}
}

func TestValidateConcurrentIndexes(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	t.Setenv("DATA_DIR", dataDir)

	indexes := make([]*Index, 4)
	for i, name := range []string{"parallel_a", "parallel_b", "parallel_c", "parallel_d"} {
		idx, err := NewIndex(name, "id")
		if err != nil {
			t.Fatalf("NewIndex(%s): %v", name, err)
		}
		indexes[i] = idx
		t.Cleanup(func() { _ = idx.Close() })
		count := 300 + i*100
		for key := 0; key < count; key++ {
			if err := idx.Insert(uint64(key), uint32(key), 0); err != nil {
				t.Fatalf("Insert %s key %d: %v", name, key, err)
			}
		}
		for key := 0; key < count/2; key++ {
			if err := idx.Delete(uint64(key)); err != nil {
				t.Fatalf("Delete %s key %d: %v", name, key, err)
			}
		}
		if err := idx.Validate(); err != nil {
			t.Fatalf("Validate %s before concurrent checks: %v", name, err)
		}
	}

	errCh := make(chan error, len(indexes))
	var wg sync.WaitGroup
	for _, idx := range indexes {
		wg.Add(1)
		go func(index *Index) {
			defer wg.Done()
			for attempt := 0; attempt < 20; attempt++ {
				if err := index.Validate(); err != nil {
					errCh <- err
					return
				}
			}
		}(idx)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("Validate concurrently: %v", err)
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

func TestDuplicatePostingChainsReopenDeleteAndReusePages(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("duplicate_postings", "age")
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Insert(42, 100, 1); err != nil {
		t.Fatalf("Insert first RID: %v", err)
	}
	total := MaxRIDsPerPosting + 3
	for n := 1; n < total; n++ {
		if err := index.InsertDuplicate(42, uint32(100+n), uint16(n+1)); err != nil {
			t.Fatalf("InsertDuplicate %d: %v", n, err)
		}
	}
	if err := index.InsertDuplicate(42, 100, 1); !errors.Is(err, ErrDuplicateRID) {
		t.Fatalf("duplicate RID error = %v, want ErrDuplicateRID", err)
	}
	if err := index.Insert(42, 9999, 1); err == nil {
		t.Fatal("unique Insert accepted duplicate key")
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate populated posting chain: %v", err)
	}
	pageCount := index.GetNumPages()
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = OpenIndex("duplicate_postings", "age")
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer index.Close()
	rids, err := index.FindAll(42)
	if err != nil {
		t.Fatalf("FindAll after reopen: %v", err)
	}
	if len(rids) != total {
		t.Fatalf("FindAll returned %d RIDs, want %d", len(rids), total)
	}
	if count, err := index.CountAll(42); err != nil || count != uint64(total) {
		t.Fatalf("CountAll = %d, %v; want %d", count, err, total)
	}
	rangeKey := uint64(42)
	ridIterator, err := index.RangeRIDIterator(&rangeKey, true, &rangeKey, true)
	if err != nil {
		t.Fatalf("RangeRIDIterator: %v", err)
	}
	streamed := 0
	for {
		entry, ok, err := ridIterator.Next()
		if err != nil {
			t.Fatalf("RangeRIDIterator.Next: %v", err)
		}
		if !ok {
			break
		}
		if entry.Key != rangeKey {
			t.Fatalf("streamed key = %d, want %d", entry.Key, rangeKey)
		}
		streamed++
	}
	if streamed != total {
		t.Fatalf("RangeRIDIterator returned %d RIDs, want %d", streamed, total)
	}
	first, err := index.Find(42)
	if err != nil || first.GetPageID() != 100 || first.GetSlotID() != 1 {
		t.Fatalf("Find first RID = %v, %v; want (100,1)", first, err)
	}
	for n := 0; n < total-1; n++ {
		if err := index.DeleteRID(42, rids[n].GetPageID(), rids[n].GetSlotID()); err != nil {
			t.Fatalf("DeleteRID %d: %v", n, err)
		}
	}
	remaining, err := index.FindAll(42)
	if err != nil || len(remaining) != 1 || remaining[0].GetPageID() != rids[total-1].GetPageID() {
		t.Fatalf("FindAll after shrinking chain = %v, %v", remaining, err)
	}
	if count, err := index.CountAll(42); err != nil || count != 1 {
		t.Fatalf("CountAll after shrinking = %d, %v; want 1", count, err)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after shrinking chain: %v", err)
	}
	if err := index.DeleteRID(42, remaining[0].GetPageID(), remaining[0].GetSlotID()); err != nil {
		t.Fatalf("Delete last RID: %v", err)
	}
	if _, err := index.FindAll(42); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("FindAll after deleting key = %v, want ErrKeyNotFound", err)
	}
	if _, err := index.CountAll(42); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("CountAll after deleting key = %v, want ErrKeyNotFound", err)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after deleting key: %v", err)
	}
	if err := index.InsertDuplicate(84, 300, 1); err != nil {
		t.Fatalf("InsertDuplicate new key: %v", err)
	}
	if err := index.InsertDuplicate(84, 301, 1); err != nil {
		t.Fatalf("InsertDuplicate duplicate key: %v", err)
	}
	if got := index.GetNumPages(); got > pageCount {
		t.Fatalf("posting page was not reused: page count %d grew beyond %d", got, pageCount)
	}
	if err := index.Delete(84); err != nil {
		t.Fatalf("Delete duplicate key and postings: %v", err)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after page reuse and duplicate deletion: %v", err)
	}
}

func TestRangeIteratorBoundsAcrossLeavesAndDuplicatePostings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("range_iterator", "key")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	for key := uint64(0); key < 1000; key++ {
		if err := index.Insert(key, uint32(key+10), 1); err != nil {
			t.Fatalf("Insert(%d): %v", key, err)
		}
	}
	for n := uint32(0); n < 4; n++ {
		if err := index.InsertDuplicate(250, 2000+n, uint16(n+1)); err != nil {
			t.Fatalf("InsertDuplicate(250,%d): %v", n, err)
		}
	}
	lower, upper := uint64(250), uint64(253)
	it, err := index.RangeIterator(&lower, true, &upper, false)
	if err != nil {
		t.Fatalf("RangeIterator: %v", err)
	}
	wantKeys := []uint64{250, 251, 252}
	for _, wantKey := range wantKeys {
		entry, ok, err := it.Next()
		if err != nil || !ok {
			t.Fatalf("Next() = %v, %v; want key %d", ok, err, wantKey)
		}
		if entry.Key != wantKey {
			t.Fatalf("range key = %d, want %d", entry.Key, wantKey)
		}
		wantRIDs := 1
		if wantKey == 250 {
			wantRIDs = 5
		}
		if len(entry.RIDs) != wantRIDs {
			t.Fatalf("key %d returned %d RIDs, want %d", wantKey, len(entry.RIDs), wantRIDs)
		}
	}
	if _, ok, err := it.Next(); err != nil || ok {
		t.Fatalf("Next() after upper bound = %v, %v; want end", ok, err)
	}

	single, err := index.RangeIterator(&lower, true, &lower, true)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok, err := single.Next()
	if err != nil || !ok || entry.Key != lower {
		t.Fatalf("inclusive point range = %v, %v, %v", entry, ok, err)
	}
	if _, ok, err := single.Next(); err != nil || ok {
		t.Fatalf("point range second Next = %v, %v; want end", ok, err)
	}

	empty, err := index.RangeIterator(&lower, false, &lower, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := empty.Next(); err != nil || ok {
		t.Fatalf("exclusive point range = %v, %v; want empty", ok, err)
	}

	unbounded, err := index.RangeIterator(nil, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		_, ok, err := unbounded.Next()
		if err != nil {
			t.Fatalf("unbounded Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	if count != 1000 {
		t.Fatalf("unbounded range returned %d keys, want 1000", count)
	}

	ridIterator, err := index.RangeRIDIterator(&lower, true, &upper, false)
	if err != nil {
		t.Fatalf("RangeRIDIterator: %v", err)
	}
	keyCounts := map[uint64]int{}
	for {
		entry, ok, err := ridIterator.Next()
		if err != nil {
			t.Fatalf("RangeRIDIterator.Next: %v", err)
		}
		if !ok {
			break
		}
		keyCounts[entry.Key]++
	}
	if keyCounts[250] != 5 || keyCounts[251] != 1 || keyCounts[252] != 1 || len(keyCounts) != 3 {
		t.Fatalf("RangeRIDIterator key counts = %v; want {250:5, 251:1, 252:1}", keyCounts)
	}
}

func TestDuplicatePostingMutationRollsBackWithWAL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("duplicate_posting_wal", "age")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err := index.Insert(7, 20, 3); err != nil {
		t.Fatal(err)
	}
	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	writes := 0
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		writes++
		if writes == 3 {
			return index.file.WriteAt(data[:len(data)/2], offset)
		}
		return index.file.WriteAt(data, offset)
	}
	if err := index.InsertDuplicate(7, 21, 4); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("InsertDuplicate error = %v, want io.ErrShortWrite (writes %d)", err, writes)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	if err := index.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	rids, err := index.FindAll(7)
	if err != nil || len(rids) != 1 || rids[0].GetPageID() != 20 || rids[0].GetSlotID() != 3 {
		t.Fatalf("FindAll after rollback = %v, %v", rids, err)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after rollback: %v", err)
	}
}

func TestValidateRejectsCorruptDuplicatePostingCycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("duplicate_posting_cycle", "age")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err := index.Insert(9, 30, 1); err != nil {
		t.Fatal(err)
	}
	if err := index.InsertDuplicate(9, 31, 1); err != nil {
		t.Fatal(err)
	}
	_, leaf, err := index.FindLeaf(9)
	if err != nil {
		t.Fatal(err)
	}
	marker := leaf.GetRID(0)
	if marker.slotID != PostingMarkerSlot {
		t.Fatalf("leaf RID slot = %d, want posting marker", marker.slotID)
	}
	posting, err := index.GetPage(marker.pageID)
	if err != nil {
		t.Fatal(err)
	}
	posting.SetNextPostingPage(posting.ID)
	if err := index.WritePage(posting); err != nil {
		t.Fatal(err)
	}
	if _, err := index.CountAll(9); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("CountAll error = %v, want posting cycle error", err)
	}
	cycleIterator, err := index.RangeRIDIterator(nil, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, ok, err := cycleIterator.Next(); err != nil || !ok {
			t.Fatalf("RangeRIDIterator.Next(%d) = ok:%t err:%v; want one posting RID", i, ok, err)
		}
	}
	if _, ok, err := cycleIterator.Next(); err == nil || !strings.Contains(err.Error(), "cycle") || ok {
		t.Fatalf("RangeRIDIterator cycle error = ok:%t err:%v", ok, err)
	}
	if err := index.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Validate error = %v, want posting cycle error", err)
	}
}
