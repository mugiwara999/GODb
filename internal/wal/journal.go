package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	recordFileStart  byte = 1
	recordPageBefore byte = 2
	recordCommit     byte = 3
	maxRecordSize         = 1 << 20
)

var journalMagic = []byte("GODBWL1\n")

type fileState struct {
	path string
	size int64
	file *os.File
}

type pageRef struct {
	path string
	id   uint32
	size uint32
}

// Journal records page before-images before the corresponding data-file write.
// A missing commit record means recovery must restore the original pages and
// truncate files to their sizes at the start of the transaction.
type Journal struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	files     map[string]fileState
	seen      map[pageRef]struct{}
	fault     func(string) error
	closed    bool
	committed bool
}

type pageBefore struct {
	path string
	id   uint32
	size uint32
	data []byte
}

type parsedLog struct {
	files     map[string]int64
	pages     []pageBefore
	committed bool
}

func Begin(path string) (*Journal, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve journal path %q: %w", path, err)
	}
	file, err := os.OpenFile(absolutePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create journal %q: %w", absolutePath, err)
	}
	j := &Journal{
		path:  absolutePath,
		file:  file,
		files: make(map[string]fileState),
		seen:  make(map[pageRef]struct{}),
	}
	if err := writeFull(file, journalMagic); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write journal header: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync journal header: %w", err)
	}
	if err := syncDirectory(filepath.Dir(absolutePath)); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync journal directory: %w", err)
	}
	return j, nil
}

// BeforePageWrite persists the original page image before allowing a caller to
// modify it. New pages are covered by the original file-size record instead.
func (j *Journal) BeforePageWrite(path string, file *os.File, pageID, pageSize uint32) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return fmt.Errorf("journal %q is closed", j.path)
	}
	if file == nil || pageSize == 0 || pageSize > maxRecordSize {
		return fmt.Errorf("invalid page write target")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve data file path %q: %w", path, err)
	}
	state, exists := j.files[absolutePath]
	if !exists {
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("stat data file %q: %w", absolutePath, err)
		}
		state = fileState{path: absolutePath, size: info.Size(), file: file}
		payload := make([]byte, 12+len(absolutePath))
		binary.LittleEndian.PutUint32(payload[:4], uint32(len(absolutePath)))
		binary.LittleEndian.PutUint64(payload[4:12], uint64(info.Size()))
		copy(payload[12:], absolutePath)
		if err := j.appendRecord(recordFileStart, payload); err != nil {
			return fmt.Errorf("record original size for %q: %w", absolutePath, err)
		}
		j.files[absolutePath] = state
	}
	state.file = file
	j.files[absolutePath] = state

	ref := pageRef{path: absolutePath, id: pageID, size: pageSize}
	if _, exists := j.seen[ref]; exists {
		return nil
	}
	j.seen[ref] = struct{}{}
	offset := int64(pageID) * int64(pageSize)
	if offset >= state.size {
		return nil
	}
	if offset+int64(pageSize) > state.size {
		delete(j.seen, ref)
		return fmt.Errorf("page %d partially exceeds original file size %d", pageID, state.size)
	}
	image := make([]byte, pageSize)
	n, err := file.ReadAt(image, offset)
	if err != nil || n != len(image) {
		delete(j.seen, ref)
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("read before-image for %q page %d: %w", absolutePath, pageID, err)
	}
	payload := encodePageBefore(absolutePath, pageID, pageSize, image)
	if err := j.appendRecord(recordPageBefore, payload); err != nil {
		delete(j.seen, ref)
		return fmt.Errorf("record before-image for %q page %d: %w", absolutePath, pageID, err)
	}
	return nil
}

func (j *Journal) appendRecord(kind byte, payload []byte) error {
	point := "record_unknown"
	switch kind {
	case recordFileStart:
		point = "record_file_start"
	case recordPageBefore:
		point = "record_page_before"
	case recordCommit:
		point = "record_commit"
	}
	if j.fault != nil {
		if err := j.fault(point); err != nil {
			return err
		}
	}
	if len(payload) > maxRecordSize {
		return fmt.Errorf("journal record is too large: %d bytes", len(payload))
	}
	frame := make([]byte, 5+len(payload)+4)
	frame[0] = kind
	binary.LittleEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	checksum := crc32.ChecksumIEEE(frame[:5+len(payload)])
	binary.LittleEndian.PutUint32(frame[5+len(payload):], checksum)
	if err := writeFull(j.file, frame); err != nil {
		return err
	}
	return j.file.Sync()
}

