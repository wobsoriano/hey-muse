package config

// Brain is where a turn's speech and answer come from when they do not come from Home Assistant: the
// direct pipeline (feature/voice/direct.go), or Meta's Muse (feature/voice/muse.go). For the direct
// pipeline, speech to text and text to speech are Wyoming servers (faster-whisper and Piper, as Home
// Assistant itself uses), and the answer is a chat model behind an OpenAI-style endpoint (llama.cpp's
// server, Ollama, and so on), which acts through the device's own abilities as tools. Empty Mode is
// Home Assistant's Assist pipeline, as always.
type Brain struct {
	Mode BrainMode `json:"mode,omitempty"`

	// STT and TTS are the Wyoming servers, host:port.
	STT string `json:"stt,omitempty"`
	TTS string `json:"tts,omitempty"`
	// Voice is the Piper voice, like en_US-lessac-medium; empty is the server's default.
	Voice string `json:"voice,omitempty"`
	// Language is what the speech is in, for the recognizer; empty is English.
	Language string `json:"language,omitempty"`

	// LLM is the chat endpoint's base, like http://192.168.1.20:8080/v1, and Model the model to ask
	// for (a server with one model ignores it). Key is sent as a bearer token when set; it is a
	// secret, never shown again once saved.
	LLM   string `json:"llm,omitempty"`
	Model string `json:"model,omitempty"`
	Key   string `json:"key,omitempty"`

	// Search is a SearXNG server's address, like http://192.168.1.20:8888, with its JSON format on: the
	// model looks things up there and reads the pages it finds. Empty is no looking anything up.
	Search string `json:"search,omitempty"`

	// Prompt is added to the device's own instructions to the model: a name, a tone, what the
	// household wants it to know.
	Prompt string `json:"prompt,omitempty"`

	// Muse is what answering through Muse needs. The pairing itself is not here: it lives in the Muse
	// feature's own file, which is never shown and never bundled.
	Muse Muse `json:"muse"`
}

// Muse is Meta's Muse answering the turn, and what says its answer, since Muse returns text: an
// OpenAI-style speech endpoint, or the device's own voice.
type Muse struct {
	// SDKToken is the developer's token from gadgets.muse.ai, which pairing hands to the Muse app and
	// the device reports to Muse once. It is a secret, never shown again once saved.
	SDKToken string `json:"sdk_token,omitempty"`
	Speech   Speech `json:"speech"`
}

// Speech is what voices Muse's answers (lib/speech). Empty Base, Model and Voice are the library's
// defaults. Key is a secret, never shown again once saved; without one the device's own voice speaks.
type Speech struct {
	Base  string `json:"base,omitempty"`
	Model string `json:"model,omitempty"`
	// Voice is one of the endpoint's by name, or "builtin" for the device's own (speech.Voice).
	Voice string `json:"voice,omitempty"`
	// Style is how to say it, in words, for a model that takes instructions.
	Style string `json:"style,omitempty"`
	Key   string `json:"key,omitempty"`
}

type BrainMode string

const (
	BrainHomeAssistant BrainMode = ""
	BrainDirect        BrainMode = "direct"
	BrainMuse          BrainMode = "muse"
)

// Direct is whether turns go to the direct pipeline, and it is set up enough to run one.
func (b Brain) Direct() bool {
	return b.Mode == BrainDirect && b.STT != "" && b.TTS != "" && b.LLM != ""
}

// Standalone is whether the device answers turns itself, with no Home Assistant pipeline needed: the
// direct pipeline when it is set up, or Muse.
func (b Brain) Standalone() bool {
	return b.Direct() || b.Mode == BrainMuse
}

type BrainWriter struct{ st *Store }

// Set replaces everything but the secrets, which only their own setters change: a form that shows a
// key as "set" and posts nothing for it must not clear it.
func (w BrainWriter) Set(b Brain) error {
	return w.st.Update(func(c *Config) {
		key, sdk, speech := c.Brain.Key, c.Brain.Muse.SDKToken, c.Brain.Muse.Speech.Key
		c.Brain = b
		c.Brain.Key, c.Brain.Muse.SDKToken, c.Brain.Muse.Speech.Key = key, sdk, speech
	})
}

// SetVoice changes the speaking voice alone ("" for the server's default), so a choice on the screen
// cannot undo a setup page save made at the same moment.
func (w BrainWriter) SetVoice(voice string) error {
	return w.st.Update(func(c *Config) { c.Brain.Voice = voice })
}

func (w BrainWriter) SetKey(key string) error {
	return w.st.Update(func(c *Config) { c.Brain.Key = key })
}

func (w BrainWriter) SetSDKToken(token string) error {
	return w.st.Update(func(c *Config) { c.Brain.Muse.SDKToken = token })
}

func (w BrainWriter) SetSpeechKey(key string) error {
	return w.st.Update(func(c *Config) { c.Brain.Muse.Speech.Key = key })
}

// SetSpeechVoice is SetVoice for Muse's speech: the voice alone, from the screen.
func (w BrainWriter) SetSpeechVoice(voice string) error {
	return w.st.Update(func(c *Config) { c.Brain.Muse.Speech.Voice = voice })
}
