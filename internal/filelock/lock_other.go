//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package filelock

import (
	"fmt"
	"path/filepath"
	"sync"
)

var locks sync.Map

type Lock struct{ mutex *sync.Mutex }

func Acquire(path string) (*Lock, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve lock path %q: %w", path, err)
	}
	value, _ := locks.LoadOrStore(absPath, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return &Lock{mutex: mutex}, nil
}

func (l *Lock) Close() error {
	if l != nil && l.mutex != nil {
		l.mutex.Unlock()
		l.mutex = nil
	}
	return nil
}
