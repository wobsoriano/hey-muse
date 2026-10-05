// Ported from Meta's Muse Gadget SDK (linux/src/musegadget/link_client.py), Apache-2.0.
// Modified: rewritten in Go, with the chat subscription and reply waiting of the MicroPython port
// and the voice note upload and early "settled" signal of the C port.

package muse

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/gorilla/websocket"
)

const (
	noisePath     = "/v1/noise"
	controlPath   = "/link-control"
	chatPath      = "/chat/stream"
	subscribePath = "/chat/subscribe"
	appID         = "musegadget"

	maxConcurrentInvokes = 4
	maxInboundMessage    = 4 << 20
	maxResponseBytes     = 1 << 20
	maxEventLine         = 256 << 10
	// maxDeviceMessage is the most the VM takes in one control message.
	maxDeviceMessage = 256 << 10
	// bodyChunk is the size the reference firmware sends a voice note's body in.
	bodyChunk    = 16 << 10
	maxAskText   = 16 << 10
	maxVoiceNote = 600 << 10
	// maxWSMessage is one Noise message, which is 64 KB at most, with room to spare.
	maxWSMessage = 128 << 10

	defaultInvokeTimeout = 30 * time.Second
)

// timing is every wait in the client, gathered so the tests can shrink them.
type timing struct {
	handshake  time.Duration
	write      time.Duration
	ping       time.Duration
	rxTimeout  time.Duration // silence, pongs included, after which the connection is taken for dead
	request    time.Duration
	firstReply time.Duration
	turnCap    time.Duration
	settle     time.Duration // pause after a complete answer before Settled
	final      time.Duration // pause after a complete answer before Ask returns

	backoffBase time.Duration
	backoffMax  time.Duration
	authFloor   time.Duration // least wait after the VM refuses the bearer
	healthy     time.Duration // a session registered this long clears the backoff
	refreshAge  time.Duration
	tokenRetry  time.Duration
}

var defaultTiming = timing{
	handshake:  20 * time.Second,
	write:      10 * time.Second,
	ping:       15 * time.Second,
	rxTimeout:  45 * time.Second,
	request:    60 * time.Second,
	firstReply: 5 * time.Minute,
	turnCap:    15 * time.Minute,
	settle:     600 * time.Millisecond,
	final:      3 * time.Second,

	backoffBase: 2 * time.Second,
	backoffMax:  60 * time.Second,
	authFloor:   15 * time.Second,
	healthy:     30 * time.Second,
	// Device access tokens live about four hours.
	refreshAge: 3 * time.Hour,
	tokenRetry: 5 * time.Minute,
}

// outcome is how a session ended, which decides what the service loop does next.
type outcome int

const (
	outcomeClosed       outcome = iota // the connection ended: reconnect
	outcomeAuthRejected                // the edge refused the VM bearer: fetch a new one
	outcomeForbidden                   // authenticated but not allowed right now
	outcomeUnpaired                    // Muse removed this device
	outcomeStopped                     // the caller's context ended
)

// noiseURL is the session's address. noiseHost may carry a wss:// scheme, and ws:// in tests.
func noiseURL(noiseHost, vmID string, allowPlaintext bool) (string, error) {
	scheme, host := "wss", noiseHost
	if rest, ok := strings.CutPrefix(noiseHost, "wss://"); ok {
		host = rest
	} else if rest, ok := strings.CutPrefix(noiseHost, "ws://"); ok && allowPlaintext {
		scheme, host = "ws", rest
	}
	// Noise XX does not say who the VM is. TLS does, so the host has to be one TLS can check.
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return "", errors.New("muse: the Noise host in the credentials is not a host")
	}
	return scheme + "://" + host + noisePath + "?vm_id=" + encodeURIComponent(vmID), nil
}

// encodeURIComponent escapes as JavaScript's function of that name does, which is what the
// reference firmware uses and so what the edge is known to accept.
func encodeURIComponent(s string) string {
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(safe, s[i]) >= 0 {
			b.WriteByte(s[i])
		} else {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
	}
	return b.String()
}

func uuid4() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0F | 0x40
	u[8] = u[8]&0x3F | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// encodeControl frames one control message: its length as a little-endian u32, then the JSON.
func encodeControl(message any) ([]byte, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return append(binary.LittleEndian.AppendUint32(make([]byte, 0, 4+len(data)), uint32(len(data))), data...), nil
}

