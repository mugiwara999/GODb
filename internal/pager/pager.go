package pager

import (
	"encoding/binary"
	"fmt"
	"github.com/mugiwara999/goDB/internal/wal"
	"os"
	"strings"
)

type Pager struct {
	file     *os.File
	path     string
	numPages int
	journal  *wal.Journal
	writeFn  func(*os.File, *Page) error
}

// page 0 for meta data
// meta data
// Bytes 0-3   : GODB
// Bytes 4-5   : version
// Bytes 6-13	 : lastID
// Bytes 14-17: numCols
// Bytes 18+ : column names (null-terminated strings)

const (
	GODB              = 0x474f4442
	MetadataVersionV1 = uint16(1)
	MetadataVersionV2 = uint16(2)
	LastIDOffset      = 6
	LastIDSize        = 8
	NumColsOffset     = 14
	NumColsSize       = 4
	ColNamesOffset    = 18
)

type ColumnType uint8

const (
	ColumnTypeText ColumnType = iota + 1
	ColumnTypeUint64
)

var ErrInvalidPager = fmt.Errorf("invalid pager")

func NewPager(path string) (*Pager, error) {
	return CreatePager(path)
}

func OpenPager(path string) (*Pager, error) {
	return openPager(path, os.O_RDWR)
}

func CreatePager(path string) (*Pager, error) {
	return openPager(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
}

func openPager(path string, flags int) (*Pager, error) {
	file, err := os.OpenFile(path, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("open pager %q: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat pager %q: %w", path, err)
	}
	if info.Size()%PAGE_SIZE != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("open pager %q: %w (file size %d is not a multiple of page size %d)", path, ErrCorruptPageData, info.Size(), PAGE_SIZE)
	}
	numPages := int(info.Size()) / PAGE_SIZE
	return &Pager{file: file, path: path, numPages: numPages}, nil
}

func (p *Pager) GetPage(pageID int) (*Page, error) {
	if pageID < 0 || pageID >= p.numPages {
		return nil, fmt.Errorf("get page %d from %q: %w (page count=%d)", pageID, p.path, ErrInvalidPageID, p.numPages)
	}

	data, err := ReadPage(p.file, pageID)

	if err != nil {
		return nil, fmt.Errorf("get page %d from %q: %w", pageID, p.path, err)
	}

	var pageData [PAGE_SIZE]byte
	copy(pageData[:], data)

	page := &Page{
		ID:   pageID,
		Data: pageData,
	}
	if pageID > 0 {
		if err := page.Validate(); err != nil {
			return nil, fmt.Errorf("get page %d from %q: %w", pageID, p.path, err)
		}
	}

	return page, nil

}

func (p *Pager) NewPage() (*Page, error) {
	data := [PAGE_SIZE]byte{}
	page := &Page{
		ID:   p.numPages,
		Data: data,
	}
	page.Init()
	if p.journal != nil {
		if err := p.journal.BeforePageWrite(p.path, p.file, uint32(page.ID), PAGE_SIZE); err != nil {
			return nil, fmt.Errorf("journal page allocation %d in %q: %w", page.ID, p.path, err)
		}
	}
	if err := p.writePage(page); err != nil {
		return nil, fmt.Errorf("allocate page %d in %q: %w", page.ID, p.path, err)
	}

	p.numPages++
	return page, nil
}

func (p *Pager) Flush(page *Page) error {
	if page == nil {
		return fmt.Errorf("flush page in %q: %w", p.path, ErrInvalidPageID)
	}
	if p.journal != nil {
		if page.ID < 0 {
			return fmt.Errorf("flush page %d to %q: %w", page.ID, p.path, ErrInvalidPageID)
		}
		if err := p.journal.BeforePageWrite(p.path, p.file, uint32(page.ID), PAGE_SIZE); err != nil {
			return fmt.Errorf("journal page %d in %q: %w", page.ID, p.path, err)
		}
	}
	if err := p.writePage(page); err != nil {
		return fmt.Errorf("flush page %d to %q: %w", page.ID, p.path, err)
	}
	return nil

}

