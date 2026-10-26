package btree

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/mugiwara999/goDB/internal/wal"
)

func TestIndexWritePageDetectsShortWriteAndJournalRestoresPage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("wal_short_write", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer index.Close()
	page, err := index.GetPage(1)
	if err != nil {
		t.Fatal(err)
	}
	original := page.Data
	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		return index.file.WriteAt(data[:len(data)/2], offset)
	}
	page.Data[len(page.Data)-1] ^= 0xff
	if err := index.WritePage(page); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WritePage error = %v, want io.ErrShortWrite", err)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	restored, err := index.GetPage(1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Data[:], original[:]) {
		t.Fatal("partial index write was not restored")
	}
}

func TestNewIndexPagePartialWriteRollbackTruncatesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("wal_new_page", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer index.Close()
	originalPages := index.GetNumPages()
	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	page, err := index.NewLeafPage()
	if err != nil {
		t.Fatalf("NewLeafPage: %v", err)
	}
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		return index.file.WriteAt(data[:len(data)/2], offset)
	}
	if err := index.WritePage(page); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WritePage error = %v, want io.ErrShortWrite", err)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	if err := index.Refresh(); err != nil {
		t.Fatalf("Refresh after rollback: %v", err)
	}
	if index.GetNumPages() != originalPages {
		t.Fatalf("page count after rollback = %d, want %d", index.GetNumPages(), originalPages)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after rollback: %v", err)
	}
}

func TestFailedLeafRebalanceRestoresAllWrittenPages(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("wal_rebalance", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer index.Close()
	const keyCount = MaxKeysPerLeaf + 1
	for key := 0; key < keyCount; key++ {
		if err := index.Insert(uint64(key), uint32(key), 0); err != nil {
			t.Fatalf("Insert(%d): %v", key, err)
		}
	}
	_, leaf, err := index.FindLeaf(0)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.GetNumKeys() != MinKeysPerLeaf {
		t.Fatalf("left leaf keys before delete = %d, want minimum %d", leaf.GetNumKeys(), MinKeysPerLeaf)
	}
	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	writes := 0
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		writes++
		if writes == 2 {
			return index.file.WriteAt(data[:len(data)/2], offset)
		}
		return index.file.WriteAt(data, offset)
	}
	if err := index.Delete(0); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Delete error = %v, want io.ErrShortWrite", err)
	}
	if writes < 2 {
		t.Fatalf("page writes before injected failure = %d, want at least 2", writes)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	if err := index.Refresh(); err != nil {
		t.Fatalf("Refresh after rollback: %v", err)
	}
	if _, err := index.Find(0); err != nil {
		t.Fatalf("Find deleted key after rollback: %v", err)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after failed rebalance: %v", err)
	}
}

func TestFailedRootCollapseRestoresRootMetadataAndPages(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("wal_root_collapse", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer index.Close()
	for key := 0; key <= MaxKeysPerLeaf; key++ {
		if err := index.Insert(uint64(key), uint32(key), 0); err != nil {
			t.Fatalf("Insert(%d): %v", key, err)
		}
	}
	for key := MaxKeysPerLeaf; key >= MaxKeysPerLeaf-1; key-- {
		if err := index.Delete(uint64(key)); err != nil {
			t.Fatalf("shrink right leaf at key %d: %v", key, err)
		}
	}
	root, err := index.GetRootPage()
	if err != nil {
		t.Fatal(err)
	}
	if root.NodeType() != InternalNode || root.GetNumKeys() != 1 {
		t.Fatalf("prepared root type/count = %d/%d, want internal/1", root.NodeType(), root.GetNumKeys())
	}
	left, err := index.GetPage(root.GetChild(0))
	if err != nil {
		t.Fatal(err)
	}
	right, err := index.GetPage(root.GetChild(1))
	if err != nil {
		t.Fatal(err)
	}
	if left.GetNumKeys() != MinKeysPerLeaf || right.GetNumKeys() != MinKeysPerLeaf {
		t.Fatalf("prepared leaf counts = %d/%d, want %d/%d", left.GetNumKeys(), right.GetNumKeys(), MinKeysPerLeaf, MinKeysPerLeaf)
	}

	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	writes := 0
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		writes++
		if writes == 6 { // root metadata is written after the merged leaf and parent
			return index.file.WriteAt(data[:len(data)/2], offset)
		}
		return index.file.WriteAt(data, offset)
	}
	if err := index.Delete(0); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Delete error = %v, want io.ErrShortWrite", err)
	}
	if writes < 6 {
		t.Fatalf("page writes before injected failure = %d, want at least 6", writes)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	if err := index.Refresh(); err != nil {
		t.Fatalf("Refresh after rollback: %v", err)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after root-collapse rollback: %v", err)
	}
	root, err = index.GetRootPage()
	if err != nil {
		t.Fatal(err)
	}
	if root.NodeType() != InternalNode || root.GetNumKeys() != 1 {
		t.Fatalf("root after rollback type/count = %d/%d, want internal/1", root.NodeType(), root.GetNumKeys())
	}
	if _, err := index.Find(0); err != nil {
		t.Fatalf("Find key 0 after rollback: %v", err)
	}
	if _, err := index.Find(uint64(MaxKeysPerLeaf)); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Find previously deleted maximum key error = %v, want key not found", err)
	}
}