// controlMessage is anything the VM says on the control stream. Every field is kept raw: the id
// is echoed back exactly as it came, and a field of an unexpected type must not cost the message.
type controlMessage struct {
	ID        json.RawMessage `json:"id"`
	Method    json.RawMessage `json:"method"`
	Event     json.RawMessage `json:"event"`
	Command   json.RawMessage `json:"command"`
	Params    json.RawMessage `json:"params"`
	TimeoutMS json.RawMessage `json:"timeout_ms"`
	Error     json.RawMessage `json:"error"`
}

// controlDecoder splits the control stream back into messages. One may span body chunks.
type controlDecoder struct{ buf []byte }

func (d *controlDecoder) feed(data []byte) ([]controlMessage, error) {
	d.buf = append(d.buf, data...)
	var out []controlMessage
	for len(d.buf) >= 4 {
		n := int(binary.LittleEndian.Uint32(d.buf))
		if n > maxInboundMessage {
			return nil, errors.New("muse: control message too large")
		}
		if len(d.buf) < 4+n {
			break
		}
		raw := d.buf[4 : 4+n]
		d.buf = d.buf[4+n:]
		var m controlMessage
		// An empty message is a keepalive. One that is not a JSON object is dropped, as the SDK does.
		if n > 0 && json.Unmarshal(raw, &m) == nil {
			out = append(out, m)
		}
	}
	// What is left moves to a buffer of its own, so the consumed bytes can be freed.
	d.buf = append([]byte(nil), d.buf...)
	return out, nil
}

// sink takes the frames of one stream. It is called with session.mu held.
type sink interface{ onFrame(f frame) }

// request collects the answer to one request stream.
type request struct {
	status int
	body   []byte
	err    error
	over   bool
	done   chan struct{}
}

func (r *request) finish(err error) {
	r.err, r.over = err, true
	close(r.done)
}

func (r *request) onFrame(f frame) {
	if r.over {
		return
	}
	if f.kind == frameReset {
		r.finish(fmt.Errorf("muse: stream reset: %s", f.reason))
		return
	}
	if f.kind == frameResponse {
		r.status = f.status
	}
	r.body = append(r.body, f.data...)
	if len(r.body) > maxResponseBytes {
		r.finish(errors.New("muse: response too large"))
	} else if f.endBody {
		r.finish(nil)
	}
}

// subscription is the chat stream: a response that never ends, one JSON event to a line.
type subscription struct {
	s      *session
	id     int64
	buf    []byte
	closed bool
	reason string
}

func (sub *subscription) close(reason string) {
	sub.closed, sub.reason = true, reason
	if sub.s.turn != nil {
		sub.s.turn.poke()
	}
}

func (sub *subscription) onFrame(f frame) {
	if sub.closed {
		return
	}
	if f.kind == frameReset {
		sub.close(f.reason)
		return
	}
	if f.kind == frameResponse && f.status >= 400 {
		sub.close(fmt.Sprintf("HTTP %d", f.status))
		return
	}
	sub.buf = append(sub.buf, f.data...)
	for {
		end := bytes.IndexByte(sub.buf, '\n')
		if end < 0 {
			break
		}
		var e chatEvent
		// Lines of any other type are the acknowledgment of the subscription itself.
		if json.Unmarshal(sub.buf[:end], &e) == nil && e.Type == "event" && sub.s.turn != nil {
			sub.s.turn.add(e)
		}
		sub.buf = sub.buf[end+1:]
	}
	sub.buf = append([]byte(nil), sub.buf...)
	if len(sub.buf) > maxEventLine {
		sub.close("event line too long")
	} else if f.endBody {
		sub.close("ended by Muse")
	}
}

type sessionParams struct {
	url        string
	bearer     string
	userAgent  string
	tls        *tls.Config
	nodeID     string
	register   map[string]any
	commands   map[string]Command
	log        *slog.Logger
	t          timing
	registered func() // called once Muse accepts the registration
}

// session is one connection to a VM, from the WebSocket upgrade until it drops.
type session struct {
	p  sessionParams
	ws *websocket.Conn

	// sendMu orders everything written: Noise numbers each message, so sealing one and writing it
	// must not interleave with another, and stream ids must reach the VM in the order they were
	// taken.
	sendMu     sync.Mutex
	sendCipher *noise.CipherState
	nextStream int64

	// Touched only by the reader.
	recvCipher *noise.CipherState
	chunks     assembler
	control    int64
	registerID string

	mu           sync.Mutex
	streams      map[int64]sink
	sub          *subscription
	turn         *turn
	registeredAt time.Time

	// ended closes when the session is over, which frees everything waiting on it.
	ended chan struct{}
	// invoking ends with the session and takes the running commands with it.
	invoking context.Context
	slots    chan struct{}
	wg       sync.WaitGroup
}

