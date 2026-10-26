package btree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/mugiwara999/goDB/internal/wal"
)

type Index struct {
	file           *os.File
	path           string
	journal        *wal.Journal
	writeAtFn      func([]byte, int64) (int, error)
	rootPageID     uint32
	numPages       uint32
	freePageID     uint32
	freeHeadOffset int
}

const (
	metaPageID = 0
)

var ErrKeyNotFound = errors.New("key not found")
var ErrDuplicateKey = errors.New("duplicate key")

// leaf node layout
// Byte 0:      nodeType (1 = leaf)
// Bytes 1-2:   numKeys
// Bytes 3-6:   parentPageID
// Bytes 7-10:  nextLeafPageID  (for range scans)
// Bytes 11+:   [key: 8 bytes][pageID: 4 bytes][slotID: 2 bytes] × numKeys

// internal node layout
// Byte 0:      nodeType (0 = internal)
// Bytes 1-2:   numKeys
// Bytes 3-6:   parentPageID
// Bytes 7-10:  leftmostChildPageID
// Bytes 11+:   [key: 8 bytes][rightChildPageID: 4 bytes] × numKeys

// if a key is equal to the key in an internal node, we go to the right child page

// index meta page layout
// Bytes 0 - 3:      rootPageID (4 bytes)
// Bytes 4 - 7:      numPages (4 bytes)
// Bytes 8 - 11:     nameLen (4 bytes)
// Bytes 12 - 12+nameLen: name (tableName_colName)
// Next 4 bytes after the name: free-list head page ID (0 means no free pages)

func NewIndex(tableName, colName string) (*Index, error) {

	dir := os.Getenv("DATA_DIR")

	if dir == "" {
		dir = "/home/rahul/Projects/goDB/data"
	}

	name := tableName + "_" + colName
	if 12+len(name)+4 > PAGE_SIZE {
		return nil, fmt.Errorf("index name is too long: %d bytes", len(name))
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.idx", tableName, colName))
	file, err := os.Create(path)

	if err != nil {
		return nil, fmt.Errorf("create index file for column %q: %v", colName, err)
	}

	metaPage := [PAGE_SIZE]byte{}

	page := &BTreePage{
		ID:   metaPageID,
		Data: metaPage,
	}

	rootPage := &BTreePage{
		ID:   1,
		Data: [PAGE_SIZE]byte{},
	}

	InitMetaPage(page, name)
	InitLeafPage(rootPage)

	n, err := file.Write(page.Data[:])

	if n < PAGE_SIZE || err != nil {
		return nil, fmt.Errorf("write meta page for column %q: %v", colName, err)
	}

	n, err = file.Write(rootPage.Data[:])

	if n < PAGE_SIZE || err != nil {
		return nil, fmt.Errorf("write root page for column %q: %v", colName, err)
	}

	return &Index{
		file:           file,
		path:           path,
		rootPageID:     1,
		numPages:       2,
		freeHeadOffset: 12 + len(name),
	}, nil

}

func OpenIndex(tableName, colName string) (*Index, error) {
	dir := os.Getenv("DATA_DIR")

	if dir == "" {
		dir = "/home/rahul/Projects/goDB/data"
	}

	path := filepath.Join(dir, fmt.Sprintf("%s_%s.idx", tableName, colName))
	file, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open index file for column %q: %v", colName, err)
	}
	openError := func(err error) (*Index, error) {
		_ = file.Close()
		return nil, fmt.Errorf("open index %s_%s: %w", tableName, colName, err)
	}

	metaPage := [PAGE_SIZE]byte{}

	_, err = file.ReadAt(metaPage[:], 0)

	if err != nil {
		return openError(fmt.Errorf("read metadata page: %w", err))
	}

	rootPageID := binary.LittleEndian.Uint32(metaPage[0:4])

	numPages := binary.LittleEndian.Uint32(metaPage[4:8])
	if numPages < 2 {
		return openError(fmt.Errorf("invalid page count %d", numPages))
	}
	if rootPageID == 0 || rootPageID >= numPages {
		return openError(fmt.Errorf("root page ID %d is out of bounds for %d pages", rootPageID, numPages))
	}
	nameLen := binary.LittleEndian.Uint32(metaPage[8:12])
	if uint64(nameLen)+16 > PAGE_SIZE {
		return openError(fmt.Errorf("metadata name length %d exceeds page capacity", nameLen))
	}
	freeHeadOffset := 12 + int(nameLen)
	freePageID := binary.LittleEndian.Uint32(metaPage[freeHeadOffset : freeHeadOffset+4])
	if freePageID != 0 && (freePageID >= numPages || freePageID == rootPageID) {
		return openError(fmt.Errorf("free-list head %d is out of bounds for %d pages", freePageID, numPages))
	}

	fi, err := file.Stat()
	if err != nil {
		return openError(fmt.Errorf("stat index file: %w", err))
	}
	if fi.Size()%PAGE_SIZE != 0 {
		return openError(fmt.Errorf("file size %d is not a multiple of page size %d", fi.Size(), PAGE_SIZE))
	}
	wantSize := int64(numPages) * int64(PAGE_SIZE)
	if fi.Size() != wantSize {
		return openError(fmt.Errorf("file size %d does not match metadata page count %d (expected %d bytes)", fi.Size(), numPages, wantSize))
	}

	index := &Index{
		file:           file,
		path:           path,
		rootPageID:     rootPageID,
		numPages:       numPages,
		freePageID:     freePageID,
		freeHeadOffset: freeHeadOffset,
	}
	root, err := index.GetPage(rootPageID)
	if err != nil {
		return openError(fmt.Errorf("read root page %d: %w", rootPageID, err))
	}
	if root.NodeType() != LeafNode && root.NodeType() != InternalNode {
		return openError(fmt.Errorf("root page %d has invalid node type %d", rootPageID, root.NodeType()))
	}
	if root.ParentID() != 0 {
		return openError(fmt.Errorf("root page %d has parent page %d", rootPageID, root.ParentID()))
	}
	if root.GetNumKeys() > MaxKeysPerLeaf && root.NodeType() == LeafNode || root.GetNumKeys() > MaxKeysPerInternal && root.NodeType() == InternalNode {
		return openError(fmt.Errorf("root page %d has invalid key count %d", rootPageID, root.GetNumKeys()))
	}
	if root.NodeType() == InternalNode && root.GetNumKeys() == 0 {
		return openError(fmt.Errorf("internal root page %d has no keys", rootPageID))
	}
	if freePageID != 0 {
		freePage, err := index.GetPage(freePageID)
		if err != nil {
			return openError(fmt.Errorf("read free-list head page %d: %w", freePageID, err))
		}
		if freePage.NodeType() != FreeNode {
			return openError(fmt.Errorf("free-list head page %d has node type %d", freePageID, freePage.NodeType()))
		}
	}
	fmt.Printf("Opened index %s_%s.idx, size=%d, rootPage=%d, numPages=%d\n", tableName, colName, fi.Size(), rootPageID, numPages)
	return index, nil

}

