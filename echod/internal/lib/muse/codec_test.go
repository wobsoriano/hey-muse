package muse

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"testing"
)

func TestIdentityNames(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !id.Valid() {
		t.Fatalf("NewIdentity made %q", id.MAC)
	}
	// Unicast and locally administered, as the SDK's generate_mac makes them.
	if first, err := strconv.ParseUint(id.MAC[:2], 16, 8); err != nil || first&0x03 != 0x02 {
		t.Errorf("first octet of %q", id.MAC)
	}
	node, ble := id.NodeID(), id.BLEName()
	if !regexp.MustCompile(`^homelink-[0-9a-f]{6}$`).MatchString(node) || !regexp.MustCompile(`^MuseGadget[0-9A-F]{6}$`).MatchString(ble) {
		t.Errorf("names = %q, %q", node, ble)
	}

	fixed := Identity{MAC: "02:11:22:a1:b2:c3"}
	if fixed.NodeID() != "homelink-a1b2c3" || fixed.BLEName() != "MuseGadgetA1B2C3" || fixed.DeviceID() != "hatch-link:02:11:22:a1:b2:c3" {
		t.Errorf("names = %q, %q, %q", fixed.NodeID(), fixed.BLEName(), fixed.DeviceID())
	}
	for _, bad := range []string{"", "02:11:22:A1:B2:C3", "02:11:22:a1:b2", "021122a1b2c3"} {
		if (Identity{MAC: bad}).Valid() {
			t.Errorf("%q passed as an identity", bad)
		}
	}
}

