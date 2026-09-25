package config

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// lookPath resolves a bare command name against PATH and returns an absolute
// path. exec.LookPath may return a relative path when the name was found in the
// current directory, and a relative path is not acceptable in a systemd unit
// whose working directory is not the operator's shell.
func lookPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("not found on PATH: %w", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("cannot make %q absolute: %w", p, err)
	}
	return abs, nil
}
