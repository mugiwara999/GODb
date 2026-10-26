//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package filelock

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockChildProcess(t *testing.T) {
	if os.Getenv("GODB_FILELOCK_CHILD") != "1" {
		return
	}
	lockPath := os.Getenv("GODB_FILELOCK_PATH")
	readyPath := os.Getenv("GODB_FILELOCK_READY")
	releasePath := os.Getenv("GODB_FILELOCK_RELEASE")
	lock, err := Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := os.WriteFile(readyPath, []byte("locked"), 0o600); err != nil {
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

func TestExclusiveLockBlocksOtherProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "table.lock")
	readyPath := filepath.Join(dir, "ready")
	releasePath := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockChildProcess$")
	cmd.Env = append(os.Environ(),
		"GODB_FILELOCK_CHILD=1",
		"GODB_FILELOCK_PATH="+lockPath,
		"GODB_FILELOCK_READY="+readyPath,
		"GODB_FILELOCK_RELEASE="+releasePath,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
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
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if cmd.ProcessState != nil {
			t.Fatalf("lock holder exited early: %s", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("lock holder did not acquire lock: %v; output: %s", err, output.String())
	}

	acquired := make(chan *Lock, 1)
	acquireErr := make(chan error, 1)
	go func() {
		lock, err := Acquire(lockPath)
		if err != nil {
			acquireErr <- err
			return
		}
		acquired <- lock
	}()
	select {
	case lock := <-acquired:
		_ = lock.Close()
		t.Fatal("second process lock request did not block")
	case err := <-acquireErr:
		t.Fatalf("second process lock request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		waited = true
		t.Fatalf("lock holder process: %v; output: %s", err, output.String())
	}
	waited = true
	select {
	case lock := <-acquired:
		if err := lock.Close(); err != nil {
			t.Fatalf("release second lock: %v", err)
		}
	case err := <-acquireErr:
		t.Fatalf("second process lock request after release: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal(fmt.Sprintf("lock was not released when child exited; output: %s", output.String()))
	}
}
