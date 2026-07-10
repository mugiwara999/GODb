package pager

import (
	"encoding/binary"
	"fmt"
	"os"
)

type Pager struct {
	file     *os.File
	path     string
	numPages int
}

// page 0 for meta data
// meta data
// Bytes 0-3   : GODB
// Bytes 4-5   : version
// Bytes 6-13	 : lastID
// Bytes 14-17: numCols
// Bytes 18+ : column names (null-terminated strings)

const (
	GODB           = 0x474f4442
	LastIDOffset   = 6
	LastIDSize     = 8
	NumColsOffset  = 14
	NumColsSize    = 4
	ColNamesOffset = 18
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

	return page, nil

}

func (p *Pager) NewPage() (*Page, error) {
	data := [PAGE_SIZE]byte{}
	page := &Page{
		ID:   p.numPages,
		Data: data,
	}
	page.Init()
	if err := WritePage(p.file, page); err != nil {
		return nil, fmt.Errorf("allocate page %d in %q: %w", page.ID, p.path, err)
	}

	p.numPages++
	return page, nil
}

func (p *Pager) Flush(page *Page) error {
	if page == nil {
		return fmt.Errorf("flush page in %q: %w", p.path, ErrInvalidPageID)
	}
	if err := WritePage(p.file, page); err != nil {
		return fmt.Errorf("flush page %d to %q: %w", page.ID, p.path, err)
	}
	return nil

}

func (p *Pager) GetColumns() ([]string, error) {
	if p.numPages == 0 {
		return nil, fmt.Errorf("read table metadata from %q: %w (metadata page is missing)", p.path, ErrMetadataCorrupt)
	}
	page, err := p.GetPage(0)

	if err != nil || page == nil {
		return nil, fmt.Errorf("read table metadata from %q: %w", p.path, err)
	}

	err = isValidPager(p)

	if err != nil {
		return nil, fmt.Errorf("read table metadata from %q: %w", p.path, err)
	}

	numCols, err := p.GetNumCols()

	if err != nil {
		return nil, fmt.Errorf("read table metadata from %q: %w", p.path, err)
	}

	cols := make([]string, numCols)

	accumulator := make([]byte, 0)

	offset := ColNamesOffset

	colIdx := 0

	for i := offset; i < PAGE_SIZE && colIdx < numCols; i++ {
		if page.Data[i] == 0 {
			cols[colIdx] = string(accumulator)
			accumulator = accumulator[:0]
			colIdx++
			continue
		}
		accumulator = append(accumulator, page.Data[i])
	}

	if colIdx != numCols {
		return nil, fmt.Errorf("read table metadata from %q: %w (expected %d columns, found %d)", p.path, ErrMetadataCorrupt, numCols, colIdx)
	}

	return cols, nil

}

func (p *Pager) WriteColumns(cols []string) error {
	if len(cols) == 0 {
		return fmt.Errorf("write table metadata to %q: at least one column is required", p.path)
	}
	page, err := p.NewPage() // page 0
	if err != nil {
		return fmt.Errorf("write table metadata to %q: %w", p.path, err)
	}

	binary.LittleEndian.PutUint32(page.Data[:4], uint32(GODB)) // "GODB"
	binary.LittleEndian.PutUint16(page.Data[4:6], uint16(1))
	binary.LittleEndian.PutUint64(page.Data[LastIDOffset:LastIDOffset+LastIDSize], uint64(0))
	binary.LittleEndian.PutUint32(page.Data[NumColsOffset:NumColsOffset+NumColsSize], uint32(len(cols)))

	offset := ColNamesOffset
	required := offset

	for _, col := range cols {
		required += len(col) + 1
		if required > PAGE_SIZE {
			return fmt.Errorf("write table metadata to %q: %w (need %d bytes, page size is %d)", p.path, ErrPageFull, required, PAGE_SIZE)
		}
		copy(page.Data[offset:], []byte(col))
		offset += len(col)
		page.Data[offset] = 0
		offset++
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

func (p *Pager) GetNumPages() int {
	return p.numPages
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
	if binary.LittleEndian.Uint16(page.Data[4:6]) != 1 {
		return 0, fmt.Errorf("read last ID from %q: %w (unsupported version %d)", p.path, ErrMetadataCorrupt, binary.LittleEndian.Uint16(page.Data[4:6]))
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

	numCols := int(binary.LittleEndian.Uint32(page.Data[NumColsOffset : NumColsOffset+NumColsSize]))

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
	if binary.LittleEndian.Uint16(metaPage.Data[4:6]) != 1 {
		return fmt.Errorf("pager metadata page has unsupported version: %w", ErrInvalidPager)
	}

	return nil
}

type rowIterator struct {
	pager  *Pager
	pageID int
	slotID int
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

		page, err := it.pager.GetPage(it.pageID)
		if err != nil {
			return nil, fmt.Errorf("iterate rows at page %d slot %d: %w", it.pageID, it.slotID, err)
		}

		numSlots, _, _, err := page.headerValues()
		if err != nil {
			return nil, fmt.Errorf("iterate rows at page %d slot %d: %w", it.pageID, it.slotID, err)
		}
		if it.slotID >= numSlots {
			it.pageID++
			it.slotID = -1
			continue
		}

		if page.isDeleted(it.slotID) {
			continue
		}

		row, err := page.GetRow(it.slotID)
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
