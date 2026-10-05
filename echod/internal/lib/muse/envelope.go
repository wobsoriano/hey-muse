// Ported from Meta's Muse Gadget SDK (linux/src/musegadget/noise/envelope.py, framing.py and
// transport.py), Apache-2.0. Modified: rewritten in Go, with the four frame kinds folded into one
// type.

package muse

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

type frameKind int

const (
	frameNone     frameKind = iota
	frameRequest            // opens a stream: verb, path, headers, and the start of the body
	frameResponse           // answers one: status, headers, and the start of the body
	frameBody               // more body, either way
	frameReset              // abandons a stream
)

type header struct{ key, value string }

// frame is one message on one stream of the session. Which fields mean anything depends on kind.
type frame struct {
	stream  int64
	kind    frameKind
	verb    string // request
	path    string // request
	headers []header
	status  int // response
	data    []byte
	endBody bool
	code    int    // reset
	reason  string // reset
}

// resetCanceled is the reset code for a stream its opener gave up on.
const resetCanceled = 1

func appendHeaders(b []byte, field int, headers []header) []byte {
	for _, h := range headers {
		var hb []byte
		if h.key != "" {
			hb = appendStringField(hb, 1, h.key)
		}
		if h.value != "" {
			hb = appendStringField(hb, 2, h.value)
		}
		b = appendBytesField(b, field, hb)
	}
	return b
}

// encodeFrame writes a frame as the server's ServiceFrame message. Fields at their zero value are
// left out, as proto3 does.
func encodeFrame(f frame) []byte {
	var inner []byte
	var field int
	switch f.kind {
	case frameRequest:
		field = 2
		if f.verb != "" {
			inner = appendStringField(inner, 1, f.verb)
		}
		if f.path != "" {
			inner = appendStringField(inner, 2, f.path)
		}
		inner = appendHeaders(inner, 3, f.headers)
		if len(f.data) > 0 {
			inner = appendBytesField(inner, 4, f.data)
		}
		if f.endBody {
			inner = appendVarintField(inner, 5, 1)
		}
	case frameResponse:
		field = 3
		if f.status != 0 {
			inner = appendVarintField(inner, 1, uint64(int64(f.status)))
		}
		inner = appendHeaders(inner, 2, f.headers)
		if len(f.data) > 0 {
			inner = appendBytesField(inner, 3, f.data)
		}
		if f.endBody {
			inner = appendVarintField(inner, 4, 1)
		}
	case frameBody:
		field = 4
		if len(f.data) > 0 {
			inner = appendBytesField(inner, 1, f.data)
		}
		if f.endBody {
			inner = appendVarintField(inner, 2, 1)
		}
	case frameReset:
		field = 5
		if f.code != 0 {
			inner = appendVarintField(inner, 1, uint64(int64(f.code)))
		}
		if f.reason != "" {
			inner = appendStringField(inner, 2, f.reason)
		}
	}
	var out []byte
	if f.stream != 0 {
		out = appendVarintField(out, 1, uint64(f.stream))
	}
	if field != 0 {
		out = appendBytesField(out, field, inner)
	}
	return out
}

func decodeHeader(b []byte) (header, error) {
	var h header
	r := protoReader{b: b}
	for {
		field, wire, ok := r.next()
		if !ok {
			return h, r.err
		}
		switch field {
		case 1:
			h.key = r.string(wire)
		case 2:
			h.value = r.string(wire)
		default:
			r.skip(wire)
		}
	}
}

// decodeFrame reads a ServiceFrame. data aliases b.
func decodeFrame(b []byte) (frame, error) {
	var f frame
	r := protoReader{b: b}
	for {
		field, wire, ok := r.next()
		if !ok {
			return f, r.err
		}
		if field == 1 {
			f.stream = int64(r.uint(wire))
			continue
		}
		if field < 2 || field > 5 {
			r.skip(wire)
			continue
		}
		// The four kinds are a oneof, so a later one replaces an earlier one whole.
		f = frame{stream: f.stream, kind: frameKind(field - 1)}
		in := protoReader{b: r.bytes(wire)}
		for {
			inField, inWire, ok := in.next()
			if !ok {
				break
			}
			switch {
			case f.kind == frameRequest && inField == 1:
				f.verb = in.string(inWire)
			case f.kind == frameRequest && inField == 2:
				f.path = in.string(inWire)
			case f.kind == frameResponse && inField == 1:
				f.status = int(in.int32(inWire))
			case f.kind == frameRequest && inField == 3, f.kind == frameResponse && inField == 2:
				h, err := decodeHeader(in.bytes(inWire))
				if err != nil {
					return f, err
				}
				f.headers = append(f.headers, h)
			case f.kind == frameRequest && inField == 4, f.kind == frameResponse && inField == 3,
				f.kind == frameBody && inField == 1:
				f.data = in.bytes(inWire)
			case f.kind == frameRequest && inField == 5, f.kind == frameResponse && inField == 4,
				f.kind == frameBody && inField == 2:
				f.endBody = in.uint(inWire) != 0
			case f.kind == frameReset && inField == 1:
				f.code = int(in.int32(inWire))
			case f.kind == frameReset && inField == 2:
				f.reason = in.string(inWire)
			default:
				in.skip(inWire)
			}
		}
		if in.err != nil {
			return f, in.err
		}
	}
}

