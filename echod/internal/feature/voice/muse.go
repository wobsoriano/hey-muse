package voice

import (
	"context"
	"encoding/binary"
	"log/slog"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/media"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
	"github.com/HuskerMinion/techo5/echod/internal/lib/speech"
)

// Muse answering the turn (config.BrainMuse): the utterance goes to Muse whole, as a voice note,
// and Muse transcribes and answers it. The device then says the answer through a speech endpoint
// (lib/speech), because Muse returns words and this device has no voice of its own for them. The
// connection belongs to feature/muse; here it is only asked. The conversation cannot tell this
// backend from the others, since it reports the same events in the same order.

// museLink is the connection a turn asks through: feature/muse's Feature, or a test's stand-in.
type museLink interface {
	Ready() bool
	Ask(ctx context.Context, wav []byte, on func(muse.ReplyEvent)) (muse.Reply, error)
}

// speakFunc voices text, calling out with the samples at speech.Rate as they arrive.
type speakFunc func(ctx context.Context, text string, out func(samples []int16) error) error

// museChunk is the least audio handed to the conversation at a time, in 16 kHz samples: 100 ms. The
// endpoint answers in reads of a few kilobytes, and the reply's queue holds chunkQueue of whatever
// it is given, so each read must not take a slot of its own.
const museChunk = mic.Rate / 10

type viaMuse struct {
	link  museLink
	speak speakFunc
	post  func(event)
	utterance
}

func newViaMuse(link museLink, speak speakFunc, post func(event)) *viaMuse {
	return &viaMuse{link: link, speak: speak, post: post}
}

func (m *viaMuse) Name() string { return "muse" }

func (m *viaMuse) Ready() bool { return config.Get().Brain.Mode == config.BrainMuse && m.link.Ready() }

// End hands the utterance over to be answered, off the send queue as direct does: Muse takes
// seconds to answer, and the next turn's start must not wait behind it.
func (m *viaMuse) End() error {
	if ctx, pcm := m.take(); ctx != nil {
		safe.Go("muse turn", func() { m.answer(ctx, pcm) })
	}
	return nil
}

// speakMuse voices text through the endpoint the settings name, read afresh for every reply so a
// voice chosen on the screen is the next answer's.
func speakMuse(ctx context.Context, text string, out func(samples []int16) error) error {
	s := config.Get().Brain.Muse.Speech
	return speech.Client{Base: s.Base, Key: s.Key, Model: s.Model, Voice: s.Voice, Style: s.Style}.Speak(ctx, text, out)
}

// answer asks Muse and reports the way Home Assistant's pipeline events are reported.
//
// The first Settled is taken as the whole answer and spoken there and then, inside the callback,
// which holds Ask open while the voice plays: Muse going on after a pause is rare, and an answer
// that waited for it would start late every time. Whatever Muse adds after that is not said.
func (m *viaMuse) answer(ctx context.Context, pcm []byte) {
	fail := func(code string, err error) {
		if ctx.Err() == nil {
			m.post(event{kind: evError, code: code, msg: err.Error()})
		}
	}

	start := time.Now()
	wav := wavFile(media.Normalize(samplesOf(pcm)))
	var heardAt, repliedAt, firstAudioAt time.Duration
	var spoken bool
	var speechErr error
	reply, err := m.link.Ask(ctx, wav, func(e muse.ReplyEvent) {
		if ctx.Err() != nil {
			return
		}
		switch e := e.(type) {
		case muse.Heard:
			heardAt = time.Since(start)
			m.post(event{kind: evHeard, text: strings.TrimSpace(e.Text)})
		case muse.Settled:
			if spoken {
				return
			}
			spoken = true
			repliedAt = time.Since(start)
			firstAudioAt, speechErr = m.say(ctx, speech.Spoken(e.Text), start)
		}
	})
	if ctx.Err() != nil {
		return
	}
	switch {
	case spoken:
		// The answer is out. Muse going on, or the connection dropping on the way out, is nothing the
		// turn still needs.
	case err != nil:
		fail("intent-failed", err)
		return
	default:
		// Ask returned without a pause it could call settled: Muse ran on to the cap, or said nothing.
		text := speech.Spoken(reply.Text)
		if text == "" {
			m.post(event{kind: evRunEnd})
			return
		}
		repliedAt = time.Since(start)
		firstAudioAt, speechErr = m.say(ctx, text, start)
	}
	if ctx.Err() != nil {
		return
	}
	if speechErr != nil {
		fail("tts-failed", speechErr)
		return
	}
	slog.Info("muse turn answered", "heard_ms", heardAt.Milliseconds(), "replied_ms", repliedAt.Milliseconds(),
		"first_audio_ms", firstAudioAt.Milliseconds(), "spoken_ms", time.Since(start).Milliseconds())
}

// say reports the answer and streams its voice: the text, a continue for a question, the audio in
// chunks of at least museChunk, then the stream's end and the run's. It returns when the first
// audio was handed over, and what went wrong if the voice did not finish. Nothing is posted once
// ctx has ended.
func (m *viaMuse) say(ctx context.Context, text string, start time.Time) (firstAudio time.Duration, err error) {
	m.post(event{kind: evReplyText, text: text})
	// An answer that asks something wants one, and gets it without the wake word again: what Home
	// Assistant's pipeline reports as continue_conversation.
	if strings.HasSuffix(text, "?") {
		m.post(event{kind: evContinue})
	}

	var down speech.Downsampler
	held := make([]int16, 0, 2*museChunk)
	flush := func() {
		if len(held) == 0 || ctx.Err() != nil {
			return
		}
		if firstAudio == 0 {
			firstAudio = time.Since(start)
		}
		m.post(event{kind: evStreamAudio, audio: bytesOf(held)})
		held = held[:0]
	}
	err = m.speak(ctx, text, func(samples []int16) error {
		held = append(held, down.Write(samples)...)
		if len(held) >= museChunk {
			flush()
		}
		return ctx.Err()
	})
	if err != nil || ctx.Err() != nil {
		return firstAudio, err
	}
	flush()
	m.post(event{kind: evStreamEnd})
	m.post(event{kind: evRunEnd})
	return firstAudio, nil
}

// wavFile wraps 16 kHz 16-bit mono samples as the voice note Muse takes.
func wavFile(samples []int16) []byte {
	data := 2 * len(samples)
	b := make([]byte, 0, 44+data)
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+data))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, 1) // mono
	b = binary.LittleEndian.AppendUint32(b, mic.Rate)
	b = binary.LittleEndian.AppendUint32(b, mic.Rate*2)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(data))
	return append(b, bytesOf(samples)...)
}
