package btree

import (
	"encoding/binary"
	"fmt"
	"math"
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
// Bytes 0 - 3:      rootPageID (4 bytes)
// Bytes 4 - 7:      numPages (4 bytes)
// Bytes 8 - 11:     nameLen (4 bytes)
// Bytes 12 - 12+nameLen: name (tableName_colName)

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

	InitMetaPage(page, tableName+"_"+colName)
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

	fi, _ := file.Stat()
	fmt.Printf("Opened index %s_%s.idx, size=%d, rootPage=%d, numPages=%d\n", tableName, colName, fi.Size(), rootPageID, numPages)
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

func InitMetaPage(p *BTreePage, name string) {
	// rootPageId 0 - > 4
	binary.LittleEndian.PutUint32(p.Data[0:4], 1)
	binary.LittleEndian.PutUint32(p.Data[4:8], 2)
	binary.LittleEndian.PutUint32(p.Data[8:12], uint32(len(name)))
	copy(p.Data[12:], []byte(name))
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
// nodes must occupy more than half and less than full capacity (except root)
// Leaf chain must be a sorted list

var visitedPages []int64

// TODO:
// make visitedPages thread safe by passing it as paramter, instead of a global variable

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
	visitedPages = make([]int64, numPages)

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

	err = ind.validateNode(rootPage, 0, math.MaxUint64, 0, 0)

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

	for i := 1; i < len(visitedPages); i++ {
		if visitedPages[i] == -1 {
			return fmt.Errorf("page %d was not visited during validation", i)
		}
	}

	return nil

}

func (ind *Index) validateNode(node *BTreePage, minKey, maxKey uint64, level int, parentPageID uint32) error {

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

	if numKeys == 0 && node.ID != ind.rootPageID {
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

		err = ind.validateNode(leftMostPage, minKey, node.GetKey(0), level+1, node.ID)

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

				err = ind.validateNode(childPage, node.GetKey(i), node.GetKey(i+1), level+1, node.ID)
			} else {
				if node.GetKey(i) > maxKey {
					return fmt.Errorf("key %d in internal node %d is out of bounds [%d, %d]", node.GetKey(i), node.ID, minKey, maxKey)
				}
				err = ind.validateNode(childPage, node.GetKey(i), maxKey, level+1, node.ID)
			}

			if err != nil {
				return err
			}

		}

	}

	return nil

}