// encodeServiceRequest wraps a frame for the VM's daemon, the service every gadget stream is for
// and the one a request names by leaving the field out.
func encodeServiceRequest(f frame) []byte {
	return appendBytesField(nil, 2, encodeFrame(f))
}

// decodeServiceResponse unwraps a frame the VM sent.
func decodeServiceResponse(b []byte) (frame, error) {
	var payload []byte
	r := protoReader{b: b}
	for {
		field, wire, ok := r.next()
		if !ok {
			break
		}
		if field == 1 {
			payload = r.bytes(wire)
		} else {
			r.skip(wire)
		}
	}
	if r.err != nil {
		return frame{}, r.err
	}
	if len(payload) == 0 {
		return frame{}, errors.New("muse: empty response envelope")
	}
	f, err := decodeFrame(payload)
	if err != nil {
		return frame{}, err
	}
	if f.kind == frameRequest {
		return frame{}, errors.New("muse: the VM sent a request")
	}
	return f, nil
}

const (
	// maxChunkPayload leaves room in a 64 KB Noise message for the chunk's own fields and the tag.
	maxChunkPayload    = 65489
	maxTotalChunks     = 256
	maxPendingAssembly = 16
	maxAssemblyBytes   = 16 << 20
	assemblyTTL        = 60 * time.Second
)

// chunk cuts one envelope into the pieces that each fit a Noise message. Every piece names the
// envelope with the same random id, so the far side can put them back together.
func chunk(envelope []byte) ([][]byte, error) {
	total := max(1, (len(envelope)+maxChunkPayload-1)/maxChunkPayload)
	if total > maxTotalChunks {
		return nil, fmt.Errorf("muse: %d bytes is too large to send", len(envelope))
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	out := make([][]byte, 0, total)
	for i := range total {
		piece := envelope[i*maxChunkPayload : min(len(envelope), (i+1)*maxChunkPayload)]
		var b []byte
		if v := binary.LittleEndian.Uint64(id[:]); v != 0 {
			b = appendVarintField(b, 1, v)
		}
		if i != 0 {
			b = appendVarintField(b, 2, uint64(i))
		}
		b = appendVarintField(b, 3, uint64(total))
		if len(piece) > 0 {
			b = appendBytesField(b, 4, piece)
		}
		out = append(out, b)
	}
	return out, nil
}

type assembly struct {
	pieces  map[uint32][]byte
	total   uint32
	bytes   int
	created time.Time
}

// assembler puts chunked envelopes back together. Any malformed chunk is the end of it, since the
// stream it belonged to can no longer be trusted to be whole.
type assembler struct {
	pending  map[int64]*assembly
	poisoned bool
}

// add takes one decrypted chunk and returns the envelope it completes, or nil if more are due.
func (a *assembler) add(b []byte) ([]byte, error) {
	if a.poisoned {
		return nil, errors.New("muse: chunk after a malformed one")
	}
	out, err := a.addChunk(b)
	if err != nil {
		a.poisoned = true
	}
	return out, err
}

func (a *assembler) addChunk(b []byte) ([]byte, error) {
	var id int64
	var index uint32
	var total uint32 = 1
	var payload []byte
	r := protoReader{b: b}
	for {
		field, wire, ok := r.next()
		if !ok {
			break
		}
		switch field {
		case 1:
			id = int64(r.uint(wire))
		case 2:
			index = r.uint32(wire)
		case 3:
			total = r.uint32(wire)
		case 4:
			payload = r.bytes(wire)
		default:
			r.skip(wire)
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if total < 1 || total > maxTotalChunks || index >= total || len(payload) > maxChunkPayload {
		return nil, errors.New("muse: chunk out of range")
	}
	if total == 1 {
		return payload, nil
	}

	now := time.Now()
	for key, as := range a.pending {
		if now.Sub(as.created) > assemblyTTL {
			delete(a.pending, key)
		}
	}
	as := a.pending[id]
	if as == nil {
		if len(a.pending) >= maxPendingAssembly {
			return nil, errors.New("muse: too many unfinished messages")
		}
		if a.pending == nil {
			a.pending = map[int64]*assembly{}
		}
		as = &assembly{pieces: map[uint32][]byte{}, total: total, created: now}
		a.pending[id] = as
	}
	if _, dup := as.pieces[index]; dup || as.total != total {
		return nil, errors.New("muse: chunks disagree")
	}
	as.bytes += len(payload)
	if as.bytes > maxAssemblyBytes {
		return nil, errors.New("muse: message too large")
	}
	as.pieces[index] = payload
	if uint32(len(as.pieces)) < total {
		return nil, nil
	}
	delete(a.pending, id)
	out := make([]byte, 0, as.bytes)
	for i := range total {
		out = append(out, as.pieces[i]...)
	}
	return out, nil
}
