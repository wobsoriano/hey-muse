package muse

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/flynn/noise"
	"github.com/gorilla/websocket"
)

// fakeMuse stands in for the Muse API and a VM, over TLS on one port. Its event shapes were
// captured from the real service: change it only to match something observed, never to make a
// test pass.
type fakeMuse struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	events   []string // API calls, upgrades and Store saves, in the order they happened
	access   string
	refresh  string
	rotation int
	bearer   string
	// coldUpgrades is how many upgrades the front door answers 403 before the VM is behind it, as
	// the real one does after a quiet spell; knockEvery is the hint fetch_vms then carries.
	coldUpgrades int
	knockEveryMs int
	// rejectUpgrades is how many WebSocket upgrades to refuse, and with what status.
	rejectUpgrades int
	rejectStatus   int
	refreshBodies  []map[string]string
	subscribes     int
	chatCount      int
	// hold, when set, stops each reply half way until released.
	hold chan struct{}

	vms chan *fakeVM
}

func newFakeMuse(t *testing.T) *fakeMuse {
	f := &fakeMuse{t: t, access: "access-0", refresh: "refresh-0", bearer: "vmtok-0", vms: make(chan *fakeVM, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device_token/refresh", f.handleRefresh)
	mux.HandleFunc("GET /fetch_vms", f.handleFetch)
	mux.HandleFunc("GET /v1/noise", f.handleNoise)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMuse) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, args...))
}

func (f *fakeMuse) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeMuse) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.events = append(f.events, "refresh "+r.Header.Get("Authorization"))
	f.refreshBodies = append(f.refreshBodies, body)
	if r.Header.Get("Authorization") != "Bearer hatch_refresh:"+f.refresh {
		f.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.rotation++
	f.access = fmt.Sprintf("access-%d", f.rotation)
	f.refresh = fmt.Sprintf("refresh-%d", f.rotation)
	reply := map[string]any{"payload": map[string]string{"access_token": f.access, "refresh_token": f.refresh}}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeMuse) handleFetch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.events = append(f.events, "fetch "+r.Header.Get("Authorization"))
	ok := r.Header.Get("Authorization") == "Bearer "+f.access && r.Header.Get("X-API-Version") == "1.0.0"
	bearer := f.bearer
	knockEveryMs := f.knockEveryMs
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	answer := map[string]any{}
	if knockEveryMs > 0 {
		answer["retry_after_ms"], answer["max_retry_count"] = knockEveryMs, 80
	}
	answer["vm_list"] = []map[string]any{
		{"vm_ws_url": "wss://ignored", "vm_auth_token": "skip", "vm_name": "other", "vm_id": "vm0"},
		{"vm_ws_url": "wss://ignored", "vm_auth_token": bearer, "vm_name": "test", "vm_id": "vm 1", "default": true},
	}
	_ = json.NewEncoder(w).Encode(answer)
}

func (f *fakeMuse) handleNoise(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := 0
	switch {
	case f.coldUpgrades > 0:
		f.coldUpgrades--
		status = http.StatusForbidden
	case f.rejectUpgrades > 0:
		f.rejectUpgrades--
		status = f.rejectStatus
		// A refused bearer is spent: the next fetch hands out another.
		f.bearer += "+"
	case r.Header.Get("Authorization") != "Bearer "+f.bearer:
		status = http.StatusUnauthorized
	}
	f.events = append(f.events, fmt.Sprintf("upgrade %s -> %d", r.Header.Get("Authorization"), status))
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	vm := &fakeVM{
		f:         f,
		ws:        ws,
		query:     r.URL.RawQuery,
		uploads:   map[int64]*upload{},
		registers: make(chan map[string]any, 1),
		results:   make(chan map[string]any, 16),
		chats:     make(chan chat, 16),
		released:  make(chan struct{}, 16),
	}
	if err := vm.handshake(); err != nil {
		f.t.Errorf("fake VM handshake: %v", err)
		return
	}
	f.vms <- vm
	vm.serve()
}

type upload struct {
	request frame
	body    []byte
	chunks  int
}

// chat is one POST /chat/stream as the VM saw it.
type chat struct {
	body      map[string]any
	headers   map[string]string
	chunks    int  // body chunks after the request frame
	inRequest bool // the request frame carried the whole body
	wavSHA    string
}

type fakeVM struct {
	f     *fakeMuse
	ws    *websocket.Conn
	query string

	wmu  sync.Mutex
	send *noise.CipherState
	recv *noise.CipherState

	chunks       assembler
	control      int64
	controlOpen  frame
	controlBuf   []byte
	subscription int64
	uploads      map[int64]*upload

	registers chan map[string]any
	results   chan map[string]any
	chats     chan chat
	// released hears each time the held half of a reply has gone out.
	released chan struct{}
}

