package btree

import "fmt"

// RangeIterator streams distinct keys and their associated RIDs in key order.
// Callers must exclude concurrent index mutations for the lifetime of the
// iterator.
type RangeIterator struct {
	index          *Index
	leaf           *BTreePage
	keyIndex       int
	upper          uint64
	hasUpper       bool
	upperInclusive bool
	seenLeaves     map[uint32]struct{}
	done           bool
}

// RangeEntry contains one distinct key and all of its row locations.
type RangeEntry struct {
	Key  uint64
	RIDs []RID
}

// RangeIterator starts a streaming scan with optional inclusive or exclusive
// bounds. A nil lower or upper bound means unbounded on that side.
func (ind *Index) RangeIterator(lower *uint64, lowerInclusive bool, upper *uint64, upperInclusive bool) (*RangeIterator, error) {
	it := &RangeIterator{index: ind, upperInclusive: upperInclusive}
	if upper != nil {
		it.upper = *upper
		it.hasUpper = true
	}
	if lower != nil && upper != nil {
		if *lower > *upper || *lower == *upper && (!lowerInclusive || !upperInclusive) {
			it.done = true
			return it, nil
		}
	}
	var leaf *BTreePage
	var err error
	if lower == nil {
		leaf, err = ind.FindLeftMostLeaf()
	} else {
		_, leaf, err = ind.FindLeaf(*lower)
	}
	if err != nil {
		return nil, fmt.Errorf("start index range: %w", err)
	}
	if leaf.NodeType() != LeafNode {
		return nil, fmt.Errorf("range start page %d has node type %d", leaf.ID, leaf.NodeType())
	}
	it.leaf = leaf
	it.seenLeaves = map[uint32]struct{}{leaf.ID: {}}
	if lower != nil {
		low, high := 0, leaf.GetNumKeys()
		for low < high {
			mid := low + (high-low)/2
			key := leaf.GetKey(mid)
			before := key < *lower || key == *lower && !lowerInclusive
			if before {
				low = mid + 1
			} else {
				high = mid
			}
		}
		it.keyIndex = low
	}
	return it, nil
}

// Next returns the next distinct key and its RIDs. ok is false at the end.
func (it *RangeIterator) Next() (entry RangeEntry, ok bool, err error) {
	if it == nil || it.done {
		return RangeEntry{}, false, nil
	}
	for it.leaf != nil {
		for it.keyIndex < it.leaf.GetNumKeys() {
			key := it.leaf.GetKey(it.keyIndex)
			if it.hasUpper && (key > it.upper || key == it.upper && !it.upperInclusive) {
				it.done = true
				return RangeEntry{}, false, nil
			}
			stored := it.leaf.GetRID(it.keyIndex)
			it.keyIndex++
			var rids []RID
			if stored.slotID == PostingMarkerSlot {
				rids, err = it.index.readPostingRIDs(stored.pageID)
				if err != nil {
					it.done = true
					return RangeEntry{}, false, fmt.Errorf("read postings for key %d: %w", key, err)
				}
			} else {
				rids = []RID{*stored}
			}
			return RangeEntry{Key: key, RIDs: rids}, true, nil
		}
		next := it.leaf.NextLeaf()
		if next == 0 {
			it.done = true
			return RangeEntry{}, false, nil
		}
		if _, exists := it.seenLeaves[next]; exists {
			it.done = true
			return RangeEntry{}, false, fmt.Errorf("leaf chain contains cycle at page %d", next)
		}
		leaf, err := it.index.GetPage(next)
		if err != nil {
			it.done = true
			return RangeEntry{}, false, fmt.Errorf("read leaf page %d: %w", next, err)
		}
		if leaf.NodeType() != LeafNode {
			it.done = true
			return RangeEntry{}, false, fmt.Errorf("leaf chain page %d has node type %d", next, leaf.NodeType())
		}
		it.seenLeaves[next] = struct{}{}
		it.leaf = leaf
		it.keyIndex = 0
	}
	it.done = true
	return RangeEntry{}, false, nil
}
