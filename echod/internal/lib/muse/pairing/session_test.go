package pairing

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

type vector struct {
	Name                    string `json:"name"`
	NodeID                  string `json:"node_id"`
	DeviceID                string `json:"device_id"`
	MAC                     string `json:"mac"`
	FirmwareVersion         string `json:"firmware_version"`
	MobileScalarHex         string `json:"mobile_private_scalar_hex"`
	DeviceScalarHex         string `json:"device_private_scalar_hex"`
	MobilePub               string `json:"mobile_pub"`
	DevicePub               string `json:"device_pub"`
	MobileNonce             string `json:"mobile_nonce"`
	DeviceNonce             string `json:"device_nonce"`
	ECDHSecretHex           string `json:"ecdh_secret_hex"`
	Transcript              string `json:"transcript"`
	TranscriptHash          string `json:"transcript_hash"`
	SessionSecretHex        string `json:"session_secret_hex"`
	MobileTXKeyHex          string `json:"mobile_tx_key_hex"`
	MobileRXKeyHex          string `json:"mobile_rx_key_hex"`
	SessionID               string `json:"session_id"`
	ClientFinishedPlaintext string `json:"client_finished_plaintext"`
	ClientFinishedAAD       string `json:"client_finished_aad"`
	ClientFinishedText      string `json:"client_finished_ciphertext"`
	ClientFinishedTag       string `json:"client_finished_tag"`
}

// The one vector for what this device speaks: community auth with the confirm_app policy.
const appVector = "community_app_v5"

func loadVectors(t *testing.T) map[string]vector {
	t.Helper()
	raw, err := os.ReadFile("testdata/link_pairing_v5.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	byName := map[string]vector{}
	for _, v := range file.Vectors {
		byName[v.Name] = v
	}
	if len(byName) != 3 || byName[appVector].Name == "" {
		t.Fatalf("expected three vectors including %s, got %d", appVector, len(byName))
	}
	return byName
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, ok := fields{"v": json.RawMessage(`"` + s + `"`)}.b64("v", maxCiphertextB64)
	if !ok {
		t.Fatalf("%q is not base64url", s)
	}
	return b
}

func (v vector) device() Device {
	return Device{NodeID: v.NodeID, BLEName: "MuseGadget000001", MAC: v.MAC, Version: v.FirmwareVersion}
}

func (v vector) hello(t *testing.T) clientHello {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"action": actionHello, "version": 5, "pairing_auth": "none", "pairing_policy": "confirm_app",
		"mobile_pub": v.MobilePub, "mobile_nonce": v.MobileNonce,
	})
	f, _ := parseFields(raw)
	h, err := parseHello(f)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// vectorSession is a session whose key and nonce are the vector's.
func vectorSession(t *testing.T, v vector) *session {
	t.Helper()
	fixed := bytes.NewReader(append(unhex(t, v.DeviceScalarHex), unb64(t, v.DeviceNonce)...))
	return newSession(v.device(), "", fixed)
}