func TestFailedInternalMergeRestoresChildParentsAndRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	index, err := NewIndex("wal_internal_merge", "id")
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer index.Close()
	const keyCount = 49591
	for key := 0; key < keyCount; key++ {
		if err := index.Insert(uint64(key), uint32(key), 0); err != nil {
			t.Fatalf("Insert(%d): %v", key, err)
		}
	}
	root, err := index.GetRootPage()
	if err != nil {
		t.Fatal(err)
	}
	if root.NodeType() != InternalNode || root.GetNumKeys() != 1 {
		t.Fatalf("prepared root type/count = %d/%d, want internal/1", root.NodeType(), root.GetNumKeys())
	}
	leftInternal, err := index.GetPage(root.GetChild(0))
	if err != nil {
		t.Fatal(err)
	}
	rightInternal, err := index.GetPage(root.GetChild(1))
	if err != nil {
		t.Fatal(err)
	}
	if leftInternal.NodeType() != InternalNode || rightInternal.NodeType() != InternalNode || leftInternal.GetNumKeys() != MinKeysPerInternal || rightInternal.GetNumKeys() != MinKeysPerInternal {
		t.Fatalf("prepared root children type/count = %d/%d and %d/%d", leftInternal.NodeType(), leftInternal.GetNumKeys(), rightInternal.NodeType(), rightInternal.GetNumKeys())
	}
	children, _, err := readInternal(leftInternal)
	if err != nil {
		t.Fatal(err)
	}
	var deleteKey uint64
	found := false
	for pos := 0; pos+1 < len(children); pos++ {
		leftLeaf, err := index.GetPage(children[pos])
		if err != nil {
			t.Fatal(err)
		}
		rightLeaf, err := index.GetPage(children[pos+1])
		if err != nil {
			t.Fatal(err)
		}
		if leftLeaf.NodeType() == LeafNode && rightLeaf.NodeType() == LeafNode && leftLeaf.GetNumKeys() == MinKeysPerLeaf && rightLeaf.GetNumKeys() == MinKeysPerLeaf {
			deleteKey, err = leftLeaf.GetKey(0)
			if err != nil {
				t.Fatal(err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("fixture has no adjacent minimum-occupancy leaves to force an internal merge")
	}
	originalPages := index.GetNumPages()
	journal, err := wal.Begin(index.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	index.SetJournal(journal)
	writes := 0
	index.writeAtFn = func(data []byte, offset int64) (int, error) {
		writes++
		if writes == 5 { // first child-parent write during the internal-page merge
			return index.file.WriteAt(data[:len(data)/2], offset)
		}
		return index.file.WriteAt(data, offset)
	}
	if err := index.Delete(deleteKey); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Delete(%d) error = %v, want io.ErrShortWrite", deleteKey, err)
	}
	if writes < 5 {
		t.Fatalf("page writes before injected failure = %d, want at least 5", writes)
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	index.SetJournal(nil)
	index.writeAtFn = nil
	if err := index.Refresh(); err != nil {
		t.Fatalf("Refresh after rollback: %v", err)
	}
	if index.GetNumPages() != originalPages {
		t.Fatalf("page count after rollback = %d, want %d", index.GetNumPages(), originalPages)
	}
	if err := index.Validate(); err != nil {
		t.Fatalf("Validate after internal merge rollback: %v", err)
	}
	if _, err := index.Find(deleteKey); err != nil {
		t.Fatalf("Find deleted key after rollback: %v", err)
	}
	root, err = index.GetRootPage()
	if err != nil {
		t.Fatal(err)
	}
	if root.NodeType() != InternalNode || root.GetNumKeys() != 1 {
		t.Fatalf("root after rollback type/count = %d/%d, want internal/1", root.NodeType(), root.GetNumKeys())
	}
}
