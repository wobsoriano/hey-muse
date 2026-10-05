package config

import (
	"os"
	"strings"
	"testing"
)

// A setup page save posts nothing for a secret it shows as "set", so Set has to leave every secret
// as it was, and only the setters move them.
func TestBrainSetKeepsTheSecrets(t *testing.T) {
	st := load(t)
	w := st.Set().Brain()
	for _, err := range []error{w.SetKey("llm-key"), w.SetSDKToken("mgst_token"), w.SetSpeechKey("sk-speech")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Set(Brain{Mode: BrainMuse, Muse: Muse{Speech: Speech{Voice: "coral"}}}); err != nil {
		t.Fatal(err)
	}
	b := st.Get().Brain
	if b.Key != "llm-key" || b.Muse.SDKToken != "mgst_token" || b.Muse.Speech.Key != "sk-speech" {
		t.Errorf("Set changed a secret: %+v", b)
	}
	if b.Mode != BrainMuse || b.Muse.Speech.Voice != "coral" {
		t.Errorf("Set did not take the rest: %+v", b)
	}

	if err := w.SetSpeechVoice("sage"); err != nil {
		t.Fatal(err)
	}
	if got := st.Get().Brain.Muse.Speech.Voice; got != "sage" {
		t.Errorf("voice = %q", got)
	}
	for _, err := range []error{w.SetSDKToken(""), w.SetSpeechKey("")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	b = st.Get().Brain
	if b.Muse.SDKToken != "" || b.Muse.Speech.Key != "" || b.Key != "llm-key" {
		t.Errorf("removing the Muse secrets: %+v", b)
	}

	raw, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"mode": "muse"`) || strings.Contains(string(raw), "mgst_") {
		t.Errorf("the file holds %s", raw)
	}
}

// Standalone is the question the boot splash asks: can the device answer without Home Assistant.
// It is wider than Direct, which the pages about the direct pipeline go on asking.
func TestBrainStandalone(t *testing.T) {
	for name, c := range map[string]struct {
		b                  Brain
		direct, standalone bool
	}{
		"home assistant":     {Brain{}, false, false},
		"direct, set up":     {Brain{Mode: BrainDirect, STT: "a:1", TTS: "b:1", LLM: "http://c"}, true, true},
		"direct, not set up": {Brain{Mode: BrainDirect, STT: "a:1"}, false, false},
		"muse":               {Brain{Mode: BrainMuse}, false, true},
	} {
		if got := c.b.Direct(); got != c.direct {
			t.Errorf("%s: Direct = %v", name, got)
		}
		if got := c.b.Standalone(); got != c.standalone {
			t.Errorf("%s: Standalone = %v", name, got)
		}
	}
}
