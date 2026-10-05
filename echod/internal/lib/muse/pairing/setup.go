// This file is a modified port of linux/src/musegadget/ble_setup.py and the pair command of cli.py
// from Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the
// Apache License, Version 2.0. The SDK's threads and locks became one goroutine that owns the state.

package pairing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Device is who this device says it is. The SDK derives all of it from one random, locally
// administered MAC that it keeps across pairings.
type Device struct {
	NodeID  string // homelink-xxxxxx
	BLEName string // MuseGadgetXXXXXX, the same six digits; what the GATT server advertises
	MAC     string // aa:bb:cc:dd:ee:ff, lower case; an identity, not a network interface's address
	Version string // firmware version; goes into the handshake transcript
}

func (d Device) deviceID() string { return deviceIDPrefix + d.MAC }

var macPattern = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

func (d Device) validate() error {
	suffix, ok := strings.CutPrefix(d.NodeID, NodeIDPrefix)
	if !ok || suffix == "" {
		return fmt.Errorf("pairing: node id %q does not start with %q", d.NodeID, NodeIDPrefix)
	}
	// The apps match the two by what follows the prefixes and show nothing when they differ.
	if want := BLENamePrefix + strings.ToUpper(suffix); d.BLEName != want {
		return fmt.Errorf("pairing: BLE name %q does not go with node id %q (want %q)", d.BLEName, d.NodeID, want)
	}
	if !macPattern.MatchString(d.MAC) {
		return fmt.Errorf("pairing: %q is not a lower-case MAC", d.MAC)
	}
	return nil
}

// Config is what a Controller needs. Device, Transport and Save are required.
type Config struct {
	Device    Device
	Transport Transport

	// SDKToken is the mgst_ token from gadgets.muse.ai, handed to the app once the session is
	// confirmed. Muse stops pairing gadgets without one.
	SDKToken string

	// Network answers the Wi-Fi half of setup. Nil is AlreadyOnline{}.
	Network Network

	// Verify checks the tokens with Muse before they are saved; an error tells the phone auth_failed.
	// It runs off the loop and must stop when ctx does. Nil is TokenCheck's Verify.
	Verify func(ctx context.Context, r Result) error

	// Save persists the tokens. The phone is told auth_ok only after it returns nil, and
	// error_storage when it does not. It runs on the loop and must only write local state.
	Save func(r Result) error

	// Progress hears each change of State. It runs on the loop and must not block.
	Progress func(Progress)

	// Window is how long setup stays open. Zero is DefaultWindow.
	Window time.Duration

	// Rand supplies the device's key and nonce. Nil is crypto/rand.
	Rand io.Reader
}

// ErrWindowClosed is what Run returns when nobody finished setup within the window.
var ErrWindowClosed = errors.New("pairing: setup window closed without pairing")

// How long the phone is given to read a last status before it is dropped.
const (
	disconnectAfterError   = 300 * time.Millisecond
	disconnectAfterFailure = 500 * time.Millisecond
)

// maxPending bounds the writes waiting for the loop: two of the largest messages at the smallest MTU.
const maxPending = 1024

// Controller holds the setup conversation with one phone at a time.
//
// Run owns all of its state on the goroutine that calls it. OnWrite and OnDisconnect only hand an
// event to that goroutine, so a Transport may call them from any goroutine, before or after Run. They
// must be called in the order things happened on the link, and not from inside Send or Disconnect.
// Send, MTU and Disconnect are only ever called from Run's goroutine.
type Controller struct {
	cfg     Config
	inbox   chan event
	stopped chan struct{}
	started atomic.Bool
	jobs    sync.WaitGroup

	// Everything below belongs to Run's goroutine.
	sess *session
	asm  Assembler
	// link counts the connections seen, so a disconnect scheduled for one phone cannot drop the next.
	link uint64
	// plaintextBlocked is set once a handshake has begun on this connection: from then on a plaintext
	// command other than a new hello or get_device_info is ignored without an answer.
	plaintextBlocked bool
	// provisioning is set while a join and token check are in flight. Only one runs at a time, even
	// across sessions.
	provisioning bool
	progress     Progress
}

type event interface{ isEvent() }

