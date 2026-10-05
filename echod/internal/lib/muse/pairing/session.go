// This file is a modified port of linux/src/musegadget/pairing.py from Meta's Muse Gadget SDK,
// Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0.

package pairing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// sessionState is where the handshake stands.
type sessionState int

const (
	sessionIdle sessionState = iota
	sessionWaitClientFinished
	sessionReady
	sessionProvisioning
)

// How long each stage may take before the keys are dropped.
const (
	clientFinishedTimeout = 60 * time.Second
	confirmedTimeout      = 120 * time.Second
	provisioningTimeout   = 120 * time.Second
)

// generation names one stretch of one session. Work that outlives a command (a Wi-Fi join, a token
// check) keeps the generation it started under, so what it finds cannot act on a later session. Zero
// is never a live generation.
type generation uint64

type direction byte

const (
	toDevice   direction = 0
	fromDevice direction = 1
)

// session is one device's pairing state: community mode only, so auth is "none", the policy is
// confirm_app, and a valid client-finished record confirms the session without a button. It is not
// safe for concurrent use; the Controller's loop owns it.
type session struct {
	dev      Device
	sdkToken string
	rand     io.Reader
	now      func() time.Time

	gen       generation
	state     sessionState
	deadline  time.Time
	rx, tx    cipher.AEAD
	id        string
	rxCounter uint64
	txCounter uint64
}

func newSession(dev Device, sdkToken string, rand io.Reader) *session {
	return &session{dev: dev, sdkToken: sdkToken, rand: rand, now: time.Now, gen: 1}
}

// buildTranscript is the canonical v5 community transcript. Its SHA-256 is the transcript hash both
// sides mix into the keys.
func buildTranscript(dev Device, mobilePub, devicePub, mobileNonce, deviceNonce string) string {
	return strings.Join([]string{
		"hatch-link-pairing-v" + strconv.Itoa(pairingVersion),
		"version=" + strconv.Itoa(pairingVersion),
		"initiator_role=mobile",
		"responder_role=link",
		"device_id=" + dev.deviceID(),
		"node_id=" + dev.NodeID,
		"mac=" + dev.MAC,
		"model=" + pairingModel,
		"firmware_version=" + dev.Version,
		"selected_cipher_suite=" + pairingSuite,
		"pairing_auth=" + authCommunity,
		"pairing_auth_epoch=0",
		"pairing_policy=" + policyApp,
		"confirm_timeout_seconds=0",
		"mobile_pub=" + mobilePub,
		"device_pub=" + devicePub,
		"mobile_nonce=" + mobileNonce,
		"device_nonce=" + deviceNonce,
	}, "\n")
}

// sessionKeys are named from the phone's side, as the protocol names them: the device opens what it
// receives with mobileTX and seals what it sends with mobileRX.
type sessionKeys struct {
	mobileTX  []byte
	mobileRX  []byte
	sessionID []byte
}

func sessionSecret(ecdhSecret, mobileNonce, deviceNonce, transcriptHash []byte) ([]byte, error) {
	salt := sha256.Sum256(concat(mobileNonce, deviceNonce, transcriptHash))
	return hkdf.Key(sha256.New, ecdhSecret, salt[:], recordLabel, 32)
}

