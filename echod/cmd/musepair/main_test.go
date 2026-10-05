//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

func TestIdentity(t *testing.T) {
	fresh := regexp.MustCompile(`^homelink-[0-9a-f]{6} MuseGadget[0-9A-F]{6} [0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	cases := []struct {
		nodeID, bleName string
		want            string // "" for a fresh identity, "error" for a refusal
	}{
		{"", "", ""},
		{"homelink-a1b2c3", "", "homelink-a1b2c3 MuseGadgetA1B2C3"},
		{"", "MuseGadgetA1B2C3", "homelink-a1b2c3 MuseGadgetA1B2C3"},
		{"homelink-a1b2c3", "MuseGadgetA1B2C3", "homelink-a1b2c3 MuseGadgetA1B2C3"},
		{"homelink-a1b2c3", "MuseGadgetFFFFFF", "error"},
		{"homelink-A1B2C3", "", "error"},
		{"homelink-a1b2", "", "error"},
		{"kitchen", "", "error"},
		{"", "Kitchen", "error"},
	}
	for _, c := range cases {
		d, err := identity(c.nodeID, c.bleName)
		if c.want == "error" {
			if err == nil {
				t.Errorf("identity(%q, %q) = %+v, want an error", c.nodeID, c.bleName, d)
			}
			continue
		}
		if err != nil {
			t.Errorf("identity(%q, %q): %v", c.nodeID, c.bleName, err)
			continue
		}
		got := d.NodeID + " " + d.BLEName + " " + d.MAC
		if !fresh.MatchString(got) || !strings.HasPrefix(got, c.want) {
			t.Errorf("identity(%q, %q) = %s", c.nodeID, c.bleName, got)
		}
		// A muse.State derives the names from the MAC's last six digits.
		if tail := strings.ReplaceAll(d.MAC, ":", "")[6:]; d.NodeID != "homelink-"+tail {
			t.Errorf("MAC %s does not end in the node id %s", d.MAC, d.NodeID)
		}
		if first := d.MAC[1]; !strings.ContainsRune("26ae", rune(first)) {
			t.Errorf("MAC %s is not unicast and locally administered", d.MAC)
		}
	}
	a, _ := identity("", "")
	b, _ := identity("", "")
	if a.NodeID == b.NodeID {
		t.Errorf("two fresh identities are both %s", a.NodeID)
	}
}

func TestSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	d := pairing.Device{NodeID: "homelink-a1b2c3", BLEName: "MuseGadgetA1B2C3", MAC: "02:11:22:a1:b2:c3"}
	r := pairing.Result{AccessToken: "a", RefreshToken: "r", TokenType: "device", Username: "u", APIURL: "https://old", APIURLV2: "https://new", NoiseHost: "h"}
	if err := save(path, d, r, time.Unix(7, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var compact map[string]any
	if err := json.Unmarshal(b, &compact); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(compact)
	// The same state muse's codec test writes, less the flag a fresh pairing has not earned.
	want := `{"credentials":{"access_token":"a","access_token_saved_at":7,"api_url":"https://old","api_url_v2":"https://new","noise_host":"h","refresh_token":"r","token_type":"device","username":"u"},"identity":{"mac":"02:11:22:a1:b2:c3"}}`
	if string(got) != want {
		t.Errorf("state\n got %s\nwant %s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("%d files left in the directory, want only the state", len(entries))
	}

	if err := save(filepath.Join(dir, "missing", "state.json"), d, r, time.Unix(7, 0)); err == nil {
		t.Error("save into a missing directory succeeded")
	}
}