func newSession(p sessionParams) *session {
	return &session{
		p:       p,
		streams: map[int64]sink{},
		ended:   make(chan struct{}),
		slots:   make(chan struct{}, maxConcurrentInvokes),
	}
}

// run serves until the connection ends or ctx does. The error says why, for a person.
func (s *session) run(ctx context.Context) (outcome, error) {
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		TLSClientConfig:  s.p.tls,
		HandshakeTimeout: s.p.t.handshake,
	}
	ws, resp, err := dialer.DialContext(ctx, s.p.url, http.Header{
		"Authorization": {"Bearer " + s.p.bearer},
		"User-Agent":    {s.p.userAgent},
	})
	if err != nil {
		defer close(s.ended)
		switch {
		case ctx.Err() != nil:
			return outcomeStopped, ctx.Err()
		case resp != nil && resp.StatusCode == http.StatusUnauthorized:
			return outcomeAuthRejected, errors.New("muse: the VM refused the connection: HTTP 401")
		case resp != nil && resp.StatusCode == http.StatusForbidden:
			return outcomeForbidden, errors.New("muse: the VM refused the connection: HTTP 403")
		}
		return outcomeClosed, fmt.Errorf("muse: connect: %w", err)
	}
	s.ws = ws
	ws.SetReadLimit(maxWSMessage)

	invoking, cancel := context.WithCancel(context.Background())
	s.invoking = invoking
	// A read on a dead connection never returns by itself, so ending the context closes the socket
	// under the reader.
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer func() {
		stop()
		close(s.ended)
		cancel()
		_ = ws.Close()
		s.wg.Wait()
	}()

	out, err := s.serve()
	if ctx.Err() != nil {
		return outcomeStopped, ctx.Err()
	}
	return out, err
}

func (s *session) serve() (outcome, error) {
	_ = s.ws.SetReadDeadline(time.Now().Add(s.p.t.handshake))
	if err := s.handshake(); err != nil {
		return outcomeClosed, fmt.Errorf("muse: Noise handshake: %w", err)
	}
	s.p.log.Info("muse: Noise session established")

	id, err := s.open(frame{kind: frameRequest, verb: http.MethodPost, path: controlPath}, nil)
	if err != nil {
		return outcomeClosed, err
	}
	s.control = id
	s.registerID = uuid4()
	if err := s.sendControl(map[string]any{
		"type":   "req",
		"id":     s.registerID,
		"method": "link.register",
		"params": s.p.register,
	}); err != nil {
		return outcomeClosed, err
	}
	s.p.log.Info("muse: sent link.register", "node", s.p.nodeID)

	rx := func() { _ = s.ws.SetReadDeadline(time.Now().Add(s.p.t.rxTimeout)) }
	s.ws.SetPongHandler(func(string) error { rx(); return nil })
	rx()
	s.wg.Go(s.keepalive)

	var control controlDecoder
	for {
		kind, data, err := s.ws.ReadMessage()
		if err != nil {
			return outcomeClosed, fmt.Errorf("muse: connection closed: %w", err)
		}
		rx()
		if kind != websocket.BinaryMessage {
			continue
		}
		plain, err := s.recvCipher.Decrypt(nil, nil, data)
		if err != nil {
			return outcomeClosed, errors.New("muse: a message from the VM did not decrypt")
		}
		envelope, err := s.chunks.add(plain)
		if err != nil {
			return outcomeClosed, err
		}
		if envelope == nil {
			continue
		}
		f, err := decodeServiceResponse(envelope)
		if err != nil {
			return outcomeClosed, err
		}
		if f.stream != s.control {
			s.mu.Lock()
			if to := s.streams[f.stream]; to != nil {
				to.onFrame(f)
			}
			s.mu.Unlock()
			continue
		}
		if f.kind == frameReset {
			return outcomeClosed, fmt.Errorf("muse: control stream reset: %s", f.reason)
		}
		if f.kind == frameResponse && f.status >= 400 {
			err := fmt.Errorf("muse: the VM refused the control stream: HTTP %d", f.status)
			if f.status == http.StatusForbidden {
				return outcomeForbidden, err
			}
			return outcomeClosed, err
		}
		messages, err := control.feed(f.data)
		if err != nil {
			return outcomeClosed, err
		}
		for _, m := range messages {
			if over, out, err := s.handle(m); over {
				return out, err
			}
		}
		if f.endBody {
			return outcomeClosed, errors.New("muse: control stream ended by the VM")
		}
	}
}

