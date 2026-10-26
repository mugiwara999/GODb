package table

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/joho/godotenv"
	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/filelock"
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
)

func Open(name string) (*Table, error) {
	path, err := tablePath(name)
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}
	lock, err := filelock.Acquire(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}
	defer lock.Close()
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
	if len(cols) == 0 {
		return nil, fmt.Errorf("create table %q: at least one column name is required", name)
	}

	path, err := tablePath(name)
	if err != nil {
		return nil, fmt.Errorf("create table %q: %w", name, err)
	}
	lock, err := filelock.Acquire(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("create table %q: %w", name, err)
	}
	defer lock.Close()

	pg, err := pager.CreatePager(path)
	if err != nil {
		return nil, fmt.Errorf("create table %q at %q: %w", name, path, err)
	}

	cols = append([]string{"id"}, cols...)

	if err := pg.WriteColumns(cols); err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("create table %q at %q: %w", name, path, err)
	}

	idx, err := btree.NewIndex(name, "id")

	if err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("create table %q at %q: %w", name, path, err)
	}

	idxs := make(map[string]*btree.Index)
	idxs["id"] = idx

	lastID, err := pg.LastID()

	if err != nil {
		_ = pg.Close()
		return nil, fmt.Errorf("create table %q at %q: %w", name, path, err)
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
		dataDir = "../data"
	}
	return filepath.Join(dataDir, strings.ToLower(name)+".bin"), nil
}
