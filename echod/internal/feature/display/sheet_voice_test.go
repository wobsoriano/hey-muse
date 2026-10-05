//go:build !dot

package display

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/lib/speech"
)

// setServerVoices stands in for an answer from the speech server.
func setServerVoices(t *testing.T, addr, lang string, names []string) {
	t.Helper()
	v := &serverVoices
	v.Lock()
	was := struct {
		addr, lang string
		names      []string
		at         time.Time
	}{v.addr, v.lang, v.names, v.at}
	v.addr, v.lang, v.names, v.at = addr, lang, names, time.Now()
	v.Unlock()
	t.Cleanup(func() {
		v.Lock()
		v.addr, v.lang, v.names, v.at = was.addr, was.lang, was.names, was.at
		v.Unlock()
	})
}

func TestVoiceChoices(t *testing.T) {
	server := []string{"en_GB-alan-medium", "en_US-ryan-high"}
	setServerVoices(t, "tts:1", "en_US", server)

	// The server's default comes first, so a voice chosen on the screen can be undone there.
	b := config.Brain{TTS: "tts:1", Language: "en-US"}
	if got, want := voiceChoices(b), append([]string{""}, server...); !slices.Equal(got, want) {
		t.Errorf("en-US: got %v, want %v", got, want)
	}

	// A list for another language is not this language's: English falls back to the common voices,
	// anything else offers only the default and the voice in use.
	b = config.Brain{TTS: "tts:1", Language: "en"}
	if got := voiceChoices(b); !slices.Equal(got, append([]string{""}, commonVoices...)) {
		t.Errorf("en, list held for en_US: got %v", got)
	}
	b = config.Brain{TTS: "tts:1", Language: "de", Voice: "de_DE-thorsten-medium"}
	if got, want := voiceChoices(b), []string{"", "de_DE-thorsten-medium"}; !slices.Equal(got, want) {
		t.Errorf("de: got %v, want %v", got, want)
	}

	// Another server's list is not this one's.
	b = config.Brain{TTS: "tts:2", Language: "en_US"}
	if got := voiceChoices(b); slices.Contains(got, "en_GB-alan-medium") {
		t.Errorf("another server's voices offered: %v", got)
	}
}

// The open picker keeps the list it showed, so a tap chooses what was under the finger even when the
// server's list arrives in between.
func TestPickerHoldsItsList(t *testing.T) {
	sv := &shownVoices
	sv.Lock()
	sv.names, sv.at = nil, time.Time{}
	sv.Unlock()
	t.Cleanup(func() {
		sv.Lock()
		sv.names, sv.at = nil, time.Time{}
		sv.Unlock()
	})

	b := config.Brain{TTS: "tts:1", Language: "en_US"}
	setServerVoices(t, "tts:9", "en_US", nil)
	first := pickerVoices(b)
	setServerVoices(t, "tts:1", "en_US", []string{"en_US-zed-medium"})
	if got := pickerVoices(b); !slices.Equal(got, first) {
		t.Errorf("the open picker's list changed under it: %v, was %v", got, first)
	}

	sv.Lock()
	sv.at = time.Now().Add(-pickerHeld - time.Second)
	sv.Unlock()
	if got := pickerVoices(b); !slices.Contains(got, "en_US-zed-medium") {
		t.Errorf("a picker opened afresh kept the old list: %v", got)
	}
}

func TestVoiceLabel(t *testing.T) {
	for name, want := range map[string]string{
		"":                    "Server default",
		"en_US-ryan-high":     "Ryan, US, high",
		"en_GB-alan-medium":   "Alan, GB",
		"fr_FR-élodie-medium": "Élodie, FR",
		"odd":                 "odd",
	} {
		if got := voiceLabel(name); got != want {
			t.Errorf("voiceLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// For Muse the built-in voice is first in the picker, and with no speech key it is the only one,
// since no other could be heard. A tap saves the voice that was under it.
func TestMuseVoicePicker(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	w := config.Set().Brain()
	if err := w.Set(config.Brain{Mode: config.BrainMuse, Muse: config.Muse{Speech: config.Speech{Voice: "coral"}}}); err != nil {
		t.Fatal(err)
	}
	p, ok := voicePicker()
	if !ok || !slices.Equal(p.opts, []string{"Built in"}) || p.cur != 0 || voiceRow().value != "Built in" {
		t.Errorf("with no speech key: picker %+v, row %q", p, voiceRow().value)
	}

	if err := w.SetSpeechKey("sk-1"); err != nil {
		t.Fatal(err)
	}
	p, _ = voicePicker()
	if len(p.opts) != 1+len(speech.Voices) || p.opts[0] != "Built in" || p.opts[p.cur] != "Coral" || voiceRow().value != "Coral" {
		t.Errorf("with a speech key: picker %+v, row %q", p, voiceRow().value)
	}
	chooseVoice(0)
	if got := config.Get().Brain.Muse.Speech.Voice; got != "builtin" {
		t.Errorf("after choosing the first: voice %q", got)
	}
	chooseVoice(1)
	if got := config.Get().Brain.Muse.Speech.Voice; got != speech.Voices[0] {
		t.Errorf("after choosing the second: voice %q", got)
	}
	chooseVoice(len(speech.Voices) + 1)
	if got := config.Get().Brain.Muse.Speech.Voice; got != speech.Voices[0] {
		t.Errorf("a tap past the list chose %q", got)
	}
}