// handshake is Noise XX as the initiator. The bearer already said who the device is at the
// upgrade, so the static key is made fresh each time and message 3 carries no payload.
func (s *session) handshake() error {
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)
	static, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return err
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: static,
	})
	if err != nil {
		return err
	}
	write := func(msg []byte) error {
		_ = s.ws.SetWriteDeadline(time.Now().Add(s.p.t.write))
		return s.ws.WriteMessage(websocket.BinaryMessage, msg)
	}
	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return err
	}
	if err := write(msg1); err != nil {
		return err
	}
	kind, msg2, err := s.ws.ReadMessage()
	if err != nil {
		return err
	}
	if kind != websocket.BinaryMessage {
		return errors.New("got a text frame")
	}
	if _, _, _, err := hs.ReadMessage(nil, msg2); err != nil {
		return err
	}
	msg3, send, recv, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return err
	}
	s.sendCipher, s.recvCipher = send, recv
	return write(msg3)
}

// keepalive pings so that a dead connection is noticed: the pongs push the reader's deadline out,
// and without them it runs out.
func (s *session) keepalive() {
	tick := time.NewTicker(s.p.t.ping)
	defer tick.Stop()
	for {
		select {
		case <-s.ended:
			return
		case <-tick.C:
			if err := s.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(s.p.t.write)); err != nil {
				_ = s.ws.Close()
				return
			}
		}
	}
}

// open starts a stream with a request frame, and returns the stream's id. to hears the answer,
// and is in place before the request leaves so that no frame of the answer can beat it.
func (s *session) open(f frame, to sink) (int64, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.nextStream++
	f.stream = s.nextStream
	if to != nil {
		s.mu.Lock()
		s.streams[f.stream] = to
		s.mu.Unlock()
	}
	return f.stream, s.writeLocked(f)
}

func (s *session) write(f frame) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.writeLocked(f)
}

func (s *session) writeLocked(f frame) error {
	pieces, err := chunk(encodeServiceRequest(f))
	if err != nil {
		return err
	}
	for _, piece := range pieces {
		sealed, err := s.sendCipher.Encrypt(nil, nil, piece)
		if err == nil {
			_ = s.ws.SetWriteDeadline(time.Now().Add(s.p.t.write))
			err = s.ws.WriteMessage(websocket.BinaryMessage, sealed)
		}
		if err != nil {
			// The far side can no longer follow the numbering, so the session is over.
			_ = s.ws.Close()
			return fmt.Errorf("muse: send: %w", err)
		}
	}
	return nil
}

func (s *session) forget(stream int64) {
	s.mu.Lock()
	delete(s.streams, stream)
	s.mu.Unlock()
}

func (s *session) sendControl(message any) error {
	data, err := encodeControl(message)
	if err != nil {
		return err
	}
	return s.write(frame{stream: s.control, kind: frameBody, data: data})
}

// handle acts on one control message. over is true when the message ends the session.
func (s *session) handle(m controlMessage) (over bool, out outcome, err error) {
	if rawString(m.ID) == s.registerID && !truthyJSON(m.Method) {
		if truthyJSON(m.Error) {
			// The SDK logs this and stays connected, registered for nothing. Ending the session
			// instead lets the service loop show the failure and try again.
			return true, outcomeClosed, errors.New("muse: Muse rejected the registration")
		}
		s.mu.Lock()
		s.registeredAt = time.Now()
		s.mu.Unlock()
		s.p.log.Info("muse: registered with Muse")
		s.p.registered()
		return false, 0, nil
	}
	switch rawString(m.Event) {
	case "link.unpaired", "node.unpaired":
		return true, outcomeUnpaired, ErrUnpaired
	}
	if rawString(m.Method) == "link.invoke" && truthyJSON(m.ID) {
		s.wg.Go(func() { s.invoke(m) })
	}
	return false, 0, nil
}

