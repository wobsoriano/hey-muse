package speech

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helper is a shell script standing in for techo5-pico, and the data directory it is given.
func helper(t *testing.T, script string) Local {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, Helper)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Local{Bin: bin, Data: dir}
}

// The helper is given the data directory and the text, and what it writes comes out as samples,
// odd reads and all. Here it writes the text back, so the samples are the text's own bytes.
func TestLocalSpeaksWhatTheHelperWrites(t *testing.T) {
	l := helper(t, `[ -d "$1" ] || exit 9; t=$(cat); printf %s "${t%?????}"; sleep 0.05; printf %s "${t#???}"`)
	var got []int16
	if err := l.Speak(context.Background(), "abcdefgh", func(s []int16) error {
		got = append(got, s...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []int16{'a' | 'b'<<8, 'c' | 'd'<<8, 'e' | 'f'<<8, 'g' | 'h'<<8}
	if len(got) != len(want) {
		t.Fatalf("samples %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("samples %v, want %v", got, want)
		}
	}
}

func TestLocalReportsWhatTheHelperSaid(t *testing.T) {
	l := helper(t, `echo "techo5-pico: no voice data" >&2; exit 1`)
	err := l.Speak(context.Background(), "x", func([]int16) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no voice data") {
		t.Fatalf("error %v", err)
	}
	err = Local{Bin: filepath.Join(t.TempDir(), "absent")}.Speak(context.Background(), "x", nil)
	if err == nil {
		t.Fatal("no error from a helper that is not there")
	}
}

// gone waits for a process to be no more.
func gone(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait); time.Sleep(10 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
	}
	t.Fatalf("the helper (pid %d) is still running", pid)
}

// A turn stopped halfway through the voice takes the helper with it, and so does a listener that
// fails: neither leaves a process speaking to nobody.
func TestLocalKillsTheHelper(t *testing.T) {
	for name, stop := range map[string]bool{"ctx ends": true, "out fails": false} {
		pidFile := filepath.Join(t.TempDir(), "pid")
		l := helper(t, `echo $$ > `+pidFile+`; printf abcd; exec sleep 60`)
		ctx, cancel := context.WithCancel(context.Background())
		refused := errors.New("refused")
		began := time.Now()
		err := l.Speak(ctx, "x", func([]int16) error {
			if stop {
				cancel()
				return nil
			}
			return refused
		})
		cancel()
		if stop && !errors.Is(err, context.Canceled) || !stop && !errors.Is(err, refused) {
			t.Errorf("%s: error %v", name, err)
		}
		if took := time.Since(began); took > 5*time.Second {
			t.Errorf("%s: Speak took %v to return", name, took)
		}
		gone(t, pidFile)
	}
}

func TestLocalAvailable(t *testing.T) {
	l := helper(t, "exit 0")
	if !l.Available() {
		t.Error("a helper and its data are not available")
	}
	if (Local{Bin: l.Bin, Data: filepath.Join(l.Data, "absent")}).Available() || (Local{Bin: l.Data + "/absent", Data: l.Data}).Available() {
		t.Error("available with the helper or the data missing")
	}
}

// Without a key the device's own voice speaks whatever is set; with one, the setting does.
func TestChosen(t *testing.T) {
	for _, c := range []struct {
		setting, key string
		want         Voice
	}{
		{"", "", BuiltIn}, {"coral", "", BuiltIn}, {"builtin", "k", BuiltIn}, {"coral", "k", "coral"}, {"", "k", DefaultVoice},
	} {
		if got := Chosen(c.setting, c.key); got != c.want {
			t.Errorf("Chosen(%q, %q) = %q, want %q", c.setting, c.key, got, c.want)
		}
	}
	if all := Choices(); all[0] != BuiltIn || len(all) != 1+len(Voices) || all[0].Label() != "Built in" {
		t.Errorf("choices %v", all)
	}
}

// Either engine comes out at 16 kHz: the endpoint's three samples for every two, the device's own
// as they are.
func TestSpeakerIsSixteenKilohertzFromEither(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 512)
		n, _ := r.Body.Read(raw)
		asked = string(raw[:n])
		_, _ = w.Write(pcm(0, 100, 200, 300, 400, 500))
	}))
	defer srv.Close()
	s := Speaker{Cloud: Client{Base: srv.URL, Key: "k", Voice: "alloy"}, Local: helper(t, "cat")}
	for v, want := range map[Voice]int{"coral": 4, BuiltIn: 6} {
		n := 0
		if err := s.Speak(context.Background(), v, "abcdefghijkl", func(s []int16) error { n += len(s); return nil }); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s: %d samples, want %d", v, n, want)
		}
	}
	if !strings.Contains(asked, `"voice":"coral"`) {
		t.Errorf("the endpoint was asked %s", asked)
	}
}

// The helper itself, where there is one: TECHO5_PICO is the program and TECHO5_PICO_DATA its voice
// data. A sentence comes out as a few seconds of something loud enough to be speech.
func TestLocalWithTheRealHelper(t *testing.T) {
	l := Local{Bin: os.Getenv("TECHO5_PICO"), Data: os.Getenv("TECHO5_PICO_DATA")}
	if l.Bin == "" {
		t.Skip("TECHO5_PICO is not set")
	}
	if !l.Available() {
		t.Fatalf("%+v is not available", l)
	}
	n, peak, calls := 0, 0, 0
	began := time.Now()
	var first time.Duration
	err := l.Speak(context.Background(), Spoken("The timer is set for twelve minutes. I’ll let you know."), func(s []int16) error {
		if calls++; calls == 1 {
			first = time.Since(began)
		}
		n += len(s)
		for _, v := range s {
			peak = max(peak, int(v), -int(v))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%.2f s of voice in %d reads, peak %d, first after %v, all after %v", float64(n)/16000, calls, peak, first, time.Since(began))
	if n < 2*16000 || n > 8*16000 || peak < 2000 {
		t.Errorf("%d samples peaking at %d is not that sentence spoken", n, peak)
	}
}
