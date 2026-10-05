package pairing_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

const wait = 3 * time.Second

var b64 = base64.RawURLEncoding

// phone is the Muse app's side, written from the protocol with the standard library alone. It shares
// no code with the package, so the two agreeing means something.
type phone struct {
	t         *testing.T
	key       *ecdh.PrivateKey
	nonce     []byte
	tx, rx    cipher.AEAD
	sessionID string
	txCounter uint64
}

func newPhone(t *testing.T) *phone {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	return &phone{t: t, key: key, nonce: nonce}
}

func (p *phone) hello() map[string]any {
	return map[string]any{
		"action": "pairing_client_hello", "version": 5, "pairing_auth": "none", "pairing_policy": "confirm_app",
		"mobile_pub": b64.EncodeToString(p.key.PublicKey().Bytes()), "mobile_nonce": b64.EncodeToString(p.nonce),
	}
}

func (p *phone) accept(ready map[string]any) {
	p.t.Helper()
	text := func(key string) string { s, _ := ready[key].(string); return s }
	if ready["type"] != "pairing_ready" {
		p.t.Fatalf("expected pairing_ready, got %v", ready)
	}
	transcript := strings.Join([]string{
		"hatch-link-pairing-v5", "version=5", "initiator_role=mobile", "responder_role=link",
		"device_id=" + text("device_id"), "node_id=" + text("node_id"), "mac=" + text("mac"),
		"model=hatch_link", "firmware_version=" + text("firmware_version"),
		"selected_cipher_suite=p256-hkdf-sha256-aes-gcm-v1", "pairing_auth=none",
		"pairing_auth_epoch=0", "pairing_policy=confirm_app", "confirm_timeout_seconds=0",
		"mobile_pub=" + b64.EncodeToString(p.key.PublicKey().Bytes()), "device_pub=" + text("device_pub"),
		"mobile_nonce=" + b64.EncodeToString(p.nonce), "device_nonce=" + text("device_nonce"),
	}, "\n")
	hash := sha256.Sum256([]byte(transcript))
	if got := b64.EncodeToString(hash[:]); got != text("transcript_hash") {
		p.t.Fatalf("the device hashed a different transcript: %s, phone has %s", text("transcript_hash"), got)
	}
	devicePub, _ := b64.DecodeString(text("device_pub"))
	deviceNonce, _ := b64.DecodeString(text("device_nonce"))
	peer, err := ecdh.P256().NewPublicKey(devicePub)
	if err != nil {
		p.t.Fatal(err)
	}
	secret, err := p.key.ECDH(peer)
	if err != nil {
		p.t.Fatal(err)
	}
	salt := sha256.Sum256(append(append(append([]byte{}, p.nonce...), deviceNonce...), hash[:]...))
	session, _ := hkdf.Key(sha256.New, secret, salt[:], "hatch-link ble setup v1", 32)
	gcm := func(info string) cipher.AEAD {
		key, _ := hkdf.Expand(sha256.New, session, info, 32)
		block, _ := aes.NewCipher(key)
		aead, _ := cipher.NewGCM(block)
		return aead
	}
	p.tx, p.rx = gcm("mobile->device"), gcm("device->mobile")
	p.sessionID = text("session_id")
	p.txCounter = 0
}

