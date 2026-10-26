package table

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/joho/godotenv"
	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/filelock"
	"github.com/mugiwara999/goDB/internal/lexer"
	"github.com/mugiwara999/goDB/internal/pager"
	"github.com/mugiwara999/goDB/internal/wal"
)

type Table struct {
	Pager      *pager.Pager
	Name       string
	Path       string
	cols       []string
	Indexes    map[string]*btree.Index
	lastID     uint64
	mutationMu sync.Mutex
	poisoned   bool
}

var (
	ErrorTakingInput = errors.New("read user input")
	ErrTableExists   = errors.New("table already exists")
)

var createMarkerMagic = []byte("GODBCREATE1\n")

func Open(name string) (*Table, error) {
	var err error
	name, err = canonicalIdentifier(name, "table")
	if err != nil {
		return nil, err
	}
	path, err := tablePath(name)
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}
	lock, err := filelock.Acquire(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}
	defer lock.Close()
	if err := recoverCreate(path); err != nil {
		return nil, fmt.Errorf("recover table creation for %q: %w", name, err)
	}
	if err := wal.Recover(path + ".wal"); err != nil {
		return nil, fmt.Errorf("recover table %q: %w", name, err)
	}
	return openTable(name, path)
}

func openTable(name, path string) (*Table, error) {
	pg, err := pager.OpenPager(path)
	if err != nil {
		return nil, fmt.Errorf("open table %q at %q: %w", name, path, err)
	}

	cols, err := pg.GetColumns()
	if err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("open table %q at %q: %w", name, path, err)
	}
	cols, err = normalizeStoredColumns(name, cols)
	if err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("open table %q at %q: %w", name, path, err)
	}

	idxs := make(map[string]*btree.Index)

	for _, col := range cols {
		idx, err := btree.OpenIndex(name, col)
		if err != nil {
			if col == "id" {
				for _, opened := range idxs {
					_ = opened.Close()
				}
				_ = pg.Close()
				return nil, fmt.Errorf("open table %q primary index: %w", name, err)
			}
			continue
		}
		idxs[col] = idx
	}

	lastID, err := pg.LastID()

	if err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("open table %q at %q: %w", name, path, err)
	}

	return &Table{
		Pager:   pg,
		Name:    strings.ToLower(name),
		Path:    path,
		cols:    cols,
		Indexes: idxs,
		lastID:  lastID,
	}, nil
}

func (t *Table) mutate(fn func() error) error {
	t.mutationMu.Lock()
	defer t.mutationMu.Unlock()
	if t.poisoned {
		return fmt.Errorf("mutate table %q: table must be reopened after a failed rollback", t.Name)
	}
	lock, err := filelock.Acquire(t.Path + ".lock")
	if err != nil {
		return fmt.Errorf("lock table %q for mutation: %w", t.Name, err)
	}
	defer lock.Close()
	if err := wal.Recover(t.Path + ".wal"); err != nil {
		return fmt.Errorf("recover table %q before mutation: %w", t.Name, err)
	}
	if err := t.Pager.Refresh(); err != nil {
		return fmt.Errorf("refresh table %q before mutation: %w", t.Name, err)
	}
	for _, idx := range t.Indexes {
		if err := idx.Refresh(); err != nil {
			return fmt.Errorf("refresh table %q index before mutation: %w", t.Name, err)
		}
	}
	if t.lastID, err = t.Pager.LastID(); err != nil {
		return fmt.Errorf("refresh table %q last ID: %w", t.Name, err)
	}

	j, err := wal.Begin(t.Path + ".wal")
	if err != nil {
		return fmt.Errorf("begin table %q mutation: %w", t.Name, err)
	}
	t.Pager.SetJournal(j)
	for _, idx := range t.Indexes {
		idx.SetJournal(j)
	}
	detach := func() {
		t.Pager.SetJournal(nil)
		for _, idx := range t.Indexes {
			idx.SetJournal(nil)
		}
	}
	if err := fn(); err != nil {
		rbErr := j.Rollback()
		detach()
		if rbErr != nil {
			t.poisoned = true
			return errors.Join(fmt.Errorf("mutate table %q: %w", t.Name, err), fmt.Errorf("rollback table %q: %w", t.Name, rbErr))
		}
		if reopenErr := t.reopenAfterRollback(); reopenErr != nil {
			t.poisoned = true
			return errors.Join(fmt.Errorf("mutate table %q: %w", t.Name, err), fmt.Errorf("reopen table %q after rollback: %w", t.Name, reopenErr))
		}
		return fmt.Errorf("mutate table %q: %w", t.Name, err)
	}
	if err := j.Commit(); err != nil {
		rbErr := j.Rollback()
		detach()
		if rbErr == nil {
			rbErr = t.reopenAfterRollback()
		}
		if rbErr != nil {
			t.poisoned = true
			return errors.Join(fmt.Errorf("commit table %q mutation: %w", t.Name, err), fmt.Errorf("restore table %q after failed commit: %w", t.Name, rbErr))
		}
		return fmt.Errorf("commit table %q mutation: %w", t.Name, err)
	}
	detach()
	return nil
}