func TestStateJSON(t *testing.T) {
	st := State{
		Identity: Identity{MAC: "02:11:22:a1:b2:c3"},
		Credentials: &Credentials{
			AccessToken: "a", RefreshToken: "r", TokenType: "device", Username: "u",
			APIURL: "https://old", APIURLV2: "https://new", NoiseHost: "h", AccessTokenSavedAt: 7,
		},
		SDKTokenReported: true,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"identity":{"mac":"02:11:22:a1:b2:c3"},"credentials":{"access_token":"a","refresh_token":"r","token_type":"device","username":"u","api_url":"https://old","api_url_v2":"https://new","noise_host":"h","access_token_saved_at":7},"sdk_token_reported":true}`
	if string(b) != want {
		t.Errorf("State = %s", b)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, st) {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	for _, f := range []frame{
		{stream: 1, kind: frameRequest, verb: "POST", path: "/link-control"},
		{stream: 2, kind: frameRequest, verb: "POST", path: "/chat/stream", headers: []header{{"x-app-id", "musegadget"}, {"empty", ""}}, data: []byte("{}"), endBody: true},
		{stream: 3, kind: frameResponse, status: 200, headers: []header{{"a", "b"}}, data: []byte("ok"), endBody: true},
		{stream: 3, kind: frameResponse, status: 403},
		{stream: 1 << 40, kind: frameBody, data: []byte{0, 1, 2}},
		{stream: 4, kind: frameBody, endBody: true},
		{stream: 5, kind: frameReset, code: resetCancelled, reason: "cancelled"},
		{stream: 6, kind: frameReset},
	} {
		got, err := decodeFrame(encodeFrame(f))
		if err != nil || !reflect.DeepEqual(got, f) {
			t.Errorf("%+v came back as %+v, %v", f, got, err)
		}
	}
}

// The bytes the SDK's encode_service_request and encode_response_envelope produce for these
// frames, worked out from its field numbers.
func TestEnvelopeBytes(t *testing.T) {
	request := encodeServiceRequest(frame{stream: 1, kind: frameRequest, verb: "POST", path: "/x", endBody: true})
	want := []byte{
		0x12, 0x10, // ServiceRequest.payload
		0x08, 0x01, // ServiceFrame.stream_id
		0x12, 0x0c, // ServiceFrame.request
		0x0a, 0x04, 'P', 'O', 'S', 'T',
		0x12, 0x02, '/', 'x',
		0x28, 0x01, // end_body
	}
	if !bytes.Equal(request, want) {
		t.Errorf("request envelope = % x", request)
	}

	response := []byte{
		0x0a, 0x0b, // ServiceResponse.payload
		0x08, 0x07, // stream_id
		0x1a, 0x07, // ServiceFrame.response
		0x08, 0xc8, 0x01, // status 200
		0x1a, 0x02, 'h', 'i',
	}
	got, err := decodeServiceResponse(response)
	if err != nil || !reflect.DeepEqual(got, frame{stream: 7, kind: frameResponse, status: 200, data: []byte("hi")}) {
		t.Errorf("response = %+v, %v", got, err)
	}
	if _, err := decodeServiceResponse(appendBytesField(nil, 1, request[2:])); err == nil {
		t.Error("a request from the VM was accepted")
	}
	if _, err := decodeServiceResponse(nil); err == nil {
		t.Error("an empty envelope was accepted")
	}
}

func TestMalformedProtoIsRefused(t *testing.T) {
	for name, b := range map[string][]byte{
		"truncated varint":   {0x08, 0x80},
		"truncated bytes":    {0x12, 0x05, 'a'},
		"field zero":         {0x00, 0x00},
		"wrong wire type":    {0x0a, 0x01, 0x00},
		"group wire type":    {0x0b},
		"overlong varint":    {0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
		"bad utf8 in a verb": {0x12, 0x03, 0x0a, 0x01, 0xff},
	} {
		if _, err := decodeFrame(b); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestChunksReassemble(t *testing.T) {
	envelope := make([]byte, 3*maxChunkPayload+17)
	for i := range envelope {
		envelope[i] = byte(i >> 3)
	}
	pieces, err := chunk(envelope)
	if err != nil || len(pieces) != 4 {
		t.Fatalf("chunk = %d pieces, %v", len(pieces), err)
	}
	small, err := chunk([]byte("one"))
	if err != nil || len(small) != 1 {
		t.Fatalf("chunk = %d pieces, %v", len(small), err)
	}

	// Another message's chunk between them, and the pieces out of order.
	var a assembler
	for _, i := range []int{2, 0} {
		if out, err := a.add(pieces[i]); out != nil || err != nil {
			t.Fatalf("piece %d: %v, %v", i, out, err)
		}
	}
	if out, err := a.add(small[0]); err != nil || string(out) != "one" {
		t.Fatalf("the whole message between = %q, %v", out, err)
	}
	if out, err := a.add(pieces[3]); out != nil || err != nil {
		t.Fatal(err)
	}
	out, err := a.add(pieces[1])
	if err != nil || !bytes.Equal(out, envelope) {
		t.Fatalf("reassembled %d bytes, %v", len(out), err)
	}

	// A repeated piece is malformed, and nothing after it is trusted.
	a = assembler{}
	_, _ = a.add(pieces[0])
	if _, err := a.add(pieces[0]); err == nil {
		t.Error("a repeated piece was accepted")
	}
	if _, err := a.add(small[0]); err == nil {
		t.Error("the assembler carried on after a malformed piece")
	}
	if _, err := chunk(make([]byte, maxTotalChunks*maxChunkPayload+1)); err == nil {
		t.Error("an oversized message was chunked")
	}
}

func TestControlMessagesSpanChunks(t *testing.T) {
	first, _ := encodeControl(map[string]any{"method": "link.invoke", "id": "a", "command": "echo"})
	second, _ := encodeControl(map[string]any{"type": "evt", "event": "link.unpaired"})
	stream := append(append(append(first, 0, 0, 0, 0), second...), 5, 0, 0, 0, 'n', 'o', 'p', 'e', '!')

	var d controlDecoder
	var got []controlMessage
	for i := 0; i < len(stream); i += 7 {
		messages, err := d.feed(stream[i:min(len(stream), i+7)])
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, messages...)
	}
	// The empty message is a keepalive and the one that is not JSON is dropped.
	if len(got) != 2 || rawString(got[0].Command) != "echo" || rawString(got[1].Event) != "link.unpaired" {
		t.Errorf("messages = %+v", got)
	}
	if _, err := (&controlDecoder{}).feed([]byte{0xff, 0xff, 0xff, 0x7f}); err == nil {
		t.Error("an oversized message was accepted")
	}
}

func TestNoiseURL(t *testing.T) {
	for _, tc := range []struct {
		host, vm  string
		plaintext bool
		want      string
	}{
		{"hatch.metaaivm.com", "vm 1", false, "wss://hatch.metaaivm.com/v1/noise?vm_id=vm%201"},
		{"wss://host:8443", "a/b+c&d=é", false, "wss://host:8443/v1/noise?vm_id=a%2Fb%2Bc%26d%3D%C3%A9"},
		{"host", "it's~(ok)!*._-", false, "wss://host/v1/noise?vm_id=it's~(ok)!*._-"},
		{"ws://127.0.0.1:9", "v", true, "ws://127.0.0.1:9/v1/noise?vm_id=v"},
		{"ws://127.0.0.1:9", "v", false, ""},
		{"host/elsewhere", "v", false, ""},
		{"user@host", "v", false, ""},
		{"", "v", false, ""},
	} {
		got, err := noiseURL(tc.host, tc.vm, tc.plaintext)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("noiseURL(%q, %q) = %q, %v", tc.host, tc.vm, got, err)
		}
	}
}

func TestAPIRootIsHTTPS(t *testing.T) {
	for _, tc := range []struct {
		v2, want string
	}{
		{"", "https://api.muse.ai"},
		{"https://api.example/", "https://api.example"},
		{"http://api.example", ""},
		{"api.example", ""},
	} {
		// api_url is never consulted, whatever it holds.
		got, err := apiRoot(&Credentials{APIURL: "https://old.example/hatch", APIURLV2: tc.v2}, false)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("apiRoot(%q) = %q, %v", tc.v2, got, err)
		}
	}
}

func event(name string, payload map[string]any) chatEvent {
	b, _ := json.Marshal(map[string]any{"type": "event", "event": name, "payload": payload})
	var e chatEvent
	_ = json.Unmarshal(b, &e)
	return e
}

// The matching rules of _Turn.reply in the MicroPython port.
func TestTurnMatching(t *testing.T) {
	tn := newTurn()
	// Before the acknowledgment: someone else's message, then our echo, then the reply starts.
	tn.add(event("delta.message_start", map[string]any{"message_id": "other", "reply_to_message_id": ""}))
	tn.add(event("delta.text_append", map[string]any{"message_id": "other", "text": "not ours"}))
	tn.add(event("message.user", map[string]any{"message_id": "someone-else", "display_text": "not ours either"}))
	tn.add(event("message.user", map[string]any{"message_id": "u1", "display_text": "half", "display_text_ready": false}))
	tn.add(event("message.user", map[string]any{"message_id": "u1", "display_text": "what I said"}))
	tn.add(event("delta.message_start", map[string]any{"message_id": "r1", "reply_to_message_id": ""}))
	tn.add(event("delta.text_append", map[string]any{"message_id": "r1", "parent_message_id": "r1", "text": "Hel"}))
	if tn.text() != "" || tn.whole() {
		t.Fatalf("matched before the acknowledgment: %q", tn.text())
	}
	tn.acknowledged("u1")
	if tn.text() != "Hel" || tn.heard != "what I said" || tn.whole() {
		t.Fatalf("after the acknowledgment: %q, heard %q", tn.text(), tn.heard)
	}

	tn.add(event("agent.status", map[string]any{"activity_code": "working"}))
	tn.add(event("delta.text_append", map[string]any{"message_id": "r1", "parent_message_id": "r1", "text": "lo"}))
	tn.add(event("delta.text_append", map[string]any{"message_id": "never-started", "text": "stray"}))
	tn.add(event("delta.message_done", map[string]any{"message_id": "r1", "status": "completed"}))
	if tn.text() != "Hello" || tn.whole() {
		t.Fatalf("done but Muse still working: %q, whole %v", tn.text(), tn.whole())
	}
	// A second message, linked by naming the first, extends the answer.
	tn.add(event("delta.message_start", map[string]any{"message_id": "r2", "reply_to_message_id": "r1"}))
	tn.add(event("agent.status", map[string]any{"activity_code": "idle"}))
	if tn.whole() {
		t.Fatal("whole with a message still open")
	}
	// A stored message replaces the streamed text, and is done unless it says otherwise.
	tn.add(event("message.assistant", map[string]any{"message_id": "r2", "display_text": "draft", "display_text_ready": false}))
	if tn.whole() {
		t.Fatal("whole on a draft")
	}
	tn.add(event("message.assistant", map[string]any{"message_id": "r2", "content": "And more."}))
	// One that names a parent this turn does not know is someone else's.
	tn.add(event("delta.message_start", map[string]any{"message_id": "r3", "reply_to_message_id": "elsewhere"}))
	if tn.text() != "Hello\n\nAnd more." || !tn.whole() {
		t.Fatalf("final: %q, whole %v", tn.text(), tn.whole())
	}

	tn.add(event("task.status", map[string]any{"status": "running"}))
	if tn.whole() {
		t.Fatal("whole while a task runs")
	}
	tn.add(event("task.status", map[string]any{"status": "completed"}))
	if !tn.whole() {
		t.Fatal("not whole after the task completed")
	}
}

// When the echo comes after the acknowledgment, any new parentless message follows ours.
func TestTurnMatchingAfterTheAcknowledgment(t *testing.T) {
	tn := newTurn()
	tn.add(event("delta.message_start", map[string]any{"message_id": "before", "reply_to_message_id": ""}))
	tn.acknowledged("u1")
	tn.add(event("delta.message_start", map[string]any{"message_id": "r1", "reply_to_message_id": ""}))
	tn.add(event("delta.text_append", map[string]any{"message_id": "before", "text": "no"}))
	tn.add(event("delta.text_append", map[string]any{"message_id": "r1", "text": "yes"}))
	tn.add(event("delta.message_done", map[string]any{"message_id": "r1"}))
	if tn.text() != "yes" || !tn.whole() {
		t.Fatalf("%q, whole %v", tn.text(), tn.whole())
	}
}

func TestTranscriptLeavesOutTheRecordingLine(t *testing.T) {
	got := transcript("What is 2 plus 2?\n[file:audio/wav workspace/user/files/voice_note-64.wav]")
	if got != "What is 2 plus 2?" {
		t.Errorf("transcript = %q", got)
	}
	if got := transcript("[file:audio/wav x.wav]"); got != "" {
		t.Errorf("a recording with no words gave %q", got)
	}
}

type notATransport struct{ http.RoundTripper }

func TestNewDoesNotLeanOnTheDefaultTransport(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = notATransport{old}
	defer func() { http.DefaultTransport = old }()
	if _, err := New(Config{Store: &memStore{}}); err != nil {
		t.Fatal(err)
	}
}
