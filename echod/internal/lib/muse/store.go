package muse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileStore keeps the State in one JSON file, owner-only, written whole.
type FileStore struct{ Path string }

// Load reads the file. A missing file is an error, since nothing distinguishes a device that was
// never paired from one whose state has gone: the caller decides what a missing file means.
func (f FileStore) Load() (State, error) {
	var st State
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", f.Path, err)
	}
	return st, nil
}

// Save writes beside the file and renames over it, so a crash cannot leave half a pairing, and
// does not return until the new file would survive a power cut.
func (f FileStore) Save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.Path)
	// CreateTemp makes the file 0600, which is what tokens want.
	tmp, err := os.CreateTemp(dir, filepath.Base(f.Path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), f.Path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
