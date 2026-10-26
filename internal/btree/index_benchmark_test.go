package btree

import "testing"

func benchmarkIndex(b *testing.B, name string) *Index {
	b.Helper()
	dir := b.TempDir()
	b.Setenv("DATA_DIR", dir)
	idx, err := NewIndex("bench", name)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := idx.Close(); err != nil {
			b.Errorf("close benchmark index: %v", err)
		}
	})
	return idx
}

func BenchmarkIndexInsert(b *testing.B) {
	idx := benchmarkIndex(b, "insert")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Insert(uint64(i), uint32(i), uint16(i)); err != nil {
			b.Fatalf("Insert(%d): %v", i, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(idx.GetNumPages()), "pages")
}

func BenchmarkIndexFind(b *testing.B) {
	const keyCount = 10000
	idx := benchmarkIndex(b, "find")
	for i := 0; i < keyCount; i++ {
		if err := idx.Insert(uint64(i), uint32(i), uint16(i)); err != nil {
			b.Fatalf("seed Insert(%d): %v", i, err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := uint64(i % keyCount)
		if _, err := idx.Find(key); err != nil {
			b.Fatalf("Find(%d): %v", key, err)
		}
	}
}

func BenchmarkIndexRangeDuplicateRIDs(b *testing.B) {
	for _, totalRIDs := range []int{10000, 100000} {
		size := "10k"
		if totalRIDs == 100000 {
			size = "100k"
		}
		for _, streaming := range []bool{false, true} {
			name := "batch"
			if streaming {
				name = "stream"
			}
			b.Run(size+"/"+name, func(b *testing.B) {
				idx := benchmarkDuplicateIndex(b, totalRIDs)
				lower, upper := uint64(7), uint64(7)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					count := 0
					if streaming {
						it, err := idx.RangeRIDIterator(&lower, true, &upper, true)
						if err != nil {
							b.Fatal(err)
						}
						for {
							_, ok, err := it.Next()
							if err != nil {
								b.Fatal(err)
							}
							if !ok {
								break
							}
							count++
						}
					} else {
						it, err := idx.RangeIterator(&lower, true, &upper, true)
						if err != nil {
							b.Fatal(err)
						}
						entry, ok, err := it.Next()
						if err != nil {
							b.Fatal(err)
						}
						if ok {
							count = len(entry.RIDs)
						}
					}
					if count != totalRIDs {
						b.Fatalf("range returned %d RIDs, want %d", count, totalRIDs)
					}
				}
			})
		}
	}
}

func benchmarkDuplicateIndex(b *testing.B, totalRIDs int) *Index {
	b.Helper()
	idx := benchmarkIndex(b, "range_duplicates")
	if err := idx.Insert(7, 0, 1); err != nil {
		b.Fatal(err)
	}
	_, leaf, err := idx.FindLeaf(7)
	if err != nil {
		b.Fatal(err)
	}
	var head, tail *BTreePage
	for offset := 0; offset < totalRIDs; {
		page, err := idx.NewPostingPage()
		if err != nil {
			b.Fatal(err)
		}
		count := totalRIDs - offset
		if count > MaxRIDsPerPosting {
			count = MaxRIDsPerPosting
		}
		if err := page.SetPostingCount(uint16(count)); err != nil {
			b.Fatal(err)
		}
		for i := 0; i < count; i++ {
			n := offset + i
			if err := page.SetPostingRID(i, RID{pageID: uint32(n/256 + 10), slotID: uint16(n % 256)}); err != nil {
				b.Fatal(err)
			}
		}
		if err := idx.WritePage(page); err != nil {
			b.Fatal(err)
		}
		if tail == nil {
			head = page
		} else {
			tail.SetNextPostingPage(page.ID)
			if err := idx.WritePage(tail); err != nil {
				b.Fatal(err)
			}
		}
		tail = page
		offset += count
	}
	if err := leaf.SetRID(0, RID{pageID: head.ID, slotID: PostingMarkerSlot}); err != nil {
		b.Fatal(err)
	}
	if err := idx.WritePage(leaf); err != nil {
		b.Fatal(err)
	}
	return idx
}