// invokeResult is link.result: the invoke's id, and either a payload or an error.
type invokeResult struct {
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (s *session) invoke(m controlMessage) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-s.invoking.Done():
		return
	}
	name := rawString(m.Command)
	s.p.log.Info("muse: invoke", "command", name)
	result := invokeResult{Method: "link.result", ID: m.ID}
	payload, err := s.runCommand(name, m)
	if err == nil && len(payload) > maxDeviceMessage-1024 {
		err = fmt.Errorf("the result is %d bytes, more than Muse takes", len(payload))
	}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.OK, result.Payload = true, payload
	}
	if err := s.sendControl(result); err != nil {
		s.p.log.Warn("muse: could not send a command's result", "command", name, "err", err)
	}
}

func (s *session) runCommand(name string, m controlMessage) (payload json.RawMessage, err error) {
	command, ok := s.p.commands[name]
	if !ok {
		return nil, fmt.Errorf("unsupported command: %s", name)
	}
	args := m.Params
	if len(args) == 0 || args[0] != '{' {
		args = json.RawMessage("{}")
	}
	timeout := command.Timeout
	var ms float64
	if json.Unmarshal(m.TimeoutMS, &ms) == nil && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = defaultInvokeTimeout
	}
	ctx, cancel := context.WithTimeout(s.invoking, timeout)
	defer cancel()
	// A handler that panics has failed one command. It must not take the session, and every
	// other command in flight, down with it.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	value, err := command.Handler(ctx, args)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return json.RawMessage("{}"), nil
	}
	return json.Marshal(value)
}

// chatBody is what POST /chat/stream takes. A voice note leaves device_id out, as the firmware
// whose upload this copies does.
type chatBody struct {
	Message        string     `json:"message"`
	OutputModality string     `json:"output_modality"`
	DeviceID       string     `json:"device_id,omitempty"`
	Items          []chatItem `json:"items,omitempty"`
}

type chatItem struct {
	Type       string `json:"type"`
	MIMEType   string `json:"mime_type"`
	Filename   string `json:"filename"`
	DataBase64 string `json:"data_base64"`
}

func (s *session) jsonHeaders(extra ...header) []header {
	return append([]header{
		{"Content-Type", "application/json"},
		{"x-request-id", uuid4()},
		{"x-app-id", appID},
	}, extra...)
}

// subscribe opens the stream Muse's chat events arrive on. It is opened once and lasts the
// session, and is only opened again if it closed.
func (s *session) subscribe() error {
	s.mu.Lock()
	old := s.sub
	if old != nil && !old.closed {
		s.mu.Unlock()
		return nil
	}
	if old != nil {
		delete(s.streams, old.id)
	}
	s.mu.Unlock()

	sub := &subscription{s: s}
	id, err := s.open(frame{
		kind:    frameRequest,
		verb:    http.MethodPost,
		path:    subscribePath,
		headers: s.jsonHeaders(header{"accept", "application/x-ndjson"}),
		data:    []byte("{}"),
		endBody: true,
	}, sub)
	if err != nil {
		return err
	}
	s.mu.Lock()
	sub.id = id
	s.sub = sub
	s.mu.Unlock()
	return nil
}

// sendChat posts the message. The answer to the post is only an acknowledgment naming the message.
func (s *session) sendChat(ctx context.Context, in AskInput, ack *request) (int64, error) {
	open := frame{kind: frameRequest, verb: http.MethodPost, path: chatPath, headers: s.jsonHeaders()}
	if in.kind == askText {
		body, err := json.Marshal(chatBody{Message: in.text, OutputModality: "text", DeviceID: s.p.nodeID})
		if err != nil {
			return 0, err
		}
		open.data, open.endBody = body, true
		return s.open(open, ack)
	}

	// The recording rides as a base64 attachment, the way Meta's voice gadgets send it, with the
	// body in pieces after a request that carries none.
	body, err := json.Marshal(chatBody{OutputModality: "text", Items: []chatItem{{
		Type:       "file",
		MIMEType:   "audio/wav",
		Filename:   "voice_note.wav",
		DataBase64: base64.StdEncoding.EncodeToString(in.wav),
	}}})
	if err != nil {
		return 0, err
	}
	id, err := s.open(open, ack)
	for start := 0; err == nil && start < len(body); start += bodyChunk {
		if ctx.Err() != nil {
			// Half a recording is no use to Muse, so the stream is withdrawn.
			_ = s.write(frame{stream: id, kind: frameReset, code: resetCancelled, reason: "cancelled"})
			return id, ctx.Err()
		}
		end := min(len(body), start+bodyChunk)
		err = s.write(frame{stream: id, kind: frameBody, data: body[start:end], endBody: end == len(body)})
	}
	return id, err
}