func (ind *Index) GetPage(id uint32) (*BTreePage, error) {
	if id >= ind.numPages {
		return nil, fmt.Errorf("page ID %d is out of bounds (numPages=%d)", id, ind.numPages)
	}
	offset := int64(id) * int64(PAGE_SIZE)

	data := [PAGE_SIZE]byte{}

	n, err := ind.file.ReadAt(data[:], offset)

	if n < PAGE_SIZE {
		return nil, fmt.Errorf("only %d  bytes read", n)
	}

	if err != nil {
		return nil, err
	}

	page := &BTreePage{
		ID:   id,
		Data: data,
	}

	return page, nil
}

func InitMetaPage(p *BTreePage, name string) {
	// rootPageId 0 - > 4
	binary.LittleEndian.PutUint32(p.Data[0:4], 1)
	binary.LittleEndian.PutUint32(p.Data[4:8], 2)
	binary.LittleEndian.PutUint32(p.Data[8:12], uint32(len(name)))
	copy(p.Data[12:], []byte(name))
}

func (ind *Index) NewLeafPage() (*BTreePage, error) {
	page, err := ind.allocatePage(LeafNode)
	if err != nil {
		return nil, err
	}

	InitLeafPage(page)

	return page, nil

}

func (ind *Index) NewInternalPage() (*BTreePage, error) {
	page, err := ind.allocatePage(InternalNode)
	if err != nil {
		return nil, err
	}

	InitInternalPage(page)

	return page, nil

}

func (ind *Index) allocatePage(nodeType NodeType) (*BTreePage, error) {
	var pageID uint32
	if ind.freePageID != 0 {
		pageID = ind.freePageID
		if pageID >= ind.numPages {
			return nil, fmt.Errorf("free page ID %d is out of bounds (numPages=%d)", pageID, ind.numPages)
		}
		freePage, err := ind.GetPage(pageID)
		if err != nil {
			return nil, fmt.Errorf("read free page %d: %w", pageID, err)
		}
		if freePage.NodeType() != FreeNode {
			return nil, fmt.Errorf("page %d on free list has node type %d", pageID, freePage.NodeType())
		}
		next := binary.LittleEndian.Uint32(freePage.Data[1:5])
		if next != 0 && (next >= ind.numPages || next == pageID) {
			return nil, fmt.Errorf("free page %d has invalid next page ID %d", pageID, next)
		}
		if err := ind.writeFreePageHead(next); err != nil {
			return nil, err
		}
		ind.freePageID = next
	} else {
		if ind.numPages == math.MaxUint32 {
			return nil, fmt.Errorf("index page ID space exhausted")
		}
		pageID = ind.numPages
		if err := ind.SetNumPages(ind.numPages + 1); err != nil {
			return nil, err
		}
	}

	page := &BTreePage{ID: pageID}
	if nodeType == LeafNode {
		InitLeafPage(page)
	} else {
		InitInternalPage(page)
	}
	return page, nil
}

func (ind *Index) writeFreePageHead(pageID uint32) error {
	metaPage, err := ind.GetPage(metaPageID)
	if err != nil {
		return fmt.Errorf("read index metadata for free list: %w", err)
	}
	binary.LittleEndian.PutUint32(metaPage.Data[ind.freeHeadOffset:ind.freeHeadOffset+4], pageID)
	if err := ind.WritePage(metaPage); err != nil {
		return fmt.Errorf("write index free-list head: %w", err)
	}
	return nil
}

func (ind *Index) freePage(pageID uint32) error {
	if pageID == 0 || pageID >= ind.numPages || pageID == ind.rootPageID {
		return fmt.Errorf("cannot free index page %d", pageID)
	}
	existing, err := ind.GetPage(pageID)
	if err != nil {
		return fmt.Errorf("read page %d before freeing: %w", pageID, err)
	}
	if existing.NodeType() == FreeNode {
		return fmt.Errorf("index page %d is already free", pageID)
	}
	page := &BTreePage{ID: pageID}
	page.Data[0] = byte(FreeNode)
	binary.LittleEndian.PutUint32(page.Data[1:5], ind.freePageID)
	if err := ind.WritePage(page); err != nil {
		return fmt.Errorf("write free page %d: %w", pageID, err)
	}
	if err := ind.writeFreePageHead(pageID); err != nil {
		return err
	}
	ind.freePageID = pageID
	return nil
}

func (ind *Index) InsertIntoLeafPage(p *BTreePage, key uint64, id RID) (uint64, *BTreePage, error) {
	numKeys := p.GetNumKeys()
	const maxLeafKeys = (PAGE_SIZE - 11) / 14

	insertPos := 0
	for insertPos < numKeys {
		existingKey := p.GetKey(insertPos)
		if key == existingKey {
			return 0, nil, fmt.Errorf("insert key %d: %w", key, ErrDuplicateKey)
		}
		if key < existingKey {
			break
		}
		insertPos++
	}

	p.InsertIntoLeafAt(insertPos, key, id, numKeys)

	p.IncrementNumKeys()

	if p.GetNumKeys() >= maxLeafKeys {
		newPage, ascendKey, err := ind.SplitLeafPage(p)
		return ascendKey, newPage, err

	}

	err := ind.WritePage(p)

	if err != nil {
		return 0, nil, err
	}

	return 0, nil, nil

}

