package speech

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pcm(samples ...int16) []byte {
	b := make([]byte, 0, 2*len(samples))
	for _, s := range samples {
		b = append(b, byte(s), byte(s>>8))
	}
	return b
}

func TestSpeakStreamsSamplesAcrossOddReads(t *testing.T) {
	var got map[string]string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		all := pcm(1, -2, 300, -400, 32767)
		f := w.(http.Flusher)
		for _, part := range [][]byte{all[:3], all[3:4], all[4:]} {
			_, _ = w.Write(part)
			f.Flush()
		}
	}))
	defer srv.Close()

	var samples []int16
	c := Client{Base: srv.URL + "/v1/", Key: "k", Style: "warm"}
	err := c.Speak(context.Background(), "hello", func(s []int16) error {
		samples = append(samples, s...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []int16{1, -2, 300, -400, 32767}
	if len(samples) != len(want) {
		t.Fatalf("samples %v, want %v", samples, want)
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("samples %v, want %v", samples, want)
		}
	}
	if auth != "Bearer k" {
		t.Errorf("authorization %q", auth)
	}
	if got["input"] != "hello" || got["response_format"] != "pcm" || got["model"] != DefaultModel ||
		got["voice"] != DefaultVoice || got["instructions"] != "warm" {
		t.Errorf("request body %v", got)
	}
}

func TestSpeakReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := Client{Base: srv.URL, Key: "k"}.Speak(context.Background(), "x", func([]int16) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("error %v", err)
	}
}

func TestSpeakStopsWhenOutFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pcm(1, 2, 3, 4))
	}))
	defer srv.Close()
	stop := errors.New("stop")
	err := Client{Base: srv.URL, Key: "k"}.Speak(context.Background(), "x", func([]int16) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("error %v", err)
	}
}

func TestSpeakNeedsAKey(t *testing.T) {
	if err := (Client{}).Speak(context.Background(), "x", nil); err == nil {
		t.Fatal("no error without a key")
	}
}

func TestDownsamplerIsTheSameHoweverItIsFed(t *testing.T) {
	in := make([]int16, 31)
	for i := range in {
		in[i] = int16(i * 100)
	}
	var whole Downsampler
	want := whole.Write(in)
	if len(want) != 20 {
		t.Fatalf("%d samples out of 31, want 20", len(want))
	}
	if want[0] != 0 || want[1] != 150 || want[2] != 300 {
		t.Fatalf("starts %v", want[:3])
	}
	var parts Downsampler
	var got []int16
	for _, cut := range [][2]int{{0, 1}, {1, 5}, {5, 6}, {6, 20}, {20, 31}} {
		got = append(got, parts.Write(in[cut[0]:cut[1]])...)
	}
	if len(got) != len(want) {
		t.Fatalf("%d samples in parts, %d whole", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d is %d in parts, %d whole", i, got[i], want[i])
		}
	}
}

// What is spoken is plain: no markdown, no typographic punctuation for the voice to trip on.
func TestSpokenIsPlain(t *testing.T) {
	if got := Spoken("**It’s** eight thirty‑two."); got != "It's eight thirty-two." {
		t.Errorf("Spoken = %q", got)
	}
}
