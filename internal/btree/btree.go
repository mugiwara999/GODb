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

// if a key is equal to the key in an internal node, we go to the right child page

type NodeType uint8

const (
	LeafNode     NodeType = 1
	InternalNode NodeType = 0
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

func (b *BTreePage) findChild(key uint64) uint32 {

	numKeys := b.GetNumKeys()
	low := uint32(0)
	high := uint32(numKeys)

	for low < high {

		mid := (low + high) / 2

		if key < b.GetKey(int(mid)) {
			high = mid
		} else {
			low = mid + 1
		}

	}

	return low
}

func SearchLeafPage(p *BTreePage, key uint64) (*RID, bool) {

	numKeys := int(binary.LittleEndian.Uint16(p.Data[1:3]))
	low := 0
	high := numKeys

	for low < high {
		mid := low + (high-low)/2
		val := p.GetKey(mid)

		if val == key {

			id := p.GetRID(mid)
			return id, true
		} else if val < key {
			low = mid + 1
		} else {
			high = mid
		}

	}

	return nil, false

}

func (b *BTreePage) GetKey(i int) uint64 {

	HeaderSize := 11
	EntrySize := 0

	KeyOffset := LeafKeyOffset // InternalKeyOffset is also 8

	if NodeType(b.Data[0]) == LeafNode {
		EntrySize = LeafEntrySize
	} else {
		EntrySize = InternalEntrySize
	}

	offset := HeaderSize + EntrySize*i
	val := binary.LittleEndian.Uint64(b.Data[offset : offset+KeyOffset])

	return val
}

func (b *BTreePage) GetRID(i int) *RID {

	offset := LeafHeaderSize + i*LeafEntrySize

	pageID := binary.LittleEndian.Uint32(b.Data[offset+LeafKeyOffset : offset+LeafPageIdOffset])
	slotID := binary.LittleEndian.Uint16(b.Data[offset+LeafPageIdOffset : offset+LeafSlotIdOffset])

	return &RID{
		pageID: pageID,
		slotID: slotID,
	}

}

func (b *BTreePage) SetKey(i int, key uint64) {
	offset := LeafHeaderSize + i*LeafEntrySize

	binary.LittleEndian.PutUint64(b.Data[offset:offset+LeafKeyOffset], key)
}

func (b *BTreePage) SetRID(i int, id RID) {

	offset := LeafHeaderSize + i*LeafEntrySize

	binary.LittleEndian.PutUint32(b.Data[offset+LeafKeyOffset:offset+LeafPageIdOffset], id.pageID)
	binary.LittleEndian.PutUint16(b.Data[offset+LeafPageIdOffset:offset+LeafSlotIdOffset], id.slotID)

}

func (b *BTreePage) InsertIntoLeafAt(pos int, key uint64, id RID, numKeys int) {

	newData := make([]byte, 14)
	binary.LittleEndian.PutUint64(newData[0:8], key)
	binary.LittleEndian.PutUint32(newData[8:12], id.pageID)
	binary.LittleEndian.PutUint16(newData[12:14], id.slotID)

	entryStart := LeafHeaderSize + pos*LeafEntrySize
	entryEnd := LeafHeaderSize + numKeys*14
	copy(b.Data[entryStart+14:entryEnd+14], b.Data[entryStart:entryEnd])
	copy(b.Data[entryStart:entryStart+14], newData)
}

func (b *BTreePage) InsertIntoInternalAt(pos int, key uint64, childPageID uint32, numKeys int) {

	newData := make([]byte, 12)
	binary.LittleEndian.PutUint64(newData[0:8], key)
	binary.LittleEndian.PutUint32(newData[8:12], childPageID)

	entryStart := InternalHeaderSize + pos*InternalEntrySize
	entryEnd := InternalHeaderSize + numKeys*InternalEntrySize
	copy(b.Data[entryStart+InternalEntrySize:entryEnd+InternalEntrySize], b.Data[entryStart:entryEnd])
	copy(b.Data[entryStart:entryStart+InternalEntrySize], newData)
}

func (b *BTreePage) GetNumKeys() int {

	numKeys := int(binary.LittleEndian.Uint16(b.Data[1:3]))
	return numKeys
}

func (b *BTreePage) IncrementNumKeys() {

	numKeys := int(binary.LittleEndian.Uint16(b.Data[1:3]))
	numKeys++

	binary.LittleEndian.PutUint16(b.Data[1:3], uint16(numKeys))
}

func (b *BTreePage) SetNumKeys(n uint16) {
	binary.LittleEndian.PutUint16(b.Data[1:3], n)
}

func (b *BTreePage) GetParentID() uint32 {
	id := binary.LittleEndian.Uint32(b.Data[3:7])
	return id
}

func (b *BTreePage) SetParentID(n uint32) {

	binary.LittleEndian.PutUint32(b.Data[3:7], n)
}

func (b *BTreePage) NextLeaf() uint32 {

	return binary.LittleEndian.Uint32(b.Data[7:11])
}

func (b *BTreePage) SetNextLeaf(n uint32) {
	binary.LittleEndian.PutUint32(b.Data[7:11], n)
}

func (b *BTreePage) GetChild(i uint32) uint32 {
	if i == 0 {
		return binary.LittleEndian.Uint32(b.Data[7:11])
	}
	offset := InternalHeaderSize + (i-1)*InternalEntrySize
	return binary.LittleEndian.Uint32(b.Data[offset+InternalKeySize:])
}

func (b *BTreePage) moveHalfKeysToLeaf(newPage *BTreePage) {

	numKeys := b.GetNumKeys()
	half := numKeys / 2

	copy(newPage.Data[LeafHeaderSize:], b.Data[LeafHeaderSize+half*LeafEntrySize:LeafHeaderSize+numKeys*LeafEntrySize])

	b.SetNumKeys(uint16(half))
	newPage.SetNumKeys(uint16(numKeys - half))
}

func (b *BTreePage) moveHalfKeysToInternal(newPage *BTreePage) uint64 {

	numKeys := b.GetNumKeys()
	half := numKeys / 2

	ascendKey := b.GetKey(half)

	copy(newPage.Data[InternalHeaderSize:], b.Data[InternalHeaderSize+(half+1)*InternalEntrySize:InternalHeaderSize+numKeys*InternalEntrySize])

	leftChild := binary.LittleEndian.Uint32(
		b.Data[InternalHeaderSize+half*InternalEntrySize+InternalKeyOffset : InternalHeaderSize+half*InternalEntrySize+InternalPageIdOffset],
	)

	newPage.SetLeftmostChild(leftChild)

	b.SetNumKeys(uint16(half))
	newPage.SetNumKeys(uint16(numKeys - half - 1))

	return ascendKey

}

func (b *BTreePage) SetLeftmostChild(childPageID uint32) {
	binary.LittleEndian.PutUint32(b.Data[7:11], childPageID)
}

func (b *BTreePage) PrintPage() {

	numKeys := b.GetNumKeys()
	fmt.Println(numKeys)

	for i := 0; i < numKeys; i++ {
		key := b.GetKey(i)

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
