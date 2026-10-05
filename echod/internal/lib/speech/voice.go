package speech

import "context"

// Voice is who says an answer: one of the endpoint's voices by name, or the device's own (BuiltIn).
// It is what the voice setting holds.
type Voice string

// BuiltIn is the device's own voice (Local).
const BuiltIn Voice = "builtin"

// Chosen is the voice a setting and the endpoint's key come to. Without a key there is no endpoint
// to ask, so the device's own voice speaks whatever the setting says; this is the one place that
// is decided.
func Chosen(setting, key string) Voice {
	if key == "" || Voice(setting) == BuiltIn {
		return BuiltIn
	}
	return Voice(orDefault(setting, DefaultVoice))
}

// Choices are the voices a picker offers, the device's own first.
func Choices() []Voice {
	all := []Voice{BuiltIn}
	for _, name := range Voices {
		all = append(all, Voice(name))
	}
	return all
}

// Local is whether the device itself speaks, with no endpoint asked.
func (v Voice) Local() bool { return v == BuiltIn }

// Label is the voice as a person reads it.
func (v Voice) Label() string {
	if v.Local() {
		return "Built in"
	}
	return string(v)
}

// Speaker is both ways of speaking, as the settings have them.
type Speaker struct {
	Cloud Client // its Voice is not read: Speak is told the voice
	Local Local
}

// Speak says text in voice v and calls out with the samples at 16 kHz, whichever engine made them:
// the endpoint's come at Rate and are brought down here, the device's own are 16 kHz already.
func (s Speaker) Speak(ctx context.Context, v Voice, text string, out func(samples []int16) error) error {
	if v.Local() {
		return s.Local.Speak(ctx, text, out)
	}
	s.Cloud.Voice = string(v)
	var down Downsampler
	return s.Cloud.Speak(ctx, text, func(samples []int16) error { return out(down.Write(samples)) })
}
