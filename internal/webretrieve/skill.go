package webretrieve

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed skills/web-access-barriers/SKILL.md
var barrierSkill []byte

// InstallSkill replaces the former MCP instructions in the bot's dedicated home.
func InstallSkill(home string) error {
	dir := filepath.Join(home, "skills", "web-access-barriers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".skill-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(barrierSkill); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "SKILL.md"))
}