func TestVectorKeySchedule(t *testing.T) {
	for name, v := range loadVectors(t) {
		t.Run(name, func(t *testing.T) {
			mobile, err := ecdh.P256().NewPrivateKey(unhex(t, v.MobileScalarHex))
			if err != nil {
				t.Fatal(err)
			}
			device, err := generateKey(bytes.NewReader(unhex(t, v.DeviceScalarHex)))
			if err != nil {
				t.Fatal(err)
			}
			if got := b64url(mobile.PublicKey().Bytes()); got != v.MobilePub {
				t.Errorf("mobile_pub = %s", got)
			}
			if got := b64url(device.PublicKey().Bytes()); got != v.DevicePub {
				t.Errorf("device_pub = %s", got)
			}
			secret, err := device.ECDH(mobile.PublicKey())
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(secret); got != v.ECDHSecretHex {
				t.Errorf("ecdh_secret = %s", got)
			}
			hash := sha256.Sum256([]byte(v.Transcript))
			if got := b64url(hash[:]); got != v.TranscriptHash {
				t.Errorf("transcript_hash = %s", got)
			}
			mobileNonce, deviceNonce := unb64(t, v.MobileNonce), unb64(t, v.DeviceNonce)
			sessionSecret, err := sessionSecret(secret, mobileNonce, deviceNonce, hash[:])
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(sessionSecret); got != v.SessionSecretHex {
				t.Errorf("session_secret = %s", got)
			}
			keys, err := deriveSessionKeys(secret, mobileNonce, deviceNonce, hash[:])
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(keys.mobileTX); got != v.MobileTXKeyHex {
				t.Errorf("mobile_tx_key = %s", got)
			}
			if got := hex.EncodeToString(keys.mobileRX); got != v.MobileRXKeyHex {
				t.Errorf("mobile_rx_key = %s", got)
			}
			if got := b64url(keys.sessionID); got != v.SessionID {
				t.Errorf("session_id = %s", got)
			}

			if got := string(recordAAD(v.SessionID, toDevice, 0)); got != v.ClientFinishedAAD {
				t.Errorf("client_finished_aad = %s", got)
			}
			gcm, err := newGCM(keys.mobileTX)
			if err != nil {
				t.Fatal(err)
			}
			sealed := gcm.Seal(nil, recordNonce(toDevice, 0), []byte(v.ClientFinishedPlaintext), recordAAD(v.SessionID, toDevice, 0))
			if got := b64url(sealed[:len(sealed)-tagBytes]); got != v.ClientFinishedText {
				t.Errorf("client_finished_ciphertext = %s", got)
			}
			if got := b64url(sealed[len(sealed)-tagBytes:]); got != v.ClientFinishedTag {
				t.Errorf("client_finished_tag = %s", got)
			}
		})
	}
}

func TestVectorTranscript(t *testing.T) {
	v := loadVectors(t)[appVector]
	if got := v.device().deviceID(); got != v.DeviceID {
		t.Fatalf("device id = %s, want %s", got, v.DeviceID)
	}
	got := buildTranscript(v.device(), v.MobilePub, v.DevicePub, v.MobileNonce, v.DeviceNonce)
	if got != v.Transcript {
		t.Errorf("transcript:\n%s\nwant:\n%s", got, v.Transcript)
	}
}

func TestVectorSession(t *testing.T) {
	v := loadVectors(t)[appVector]
	s := vectorSession(t, v)
	ready, err := s.hello(v.hello(t))
	if err != nil {
		t.Fatal(err)
	}
	want := pairingReady{
		Type: "pairing_ready", Version: 5, DeviceID: v.DeviceID, NodeID: v.NodeID, MAC: v.MAC,
		Model: "hatch_link", FirmwareVersion: v.FirmwareVersion, PairingAuth: "none", PairingPolicy: "confirm_app",
		DevicePub: v.DevicePub, DeviceNonce: v.DeviceNonce, TranscriptHash: v.TranscriptHash, SessionID: v.SessionID,
	}
	if ready != want {
		t.Fatalf("pairing_ready = %+v\nwant %+v", ready, want)
	}

	finished := record{sessionID: v.SessionID, ciphertext: unb64(t, v.ClientFinishedText), tag: unb64(t, v.ClientFinishedTag)}
	plaintext, ok := s.open(finished)
	if !ok || string(plaintext) != v.ClientFinishedPlaintext {
		t.Fatalf("open = %q, %v", plaintext, ok)
	}
	f, _ := parseFields(plaintext)
	gen := s.clientFinished(parseClientFinished(f))
	if gen == 0 || !s.confirmed() {
		t.Fatal("the vector's client-finished record did not confirm the session")
	}

	// What the device seals, the phone's receive key opens.
	env, ok := s.sealStatus(statusConfirmed, gen)
	if !ok || env.Counter != "0" || env.SessionID != v.SessionID {
		t.Fatalf("sealStatus = %+v, %v", env, ok)
	}
	gcm, _ := newGCM(unhex(t, v.MobileRXKeyHex))
	sealed := append(unb64(t, env.Ciphertext), unb64(t, env.Tag)...)
	opened, err := gcm.Open(nil, recordNonce(fromDevice, 0), sealed, []byte("hatch-link ble setup v1|"+v.SessionID+"|d2m|0"))
	if err != nil || string(opened) != `{"type":"status","status":"pairing_confirmed"}` {
		t.Fatalf("status record opened as %q, %v", opened, err)
	}
}

