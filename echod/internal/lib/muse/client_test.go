package muse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testIdentity = Identity{MAC: "02:11:22:a1:b2:c3"}

type harness struct {
	t      *testing.T
	fake   *fakeMuse
	store  *memStore
	client *Client
	states chan ConnState
	logs   *syncBuffer
	done   chan error
	cancel context.CancelFunc
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func echoCommand() Command {
	return Command{
		Name:        "echo",
		Description: "Say it back.",
		Required:    []Param{{Name: "text", Type: String, Description: "What to say."}},
		Optional:    []Param{{Name: "times", Type: Integer, Description: "How often."}},
		Timeout:     5 * time.Second,
		Handler: func(_ context.Context, args json.RawMessage) (any, error) {
			var in struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			return map[string]string{"text": in.Text}, nil
		},
	}
}

// start pairs a device with the fake and runs a client for it. tune may change the stored State
// and the configuration before the client is made.
func start(t *testing.T, tune func(h *harness, st *State, cfg *Config)) *harness {
	t.Helper()
	fake := newFakeMuse(t)
	h := &harness{
		t:      t,
		fake:   fake,
		store:  &memStore{f: fake},
		states: make(chan ConnState, 64),
		logs:   &syncBuffer{},
		done:   make(chan error, 1),
	}
	st := State{Identity: testIdentity, Credentials: &Credentials{
		AccessToken:        "access-0",
		RefreshToken:       "hatch_refresh:refresh-0",
		TokenType:          "device",
		Username:           "someone",
		APIURLV2:           fake.srv.URL,
		NoiseHost:          strings.TrimPrefix(fake.srv.URL, "https://"),
		AccessTokenSavedAt: time.Now().Unix(),
	}}
	cfg := Config{
		Store:       h.store,
		DisplayName: "Kitchen",
		Version:     "1.2.3",
		Commands:    []Command{echoCommand()},
		OnState:     func(s ConnState) { h.states <- s },
		Logger:      slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		TLS:         fake.srv.Client().Transport.(*http.Transport).TLSClientConfig,
	}
	if tune != nil {
		tune(h, &st, &cfg)
	}
	h.store.state = st
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.t.settle = 40 * time.Millisecond
	client.t.final = 150 * time.Millisecond
	client.t.backoffBase = 10 * time.Millisecond
	client.t.backoffMax = 40 * time.Millisecond
	client.t.authFloor = 20 * time.Millisecond
	client.t.tokenRetry = 40 * time.Millisecond
	h.client = client

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after its context ended")
		}
	})
	return h
}

// until waits for a state in the given phase and returns it, with every state seen on the way.
func (h *harness) until(phase Phase) (ConnState, []ConnState) {
	h.t.Helper()
	var seen []ConnState
	for {
		select {
		case s := <-h.states:
			seen = append(seen, s)
			if s.Phase == phase {
				return s, seen
			}
		case <-time.After(5 * time.Second):
			h.t.Fatalf("never reached %v; saw %v; events %q", phase, seen, h.fake.log())
		}
	}
}

