package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const PAGE_SIZE = 4096

type Page struct {
	ID   int
	Data [PAGE_SIZE]byte
}

var (
	ErrPageFull          = errors.New("page full")
	ErrInvalidSlot       = errors.New("invalid slot")
	ErrInvalidPageID     = errors.New("invalid page id")
	ErrCorruptPageHeader = errors.New("corrupt page header")
	ErrCorruptPageData   = errors.New("corrupt page data")
	ErrMetadataCorrupt   = errors.New("corrupt table metadata")
	ErrorWritingPage     = errors.New("write page")
	ErrorReadingPage     = errors.New("read page")
	ErrorNotEnoughSpace  = ErrPageFull
)

// Bytes 0-1:   numSlots  (how many rows on this page)
// Bytes 2-3:   freeStart (where free space starts, after slots)
// Bytes 4-5:   freeEnd   (where free space ends, before row data)
// Bytes 6+:    slot array - each slot is 5 bytes
// 2 bytes since they are used to represent offset, 1 byte can represent only till 256
// each slot is 4 bytes. 2 bytes for offset, 2 bytes for len of row
// and last slot is a flag to represent a deleted slot
// since after deletion moving all the slots is expensive
// so my approach is to flag it and have a function run every 24 hrs
// which rebuilds the page with only the active slots

func (p *Page) Init() {
	binary.LittleEndian.PutUint16(p.Data[0:2], 0)         // numSlots = 0
	binary.LittleEndian.PutUint16(p.Data[2:4], 6)         // freeStart = 6 (after header)
	binary.LittleEndian.PutUint16(p.Data[4:6], PAGE_SIZE) // freeEnd = 4096
}

func WritePage(file *os.File, page *Page) error {
	_, err := file.Seek(int64(page.ID)*PAGE_SIZE, 0)

	if err != nil {
		return fmt.Errorf("write page %d: %w", page.ID, err)
	}

	n, err := file.Write(page.Data[:])

	if err != nil {
		return fmt.Errorf("write page %d: %w", page.ID, err)
	}

	if n < PAGE_SIZE {
		return fmt.Errorf("write page %d: %w (wrote %d of %d bytes)", page.ID, ErrorWritingPage, n, PAGE_SIZE)
	}

	return nil
}

func ReadPage(file *os.File, pageID int) ([]byte, error) {
	if pageID < 0 {
		return nil, fmt.Errorf("read page %d: %w", pageID, ErrInvalidPageID)
	}

	_, err := file.Seek(int64(pageID)*PAGE_SIZE, 0)

	if err != nil {
		return nil, fmt.Errorf("read page %d: %w", pageID, err)
	}

	data := make([]byte, PAGE_SIZE)

	n, err := io.ReadFull(file, data)

	if err != nil {
		return nil, fmt.Errorf("read page %d: %w", pageID, err)
	}

	if n != PAGE_SIZE {
		return nil, fmt.Errorf("read page %d: %w (read %d of %d bytes)", pageID, ErrorReadingPage, n, PAGE_SIZE)
	}
	return data, nil
}

func (p *Page) AddRow(data []byte) error {
	numSlots, freeStart, freeEnd, err := p.headerValues()
	if err != nil {
		return err
	}

	if freeEnd-freeStart < len(data)+5 {
		return fmt.Errorf("add row to page %d: %w (need %d bytes, have %d bytes)", p.ID, ErrPageFull, len(data)+5, freeEnd-freeStart)
	}

	start := freeEnd - len(data)

	n := copy(p.Data[start:freeEnd], data)

	if n != len(data) {
		return fmt.Errorf("add row to page %d: %w (copied %d of %d bytes)", p.ID, ErrorWritingPage, n, len(data))
	}

	numSlots++ // increase num of rows

	binary.LittleEndian.PutUint16(p.Data[:2], uint16(numSlots))

	binary.LittleEndian.PutUint16(p.Data[freeStart:freeStart+2], uint16(start)) // add offset to slots
	binary.LittleEndian.PutUint16(p.Data[freeStart+2:freeStart+4], uint16(len(data)))
	p.Data[freeStart+4] = 0

	freeStart += 5

	binary.LittleEndian.PutUint16(p.Data[2:4], uint16(freeStart))

	freeEnd = start

	binary.LittleEndian.PutUint16(p.Data[4:6], uint16(freeEnd))

	return nil

}

