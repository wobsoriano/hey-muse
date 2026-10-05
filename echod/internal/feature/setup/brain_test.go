package setup

import (
	"net/url"
	"strings"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/config"
)

// has says whether this image has a built-in voice, for the length of a test.
func has(t *testing.T, builtIn bool) {
	t.Helper()
	was := builtInVoice
	builtInVoice = func() bool { return builtIn }
	t.Cleanup(func() { builtInVoice = was })
}

// Muse needs its SDK token before it can answer, whether it was saved before or arrives with the
// form. The speech key it can do without, the built-in voice speaking instead, and a save keeps
// the secrets it was not given.
func TestSaveBrainMuseNeedsItsSecrets(t *testing.T) {
	f, c := in(t)
	has(t, true)
	muse := func(extra url.Values) url.Values {
		v := url.Values{"what": {"brain"}, "tab": {"sound"}, "mode": {"muse"}, "speechvoice": {"coral"}}
		for k, vals := range extra {
			v[k] = vals
		}
		return v
	}
	if to := post(t, f, c, muse(nil)); !strings.Contains(to.Query().Get("problem"), "SDK token") {
		t.Fatalf("Muse with no secrets was saved: %q", to.Query().Get("problem"))
	}
	if to := post(t, f, c, muse(url.Values{"speechkey": {"sk-1"}})); to.Query().Get("problem") == "" {
		t.Fatal("Muse with only the speech key was saved")
	}
	if config.Get().Brain.Mode == config.BrainMuse {
		t.Fatal("a refused save changed the mode")
	}

	// The SDK token alone is enough, and the page says who will speak.
	if to := post(t, f, c, muse(url.Values{"sdk": {"mgst_abc"}})); to.Query().Get("problem") != "" {
		t.Fatalf("saving Muse with only the SDK token: %q", to.Query().Get("problem"))
	}
	if b := config.Get().Brain; b.Mode != config.BrainMuse || b.Muse.Speech.Key != "" {
		t.Fatalf("saved %+v", b)
	}
	body := get(f, "/setup?tab=sound", c).Body.String()
	for _, want := range []string{`<option value="builtin" selected>Built in</option><option value="alloy">`, "answers use the built-in voice"} {
		if !strings.Contains(body, want) {
			t.Errorf("with no speech key the sound tab lacks %q", want)
		}
	}

	to := post(t, f, c, muse(url.Values{"speechkey": {"sk-1"}, "speechstyle": {"warmly"}}))
	if p := to.Query().Get("problem"); p != "" {
		t.Fatalf("saving Muse with both secrets: %q", p)
	}
	b := config.Get().Brain
	if b.Mode != config.BrainMuse || b.Muse.SDKToken != "mgst_abc" || b.Muse.Speech.Key != "sk-1" ||
		b.Muse.Speech.Voice != "coral" || b.Muse.Speech.Style != "warmly" {
		t.Fatalf("saved %+v", b)
	}
	body = get(f, "/setup?tab=sound", c).Body.String()
	if !strings.Contains(body, `<option value="coral" selected>`) || strings.Contains(body, "answers use the built-in voice") {
		t.Error("with a speech key the sound tab does not show the voice chosen, or still says the built-in one answers")
	}

	// Saved again with nothing posted for the secrets: they stay, and the mode may stay Muse.
	if to := post(t, f, c, muse(url.Values{"speechstyle": {"briskly"}})); to.Query().Get("problem") != "" {
		t.Fatalf("saving again: %q", to.Query().Get("problem"))
	}
	b = config.Get().Brain
	if b.Muse.SDKToken != "mgst_abc" || b.Muse.Speech.Key != "sk-1" || b.Muse.Speech.Style != "briskly" {
		t.Errorf("a save with nothing for the secrets changed them: %+v", b)
	}

	// The built-in voice can be chosen with a key saved, and the key stays.
	if to := post(t, f, c, muse(url.Values{"speechvoice": {"builtin"}})); to.Query().Get("problem") != "" {
		t.Fatalf("choosing the built-in voice: %q", to.Query().Get("problem"))
	}
	if b = config.Get().Brain; b.Muse.Speech.Voice != "builtin" || b.Muse.Speech.Key != "sk-1" {
		t.Errorf("after choosing the built-in voice: %+v", b)
	}

	// Removing the SDK token while Muse answers is refused; the speech key may go, and both may with
	// another assistant chosen.
	if to := post(t, f, c, muse(url.Values{"nosdk": {"yes"}})); to.Query().Get("problem") == "" {
		t.Error("removing the SDK token under Muse was allowed")
	}
	if to := post(t, f, c, muse(url.Values{"nospeechkey": {"yes"}})); to.Query().Get("problem") != "" {
		t.Errorf("removing the speech key under Muse: %q", to.Query().Get("problem"))
	}
	v := muse(url.Values{"nosdk": {"yes"}})
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

// An image built without the built-in voice has only the endpoint to speak with, so there Muse
// still needs the speech key and one of its voices, and the page says why.
func TestSaveBrainMuseWithNoBuiltInVoice(t *testing.T) {
	f, c := in(t)
	has(t, false)
	muse := func(extra url.Values) url.Values {
		v := url.Values{"what": {"brain"}, "tab": {"sound"}, "mode": {"muse"}, "sdk": {"mgst_abc"}, "speechvoice": {"coral"}}
		for k, vals := range extra {
			v[k] = vals
		}
		return v
	}
	if to := post(t, f, c, muse(nil)); !strings.Contains(to.Query().Get("problem"), "no built-in voice") {
		t.Errorf("Muse with nothing to speak with was saved: %q", to.Query().Get("problem"))
	}
	if to := post(t, f, c, muse(url.Values{"speechkey": {"sk-1"}, "speechvoice": {"builtin"}})); to.Query().Get("problem") == "" {
		t.Error("the built-in voice was chosen where there is none")
	}
	if to := post(t, f, c, muse(url.Values{"speechkey": {"sk-1"}})); to.Query().Get("problem") != "" {
		t.Errorf("Muse with a speech key: %q", to.Query().Get("problem"))
	}
	if body := get(f, "/setup?tab=sound", c).Body.String(); !strings.Contains(body, "This image has no built-in voice") {
		t.Error("the sound tab does not say the image has no built-in voice")
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