func (h *harness) vm() *fakeVM {
	h.t.Helper()
	select {
	case vm := <-h.fake.vms:
		return vm
	case <-time.After(5 * time.Second):
		h.t.Fatalf("no VM connection; events %q", h.fake.log())
		return nil
	}
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// online starts a client and waits for it to register.
func online(t *testing.T, tune func(h *harness, st *State, cfg *Config)) (*harness, *fakeVM) {
	t.Helper()
	h := start(t, tune)
	h.until(Online)
	return h, h.vm()
}

// noSecrets fails if anything that must stay out of the log is in it.
func (h *harness) noSecrets(secrets ...string) {
	h.t.Helper()
	logged := h.logs.String()
	for _, secret := range append(secrets, "access-", "refresh-", "vmtok") {
		if strings.Contains(logged, secret) {
			h.t.Errorf("the log contains %q:\n%s", secret, logged)
		}
	}
}

func TestSessionRegisters(t *testing.T) {
	h, vm := online(t, func(_ *harness, _ *State, cfg *Config) {
		cfg.Commands = append(cfg.Commands, Command{
			Name:        "device.health",
			Description: "Report status.",
			Handler:     func(context.Context, json.RawMessage) (any, error) { return nil, nil },
		})
	})
	if vm.query != "vm_id=vm%201" {
		t.Errorf("query = %q", vm.query)
	}
	open := vm.controlOpen
	if open.verb != "POST" || open.path != "/link-control" || open.endBody || len(open.data) != 0 {
		t.Errorf("control stream opened as %+v", open)
	}
	register := receive(t, vm.registers, "link.register")
	if register["type"] != "req" || register["method"] != "link.register" || register["id"] == "" {
		t.Errorf("register = %v", register)
	}
	// The shape of DeviceDescription.register_params and COMMAND_SPECS in the SDK.
	var want map[string]any
	if err := json.Unmarshal([]byte(`{
		"node_id": "homelink-a1b2c3",
		"display_name": "Kitchen",
		"platform": "linux",
		"version": "1.2.3",
		"device_family": "homehub",
		"model_id": "linux",
		"is_wakeup_supported": false,
		"commands_v2": {
			"echo": {
				"description": "Say it back.",
				"required": {"text": {"type": "string", "description": "What to say."}},
				"optional": {"times": {"type": "integer", "description": "How often."}},
				"timeout_ms": 5000
			},
			"device.health": {"description": "Report status.", "required": {}, "optional": {}}
		}
	}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(register["params"], want) {
		got, _ := json.MarshalIndent(register["params"], "", "  ")
		t.Errorf("register params = %s", got)
	}
	if got := h.client.State(); got.Phase != Online || got.Err != nil {
		t.Errorf("State() = %+v", got)
	}
	events := h.fake.log()
	if len(events) != 2 || events[0] != "fetch Bearer access-0" || events[1] != "upgrade Bearer vmtok-0 -> 0" {
		t.Errorf("events = %q", events)
	}
	h.noSecrets()
}

func TestNewRefusesBadCommands(t *testing.T) {
	handler := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	for name, commands := range map[string][]Command{
		"ota":       {{Name: "device.ota", Handler: handler}},
		"twice":     {{Name: "a", Handler: handler}, {Name: "a", Handler: handler}},
		"unhandled": {{Name: "a"}},
		"param":     {{Name: "a", Handler: handler, Required: []Param{{Name: "p", Type: String}}, Optional: []Param{{Name: "p", Type: String}}}},
	} {
		if _, err := New(Config{Store: &memStore{}, Commands: commands}); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

func TestInvokeAnswered(t *testing.T) {
	_, vm := online(t, func(_ *harness, _ *State, cfg *Config) {
		cfg.Commands = append(cfg.Commands,
			Command{Name: "fail", Handler: func(context.Context, json.RawMessage) (any, error) {
				return nil, errors.New("it broke")
			}},
			Command{Name: "panic", Handler: func(context.Context, json.RawMessage) (any, error) {
				panic("boom")
			}},
			Command{Name: "deadline", Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				deadline, _ := ctx.Deadline()
				return map[string]bool{"soon": time.Until(deadline) < time.Second}, nil
			}},
		)
	})
	receive(t, vm.registers, "link.register")

	// 120 KB, so the invoke and its result each span two Noise chunks.
	big := strings.Repeat("badge ", 20000)
	for _, tc := range []struct {
		invoke map[string]any
		want   map[string]any
	}{
		{
			map[string]any{"id": "inv-1", "command": "echo", "params": map[string]any{"text": "hello"}, "timeout_ms": 5000},
			map[string]any{"method": "link.result", "id": "inv-1", "ok": true, "payload": map[string]any{"text": "hello"}},
		},
		{
			map[string]any{"id": "inv-2", "command": "echo", "params": map[string]any{"text": big}},
			map[string]any{"method": "link.result", "id": "inv-2", "ok": true, "payload": map[string]any{"text": big}},
		},
		{
			map[string]any{"id": "inv-3", "command": "fail"},
			map[string]any{"method": "link.result", "id": "inv-3", "ok": false, "error": "it broke"},
		},
		{
			map[string]any{"id": "inv-4", "command": "nope"},
			map[string]any{"method": "link.result", "id": "inv-4", "ok": false, "error": "unsupported command: nope"},
		},
		{
			map[string]any{"id": "inv-5", "command": "panic"},
			map[string]any{"method": "link.result", "id": "inv-5", "ok": false, "error": "panic: boom"},
		},
		{
			map[string]any{"id": "inv-6", "command": "deadline", "timeout_ms": 200},
			map[string]any{"method": "link.result", "id": "inv-6", "ok": true, "payload": map[string]any{"soon": true}},
		},
	} {
		tc.invoke["method"] = "link.invoke"
		vm.message(tc.invoke)
		got := receive(t, vm.results, "link.result")
		if !reflect.DeepEqual(got, tc.want) {
			if text, _ := tc.invoke["params"].(map[string]any)["text"].(string); len(text) > 100 {
				t.Errorf("%v: the large result did not match", tc.invoke["id"])
			} else {
				t.Errorf("%v: result = %v, want %v", tc.invoke["id"], got, tc.want)
			}
		}
	}
}

// collect returns an Ask callback and a way to read what it has been given.
func collect() (func(ReplyEvent), func() []ReplyEvent) {
	var mu sync.Mutex
	var events []ReplyEvent
	return func(e ReplyEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
		}, func() []ReplyEvent {
			mu.Lock()
			defer mu.Unlock()
			return append([]ReplyEvent(nil), events...)
		}
}

func TestAskText(t *testing.T) {
	h, vm := online(t, nil)
	on, events := collect()
	reply, err := h.client.Ask(context.Background(), Text("hello"), on)
	if err != nil {
		t.Fatal(err)
	}
	if reply != (Reply{Text: "you said: hello"}) {
		t.Errorf("reply = %+v", reply)
	}
	// The earlier message on the stream is not part of the answer, and a typed message has no
	// transcript to report. The fake sends the whole answer ahead of the acknowledgment, as the
	// real service can, so it is all there by the time the device knows which message was its own.
	want := []ReplyEvent{ReplyText{Text: "you said: hello"}, Settled{Text: "you said: hello"}}
	if got := events(); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %#v", got)
	}

	seen := receive(t, vm.chats, "the chat")
	wantBody := map[string]any{"message": "hello", "output_modality": "text", "device_id": "homelink-a1b2c3"}
	if !reflect.DeepEqual(seen.body, wantBody) || !seen.inRequest {
		t.Errorf("chat = %+v", seen)
	}
	if seen.headers["x-app-id"] != "musegadget" || seen.headers["Content-Type"] != "application/json" || seen.headers["x-request-id"] == "" {
		t.Errorf("chat headers = %v", seen.headers)
	}

	// The second question's answer is its own, and the chat stream is opened once for the session.
	reply, err = h.client.Ask(context.Background(), Text("again"), nil)
	if err != nil || reply.Text != "you said: again" {
		t.Errorf("second reply = %+v, %v", reply, err)
	}
	if h.fake.subscribes != 1 {
		t.Errorf("subscribed %d times", h.fake.subscribes)
	}
	if strings.Contains(h.logs.String(), "hello") {
		t.Errorf("a message body reached the log:\n%s", h.logs.String())
	}
}

func TestReplyStreams(t *testing.T) {
	h, _ := online(t, func(h *harness, _ *State, _ *Config) { h.fake.hold = make(chan struct{}) })
	on, events := collect()
	var release sync.Once
	reply, err := h.client.Ask(context.Background(), Text("hello"), func(e ReplyEvent) {
		on(e)
		release.Do(func() { close(h.fake.hold) })
	})
	if err != nil || reply.Text != "you said: hello" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	// The first half is shown while Muse is still working, and is not mistaken for the answer.
	want := []ReplyEvent{
		ReplyText{Text: "you sai"},
		ReplyText{Text: "you said: hello"},
		Settled{Text: "you said: hello"},
	}
	if got := events(); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %#v", got)
	}
}

func testWAV(size int) []byte {
	wav := make([]byte, size)
	for i := range wav {
		wav[i] = byte(i * 7)
	}
	copy(wav, "RIFF\x00\x00\x00\x00WAVEfmt ")
	return wav
}

func TestAskVoiceNote(t *testing.T) {
	h, vm := online(t, nil)
	wav := testWAV(40_000)
	on, events := collect()
	reply, err := h.client.Ask(context.Background(), VoiceNote(wav), on)
	if err != nil {
		t.Fatal(err)
	}
	if reply != (Reply{Text: "you said: a voice note", Heard: "a voice note"}) {
		t.Errorf("reply = %+v", reply)
	}
	got := events()
	if got[0] != (Heard{Text: "a voice note"}) || got[len(got)-1] != (Settled{Text: "you said: a voice note"}) {
		t.Errorf("events = %#v", got)
	}

	seen := receive(t, vm.chats, "the chat")
	sum := sha256.Sum256(wav)
	if seen.wavSHA != hex.EncodeToString(sum[:]) {
		t.Error("the recording did not arrive intact")
	}
	// As the firmware sends it, the recording as one attachment and the body in 16 KB pieces after
	// a request frame that carries none of it, and with the device named, as a typed message is.
	wantBody := map[string]any{"message": "", "output_modality": "text", "device_id": "homelink-a1b2c3", "items": []any{
		map[string]any{"type": "file", "mime_type": "audio/wav", "filename": "voice_note.wav"},
	}}
	if !reflect.DeepEqual(seen.body, wantBody) {
		t.Errorf("chat body = %v", seen.body)
	}
	bodyLen := len(`{"message":"","output_modality":"text","device_id":"homelink-a1b2c3","items":[{"type":"file","mime_type":"audio/wav","filename":"voice_note.wav","data_base64":""}]}`) + (len(wav)+2)/3*4
	if wantChunks := (bodyLen + bodyChunk - 1) / bodyChunk; seen.inRequest || seen.chunks != wantChunks {
		t.Errorf("sent in %d body chunks (whole in the request: %v), want %d", seen.chunks, seen.inRequest, wantChunks)
	}
}

// A command for a turn the device began comes as an event on the chat stream, not on the control
// stream, and is answered the way the other kind is: run, and a link.result with the event's id.
func TestACommandOnTheChatStreamIsRunAndAnswered(t *testing.T) {
	ran := make(chan string, 1)
	h, vm := online(t, func(_ *harness, _ *State, cfg *Config) {
		cfg.Commands = append(cfg.Commands, Command{Name: "volume", Handler: func(_ context.Context, args json.RawMessage) (any, error) {
			ran <- string(args)
			return map[string]string{"result": "volume is 10 of 30"}, nil
		}})
	})
	// The chat stream opens with the first question.
	if _, err := h.client.Ask(context.Background(), Text("hello"), func(ReplyEvent) {}); err != nil {
		t.Fatal(err)
	}
	vm.streamBytes([]byte(`{"type":"event","event":"client.invoke","ts_ms":1,"payload":{"command_id":"volume",` +
		`"invoke_id":"inv-chat","params_json":"{\"level\":10}","timeout_ms":30000}}` + "\n"))
	if got := receive(t, ran, "the command"); got != `{"level":10}` {
		t.Errorf("the command was given %s", got)
	}
	want := map[string]any{"method": "link.result", "id": "inv-chat", "ok": true, "payload": map[string]any{"result": "volume is 10 of 30"}}
	if got := receive(t, vm.results, "link.result"); !reflect.DeepEqual(got, want) {
		t.Errorf("result = %v, want %v", got, want)
	}
}

func TestAskRefusesBadInput(t *testing.T) {
	h, vm := online(t, nil)
	for name, in := range map[string]AskInput{
		"zero":      {},
		"blank":     Text("  "),
		"long":      Text(strings.Repeat("a", maxAskText+1)),
		"not a wav": VoiceNote([]byte("definitely not audio")),
		"huge":      VoiceNote(testWAV(maxVoiceNote + 1)),
	} {
		if _, err := h.client.Ask(context.Background(), in, nil); err == nil {
			t.Errorf("%s: Ask took it", name)
		}
	}
	select {
	case seen := <-vm.chats:
		t.Errorf("something was sent: %+v", seen)
	default:
	}
}

func TestAskWhileOffline(t *testing.T) {
	client, err := New(Config{Store: &memStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ask(context.Background(), Text("hello"), nil); !errors.Is(err, ErrOffline) {
		t.Errorf("err = %v", err)
	}
}

func TestTokenRotationIsSavedBeforeUse(t *testing.T) {
	h, _ := online(t, func(_ *harness, st *State, cfg *Config) {
		st.Credentials.AccessTokenSavedAt = time.Now().Add(-4 * time.Hour).Unix()
		cfg.SDKToken = "sdk-secret"
	})
	events := h.fake.log()
	want := []string{
		"refresh Bearer hatch_refresh:refresh-0",
		"save access-1",
		"fetch Bearer access-1",
		"upgrade Bearer vmtok-0 -> 0",
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if body := h.fake.refreshBodies[0]; body["device_id"] != "homelink-a1b2c3" || body["sdk_token"] != "sdk-secret" {
		t.Errorf("refresh body = %v", body)
	}
	saved := h.store.current()
	creds := saved.Credentials
	if creds.AccessToken != "access-1" || creds.RefreshToken != "refresh-1" || time.Since(time.Unix(creds.AccessTokenSavedAt, 0)) > time.Minute {
		t.Errorf("saved credentials = %+v", creds)
	}
	if creds.Username != "someone" || creds.TokenType != "device" || saved.Identity != testIdentity {
		t.Errorf("the rest of the state was not kept: %+v", saved)
	}
	if !saved.SDKTokenReported {
		t.Error("the SDK token was sent but not marked as reported")
	}
	h.noSecrets("sdk-secret")
}

func TestSDKTokenIsReportedOnce(t *testing.T) {
	h, _ := online(t, func(_ *harness, _ *State, cfg *Config) { cfg.SDKToken = "sdk-secret" })
	if events := h.fake.log(); events[0] != "refresh Bearer hatch_refresh:refresh-0" || events[1] != "save access-1" {
		t.Errorf("a fresh token was not rotated to report the SDK token: %q", events)
	}

	h, _ = online(t, func(_ *harness, st *State, cfg *Config) {
		cfg.SDKToken = "sdk-secret"
		st.SDKTokenReported = true
	})
	if events := h.fake.log(); events[0] != "fetch Bearer access-0" {
		t.Errorf("a reported SDK token was reported again: %q", events)
	}
}

func TestSaveFailureHoldsTheRotatedTokens(t *testing.T) {
	h := start(t, func(h *harness, st *State, _ *Config) {
		st.Credentials.AccessTokenSavedAt = 0
		h.store.failSaves = 3
	})
	offline, _ := h.until(Offline)
	if offline.Err == nil || !strings.Contains(offline.Err.Error(), "could not save the rotated tokens") {
		t.Errorf("offline for %v", offline.Err)
	}
	// The pair in the store is dead on the server, but it is all the store has, so it stays.
	if got := h.store.current().Credentials.AccessToken; got != "access-0" {
		t.Errorf("the stored token became %q before a save succeeded", got)
	}
	h.until(Online)

	events := h.fake.log()
	want := []string{
		"refresh Bearer hatch_refresh:refresh-0",
		"save failed access-1",
		"save failed access-1",
		"save failed access-1",
		"save access-1",
		"fetch Bearer access-1",
		"upgrade Bearer vmtok-0 -> 0",
	}
	// One refresh only: the dead refresh token is never presented again, and the new access
	// token goes nowhere until the save that follows the three failures.
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if got := h.store.current().Credentials; got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" {
		t.Errorf("saved credentials = %+v", got)
	}
}

func TestRejectedTokenIsRefreshed(t *testing.T) {
	h, _ := online(t, func(h *harness, _ *State, _ *Config) {
		// The server has moved on from the token the device holds, though the refresh token
		// is still good.
		h.fake.access = "access-elsewhere"
	})
	want := []string{
		"fetch Bearer access-0",
		"refresh Bearer hatch_refresh:refresh-0",
		"save access-1",
		"fetch Bearer access-1",
		"upgrade Bearer vmtok-0 -> 0",
	}
	if events := h.fake.log(); !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestRevokedPairingUnpairs(t *testing.T) {
	h := start(t, func(h *harness, st *State, _ *Config) {
		st.Credentials.AccessTokenSavedAt = 0
		h.fake.refresh = "refresh-elsewhere"
	})
	h.until(Unpaired)
	if err := receive(t, h.done, "Run"); !errors.Is(err, ErrUnpaired) {
		t.Errorf("Run returned %v", err)
	}
	h.done <- nil
	if saved := h.store.current(); saved.Credentials != nil || saved.Identity != testIdentity {
		t.Errorf("state after revocation = %+v", saved)
	}
}

// A VM that is not behind its front door yet is asked again with the same bearer, as often as the
// API said, and the device is never reported offline for it: asked once and then left for a minute,
// the real one stayed shut for half an hour.
func TestAColdVMIsAskedAgainUntilItOpens(t *testing.T) {
	h := start(t, func(h *harness, _ *State, _ *Config) {
		h.fake.coldUpgrades, h.fake.knockEveryMs = 5, 100
	})
	_, seen := h.until(Online)
	for _, s := range seen {
		if s.Phase == Offline {
			t.Fatalf("reported offline before the VM opened: %v", s.Err)
		}
	}
	want := []string{"fetch Bearer access-0"}
	for range 5 {
		want = append(want, "upgrade Bearer vmtok-0 -> 403")
	}
	want = append(want, "upgrade Bearer vmtok-0 -> 0")
	if events := h.fake.log(); !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

// A door that stays shut through a spell of asking is not asked that way again for a while: the
// rounds after it try once each, at the backoff's pace, however often the API says to ask.
func TestADoorThatStaysShutIsNotAskedAgainEveryRound(t *testing.T) {
	h := start(t, func(h *harness, _ *State, _ *Config) {
		h.fake.coldUpgrades, h.fake.knockEveryMs, h.fake.knockTries = 6, 100, 3
	})
	h.until(Online)
	want := []string{"fetch Bearer access-0"}
	for range 4 { // the first try and the three more the API allowed
		want = append(want, "upgrade Bearer vmtok-0 -> 403")
	}
	for range 2 { // then one try a round
		want = append(want, "fetch Bearer access-0", "upgrade Bearer vmtok-0 -> 403")
	}
	want = append(want, "fetch Bearer access-0", "upgrade Bearer vmtok-0 -> 0")
	if events := h.fake.log(); !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestUpgradeRefusalFetchesFreshCredentials(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		h := start(t, func(h *harness, _ *State, _ *Config) {
			h.fake.rejectUpgrades, h.fake.rejectStatus = 1, status
		})
		offline, _ := h.until(Offline)
		if offline.Err == nil || !strings.Contains(offline.Err.Error(), "refused the connection") {
			t.Errorf("%d: offline for %v", status, offline.Err)
		}
		h.until(Online)
		want := []string{
			"fetch Bearer access-0",
			"upgrade Bearer vmtok-0 -> " + strconv.Itoa(status),
			"fetch Bearer access-0",
			"upgrade Bearer vmtok-0+ -> 0",
		}
		if events := h.fake.log(); !reflect.DeepEqual(events, want) {
			t.Errorf("%d: events = %q, want %q", status, events, want)
		}
		h.cancel()
	}
}

func TestReconnectsAfterTheServerDrops(t *testing.T) {
	h, vm := online(t, nil)
	receive(t, vm.registers, "link.register")
	_ = vm.ws.Close()

	_, seen := h.until(Online)
	if len(seen) != 3 || seen[0].Phase != Offline || seen[0].Err == nil || seen[1].Phase != Connecting {
		t.Errorf("states after the drop = %+v", seen)
	}
	again := h.vm()
	receive(t, again.registers, "the second link.register")

	// The new session is a whole one: it carries an ask.
	if reply, err := h.client.Ask(context.Background(), Text("back"), nil); err != nil || reply.Text != "you said: back" {
		t.Errorf("reply = %+v, %v", reply, err)
	}
}

func TestDropMidAskFailsTheAsk(t *testing.T) {
	h, vm := online(t, func(h *harness, _ *State, _ *Config) { h.fake.hold = make(chan struct{}) })
	defer close(h.fake.hold)
	_, err := h.client.Ask(context.Background(), Text("hello"), func(e ReplyEvent) {
		if _, ok := e.(ReplyText); ok {
			_ = vm.ws.Close()
		}
	})
	if !errors.Is(err, ErrOffline) {
		t.Errorf("err = %v", err)
	}
}

func TestUnpairedByMuse(t *testing.T) {
	h, vm := online(t, nil)
	receive(t, vm.registers, "link.register")
	vm.message(map[string]any{"type": "evt", "event": "link.unpaired"})

	h.until(Unpaired)
	if err := receive(t, h.done, "Run"); !errors.Is(err, ErrUnpaired) {
		t.Errorf("Run returned %v", err)
	}
	h.done <- nil
	if saved := h.store.current(); saved.Credentials != nil || saved.Identity != testIdentity {
		t.Errorf("state after unpairing = %+v", saved)
	}
	if _, err := h.client.Ask(context.Background(), Text("hello"), nil); !errors.Is(err, ErrOffline) {
		t.Errorf("Ask after unpairing: %v", err)
	}
	// With nothing to connect with, Run says so at once and touches nothing.
	before := len(h.fake.log())
	if err := h.client.Run(context.Background()); !errors.Is(err, ErrUnpaired) {
		t.Errorf("Run on an unpaired device returned %v", err)
	}
	if after := len(h.fake.log()); after != before {
		t.Errorf("an unpaired Run reached the server: %q", h.fake.log()[before:])
	}
}

func TestCancelMidAskDeliversNothingAfter(t *testing.T) {
	h, vm := online(t, func(h *harness, _ *State, _ *Config) { h.fake.hold = make(chan struct{}) })
	ctx, cancel := context.WithCancel(context.Background())
	on, events := collect()
	busy := make(chan error, 1)
	_, err := h.client.Ask(ctx, Text("hello"), func(e ReplyEvent) {
		on(e)
		if _, ok := e.(ReplyText); ok {
			// While one answer is open, a second question is refused.
			_, err := h.client.Ask(context.Background(), Text("me too"), nil)
			busy <- err
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask returned %v", err)
	}
	if err := <-busy; !errors.Is(err, ErrBusy) {
		t.Errorf("the overlapping Ask returned %v", err)
	}
	delivered := events()
	if len(delivered) != 1 || delivered[0] != (ReplyText{Text: "you sai"}) {
		t.Errorf("delivered before the cancel: %#v", delivered)
	}

	// The rest of the answer arrives after the cancel, and long enough passes for it to have
	// settled and finished.
	h.fake.mu.Lock()
	hold := h.fake.hold
	h.fake.hold = nil
	h.fake.mu.Unlock()
	close(hold)
	receive(t, vm.released, "the rest of the answer")
	time.Sleep(3 * h.client.t.final)
	if after := events(); len(after) != len(delivered) {
		t.Errorf("delivered after the cancel: %#v", after[len(delivered):])
	}

	// The abandoned answer does not leak into the next one.
	reply, err := h.client.Ask(context.Background(), Text("again"), nil)
	if err != nil || reply.Text != "you said: again" {
		t.Errorf("next reply = %+v, %v", reply, err)
	}
}

func TestCancelMidUploadWithdrawsTheVoiceNote(t *testing.T) {
	h, vm := online(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.client.Ask(ctx, VoiceNote(testWAV(40_000)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask returned %v", err)
	}
	// The next ask is answered, so the reset has been read by then.
	if _, err := h.client.Ask(context.Background(), Text("after"), nil); err != nil {
		t.Fatal(err)
	}
	if indexOf(h.fake.log(), "reset canceled") < 0 {
		t.Errorf("the upload was not reset: %q", h.fake.log())
	}
	if seen := receive(t, vm.chats, "the chat"); seen.body["message"] != "after" {
		t.Errorf("the abandoned voice note was taken: %+v", seen.body)
	}
}
