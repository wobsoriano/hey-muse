package voice

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/media"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
	"github.com/HuskerMinion/techo5/echod/internal/lib/wyoming"
)

// The direct pipeline: a turn answered without Home Assistant. What was said goes to a Wyoming
// speech-to-text server, the words to a chat model (Think, which the assistant feature provides), and
// the answer to a Wyoming text-to-speech server; the audio then plays the way Home Assistant's
// streamed replies do. See config.Brain.
//
// The utterance is collected whole and sent when the speaker has finished: faster-whisper only
// transcribes once the audio has stopped anyway, and whole it can be brought up to speaking loudness
// first, which is what Home Assistant's auto gain did for the same quiet microphone.

// Think answers what was heard, doing whatever it asks for on the way. Set by the assistant feature;
// nil until it is.
var Think func(ctx context.Context, heard string) (string, error)

// SetThink installs the answerer.
func SetThink(fn func(ctx context.Context, heard string) (string, error)) { Think = fn }

// maxUtterance bounds what one turn keeps: the longest a slot may listen for.
const maxUtterance = 60 * mic.Rate * 2

// directChunk is how much audio goes to the recognizer in one event.
const directChunk = 3200

// utterance is one turn's audio, collected whole for a backend that answers it once the speaker has
// finished, under a context that Stop ends: a turn that is over posts nothing more.
type utterance struct {
	mu     sync.Mutex
	pcm    []byte
	cancel context.CancelFunc
	ctx    context.Context
}

func (u *utterance) Start(string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cancel != nil {
		u.cancel()
	}
	u.ctx, u.cancel = context.WithCancel(context.Background())
	u.pcm = u.pcm[:0]
	return nil
}

func (u *utterance) Audio(frame []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ctx == nil || len(u.pcm)+len(frame) > maxUtterance {
		return nil
	}
	u.pcm = append(u.pcm, frame...)
	return nil
}

// take is the turn's context and everything heard, or a nil context when no turn is open.
func (u *utterance) take() (context.Context, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	pcm := append([]byte(nil), u.pcm...)
	u.pcm = u.pcm[:0]
	return u.ctx, pcm
}

func (u *utterance) Stop() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cancel != nil {
		u.cancel()
	}
	u.ctx, u.cancel = nil, nil
	u.pcm = u.pcm[:0]
	return nil
}

type direct struct {
	post func(event)
	utterance
}

func newDirect(post func(event)) *direct { return &direct{post: post} }

func (d *direct) Name() string { return "direct" }

func (d *direct) Ready() bool { return config.Get().Brain.Direct() }

// End hands the utterance over to be answered. Not here: this runs on the send queue, and a model
// thinking for two seconds would hold the next turn's start behind it.
func (d *direct) End() error {
	if ctx, pcm := d.take(); ctx != nil {
		safe.Go("direct turn", func() { d.answer(ctx, pcm) })
	}
	return nil
}

// answer runs the three steps and reports each the way Home Assistant's pipeline events are
// reported, so the conversation cannot tell the two apart.
func (d *direct) answer(ctx context.Context, pcm []byte) {
	b := config.Get().Brain
	fail := func(code string, err error) {
		if ctx.Err() == nil {
			d.post(event{kind: evError, code: code, msg: err.Error()})
		}
	}

	start := time.Now()
	heard, err := transcribe(ctx, b, pcm)
	if err != nil {
		fail("stt-failed", err)
		return
	}
	heard = strings.TrimSpace(heard)
	heardAt := time.Since(start)
	d.post(event{kind: evHeard, text: heard})
	if heard == "" {
		d.post(event{kind: evRunEnd})
		return
	}

	if Think == nil {
		fail("intent-failed", errors.New("nothing to answer with"))
		return
	}
	reply, err := Think(ctx, heard)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		fail("intent-failed", err)
		return
	}
	thoughtAt := time.Since(start)
	reply = strings.TrimSpace(reply)
	if reply == "" {
		d.post(event{kind: evRunEnd})
		return
	}
	d.post(event{kind: evReplyText, text: reply})
	// An answer that asks something wants one, and gets it without the wake word again: what Home
	// Assistant's pipeline reports as continue_conversation for a chat model's reply that ends in a
	// question.
	if strings.HasSuffix(reply, "?") {
		d.post(event{kind: evContinue})
	}

	voice, f, err := wyoming.Synthesize(ctx, b.TTS, reply, b.Voice)
	if err != nil {
		fail("tts-failed", err)
		return
	}
	slog.Info("direct turn answered", "heard_ms", heardAt.Milliseconds(), "thought_ms", thoughtAt.Milliseconds(),
		"spoken_ms", time.Since(start).Milliseconds())
	out := bytesOf(media.ToVoiceRate(voice, f.Rate))
	if ctx.Err() != nil {
		return
	}
	d.post(event{kind: evStreamAudio, audio: out})
	d.post(event{kind: evStreamEnd})
	d.post(event{kind: evRunEnd})
}

// transcribe sends the utterance, brought up to speaking loudness, and returns the words.
func transcribe(ctx context.Context, b config.Brain, pcm []byte) (string, error) {
	samples := media.Normalize(samplesOf(pcm))
	lang := b.Language
	if lang == "" {
		lang = "en"
	}
	t, err := wyoming.Transcribe(ctx, b.STT, lang)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 0, directChunk)
	for i, s := range samples {
		buf = append(buf, byte(s), byte(s>>8))
		if len(buf) == directChunk || i == len(samples)-1 {
			if err := t.Audio(buf); err != nil {
				t.Close()
				return "", err
			}
			buf = buf[:0]
		}
	}
	return t.Finish()
}

// samplesOf and bytesOf are 16-bit little-endian audio as the microphone hands it and as the
// recognizer and the speaker take it.
func samplesOf(pcm []byte) []int16 {
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
	}
	return samples
}

func bytesOf(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		out[2*i], out[2*i+1] = byte(s), byte(s>>8)
	}
	return out
}