func (p *phone) nonceFor(direction byte, counter uint64) []byte {
	n := make([]byte, 12)
	n[0] = direction
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

func (p *phone) seal(command map[string]any) map[string]any {
	counter := p.txCounter
	p.txCounter++
	plaintext, _ := json.Marshal(command)
	aad := fmt.Sprintf("hatch-link ble setup v1|%s|m2d|%d", p.sessionID, counter)
	sealed := p.tx.Seal(nil, p.nonceFor(0, counter), plaintext, []byte(aad))
	return map[string]any{
		"action": "pairing_encrypted", "session_id": p.sessionID, "counter": strconv.FormatUint(counter, 10),
		"ciphertext": b64.EncodeToString(sealed[:len(sealed)-16]), "tag": b64.EncodeToString(sealed[len(sealed)-16:]),
	}
}

func (p *phone) open(envelope map[string]any) map[string]any {
	p.t.Helper()
	if envelope["type"] != "pairing_encrypted" || envelope["session_id"] != p.sessionID {
		p.t.Fatalf("expected a record of session %s, got %v", p.sessionID, envelope)
	}
	counter, err := strconv.ParseUint(envelope["counter"].(string), 10, 64)
	if err != nil {
		p.t.Fatal(err)
	}
	ciphertext, _ := b64.DecodeString(envelope["ciphertext"].(string))
	tag, _ := b64.DecodeString(envelope["tag"].(string))
	aad := fmt.Sprintf("hatch-link ble setup v1|%s|d2m|%d", p.sessionID, counter)
	plaintext, err := p.rx.Open(nil, p.nonceFor(1, counter), append(ciphertext, tag...), []byte(aad))
	if err != nil {
		p.t.Fatalf("record %d did not open: %v", counter, err)
	}
	var message map[string]any
	if err := json.Unmarshal(plaintext, &message); err != nil {
		p.t.Fatal(err)
	}
	return message
}

// link is the Transport: it reassembles what the device notifies and counts the hang-ups.
type link struct {
	t           *testing.T
	mtu         int
	mu          sync.Mutex
	asm         pairing.Assembler
	messages    chan []byte
	disconnects chan struct{}
}

func (l *link) MTU() int { return l.mtu }

func (l *link) Send(packets [][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range packets {
		// Bare statuses go out unframed, whatever the MTU, as in the SDK.
		if p[0] == 0xFE && len(p) > max(l.mtu-3, 20) {
			l.t.Errorf("packet of %d bytes at MTU %d", len(p), l.mtu)
		}
		if len(p) > pairing.MaxPacket {
			l.t.Errorf("packet of %d bytes, over MaxPacket", len(p))
		}
		if message, ok := l.asm.Feed(p); ok {
			l.messages <- append([]byte{}, message...)
		}
	}
	return nil
}

func (l *link) Disconnect() error {
	l.disconnects <- struct{}{}
	return nil
}

type options struct {
	mtu    int
	online *atomic.Bool
	verify func(context.Context, pairing.Result) error
	save   func(pairing.Result) error
	window time.Duration
}

type harness struct {
	t      *testing.T
	c      *pairing.Controller
	link   *link
	phone  *phone
	saved  chan pairing.Result
	done   chan error
	cancel context.CancelFunc

	mu       sync.Mutex
	progress []pairing.Progress
}

var device = pairing.Device{NodeID: "homelink-0a1b2c", BLEName: "MuseGadget0A1B2C", MAC: "02:11:22:0a:1b:2c", Version: "0.9.0"}

const sdkToken = "mgst_test"

func start(t *testing.T, o options) *harness {
	t.Helper()
	if o.mtu == 0 {
		o.mtu = 185
	}
	if o.online == nil {
		o.online = new(atomic.Bool)
		o.online.Store(true)
	}
	if o.verify == nil {
		o.verify = func(context.Context, pairing.Result) error { return nil }
	}
	h := &harness{
		t:     t,
		link:  &link{t: t, mtu: o.mtu, messages: make(chan []byte, 64), disconnects: make(chan struct{}, 8)},
		phone: newPhone(t),
		saved: make(chan pairing.Result, 4),
		done:  make(chan error, 1),
	}
	if o.save == nil {
		o.save = func(r pairing.Result) error { h.saved <- r; return nil }
	}
	c, err := pairing.New(pairing.Config{
		Device:    device,
		Transport: h.link,
		SDKToken:  sdkToken,
		Network: pairing.AlreadyOnline{
			Reachable: func(context.Context) bool { return o.online.Load() },
			SSID:      func() string { return "HomeNet" },
		},
		Verify: o.verify,
		Save:   o.save,
		Progress: func(p pairing.Progress) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.progress = append(h.progress, p)
		},
		Window: o.window,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.c = c
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(wait):
			t.Error("Run did not stop when its context ended")
		}
	})
	return h
}

// write sends one command the way the app does: chunked to the MTU, one GATT write per chunk.
func (h *harness) write(command map[string]any) {
	h.t.Helper()
	raw, _ := json.Marshal(command)
	packets, err := pairing.EncodeChunks(raw, h.link.mtu)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range packets {
		h.c.OnWrite(p)
	}
}

func (h *harness) next() []byte {
	h.t.Helper()
	select {
	case m := <-h.link.messages:
		return m
	case <-time.After(wait):
		h.t.Fatalf("the device sent nothing; progress so far %v", h.states())
		return nil
	}
}

func (h *harness) nextJSON() map[string]any {
	h.t.Helper()
	raw := h.next()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		h.t.Fatalf("expected JSON, got %q", raw)
	}
	return m
}

func (h *harness) nextOpened() map[string]any {
	h.t.Helper()
	return h.phone.open(h.nextJSON())
}

