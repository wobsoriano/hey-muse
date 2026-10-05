// This file is a modified port of linux/src/musegadget/pairing.py and ble_setup.py from Meta's Muse
// Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License,
// Version 2.0. It holds the wire shapes and the checks the SDK makes on what the phone sends.

package pairing

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Wire identifiers. "hatch" is the protocol's own name for a Muse gadget and stays only here.
const (
	pairingVersion = 5
	pairingModel   = "hatch_link"
	pairingSuite   = "p256-hkdf-sha256-aes-gcm-v1"
	authCommunity  = "none"
	policyApp      = "confirm_app"
	deviceIDPrefix = "hatch-link:"
	recordLabel    = "hatch-link ble setup v1"
	sessionIDLabel = "hatch-link session id v1"
)

const (
	actionHello          = "pairing_client_hello"
	actionEncrypted      = "pairing_encrypted"
	actionDeviceInfo     = "get_device_info"
	actionClientFinished = "pairing_client_finished"
	actionWiFiScan       = "wifi_scan"
	actionProvision      = "provision_v2"
)

// sensitiveActions are refused outside a confirmed, encrypted session, including the ones this device
// does not carry out.
var sensitiveActions = map[string]bool{
	"provision": true, actionProvision: true, actionWiFiScan: true, "ota": true, "device.ota": true,
	"unpair": true, "set_wifi": true, "set_auth": true,
}

// status is a word the phone is told, inside a status message or bare.
type status string

const (
	statusConfirmed          status = "pairing_confirmed"
	statusWiFiConnecting     status = "wifi_connecting"
	statusWiFiConnected      status = "wifi_connected"
	statusWiFiFailed         status = "wifi_failed"
	statusAuthOK             status = "auth_ok"
	statusAuthFailed         status = "auth_failed"
	statusStorage            status = "error_storage"
	statusInvalidCommand     status = "error_invalid_command"
	statusUnknownAction      status = "error_unknown_action"
	statusEncryptionRequired status = "error_encryption_required"
	statusConfirmRequired    status = "error_pairing_confirm_required"
	statusInvalidHello       status = "error_pairing_invalid_hello"
	statusUnavailable        status = "error_pairing_unavailable"
	statusDecrypt            status = "error_pairing_decrypt"
	statusMissingCredentials status = "error_missing_credentials"
	statusInProgress         status = "error_operation_in_progress"
)

// plaintextStatuses may go out bare, before any session exists. Everything else is only ever sent
// encrypted.
var plaintextStatuses = map[status]bool{
	statusEncryptionRequired: true, statusInvalidHello: true, statusUnavailable: true, statusDecrypt: true,
}

const (
	p256PointBytes   = 65
	nonceBytes       = 16
	tagBytes         = 16
	maxB64Chars      = 4096
	maxCiphertextB64 = 16384
)

// What the device sends.

type deviceInfo struct {
	Type            string `json:"type"`
	NodeID          string `json:"node_id"`
	Version         string `json:"version"`
	DeviceID        string `json:"device_id"`
	MAC             string `json:"mac"`
	Model           string `json:"model"`
	PairingProtocol int    `json:"pairing_protocol"`
	PairingAuth     string `json:"pairing_auth"`
	PairingEpoch    int    `json:"pairing_auth_epoch"`
	PairingPolicy   string `json:"pairing_policy"`
	BuildSHA        string `json:"build_sha"`
	NetworkReady    bool   `json:"network_ready"`
}

type pairingReady struct {
	Type            string `json:"type"`
	Version         int    `json:"version"`
	DeviceID        string `json:"device_id"`
	NodeID          string `json:"node_id"`
	MAC             string `json:"mac"`
	Model           string `json:"model"`
	FirmwareVersion string `json:"firmware_version"`
	PairingAuth     string `json:"pairing_auth"`
	PairingEpoch    int    `json:"pairing_auth_epoch"`
	PairingPolicy   string `json:"pairing_policy"`
	DevicePub       string `json:"device_pub"`
	DeviceNonce     string `json:"device_nonce"`
	TranscriptHash  string `json:"transcript_hash"`
	SessionID       string `json:"session_id"`
}

type statusMessage struct {
	Type     string `json:"type"`
	Status   status `json:"status"`
	SDKToken string `json:"sdk_token,omitempty"`
}

type envelope struct {
	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	Counter    string `json:"counter"`
	Ciphertext string `json:"ciphertext"`
	Tag        string `json:"tag"`
}

type scanResult struct {
	Type     string        `json:"type"`
	Networks []WiFiNetwork `json:"networks"`
}

// What the phone sends, once checked.

