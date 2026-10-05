// Identity, Credentials and State are ported from linux/src/musegadget/identity.py and config.py in
// Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the
// Apache License, Version 2.0 (LICENSE in this directory). Modified: rewritten in Go.

// Package muse makes the device a Muse gadget: it keeps one encrypted session open to the owner's
// Muse, runs the commands Muse sends, and asks Muse things in text or as a voice note.
//
// It is a port of the Linux client in Meta's Muse Gadget SDK and speaks the same protocol. The
// device token buys a list of VMs, each with its own bearer. A WebSocket to the VM carries a Noise
// XX session, and inside that session ride HTTP-shaped streams: one long POST /link-control for
// commands, one POST /chat/subscribe for everything Muse says, and a POST /chat/stream for each
// thing said to it. Pairing, which is where the device token comes from, is not here.
package muse

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// Identity is who the device says it is, to the app while pairing and to Muse afterwards. It is
// made once and kept across unpairing. MAC is shaped like a hardware address but is random, so it
// names no real interface.
type Identity struct {
	MAC string `json:"mac"`
}

var macShape = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// NewIdentity makes a fresh identity.
func NewIdentity() (Identity, error) {
	var octets [6]byte
	if _, err := rand.Read(octets[:]); err != nil {
		return Identity{}, err
	}
	// Unicast and locally administered, so it can never be mistaken for a vendor's address.
	octets[0] = octets[0]&0xFC | 0x02
	parts := make([]string, len(octets))
	for i, b := range octets {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return Identity{MAC: strings.Join(parts, ":")}, nil
}

// Valid says whether the identity is one NewIdentity could have made.
func (id Identity) Valid() bool { return macShape.MatchString(id.MAC) }

func (id Identity) suffix() string {
	hex := strings.ReplaceAll(id.MAC, ":", "")
	if len(hex) < 6 {
		return hex
	}
	return hex[len(hex)-6:]
}

// NodeID is the name Muse knows the device by, like homelink-a1b2c3.
func (id Identity) NodeID() string { return "homelink-" + id.suffix() }

// BLEName is the name to advertise while pairing, like MuseGadgetA1B2C3. There is no separator: the
// apps compare what follows the prefix with what follows "homelink-" in the node id.
func (id Identity) BLEName() string { return "MuseGadget" + strings.ToUpper(id.suffix()) }

// DeviceID is the id the pairing protocol carries.
func (id Identity) DeviceID() string { return "hatch-link:" + id.MAC }

// Credentials are what pairing leaves behind, in the shape the SDK keeps in pairing.json.
type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Username     string `json:"username"`
	// APIURL is kept because the app sends it, and never read: only older firmware uses it.
	APIURL             string `json:"api_url"`
	APIURLV2           string `json:"api_url_v2"`
	NoiseHost          string `json:"noise_host"`
	AccessTokenSavedAt int64  `json:"access_token_saved_at"` // Unix seconds
}

// State is everything that has to outlive a restart. A nil Credentials is an unpaired device.
type State struct {
	Identity    Identity     `json:"identity"`
	Credentials *Credentials `json:"credentials,omitempty"`
	// SDKTokenReported is set once Muse has been told the SDK token, which rides on a token
	// refresh, so that a restart does not rotate the tokens just to say it again.
	SDKTokenReported bool `json:"sdk_token_reported,omitempty"`
}

// Store keeps the State. Refreshing the tokens kills the pair that was used, so Save must not
// return nil until the new State would survive a power cut: the client uses rotated tokens only
// after Save has accepted them.
type Store interface {
	Load() (State, error)
	Save(State) error
}

// ParamType is the JSON type of a command parameter.
type ParamType string

const (
	String  ParamType = "string"
	Integer ParamType = "integer"
	Boolean ParamType = "boolean"
)

// Param is one parameter of a command.
type Param struct {
	Name        string
	Type        ParamType
	Description string
}

// Handler runs a command. args is the JSON object Muse sent, {} when it sent none. The payload is
// what Muse gets back and must marshal to JSON, and an error is reported to it as the failure. ctx
// ends when the command's time is up or the connection is gone.
type Handler func(ctx context.Context, args json.RawMessage) (payload any, err error)

// Command is something Muse may ask the device to do. Muse reads Description to decide when to
// call it, so it should say when to use it.
type Command struct {
	Name        string
	Description string
	Required    []Param
	Optional    []Param
	// Timeout is how long Muse should wait for it. Zero leaves Muse's default of 30 seconds.
	Timeout time.Duration
	Handler Handler
}

type askKind int

const (
	askNone askKind = iota
	askText
	askVoice
)

// AskInput is one thing to say to Muse: words, or a recording of them. Make it with Text or
// VoiceNote.
type AskInput struct {
	kind askKind
	text string
	wav  []byte
}

// Text is a typed message.
func Text(message string) AskInput { return AskInput{kind: askText, text: message} }

// VoiceNote is a spoken message as a WAV file, which Muse transcribes itself.
func VoiceNote(wav []byte) AskInput { return AskInput{kind: askVoice, wav: wav} }

// ReplyEvent is news of an answer on its way: Heard, ReplyText or Settled.
type ReplyEvent interface{ replyEvent() }

// Heard is what Muse made of a voice note.
type Heard struct{ Text string }

// ReplyText is the whole answer so far, sent each time it grows.
type ReplyText struct{ Text string }

// Settled is the answer once every message in it is complete and Muse has paused. It is almost
// always the whole answer, a moment before Ask returns to confirm it, so it is the time to start
// anything slow such as speaking. If Muse does go on, another Settled carries the longer text.
type Settled struct{ Text string }

func (Heard) replyEvent()     {}
func (ReplyText) replyEvent() {}
func (Settled) replyEvent()   {}

// Reply is a finished answer.
type Reply struct {
	Text string
	// Heard is what Muse made of the voice note, and empty for a typed message.
	Heard string
}

// Phase is where the connection stands.
type Phase int

const (
	Offline    Phase = iota // not connected, and ConnState.Err says why if something went wrong
	Connecting              // reaching Muse
	Online                  // registered: commands arrive and Ask works
	Unpaired                // no credentials, or Muse removed the device
)

func (p Phase) String() string {
	switch p {
	case Connecting:
		return "connecting"
	case Online:
		return "online"
	case Unpaired:
		return "unpaired"
	}
	return "offline"
}

// ConnState is what a screen would show about the connection. Err is only ever set when Phase is
// Offline.
type ConnState struct {
	Phase Phase
	Err   error
}

var (
	// ErrUnpaired ends Run: the device has no credentials, or Muse took them away.
	ErrUnpaired = errors.New("muse: not paired")
	// ErrOffline is Ask with no registered session to carry it.
	ErrOffline = errors.New("muse: not connected")
	// ErrBusy is Ask while another is still being answered. Muse does not say which question an
	// answer belongs to, so only one can be open.
	ErrBusy = errors.New("muse: already asking")
)

// Config is what a Client needs.
type Config struct {
	Store    Store
	Commands []Command
	// DisplayName is what the Muse app calls the device. Empty means the hostname.
	DisplayName string
	// Version is the software version reported to Muse.
	Version string
	// SDKToken is the developer's SDK token, if there is one. It is sent to Muse once.
	SDKToken string
	// OnState hears every change of connection state, on the goroutine that called Run.
	OnState func(ConnState)
	Logger  *slog.Logger
	// TLS replaces the default configuration, which verifies against the system's roots.
	TLS *tls.Config
}