func (h *harness) nextStatus() string {
	h.t.Helper()
	m := h.nextOpened()
	if m["type"] != "status" {
		h.t.Fatalf("expected a status, got %v", m)
	}
	return m["status"].(string)
}

func (h *harness) quiet() {
	h.t.Helper()
	select {
	case m := <-h.link.messages:
		h.t.Fatalf("expected silence, the device sent %q", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func (h *harness) states() []pairing.State {
	h.mu.Lock()
	defer h.mu.Unlock()
	states := make([]pairing.State, len(h.progress))
	for i, p := range h.progress {
		states[i] = p.State
	}
	return states
}

func (h *harness) last() pairing.Progress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.progress[len(h.progress)-1]
}

func (h *harness) waitFor(want pairing.State) pairing.Progress {
	h.t.Helper()
	deadline := time.Now().Add(wait)
	for {
		if p := h.last(); p.State == want {
			return p
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("never reached %q; progress %v", want, h.states())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) handshake() {
	h.t.Helper()
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())
	h.write(h.phone.seal(map[string]any{"action": "pairing_client_finished"}))
	want := map[string]any{"type": "status", "status": "pairing_confirmed", "sdk_token": sdkToken}
	if got := h.nextOpened(); !reflect.DeepEqual(got, want) {
		h.t.Fatalf("after client finished the device sent %v", got)
	}
}

func (h *harness) expectStatuses(want ...string) {
	h.t.Helper()
	for _, w := range want {
		if got := h.nextStatus(); got != w {
			h.t.Fatalf("status %q, want %q", got, w)
		}
	}
}

func (h *harness) expectRun(want error) {
	h.t.Helper()
	select {
	case err := <-h.done:
		if !errors.Is(err, want) {
			h.t.Fatalf("Run returned %v, want %v", err, want)
		}
		h.done <- err
	case <-time.After(wait):
		h.t.Fatalf("Run did not return; progress %v", h.states())
	}
}

func provisionCommand() map[string]any {
	return map[string]any{
		"action": "provision_v2", "ssid": "HomeNet", "password": "pw",
		"access_token": "device-access", "refresh_token": "device-refresh", "token_type": "device",
		"username": "someone", "api_url": "https://legacy-api.example", "api_url_v2": "https://api.example",
		"noise_host": "noise.example",
	}
}

var wantResult = pairing.Result{
	AccessToken: "device-access", RefreshToken: "device-refresh", TokenType: "device", Username: "someone",
	APIURL: "https://legacy-api.example", APIURLV2: "https://api.example", NoiseHost: "noise.example",
}

func TestWholeConversation(t *testing.T) {
	for _, mtu := range []int{23, 185, 517} {
		t.Run(fmt.Sprintf("mtu %d", mtu), func(t *testing.T) {
			var verified atomic.Value
			h := start(t, options{mtu: mtu, verify: func(_ context.Context, r pairing.Result) error {
				verified.Store(r)
				return nil
			}})

			h.write(map[string]any{"action": "get_device_info"})
			wantInfo := map[string]any{
				"type": "device_info", "node_id": device.NodeID, "version": device.Version,
				"device_id": "hatch-link:" + device.MAC, "mac": device.MAC, "model": "hatch_link",
				"pairing_protocol": 5.0, "pairing_auth": "none", "pairing_auth_epoch": 0.0,
				"pairing_policy": "confirm_app", "build_sha": "", "network_ready": true,
			}
			if got := h.nextJSON(); !reflect.DeepEqual(got, wantInfo) {
				t.Fatalf("device_info = %v", got)
			}

			h.write(map[string]any{"action": "wifi_scan"})
			if got := string(h.next()); got != "error_encryption_required" {
				t.Fatalf("plaintext wifi_scan answered %q", got)
			}

			h.handshake()

			h.write(h.phone.seal(map[string]any{"action": "wifi_scan"}))
			wantScan := map[string]any{"type": "wifi_scan_result", "networks": []any{
				map[string]any{"ssid": "HomeNet", "rssi": -40.0, "secure": false},
			}}
			if got := h.nextOpened(); !reflect.DeepEqual(got, wantScan) {
				t.Fatalf("wifi_scan_result = %v", got)
			}

			h.write(h.phone.seal(provisionCommand()))
			h.expectStatuses("wifi_connecting", "wifi_connected", "auth_ok")
			h.expectRun(nil)

			if got := <-h.saved; got != wantResult {
				t.Errorf("saved %+v", got)
			}
			if got := verified.Load(); got != wantResult {
				t.Errorf("verified %+v", got)
			}
			wantStates := []pairing.State{
				pairing.StateWaiting, pairing.StateConnected, pairing.StateConfirmed,
				pairing.StateProvisioning, pairing.StateDone,
			}
			if got := h.states(); !reflect.DeepEqual(got, wantStates) {
				t.Errorf("progress %v", got)
			}
			select {
			case <-h.link.disconnects:
				t.Error("the phone was dropped after a clean setup")
			default:
			}
		})
	}
}

func TestDeviceInfoAgainMidHandshake(t *testing.T) {
	h := start(t, options{})
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())

	h.write(map[string]any{"action": "get_device_info"})
	if got := h.nextJSON(); got["type"] != "device_info" || got["model"] != "hatch_link" {
		t.Fatalf("mid-handshake get_device_info answered %v", got)
	}
	// Any other plaintext is ignored once a handshake has begun.
	h.write(map[string]any{"action": "wifi_scan"})
	h.quiet()

	h.write(h.phone.seal(map[string]any{"action": "pairing_client_finished"}))
	if got := h.nextStatus(); got != "pairing_confirmed" {
		t.Fatalf("status %q", got)
	}
}

func TestHandshakeRestartsOnTheSameConnection(t *testing.T) {
	h := start(t, options{})
	h.handshake()

	h.write(map[string]any{"action": "get_device_info"})
	if got := h.nextJSON(); got["type"] != "device_info" {
		t.Fatalf("get_device_info answered %v", got)
	}
	h.phone = newPhone(t)
	h.handshake()

	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected", "auth_ok")
	h.expectRun(nil)
	wantStates := []pairing.State{
		pairing.StateWaiting, pairing.StateConnected, pairing.StateConfirmed, pairing.StateConnected,
		pairing.StateConfirmed, pairing.StateProvisioning, pairing.StateDone,
	}
	if got := h.states(); !reflect.DeepEqual(got, wantStates) {
		t.Errorf("progress %v", got)
	}
}

func TestDisconnectMidway(t *testing.T) {
	h := start(t, options{})
	h.handshake()
	h.c.OnDisconnect()
	h.waitFor(pairing.StateWaiting)

	// The session went with the phone: its records no longer open, and plaintext is answered again.
	h.write(map[string]any{"action": "wifi_scan"})
	if got := string(h.next()); got != "error_encryption_required" {
		t.Fatalf("after a disconnect plaintext wifi_scan answered %q", got)
	}
	h.write(h.phone.seal(provisionCommand()))
	if got := string(h.next()); got != "error_pairing_decrypt" {
		t.Fatalf("a record from the lost session answered %q", got)
	}

	h.phone = newPhone(t)
	h.handshake()
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected", "auth_ok")
	h.expectRun(nil)
}

func TestDisconnectInTheMiddleOfAChunkedWrite(t *testing.T) {
	h := start(t, options{mtu: 23})
	raw, _ := json.Marshal(h.phone.hello())
	packets, _ := pairing.EncodeChunks(raw, 23)
	for _, p := range packets[:len(packets)-1] {
		h.c.OnWrite(p)
	}
	h.c.OnDisconnect()
	h.c.OnWrite(packets[len(packets)-1])
	h.quiet()
	h.handshake()
}

func TestTamperedCiphertext(t *testing.T) {
	h := start(t, options{})
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())

	finished := h.phone.seal(map[string]any{"action": "pairing_client_finished"})
	ciphertext, _ := b64.DecodeString(finished["ciphertext"].(string))
	ciphertext[0] ^= 1
	finished["ciphertext"] = b64.EncodeToString(ciphertext)
	h.write(finished)

	// The session is gone and plaintext is blocked, so the phone hears nothing and is dropped.
	select {
	case <-h.link.disconnects:
	case <-time.After(wait):
		t.Fatal("the phone was not dropped after a tampered record")
	}
	select {
	case m := <-h.link.messages:
		t.Fatalf("the device answered a tampered record with %q", m)
	default:
	}
	if p := h.waitFor(pairing.StateFailed); p.Reason != pairing.ReasonHandshake {
		t.Fatalf("failed for %q", p.Reason)
	}

	// The untampered record is no good either: the keys went with the session.
	h.phone.txCounter = 0
	h.write(h.phone.seal(map[string]any{"action": "pairing_client_finished"}))
	h.quiet()

	h.c.OnDisconnect()
	h.phone = newPhone(t)
	h.handshake()
}

func TestScheduledHangUpSparesTheNextPhone(t *testing.T) {
	h := start(t, options{})
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())
	finished := h.phone.seal(map[string]any{"action": "pairing_client_finished"})
	finished["tag"] = b64.EncodeToString(make([]byte, 16))
	h.write(finished)
	h.waitFor(pairing.StateFailed)

	// The phone leaves by itself and another connects before the hang-up comes due.
	h.c.OnDisconnect()
	h.phone = newPhone(t)
	h.handshake()
	select {
	case <-h.link.disconnects:
		t.Fatal("a hang-up meant for the first phone dropped the second")
	case <-time.After(600 * time.Millisecond):
	}
}