type (
	wrote        struct{ packet []byte }
	disconnected struct{}
	// scanned, joined and checked carry the generation their work began under.
	scanned struct {
		gen      generation
		networks []WiFiNetwork
	}
	joined struct {
		gen generation
		err error
	}
	checked struct {
		gen    generation
		result Result
		err    error
	}
	hangUp struct{ link uint64 }
)

func (wrote) isEvent()        {}
func (disconnected) isEvent() {}
func (scanned) isEvent()      {}
func (joined) isEvent()       {}
func (checked) isEvent()      {}
func (hangUp) isEvent()       {}

// New checks cfg and returns a Controller waiting for Run.
func New(cfg Config) (*Controller, error) {
	if err := cfg.Device.validate(); err != nil {
		return nil, err
	}
	if cfg.Transport == nil || cfg.Save == nil {
		return nil, errors.New("pairing: Transport and Save are required")
	}
	// An empty version would leave a transcript line empty, which the protocol does not allow.
	if cfg.Device.Version == "" {
		cfg.Device.Version = "unknown"
	}
	if cfg.Network == nil {
		cfg.Network = AlreadyOnline{}
	}
	if cfg.Verify == nil {
		ua := fmt.Sprintf("echod/%s (%s %s)", cfg.Device.Version, runtime.GOOS, runtime.GOARCH)
		cfg.Verify = TokenCheck{UserAgent: ua}.Verify
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Controller{
		cfg:     cfg,
		inbox:   make(chan event, maxPending),
		stopped: make(chan struct{}),
		sess:    newSession(cfg.Device, cfg.SDKToken, cfg.Rand),
	}, nil
}

// OnWrite takes one write to the RX characteristic. It copies data.
func (c *Controller) OnWrite(data []byte) { c.post(wrote{packet: bytes.Clone(data)}) }

// OnDisconnect tells the Controller the phone is gone.
func (c *Controller) OnDisconnect() { c.post(disconnected{}) }

// post blocks only when maxPending events are waiting, and gives up once Run has returned.
func (c *Controller) post(ev event) {
	select {
	case <-c.stopped:
	default:
		select {
		case c.inbox <- ev:
		case <-c.stopped:
		}
	}
}

// Run holds setup open until a phone finishes it, the window closes, or ctx ends. It returns nil once
// the tokens are saved and the phone was told; the caller should keep the Transport up for
// ShutdownGrace after that. It returns ErrWindowClosed when the window closed, and ctx's error when
// ctx ended. A Controller runs once.
func (c *Controller) Run(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("pairing: a Controller runs once")
	}
	ctx, cancel := context.WithTimeoutCause(ctx, c.cfg.Window, ErrWindowClosed)
	defer func() {
		cancel()
		close(c.stopped)
		c.jobs.Wait()
	}()
	c.report()
	for {
		select {
		case <-ctx.Done():
			err := context.Cause(ctx)
			reason := ReasonCanceled
			if errors.Is(err, ErrWindowClosed) {
				reason = ReasonWindowClosed
			}
			c.sess.reset()
			c.advance(windowClosed, reason, err)
			return err
		case ev := <-c.inbox:
			if c.handleEvent(ctx, ev) {
				return nil
			}
		}
	}
}

// handleEvent reports whether setup is finished.
func (c *Controller) handleEvent(ctx context.Context, ev event) bool {
	switch ev := ev.(type) {
	case wrote:
		c.advance(phoneWrote, ReasonNone, nil)
		if message, ok := c.asm.Feed(ev.packet); ok {
			c.handleMessage(ctx, message, false)
		}
	case disconnected:
		slog.Info("muse pairing: phone disconnected")
		c.asm.Reset()
		c.plaintextBlocked = false
		c.sess.reset()
		c.link++
		c.advance(phoneLeft, ReasonNone, nil)
	case scanned:
		c.sendEncrypted(scanResult{Type: "wifi_scan_result", Networks: ev.networks}, ev.gen)
	case joined:
		c.onJoined(ev)
	case checked:
		return c.onChecked(ev)
	case hangUp:
		if ev.link != c.link {
			break
		}
		if err := c.cfg.Transport.Disconnect(); err != nil {
			slog.Warn("muse pairing: could not drop the phone", "err", err)
		}
	}
	return false
}

