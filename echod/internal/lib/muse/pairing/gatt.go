// This file is a modified port of linux/src/musegadget/ble_server.py and identity.py from Meta's Muse
// Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License,
// Version 2.0. Only the GATT shape is kept; the BlueZ server itself is not here.

package pairing

import "time"

// The setup service. The phone writes commands to RX and subscribes to TX for the answers.
const (
	ServiceUUID = "7fdd3d1c-38ea-46cf-8b46-314ecf5f240c"
	RXUUID      = "4d593029-28a2-4a6e-a1f0-3c2d5e8f9b01"
	TXUUID      = "d75dc4ca-7b2b-4e9c-8f0a-1d2e3f4a5b6c"
)

// The characteristic flags, as BlueZ names them.
var (
	RXFlags = []string{"write", "write-without-response"}
	TXFlags = []string{"read", "notify"}
)

// The advertisement carries the service UUID, the BLE name, and this manufacturer data, which the apps
// read as the device's paired flag. 0xFFFF is the unassigned company id.
const (
	ManufacturerID   uint16 = 0xFFFF
	ManufacturerData byte   = 0x00
)

// The names the apps expect. They compare what follows BLENamePrefix with what follows NodeIDPrefix,
// so the BLE name has no separator.
const (
	NodeIDPrefix  = "homelink-"
	BLENamePrefix = "MuseGadget"
)

const (
	// AssumedMTU stands in until the stack reports the negotiated one: phones running the Muse app
	// negotiate at least enough for full packets. Android writes MTU-3 bytes uncapped, so BlueZ needs
	// ExchangeMTU = 256 in main.conf or setup stalls after the Wi-Fi step.
	AssumedMTU = MaxPacket + 3

	// ChunkStagger is the pause the SDK's server leaves between the packets of one Send.
	ChunkStagger = 50 * time.Millisecond

	// ShutdownGrace is how long to keep the GATT server up after Run returns nil, so the final
	// auth_ok reaches the phone.
	ShutdownGrace = 1500 * time.Millisecond

	// DefaultWindow is how long setup stays open.
	DefaultWindow = 10 * time.Minute
)

// Transport is the GATT link to the phone. Implementations: BlueZ on the device, a fake in tests.
type Transport interface {
	// Send notifies each packet on the TX characteristic, in order.
	Send(packets [][]byte) error
	// MTU is the negotiated ATT MTU of the current connection.
	MTU() int
	// Disconnect drops the phone.
	Disconnect() error
}