func (vm *fakeVM) handshake() error {
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)
	static, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return err
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Random: rand.Reader, Pattern: noise.HandshakeXX, StaticKeypair: static,
	})
	if err != nil {
		return err
	}
	_, msg1, err := vm.ws.ReadMessage()
	if err != nil {
		return err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg1); err != nil {
		return err
	}
	msg2, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return err
	}
	if err := vm.ws.WriteMessage(websocket.BinaryMessage, msg2); err != nil {
		return err
	}
	_, msg3, err := vm.ws.ReadMessage()
	if err != nil {
		return err
	}
	_, fromDevice, toDevice, err := hs.ReadMessage(nil, msg3)
	vm.recv, vm.send = fromDevice, toDevice
	return err
}

func (vm *fakeVM) serve() {
	for {
		_, data, err := vm.ws.ReadMessage()
		if err != nil {
			return
		}
		plain, err := vm.recv.Decrypt(nil, nil, data)
		if err != nil {
			vm.f.t.Errorf("fake VM decrypt: %v", err)
			return
		}
		envelope, err := vm.chunks.add(plain)
		if err != nil {
			vm.f.t.Errorf("fake VM reassembly: %v", err)
			return
		}
		if envelope == nil {
			continue
		}
		f, err := decodeServiceRequest(envelope)
		if err != nil {
			vm.f.t.Errorf("fake VM envelope: %v", err)
			return
		}
		vm.onFrame(f)
	}
}

// decodeServiceRequest is the VM's side of encodeServiceRequest.
func decodeServiceRequest(b []byte) (frame, error) {
	var payload []byte
	r := protoReader{b: b}
	for {
		field, wire, ok := r.next()
		if !ok {
			break
		}
		if field == 2 {
			payload = r.bytes(wire)
		} else {
			r.skip(wire)
		}
	}
	if r.err != nil {
		return frame{}, r.err
	}
	return decodeFrame(payload)
}

func (vm *fakeVM) write(f frame) {
	vm.wmu.Lock()
	defer vm.wmu.Unlock()
	pieces, err := chunk(appendBytesField(nil, 1, encodeFrame(f)))
	if err != nil {
		vm.f.t.Errorf("fake VM chunk: %v", err)
		return
	}
	for _, piece := range pieces {
		sealed, err := vm.send.Encrypt(nil, nil, piece)
		if err != nil {
			vm.f.t.Errorf("fake VM encrypt: %v", err)
			return
		}
		// The device may already have gone, which is some tests' whole point.
		_ = vm.ws.WriteMessage(websocket.BinaryMessage, sealed)
	}
}

// message sends one control message to the device.
func (vm *fakeVM) message(m map[string]any) {
	data, _ := encodeControl(m)
	vm.write(frame{stream: vm.control, kind: frameBody, data: data})
}

func (vm *fakeVM) onFrame(f frame) {
	switch {
	case f.kind == frameRequest && f.path == controlPath:
		vm.control, vm.controlOpen = f.stream, f
		vm.write(frame{stream: f.stream, kind: frameResponse, status: 200})
	case f.kind == frameRequest && f.endBody:
		vm.answer(f.stream, f, f.data, 0)
	case f.kind == frameRequest:
		vm.uploads[f.stream] = &upload{request: f, body: append([]byte(nil), f.data...)}
	case f.kind == frameBody && f.stream == vm.control:
		vm.onControl(f.data)
	case vm.uploads[f.stream] != nil && f.kind == frameReset:
		delete(vm.uploads, f.stream)
		vm.f.record("reset %s", f.reason)
	case vm.uploads[f.stream] != nil:
		up := vm.uploads[f.stream]
		up.body = append(up.body, f.data...)
		up.chunks++
		if f.endBody {
			delete(vm.uploads, f.stream)
			vm.answer(f.stream, up.request, up.body, up.chunks)
		}
	}
}

