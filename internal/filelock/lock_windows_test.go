//go:build windows

package filelock

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsLockChildProcess(t *testing.T) {
	if os.Getenv("GODB_WINDOWS_FILELOCK_CHILD") != "1" {
		return
	}
	lockPath := os.Getenv("GODB_WINDOWS_FILELOCK_PATH")
	startedPath := os.Getenv("GODB_WINDOWS_FILELOCK_STARTED")
	acquiredPath := os.Getenv("GODB_WINDOWS_FILELOCK_ACQUIRED")
	releasePath := os.Getenv("GODB_WINDOWS_FILELOCK_RELEASE")
	if err := os.WriteFile(startedPath, []byte("started"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := os.WriteFile(acquiredPath, []byte("acquired"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(releasePath); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for parent release signal")
}

func TestExclusiveLockBlocksOtherWindowsProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "table.lock")
	startedPath := filepath.Join(dir, "started")
	acquiredPath := filepath.Join(dir, "acquired")
	releasePath := filepath.Join(dir, "release")
	first, err := Acquire(lockPath)
	if err != nil {
		t.Fatalf("Acquire first lock: %v", err)
	}
	defer first.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsLockChildProcess$")
	cmd.Env = append(os.Environ(),
		"GODB_WINDOWS_FILELOCK_CHILD=1",
		"GODB_WINDOWS_FILELOCK_PATH="+lockPath,
		"GODB_WINDOWS_FILELOCK_STARTED="+startedPath,
		"GODB_WINDOWS_FILELOCK_ACQUIRED="+acquiredPath,
		"GODB_WINDOWS_FILELOCK_RELEASE="+releasePath,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock process: %v", err)
	}
	waited := false
	defer func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(startedPath); err != nil {
		t.Fatalf("child did not start lock request: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(acquiredPath); err == nil {
		t.Fatal("second process acquired the lock before the first released it")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(acquiredPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(acquiredPath); err != nil {
		t.Fatalf("child did not acquire lock after release: %v", err)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		waited = true
		t.Fatalf("child lock process: %v", err)
	}
	waited = true
}
