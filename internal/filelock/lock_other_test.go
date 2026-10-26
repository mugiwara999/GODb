//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package filelock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireSerializesLocksWithinProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire first lock: %v", err)
	}

	started := make(chan struct{})
	acquired := make(chan *Lock, 1)
	failed := make(chan error, 1)
	go func() {
		close(started)
		second, err := Acquire(path)
		if err != nil {
			failed <- err
			return
		}
		acquired <- second
	}()
	<-started

	select {
	case lock := <-acquired:
		_ = lock.Close()
		t.Fatal("second lock acquired before the first lock was released")
	case err := <-failed:
		t.Fatalf("Acquire second lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close first lock: %v", err)
	}
	select {
	case lock := <-acquired:
		if err := lock.Close(); err != nil {
			t.Fatalf("Close second lock: %v", err)
		}
	case err := <-failed:
		t.Fatalf("Acquire second lock after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second lock did not acquire after first lock was released")
	}
}
