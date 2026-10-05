// Ported from linux/src/musegadget/noise/_proto.py in Meta's Muse Gadget SDK, Copyright (c) Meta
// Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (LICENSE in this
// directory). Modified: rewritten in Go, keeping only the wire types the Muse messages use.

package muse

import (
	"errors"
	"unicode/utf8"
)

const (
	wireVarint    = 0
	wireFixed64   = 1
	wireDelimited = 2
	wireFixed32   = 5
)

var errProto = errors.New("muse: malformed message")

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendVarintField(b []byte, field int, v uint64) []byte {
	return appendVarint(appendVarint(b, uint64(field)<<3|wireVarint), v)
}

func appendBytesField(b []byte, field int, v []byte) []byte {
	b = appendVarint(appendVarint(b, uint64(field)<<3|wireDelimited), uint64(len(v)))
	return append(b, v...)
}

func appendStringField(b []byte, field int, v string) []byte {
	b = appendVarint(appendVarint(b, uint64(field)<<3|wireDelimited), uint64(len(v)))
	return append(b, v...)
}

// protoReader walks the fields of one message. The first malformed byte sets err and ends the
// walk, so a decoder is a loop over next and one check of err after it.
type protoReader struct {
	b   []byte
	err error
}

func (r *protoReader) varint() uint64 {
	var v uint64
	for i := 0; i < 10; i++ {
		if len(r.b) == 0 {
			r.err = errProto
			return 0
		}
		c := r.b[0]
		r.b = r.b[1:]
		if i == 9 && c > 1 {
			r.err = errProto
			return 0
		}
		v |= uint64(c&0x7F) << (7 * i)
		if c < 0x80 {
			return v
		}
	}
	r.err = errProto
	return 0
}

func (r *protoReader) take(n uint64) []byte {
	if n > uint64(len(r.b)) {
		r.err = errProto
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// next returns the next field's number and wire type, or ok false at the end or on an error.
func (r *protoReader) next() (field int, wire int, ok bool) {
	if r.err != nil || len(r.b) == 0 {
		return 0, 0, false
	}
	key := r.varint()
	if r.err != nil {
		return 0, 0, false
	}
	n := key >> 3
	if n == 0 || n > 1<<29-1 || (n >= 19000 && n <= 19999) {
		r.err = errProto
		return 0, 0, false
	}
	return int(n), int(key & 7), true
}

// uint reads a varint field's value, and fails the walk if the field is anything else.
func (r *protoReader) uint(wire int) uint64 {
	if wire != wireVarint {
		r.err = errProto
		return 0
	}
	return r.varint()
}

// bytes reads a length-delimited field's value. It aliases the message.
func (r *protoReader) bytes(wire int) []byte {
	if wire != wireDelimited {
		r.err = errProto
		return nil
	}
	return r.take(r.varint())
}

func (r *protoReader) string(wire int) string {
	v := r.bytes(wire)
	if !utf8.Valid(v) {
		r.err = errProto
		return ""
	}
	return string(v)
}

// skip passes over a field this code has no use for, which is how newer servers stay compatible.
func (r *protoReader) skip(wire int) {
	switch wire {
	case wireVarint:
		r.varint()
	case wireFixed64:
		r.take(8)
	case wireDelimited:
		r.take(r.varint())
	case wireFixed32:
		r.take(4)
	default:
		r.err = errProto
	}
}

// int32 narrows a varint that carries a signed 32-bit value, which the wire sign-extends to 64.
func (r *protoReader) int32(wire int) int32 {
	v := int64(r.uint(wire))
	if v < -1<<31 || v > 1<<31-1 {
		r.err = errProto
		return 0
	}
	return int32(v)
}

func (r *protoReader) uint32(wire int) uint32 {
	v := r.uint(wire)
	if v > 0xFFFFFFFF {
		r.err = errProto
		return 0
	}
	return uint32(v)
}
