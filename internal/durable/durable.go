// Package durable provides process ownership and rename durability for state files.
package durable

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func SyncDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// WriteJSON atomically publishes an independently recoverable archive record.
func WriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDirectory(path)
}

func ArchivePath(stateFile, category, id string) (string, error) {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return "", fmt.Errorf("invalid archive identifier")
	}
	return filepath.Join(filepath.Dir(stateFile), "archive", category, id+".json"), nil
}

// Acquire prevents two OpenDev processes from overwriting the same JSON stores.
// Keep the returned file open for the lifetime of the process.
func Acquire(directory string) (*os.File, error) {
	if err := os.MkdirAll(directory, 0750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, ".opendev.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("state directory is already in use: %w", err)
	}
	return file, nil
}
