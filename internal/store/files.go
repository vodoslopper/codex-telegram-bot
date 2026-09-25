package store

import (
	"os"
	"path/filepath"
)

// createPrivate creates path 0600 if it does not exist, and tightens it if it
// does. An existing file with the wrong mode is fixed rather than reported: the
// bot can still work, and the fix is what the operator wanted.
func createPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	return os.Chmod(path, 0o600)
}

// mkdirAll is os.MkdirAll with the mode re-asserted afterwards.
//
// MkdirAll only applies mode to directories it creates, and it masks the mode
// with the process umask. A systemd unit with UMask=0077 gives 0700 anyway, but
// a bot started from a login shell with umask 022 would leave 0755 — readable by
// every local user. The explicit chmod makes the result independent of how the
// process was started.
func mkdirAll(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	// Best effort: some filesystems refuse chmod, and failing the whole
	// startup over that would be worse than logging it later.
	_ = os.Chmod(dir, mode)
	return nil
}

// FileModeOf returns the permission bits of path, for the startup log and for
// tests that assert the state directory is private.
func FileModeOf(path string) (os.FileMode, error) {
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return 0, err
	}
	return info.Mode().Perm(), nil
}
