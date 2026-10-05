package pairing

import (
	"bytes"
	"testing"
)

func TestChunkPayloadFollowsMTU(t *testing.T) {
	message := bytes.Repeat([]byte("x"), 1000)
	for mtu, perChunk := range map[int]int{23: 17, 185: 157, 256: 157, 3: 17} {
		chunks, err := EncodeChunks(message, mtu)
		if err != nil {
			t.Fatal(err)
		}
		var joined []byte
		for i, c := range chunks {
			if i < len(chunks)-1 && len(c)-headerBytes != perChunk {
				t.Errorf("mtu %d: chunk %d carries %d bytes, want %d", mtu, i, len(c)-headerBytes, perChunk)
			}
			joined = append(joined, c[headerBytes:]...)
		}
		if !bytes.Equal(joined, message) {
			t.Errorf("mtu %d: chunks do not join back into the message", mtu)
		}
	}
}

func TestChunkHeaders(t *testing.T) {
	chunks, _ := EncodeChunks(bytes.Repeat([]byte("a"), 40), 23)
	want := [][]byte{{0xFE, 0, 3}, {0xFE, 1, 3}, {0xFE, 2, 3}}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
	}
	for i, c := range chunks {
		if !bytes.Equal(c[:headerBytes], want[i]) {
			t.Errorf("chunk %d header = %x, want %x", i, c[:headerBytes], want[i])
		}
	}
}

func TestEmptyMessageIsOneChunk(t *testing.T) {
	chunks, err := EncodeChunks(nil, DefaultMTU)
	if err != nil || len(chunks) != 1 || !bytes.Equal(chunks[0], []byte{0xFE, 0, 1}) {
		t.Fatalf("EncodeChunks(nil) = %x, %v", chunks, err)
	}
	if message, ok := new(Assembler).Feed(chunks[0]); !ok || len(message) != 0 {
		t.Fatalf("Feed = %x, %v; want an empty message", message, ok)
	}
}

func TestTooManyChunksIsAnError(t *testing.T) {
	if _, err := EncodeChunks(make([]byte, 17*255), 23); err != nil {
		t.Fatalf("255 chunks should fit: %v", err)
	}
	if _, err := EncodeChunks(make([]byte, 17*255+1), 23); err == nil {
		t.Fatal("256 chunks should not fit")
	}
}

// feedAll returns the message the last packet completed, if it did.
func feedAll(a *Assembler, packets [][]byte) ([]byte, bool) {
	var message []byte
	var ok bool
	for _, p := range packets {
		message, ok = a.Feed(p)
	}
	return message, ok
}

func TestRoundTrip(t *testing.T) {
	message := make([]byte, 2560)
	for i := range message {
		message[i] = byte(i)
	}
	chunks, _ := EncodeChunks(message, 185)
	var a Assembler
	for _, c := range chunks[:len(chunks)-1] {
		if _, ok := a.Feed(c); ok {
			t.Fatal("message complete before its last chunk")
		}
	}
	if got, ok := a.Feed(chunks[len(chunks)-1]); !ok || !bytes.Equal(got, message) {
		t.Fatal("message did not survive the round trip")
	}
}

func TestPlainWritePassesThrough(t *testing.T) {
	plain := []byte(`{"action":"get_device_info"}`)
	if got, ok := new(Assembler).Feed(plain); !ok || !bytes.Equal(got, plain) {
		t.Fatalf("Feed = %q, %v", got, ok)
	}
}

func TestOutOfOrderChunkDiscardsTheMessage(t *testing.T) {
	chunks, _ := EncodeChunks(bytes.Repeat([]byte("y"), 60), 23)
	var a Assembler
	for _, i := range []int{0, 2, 1, 3} {
		if _, ok := a.Feed(chunks[i]); ok {
			t.Fatalf("chunk %d completed a message that lost its order", i)
		}
	}
}

func TestIndexZeroRestartsTheMessage(t *testing.T) {
	stale, _ := EncodeChunks(bytes.Repeat([]byte("o"), 60), 23)
	fresh, _ := EncodeChunks(bytes.Repeat([]byte("n"), 30), 23)
	var a Assembler
	a.Feed(stale[0])
	a.Feed(stale[1])
	if got, ok := feedAll(&a, fresh); !ok || !bytes.Equal(got, bytes.Repeat([]byte("n"), 30)) {
		t.Fatalf("Feed = %q, %v", got, ok)
	}
}

func TestChangedTotalMidMessageDiscardsIt(t *testing.T) {
	var a Assembler
	a.Feed([]byte("\xfe\x00\x03abc"))
	if _, ok := a.Feed([]byte("\xfe\x01\x02def")); ok {
		t.Fatal("a chunk with a new total completed a message")
	}
	if _, ok := a.Feed([]byte("\xfe\x02\x03ghi")); ok {
		t.Fatal("the discarded message completed")
	}
}

func TestZeroTotalIsIgnored(t *testing.T) {
	if _, ok := new(Assembler).Feed([]byte("\xfe\x00\x00abc")); ok {
		t.Fatal("a chunk of a zero-chunk message completed one")
	}
}

func TestOversizeMessageIsDiscarded(t *testing.T) {
	chunks, _ := EncodeChunks(make([]byte, MaxMessage+1), AssumedMTU)
	if _, ok := feedAll(new(Assembler), chunks); ok {
		t.Fatal("a message over MaxMessage was accepted")
	}
	chunks, _ = EncodeChunks(make([]byte, MaxMessage), AssumedMTU)
	if got, ok := feedAll(new(Assembler), chunks); !ok || len(got) != MaxMessage {
		t.Fatal("a message of exactly MaxMessage was not accepted")
	}
}
