package table

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/mugiwara999/goDB/internal/btree"
	"github.com/mugiwara999/goDB/internal/pager"
)

type Table struct {
	Pager   *pager.Pager
	Name    string
	Path    string
	cols    []string
	Indexes map[string]*btree.Index
	lastID  uint64
}

var (
	ErrorTakingInput = errors.New("read user input")
)

func Open(name string) (*Table, error) {
	path, err := tablePath(name)
	if err != nil {
		return nil, fmt.Errorf("open table %q: %w", name, err)
	}

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

func Create(name string, cols []string) (*Table, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("create table %q: at least one column name is required", name)
	}

	path, err := tablePath(name)
	if err != nil {
		return nil, fmt.Errorf("create table %q: %w", name, err)
	}

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
	if err := godotenv.Load(); err != nil {
		return "../data/" + strings.ToLower(name) + ".bin", nil
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "../data"
	}
	return dataDir + "/" + strings.ToLower(name) + ".bin", nil
}