func (p *Page) GetRow(slotID int) ([]byte, error) {
	slotPos, err := p.slotPos(slotID)
	if err != nil {
		return nil, err
	}

	if p.Data[slotPos+4] == 1 {
		return nil, fmt.Errorf("read row from page %d slot %d: %w", p.ID, slotID, ErrInvalidSlot)
	}

	offset := int(binary.LittleEndian.Uint16(p.Data[slotPos : slotPos+2]))
	size := int(binary.LittleEndian.Uint16(p.Data[slotPos+2 : slotPos+4]))

	if offset < 0 || size < 0 || offset > PAGE_SIZE || size > PAGE_SIZE || offset+size > PAGE_SIZE {
		return nil, fmt.Errorf("read row from page %d slot %d: %w (offset=%d size=%d)", p.ID, slotID, ErrCorruptPageData, offset, size)
	}

	data := make([]byte, size)
	copy(data, p.Data[offset:offset+size])

	return data, nil

}

func (p *Page) DeleteRow(slotID int) error {
	offsetPos, err := p.slotPos(slotID)
	if err != nil {
		return err
	}
	p.Data[offsetPos+4] = 1
	return nil
}

func (p *Page) CanFit(dataLen int) bool {
	_, freeStart, freeEnd, err := p.headerValues()
	if err != nil {
		return false
	}
	return (freeEnd - freeStart) >= dataLen+5

}

func (p *Page) isDeleted(slotId int) bool {
	offset := 6 + slotId*5

	x := int(p.Data[offset+4])

	return x == 1

}

func (p *Page) Overwrite(slotId int, data []byte) error {
	offset, err := p.slotPos(slotId)
	if err != nil {
		return err
	}
	size := int(binary.LittleEndian.Uint16(p.Data[offset+2 : offset+4]))
	if len(data) > size {
		return fmt.Errorf("overwrite page %d slot %d: %w (new size %d exceeds %d)", p.ID, slotId, ErrPageFull, len(data), size)
	}
	start := int(binary.LittleEndian.Uint16(p.Data[offset : offset+2]))
	if start < 0 || start+len(data) > PAGE_SIZE {
		return fmt.Errorf("overwrite page %d slot %d: %w (offset=%d len=%d)", p.ID, slotId, ErrCorruptPageData, start, len(data))
	}
	copy(p.Data[start:start+len(data)], data)
	return nil
}

func (p *Page) headerValues() (numSlots, freeStart, freeEnd int, err error) {
	numSlots = int(binary.LittleEndian.Uint16(p.Data[:2]))
	freeStart = int(binary.LittleEndian.Uint16(p.Data[2:4]))
	freeEnd = int(binary.LittleEndian.Uint16(p.Data[4:6]))

	expectedFreeStart := 6 + numSlots*5
	if freeStart != expectedFreeStart || freeStart < 6 || freeStart > PAGE_SIZE || freeEnd > PAGE_SIZE {
		return 0, 0, 0, fmt.Errorf("page %d header: %w (numSlots=%d freeStart=%d freeEnd=%d)", p.ID, ErrCorruptPageHeader, numSlots, freeStart, freeEnd)
	}

	if freeStart > freeEnd {
		return 0, 0, 0, fmt.Errorf("page %d header: %w (freeStart=%d freeEnd=%d)", p.ID, ErrCorruptPageHeader, freeStart, freeEnd)
	}

	return numSlots, freeStart, freeEnd, nil
}

func (p *Page) slotPos(slotID int) (int, error) {
	numSlots, _, _, err := p.headerValues()
	if err != nil {
		return 0, err
	}
	if slotID < 0 || slotID >= numSlots {
		return 0, fmt.Errorf("page %d slot %d: %w (numSlots=%d)", p.ID, slotID, ErrInvalidSlot, numSlots)
	}
	return 6 + slotID*5, nil
}

func (p *Page) FreeSpace() (int, error) {
	_, freeStart, freeEnd, err := p.headerValues()
	if err != nil {
		return 0, err
	}
	return freeEnd - freeStart, nil
}

func (p *Page) GetNumSlots() (int, error) {
	numSlots, _, _, err := p.headerValues()
	if err != nil {
		return 0, err
	}
	return numSlots, nil

}
