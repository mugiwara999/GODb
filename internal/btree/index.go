package btree

import (
	"encoding/binary"
	"fmt"
	"os"
)

type Index struct {
	file       *os.File
	rootPageID uint32
	numPages   uint32
}

const (
	metaPageID = 0
)

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
// Byte 0 - 3:      rootPageID (4 bytes)
// Byte 4 - 7:      numPages (4 bytes)

func NewIndex(tableName, colName string) (*Index, error) {

	dir := os.Getenv("DATA_DIR")

	if dir == "" {
		dir = "/home/rahul/Projects/goDB/data"
	}

	file, err := os.Create(fmt.Sprintf("%s/%s_%s.idx", dir, tableName, colName))

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

	InitMetaPage(page)
	InitLeafPage(rootPage)

	n, err := file.Write(metaPage[:])

	if n < PAGE_SIZE || err != nil {
		return nil, fmt.Errorf("write meta page for column %q: %v", colName, err)
	}

	n, err = file.Write(rootPage.Data[:])

	if n < PAGE_SIZE || err != nil {
		return nil, fmt.Errorf("write root page for column %q: %v", colName, err)
	}

	return &Index{
		file:       file,
		rootPageID: 1,
		numPages:   2,
	}, nil

}

func OpenIndex(tableName, colName string) (*Index, error) {
	dir := os.Getenv("DATA_DIR")

	if dir == "" {
		dir = "/home/rahul/Projects/goDB/data"
	}

	file, err := os.OpenFile(fmt.Sprintf("%s/%s_%s.idx", dir, tableName, colName), os.O_RDWR, 0644)

	if err != nil {
		return nil, fmt.Errorf("open index file for column %q: %v", colName, err)
	}

	metaPage := [PAGE_SIZE]byte{}

	_, err = file.ReadAt(metaPage[:], 0)

	if err != nil {
		return nil, err
	}

	rootPageID := binary.LittleEndian.Uint32(metaPage[0:4])

	numPages := binary.LittleEndian.Uint32(metaPage[4:8])

	return &Index{
		file:       file,
		rootPageID: rootPageID,
		numPages:   numPages,
	}, nil

}

func (ind *Index) GetPage(id uint32) (*BTreePage, error) {
	offset := id * PAGE_SIZE

	data := [PAGE_SIZE]byte{}

	n, err := ind.file.ReadAt(data[:], int64(offset))

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

func InitMetaPage(p *BTreePage) {
	// rootPageId 0 - > 4
	binary.LittleEndian.PutUint32(p.Data[0:4], 1)
	binary.LittleEndian.PutUint32(p.Data[4:8], 2)
}

func (ind *Index) NewLeafPage() (*BTreePage, error) {

	data := [PAGE_SIZE]byte{}
	id := ind.numPages

	err := ind.SetNumPages(ind.numPages + 1)

	if err != nil {
		return nil, err
	}

	page := &BTreePage{
		ID:   id,
		Data: data,
	}

	InitLeafPage(page)

	return page, nil

}

func (ind *Index) NewInternalPage() (*BTreePage, error) {

	data := [PAGE_SIZE]byte{}
	id := ind.numPages

	err := ind.SetNumPages(ind.numPages + 1)

	if err != nil {
		return nil, err
	}
	page := &BTreePage{
		ID:   id,
		Data: data,
	}

	InitInternalPage(page)

	return page, nil

}

func (ind *Index) InsertIntoLeafPage(p *BTreePage, key uint64, id RID) (uint64, *BTreePage, error) {
	numKeys := p.GetNumKeys()
	const maxLeafKeys = (PAGE_SIZE - 11) / 14

	insertPos := 0
	for insertPos < numKeys {
		existingKey := p.GetKey(insertPos)
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
	offset := int64(p.ID) * PAGE_SIZE
	_, err := ind.file.WriteAt(p.Data[:], offset)
	return err
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
	ind.numPages = n

	if err != nil {
		return err
	}

	binary.LittleEndian.PutUint32(metaPage.Data[4:8], n)

	return ind.WritePage(metaPage)
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

	return nil, fmt.Errorf("key %d not found in leaf page %d", key, leaf.ID)

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