func (c *Controller) handleMessage(ctx context.Context, raw []byte, decrypted bool) {
	f, ok := parseFields(raw)
	if !ok {
		slog.Warn("muse pairing: command is not a JSON object", "bytes", len(raw))
		c.sendStatus(statusInvalidCommand, 0)
		return
	}
	action := f.text("action")
	slog.Debug("muse pairing: command", "action", action, "encrypted", decrypted)

	switch {
	case !decrypted && action == actionHello:
		c.onHello(f)
	case !decrypted && action == actionEncrypted:
		c.onRecord(ctx, f)
	case action == actionDeviceInfo:
		// Public, so answered in plaintext at any point. Android reads it again when it restarts a
		// handshake on the same connection.
		c.send(c.deviceInfo(ctx))
	case !decrypted && c.plaintextBlocked:
		slog.Warn("muse pairing: plaintext command ignored after the handshake began", "action", action)
	case !decrypted && sensitiveActions[action]:
		c.sendStatus(statusEncryptionRequired, 0)
	case decrypted && action == actionClientFinished:
		c.onClientFinished(f)
	case decrypted && sensitiveActions[action] && !c.sess.confirmed():
		c.sendStatus(statusConfirmRequired, 0)
	case decrypted && action == actionWiFiScan:
		c.onWiFiScan(ctx)
	case decrypted && action == actionProvision:
		c.onProvision(ctx, f)
	default:
		c.sendStatus(statusUnknownAction, 0)
	}
}

func (c *Controller) deviceInfo(ctx context.Context) deviceInfo {
	d := c.cfg.Device
	return deviceInfo{
		Type:            "device_info",
		NodeID:          d.NodeID,
		Version:         d.Version,
		DeviceID:        d.deviceID(),
		MAC:             d.MAC,
		Model:           pairingModel,
		PairingProtocol: pairingVersion,
		PairingAuth:     authCommunity,
		PairingPolicy:   policyApp,
		NetworkReady:    c.cfg.Network.Online(ctx),
	}
}

func (c *Controller) onHello(f fields) {
	h, err := parseHello(f)
	if err != nil {
		if errors.Is(err, errHelloKey) {
			c.sess.reset()
			c.advance(attemptFailed, ReasonHandshake, nil)
		}
		c.sendStatus(statusInvalidHello, 0)
		return
	}
	ready, err := c.sess.hello(h)
	if err != nil {
		slog.Error("muse pairing: could not start a session", "err", err)
		c.advance(attemptFailed, ReasonHandshake, err)
		c.sendStatus(statusUnavailable, 0)
		return
	}
	c.plaintextBlocked = true
	c.advance(handshakeBegan, ReasonNone, nil)
	c.send(ready)
}

func (c *Controller) onRecord(ctx context.Context, f fields) {
	var plaintext []byte
	r, ok := parseRecord(f)
	if ok {
		plaintext, ok = c.sess.open(r)
	} else {
		c.sess.reset()
	}
	if !ok {
		c.dropSession()
		return
	}
	c.handleMessage(ctx, plaintext, true)
}

func (c *Controller) onClientFinished(f fields) {
	gen := c.sess.clientFinished(parseClientFinished(f))
	if gen == 0 {
		c.dropSession()
		return
	}
	slog.Info("muse pairing: session confirmed by the app")
	c.advance(phoneConfirmed, ReasonNone, nil)
	c.sendStatus(statusConfirmed, gen)
}

// dropSession follows a record that did not open or did not belong. The session is already gone, so
// the status only reaches a phone that never began a handshake; the others just see the link drop.
func (c *Controller) dropSession() {
	c.sendStatus(statusDecrypt, 0)
	c.hangUpAfter(disconnectAfterError)
	c.advance(attemptFailed, ReasonHandshake, nil)
}

func (c *Controller) onWiFiScan(ctx context.Context) {
	gen := c.sess.gen
	c.jobs.Go(func() {
		networks, err := c.cfg.Network.Scan(ctx)
		if err != nil {
			slog.Warn("muse pairing: Wi-Fi scan failed", "err", err)
		}
		if networks == nil {
			networks = []WiFiNetwork{}
		}
		c.post(scanned{gen: gen, networks: networks})
	})
}