func TestWrongFirstRecordNeverConfirms(t *testing.T) {
	h := start(t, options{})
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())
	h.write(h.phone.seal(map[string]any{"action": "wifi_scan"}))
	if got := h.nextStatus(); got != "error_pairing_confirm_required" {
		t.Fatalf("status %q", got)
	}
	h.write(h.phone.seal(map[string]any{"action": "pairing_client_finished"}))
	select {
	case <-h.link.disconnects:
	case <-time.After(wait):
		t.Fatal("a late client-finished did not end the session")
	}
}

func TestSaveFailureIsReportedAsStorageError(t *testing.T) {
	h := start(t, options{save: func(pairing.Result) error { return errors.New("disk full") }})
	h.handshake()
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected", "error_storage")
	select {
	case <-h.link.disconnects:
	case <-time.After(wait):
		t.Fatal("the phone was not dropped after the save failed")
	}
	p := h.waitFor(pairing.StateFailed)
	if p.Reason != pairing.ReasonStorage || p.Err == nil || p.Err.Error() != "disk full" {
		t.Fatalf("failed with %q, %v", p.Reason, p.Err)
	}
	select {
	case err := <-h.done:
		t.Fatalf("Run returned %v; the window should stay open for another try", err)
	default:
	}
}

func TestRejectedTokenIsNotSaved(t *testing.T) {
	h := start(t, options{verify: func(context.Context, pairing.Result) error { return errors.New("HTTP 401") }})
	h.handshake()
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected", "auth_failed")
	if p := h.waitFor(pairing.StateFailed); p.Reason != pairing.ReasonAuthRejected {
		t.Fatalf("failed for %q", p.Reason)
	}
	select {
	case <-h.link.disconnects:
	case <-time.After(wait):
		t.Fatal("the phone was not dropped after the token was rejected")
	}
	select {
	case r := <-h.saved:
		t.Fatalf("a rejected token was saved: %v", r)
	default:
	}
}