func (p *Pager) SetJournal(j *wal.Journal) { p.journal = j }

func (p *Pager) writePage(page *Page) error {
	if p.writeFn != nil {
		return p.writeFn(p.file, page)
	}
	return WritePage(p.file, page)
}

func (p *Pager) GetColumns() ([]string, error) {
	cols, _, err := p.GetSchema()
	return cols, err
}

func (p *Pager) GetSchema() ([]string, []ColumnType, error) {
	if p.numPages == 0 {
		return nil, nil, fmt.Errorf("read table metadata from %q: %w (metadata page is missing)", p.path, ErrMetadataCorrupt)
	}
	page, err := p.GetPage(0)

	if err != nil || page == nil {
		return nil, nil, fmt.Errorf("read table metadata from %q: %w", p.path, err)
	}

	err = isValidPager(p)

	if err != nil {
		return nil, nil, fmt.Errorf("read table metadata from %q: %w", p.path, err)
	}

	numCols, err := p.GetNumCols()
	if err != nil {
		return nil, nil, fmt.Errorf("read table metadata from %q: %w", p.path, err)
	}
	cols := make([]string, numCols)
	types := make([]ColumnType, numCols)
	accumulator := make([]byte, 0)
	offset := ColNamesOffset
	for colIdx := 0; colIdx < numCols; colIdx++ {
		start := offset
		for offset < PAGE_SIZE && page.Data[offset] != 0 {
			accumulator = append(accumulator, page.Data[offset])
			offset++
		}
		if offset >= PAGE_SIZE {
			return nil, nil, fmt.Errorf("read table metadata from %q: %w (unterminated column %d)", p.path, ErrMetadataCorrupt, colIdx)
		}
		if offset == start {
			return nil, nil, fmt.Errorf("read table metadata from %q: %w (empty column %d)", p.path, ErrMetadataCorrupt, colIdx)
		}
		cols[colIdx] = string(accumulator)
		accumulator = accumulator[:0]
		offset++
	}

	version := binary.LittleEndian.Uint16(page.Data[4:6])
	if version == MetadataVersionV1 {
		for i := range types {
			types[i] = ColumnTypeText
		}
		if len(cols) > 0 && strings.EqualFold(cols[0], "id") {
			types[0] = ColumnTypeUint64
		}
		return cols, types, nil
	}
	if version != MetadataVersionV2 {
		return nil, nil, fmt.Errorf("read table metadata from %q: %w (unsupported version %d)", p.path, ErrMetadataCorrupt, version)
	}
	if offset+numCols > PAGE_SIZE {
		return nil, nil, fmt.Errorf("read table metadata from %q: %w (column type list exceeds metadata page)", p.path, ErrMetadataCorrupt)
	}
	for i := range types {
		types[i] = ColumnType(page.Data[offset+i])
		if types[i] != ColumnTypeText && types[i] != ColumnTypeUint64 {
			return nil, nil, fmt.Errorf("read table metadata from %q: %w (invalid type tag %d for column %q)", p.path, ErrMetadataCorrupt, types[i], cols[i])
		}
	}
	return cols, types, nil
}

func (p *Pager) WriteColumns(cols []string) error {
	return p.writeColumns(cols, nil, MetadataVersionV1)
}

func (p *Pager) WriteSchema(cols []string, types []ColumnType) error {
	if len(cols) != len(types) {
		return fmt.Errorf("write typed schema to %q: %w (got %d names and %d types)", p.path, ErrMetadataCorrupt, len(cols), len(types))
	}
	return p.writeColumns(cols, types, MetadataVersionV2)
}