func (c *Controller) onProvision(ctx context.Context, f fields) {
	p, ok := parseProvision(f)
	if !ok {
		c.sendStatus(statusMissingCredentials, 0)
		return
	}
	if c.provisioning {
		c.sendStatus(statusInProgress, 0)
		return
	}
	gen := c.sess.markProvisioning()
	if gen == 0 {
		c.sendStatus(statusConfirmRequired, 0)
		return
	}
	c.provisioning = true
	c.advance(provisionBegan, ReasonNone, nil)
	c.sendStatus(statusWiFiConnecting, gen)
	c.jobs.Go(func() {
		err := c.cfg.Network.Join(ctx, p.ssid, p.password)
		c.post(joined{gen: gen, err: err})
		if err != nil {
			return
		}
		c.post(checked{gen: gen, result: p.result, err: c.cfg.Verify(ctx, p.result)})
	})
}

func (c *Controller) onJoined(ev joined) {
	if ev.err == nil {
		c.sendStatus(statusWiFiConnected, ev.gen)
		return
	}
	c.provisioning = false
	slog.Warn("muse pairing: not online", "err", ev.err)
	// The session stays in provisioning so the app can retry, as the firmware does.
	if c.sess.extendProvisioning(ev.gen) {
		c.sendStatus(statusWiFiFailed, ev.gen)
		c.advance(attemptFailed, ReasonOffline, ev.err)
	}
}

// onChecked reports whether setup is finished.
func (c *Controller) onChecked(ev checked) bool {
	c.provisioning = false
	if !c.sess.provisioningValid(ev.gen) {
		slog.Info("muse pairing: token check finished for a session that is gone")
		return false
	}
	if ev.err != nil {
		slog.Warn("muse pairing: device token check failed", "err", ev.err)
		c.failAttempt(statusAuthFailed, ReasonAuthRejected, ev.gen, ev.err)
		return false
	}
	if err := c.cfg.Save(ev.result); err != nil {
		slog.Error("muse pairing: could not save the pairing", "err", err)
		c.failAttempt(statusStorage, ReasonStorage, ev.gen, err)
		return false
	}
	c.sendStatus(statusAuthOK, ev.gen)
	slog.Info("muse pairing: setup complete")
	c.advance(paired, ReasonNone, nil)
	return true
}

func (c *Controller) failAttempt(st status, reason Reason, gen generation, err error) {
	c.sendStatus(st, gen)
	c.hangUpAfter(disconnectAfterFailure)
	c.advance(attemptFailed, reason, err)
}

func (c *Controller) hangUpAfter(d time.Duration) {
	link := c.link
	time.AfterFunc(d, func() { c.post(hangUp{link: link}) })
}

// advance moves the State along transitions and tells Progress.
func (c *Controller) advance(t trigger, reason Reason, err error) {
	next, ok := transitions[c.progress.State][t]
	if !ok {
		return
	}
	c.progress = Progress{State: next}
	if next == StateFailed {
		c.progress.Reason, c.progress.Err = reason, err
	}
	c.report()
}

func (c *Controller) report() {
	if c.cfg.Progress != nil {
		c.cfg.Progress(c.progress)
	}
}

// sendStatus tells the phone st, encrypted whenever a session is open. A nonzero gen ties it to the
// work it reports on, and it is dropped once that work is stale.
func (c *Controller) sendStatus(st status, gen generation) {
	if env, ok := c.sess.sealStatus(st, gen); ok {
		c.send(env)
		slog.Debug("muse pairing: status", "status", string(st), "counter", env.Counter)
		return
	}
	if gen != 0 || c.plaintextBlocked || !plaintextStatuses[st] {
		slog.Debug("muse pairing: status not sent", "status", string(st))
		return
	}
	// Bare and unframed, as the firmware sends it.
	c.transmit([][]byte{[]byte(st)})
}

func (c *Controller) sendEncrypted(v any, gen generation) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		slog.Error("muse pairing: could not encode a message", "err", err)
		return
	}
	if env, ok := c.sess.seal(plaintext, gen); ok {
		c.send(env)
	}
}

func (c *Controller) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("muse pairing: could not encode a message", "err", err)
		return
	}
	packets, err := EncodeChunks(b, c.cfg.Transport.MTU())
	if err != nil {
		slog.Error("muse pairing: message too long to send", "err", err)
		return
	}
	c.transmit(packets)
}

func (c *Controller) transmit(packets [][]byte) {
	if err := c.cfg.Transport.Send(packets); err != nil {
		slog.Warn("muse pairing: could not notify the phone", "err", err)
	}
}
