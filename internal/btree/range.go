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

// RangeRIDEntry contains one RID for a distinct key in a range.
type RangeRIDEntry struct {
	Key uint64
	RID RID
}

// RangeRIDIterator streams one RID at a time. Posting pages are validated as
// they are visited; callers that require duplicate-RID detection should track
// RIDs in the portion they consume.
type RangeRIDIterator struct {
	index            *Index
	leaf             *BTreePage
	keyIndex         int
	upper            uint64
	hasUpper         bool
	upperInclusive   bool
	seenLeaves       map[uint32]struct{}
	done             bool
	key              uint64
	singleRID        *RID
	postingHead      uint32
	postingPageID    uint32
	postingPage      *BTreePage
	postingIndex     int
	postingCount     int
	postingCountRead int
	seenPostings     map[uint32]struct{}
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

// RangeRIDIterator starts a streaming scan with optional inclusive or
// exclusive bounds. A nil lower or upper bound means unbounded on that side.
func (ind *Index) RangeRIDIterator(lower *uint64, lowerInclusive bool, upper *uint64, upperInclusive bool) (*RangeRIDIterator, error) {
	it := &RangeRIDIterator{index: ind, upperInclusive: upperInclusive}
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
		return nil, fmt.Errorf("start index RID range: %w", err)
	}
	if leaf.NodeType() != LeafNode {
		return nil, fmt.Errorf("RID range start page %d has node type %d", leaf.ID, leaf.NodeType())
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

// Next returns the next key/RID pair. ok is false at the end of the range.
func (it *RangeRIDIterator) Next() (entry RangeRIDEntry, ok bool, err error) {
	if it == nil || it.done {
		return RangeRIDEntry{}, false, nil
	}
	for {
		if it.singleRID != nil {
			rid := *it.singleRID
			it.singleRID = nil
			return RangeRIDEntry{Key: it.key, RID: rid}, true, nil
		}
		if it.postingPageID != 0 {
			if it.postingPage == nil {
				if _, exists := it.seenPostings[it.postingPageID]; exists {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("posting chain contains cycle at page %d", it.postingPageID)
				}
				it.seenPostings[it.postingPageID] = struct{}{}
				page, err := it.index.GetPage(it.postingPageID)
				if err != nil {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("read posting page %d: %w", it.postingPageID, err)
				}
				if page.NodeType() != PostingNode {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("posting page %d has node type %d", it.postingPageID, page.NodeType())
				}
				it.postingCount = page.PostingCount()
				if it.postingCount == 0 || it.postingCount > MaxRIDsPerPosting {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("posting page %d has invalid RID count %d", it.postingPageID, it.postingCount)
				}
				next := page.NextPostingPage()
				if next != 0 && next >= it.index.numPages {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("posting page %d has out-of-bounds next page %d", it.postingPageID, next)
				}
				it.postingPage = page
				it.postingIndex = 0
			}
			if it.postingIndex < it.postingCount {
				rid := it.postingPage.GetPostingRID(it.postingIndex)
				it.postingIndex++
				if rid.slotID == PostingMarkerSlot {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("posting page %d contains reserved marker RID", it.postingPageID)
				}
				it.postingCountRead++
				return RangeRIDEntry{Key: it.key, RID: rid}, true, nil
			}
			next := it.postingPage.NextPostingPage()
			if next == 0 {
				if it.postingCountRead < 2 {
					it.done = true
					return RangeRIDEntry{}, false, fmt.Errorf("posting chain at page %d has fewer than two RIDs", it.postingHead)
				}
				it.postingPageID = 0
				it.postingPage = nil
				it.seenPostings = nil
				it.postingCountRead = 0
				continue
			}
			it.postingPageID = next
			it.postingPage = nil
			continue
		}
		if it.leaf == nil {
			it.done = true
			return RangeRIDEntry{}, false, nil
		}
		if it.keyIndex >= it.leaf.GetNumKeys() {
			next := it.leaf.NextLeaf()
			if next == 0 {
				it.done = true
				return RangeRIDEntry{}, false, nil
			}
			if _, exists := it.seenLeaves[next]; exists {
				it.done = true
				return RangeRIDEntry{}, false, fmt.Errorf("leaf chain contains cycle at page %d", next)
			}
			leaf, err := it.index.GetPage(next)
			if err != nil {
				it.done = true
				return RangeRIDEntry{}, false, fmt.Errorf("read leaf page %d: %w", next, err)
			}
			if leaf.NodeType() != LeafNode {
				it.done = true
				return RangeRIDEntry{}, false, fmt.Errorf("leaf chain page %d has node type %d", next, leaf.NodeType())
			}
			it.seenLeaves[next] = struct{}{}
			it.leaf = leaf
			it.keyIndex = 0
			continue
		}
		key := it.leaf.GetKey(it.keyIndex)
		if it.hasUpper && (key > it.upper || key == it.upper && !it.upperInclusive) {
			it.done = true
			return RangeRIDEntry{}, false, nil
		}
		stored := it.leaf.GetRID(it.keyIndex)
		it.keyIndex++
		it.key = key
		if stored.slotID != PostingMarkerSlot {
			it.singleRID = stored
			continue
		}
		if stored.pageID == 0 {
			it.done = true
			return RangeRIDEntry{}, false, fmt.Errorf("key %d has invalid zero posting page ID", key)
		}
		it.postingHead = stored.pageID
		it.postingPageID = stored.pageID
		it.postingPage = nil
		it.postingCountRead = 0
		it.seenPostings = make(map[uint32]struct{})
	}
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