func (ind *Index) SplitLeafPage(p *BTreePage) (newPage *BTreePage, ascendKey uint64, err error) {
	newPage, err = ind.NewLeafPage()

	if err != nil {
		return nil, 0, err
	}

	newPage.SetParentID(p.GetParentID())

	p.moveHalfKeysToLeaf(newPage)

	newPage.SetNextLeaf(p.NextLeaf())
	p.SetNextLeaf(newPage.ID)

	newPage.SetParentID(p.GetParentID())

	ascendKey = newPage.GetKey(0)

	err = ind.WritePage(p)

	if err != nil {
		return nil, 0, err
	}

	err = ind.WritePage(newPage)

	return newPage, ascendKey, err
}

func (ind *Index) FindLeaf(key uint64) (searchPath []uint32, page *BTreePage, err error) {

	node, err := ind.GetPage(ind.rootPageID)

	if err != nil {
		return nil, nil, err
	}

	searchPath = make([]uint32, 0)

	searchPath = append(searchPath, node.ID)

	if node.NodeType() == LeafNode {
		return searchPath, node, nil
	}

	for node.NodeType() == InternalNode {
		pos := node.findChild(key)

		pageId := node.GetChild(pos)

		node, err = ind.GetPage(pageId)

		if err != nil {
			return nil, nil, err
		}
		searchPath = append(searchPath, node.ID)
	}

	if node.NodeType() == LeafNode {
		return searchPath, node, nil
	}

	return nil, nil, fmt.Errorf("unexpected node type: %v", node.NodeType())

}

func (ind *Index) Insert(key uint64, pageID uint32, slotID uint16) error {

	searchPath, leaf, err := ind.FindLeaf(key)

	if err != nil {
		return err
	}

	id := RID{
		pageID: pageID,
		slotID: slotID,
	}

	ascendKey, newPage, err := ind.InsertIntoLeafPage(leaf, key, id)

	if err != nil {
		return err
	}

	if newPage == nil {
		return nil // newPage is already written to disk in InsertIntoLeafPage
	}

	for len(searchPath) > 1 {
		parentPageID := searchPath[len(searchPath)-2]
		parent, err := ind.GetPage(parentPageID)

		if err != nil {
			return err
		}

		ascendKey, newPage, err = ind.InsertIntoInternalPage(parent, ascendKey, newPage.ID)

		if err != nil {
			return err
		}

		if newPage == nil {
			return nil
		}

		searchPath = searchPath[:len(searchPath)-1]
	}

	root, err := ind.GetPage(ind.rootPageID)

	if err != nil {
		return err
	}
	newRoot, err := ind.CreateNewRoot(root, ascendKey, newPage)

	if err != nil {
		return err
	}

	if newRoot == nil {
		return fmt.Errorf("failed to create new root")
	}

	return nil

}

func (ind *Index) CreateNewRoot(oldRoot *BTreePage, ascendKey uint64, newPage *BTreePage) (*BTreePage, error) {
	newRoot, err := ind.NewInternalPage()

	if err != nil {
		return nil, err
	}

	newRoot.SetLeftmostChild(oldRoot.ID)
	newRoot.InsertIntoInternalAt(0, ascendKey, newPage.ID, 0)
	newRoot.IncrementNumKeys()

	oldRoot.SetParentID(newRoot.ID)
	newPage.SetParentID(newRoot.ID)

	err = ind.WritePage(oldRoot)

	if err != nil {
		return nil, err
	}

	err = ind.WritePage(newPage)
	if err != nil {
		return nil, err
	}

	err = ind.WritePage(newRoot)
	if err != nil {
		return nil, err
	}

	ind.rootPageID = newRoot.ID

	err = ind.WriteRootPageID()

	if err != nil {
		return nil, err
	}

	return newRoot, nil

}

func (ind *Index) InsertIntoInternalPage(parent *BTreePage, key uint64, childPageID uint32) (uint64, *BTreePage, error) {
	numKeys := parent.GetNumKeys()
	const maxInternalKeys = (PAGE_SIZE - 11) / 12

	insertPos := 0
	for insertPos < numKeys {
		existingKey := parent.GetKey(insertPos)
		if key < existingKey {
			break
		}
		insertPos++
	}

	childPage, err := ind.GetPage(childPageID)

	if err != nil {
		return 0, nil, err
	}

	childPage.SetParentID(parent.ID)

	parent.InsertIntoInternalAt(insertPos, key, childPageID, numKeys)

	parent.IncrementNumKeys()

	err = ind.WritePage(parent)
	if err != nil {
		return 0, nil, err
	}

	if parent.GetNumKeys() >= maxInternalKeys {
		ascendKey, newPage, err := ind.SplitInternalPage(parent)

		return ascendKey, newPage, err
	}

	return 0, nil, nil

}

func (ind *Index) SplitInternalPage(p *BTreePage) (ascendKey uint64, newPage *BTreePage, err error) {
	newPage, err = ind.NewInternalPage()

	if err != nil {
		return 0, nil, err
	}

	ascendKey = p.moveHalfKeysToInternal(newPage)

	newPage.SetParentID(p.GetParentID())

	err = ind.WritePage(p)

	if err != nil {
		return 0, nil, err
	}

	err = ind.WritePage(newPage)
	if err != nil {
		return 0, nil, err
	}

	numKeys := newPage.GetNumKeys()

	for i := 0; i <= int(numKeys); i++ {
		pageID := newPage.GetChild(uint32(i))
		childPage, err := ind.GetPage(pageID)

		if err != nil {
			return 0, nil, err
		}

		childPage.SetParentID(newPage.ID)
		err = ind.WritePage(childPage)

		if err != nil {
			return 0, nil, err
		}
	}

	return ascendKey, newPage, nil

}