func (j *Journal) Commit() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return fmt.Errorf("journal %q is closed", j.path)
	}
	for path, state := range j.files {
		if j.fault != nil {
			if err := j.fault("sync_data_file"); err != nil {
				return fmt.Errorf("injected data-file sync failure for %q: %w", path, err)
			}
		}
		if err := state.file.Sync(); err != nil {
			return fmt.Errorf("sync data file %q before commit: %w", path, err)
		}
	}
	if err := j.appendRecord(recordCommit, nil); err != nil {
		return fmt.Errorf("write commit record: %w", err)
	}
	j.committed = true
	j.closed = true
	_ = j.file.Close()
	// The commit marker is durable; a leftover journal is safe and will be
	// removed by Recover on the next open.
	if err := os.Remove(j.path); err == nil {
		_ = syncDirectory(filepath.Dir(j.path))
	}
	return nil
}

func (j *Journal) Rollback() error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return fmt.Errorf("journal %q is closed", j.path)
	}
	if err := j.file.Sync(); err != nil {
		j.mu.Unlock()
		return fmt.Errorf("sync journal before rollback: %w", err)
	}
	j.closed = true
	closeErr := j.file.Close()
	j.mu.Unlock()
	if closeErr != nil {
		return fmt.Errorf("close journal before rollback: %w", closeErr)
	}
	return Recover(j.path)
}

// CloseUncommitted simulates a process exit: it leaves the journal in place so
// the next Recover call can undo the transaction.
func (j *Journal) CloseUncommitted() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.file.Close()
}

func Recover(path string) error {
	return recoverWith(path, nil)
}

func recoverWith(path string, fault func(string, *os.File, []byte, int64) error) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve journal path %q: %w", path, err)
	}
	log, err := readLog(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read journal %q: %w", absPath, err)
	}
	journalDir := filepath.Dir(absPath)
	for dataPath := range log.files {
		if filepath.Dir(filepath.Clean(dataPath)) != journalDir {
			return fmt.Errorf("journal %q names data file outside its directory: %q", absPath, dataPath)
		}
	}
	if !log.committed {
		for i := len(log.pages) - 1; i >= 0; i-- {
			page := log.pages[i]
			file, err := os.OpenFile(page.path, os.O_RDWR, 0)
			if err != nil {
				return fmt.Errorf("open %q during recovery: %w", page.path, err)
			}
			offset := int64(page.id) * int64(page.size)
			var restoreErr error
			if fault != nil {
				restoreErr = fault("restore_page", file, page.data, offset)
			} else {
				restoreErr = writeAtFull(file, page.data, offset)
			}
			if restoreErr != nil {
				_ = file.Close()
				return fmt.Errorf("restore %q page %d: %w", page.path, page.id, restoreErr)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close recovered file %q: %w", page.path, err)
			}
		}
		for filePath, originalSize := range log.files {
			file, err := os.OpenFile(filePath, os.O_RDWR, 0)
			if err != nil {
				return fmt.Errorf("open %q to restore file size: %w", filePath, err)
			}
			if fault != nil {
				if err := fault("truncate_file", file, nil, originalSize); err != nil {
					_ = file.Close()
					return fmt.Errorf("restore size of %q: %w", filePath, err)
				}
			}
			if err := file.Truncate(originalSize); err != nil {
				_ = file.Close()
				return fmt.Errorf("restore size of %q: %w", filePath, err)
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				return fmt.Errorf("sync restored file %q: %w", filePath, err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close restored file %q: %w", filePath, err)
			}
		}
	}
	if err := os.Remove(absPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovered journal %q: %w", absPath, err)
	}
	_ = syncDirectory(filepath.Dir(absPath))
	return nil
}

