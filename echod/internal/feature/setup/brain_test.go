package setup

import (
	"net/url"
	"strings"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/config"
)

// Muse needs both of its secrets before it can answer, whether they were saved before or arrive with
// the form, and a save keeps the secrets it was not given.
func TestSaveBrainMuseNeedsItsSecrets(t *testing.T) {
	f, c := in(t)
	muse := func(extra url.Values) url.Values {
		v := url.Values{"what": {"brain"}, "tab": {"sound"}, "mode": {"muse"}, "speechvoice": {"coral"}}
		for k, vals := range extra {
			v[k] = vals
		}
		return v
	}
	if to := post(t, f, c, muse(nil)); !strings.Contains(to.Query().Get("problem"), "SDK token and the speech key") {
		t.Fatalf("Muse with no secrets was saved: %q", to.Query().Get("problem"))
	}
	if to := post(t, f, c, muse(url.Values{"sdk": {"mgst_abc"}})); to.Query().Get("problem") == "" {
		t.Fatal("Muse with only the SDK token was saved")
	}
	if config.Get().Brain.Mode == config.BrainMuse {
		t.Fatal("a refused save changed the mode")
	}

	to := post(t, f, c, muse(url.Values{"sdk": {"mgst_abc"}, "speechkey": {"sk-1"}, "speechstyle": {"warmly"}}))
	if p := to.Query().Get("problem"); p != "" {
		t.Fatalf("saving Muse with both secrets: %q", p)
	}
	b := config.Get().Brain
	if b.Mode != config.BrainMuse || b.Muse.SDKToken != "mgst_abc" || b.Muse.Speech.Key != "sk-1" ||
		b.Muse.Speech.Voice != "coral" || b.Muse.Speech.Style != "warmly" {
		t.Fatalf("saved %+v", b)
	}

	// Saved again with nothing posted for the secrets: they stay, and the mode may stay Muse.
	if to := post(t, f, c, muse(url.Values{"speechstyle": {"briskly"}})); to.Query().Get("problem") != "" {
		t.Fatalf("saving again: %q", to.Query().Get("problem"))
	}
	b = config.Get().Brain
	if b.Muse.SDKToken != "mgst_abc" || b.Muse.Speech.Key != "sk-1" || b.Muse.Speech.Style != "briskly" {
		t.Errorf("a save with nothing for the secrets changed them: %+v", b)
	}

	// Removing a secret while Muse answers is refused; removing it with another assistant chosen is not.
	if to := post(t, f, c, muse(url.Values{"nospeechkey": {"yes"}})); to.Query().Get("problem") == "" {
		t.Error("removing the speech key under Muse was allowed")
	}
	v := muse(url.Values{"nospeechkey": {"yes"}, "nosdk": {"yes"}})
	v.Set("mode", "")
	if to := post(t, f, c, v); to.Query().Get("problem") != "" {
		t.Fatalf("going back to Home Assistant: %q", to.Query().Get("problem"))
	}
	b = config.Get().Brain
	if b.Mode != config.BrainHomeAssistant || b.Muse.SDKToken != "" || b.Muse.Speech.Key != "" {
		t.Errorf("after removing the secrets: %+v", b)
	}

	if to := post(t, f, c, muse(url.Values{"speechvoice": {"hal"}, "sdk": {"a"}, "speechkey": {"b"}})); to.Query().Get("problem") == "" {
		t.Error("a voice the endpoint does not have was saved")
	}
	if to := post(t, f, c, muse(url.Values{"speechbase": {"not a url"}, "sdk": {"a"}, "speechkey": {"b"}})); to.Query().Get("problem") == "" {
		t.Error("a speech endpoint that is not an address was saved")
	}
}

// The pairing panel is on the page, says what the device is to Muse, and the state the panel polls
// carries the same words only to a browser that is in.
func TestMusePairingPanelAndState(t *testing.T) {
	f, c := in(t)
	body := get(f, "/setup?tab=sound", c).Body.String()
	for _, want := range []string{"Muse pairing", "Not paired", "muse_phase"} {
		if !strings.Contains(body, want) {
			t.Errorf("the sound tab lacks %q", want)
		}
	}
	if st := get(f, "/setup/state", c).Body.String(); !strings.Contains(st, `"muse":"Not paired."`) || !strings.Contains(st, `"muse_phase":"unpaired"`) {
		t.Errorf("state for a browser that is in: %s", st)
	}
	if st := get(f, "/setup/state", nil).Body.String(); strings.Contains(st, "muse") {
		t.Errorf("state for a stranger says %s", st)
	}
}