func deriveSessionKeys(ecdhSecret, mobileNonce, deviceNonce, transcriptHash []byte) (sessionKeys, error) {
	secret, err := sessionSecret(ecdhSecret, mobileNonce, deviceNonce, transcriptHash)
	if err != nil {
		return sessionKeys{}, err
	}
	mobileTX, err := hkdf.Expand(sha256.New, secret, "mobile->device", 32)
	if err != nil {
		return sessionKeys{}, err
	}
	mobileRX, err := hkdf.Expand(sha256.New, secret, "device->mobile", 32)
	if err != nil {
		return sessionKeys{}, err
	}
	id := sha256.Sum256(concat([]byte(sessionIDLabel), transcriptHash, ecdhSecret))
	return sessionKeys{mobileTX: mobileTX, mobileRX: mobileRX, sessionID: id[:16]}, nil
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func recordNonce(d direction, counter uint64) []byte {
	nonce := make([]byte, 12)
	nonce[0] = byte(d)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func recordAAD(sessionID string, d direction, counter uint64) []byte {
	arrow := "m2d"
	if d == fromDevice {
		arrow = "d2m"
	}
	return []byte(recordLabel + "|" + sessionID + "|" + arrow + "|" + strconv.FormatUint(counter, 10))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// generateKey draws a P-256 key from r. ecdh.GenerateKey no longer reads the reader it is given, and
// the published vectors need the scalar fixed, so the scalar is drawn here; one outside the curve's
// range is drawn again.
func generateKey(r io.Reader) (*ecdh.PrivateKey, error) {
	scalar := make([]byte, 32)
	for range 8 {
		if _, err := io.ReadFull(r, scalar); err != nil {
			return nil, err
		}
		if key, err := ecdh.P256().NewPrivateKey(scalar); err == nil {
			return key, nil
		}
	}
	return nil, errors.New("pairing: the random source gave no usable key")
}

func (s *session) confirmed() bool {
	s.expire()
	return s.state == sessionReady || s.state == sessionProvisioning
}

func (s *session) isCurrent(gen generation) bool { return gen != 0 && gen == s.gen }

// reset ends the session and makes everything started under it stale.
func (s *session) reset() {
	s.gen++
	s.clear()
}

func (s *session) clear() {
	s.state = sessionIdle
	s.deadline = time.Time{}
	s.rx, s.tx = nil, nil
	s.id = ""
	s.rxCounter, s.txCounter = 0, 0
}

// expire drops the keys of a session that ran out of time. The generation stays, so whoever owns the
// expired session still recognizes it.
func (s *session) expire() bool {
	if s.state == sessionIdle || !s.now().After(s.deadline) {
		return false
	}
	s.clear()
	return true
}

// hello starts a session and returns the pairing_ready to send. An error means the device could not
// make its half, not that the phone sent something wrong.
func (s *session) hello(h clientHello) (pairingReady, error) {
	s.reset()
	key, err := generateKey(s.rand)
	if err != nil {
		return pairingReady{}, err
	}
	deviceNonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(s.rand, deviceNonce); err != nil {
		return pairingReady{}, err
	}
	devicePub := key.PublicKey().Bytes()
	transcript := buildTranscript(s.dev, b64url(h.pub), b64url(devicePub), b64url(h.nonce), b64url(deviceNonce))
	transcriptHash := sha256.Sum256([]byte(transcript))
	secret, err := key.ECDH(h.peer)
	if err != nil {
		return pairingReady{}, err
	}
	keys, err := deriveSessionKeys(secret, h.nonce, deviceNonce, transcriptHash[:])
	if err != nil {
		return pairingReady{}, err
	}
	rx, err := newGCM(keys.mobileTX)
	if err != nil {
		return pairingReady{}, err
	}
	tx, err := newGCM(keys.mobileRX)
	if err != nil {
		return pairingReady{}, err
	}
	s.rx, s.tx = rx, tx
	s.id = b64url(keys.sessionID)
	s.state = sessionWaitClientFinished
	s.deadline = s.now().Add(clientFinishedTimeout)
	return pairingReady{
		Type:            "pairing_ready",
		Version:         pairingVersion,
		DeviceID:        s.dev.deviceID(),
		NodeID:          s.dev.NodeID,
		MAC:             s.dev.MAC,
		Model:           pairingModel,
		FirmwareVersion: s.dev.Version,
		PairingAuth:     authCommunity,
		PairingPolicy:   policyApp,
		DevicePub:       b64url(devicePub),
		DeviceNonce:     b64url(deviceNonce),
		TranscriptHash:  b64url(transcriptHash[:]),
		SessionID:       s.id,
	}, nil
}

// open decrypts one record from the phone. Any failure (another session's record, a skipped or
// replayed counter, a bad tag, a session out of time) ends the session, as the firmware does.
func (s *session) open(r record) ([]byte, bool) {
	if s.expire() || s.state == sessionIdle || r.sessionID != s.id || r.counter != s.rxCounter {
		s.reset()
		return nil, false
	}
	sealed := concat(r.ciphertext, r.tag)
	plaintext, err := s.rx.Open(nil, recordNonce(toDevice, r.counter), sealed, recordAAD(s.id, toDevice, r.counter))
	if err != nil || !utf8.Valid(plaintext) {
		s.reset()
		return nil, false
	}
	s.rxCounter++
	return plaintext, true
}

// clientFinished confirms the session if the first record was exactly the client-finished command.
// Under confirm_app the app already collected consent, so nothing more is asked. It returns the new
// generation, or zero after ending the session.
func (s *session) clientFinished(exact bool) generation {
	if !exact || s.expire() || s.state != sessionWaitClientFinished || s.rxCounter != 1 {
		s.reset()
		return 0
	}
	s.gen++
	s.state = sessionReady
	s.deadline = s.now().Add(confirmedTimeout)
	return s.gen
}

// markProvisioning moves a confirmed session into provisioning and returns its generation. A session
// already provisioning keeps its generation, so the phone may retry after a failed Wi-Fi join.
func (s *session) markProvisioning() generation {
	if !s.expire() && s.state == sessionReady {
		s.gen++
		s.state = sessionProvisioning
		s.deadline = s.now().Add(provisioningTimeout)
	}
	if s.state == sessionProvisioning {
		return s.gen
	}
	return 0
}

// provisioningValid reports whether gen is the provisioning still under way.
func (s *session) provisioningValid(gen generation) bool {
	return s.isCurrent(gen) && !s.expire() && s.state == sessionProvisioning
}

func (s *session) extendProvisioning(gen generation) bool {
	if !s.provisioningValid(gen) {
		return false
	}
	s.deadline = s.now().Add(provisioningTimeout)
	return true
}

// seal encrypts one record for the phone. It reports false when there is no session, or when gen is
// set and is no longer current.
func (s *session) seal(plaintext []byte, gen generation) (envelope, bool) {
	if (gen != 0 && gen != s.gen) || s.expire() || s.state == sessionIdle {
		return envelope{}, false
	}
	counter := s.txCounter
	sealed := s.tx.Seal(nil, recordNonce(fromDevice, counter), plaintext, recordAAD(s.id, fromDevice, counter))
	s.txCounter++
	return envelope{
		Type:       actionEncrypted,
		SessionID:  s.id,
		Counter:    strconv.FormatUint(counter, 10),
		Ciphertext: b64url(sealed[:len(sealed)-tagBytes]),
		Tag:        b64url(sealed[len(sealed)-tagBytes:]),
	}, true
}

// sealStatus seals a status message. pairing_confirmed also carries the SDK token; apps read only type
// and status, so older ones ignore it.
func (s *session) sealStatus(st status, gen generation) (envelope, bool) {
	m := statusMessage{Type: "status", Status: st}
	if st == statusConfirmed {
		m.SDKToken = s.sdkToken
	}
	plaintext, _ := json.Marshal(m)
	return s.seal(plaintext, gen)
}
