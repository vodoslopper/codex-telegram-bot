package lockfile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poller.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("the first Acquire failed: %v", err)
	}
	if first.Path() != path {
		t.Errorf("Path() = %q, want %q", first.Path(), path)
	}

	// A second poller in the same process must be refused: that is the exact
	// situation a stray foreground run creates next to an active service.
	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Errorf("the second Acquire = %v, want ErrHeld", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// And after a release the lock is available again.
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLockFileIsPrivateAndRecordsTheOwner(t *testing.T) {
	// Acquire does not create parent directories: config.Load has already
	// created BOT_STATE_DIR (0700) by the time the lock is taken.
	path := filepath.Join(t.TempDir(), "poller.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lock.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the lock file is group/world accessible: mode %o", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) == 0 {
		t.Error("the lock file does not say which process holds it")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poller.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("the second Close = %v, want nil", err)
	}
	var nilLock *Lock
	if err := nilLock.Close(); err != nil {
		t.Errorf("Close on a nil Lock = %v, want nil", err)
	}
	// Path is called on the error path of main, where Acquire returned no lock.
	// A nil dereference there would turn a clean refusal into a crash.
	if got := nilLock.Path(); got != "" {
		t.Errorf("Path on a nil Lock = %q, want the empty string", got)
	}
}

func TestAcquireReportsAMissingDirectory(t *testing.T) {
	// The lock file's directory is BOT_STATE_DIR, which config.Load has already
	// created; a missing one is a real error rather than something to paper over
	// by creating directories in two places.
	if _, err := Acquire(filepath.Join(t.TempDir(), "no", "such", "dir", "x.lock")); err == nil {
		t.Error("Acquire succeeded in a directory that does not exist")
	}
}

// TestKernelReleasesOnProcessDeath documents why flock is used instead of a PID
// file: it cannot be left behind by a crash. The check needs a second process, so
// it re-runs this test binary as a child that takes the lock and is then killed.
func TestKernelReleasesOnProcessDeath(t *testing.T) {
	if os.Getenv("LOCKFILE_TEST_CHILD") != "" {
		lock, err := Acquire(os.Getenv("LOCKFILE_TEST_PATH"))
		if err != nil {
			os.Exit(1)
		}
		_ = lock
		os.Stdout.WriteString("held\n")
		select {} // wait to be killed
	}

	path := filepath.Join(t.TempDir(), "poller.lock")
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary: %v", err)
	}

	cmd := exec.Command(self, "-test.run=TestKernelReleasesOnProcessDeath")
	cmd.Env = append(os.Environ(), "LOCKFILE_TEST_CHILD=1", "LOCKFILE_TEST_PATH="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait until the child says it holds the lock.
	buf := make([]byte, 16)
	if _, err := out.Read(buf); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("the child never reported holding the lock: %v", err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		_ = cmd.Process.Kill()
		t.Fatalf("Acquire while the child holds it = %v, want ErrHeld", err)
	}

	// Kill it the way an OOM killer or a power cut would: no Close, no unlock.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_, _ = cmd.Process.Wait()

	if _, err := Acquire(path); err != nil {
		t.Errorf("the lock survived the death of its owner: %v", err)
	}
}
