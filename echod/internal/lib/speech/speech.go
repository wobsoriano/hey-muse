// Package speech turns text into a voice through an OpenAI-style endpoint (/v1/audio/speech), for a
// device with no speech server of its own on the network. The audio is asked for as raw PCM and handed
// on as it arrives, so a long answer starts playing before it has all been made.
package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Rate is what the endpoint's pcm format is: 24 kHz, 16-bit little-endian, mono.
const Rate = 24000

// DefaultBase, DefaultModel and DefaultVoice are what an empty setting means.
const (
	DefaultBase  = "https://api.openai.com/v1"
	DefaultModel = "gpt-4o-mini-tts"
	DefaultVoice = "alloy"
)

// Voices are the ones the default model has, for a picker.
var Voices = []string{"alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer", "verse"}

// Client is one endpoint and the voice asked of it.
type Client struct {
	Base  string // like https://api.openai.com/v1; empty is DefaultBase
	Key   string // bearer token
	Model string // empty is DefaultModel
	Voice string // empty is DefaultVoice
	// Style is how to say it, in words, for a model that takes instructions; empty for none.
	Style string
	HTTP  *http.Client
}

// maxError bounds how much of a refusal is read into the error.
const maxError = 2048

var client = &http.Client{Timeout: 2 * time.Minute}

// Speak asks for text as speech and calls out with the samples as they arrive, at Rate. It returns
// when the voice has ended, out has failed, or ctx is done.
func (c Client) Speak(ctx context.Context, text string, out func(samples []int16) error) error {
	if c.Key == "" {
		return errors.New("speech: no key")
	}
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = DefaultBase
	}
	body := map[string]string{
		"model":           orDefault(c.Model, DefaultModel),
		"voice":           orDefault(c.Voice, DefaultVoice),
		"input":           text,
		"response_format": "pcm",
	}
	if c.Style != "" {
		body["instructions"] = c.Style
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/speech", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Key)
	hc := c.HTTP
	if hc == nil {
		hc = client
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("speech: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxError))
		return fmt.Errorf("speech: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	// A read may end on half a sample; the odd byte waits for the next one.
	buf := make([]byte, 8192)
	held := 0
	for {
		n, err := resp.Body.Read(buf[held:])
		n += held
		even := n &^ 1
		if even > 0 {
			samples := make([]int16, even/2)
			for i := range samples {
				samples[i] = int16(uint16(buf[2*i]) | uint16(buf[2*i+1])<<8)
			}
			if oerr := out(samples); oerr != nil {
				return oerr
			}
		}
		held = n - even
		if held == 1 {
			buf[0] = buf[even]
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("speech: %w", err)
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Downsampler brings Rate down to 16 kHz across chunk boundaries: two samples out for every three in,
// by the same linear interpolation the rest of the device uses for a voice.
type Downsampler struct{ held []int16 }

// Write takes the next samples at Rate and returns what they make at 16 kHz.
func (d *Downsampler) Write(in []int16) []int16 {
	if len(d.held) > 0 {
		in = append(d.held, in...)
	}
	groups := len(in) / 3
	out := make([]int16, 0, 2*groups)
	for g := 0; g < groups; g++ {
		a, b, c := in[3*g], in[3*g+1], in[3*g+2]
		out = append(out, a, int16((int32(b)+int32(c))/2))
	}
	d.held = append(d.held[:0:0], in[3*groups:]...)
	return out
}

// Spoken tidies an answer for a voice: a model told not to use markdown sometimes does anyway, and
// its typographic punctuation (a non-breaking hyphen, a curly apostrophe) is plain for the voice.
func Spoken(s string) string {
	r := strings.NewReplacer("**", "", "__", "", "`", "", "#", "",
		"‑", "-", "‐", "-", "–", "-", "—", ", ", "‘", "'", "’", "'", "“", `"`, "”", `"`,
		" ", " ", " ", " ")
	return strings.TrimSpace(r.Replace(s))
}