func TestOfflineDeviceOffersNothingAndCanRetry(t *testing.T) {
	online := new(atomic.Bool)
	h := start(t, options{online: online})
	h.write(map[string]any{"action": "get_device_info"})
	if got := h.nextJSON(); got["network_ready"] != false {
		t.Fatalf("offline device_info = %v", got)
	}
	h.handshake()
	h.write(h.phone.seal(map[string]any{"action": "wifi_scan"}))
	if got := h.nextOpened(); !reflect.DeepEqual(got["networks"], []any{}) {
		t.Fatalf("offline scan offered %v", got["networks"])
	}
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_failed")
	if p := h.waitFor(pairing.StateFailed); p.Reason != pairing.ReasonOffline {
		t.Fatalf("failed for %q", p.Reason)
	}

	online.Store(true)
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected", "auth_ok")
	h.expectRun(nil)
}

func TestProvisionNeedsDeviceTokens(t *testing.T) {
	for name, change := range map[string]map[string]any{
		"no refresh token":  {"refresh_token": ""},
		"user token":        {"token_type": "user"},
		"null access token": {"access_token": nil},
		"null password":     {"password": nil},
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, options{})
			h.handshake()
			command := provisionCommand()
			for k, v := range change {
				command[k] = v
			}
			h.write(h.phone.seal(command))
			if got := h.nextStatus(); got != "error_missing_credentials" {
				t.Fatalf("status %q", got)
			}
		})
	}
}

func TestTokensGoOnlyToHTTPS(t *testing.T) {
	h := start(t, options{})
	h.handshake()
	command := provisionCommand()
	command["api_url"], command["api_url_v2"] = "http://legacy-api.example", "ftp://api.example"
	h.write(h.phone.seal(command))
	h.expectStatuses("wifi_connecting", "wifi_connected", "auth_ok")
	if got := <-h.saved; got.APIURL != "" || got.APIURLV2 != "" || got.APIRoot() != "https://api.muse.ai" {
		t.Fatalf("saved %+v with root %s", got, got.APIRoot())
	}
}

