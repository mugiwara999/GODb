package pager_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mugiwara999/goDB/internal/pager"
)

type slotMeta struct {
	offset  int
	length  int
	deleted bool
}

func newPage() *pager.Page {
	p := &pager.Page{ID: 0}
	p.Init()
	return p
}

func header(p *pager.Page) (numSlots, freeStart, freeEnd int) {
	numSlots = int(binary.LittleEndian.Uint16(p.Data[0:2]))
	freeStart = int(binary.LittleEndian.Uint16(p.Data[2:4]))
	freeEnd = int(binary.LittleEndian.Uint16(p.Data[4:6]))
	return
}

func slotAt(p *pager.Page, slotID int) slotMeta {
	pos := 6 + slotID*5
	return slotMeta{
		offset:  int(binary.LittleEndian.Uint16(p.Data[pos : pos+2])),
		length:  int(binary.LittleEndian.Uint16(p.Data[pos+2 : pos+4])),
		deleted: p.Data[pos+4] == 1,
	}
}

func assertPageInvariants(t *testing.T, p *pager.Page, wantRows [][]byte, deleted map[int]bool) {
	t.Helper()

	numSlots, freeStart, freeEnd := header(p)
	if numSlots != len(wantRows) {
		t.Fatalf("numSlots = %d, want %d", numSlots, len(wantRows))
	}
	if freeStart != 6+numSlots*5 {
		t.Fatalf("freeStart = %d, want %d", freeStart, 6+numSlots*5)
	}
	if freeStart > freeEnd {
		t.Fatalf("freeStart = %d, freeEnd = %d: free space region is not contiguous", freeStart, freeEnd)
	}
	if freeEnd > pager.PAGE_SIZE {
		t.Fatalf("freeEnd = %d exceeds page size %d", freeEnd, pager.PAGE_SIZE)
	}

	type recordRange struct {
		start int
		end   int
		slot  int
	}

	ranges := make([]recordRange, 0, numSlots)
	for i := 0; i < numSlots; i++ {
		meta := slotAt(p, i)
		if meta.length < 0 || meta.offset < 0 {
			t.Fatalf("slot %d has negative metadata: offset=%d length=%d", i, meta.offset, meta.length)
		}
		if meta.offset < freeEnd {
			t.Fatalf("slot %d offset=%d starts inside free space; freeEnd=%d", i, meta.offset, freeEnd)
		}
		if meta.offset > pager.PAGE_SIZE {
			t.Fatalf("slot %d offset=%d exceeds page size %d", i, meta.offset, pager.PAGE_SIZE)
		}
		if meta.offset+meta.length > pager.PAGE_SIZE {
			t.Fatalf("slot %d range [%d,%d) exceeds page size %d", i, meta.offset, meta.offset+meta.length, pager.PAGE_SIZE)
		}

		ranges = append(ranges, recordRange{start: meta.offset, end: meta.offset + meta.length, slot: i})

		if deleted != nil && deleted[i] {
			continue
		}

		got, err := p.GetRow(i)
		if err != nil {
			t.Fatalf("GetRow(%d) returned error: %v", i, err)
		}
		if !bytes.Equal(got, wantRows[i]) {
			t.Fatalf("slot %d row mismatch: got %q want %q", i, got, wantRows[i])
		}
		if meta.length != len(wantRows[i]) {
			t.Fatalf("slot %d length = %d, want %d", i, meta.length, len(wantRows[i]))
		}
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	for i := 1; i < len(ranges); i++ {
		if ranges[i-1].end > ranges[i].start {
			t.Fatalf("record overlap: slot %d [%d,%d) overlaps slot %d [%d,%d)", ranges[i-1].slot, ranges[i-1].start, ranges[i-1].end, ranges[i].slot, ranges[i].start, ranges[i].end)
		}
	}
}

func insertRows(t *testing.T, p *pager.Page, rows [][]byte) {
	t.Helper()
	for i, row := range rows {
		if err := p.AddRow(row); err != nil {
			t.Fatalf("AddRow(%d) failed: %v", i, err)
		}
		assertPageInvariants(t, p, rows[:i+1], nil)
	}
}

func copyPage(p *pager.Page) *pager.Page {
	clone := &pager.Page{ID: p.ID}
	clone.Data = p.Data
	return clone
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

func TestPageInitialization(t *testing.T) {
	t.Run("header values and free space", func(t *testing.T) {
		// Catches off-by-one errors in page header initialization.
		// These bugs are common because engines often mix inclusive and exclusive page boundaries.
		p := newPage()

		numSlots, freeStart, freeEnd := header(p)
		if numSlots != 0 {
			t.Fatalf("numSlots = %d, want 0", numSlots)
		}
		if freeStart != 6 {
			t.Fatalf("freeStart = %d, want 6", freeStart)
		}
		if freeEnd != pager.PAGE_SIZE {
			t.Fatalf("freeEnd = %d, want %d", freeEnd, pager.PAGE_SIZE)
		}

		freeSpace, err := p.FreeSpace()
		if err != nil {
			t.Fatalf("FreeSpace returned error: %v", err)
		}
		if freeSpace != pager.PAGE_SIZE-6 {
			t.Fatalf("FreeSpace = %d, want %d", freeSpace, pager.PAGE_SIZE-6)
		}
	})

	t.Run("empty page slot lookup fails", func(t *testing.T) {
		// Catches invalid-slot handling bugs.
		// Storage engines commonly forget to guard empty-page reads and negative slot IDs.
		p := newPage()

		if _, err := p.GetRow(0); !errors.Is(err, pager.ErrInvalidSlot) {
			t.Fatalf("GetRow(0) error = %v, want ErrInvalidSlot", err)
		}
		if _, err := p.GetRow(-1); !errors.Is(err, pager.ErrInvalidSlot) {
			t.Fatalf("GetRow(-1) error = %v, want ErrInvalidSlot", err)
		}
	})
}

func TestRecordInsertionAndRetrieval(t *testing.T) {
	tests := []struct {
		name string
		rows [][]byte
	}{
		{
			name: "single record",
			rows: [][]byte{[]byte("hello")},
		},
		{
			name: "multiple records",
			rows: [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")},
		},
		{
			name: "varying record sizes",
			rows: [][]byte{
				{},
				[]byte("x"),
				bytes.Repeat([]byte("y"), 17),
				bytes.Repeat([]byte("z"), 128),
				bytes.Repeat([]byte("k"), 255),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Catches slot metadata drift, row-length corruption, and row-byte mismatches.
			// These bugs are common because insert logic must update both the slot directory and the row payload atomically.
			p := newPage()
			insertRows(t, p, tt.rows)

			for i, want := range tt.rows {
				got, err := p.GetRow(i)
				if err != nil {
					t.Fatalf("GetRow(%d) returned error: %v", i, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("row %d mismatch: got %q want %q", i, got, want)
				}
			}
			assertPageInvariants(t, p, tt.rows, nil)
		})
	}
}

func TestRecordInsertionBoundaryConditions(t *testing.T) {
	t.Run("insert until nearly full", func(t *testing.T) {
		// Catches freeStart/freeEnd drift and off-by-one errors when the page is almost full.
		// These are common because page allocators often miscount slot overhead vs payload bytes.
		p := newPage()
		var rows [][]byte

		for p.CanFit(1) {
			row := []byte{byte(len(rows) % 251)}
			if err := p.AddRow(row); err != nil {
				t.Fatalf("AddRow at len=%d failed early: %v", len(rows), err)
			}
			rows = append(rows, row)
		}

		numSlots, freeStart, _ := header(p)
		if numSlots != len(rows) {
			t.Fatalf("numSlots = %d, want %d", numSlots, len(rows))
		}
		if freeStart != 6+numSlots*5 {
			t.Fatalf("freeStart = %d, want %d", freeStart, 6+numSlots*5)
		}
		if freeSpace, err := p.FreeSpace(); err != nil || freeSpace < 0 {
			t.Fatalf("FreeSpace = %d, err=%v", freeSpace, err)
		}
		if err := p.AddRow([]byte("x")); !errors.Is(err, pager.ErrPageFull) {
			t.Fatalf("AddRow on nearly-full page error = %v, want ErrPageFull", err)
		}
		assertPageInvariants(t, p, rows, nil)
	})

	t.Run("exact fit insert consumes remaining space", func(t *testing.T) {
		// Catches exact-fit bugs and incorrect freeEnd updates.
		// Exact-fit logic is a classic source of off-by-one errors in slotted-page layouts.
		p := newPage()
		payloadLen := pager.PAGE_SIZE - 6 - 5
		row := bytes.Repeat([]byte("a"), payloadLen)

		if err := p.AddRow(row); err != nil {
			t.Fatalf("exact-fit AddRow failed: %v", err)
		}

		numSlots, freeStart, freeEnd := header(p)
		if numSlots != 1 {
			t.Fatalf("numSlots = %d, want 1", numSlots)
		}
		if freeStart != 11 {
			t.Fatalf("freeStart = %d, want 11", freeStart)
		}
		if freeEnd != 11 {
			t.Fatalf("freeEnd = %d, want 11", freeEnd)
		}
		freeSpace, err := p.FreeSpace()
		if err != nil {
			t.Fatalf("FreeSpace returned error: %v", err)
		}
		if freeSpace != 0 {
			t.Fatalf("FreeSpace = %d, want 0", freeSpace)
		}

		meta := slotAt(p, 0)
		if meta.offset != 11 || meta.length != payloadLen {
			t.Fatalf("slot metadata = %+v, want offset=11 length=%d", meta, payloadLen)
		}
		if got, err := p.GetRow(0); err != nil || !bytes.Equal(got, row) {
			t.Fatalf("GetRow(0) got %q err=%v want exact payload", got, err)
		}
		if p.CanFit(1) {
			t.Fatal("CanFit(1) = true on a full page, want false")
		}
		if err := p.AddRow([]byte("b")); !errors.Is(err, pager.ErrPageFull) {
			t.Fatalf("AddRow on full page error = %v, want ErrPageFull", err)
		}
		assertPageInvariants(t, p, [][]byte{row}, nil)
	})
}

func TestRecordRetrievalOrders(t *testing.T) {
	p := newPage()
	rows := [][]byte{
		[]byte("first"),
		bytes.Repeat([]byte("m"), 7),
		bytes.Repeat([]byte("l"), 13),
		[]byte("last"),
	}
	insertRows(t, p, rows)

	t.Run("first middle last", func(t *testing.T) {
		// Catches slot index/offset confusion.
		// Engines commonly mix insertion order with physical row order when pages fill from the end.
		cases := []struct {
			slot int
			want []byte
		}{
			{slot: 0, want: rows[0]},
			{slot: 1, want: rows[1]},
			{slot: 2, want: rows[2]},
			{slot: 3, want: rows[3]},
		}
		for _, tc := range cases {
			got, err := p.GetRow(tc.slot)
			if err != nil {
				t.Fatalf("GetRow(%d) returned error: %v", tc.slot, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("slot %d mismatch: got %q want %q", tc.slot, got, tc.want)
			}
		}
	})

	t.Run("random slot order", func(t *testing.T) {
		// Catches stale slot metadata and read-path corruption.
		// Random access is where page-layout bugs often show up first.
		order := []int{0, 1, 2, 3}
		r := rand.New(rand.NewSource(99))
		r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, slot := range order {
			got, err := p.GetRow(slot)
			if err != nil {
				t.Fatalf("GetRow(%d) returned error: %v", slot, err)
			}
			if !bytes.Equal(got, rows[slot]) {
				t.Fatalf("slot %d mismatch: got %q want %q", slot, got, rows[slot])
			}
		}
	})
}

func TestSlotDirectoryCorrectness(t *testing.T) {
	// Catches overlapping rows, bad slot offsets, and broken length bookkeeping.
	// This is common in slotted pages because every insert updates both the slot directory and the payload region.
	p := newPage()
	rows := make([][]byte, 0, 40)
	for i := 0; i < 40; i++ {
		size := (i*7)%64 + i%5
		row := bytes.Repeat([]byte{byte('a' + i%26)}, size)
		if !p.CanFit(len(row)) {
			break
		}
		if err := p.AddRow(row); err != nil {
			t.Fatalf("AddRow(%d) failed: %v", i, err)
		}
		rows = append(rows, row)
	}

	assertPageInvariants(t, p, rows, nil)

	for i, want := range rows {
		meta := slotAt(p, i)
		if meta.length != len(want) {
			t.Fatalf("slot %d length = %d, want %d", i, meta.length, len(want))
		}
		if meta.offset < 0 || meta.offset+meta.length > pager.PAGE_SIZE {
			t.Fatalf("slot %d points outside page: offset=%d length=%d", i, meta.offset, meta.length)
		}
		got, err := p.GetRow(i)
		if err != nil {
			t.Fatalf("GetRow(%d) returned error: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("slot %d bytes mismatch", i)
		}
	}
}

func TestDeletionAndTombstones(t *testing.T) {
	// Catches delete-flag bugs and iterator regression bugs.
	// Tombstones are common in storage engines because physical row compaction is expensive.
	p := newPage()
	rows := [][]byte{[]byte("keep-1"), []byte("delete-me"), []byte("keep-2")}
	insertRows(t, p, rows)

	before := copyPage(p)
	if err := p.DeleteRow(1); err != nil {
		t.Fatalf("DeleteRow(1) returned error: %v", err)
	}

	if _, err := p.GetRow(1); !errors.Is(err, pager.ErrInvalidSlot) {
		t.Fatalf("GetRow(1) after delete error = %v, want ErrInvalidSlot", err)
	}

	afterNum, afterFreeStart, afterFreeEnd := header(p)
	beforeNum, beforeFreeStart, beforeFreeEnd := header(before)
	if afterNum != beforeNum || afterFreeStart != beforeFreeStart || afterFreeEnd != beforeFreeEnd {
		t.Fatalf("delete changed header: before=(%d,%d,%d) after=(%d,%d,%d)", beforeNum, beforeFreeStart, beforeFreeEnd, afterNum, afterFreeStart, afterFreeEnd)
	}
}

func TestOverwriteActiveSlot(t *testing.T) {
	// Catches in-place overwrite bugs on active rows.
	// These bugs are common because page engines must not move the slot directory when payload size stays the same.
	p := newPage()
	row := []byte("abcde")
	if err := p.AddRow(row); err != nil {
		t.Fatalf("AddRow returned error: %v", err)
	}

	replacement := []byte("vwxyz")
	if err := p.Overwrite(0, replacement); err != nil {
		t.Fatalf("Overwrite returned error: %v", err)
	}

	got, err := p.GetRow(0)
	if err != nil {
		t.Fatalf("GetRow(0) returned error: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("overwrite mismatch: got %q want %q", got, replacement)
	}

	meta := slotAt(p, 0)
	if meta.length != len(row) {
		t.Fatalf("overwrite changed slot length: got %d want %d", meta.length, len(row))
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	// Catches serialization bugs, seek-offset bugs, and header corruption during disk writes.
	// Persistence bugs are common because page writes often look correct in memory but fail once flushed to disk.
	dir := t.TempDir()
	path := filepath.Join(dir, "page.bin")

	rows := [][]byte{
		[]byte("alpha"),
		bytes.Repeat([]byte("b"), 17),
		bytes.Repeat([]byte("c"), 64),
	}

	p := newPage()
	insertRows(t, p, rows)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if err := pager.WritePage(file, p); err != nil {
		_ = file.Close()
		t.Fatalf("WritePage failed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	file, err = os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer file.Close()

	raw, err := pager.ReadPage(file, 0)
	if err != nil {
		t.Fatalf("ReadPage failed: %v", err)
	}

	roundTripped := &pager.Page{ID: 0}
	copy(roundTripped.Data[:], raw)

	assertPageInvariants(t, roundTripped, rows, nil)

	for i, want := range rows {
		got, err := roundTripped.GetRow(i)
		if err != nil {
			t.Fatalf("round-tripped GetRow(%d) failed: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round-tripped row %d mismatch: got %q want %q", i, got, want)
		}
	}

	// Repeat the serialization/deserialization cycle to catch drifting metadata.
	for i := 0; i < 3; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("roundtrip open failed: %v", err)
		}
		if err := pager.WritePage(f, roundTripped); err != nil {
			_ = f.Close()
			t.Fatalf("roundtrip WritePage failed: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("roundtrip Close failed: %v", err)
		}
		f, err = os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("roundtrip reopen failed: %v", err)
		}
		raw, err = pager.ReadPage(f, 0)
		_ = f.Close()
		if err != nil {
			t.Fatalf("roundtrip ReadPage failed: %v", err)
		}
		copy(roundTripped.Data[:], raw)
	}
	assertPageInvariants(t, roundTripped, rows, nil)
}

func TestMetadataPageRoundTrip(t *testing.T) {
	// Catches metadata serialization bugs and column-name truncation.
	// These bugs are common because metadata lives in the same file as data pages and is easy to read back incorrectly.
	dir := t.TempDir()
	path := filepath.Join(dir, "table.bin")

	cases := []struct {
		name string
		cols []string
	}{
		{
			name: "single column",
			cols: []string{"id"},
		},
		{
			name: "multiple columns",
			cols: []string{"id", "name", "email"},
		},
		{
			name: "long column names",
			cols: []string{
				"identifier_" + string(bytes.Repeat([]byte("x"), 128)),
				"display_name_" + string(bytes.Repeat([]byte("y"), 160)),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pg, err := pager.CreatePager(path)
			if err != nil {
				t.Fatalf("CreatePager failed: %v", err)
			}
			t.Cleanup(func() { _ = pg.Close() })

			if err := pg.WriteColumns(tc.cols); err != nil {
				t.Fatalf("WriteColumns failed: %v", err)
			}
			if got := pg.GetNumPages(); got != 1 {
				t.Fatalf("GetNumPages after metadata write = %d, want 1", got)
			}

			opened, err := pager.OpenPager(path)
			if err != nil {
				t.Fatalf("OpenPager failed: %v", err)
			}
			defer opened.Close()

			gotCols, err := opened.GetColumns()
			if err != nil {
				t.Fatalf("GetColumns failed: %v", err)
			}
			if !equalStrings(gotCols, tc.cols) {
				t.Fatalf("GetColumns = %q, want %q", gotCols, tc.cols)
			}
			if opened.GetNumPages() != 1 {
				t.Fatalf("GetNumPages after reopen = %d, want 1", opened.GetNumPages())
			}
		})
	}
}

func TestMetadataCorruptionDetection(t *testing.T) {
	// Catches corrupted-metadata reads.
	// A storage engine must reject bad magic/version bytes instead of silently accepting a broken schema page.
	dir := t.TempDir()
	path := filepath.Join(dir, "table.bin")

	pg, err := pager.CreatePager(path)
	if err != nil {
		t.Fatalf("CreatePager failed: %v", err)
	}
	if err := pg.WriteColumns([]string{"name", "roll"}); err != nil {
		t.Fatalf("WriteColumns failed: %v", err)
	}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	raw, err := pager.ReadPage(file, 0)
	if err != nil {
		t.Fatalf("ReadPage failed: %v", err)
	}
	_ = file.Close()

	raw[0] = 'X'
	corrupted := &pager.Page{ID: 0}
	copy(corrupted.Data[:], raw)

	file, err = os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	if err := pager.WritePage(file, corrupted); err != nil {
		_ = file.Close()
		t.Fatalf("WritePage failed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	opened, err := pager.OpenPager(path)
	if err != nil {
		t.Fatalf("OpenPager failed: %v", err)
	}
	defer opened.Close()

	if _, err := opened.GetColumns(); !errors.Is(err, pager.ErrMetadataCorrupt) && !errors.Is(err, pager.ErrInvalidPager) {
		t.Fatalf("GetColumns error = %v, want ErrMetadataCorrupt", err)
	}
}

func TestCorruptedSlotOffsetsAndHeaders(t *testing.T) {
	// Catches bad slot offsets and corrupted page headers.
	// Both are common after partial writes, torn pages, or memory corruption.
	t.Run("corrupted slot offset", func(t *testing.T) {
		p := newPage()
		row := []byte("hello")
		if err := p.AddRow(row); err != nil {
			t.Fatalf("AddRow failed: %v", err)
		}

		binary.LittleEndian.PutUint16(p.Data[6:8], uint16(pager.PAGE_SIZE-1))
		binary.LittleEndian.PutUint16(p.Data[8:10], 10)
		if _, err := p.GetRow(0); !errors.Is(err, pager.ErrCorruptPageData) {
			t.Fatalf("GetRow error = %v, want ErrCorruptPageData", err)
		}
	})

	t.Run("corrupted header", func(t *testing.T) {
		p := newPage()
		binary.LittleEndian.PutUint16(p.Data[2:4], 5)
		if _, err := p.FreeSpace(); !errors.Is(err, pager.ErrCorruptPageHeader) {
			t.Fatalf("FreeSpace error = %v, want ErrCorruptPageHeader", err)
		}
		if err := p.AddRow([]byte("x")); !errors.Is(err, pager.ErrCorruptPageHeader) {
			t.Fatalf("AddRow error = %v, want ErrCorruptPageHeader", err)
		}
	})
}

func TestRandomizedRoundTripAndInvariants(t *testing.T) {
	// Catches data-dependent slot bugs and random-access corruption.
	// Randomized layouts are a common way to surface bugs that fixed examples miss.
	r := rand.New(rand.NewSource(12345))
	p := newPage()
	rows := make([][]byte, 0, 80)

	for i := 0; i < 80; i++ {
		size := r.Intn(32)
		row := randomBytes(r, size)
		if !p.CanFit(len(row)) {
			break
		}
		if err := p.AddRow(row); err != nil {
			t.Fatalf("AddRow(%d) failed: %v", i, err)
		}
		rows = append(rows, row)
		assertPageInvariants(t, p, rows, nil)
	}

	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	for _, slot := range order {
		got, err := p.GetRow(slot)
		if err != nil {
			t.Fatalf("GetRow(%d) failed: %v", slot, err)
		}
		if !bytes.Equal(got, rows[slot]) {
			t.Fatalf("random slot %d mismatch", slot)
		}
	}
}

func FuzzPageAddRowRoundTrip(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("seed"))
	f.Add(bytes.Repeat([]byte("a"), 1))
	f.Add(bytes.Repeat([]byte("b"), pager.PAGE_SIZE-6-5))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Fuzzing catches record-layout bugs, especially exact-fit and empty-record edge cases.
		// Those bugs are common because row serialization tends to hide boundary errors until random inputs hit them.
		if len(data) > pager.PAGE_SIZE-11 {
			data = data[:pager.PAGE_SIZE-11]
		}

		p := newPage()
		if err := p.AddRow(data); err != nil {
			t.Fatalf("AddRow failed for len=%d: %v", len(data), err)
		}

		got, err := p.GetRow(0)
		if err != nil {
			t.Fatalf("GetRow(0) failed: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("round-trip mismatch: got %x want %x", got, data)
		}
		assertPageInvariants(t, p, [][]byte{data}, nil)
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