func (p *Pager) writeColumns(cols []string, types []ColumnType, version uint16) error {
	if len(cols) == 0 {
		return fmt.Errorf("write table metadata to %q: at least one column is required", p.path)
	}
	if version == MetadataVersionV2 {
		for i, columnType := range types {
			if columnType != ColumnTypeText && columnType != ColumnTypeUint64 {
				return fmt.Errorf("write typed schema to %q: %w (invalid type %d at column %d)", p.path, ErrMetadataCorrupt, columnType, i)
			}
		}
	}
	required := ColNamesOffset + len(types)
	for _, col := range cols {
		required += len(col) + 1
	}
	if required > PAGE_SIZE {
		return fmt.Errorf("write table metadata to %q: %w (need %d bytes, page size is %d)", p.path, ErrPageFull, required, PAGE_SIZE)
	}
	page, err := p.NewPage() // page 0
	if err != nil {
		return fmt.Errorf("write table metadata to %q: %w", p.path, err)
	}

	binary.LittleEndian.PutUint32(page.Data[:4], uint32(GODB))
	binary.LittleEndian.PutUint16(page.Data[4:6], version)
	binary.LittleEndian.PutUint64(page.Data[LastIDOffset:LastIDOffset+LastIDSize], uint64(0))
	binary.LittleEndian.PutUint32(page.Data[NumColsOffset:NumColsOffset+NumColsSize], uint32(len(cols)))

	offset := ColNamesOffset
	for _, col := range cols {
		copy(page.Data[offset:], []byte(col))
		offset += len(col)
		page.Data[offset] = 0
		offset++
	}
	if version == MetadataVersionV2 {
		for _, columnType := range types {
			page.Data[offset] = byte(columnType)
			offset++
		}
	}
	if err := p.Flush(page); err != nil {
		return fmt.Errorf("write table metadata to %q: %w", p.path, err)
	}
	return nil
}

func (p *Pager) Close() error {
	if p == nil || p.file == nil {
		return nil
	}
	if err := p.file.Close(); err != nil {
		return fmt.Errorf("close pager %q: %w", p.path, err)
	}
	return nil
}

func (p *Pager) Sync() error {
	if p == nil || p.file == nil {
		return nil
	}
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("sync pager %q: %w", p.path, err)
	}
	return nil
}

func (p *Pager) GetNumPages() int {
	return p.numPages
}

// Refresh updates the in-memory page count after another process has changed
// the same table file while this pager remained open.
func (p *Pager) Refresh() error {
	info, err := p.file.Stat()
	if err != nil {
		return fmt.Errorf("refresh pager %q: %w", p.path, err)
	}
	if info.Size()%PAGE_SIZE != 0 {
		return fmt.Errorf("refresh pager %q: %w (file size %d is not a multiple of page size %d)", p.path, ErrCorruptPageData, info.Size(), PAGE_SIZE)
	}
	p.numPages = int(info.Size()) / PAGE_SIZE
	return nil
}

func (p *Pager) LastID() (uint64, error) {
	if p.numPages == 0 {
		return 0, fmt.Errorf("read last ID from %q: %w (metadata page is missing)", p.path, ErrMetadataCorrupt)
	}
	page, err := p.GetPage(0)

	if err != nil || page == nil {
		return 0, fmt.Errorf("read last ID from %q: %w", p.path, err)
	}

	if binary.LittleEndian.Uint32(page.Data[:4]) != GODB {
		return 0, fmt.Errorf("read last ID from %q: %w (invalid magic)", p.path, ErrMetadataCorrupt)
	}
	version := binary.LittleEndian.Uint16(page.Data[4:6])
	if !supportedMetadataVersion(version) {
		return 0, fmt.Errorf("read last ID from %q: %w (unsupported version %d)", p.path, ErrMetadataCorrupt, version)
	}

	lastID := binary.LittleEndian.Uint64(page.Data[LastIDOffset : LastIDOffset+LastIDSize])

	return lastID, nil

}

func (p *Pager) SetLastID(lastID uint64) error {
	if p.numPages == 0 {
		return fmt.Errorf("write last ID to %q: %w (metadata page is missing)", p.path, ErrMetadataCorrupt)
	}

	err := isValidPager(p)

	if err != nil {
		return err
	}

	page, _ := p.GetPage(0)

	binary.LittleEndian.PutUint64(page.Data[LastIDOffset:LastIDOffset+LastIDSize], lastID)

	if err := p.Flush(page); err != nil {
		return fmt.Errorf("write last ID to %q: %w", p.path, err)
	}

	return nil

}