func (vm *fakeVM) onControl(data []byte) {
	vm.controlBuf = append(vm.controlBuf, data...)
	for len(vm.controlBuf) >= 4 {
		n := int(binary.LittleEndian.Uint32(vm.controlBuf))
		if len(vm.controlBuf) < 4+n {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(vm.controlBuf[4:4+n], &m); err != nil {
			vm.f.t.Errorf("fake VM control message: %v", err)
		}
		vm.controlBuf = vm.controlBuf[4+n:]
		switch m["method"] {
		case "link.register":
			vm.registers <- m
			vm.message(map[string]any{"type": "res", "id": m["id"], "ok": true})
		case "link.result":
			vm.results <- m
		}
	}
}

func (vm *fakeVM) answer(stream int64, request frame, body []byte, chunks int) {
	headers := map[string]string{}
	for _, h := range request.headers {
		headers[h.key] = h.value
	}
	switch request.verb + " " + request.path {
	case "POST " + subscribePath:
		vm.f.mu.Lock()
		vm.f.subscribes++
		vm.f.mu.Unlock()
		vm.subscription = stream
		vm.write(frame{stream: stream, kind: frameResponse, status: 200, data: []byte("{\"type\":\"ack\"}\n")})
	case "POST " + chatPath:
		seen := chat{headers: headers, chunks: chunks, inRequest: request.endBody}
		if err := json.Unmarshal(body, &seen.body); err != nil {
			vm.f.t.Errorf("fake VM chat body: %v", err)
		}
		heard, _ := seen.body["message"].(string)
		if items, _ := seen.body["items"].([]any); len(items) > 0 {
			item := items[0].(map[string]any)
			wav, err := base64.StdEncoding.DecodeString(item["data_base64"].(string))
			if err != nil {
				vm.f.t.Errorf("fake VM voice note: %v", err)
			}
			sum := sha256.Sum256(wav)
			seen.wavSHA = hex.EncodeToString(sum[:])
			delete(item, "data_base64")
			// Stands in for the transcript the real service makes of the audio.
			heard = "a voice note"
		}
		vm.chats <- seen
		vm.f.mu.Lock()
		vm.f.chatCount++
		userID := fmt.Sprintf("u%d", vm.f.chatCount)
		vm.f.mu.Unlock()
		var held []byte
		var hold chan struct{}
		if vm.subscription != 0 {
			// Events race the acknowledgment on the real service, so they go first here.
			held, hold = vm.streamReply(userID, heard, "you said: "+heard)
		}
		ack, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]string{"message_id": userID}})
		vm.write(frame{stream: stream, kind: frameResponse, status: 200, data: ack, endBody: true})
		if hold != nil {
			go func() {
				<-hold
				vm.streamBytes(held)
				vm.released <- struct{}{}
			}()
		}
	default:
		vm.write(frame{stream: stream, kind: frameResponse, status: 404, endBody: true})
	}
}

// streamReply sends the events of one answer. If replies are being held, it stops half way and
// returns the rest with the channel that releases it.
func (vm *fakeVM) streamReply(userID, heard, text string) ([]byte, chan struct{}) {
	seq := 0
	line := func(name string, payload map[string]any) []byte {
		seq++
		b, _ := json.Marshal(map[string]any{"type": "event", "seq": seq, "event": name, "payload": payload})
		return append(b, '\n')
	}
	// The shapes the real service sends: the reply names no parent, its text chunks name
	// themselves as parent, and "done" carries no text.
	replyID := "assistant-msg-" + userID
	half := len(text) / 2
	first := [][]byte{
		line("delta.message_start", map[string]any{"message_id": "earlier", "reply_to_message_id": ""}),
		line("delta.text_append", map[string]any{"message_id": "earlier", "text": "not for this device"}),
		line("message.user", map[string]any{"message_id": userID, "display_text": heard}),
		line("agent.status", map[string]any{"activity_code": "working"}),
		line("delta.message_start", map[string]any{"message_id": replyID, "reply_to_message_id": ""}),
		line("delta.text_append", map[string]any{"message_id": replyID, "parent_message_id": replyID, "text": text[:half]}),
	}
	second := [][]byte{
		line("delta.text_append", map[string]any{"message_id": replyID, "parent_message_id": replyID, "text": text[half:]}),
		line("agent.status", map[string]any{"activity_code": "online"}),
		line("delta.message_done", map[string]any{"message_id": replyID, "reply_to_message_id": "", "status": "completed"}),
	}
	vm.f.mu.Lock()
	hold := vm.f.hold
	vm.f.mu.Unlock()
	if hold != nil {
		vm.streamBytes(join(first))
		return join(second), hold
	}
	vm.streamBytes(join(append(first, second...)))
	return nil, nil
}

func join(lines [][]byte) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
	}
	return out
}

// streamBytes sends chat stream data split mid-line, so the device has to put lines back together.
func (vm *fakeVM) streamBytes(data []byte) {
	for start := 0; start < len(data); start += 97 {
		vm.write(frame{stream: vm.subscription, kind: frameBody, data: data[start:min(len(data), start+97)]})
	}
}

// memStore is a Store in memory that records every Save in the fake's event log, and can be told
// to fail.
type memStore struct {
	f *fakeMuse

	mu        sync.Mutex
	state     State
	failSaves int
	saved     []State
}

func (m *memStore) Load() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, nil
}

func (m *memStore) Save(st State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	access := "none"
	if st.Credentials != nil {
		access = st.Credentials.AccessToken
	}
	if m.failSaves > 0 {
		m.failSaves--
		m.f.record("save failed %s", access)
		return io.ErrShortWrite
	}
	m.f.record("save %s", access)
	m.state = st
	m.saved = append(m.saved, st)
	return nil
}

func (m *memStore) current() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func indexOf(events []string, prefix string) int {
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}