func TestReplayedRecordEndsTheSession(t *testing.T) {
	v := loadVectors(t)[appVector]
	s := vectorSession(t, v)
	if _, err := s.hello(v.hello(t)); err != nil {
		t.Fatal(err)
	}
	finished := record{sessionID: v.SessionID, ciphertext: unb64(t, v.ClientFinishedText), tag: unb64(t, v.ClientFinishedTag)}
	if _, ok := s.open(finished); !ok {
		t.Fatal("first record did not open")
	}
	if _, ok := s.open(finished); ok {
		t.Fatal("a replayed record opened")
	}
	if s.state != sessionIdle {
		t.Fatal("the session survived a replay")
	}
}

func TestSessionStagesExpire(t *testing.T) {
	v := loadVectors(t)[appVector]
	s := vectorSession(t, v)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.hello(v.hello(t)); err != nil {
		t.Fatal(err)
	}
	gen := s.gen
	now = now.Add(clientFinishedTimeout + time.Second)
	if _, ok := s.sealStatus(statusConfirmRequired, 0); ok {
		t.Fatal("a session past its deadline still sealed a record")
	}
	if s.gen != gen {
		t.Fatal("expiry changed the generation; its owner could no longer recognize it")
	}
}

func TestHelloIsCheckedAtTheBoundary(t *testing.T) {
	v := loadVectors(t)[appVector]
	offPoint := b64url(append([]byte{0x04}, make([]byte, 64)...))
	for name, tc := range map[string]struct {
		change map[string]any
		want   error
	}{
		"version 4":          {map[string]any{"version": 4}, errHelloOffer},
		"version true":       {map[string]any{"version": true}, errHelloOffer},
		"version as text":    {map[string]any{"version": "5"}, errHelloOffer},
		"button policy":      {map[string]any{"pairing_policy": "confirm_press"}, errHelloOffer},
		"fleet auth":         {map[string]any{"pairing_auth": "fleet_ecdsa_p256_v1"}, errHelloOffer},
		"version 5.0":        {map[string]any{"version": 5.0}, nil},
		"point off curve":    {map[string]any{"mobile_pub": offPoint}, errHelloKey},
		"compressed point":   {map[string]any{"mobile_pub": b64url(append([]byte{0x02}, make([]byte, 64)...))}, errHelloKey},
		"short nonce":        {map[string]any{"mobile_nonce": "AAEC"}, errHelloKey},
		"padded base64":      {map[string]any{"mobile_nonce": v.MobileNonce + "=="}, errHelloKey},
		"base64 with spaces": {map[string]any{"mobile_nonce": v.MobileNonce[:10] + "\n" + v.MobileNonce[10:]}, errHelloKey},
		"key not text":       {map[string]any{"mobile_pub": 7}, errHelloKey},
	} {
		m := map[string]any{
			"action": actionHello, "version": 5, "pairing_auth": "none", "pairing_policy": "confirm_app",
			"mobile_pub": v.MobilePub, "mobile_nonce": v.MobileNonce,
		}
		for k, val := range tc.change {
			m[k] = val
		}
		raw, _ := json.Marshal(m)
		f, _ := parseFields(raw)
		if _, err := parseHello(f); err != tc.want {
			t.Errorf("%s: parseHello error = %v, want %v", name, err, tc.want)
		}
	}
}

func TestEveryStateCanFailExceptDone(t *testing.T) {
	for s := StateWaiting; s <= StateFailed; s++ {
		moves, ok := transitions[s]
		if !ok {
			t.Errorf("%v has no row in transitions", s)
		}
		if _, closes := moves[windowClosed]; closes == (s == StateDone) {
			t.Errorf("%v: windowClosed legal = %v", s, closes)
		}
	}
	if len(transitions[StateDone]) != 0 {
		t.Error("done is not terminal")
	}
}