func TestSecondProvisionWhileOneIsRunning(t *testing.T) {
	release := make(chan struct{})
	h := start(t, options{verify: func(ctx context.Context, _ pairing.Result) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}})
	h.handshake()
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected")
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("error_operation_in_progress")
	close(release)
	h.expectStatuses("auth_ok")
}

func TestTokenCheckForALostSessionSavesNothing(t *testing.T) {
	release := make(chan struct{})
	h := start(t, options{verify: func(ctx context.Context, _ pairing.Result) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}})
	h.handshake()
	h.write(h.phone.seal(provisionCommand()))
	h.expectStatuses("wifi_connecting", "wifi_connected")
	h.c.OnDisconnect()
	h.waitFor(pairing.StateWaiting)
	close(release)
	h.quiet()
	select {
	case r := <-h.saved:
		t.Fatalf("tokens from a phone that left were saved: %v", r)
	default:
	}
}

func TestBadCommands(t *testing.T) {
	h := start(t, options{})
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())
	h.write(h.phone.seal(map[string]any{"action": "pairing_client_finished"}))
	h.nextStatus()

	h.write(h.phone.seal(map[string]any{"action": "dance"}))
	h.expectStatuses("error_unknown_action")
	h.write(h.phone.seal(map[string]any{"action": "ota"}))
	h.expectStatuses("error_unknown_action")
	h.c.OnWrite([]byte("not json"))
	h.expectStatuses("error_invalid_command")
	h.c.OnWrite([]byte("[1,2]"))
	h.expectStatuses("error_invalid_command")
}

func TestBadHelloGetsAPlaintextError(t *testing.T) {
	h := start(t, options{})
	hello := h.phone.hello()
	hello["pairing_policy"] = "confirm_press"
	h.write(hello)
	if got := string(h.next()); got != "error_pairing_invalid_hello" {
		t.Fatalf("a hello for the button policy answered %q", got)
	}
}

func TestWindowCloses(t *testing.T) {
	h := start(t, options{window: 50 * time.Millisecond})
	h.write(h.phone.hello())
	h.phone.accept(h.nextJSON())
	h.expectRun(pairing.ErrWindowClosed)
	if p := h.last(); p.State != pairing.StateFailed || p.Reason != pairing.ReasonWindowClosed {
		t.Fatalf("ended in %q for %q", p.State, p.Reason)
	}
	// A transport that has not heard yet must not block.
	h.c.OnWrite([]byte(`{"action":"get_device_info"}`))
	h.c.OnDisconnect()
}

func TestContextEndsRunWhileATokenCheckIsOut(t *testing.T) {
	entered := make(chan struct{})
	h := start(t, options{verify: func(ctx context.Context, _ pairing.Result) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}})
	h.handshake()
	h.write(h.phone.seal(provisionCommand()))
	<-entered
	h.cancel()
	h.expectRun(context.Canceled)
	if p := h.last(); p.State != pairing.StateFailed || p.Reason != pairing.ReasonCanceled {
		t.Fatalf("ended in %q for %q", p.State, p.Reason)
	}
}

func TestNewChecksTheNames(t *testing.T) {
	ok := pairing.Config{Device: device, Transport: &link{}, Save: func(pairing.Result) error { return nil }}
	if _, err := pairing.New(ok); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*pairing.Config){
		"hyphen in BLE name":  func(c *pairing.Config) { c.Device.BLEName = "MuseGadget-0A1B2C" },
		"lower-case BLE name": func(c *pairing.Config) { c.Device.BLEName = "MuseGadget0a1b2c" },
		"other node prefix":   func(c *pairing.Config) { c.Device.NodeID = "muse-0a1b2c" },
		"upper-case MAC":      func(c *pairing.Config) { c.Device.MAC = "02:11:22:0A:1B:2C" },
		"no transport":        func(c *pairing.Config) { c.Transport = nil },
		"no save":             func(c *pairing.Config) { c.Save = nil },
	} {
		c := ok
		change(&c)
		if _, err := pairing.New(c); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

func TestResultDoesNotPrintItsTokens(t *testing.T) {
	for _, format := range []string{"%v", "%+v", "%s"} {
		if s := fmt.Sprintf(format, wantResult); strings.Contains(s, "device-access") || strings.Contains(s, "device-refresh") {
			t.Errorf("%s printed a token: %s", format, s)
		}
	}
}
