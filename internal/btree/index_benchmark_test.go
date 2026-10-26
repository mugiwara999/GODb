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
