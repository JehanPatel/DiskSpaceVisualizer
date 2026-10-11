//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

const trashName = "Trash"
const canPickFolder = false

var extraSkipNames = map[string]bool{}

func systemSkipPaths() []string { return []string{} }

func listDrives() []string { return []string{"/"} }

func moveToTrash(path string) error {
	base, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	trash := filepath.Join(base, ".Trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(trash, filepath.Base(path)))
}

func openBrowser(url string) {
	for _, name := range []string{"xdg-open", "open"} {
		if p, err := exec.LookPath(name); err == nil {
			_ = exec.Command(p, url).Start()
			return
		}
	}
}

func pickFolder() (string, error) {
	return "", errors.New("Folder picking is not available on this platform. Type a path instead.")
}

func revealPath(path string) error {
	if path == "" {
		return errors.New("No file path was provided.")
	}
	dir := filepath.Dir(path)
	for _, name := range []string{"xdg-open", "open"} {
		if p, err := exec.LookPath(name); err == nil {
			return exec.Command(p, dir).Start()
		}
	}
	return errors.New("No file manager opener was found.")
}