// clientHello is a pairing_client_hello whose offer this device accepts and whose key is on the curve.
type clientHello struct {
	peer  *ecdh.PublicKey
	pub   []byte
	nonce []byte
}

// record is a pairing_encrypted envelope that is well formed; whether it opens is the session's call.
type record struct {
	sessionID  string
	counter    uint64
	ciphertext []byte
	tag        []byte
}

// provision is a provision_v2 that carries device tokens.
type provision struct {
	ssid     string
	password string
	result   Result
}

var (
	// errHelloOffer is a hello for a version, auth or policy this device does not speak. A session
	// already open survives it.
	errHelloOffer = errors.New("pairing: unsupported hello")
	// errHelloKey is a hello with bad key material. It ends any session already open.
	errHelloKey = errors.New("pairing: bad hello key material")
)

// fields is a command as it arrived: a JSON object, its values not yet looked at. Keys are matched
// exactly, which encoding/json's struct decoding would not do.
type fields map[string]json.RawMessage

func parseFields(raw []byte) (fields, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return nil, false
	}
	return f, true
}

// str is the value at key when it is a JSON string.
func (f fields) str(key string) (string, bool) {
	v := bytes.TrimSpace(f[key])
	if len(v) == 0 || v[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}

func (f fields) text(key string) string {
	s, _ := f.str(key)
	return s
}

// number is the value at key when it is a JSON number; true and "5" are not.
func (f fields) number(key string) (float64, bool) {
	v := bytes.TrimSpace(f[key])
	if len(v) == 0 || !(v[0] == '-' || v[0] >= '0' && v[0] <= '9') {
		return 0, false
	}
	n, err := strconv.ParseFloat(string(v), 64)
	return n, err == nil
}

// b64 is the value at key decoded from unpadded base64url. encoding/base64 skips line endings, which
// the firmware rejects, so the alphabet is checked first.
func (f fields) b64(key string, maxChars int) ([]byte, bool) {
	s, ok := f.str(key)
	if !ok || len(s) == 0 || len(s) > maxChars || len(s)%4 == 1 {
		return nil, false
	}
	if strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return b, err == nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func parseHello(f fields) (clientHello, error) {
	version, ok := f.number("version")
	if !ok || version != pairingVersion || f.text("pairing_auth") != authCommunity || f.text("pairing_policy") != policyApp {
		return clientHello{}, errHelloOffer
	}
	pub, okPub := f.b64("mobile_pub", maxB64Chars)
	nonce, okNonce := f.b64("mobile_nonce", maxB64Chars)
	if !okPub || !okNonce || len(pub) != p256PointBytes || pub[0] != 0x04 || len(nonce) != nonceBytes {
		return clientHello{}, errHelloKey
	}
	peer, err := ecdh.P256().NewPublicKey(pub)
	if err != nil {
		return clientHello{}, errHelloKey
	}
	return clientHello{peer: peer, pub: pub, nonce: nonce}, nil
}

func parseRecord(f fields) (record, bool) {
	sessionID, okID := f.str("session_id")
	counterText, okCounter := f.str("counter")
	ciphertext, okText := f.b64("ciphertext", maxCiphertextB64)
	tag, okTag := f.b64("tag", maxB64Chars)
	if !okID || !okCounter || !okText || !okTag || len(tag) != tagBytes {
		return record{}, false
	}
	// ParseUint takes digits only at base 10, and its range is the counter's.
	counter, err := strconv.ParseUint(counterText, 10, 64)
	if err != nil {
		return record{}, false
	}
	return record{sessionID: sessionID, counter: counter, ciphertext: ciphertext, tag: tag}, true
}

// parseClientFinished reports whether the command is exactly {"action":"pairing_client_finished"}.
func parseClientFinished(f fields) bool {
	return len(f) == 1 && f.text("action") == actionClientFinished
}

func parseProvision(f fields) (provision, bool) {
	ssid, okSSID := f.str("ssid")
	password, okPassword := f.str("password")
	p := provision{ssid: ssid, password: password, result: Result{
		AccessToken:  f.text("access_token"),
		RefreshToken: f.text("refresh_token"),
		TokenType:    f.text("token_type"),
		Username:     f.text("username"),
		APIURL:       httpsOnly(f.text("api_url")),
		APIURLV2:     httpsOnly(f.text("api_url_v2")),
		NoiseHost:    f.text("noise_host"),
	}}
	if !okSSID || !okPassword || p.result.AccessToken == "" || p.result.RefreshToken == "" || p.result.TokenType != "device" {
		return provision{}, false
	}
	return p, true
}

// httpsOnly drops a URL the tokens must not be sent to.
func httpsOnly(u string) string {
	if strings.HasPrefix(u, "https://") {
		return u
	}
	return ""
}
