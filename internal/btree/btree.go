package btree

import (
	"encoding/binary"
	"fmt"
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

// posting node layout
// Byte 0:      nodeType (3 = posting)
// Bytes 1-2:   numRIDs
// Bytes 3-6:   nextPostingPageID
// Bytes 7+:    [pageID: 4 bytes][slotID: 2 bytes] × numRIDs

// if a key is equal to the key in an internal node, we go to the right child page

type NodeType uint8

const (
	LeafNode     NodeType = 1
	InternalNode NodeType = 0
	FreeNode     NodeType = 2
	PostingNode  NodeType = 3
)

type BTreePage struct {
	ID   uint32
	Data [PAGE_SIZE]byte
}

type RID struct {
	pageID uint32
	slotID uint16
}

const PAGE_SIZE = 4096

const (
	MaxKeysPerLeaf     = (PAGE_SIZE - LeafHeaderSize) / LeafEntrySize
	MaxKeysPerInternal = (PAGE_SIZE - InternalHeaderSize) / InternalEntrySize
	MinKeysPerLeaf     = MaxKeysPerLeaf / 2
	MinKeysPerInternal = MaxKeysPerInternal / 2
)
const (
	LeafHeaderSize   = 11
	LeafEntrySize    = 14
	LeafKeyOffset    = 8
	LeafPageIdOffset = 12
	LeafSlotIdOffset = 14
)

const (
	InternalHeaderSize   = 11
	InternalEntrySize    = 12
	InternalKeyOffset    = 8
	InternalPageIdOffset = 12
	InternalKeySize      = 8
)

const (
	PostingHeaderSize = 7
	PostingRIDSize    = 6
	MaxRIDsPerPosting = (PAGE_SIZE - PostingHeaderSize) / PostingRIDSize
	PostingMarkerSlot = ^uint16(0)
)

type Status string

const (
	Done  Status = "done"
	Split Status = "split"
)

func InitLeafPage(p *BTreePage) {
	p.Data[0] = 1
	binary.LittleEndian.PutUint16(p.Data[1:3], 0) // numKeys
	binary.LittleEndian.PutUint32(p.Data[3:7], 0) // parent page ID
}

func InitInternalPage(p *BTreePage) {
	p.Data[0] = 0
	binary.LittleEndian.PutUint16(p.Data[1:3], 0) // numKeys
	binary.LittleEndian.PutUint32(p.Data[3:7], 0) // parent page ID
}

func (b *BTreePage) NodeType() NodeType {

	return NodeType(b.Data[0])
}

func (b *BTreePage) ParentID() uint32 {
	id := binary.LittleEndian.Uint32(b.Data[3:7])
	return id
}

func (b *BTreePage) findChild(key uint64) (uint32, error) {
	if b == nil || b.NodeType() != InternalNode {
		return 0, fmt.Errorf("search child in nil or non-internal page")
	}
	numKeys := b.GetNumKeys()
	if numKeys > MaxKeysPerInternal {
		return 0, fmt.Errorf("search child in page %d: corrupt key count %d", b.ID, numKeys)
	}
	low := uint32(0)
	high := uint32(numKeys)

	for low < high {

		mid := (low + high) / 2

		midKey, err := b.GetKey(int(mid))
		if err != nil {
			return 0, err
		}
		if key < midKey {
			high = mid
		} else {
			low = mid + 1
		}

	}

	return low, nil
}

func SearchLeafPage(p *BTreePage, key uint64) (*RID, bool, error) {
	if p == nil || p.NodeType() != LeafNode {
		return nil, false, fmt.Errorf("search leaf in nil or non-leaf page")
	}

	numKeys := int(binary.LittleEndian.Uint16(p.Data[1:3]))
	if numKeys > MaxKeysPerLeaf {
		return nil, false, fmt.Errorf("search leaf page %d: corrupt key count %d", p.ID, numKeys)
	}
	low := 0
	high := numKeys

	for low < high {
		mid := low + (high-low)/2
		val, err := p.GetKey(mid)
		if err != nil {
			return nil, false, err
		}

		if val == key {

			id, err := p.GetRID(mid)
			if err != nil {
				return nil, false, err
			}
			return id, true, nil
		} else if val < key {
			low = mid + 1
		} else {
			high = mid
		}

	}

	return nil, false, nil

}

func (b *BTreePage) GetKey(i int) (uint64, error) {
	if b == nil {
		return 0, fmt.Errorf("get key: nil page")
	}
	numKeys := b.GetNumKeys()
	entrySize := 0
	maxKeys := 0
	switch b.NodeType() {
	case LeafNode:
		entrySize, maxKeys = LeafEntrySize, MaxKeysPerLeaf
	case InternalNode:
		entrySize, maxKeys = InternalEntrySize, MaxKeysPerInternal
	default:
		return 0, fmt.Errorf("get key from page %d: corrupt node type %d", b.ID, b.NodeType())
	}
	if numKeys > maxKeys {
		return 0, fmt.Errorf("get key from page %d: corrupt key count %d", b.ID, numKeys)
	}
	if i < 0 || i >= numKeys {
		return 0, fmt.Errorf("get key %d from page %d: index out of range [0, %d)", i, b.ID, numKeys)
	}
	offset := LeafHeaderSize + entrySize*i
	return binary.LittleEndian.Uint64(b.Data[offset : offset+LeafKeyOffset]), nil
}

func (b *BTreePage) GetRID(i int) (*RID, error) {
	if b == nil {
		return nil, fmt.Errorf("get RID: nil page")
	}
	if b.NodeType() != LeafNode {
		return nil, fmt.Errorf("get RID from page %d: expected leaf node, got %d", b.ID, b.NodeType())
	}
	numKeys := b.GetNumKeys()
	if numKeys > MaxKeysPerLeaf {
		return nil, fmt.Errorf("get RID from page %d: corrupt key count %d", b.ID, numKeys)
	}
	if i < 0 || i >= numKeys {
		return nil, fmt.Errorf("get RID %d from page %d: index out of range [0, %d)", i, b.ID, numKeys)
	}

	offset := LeafHeaderSize + i*LeafEntrySize

	pageID := binary.LittleEndian.Uint32(b.Data[offset+LeafKeyOffset : offset+LeafPageIdOffset])
	slotID := binary.LittleEndian.Uint16(b.Data[offset+LeafPageIdOffset : offset+LeafSlotIdOffset])

	return &RID{
		pageID: pageID,
		slotID: slotID,
	}, nil

}

func (b *BTreePage) SetKey(i int, key uint64) error {
	if err := b.validateLeafWriteIndex(i); err != nil {
		return err
	}
	offset := LeafHeaderSize + i*LeafEntrySize

	binary.LittleEndian.PutUint64(b.Data[offset:offset+LeafKeyOffset], key)
	return nil
}

func (b *BTreePage) SetRID(i int, id RID) error {
	if err := b.validateLeafWriteIndex(i); err != nil {
		return err
	}

	offset := LeafHeaderSize + i*LeafEntrySize

	binary.LittleEndian.PutUint32(b.Data[offset+LeafKeyOffset:offset+LeafPageIdOffset], id.pageID)
	binary.LittleEndian.PutUint16(b.Data[offset+LeafPageIdOffset:offset+LeafSlotIdOffset], id.slotID)
	return nil
}

func (b *BTreePage) validateLeafWriteIndex(i int) error {
	if b == nil {
		return fmt.Errorf("write leaf entry: nil page")
	}
	if b.NodeType() != LeafNode {
		return fmt.Errorf("write leaf entry on page %d: expected leaf node, got %d", b.ID, b.NodeType())
	}
	if b.GetNumKeys() > MaxKeysPerLeaf {
		return fmt.Errorf("write leaf entry on page %d: corrupt key count %d", b.ID, b.GetNumKeys())
	}
	if i < 0 || i >= MaxKeysPerLeaf {
		return fmt.Errorf("write leaf entry %d on page %d: index out of capacity [0, %d)", i, b.ID, MaxKeysPerLeaf)
	}
	return nil
}

func (b *BTreePage) InsertIntoLeafAt(pos int, key uint64, id RID, numKeys int) error {
	if b == nil {
		return fmt.Errorf("insert leaf entry: nil page")
	}
	if b.NodeType() != LeafNode {
		return fmt.Errorf("insert leaf entry on page %d: expected leaf node, got %d", b.ID, b.NodeType())
	}
	currentCount := b.GetNumKeys()
	if currentCount > MaxKeysPerLeaf || numKeys != currentCount || numKeys >= MaxKeysPerLeaf {
		return fmt.Errorf("insert leaf entry on page %d: invalid key count %d (argument %d)", b.ID, currentCount, numKeys)
	}
	if pos < 0 || pos > numKeys {
		return fmt.Errorf("insert leaf entry position %d on page %d: expected [0, %d]", pos, b.ID, numKeys)
	}

	newData := make([]byte, 14)
	binary.LittleEndian.PutUint64(newData[0:8], key)
	binary.LittleEndian.PutUint32(newData[8:12], id.pageID)
	binary.LittleEndian.PutUint16(newData[12:14], id.slotID)

	entryStart := LeafHeaderSize + pos*LeafEntrySize
	entryEnd := LeafHeaderSize + numKeys*14
	copy(b.Data[entryStart+14:entryEnd+14], b.Data[entryStart:entryEnd])
	copy(b.Data[entryStart:entryStart+14], newData)
	return nil
}

func (b *BTreePage) InsertIntoInternalAt(pos int, key uint64, childPageID uint32, numKeys int) error {
	if b == nil {
		return fmt.Errorf("insert internal entry: nil page")
	}
	if b.NodeType() != InternalNode {
		return fmt.Errorf("insert internal entry on page %d: expected internal node, got %d", b.ID, b.NodeType())
	}
	currentCount := b.GetNumKeys()
	if currentCount > MaxKeysPerInternal || numKeys != currentCount || numKeys >= MaxKeysPerInternal {
		return fmt.Errorf("insert internal entry on page %d: invalid key count %d (argument %d)", b.ID, currentCount, numKeys)
	}
	if pos < 0 || pos > numKeys {
		return fmt.Errorf("insert internal entry position %d on page %d: expected [0, %d]", pos, b.ID, numKeys)
	}

	newData := make([]byte, 12)
	binary.LittleEndian.PutUint64(newData[0:8], key)
	binary.LittleEndian.PutUint32(newData[8:12], childPageID)

	entryStart := InternalHeaderSize + pos*InternalEntrySize
	entryEnd := InternalHeaderSize + numKeys*InternalEntrySize
	copy(b.Data[entryStart+InternalEntrySize:entryEnd+InternalEntrySize], b.Data[entryStart:entryEnd])
	copy(b.Data[entryStart:entryStart+InternalEntrySize], newData)
	return nil
}

func (b *BTreePage) GetNumKeys() int {

	numKeys := int(binary.LittleEndian.Uint16(b.Data[1:3]))
	return numKeys
}

func (b *BTreePage) IncrementNumKeys() error {
	if b == nil {
		return fmt.Errorf("increment key count: nil page")
	}
	maxKeys := 0
	switch b.NodeType() {
	case LeafNode:
		maxKeys = MaxKeysPerLeaf
	case InternalNode:
		maxKeys = MaxKeysPerInternal
	default:
		return fmt.Errorf("increment key count on page %d: invalid node type %d", b.ID, b.NodeType())
	}
	numKeys := b.GetNumKeys()
	if numKeys >= maxKeys {
		return fmt.Errorf("increment key count on page %d: count %d reached maximum %d", b.ID, numKeys, maxKeys)
	}
	binary.LittleEndian.PutUint16(b.Data[1:3], uint16(numKeys+1))
	return nil
}

func (b *BTreePage) SetNumKeys(n uint16) error {
	if b == nil {
		return fmt.Errorf("set key count: nil page")
	}
	maxKeys := 0
	switch b.NodeType() {
	case LeafNode:
		maxKeys = MaxKeysPerLeaf
	case InternalNode:
		maxKeys = MaxKeysPerInternal
	default:
		return fmt.Errorf("set key count on page %d: invalid node type %d", b.ID, b.NodeType())
	}
	if int(n) > maxKeys {
		return fmt.Errorf("set key count on page %d: count %d exceeds maximum %d", b.ID, n, maxKeys)
	}
	binary.LittleEndian.PutUint16(b.Data[1:3], n)
	return nil
}

func (b *BTreePage) GetParentID() uint32 {
	id := binary.LittleEndian.Uint32(b.Data[3:7])
	return id
}

func (b *BTreePage) SetParentID(n uint32) {

	binary.LittleEndian.PutUint32(b.Data[3:7], n)
}

func (b *BTreePage) NextLeaf() (uint32, error) {
	if b == nil {
		return 0, fmt.Errorf("get next leaf: nil page")
	}
	if b.NodeType() != LeafNode {
		return 0, fmt.Errorf("get next leaf from page %d: expected leaf node, got %d", b.ID, b.NodeType())
	}
	return binary.LittleEndian.Uint32(b.Data[7:11]), nil
}

func (b *BTreePage) SetNextLeaf(n uint32) error {
	if b == nil {
		return fmt.Errorf("set next leaf: nil page")
	}
	if b.NodeType() != LeafNode {
		return fmt.Errorf("set next leaf on page %d: expected leaf node, got %d", b.ID, b.NodeType())
	}
	binary.LittleEndian.PutUint32(b.Data[7:11], n)
	return nil
}

func (b *BTreePage) PostingCount() (int, error) {
	if b == nil {
		return 0, fmt.Errorf("get posting count: nil page")
	}
	if b.NodeType() != PostingNode {
		return 0, fmt.Errorf("get posting count from page %d: expected posting node, got %d", b.ID, b.NodeType())
	}
	count := int(binary.LittleEndian.Uint16(b.Data[1:3]))
	if count > MaxRIDsPerPosting {
		return 0, fmt.Errorf("get posting count from page %d: count %d exceeds maximum %d", b.ID, count, MaxRIDsPerPosting)
	}
	return count, nil
}

func (b *BTreePage) SetPostingCount(n uint16) error {
	if b == nil {
		return fmt.Errorf("set posting count: nil page")
	}
	if b.NodeType() != PostingNode {
		return fmt.Errorf("set posting count on page %d: expected posting node, got %d", b.ID, b.NodeType())
	}
	if int(n) > MaxRIDsPerPosting {
		return fmt.Errorf("set posting count on page %d: count %d exceeds maximum %d", b.ID, n, MaxRIDsPerPosting)
	}
	binary.LittleEndian.PutUint16(b.Data[1:3], n)
	return nil
}

func (b *BTreePage) NextPostingPage() (uint32, error) {
	if b == nil {
		return 0, fmt.Errorf("get next posting page: nil page")
	}
	if b.NodeType() != PostingNode {
		return 0, fmt.Errorf("get next posting page from page %d: expected posting node, got %d", b.ID, b.NodeType())
	}
	return binary.LittleEndian.Uint32(b.Data[3:7]), nil
}

func (b *BTreePage) SetNextPostingPage(id uint32) error {
	if b == nil {
		return fmt.Errorf("set next posting page: nil page")
	}
	if b.NodeType() != PostingNode {
		return fmt.Errorf("set next posting page on page %d: expected posting node, got %d", b.ID, b.NodeType())
	}
	binary.LittleEndian.PutUint32(b.Data[3:7], id)
	return nil
}

func (b *BTreePage) GetPostingRID(i int) (RID, error) {
	if b == nil {
		return RID{}, fmt.Errorf("get posting RID: nil page")
	}
	if b.NodeType() != PostingNode {
		return RID{}, fmt.Errorf("get posting RID from page %d: expected posting node, got %d", b.ID, b.NodeType())
	}
	count, err := b.PostingCount()
	if err != nil {
		return RID{}, err
	}
	if count < 1 || count > MaxRIDsPerPosting {
		return RID{}, fmt.Errorf("get posting RID from page %d: invalid RID count %d", b.ID, count)
	}
	if i < 0 || i >= count {
		return RID{}, fmt.Errorf("get posting RID %d from page %d: index out of range [0, %d)", i, b.ID, count)
	}
	offset := PostingHeaderSize + i*PostingRIDSize
	return RID{
		pageID: binary.LittleEndian.Uint32(b.Data[offset : offset+4]),
		slotID: binary.LittleEndian.Uint16(b.Data[offset+4 : offset+6]),
	}, nil
}

func (b *BTreePage) SetPostingRID(i int, rid RID) error {
	if b == nil {
		return fmt.Errorf("set posting RID: nil page")
	}
	if b.NodeType() != PostingNode {
		return fmt.Errorf("set posting RID on page %d: expected posting node, got %d", b.ID, b.NodeType())
	}
	count, err := b.PostingCount()
	if err != nil {
		return err
	}
	if count > MaxRIDsPerPosting {
		return fmt.Errorf("set posting RID on page %d: invalid RID count %d", b.ID, count)
	}
	if i < 0 || i >= MaxRIDsPerPosting || i > count {
		return fmt.Errorf("set posting RID %d on page %d: index must be within [0, %d]", i, b.ID, count)
	}
	offset := PostingHeaderSize + i*PostingRIDSize
	binary.LittleEndian.PutUint32(b.Data[offset:offset+4], rid.pageID)
	binary.LittleEndian.PutUint16(b.Data[offset+4:offset+6], rid.slotID)
	return nil
}

func (b *BTreePage) GetChild(i uint32) (uint32, error) {
	if b == nil {
		return 0, fmt.Errorf("get child: nil page")
	}
	if b.NodeType() != InternalNode {
		return 0, fmt.Errorf("get child from page %d: expected internal node, got %d", b.ID, b.NodeType())
	}
	numKeys := b.GetNumKeys()
	if numKeys > MaxKeysPerInternal {
		return 0, fmt.Errorf("get child from page %d: corrupt key count %d", b.ID, numKeys)
	}
	if i > uint32(numKeys) {
		return 0, fmt.Errorf("get child %d from page %d: child index out of range [0, %d]", i, b.ID, numKeys)
	}
	if i == 0 {
		return binary.LittleEndian.Uint32(b.Data[7:11]), nil
	}
	offset := InternalHeaderSize + (i-1)*InternalEntrySize
	return binary.LittleEndian.Uint32(b.Data[offset+InternalKeySize:]), nil
}

func (b *BTreePage) moveHalfKeysToLeaf(newPage *BTreePage) error {
	if b == nil || newPage == nil {
		return fmt.Errorf("split leaf page: source and destination must be non-nil")
	}
	if b.NodeType() != LeafNode || newPage.NodeType() != LeafNode {
		return fmt.Errorf("split leaf page: expected leaf pages, got %d and %d", b.NodeType(), newPage.NodeType())
	}
	numKeys := b.GetNumKeys()
	if numKeys < 2 || numKeys > MaxKeysPerLeaf || newPage.GetNumKeys() != 0 {
		return fmt.Errorf("split leaf page %d: invalid source/destination counts %d and %d", b.ID, numKeys, newPage.GetNumKeys())
	}
	half := numKeys / 2

	copy(newPage.Data[LeafHeaderSize:], b.Data[LeafHeaderSize+half*LeafEntrySize:LeafHeaderSize+numKeys*LeafEntrySize])

	if err := b.SetNumKeys(uint16(half)); err != nil {
		return err
	}
	if err := newPage.SetNumKeys(uint16(numKeys - half)); err != nil {
		return err
	}
	return nil
}

func (b *BTreePage) moveHalfKeysToInternal(newPage *BTreePage) (uint64, error) {
	if b == nil || newPage == nil {
		return 0, fmt.Errorf("split internal page: source and destination must be non-nil")
	}
	if b.NodeType() != InternalNode || newPage.NodeType() != InternalNode {
		return 0, fmt.Errorf("split internal page: expected internal pages, got %d and %d", b.NodeType(), newPage.NodeType())
	}
	numKeys := b.GetNumKeys()
	if numKeys < 2 || numKeys > MaxKeysPerInternal || newPage.GetNumKeys() != 0 {
		return 0, fmt.Errorf("split internal page %d: invalid source/destination counts %d and %d", b.ID, numKeys, newPage.GetNumKeys())
	}
	half := numKeys / 2

	ascendKey, err := b.GetKey(half)
	if err != nil {
		return 0, err
	}

	copy(newPage.Data[InternalHeaderSize:], b.Data[InternalHeaderSize+(half+1)*InternalEntrySize:InternalHeaderSize+numKeys*InternalEntrySize])

	leftChild := binary.LittleEndian.Uint32(
		b.Data[InternalHeaderSize+half*InternalEntrySize+InternalKeyOffset : InternalHeaderSize+half*InternalEntrySize+InternalPageIdOffset],
	)

	if err := newPage.SetLeftmostChild(leftChild); err != nil {
		return 0, err
	}

	if err := b.SetNumKeys(uint16(half)); err != nil {
		return 0, err
	}
	if err := newPage.SetNumKeys(uint16(numKeys - half - 1)); err != nil {
		return 0, err
	}

	return ascendKey, nil

}

func (b *BTreePage) SetLeftmostChild(childPageID uint32) error {
	if b == nil {
		return fmt.Errorf("set leftmost child: nil page")
	}
	if b.NodeType() != InternalNode {
		return fmt.Errorf("set leftmost child on page %d: expected internal node, got %d", b.ID, b.NodeType())
	}
	binary.LittleEndian.PutUint32(b.Data[7:11], childPageID)
	return nil
}

func (b *BTreePage) PrintPage() {

	numKeys := b.GetNumKeys()
	fmt.Println(numKeys)

	for i := 0; i < numKeys; i++ {
		key, err := b.GetKey(i)
		if err != nil {
			fmt.Printf("<invalid key: %v> ", err)
			continue
		}

		fmt.Print(key, " ")

	}

	print("|")
}

func (id *RID) GetPageID() uint32 {
	return id.pageID
}

func (id *RID) GetSlotID() uint16 {
	return id.slotID
}