func (ind *Index) WritePage(p *BTreePage) error {
	if p.ID >= ind.numPages {
		return fmt.Errorf("page ID %d is out of bounds (numPages=%d)", p.ID, ind.numPages)
	}
	if ind.journal != nil {
		if err := ind.journal.BeforePageWrite(ind.path, ind.file, p.ID, PAGE_SIZE); err != nil {
			return fmt.Errorf("journal index page %d in %q: %w", p.ID, ind.path, err)
		}
	}
	offset := int64(p.ID) * int64(PAGE_SIZE)
	writeAt := ind.file.WriteAt
	if ind.writeAtFn != nil {
		writeAt = ind.writeAtFn
	}
	n, err := writeAt(p.Data[:], offset)
	if err != nil {
		return err
	}
	if n != PAGE_SIZE {
		return fmt.Errorf("write index page %d: %w (wrote %d of %d bytes)", p.ID, io.ErrShortWrite, n, PAGE_SIZE)
	}
	return nil
}

func (ind *Index) SetJournal(j *wal.Journal) { ind.journal = j }

// Refresh reloads index metadata after another process changed this index.
// Callers must hold the table's writer lock so metadata and page reads form a
// single consistent view.
func (ind *Index) Refresh() error {
	var meta [PAGE_SIZE]byte
	if n, err := ind.file.ReadAt(meta[:], 0); err != nil || n != len(meta) {
		if err == nil {
			err = fmt.Errorf("short metadata read: %d bytes", n)
		}
		return fmt.Errorf("refresh index %q metadata: %w", ind.path, err)
	}
	rootPageID := binary.LittleEndian.Uint32(meta[0:4])
	numPages := binary.LittleEndian.Uint32(meta[4:8])
	nameLen := binary.LittleEndian.Uint32(meta[8:12])
	if numPages < 2 || rootPageID == 0 || rootPageID >= numPages {
		return fmt.Errorf("refresh index %q: invalid root/page count %d/%d", ind.path, rootPageID, numPages)
	}
	if uint64(nameLen)+16 > PAGE_SIZE {
		return fmt.Errorf("refresh index %q: invalid name length %d", ind.path, nameLen)
	}
	freeHeadOffset := 12 + int(nameLen)
	freePageID := binary.LittleEndian.Uint32(meta[freeHeadOffset : freeHeadOffset+4])
	if freePageID != 0 && (freePageID >= numPages || freePageID == rootPageID) {
		return fmt.Errorf("refresh index %q: invalid free-list head %d", ind.path, freePageID)
	}
	info, err := ind.file.Stat()
	if err != nil {
		return fmt.Errorf("refresh index %q file size: %w", ind.path, err)
	}
	wantSize := int64(numPages) * int64(PAGE_SIZE)
	if info.Size() != wantSize {
		return fmt.Errorf("refresh index %q: file size %d does not match metadata size %d", ind.path, info.Size(), wantSize)
	}
	fresh := &Index{file: ind.file, numPages: numPages}
	root, err := fresh.GetPage(rootPageID)
	if err != nil || (root.NodeType() != LeafNode && root.NodeType() != InternalNode) || root.ParentID() != 0 {
		if err == nil {
			err = fmt.Errorf("invalid root node type %d or parent %d", root.NodeType(), root.ParentID())
		}
		return fmt.Errorf("refresh index %q root page %d: %w", ind.path, rootPageID, err)
	}
	if root.NodeType() == LeafNode && root.GetNumKeys() > MaxKeysPerLeaf || root.NodeType() == InternalNode && (root.GetNumKeys() == 0 || root.GetNumKeys() > MaxKeysPerInternal) {
		return fmt.Errorf("refresh index %q: root page %d has invalid key count %d", ind.path, rootPageID, root.GetNumKeys())
	}
	if freePageID != 0 {
		free, err := fresh.GetPage(freePageID)
		if err != nil || free.NodeType() != FreeNode {
			if err == nil {
				err = fmt.Errorf("invalid free page type %d", free.NodeType())
			}
			return fmt.Errorf("refresh index %q free-list head %d: %w", ind.path, freePageID, err)
		}
	}
	ind.rootPageID = rootPageID
	ind.numPages = numPages
	ind.freePageID = freePageID
	ind.freeHeadOffset = freeHeadOffset
	return nil
}

func (ind *Index) GetRootPage() (*BTreePage, error) {
	return ind.GetPage(ind.rootPageID)
}

func (ind *Index) WriteRootPageID() error {
	metaPage, err := ind.GetPage(metaPageID)

	if err != nil {
		return err
	}

	binary.LittleEndian.PutUint32(metaPage.Data[0:4], ind.rootPageID)

	return ind.WritePage(metaPage)
}

func (ind *Index) SetNumPages(n uint32) error {
	metaPage, err := ind.GetPage(metaPageID)
	if err != nil {
		return err
	}

	binary.LittleEndian.PutUint32(metaPage.Data[4:8], n)

	if err := ind.WritePage(metaPage); err != nil {
		return err
	}
	ind.numPages = n
	return nil
}

func (ind *Index) Close() error {
	return ind.file.Close()
}

