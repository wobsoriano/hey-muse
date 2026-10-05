package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
	"github.com/HuskerMinion/techo5/echod/internal/lib/speech"
)

// posted collects what the backend reports, under a lock because the answer runs on its own
// goroutine.
type posted struct {
	mu     sync.Mutex
	events []event
}

func (p *posted) post(e event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *posted) all() []event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event(nil), p.events...)
}

func (p *posted) kinds() []eventKind {
	var out []eventKind
	for _, e := range p.all() {
		out = append(out, e.kind)
	}
	return out
}

// fakeLink is Muse answering however the test says.
type fakeLink struct {
	ask func(ctx context.Context, wav []byte, on func(muse.ReplyEvent)) (muse.Reply, error)
}

func (l fakeLink) Ready() bool { return true }

func (l fakeLink) Ask(ctx context.Context, wav []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
	return l.ask(ctx, wav, on)
}

// speaking is a voice of n samples at 16 kHz handed over per at a time, as a read brings them.
func speaking(n, per int) speakFunc {
	return func(ctx context.Context, _ speech.Voice, text string, out func([]int16) error) error {
		for n > 0 {
			k := min(per, n)
			if err := out(make([]int16, k)); err != nil {
				return err
			}
			n -= k
		}
		return nil
	}
}

// builtIn is the settings choosing the device's own voice.
func builtIn() speech.Voice { return speech.BuiltIn }