func readLog(path string) (*parsedLog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := io.Reader(file)
	header := make([]byte, len(journalMagic))
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if !bytes.Equal(header, journalMagic) {
		return nil, fmt.Errorf("invalid journal magic")
	}
	result := &parsedLog{files: make(map[string]int64)}
	for {
		start, _ := file.Seek(0, io.SeekCurrent)
		var recordHeader [5]byte
		_, err := io.ReadFull(file, recordHeader[:])
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			break // a torn final record cannot have authorized a data write
		}
		if err != nil {
			return nil, err
		}
		length := binary.LittleEndian.Uint32(recordHeader[1:5])
		if length > maxRecordSize {
			return nil, fmt.Errorf("record length %d exceeds limit", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(file, payload); errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		var checksumBytes [4]byte
		if _, err := io.ReadFull(file, checksumBytes[:]); errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		frame := make([]byte, 5+len(payload))
		copy(frame[:5], recordHeader[:])
		copy(frame[5:], payload)
		got := binary.LittleEndian.Uint32(checksumBytes[:])
		if crc32.ChecksumIEEE(frame) != got {
			return nil, fmt.Errorf("corrupt journal record at offset %d", start)
		}
		if result.committed {
			return nil, fmt.Errorf("journal contains records after commit")
		}
		switch recordHeader[0] {
		case recordFileStart:
			filePath, originalSize, err := decodeFileStart(payload)
			if err != nil {
				return nil, err
			}
			if _, exists := result.files[filePath]; exists {
				return nil, fmt.Errorf("duplicate file-start record for %q", filePath)
			}
			result.files[filePath] = originalSize
		case recordPageBefore:
			page, err := decodePageBefore(payload)
			if err != nil {
				return nil, err
			}
			if _, exists := result.files[page.path]; !exists {
				return nil, fmt.Errorf("page image for %q precedes file-start record", page.path)
			}
			result.pages = append(result.pages, page)
		case recordCommit:
			if len(payload) != 0 {
				return nil, fmt.Errorf("commit record has unexpected payload")
			}
			result.committed = true
		default:
			return nil, fmt.Errorf("unknown journal record type %d", recordHeader[0])
		}
	}
	seenPages := make(map[pageRef]struct{}, len(result.pages))
	pageSizes := make(map[string]uint32)
	for _, page := range result.pages {
		ref := pageRef{path: page.path, id: page.id, size: page.size}
		if _, exists := seenPages[ref]; exists {
			return nil, fmt.Errorf("duplicate before-image for %q page %d", page.path, page.id)
		}
		seenPages[ref] = struct{}{}
		if size, exists := pageSizes[page.path]; exists && size != page.size {
			return nil, fmt.Errorf("inconsistent page sizes for %q", page.path)
		}
		pageSizes[page.path] = page.size
		originalSize := result.files[page.path]
		offset := int64(page.id) * int64(page.size)
		if offset+int64(page.size) > originalSize {
			return nil, fmt.Errorf("before-image for %q page %d exceeds original file size %d", page.path, page.id, originalSize)
		}
	}
	return result, nil
}

func decodeFileStart(payload []byte) (string, int64, error) {
	if len(payload) < 12 {
		return "", 0, fmt.Errorf("short file-start record")
	}
	pathLen := binary.LittleEndian.Uint32(payload[:4])
	if uint64(pathLen)+12 != uint64(len(payload)) {
		return "", 0, fmt.Errorf("invalid file-start path length %d", pathLen)
	}
	path := string(payload[12:])
	if !filepath.IsAbs(path) {
		return "", 0, fmt.Errorf("journal contains non-absolute data path %q", path)
	}
	size := binary.LittleEndian.Uint64(payload[4:12])
	if size > uint64(^uint64(0)>>1) {
		return "", 0, fmt.Errorf("invalid original size for %q", path)
	}
	return path, int64(size), nil
}

func decodePageBefore(payload []byte) (pageBefore, error) {
	if len(payload) < 12 {
		return pageBefore{}, fmt.Errorf("short page-before record")
	}
	pathLen := binary.LittleEndian.Uint32(payload[:4])
	pageSize := binary.LittleEndian.Uint32(payload[4:8])
	pageID := binary.LittleEndian.Uint32(payload[8:12])
	if pageSize == 0 || pageSize > maxRecordSize || uint64(pathLen)+12+uint64(pageSize) != uint64(len(payload)) {
		return pageBefore{}, fmt.Errorf("invalid page-before record dimensions")
	}
	path := string(payload[12 : 12+pathLen])
	if !filepath.IsAbs(path) {
		return pageBefore{}, fmt.Errorf("journal contains non-absolute data path %q", path)
	}
	return pageBefore{path: path, id: pageID, size: pageSize, data: append([]byte(nil), payload[12+pathLen:]...)}, nil
}

func encodePageBefore(path string, pageID, pageSize uint32, image []byte) []byte {
	payload := make([]byte, 12+len(path)+len(image))
	binary.LittleEndian.PutUint32(payload[:4], uint32(len(path)))
	binary.LittleEndian.PutUint32(payload[4:8], pageSize)
	binary.LittleEndian.PutUint32(payload[8:12], pageID)
	copy(payload[12:], path)
	copy(payload[12+len(path):], image)
	return payload
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func writeAtFull(file *os.File, data []byte, offset int64) error {
	for len(data) > 0 {
		n, err := file.WriteAt(data, offset)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
		offset += int64(n)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