func (t *Table) reopenAfterRollback() error {
	for _, idx := range t.Indexes {
		_ = idx.Close()
	}
	if err := t.Pager.Close(); err != nil {
		return err
	}
	reopened, err := openTable(t.Name, t.Path)
	if err != nil {
		return err
	}
	t.Pager, t.cols, t.Indexes, t.lastID = reopened.Pager, reopened.cols, reopened.Indexes, reopened.lastID
	return nil
}

func Create(name string, cols []string) (*Table, error) {
	var err error
	name, err = canonicalIdentifier(name, "table")
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("create table %q: at least one column name is required", name)
	}
	canonicalCols := make([]string, 0, len(cols))
	seenCols := make(map[string]struct{}, len(cols))
	for _, col := range cols {
		canonicalCol, err := canonicalIdentifier(col, "column")
		if err != nil {
			return nil, fmt.Errorf("create table %q: %w", name, err)
		}
		if canonicalCol == "id" {
			return nil, fmt.Errorf("create table %q: column name %q is reserved for the generated primary key", name, col)
		}
		if _, exists := seenCols[canonicalCol]; exists {
			return nil, fmt.Errorf("create table %q: duplicate column name %q", name, canonicalCol)
		}
		seenCols[canonicalCol] = struct{}{}
		canonicalCols = append(canonicalCols, canonicalCol)
	}
	cols = canonicalCols

	path, err := tablePath(name)
	if err != nil {
		return nil, fmt.Errorf("create table %q: %w", name, err)
	}
	lock, err := filelock.Acquire(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("create table %q: %w", name, err)
	}
	defer lock.Close()
	if err := recoverCreate(path); err != nil {
		return nil, fmt.Errorf("recover table creation for %q: %w", name, err)
	}
	indexPath := btree.IndexPath(name, "id")
	for _, finalPath := range []string{path, indexPath} {
		exists, err := pathExists(finalPath)
		if err != nil {
			return nil, fmt.Errorf("create table %q: check existing file %q: %w", name, finalPath, err)
		}
		if exists {
			return nil, fmt.Errorf("create table %q: %w", name, ErrTableExists)
		}
	}
	if err := removeCreateTemps(path, indexPath); err != nil {
		return nil, fmt.Errorf("create table %q: remove stale staging files: %w", name, err)
	}
	return createTableFiles(name, cols, path, nil)
}

func canonicalIdentifier(name, kind string) (string, error) {
	canonical := strings.ToLower(name)
	scanner := lexer.New(canonical)
	token := scanner.NextToken()
	if token.Type != lexer.TOKEN_IDENT || token.Value != canonical || scanner.NextToken().Type != lexer.TOKEN_EOF {
		return "", fmt.Errorf("invalid %s name %q", kind, name)
	}
	return canonical, nil
}

func normalizeStoredColumns(tableName string, cols []string) ([]string, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("%w: table %q has no columns", pager.ErrMetadataCorrupt, tableName)
	}
	primary, err := canonicalIdentifier(cols[0], "column")
	if err != nil || primary != "id" {
		return nil, fmt.Errorf("%w: table %q must begin with the generated id column", pager.ErrMetadataCorrupt, tableName)
	}
	normalized := make([]string, len(cols))
	normalized[0] = "id"
	seen := map[string]struct{}{"id": {}}
	for i, col := range cols[1:] {
		canonical, err := canonicalIdentifier(col, "column")
		if err != nil {
			return nil, fmt.Errorf("%w: table %q has invalid column %q", pager.ErrMetadataCorrupt, tableName, col)
		}
		if _, exists := seen[canonical]; exists {
			return nil, fmt.Errorf("%w: table %q has duplicate column %q", pager.ErrMetadataCorrupt, tableName, canonical)
		}
		seen[canonical] = struct{}{}
		normalized[i+1] = canonical
	}
	return normalized, nil
}