func sameKinds(a, b []eventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Twenty milliseconds of something said, as the microphone hands it over.
var someAudio = bytesOf(make([]int16, 320))

// A turn is reported the way Home Assistant's is: heard, then the reply and the question it asks,
// then the voice, then the ends. The voice note is a WAV of what was heard, and a second settle
// changes nothing.
func TestMuseTurnIsReportedInOrder(t *testing.T) {
	var got []event
	var wav []byte
	settles := 0
	m := newViaMuse(fakeLink{ask: func(ctx context.Context, w []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
		wav = w
		on(muse.Heard{Text: " set a timer "})
		on(muse.Settled{Text: "**Done.** Anything else?"})
		settles++
		on(muse.Settled{Text: "Done. Anything else? Also, the weather is fine."})
		settles++
		return muse.Reply{Text: "Done. Anything else? Also, the weather is fine.", Heard: "set a timer"}, nil
	}}, builtIn, speaking(3*museChunk, museChunk), func(e event) { got = append(got, e) })

	m.answer(context.Background(), someAudio)

	var kinds []eventKind
	for _, e := range got {
		kinds = append(kinds, e.kind)
	}
	want := []eventKind{evHeard, evReplyText, evContinue, evStreamAudio, evStreamAudio, evStreamAudio, evStreamEnd, evRunEnd}
	if !sameKinds(kinds, want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	if got[0].text != "set a timer" || got[1].text != "Done. Anything else?" {
		t.Errorf("heard %q, replied %q", got[0].text, got[1].text)
	}
	for _, e := range got[3:6] {
		if len(e.audio) != 2*museChunk {
			t.Errorf("a chunk of %d bytes, want %d", len(e.audio), 2*museChunk)
		}
	}
	if settles != 2 {
		t.Errorf("the link saw %d settles", settles)
	}
	if string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || binary.LittleEndian.Uint32(wav[24:28]) != 16000 ||
		int(binary.LittleEndian.Uint32(wav[40:44])) != len(someAudio) || len(wav) != 44+len(someAudio) {
		t.Errorf("the voice note is not the WAV expected: %d bytes, header %q", len(wav), wav[:44])
	}
}

// Nothing heard and nothing answered ends the turn; Muse failing, or the voice failing, fails it
// with the pipeline's own codes.
func TestMuseEmptyAndFailedTurns(t *testing.T) {
	for name, c := range map[string]struct {
		ask   func(ctx context.Context, wav []byte, on func(muse.ReplyEvent)) (muse.Reply, error)
		speak speakFunc
		want  []eventKind
		code  string
	}{
		"nothing said": {
			ask:   func(context.Context, []byte, func(muse.ReplyEvent)) (muse.Reply, error) { return muse.Reply{}, nil },
			speak: speaking(museChunk, museChunk),
			want:  []eventKind{evRunEnd},
		},
		"answered without settling": {
			ask: func(_ context.Context, _ []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
				on(muse.Heard{Text: "hello"})
				return muse.Reply{Text: "Hi.", Heard: "hello"}, nil
			},
			speak: speaking(museChunk, museChunk),
			want:  []eventKind{evHeard, evReplyText, evStreamAudio, evStreamEnd, evRunEnd},
		},
		"muse failed": {
			ask: func(context.Context, []byte, func(muse.ReplyEvent)) (muse.Reply, error) {
				return muse.Reply{}, errors.New("muse: Muse did not reply")
			},
			speak: speaking(museChunk, museChunk),
			want:  []eventKind{evError},
			code:  "intent-failed",
		},
		"voice failed": {
			ask: func(_ context.Context, _ []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
				on(muse.Heard{Text: "hello"})
				on(muse.Settled{Text: "Hi."})
				return muse.Reply{Text: "Hi."}, nil
			},
			speak: func(context.Context, speech.Voice, string, func([]int16) error) error {
				return errors.New("speech: the built-in voice: not found")
			},
			want: []eventKind{evHeard, evReplyText, evError},
			code: "tts-failed",
		},
	} {
		p := &posted{}
		m := newViaMuse(fakeLink{ask: c.ask}, builtIn, c.speak, p.post)
		m.answer(context.Background(), someAudio)
		if got := p.kinds(); !sameKinds(got, c.want) {
			t.Errorf("%s: events %v, want %v", name, got, c.want)
			continue
		}
		if c.code != "" {
			if last := p.all()[len(c.want)-1]; last.code != c.code {
				t.Errorf("%s: failed with %q, want %q", name, last.code, c.code)
			}
		}
	}
}

// Small reads of a voice are gathered into chunks of at least a hundred milliseconds, and what is
// left at the end goes out as it is.
func TestMuseChunksAreCoalesced(t *testing.T) {
	p := &posted{}
	// 35 reads of 10 ms: 350 ms, which is 5600 samples.
	m := newViaMuse(fakeLink{ask: func(_ context.Context, _ []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
		on(muse.Settled{Text: "Hi."})
		return muse.Reply{Text: "Hi."}, nil
	}}, builtIn, speaking(35*160, 160), p.post)
	m.answer(context.Background(), someAudio)

	var sizes []int
	for _, e := range p.all() {
		if e.kind == evStreamAudio {
			sizes = append(sizes, len(e.audio)/2)
		}
	}
	want := []int{museChunk, museChunk, museChunk, 800}
	if len(sizes) != len(want) {
		t.Fatalf("chunks of %v samples, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("chunks of %v samples, want %v", sizes, want)
			break
		}
	}
}

// Stop ends the turn wherever it is: still waiting on Muse, or halfway through the voice. Nothing
// is reported after it, and both the ask and the voice are cut.
func TestMuseStopPostsNothingAfter(t *testing.T) {
	for name, c := range map[string]struct {
		ask   func(ctx context.Context, wav []byte, on func(muse.ReplyEvent)) (muse.Reply, error)
		speak speakFunc
	}{
		"waiting on muse": {
			ask: func(ctx context.Context, _ []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
				<-ctx.Done()
				on(muse.Settled{Text: "Too late."})
				return muse.Reply{}, ctx.Err()
			},
			speak: speaking(museChunk, museChunk),
		},
		"speaking": {
			ask: func(ctx context.Context, _ []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
				on(muse.Settled{Text: "A long answer."})
				return muse.Reply{Text: "A long answer."}, nil
			},
			speak: func(ctx context.Context, _ speech.Voice, _ string, out func([]int16) error) error {
				if err := out(make([]int16, 2400)); err != nil {
					return err
				}
				<-ctx.Done()
				_ = out(make([]int16, 2400))
				return ctx.Err()
			},
		},
	} {
		p := &posted{}
		m := newViaMuse(fakeLink{ask: c.ask}, builtIn, c.speak, p.post)
		if err := m.Start(""); err != nil {
			t.Fatal(err)
		}
		if err := m.Audio(someAudio); err != nil {
			t.Fatal(err)
		}
		ctx, pcm := m.take()
		done := make(chan struct{})
		go func() { defer close(done); m.answer(ctx, pcm) }()

		// Give the answer time to reach where it waits, then stop it there.
		time.Sleep(50 * time.Millisecond)
		before := len(p.all())
		if err := m.Stop(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the answer did not end after Stop", name)
		}
		if after := len(p.all()); after != before {
			t.Errorf("%s: %d events posted after Stop: %v", name, after-before, p.kinds()[before:])
		}
		for _, e := range p.all() {
			if e.kind == evError || e.kind == evRunEnd || e.kind == evStreamEnd {
				t.Errorf("%s: a stopped turn reported %v", name, e.kind)
			}
		}
	}
}

// Which voice says the answer: the device's own when that is what the settings come to, and also
// when the endpoint fails before any of its voice was played, a short held piece included. Once
// the endpoint's voice has started, its failure is the turn's; and with no voice left at all the
// answer is still on the screen and the turn fails as a voice does.
func TestMuseVoiceAndItsFallback(t *testing.T) {
	refused := errors.New("speech: 401 Unauthorized: bad key")
	absent := errors.New("speech: the built-in voice: not found")
	// cloud says n samples and then fails with err; the device's own says half a second, or fails.
	engines := func(asked *[]speech.Voice, n int, err, localErr error) speakFunc {
		return func(ctx context.Context, v speech.Voice, text string, out func([]int16) error) error {
			*asked = append(*asked, v)
			if v.Local() {
				if localErr != nil {
					return localErr
				}
				return speaking(5*museChunk, museChunk)(ctx, v, text, out)
			}
			if n > 0 {
				if oerr := out(make([]int16, n)); oerr != nil {
					return oerr
				}
			}
			return err
		}
	}
	spoke := []eventKind{evReplyText, evStreamAudio, evStreamAudio, evStreamAudio, evStreamAudio, evStreamAudio, evStreamEnd, evRunEnd}
	for name, c := range map[string]struct {
		voice    speech.Voice
		n        int
		err      error
		localErr error
		asked    []speech.Voice
		want     []eventKind
		msg      []string
	}{
		"built in":                   {voice: speech.BuiltIn, asked: []speech.Voice{speech.BuiltIn}, want: spoke},
		"the endpoint":               {voice: "coral", n: museChunk, asked: []speech.Voice{"coral"}, want: []eventKind{evReplyText, evStreamAudio, evStreamEnd, evRunEnd}},
		"refused":                    {voice: "coral", err: refused, asked: []speech.Voice{"coral", speech.BuiltIn}, want: spoke},
		"failed with a piece held":   {voice: "coral", n: museChunk / 2, err: refused, asked: []speech.Voice{"coral", speech.BuiltIn}, want: spoke},
		"failed after it started":    {voice: "coral", n: museChunk, err: refused, asked: []speech.Voice{"coral"}, want: []eventKind{evReplyText, evStreamAudio, evError}, msg: []string{"bad key"}},
		"refused and no voice here":  {voice: "coral", err: refused, localErr: absent, asked: []speech.Voice{"coral", speech.BuiltIn}, want: []eventKind{evReplyText, evError}, msg: []string{"bad key", "not found"}},
		"built in and no voice here": {voice: speech.BuiltIn, localErr: absent, asked: []speech.Voice{speech.BuiltIn}, want: []eventKind{evReplyText, evError}, msg: []string{"not found"}},
	} {
		p := &posted{}
		var asked []speech.Voice
		m := newViaMuse(fakeLink{ask: func(_ context.Context, _ []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
			on(muse.Settled{Text: "Hi."})
			return muse.Reply{Text: "Hi."}, nil
		}}, func() speech.Voice { return c.voice }, engines(&asked, c.n, c.err, c.localErr), p.post)
		m.answer(context.Background(), someAudio)

		if !slices.Equal(asked, c.asked) {
			t.Errorf("%s: asked %v, want %v", name, asked, c.asked)
		}
		if got := p.kinds(); !sameKinds(got, c.want) {
			t.Errorf("%s: events %v, want %v", name, got, c.want)
			continue
		}
		all := p.all()
		for _, e := range all {
			if e.kind == evStreamAudio && len(e.audio) != 2*museChunk {
				t.Errorf("%s: a chunk of %d bytes, want %d", name, len(e.audio), 2*museChunk)
			}
		}
		if last := all[len(all)-1]; last.kind == evError {
			if last.code != "tts-failed" {
				t.Errorf("%s: failed with %q", name, last.code)
			}
			for _, want := range c.msg {
				if !strings.Contains(last.msg, want) {
					t.Errorf("%s: the error %q does not say %q", name, last.msg, want)
				}
			}
		}
	}
}
