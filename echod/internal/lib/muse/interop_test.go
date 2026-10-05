package muse

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestInteropWithTheSDK runs the client against a Python fake of Muse that is built on Meta's own
// code: the SDK's Noise XX responder, its chunk framing and its envelope codec. It is the check
// that this package's wire format is the SDK's and not merely consistent with itself.
//
// The fake is not in this repository, and it needs uv and an SDK checkout, so the test runs only
// when pointed at one:
//
//	MUSE_INTEROP_FAKE=/path/to/fake_muse.py go test -run Interop ./internal/lib/muse/
func TestInteropWithTheSDK(t *testing.T) {
	script := os.Getenv("MUSE_INTEROP_FAKE")
	if script == "" {
		t.Skip("MUSE_INTEROP_FAKE is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "uv", "run", "--no-project", "--with", "cryptography", "--with", "websockets", "python", script, "--voice")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	lines := bufio.NewReaderSize(stdout, 1<<20)
	var ports struct{ API, VM int }
	line, err := lines.ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &ports) != nil || ports.VM == 0 {
		t.Fatalf("the fake did not start: %q, %v", line, err)
	}

	fake := &fakeMuse{}
	store := &memStore{f: fake, state: State{Identity: testIdentity, Credentials: &Credentials{
		AccessToken:  "old-access",
		RefreshToken: "hatch_refresh:r1",
		APIURLV2:     fmt.Sprintf("http://127.0.0.1:%d", ports.API),
		NoiseHost:    fmt.Sprintf("ws://127.0.0.1:%d", ports.VM),
	}}}
	var asked atomic.Int32
	echo := echoCommand()
	client, err := New(Config{
		Store:       store,
		DisplayName: "Interop",
		Version:     "1.2.3",
		Commands: []Command{
			echo,
			{Name: "badge.show_message", Description: "Show text.", Required: echo.Required, Handler: echo.Handler},
			{Name: "test.progress", Handler: func(context.Context, json.RawMessage) (any, error) {
				return map[string]int32{"asks_done": asked.Load()}, nil
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.allowPlaintext = true
	client.t.final = 400 * time.Millisecond
	online := make(chan struct{}, 1)
	client.cfg.OnState = func(s ConnState) {
		if s.Phase == Online {
			online <- struct{}{}
		}
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-online:
	case err := <-done:
		t.Fatalf("Run ended before registering: %v", err)
	}

	wav := testWAV(40_000)
	for _, tc := range []struct {
		in   AskInput
		want Reply
	}{
		{Text("first"), Reply{Text: "you said: first"}},
		{Text("second"), Reply{Text: "you said: second"}},
		// The fake stores "..." as the words of every message, the voice note's included.
		{VoiceNote(wav), Reply{Text: "you said: a voice note", Heard: "..."}},
	} {
		reply, err := client.Ask(ctx, tc.in, nil)
		if err != nil || reply != tc.want {
			t.Fatalf("reply = %+v, %v; want %+v", reply, err, tc.want)
		}
		asked.Add(1)
	}
	// The fake finishes its script, a 120 KB echo and a ping, by removing the device.
	if err := <-done; !errors.Is(err, ErrUnpaired) {
		t.Fatalf("Run returned %v", err)
	}

	line, err = lines.ReadBytes('\n')
	if err != nil {
		t.Fatalf("no transcript: %v", err)
	}
	var transcript struct {
		API        []struct{ Method, Path, Authorization, Body string }
		VM         []map[string]json.RawMessage
		Chats      [][]json.RawMessage
		Subscribes int
		Voice      struct {
			WAVLen     int    `json:"wav_len"`
			SHA256     string `json:"sha256"`
			BodyChunks int    `json:"body_chunks"`
		}
	}
	if err := json.Unmarshal(line, &transcript); err != nil {
		t.Fatal(err)
	}
	var api []string
	for _, call := range transcript.API {
		api = append(api, call.Method+" "+call.Path+" "+call.Authorization)
	}
	if got := strings.Join(api, "\n"); got != "POST /device_token/refresh Bearer hatch_refresh:r1\nGET /fetch_vms Bearer new-access" {
		t.Errorf("API calls:\n%s", got)
	}
	// The rotated pair was saved, and then the unpairing cleared it and kept the identity.
	if len(store.saved) != 2 || store.saved[0].Credentials.AccessToken != "new-access" || store.saved[0].Credentials.RefreshToken != "r2" ||
		store.saved[1].Credentials != nil || store.saved[1].Identity != testIdentity {
		t.Errorf("saves = %+v", store.saved)
	}
	vm := map[string]string{}
	for _, entry := range transcript.VM {
		for key, value := range entry {
			vm[key] = string(value)
		}
	}
	if vm["path"] != `"/v1/noise?vm_id=vm%201"` || vm["authorization"] != `"Bearer vmtok"` {
		t.Errorf("upgrade = %s, %s", vm["path"], vm["authorization"])
	}
	if vm["control"] != `["POST", "/link-control", false]` {
		t.Errorf("control stream = %s", vm["control"])
	}
	if !strings.Contains(vm["register"], `"platform": "linux"`) || !strings.Contains(vm["register"], `"device_family": "homehub"`) || !strings.Contains(vm["register"], `"node_id": "homelink-a1b2c3"`) {
		t.Errorf("register = %s", vm["register"])
	}
	if vm["result"] != `{"method": "link.result", "id": "inv-1", "ok": true, "payload": {"text": "hello"}}` {
		t.Errorf("invoke result = %s", vm["result"])
	}
	if vm["big_result_ok"] != "true" || vm["big_echo_matches"] != "true" {
		t.Errorf("large echo: ok %s, matches %s", vm["big_result_ok"], vm["big_echo_matches"])
	}
	sum := sha256.Sum256(wav)
	if transcript.Voice.SHA256 != hex.EncodeToString(sum[:]) || transcript.Voice.WAVLen != len(wav) || transcript.Voice.BodyChunks != 4 {
		t.Errorf("voice note = %+v", transcript.Voice)
	}
	if len(transcript.Chats) != 3 || transcript.Subscribes != 1 {
		t.Errorf("%d chats, %d subscriptions", len(transcript.Chats), transcript.Subscribes)
	}
}
