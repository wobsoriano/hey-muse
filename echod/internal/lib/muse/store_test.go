package muse

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileStoreSavesWholeAndPrivate(t *testing.T) {
	dir := t.TempDir()
	store := FileStore{Path: filepath.Join(dir, "state.json")}
	if _, err := store.Load(); err == nil {
		t.Error("Load of a missing file succeeded")
	}
	first := State{Identity: Identity{MAC: "02:11:22:a1:b2:c3"}, Credentials: &Credentials{AccessToken: "one"}}
	second := first
	second.Credentials = &Credentials{AccessToken: "two", RefreshToken: "r"}
	second.SDKTokenReported = true
	for _, st := range []State{first, second} {
		if err := store.Save(st); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load()
		if err != nil || !reflect.DeepEqual(got, st) {
			t.Errorf("Load = %+v, %v", got, err)
		}
	}
	info, err := os.Stat(store.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
	// A save that cannot land leaves the old file as it was.
	if err := (FileStore{Path: filepath.Join(dir, "missing", "state.json")}).Save(first); err == nil {
		t.Error("Save into a missing directory succeeded")
	}
	if got, _ := store.Load(); !reflect.DeepEqual(got, second) {
		t.Errorf("the file changed: %+v", got)
	}
}