// snapshot is the turn as the asker last saw it.
type snapshot struct {
	text, heard string
	whole       bool
	quiet       time.Duration
	subClosed   string
	subOpen     bool
}

func (s *session) snapshot(t *turn) snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := snapshot{text: t.text(), heard: t.heard, whole: t.whole(), quiet: time.Since(t.lastEvent)}
	if s.sub != nil {
		snap.subOpen, snap.subClosed = !s.sub.closed, s.sub.reason
	}
	return snap
}

// ask sends one message and waits out the answer. on runs on the caller's goroutine, between the
// waits of this function, so nothing can reach it once ask has returned.
func (s *session) ask(ctx context.Context, in AskInput, on func(ReplyEvent)) (Reply, error) {
	t := newTurn()
	s.mu.Lock()
	switch {
	case s.registeredAt.IsZero():
		s.mu.Unlock()
		return Reply{}, ErrOffline
	case s.turn != nil:
		s.mu.Unlock()
		return Reply{}, ErrBusy
	}
	s.turn = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
	}()

	dropped := func(err error) (Reply, error) {
		select {
		case <-s.ended:
			return Reply{}, fmt.Errorf("%w: the connection dropped before the answer was complete", ErrOffline)
		default:
			return Reply{}, err
		}
	}

	if err := s.subscribe(); err != nil {
		return dropped(err)
	}
	ack := &request{done: make(chan struct{})}
	stream, err := s.sendChat(ctx, in, ack)
	defer s.forget(stream)
	if err != nil {
		return dropped(err)
	}
	timeout := time.NewTimer(s.p.t.request)
	defer timeout.Stop()
	select {
	case <-ack.done:
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	case <-s.ended:
		return dropped(nil)
	case <-timeout.C:
		return Reply{}, errors.New("muse: Muse did not acknowledge the message")
	}
	if ack.err != nil {
		return Reply{}, ack.err
	}
	if ack.status < 200 || ack.status >= 300 {
		return Reply{}, fmt.Errorf("muse: Muse did not take the message: HTTP %d", ack.status)
	}
	var acked struct {
		MessageID string `json:"message_id"`
		Result    *struct {
			MessageID string `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(ack.body, &acked)
	messageID := acked.MessageID
	if acked.Result != nil {
		messageID = acked.Result.MessageID
	}
	if messageID == "" {
		// Without it no reply can ever be matched, and the references would wait five minutes
		// to say so.
		return Reply{}, errors.New("muse: Muse acknowledged the message without naming it")
	}
	s.mu.Lock()
	t.acknowledged(messageID)
	s.mu.Unlock()

	started := time.Now()
	var shown, heard, settled string
	wait := time.NewTimer(0)
	defer wait.Stop()
	for {
		snap := s.snapshot(t)
		if err := ctx.Err(); err != nil {
			return Reply{}, err
		}
		if in.kind == askVoice && snap.heard != heard {
			heard = snap.heard
			on(Heard{Text: heard})
		}
		if snap.text != shown {
			shown = snap.text
			on(ReplyText{Text: shown})
		}
		elapsed := time.Since(started)
		next := 250 * time.Millisecond
		switch {
		case snap.whole && snap.quiet >= s.p.t.final:
			return Reply{Text: shown, Heard: heard}, nil
		case snap.whole && snap.quiet >= s.p.t.settle && settled != shown:
			settled = shown
			on(Settled{Text: shown})
			continue
		case snap.whole && settled != shown:
			next = s.p.t.settle - snap.quiet
		case snap.whole:
			next = s.p.t.final - snap.quiet
		case !snap.subOpen:
			return dropped(fmt.Errorf("muse: chat stream closed: %s", snap.subClosed))
		case shown == "" && elapsed > s.p.t.firstReply:
			return Reply{}, errors.New("muse: Muse did not reply")
		case elapsed > s.p.t.turnCap:
			if shown == "" {
				return Reply{}, errors.New("muse: Muse did not finish replying")
			}
			return Reply{Text: shown, Heard: heard}, nil
		}
		wait.Reset(next)
		select {
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		case <-s.ended:
			return dropped(nil)
		case <-t.wake:
		case <-wait.C:
		}
	}
}
