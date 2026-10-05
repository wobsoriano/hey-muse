// This file is a modified port of linux/src/musegadget/ble_framing.py from Meta's Muse Gadget SDK,
// Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0.

package pairing

import "fmt"

// Both directions use one frame: 0xFE, chunk index, chunk count, then a piece of the message. A write
// or notification that does not start with 0xFE is a whole message on its own.
const (
	chunkMagic  = 0xFE
	headerBytes = 3
	maxChunks   = 255

	// MaxPacket is the most one notification carries, whatever the MTU.
	MaxPacket = 160
	// MaxMessage is the most a reassembled message may hold.
	MaxMessage = 8192
	// DefaultMTU is what ATT gives before anything is negotiated.
	DefaultMTU = 23
)

// EncodeChunks splits data into framed packets that each fit one notification at mtu.
func EncodeChunks(data []byte, mtu int) ([][]byte, error) {
	room := 20
	if mtu > 3 {
		room = mtu - 3
	}
	usable := min(room, MaxPacket) - headerBytes
	total := max(1, (len(data)+usable-1)/usable)
	if total > maxChunks {
		return nil, fmt.Errorf("pairing: a message of %d bytes needs %d chunks, more than %d", len(data), total, maxChunks)
	}
	packets := make([][]byte, total)
	for i := range packets {
		piece := data[min(i*usable, len(data)):min((i+1)*usable, len(data))]
		packets[i] = append([]byte{chunkMagic, byte(i), byte(total)}, piece...)
	}
	return packets, nil
}

// Assembler puts chunked writes back together, strictly in order. Index 0, or a different count,
// starts a new message; a chunk out of order or a message over MaxMessage throws away what was
// collected.
type Assembler struct {
	buf   []byte
	total int
	next  int
}

// Reset forgets a message that is part way in.
func (a *Assembler) Reset() { a.buf, a.total, a.next = nil, 0, 0 }

// Feed takes one write and returns a message once one is whole.
func (a *Assembler) Feed(packet []byte) ([]byte, bool) {
	if len(packet) < headerBytes || packet[0] != chunkMagic {
		return packet, true
	}
	index, total, piece := int(packet[1]), int(packet[2]), packet[headerBytes:]
	if total == 0 {
		a.Reset()
		return nil, false
	}
	if index == 0 || total != a.total {
		a.Reset()
		a.total = total
	}
	if index != a.next || index >= a.total || len(a.buf)+len(piece) > MaxMessage {
		a.Reset()
		return nil, false
	}
	a.buf = append(a.buf, piece...)
	a.next = index + 1
	if a.next < a.total {
		return nil, false
	}
	message := a.buf
	a.Reset()
	if message == nil {
		message = []byte{}
	}
	return message, true
}