func (p *Pager) GetNumCols() (int, error) {
	if p.numPages == 0 {
		return 0, fmt.Errorf("read number of columns from %q: %w (metadata page is missing)", p.path, ErrMetadataCorrupt)
	}

	err := isValidPager(p)

	if err != nil {
		return 0, fmt.Errorf("read number of columns from %q: %w", p.path, err)
	}

	page, _ := p.GetPage(0)

	storedNumCols := binary.LittleEndian.Uint32(page.Data[NumColsOffset : NumColsOffset+NumColsSize])
	if storedNumCols == 0 || storedNumCols > PAGE_SIZE-ColNamesOffset {
		return 0, fmt.Errorf("read number of columns from %q: %w (invalid column count %d)", p.path, ErrMetadataCorrupt, storedNumCols)
	}
	numCols := int(storedNumCols)

	return numCols, nil

}

func isValidPager(p *Pager) error {
	if p == nil {
		return fmt.Errorf("pager is nil: %w", ErrInvalidPager)
	}
	if p.file == nil {
		return fmt.Errorf("pager file is nil: %w", ErrInvalidPager)
	}
	if p.path == "" {
		return fmt.Errorf("pager path is empty: %w", ErrInvalidPager)
	}
	if p.numPages < 0 {
		return fmt.Errorf("pager numPages is negative: %w", ErrInvalidPager)
	}

	metaPage, err := p.GetPage(0)
	if err != nil {
		return fmt.Errorf("pager metadata page is invalid: %w", ErrInvalidPager)
	}
	if metaPage == nil {
		return fmt.Errorf("pager metadata page is nil: %w", ErrInvalidPager)
	}
	if binary.LittleEndian.Uint32(metaPage.Data[:4]) != GODB {
		return fmt.Errorf("pager metadata page has invalid magic: %w, %s", ErrInvalidPager, string(metaPage.Data[:4]))
	}
	if !supportedMetadataVersion(binary.LittleEndian.Uint16(metaPage.Data[4:6])) {
		return fmt.Errorf("pager metadata page has unsupported version: %w", ErrInvalidPager)
	}

	return nil
}

func supportedMetadataVersion(version uint16) bool {
	return version == MetadataVersionV1 || version == MetadataVersionV2
}

type rowIterator struct {
	pager  *Pager
	pageID int
	slotID int
	page   *Page
	nSlots int
}

func (p *Pager) RowIterator() *rowIterator {
	return &rowIterator{
		pager:  p,
		pageID: 1,
		slotID: -1,
	}
}

func (it *rowIterator) Next() ([]byte, error) {
	for {
		it.slotID++
		if it.pageID >= it.pager.GetNumPages() {
			return nil, nil
		}

		if it.page == nil || it.page.ID != it.pageID {
			page, err := it.pager.GetPage(it.pageID)
			if err != nil {
				return nil, fmt.Errorf("iterate rows at page %d slot %d: %w", it.pageID, it.slotID, err)
			}
			numSlots, _, _, err := page.headerValues()
			if err != nil {
				return nil, fmt.Errorf("iterate rows at page %d slot %d: %w", it.pageID, it.slotID, err)
			}
			it.page = page
			it.nSlots = numSlots
		}
		if it.slotID >= it.nSlots {
			it.pageID++
			it.slotID = -1
			continue
		}

		if it.page.isDeleted(it.slotID) {
			continue
		}

		row, err := it.page.GetRow(it.slotID)
		if err != nil {
			return nil, fmt.Errorf("iterate rows at page %d slot %d: %w", it.pageID, it.slotID, err)
		}

		return row, nil
	}

}

type RowItInfo struct {
	PageID int
	SlotID int
}

func (it *rowIterator) GetCurrentInfo() RowItInfo {
	return RowItInfo{
		PageID: it.pageID,
		SlotID: it.slotID,
	}
}