func (ind *Index) Find(key uint64) (*RID, error) {

	_, leaf, err := ind.FindLeaf(key)

	if err != nil {
		return nil, err
	}

	numKeys := leaf.GetNumKeys()
	low, high := 0, int(numKeys)-1

	for low <= high {
		mid := low + (high-low)/2
		midKey := leaf.GetKey(mid)

		if midKey == key {
			return leaf.GetRID(mid), nil
		}

		if midKey < key {
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	return nil, fmt.Errorf("key %d not found in leaf page %d: %w", key, leaf.ID, ErrKeyNotFound)

}

// UpdateRID changes the stored location for an existing key without adding a
// second key to the tree.
func (ind *Index) UpdateRID(key uint64, pageID uint32, slotID uint16) error {
	_, leaf, err := ind.FindLeaf(key)
	if err != nil {
		return fmt.Errorf("find key %d for RID update: %w", key, err)
	}
	for i := 0; i < leaf.GetNumKeys(); i++ {
		if leaf.GetKey(i) != key {
			continue
		}
		leaf.SetRID(i, RID{pageID: pageID, slotID: slotID})
		if err := ind.WritePage(leaf); err != nil {
			return fmt.Errorf("write RID update for key %d: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("key %d not found for RID update", key)
}

// Delete removes an existing key and rebalances underfull nodes by borrowing
// from a sibling or merging pages. Merged pages are linked into the free list.
func (ind *Index) Delete(key uint64) error {
	_, leaf, err := ind.FindLeaf(key)
	if err != nil {
		return fmt.Errorf("find key %d for deletion: %w", key, err)
	}

	numKeys := leaf.GetNumKeys()
	for i := 0; i < numKeys; i++ {
		if leaf.GetKey(i) != key {
			continue
		}

		removeLeafEntry(leaf, i)
		if leaf.ID == ind.rootPageID || leaf.GetNumKeys() >= MinKeysPerLeaf {
			return ind.WritePage(leaf)
		}
		return ind.rebalanceLeaf(leaf)
	}

	return fmt.Errorf("key %d: %w", key, ErrKeyNotFound)
}

func removeLeafEntry(leaf *BTreePage, pos int) {
	numKeys := leaf.GetNumKeys()
	start := LeafHeaderSize + pos*LeafEntrySize
	end := LeafHeaderSize + numKeys*LeafEntrySize
	copy(leaf.Data[start:end-LeafEntrySize], leaf.Data[start+LeafEntrySize:end])
	clear(leaf.Data[end-LeafEntrySize : end])
	leaf.SetNumKeys(uint16(numKeys - 1))
}

func readInternal(p *BTreePage) ([]uint32, []uint64) {
	n := p.GetNumKeys()
	children := make([]uint32, n+1)
	keys := make([]uint64, n)
	for i := range children {
		children[i] = p.GetChild(uint32(i))
	}
	for i := range keys {
		keys[i] = p.GetKey(i)
	}
	return children, keys
}

func writeInternal(p *BTreePage, children []uint32, keys []uint64) error {
	if len(children) != len(keys)+1 || len(keys) > MaxKeysPerInternal {
		return fmt.Errorf("invalid internal node contents: %d children, %d keys", len(children), len(keys))
	}
	clear(p.Data[InternalHeaderSize:])
	p.SetLeftmostChild(children[0])
	for i, key := range keys {
		offset := InternalHeaderSize + i*InternalEntrySize
		binary.LittleEndian.PutUint64(p.Data[offset:offset+8], key)
		binary.LittleEndian.PutUint32(p.Data[offset+8:offset+12], children[i+1])
	}
	p.SetNumKeys(uint16(len(keys)))
	return nil
}

func setInternalKey(p *BTreePage, pos int, key uint64) {
	offset := InternalHeaderSize + pos*InternalEntrySize
	binary.LittleEndian.PutUint64(p.Data[offset:offset+8], key)
}

func removeChild(children []uint32, keys []uint64, childPos int) ([]uint32, []uint64) {
	children = append(children[:childPos], children[childPos+1:]...)
	keyPos := childPos - 1
	if keyPos < 0 {
		keyPos = 0
	}
	keys = append(keys[:keyPos], keys[keyPos+1:]...)
	return children, keys
}

func childPosition(parent *BTreePage, childID uint32) (int, error) {
	children, _ := readInternal(parent)
	for pos, id := range children {
		if id == childID {
			return pos, nil
		}
	}
	return 0, fmt.Errorf("page %d is not a child of parent %d", childID, parent.ID)
}

func (ind *Index) subtreeMin(pageID uint32) (uint64, error) {
	page, err := ind.GetPage(pageID)
	if err != nil {
		return 0, err
	}
	for page.NodeType() == InternalNode {
		page, err = ind.GetPage(page.GetChild(0))
		if err != nil {
			return 0, err
		}
	}
	if page.NodeType() != LeafNode || page.GetNumKeys() == 0 {
		return 0, fmt.Errorf("subtree rooted at page %d has no minimum key", pageID)
	}
	return page.GetKey(0), nil
}

func (ind *Index) rebalanceLeaf(leaf *BTreePage) error {
	parent, err := ind.GetPage(leaf.ParentID())
	if err != nil {
		return err
	}
	children, keys := readInternal(parent)
	pos, err := childPosition(parent, leaf.ID)
	if err != nil {
		return err
	}
	leafCount := leaf.GetNumKeys()

	if pos > 0 {
		left, err := ind.GetPage(children[pos-1])
		if err != nil {
			return err
		}
		if left.GetNumKeys() > MinKeysPerLeaf {
			last := left.GetNumKeys() - 1
			key, rid := left.GetKey(last), left.GetRID(last)
			removeLeafEntry(left, last)
			leaf.InsertIntoLeafAt(0, key, *rid, leafCount)
			leaf.SetNumKeys(uint16(leafCount + 1))
			setInternalKey(parent, pos-1, key)
			for _, page := range []*BTreePage{left, leaf, parent} {
				if err := ind.WritePage(page); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if pos+1 < len(children) {
		right, err := ind.GetPage(children[pos+1])
		if err != nil {
			return err
		}
		if right.GetNumKeys() > MinKeysPerLeaf {
			key, rid := right.GetKey(0), right.GetRID(0)
			leaf.InsertIntoLeafAt(leafCount, key, *rid, leafCount)
			leaf.SetNumKeys(uint16(leafCount + 1))
			removeLeafEntry(right, 0)
			setInternalKey(parent, pos, right.GetKey(0))
			for _, page := range []*BTreePage{leaf, right, parent} {
				if err := ind.WritePage(page); err != nil {
					return err
				}
			}
			return nil
		}
	}

	if pos > 0 {
		left, err := ind.GetPage(children[pos-1])
		if err != nil {
			return err
		}
		leftCount := left.GetNumKeys()
		copy(left.Data[LeafHeaderSize+leftCount*LeafEntrySize:], leaf.Data[LeafHeaderSize:LeafHeaderSize+leafCount*LeafEntrySize])
		left.SetNumKeys(uint16(leftCount + leafCount))
		left.SetNextLeaf(leaf.NextLeaf())
		children, keys = removeChild(children, keys, pos)
		if err := writeInternal(parent, children, keys); err != nil {
			return err
		}
		if err := ind.WritePage(left); err != nil {
			return err
		}
		if err := ind.WritePage(parent); err != nil {
			return err
		}
		if err := ind.freePage(leaf.ID); err != nil {
			return err
		}
		return ind.rebalanceInternal(parent)
	}

	right, err := ind.GetPage(children[pos+1])
	if err != nil {
		return err
	}
	rightCount := right.GetNumKeys()
	copy(leaf.Data[LeafHeaderSize+leafCount*LeafEntrySize:], right.Data[LeafHeaderSize:LeafHeaderSize+rightCount*LeafEntrySize])
	leaf.SetNumKeys(uint16(leafCount + rightCount))
	leaf.SetNextLeaf(right.NextLeaf())
	children, keys = removeChild(children, keys, pos+1)
	if err := writeInternal(parent, children, keys); err != nil {
		return err
	}
	if err := ind.WritePage(leaf); err != nil {
		return err
	}
	if err := ind.WritePage(parent); err != nil {
		return err
	}
	if err := ind.freePage(right.ID); err != nil {
		return err
	}
	return ind.rebalanceInternal(parent)
}

func (ind *Index) rebalanceInternal(node *BTreePage) error {
	if node.ID == ind.rootPageID {
		if node.GetNumKeys() == 0 {
			childID := node.GetChild(0)
			child, err := ind.GetPage(childID)
			if err != nil {
				return err
			}
			child.SetParentID(0)
			if err := ind.WritePage(child); err != nil {
				return err
			}
			oldRootID := ind.rootPageID
			ind.rootPageID = childID
			if err := ind.WriteRootPageID(); err != nil {
				ind.rootPageID = oldRootID
				return err
			}
			return ind.freePage(oldRootID)
		}
		return ind.WritePage(node)
	}
	if node.GetNumKeys() >= MinKeysPerInternal {
		return ind.WritePage(node)
	}

	parent, err := ind.GetPage(node.ParentID())
	if err != nil {
		return err
	}
	parentChildren, parentKeys := readInternal(parent)
	pos, err := childPosition(parent, node.ID)
	if err != nil {
		return err
	}
	children, keys := readInternal(node)

	if pos > 0 {
		left, err := ind.GetPage(parentChildren[pos-1])
		if err != nil {
			return err
		}
		leftChildren, leftKeys := readInternal(left)
		if len(leftKeys) > MinKeysPerInternal {
			movedChild := leftChildren[len(leftChildren)-1]
			newParentKey, err := ind.subtreeMin(movedChild)
			if err != nil {
				return err
			}
			newNodeKey, err := ind.subtreeMin(children[0])
			if err != nil {
				return err
			}
			leftChildren = leftChildren[:len(leftChildren)-1]
			leftKeys = leftKeys[:len(leftKeys)-1]
			children = append([]uint32{movedChild}, children...)
			keys = append([]uint64{newNodeKey}, keys...)
			parentKeys[pos-1] = newParentKey
			if err := writeInternal(left, leftChildren, leftKeys); err != nil {
				return err
			}
			if err := writeInternal(node, children, keys); err != nil {
				return err
			}
			if err := writeInternal(parent, parentChildren, parentKeys); err != nil {
				return err
			}
			moved, err := ind.GetPage(movedChild)
			if err != nil {
				return err
			}
			moved.SetParentID(node.ID)
			if err := ind.WritePage(moved); err != nil {
				return err
			}
			for _, page := range []*BTreePage{left, node, parent} {
				if err := ind.WritePage(page); err != nil {
					return err
				}
			}
			return ind.rebalanceInternal(node)
		}
	}
	if pos+1 < len(parentChildren) {
		right, err := ind.GetPage(parentChildren[pos+1])
		if err != nil {
			return err
		}
		rightChildren, rightKeys := readInternal(right)
		if len(rightKeys) > MinKeysPerInternal {
			movedChild := rightChildren[0]
			newParentKey, err := ind.subtreeMin(rightChildren[1])
			if err != nil {
				return err
			}
			newNodeKey, err := ind.subtreeMin(movedChild)
			if err != nil {
				return err
			}
			children = append(children, movedChild)
			keys = append(keys, newNodeKey)
			rightChildren = rightChildren[1:]
			rightKeys = rightKeys[1:]
			parentKeys[pos] = newParentKey
			if err := writeInternal(node, children, keys); err != nil {
				return err
			}
			if err := writeInternal(right, rightChildren, rightKeys); err != nil {
				return err
			}
			if err := writeInternal(parent, parentChildren, parentKeys); err != nil {
				return err
			}
			moved, err := ind.GetPage(movedChild)
			if err != nil {
				return err
			}
			moved.SetParentID(node.ID)
			if err := ind.WritePage(moved); err != nil {
				return err
			}
			for _, page := range []*BTreePage{node, right, parent} {
				if err := ind.WritePage(page); err != nil {
					return err
				}
			}
			return ind.rebalanceInternal(node)
		}
	}

	if pos > 0 {
		left, err := ind.GetPage(parentChildren[pos-1])
		if err != nil {
			return err
		}
		leftChildren, leftKeys := readInternal(left)
		bridgeKey, err := ind.subtreeMin(children[0])
		if err != nil {
			return err
		}
		leftKeys = append(leftKeys, bridgeKey)
		leftKeys = append(leftKeys, keys...)
		leftChildren = append(leftChildren, children...)
		if err := writeInternal(left, leftChildren, leftKeys); err != nil {
			return err
		}
		for _, childID := range children {
			child, err := ind.GetPage(childID)
			if err != nil {
				return err
			}
			child.SetParentID(left.ID)
			if err := ind.WritePage(child); err != nil {
				return err
			}
		}
		parentChildren, parentKeys = removeChild(parentChildren, parentKeys, pos)
		if err := writeInternal(parent, parentChildren, parentKeys); err != nil {
			return err
		}
		if err := ind.WritePage(left); err != nil {
			return err
		}
		if err := ind.WritePage(parent); err != nil {
			return err
		}
		if err := ind.freePage(node.ID); err != nil {
			return err
		}
		return ind.rebalanceInternal(parent)
	}

	right, err := ind.GetPage(parentChildren[pos+1])
	if err != nil {
		return err
	}
	rightChildren, rightKeys := readInternal(right)
	bridgeKey, err := ind.subtreeMin(rightChildren[0])
	if err != nil {
		return err
	}
	keys = append(keys, bridgeKey)
	keys = append(keys, rightKeys...)
	children = append(children, rightChildren...)
	if err := writeInternal(node, children, keys); err != nil {
		return err
	}
	for _, childID := range rightChildren {
		child, err := ind.GetPage(childID)
		if err != nil {
			return err
		}
		child.SetParentID(node.ID)
		if err := ind.WritePage(child); err != nil {
			return err
		}
	}
	parentChildren, parentKeys = removeChild(parentChildren, parentKeys, pos+1)
	if err := writeInternal(parent, parentChildren, parentKeys); err != nil {
		return err
	}
	if err := ind.WritePage(node); err != nil {
		return err
	}
	if err := ind.WritePage(parent); err != nil {
		return err
	}
	if err := ind.freePage(right.ID); err != nil {
		return err
	}
	return ind.rebalanceInternal(parent)
}

func (ind *Index) PrintTree() error {
	root, err := ind.GetRootPage()

	if err != nil {
		return err
	}

	queue := make([]*BTreePage, 0)

	queue = append(queue, root)

	for len(queue) > 0 {
		current := queue[0]

		l := len(queue)

		for i := range l {
			queue[i].PrintPage()
		}
		fmt.Println()

		queue = queue[1:]
		queue = append(queue, ind.GetChildren(current)...)

	}

	return nil

}

func (ind *Index) GetChildren(b *BTreePage) []*BTreePage {
	if b.NodeType() == LeafNode {
		return nil
	}

	numKeys := b.GetNumKeys()
	children := make([]*BTreePage, numKeys+1)

	for i := 0; i <= numKeys; i++ {
		childID := b.GetChild(uint32(i))
		page, _ := ind.GetPage(childID)

		if page == nil {
			continue
		}
		children[i] = page
	}

	return children
}

func (ind *Index) Name() (string, error) {
	metaPage, err := ind.GetPage(metaPageID)

	if err != nil {
		return "", fmt.Errorf("failed to read meta page: %v", err)
	}

	nameLen := binary.LittleEndian.Uint32(metaPage.Data[8:12])

	name := string(metaPage.Data[12 : 12+nameLen])

	return name, nil
}

func (ind *Index) GetNumPages() uint32 {
	return ind.numPages
}

func (ind *Index) FindLeftMostLeaf() (*BTreePage, error) {
	root, err := ind.GetRootPage()

	if err != nil {
		return nil, err
	}

	for root.NodeType() != LeafNode {

		if root.GetNumKeys() == 0 {
			return nil, fmt.Errorf("internal node %d has no keys", root.ID)
		}

		root, err = ind.GetPage(root.GetChild(0))

		if err != nil {
			return nil, err
		}
	}

	return root, nil
}

// Invariants:
// Every Leaf is at same level
// Leaf nodes must have keys in sorted order.
// Children of internal nodes must have keys in sorted order.
// max(left subtree) < separator
// min(right subtree) >= separator
// parent pointers must be correct
// pageID should be less than numPages
// internal nodes must occupy more than half and less than full capacity (except root)
// Leaf chain must be a sorted list

// -1 represents unvisited
// else number represents level

func (ind *Index) Validate() error {

	_, err := ind.GetPage(metaPageID)

	if err != nil {
		return fmt.Errorf("failed to read meta page: %v", err)
	}

	rootPage, err := ind.GetRootPage()

	if err != nil {
		return err
	}

	numPages := ind.GetNumPages()
	visitedPages := make([]int64, numPages)

	for i := range visitedPages {
		visitedPages[i] = -1
	}

	if rootPage.ID != ind.rootPageID {
		return fmt.Errorf("root page ID mismatch: expected %d, got %d", ind.rootPageID, rootPage.ID)
	}

	if rootPage.ID == 0 {
		return fmt.Errorf("root page ID is 0, index is empty")
	}

	if rootPage.ParentID() != 0 {
		return fmt.Errorf("root page has non-zero parent ID: %d", rootPage.ParentID())
	}

	err = ind.validateNode(rootPage, 0, math.MaxUint64, 0, 0, visitedPages)

	if err != nil {
		return err
	}

	leaf, err := ind.FindLeftMostLeaf()

	if err != nil {
		return err
	}

	visitedLeaves := make(map[uint32]bool)

	if visitedPages[leaf.ID] == -1 {
		return fmt.Errorf("leftmost leaf page %d was not visited during validation", leaf.ID)
	}

	leafLevel := visitedPages[leaf.ID] // used to check if all leaf nodes are on same level

	if err != nil {
		return fmt.Errorf("failed to find leftmost leaf: %v", err)
	}

	for leaf != nil {
		if leaf.NodeType() != LeafNode {
			return fmt.Errorf("expected leaf node, got %v", leaf.NodeType())
		}

		if visitedPages[leaf.ID] == -1 {
			return fmt.Errorf("leaf page %d was not visited during validation", leaf.ID)
		}

		if visitedPages[leaf.ID] != leafLevel {
			return fmt.Errorf("leaf page %d is at level %d, expected level %d", leaf.ID, visitedPages[leaf.ID], leafLevel)
		}

		if visitedLeaves[leaf.ID] {
			return fmt.Errorf("leaf page %d is visited more than once in the leaf chain", leaf.ID)
		}

		visitedLeaves[leaf.ID] = true

		nextLeafID := leaf.NextLeaf()

		if nextLeafID == 0 {
			break
		}
		nextLeaf, err := ind.GetPage(nextLeafID)

		if err != nil {
			return err
		}

		if nextLeaf.NodeType() != LeafNode {
			return fmt.Errorf("expected leaf node, got %v", leaf.NodeType())
		}

		if leaf.GetNumKeys() > 0 && nextLeaf.GetNumKeys() > 0 {
			if leaf.GetKey(leaf.GetNumKeys()-1) > nextLeaf.GetKey(0) {
				return fmt.Errorf("leaf chain not sorted …")
			}
		}

		leaf = nextLeaf

	}

	for freeID := ind.freePageID; freeID != 0; {
		if freeID >= ind.numPages {
			return fmt.Errorf("free page ID %d is out of bounds (numPages=%d)", freeID, ind.numPages)
		}
		if visitedPages[freeID] != -1 {
			return fmt.Errorf("free page %d is already reachable or appears more than once", freeID)
		}
		freePage, err := ind.GetPage(freeID)
		if err != nil {
			return fmt.Errorf("read free page %d: %w", freeID, err)
		}
		if freePage.NodeType() != FreeNode {
			return fmt.Errorf("free list page %d has node type %d", freeID, freePage.NodeType())
		}
		visitedPages[freeID] = -2
		freeID = binary.LittleEndian.Uint32(freePage.Data[1:5])
	}

	for i := 1; i < len(visitedPages); i++ {
		if visitedPages[i] == -1 {
			return fmt.Errorf("page %d is neither reachable nor free", i)
		}
	}

	return nil

}

func (ind *Index) validateNode(node *BTreePage, minKey, maxKey uint64, level int, parentPageID uint32, visitedPages []int64) error {

	if node == nil {
		return fmt.Errorf("node is nil")
	}

	if node.ID >= ind.numPages {
		return fmt.Errorf("node ID %d is out of bounds (numPages=%d)", node.ID, ind.numPages)
	}

	if node.ParentID() != parentPageID {
		return fmt.Errorf("node %d has incorrect parent ID: expected %d, got %d", node.ID, parentPageID, node.ParentID())
	}

	if visitedPages[node.ID] != -1 {
		return fmt.Errorf("node %d has already been visited at level %d, current level %d", node.ID, visitedPages[node.ID], level)
	}

	visitedPages[node.ID] = int64(level)

	numKeys := node.GetNumKeys()
	if node.NodeType() != LeafNode && node.NodeType() != InternalNode {
		return fmt.Errorf("node %d has invalid node type %d", node.ID, node.NodeType())
	}

	if numKeys == 0 && node.NodeType() == InternalNode && node.ID != ind.rootPageID {
		return fmt.Errorf("node %d has no keys", node.ID)
	}

	if node.NodeType() == LeafNode {
		if numKeys < MinKeysPerLeaf && node.ID != ind.rootPageID {
			return fmt.Errorf("leaf node %d has too few keys: %d", node.ID, numKeys)
		}
		if numKeys > MaxKeysPerLeaf {
			return fmt.Errorf("leaf node %d has too many keys: %d", node.ID, numKeys)
		}

		for i := 0; i < int(numKeys); i++ {
			key := node.GetKey(i)
			if key < minKey || key >= maxKey {
				return fmt.Errorf("key %d in leaf node %d is out of bounds [%d, %d]", key, node.ID, minKey, maxKey)
			}

			if i > 0 {
				prevKey := node.GetKey(i - 1)
				if key <= prevKey {
					return fmt.Errorf("keys in leaf node %d are not sorted: %d <= %d", node.ID, key, prevKey)
				}
			}
		}
	} else if node.NodeType() == InternalNode {
		if numKeys < MinKeysPerInternal && node.ID != ind.rootPageID {
			return fmt.Errorf("internal node %d has too few keys: %d", node.ID, numKeys)
		}

		if numKeys > MaxKeysPerInternal {
			return fmt.Errorf("internal node %d has too many keys: %d", node.ID, numKeys)
		}

		leftMostChild := node.GetChild(0)
		leftMostPage, err := ind.GetPage(leftMostChild)

		if err != nil {
			return fmt.Errorf("failed to get leftmost child page %d of internal node %d: %v", leftMostChild, node.ID, err)
		}

		if node.GetKey(0) < minKey {
			return fmt.Errorf("key %d in internal node %d is out of bounds [%d, %d]", node.GetKey(0), node.ID, minKey, maxKey)
		}

		err = ind.validateNode(leftMostPage, minKey, node.GetKey(0), level+1, node.ID, visitedPages)

		if err != nil {
			return err
		}

		for i := 0; i < int(numKeys); i++ {
			child := node.GetChild(uint32(i + 1))
			childPage, err := ind.GetPage(child)

			if err != nil {
				return fmt.Errorf("failed to get child page %d of internal node %d: %v", child, node.ID, err)
			}

			if i < int(numKeys)-1 {
				if node.GetKey(i) >= node.GetKey(i+1) {
					return fmt.Errorf("keys in internal node %d are not sorted: %d >= %d", node.ID, node.GetKey(i), node.GetKey(i+1))
				}

				err = ind.validateNode(childPage, node.GetKey(i), node.GetKey(i+1), level+1, node.ID, visitedPages)
			} else {
				if node.GetKey(i) > maxKey {
					return fmt.Errorf("key %d in internal node %d is out of bounds [%d, %d]", node.GetKey(i), node.ID, minKey, maxKey)
				}
				err = ind.validateNode(childPage, node.GetKey(i), maxKey, level+1, node.ID, visitedPages)
			}

			if err != nil {
				return err
			}

		}

	}

	return nil

}