// createTableFiles assumes the caller holds the table lock and has checked
// that neither final file exists. The optional hook is used by package tests
// to simulate interruption between publication steps.
func createTableFiles(name string, cols []string, path string, afterStep func(string) error) (*Table, error) {
	indexPath := btree.IndexPath(name, "id")
	tableTemp := path + ".create-tmp"
	indexTemp := indexPath + ".create-tmp"
	markerPath := path + ".create"
	markerTemp := markerPath + ".tmp"
	cleanupTemps := func() error { return removePaths(tableTemp, indexTemp, markerTemp) }

	pg, err := pager.CreatePager(tableTemp)
	if err != nil {
		_ = cleanupTemps()
		return nil, fmt.Errorf("create staged table %q at %q: %w", name, tableTemp, err)
	}

	closePager := func() error { return pg.Close() }
	stagedCols := append([]string{"id"}, cols...)

	if err := pg.WriteColumns(stagedCols); err != nil {
		_ = closePager()
		_ = cleanupTemps()
		return nil, fmt.Errorf("write staged table metadata for %q: %w", name, err)
	}

	idx, err := btree.NewIndexAt(name, "id", indexTemp)
	if err != nil {
		_ = closePager()
		_ = cleanupTemps()
		return nil, fmt.Errorf("create staged primary index for %q: %w", name, err)
	}
	if err := pg.Sync(); err != nil {
		_ = idx.Close()
		_ = closePager()
		_ = cleanupTemps()
		return nil, err
	}
	if err := idx.Sync(); err != nil {
		_ = idx.Close()
		_ = closePager()
		_ = cleanupTemps()
		return nil, err
	}
	idxCloseErr := idx.Close()
	pageCloseErr := closePager()
	if err := errors.Join(idxCloseErr, pageCloseErr); err != nil {
		_ = cleanupTemps()
		return nil, fmt.Errorf("close staged table files for %q: %w", name, err)
	}
	if err := writeCreateMarker(markerTemp, markerPath); err != nil {
		_ = cleanupTemps()
		return nil, fmt.Errorf("write create marker for %q: %w", name, err)
	}
	if err := runCreateStep(afterStep, "after_marker"); err != nil {
		return nil, cleanupFailedCreate(path, err)
	}
	if err := os.Rename(tableTemp, path); err != nil {
		return nil, cleanupFailedCreate(path, fmt.Errorf("publish table file: %w", err))
	}
	if err := runCreateStep(afterStep, "after_table_publish"); err != nil {
		return nil, cleanupFailedCreate(path, err)
	}
	if err := os.Rename(indexTemp, indexPath); err != nil {
		return nil, cleanupFailedCreate(path, fmt.Errorf("publish primary index: %w", err))
	}
	if err := runCreateStep(afterStep, "after_index_publish"); err != nil {
		return nil, cleanupFailedCreate(path, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return nil, cleanupFailedCreate(path, fmt.Errorf("sync published table files: %w", err))
	}
	if err := os.Remove(markerPath); err != nil {
		return nil, cleanupFailedCreate(path, fmt.Errorf("remove create marker: %w", err))
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("sync completed table creation for %q: %w", name, err)
	}
	created, err := openTable(name, path)
	if err != nil {
		return nil, fmt.Errorf("open newly created table %q: %w", name, err)
	}
	return created, nil
}

func runCreateStep(hook func(string) error, step string) error {
	if hook == nil {
		return nil
	}
	return hook(step)
}

func cleanupFailedCreate(path string, cause error) error {
	if err := recoverCreate(path); err != nil {
		return errors.Join(cause, fmt.Errorf("clean up interrupted create: %w", err))
	}
	return cause
}

func writeCreateMarker(tempPath, markerPath string) error {
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(createMarkerMagic)
	if writeErr == nil && n != len(createMarkerMagic) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(tempPath, markerPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(markerPath))
}

func recoverCreate(path string) error {
	markerPath := path + ".create"
	marker, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(marker, createMarkerMagic) {
		return fmt.Errorf("invalid create marker %q", markerPath)
	}
	tableName := strings.TrimSuffix(filepath.Base(path), ".bin")
	indexPath := btree.IndexPath(tableName, "id")
	if err := removePaths(path, indexPath, path+".create-tmp", indexPath+".create-tmp", markerPath+".tmp"); err != nil {
		return err
	}
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func removeCreateTemps(path, indexPath string) error {
	return removePaths(path+".create-tmp", indexPath+".create-tmp", path+".create.tmp")
}

func removePaths(paths ...string) error {
	var errs []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (t *Table) Close() error {
	if t == nil || t.Pager == nil {
		return nil
	}
	err := t.Pager.Close()

	if err != nil {
		return fmt.Errorf("close table %q: %w", t.Name, err)
	}

	for _, idx := range t.Indexes {
		if err := idx.Close(); err != nil {
			name, err := idx.Name()
			if err != nil {
				return fmt.Errorf("close index: %w", err)
			}
			return fmt.Errorf("close index %q: %w", name, err)
		}
	}

	return nil

}

func (t *Table) GetColumns() []string {
	cols := make([]string, len(t.cols))
	copy(cols, t.cols)
	return cols
}

func tablePath(name string) (string, error) {
	_ = godotenv.Load()
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	return filepath.Join(dataDir, strings.ToLower(name)+".bin"), nil
}
