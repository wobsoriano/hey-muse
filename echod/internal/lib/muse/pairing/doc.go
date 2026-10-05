// Package pairing is the device side of Muse gadget Bluetooth setup: community pairing version 5 and
// the conversation the Muse app holds over two GATT characteristics until the device has its tokens.
//
// The phone writes commands to RX and reads answers as notifications on TX. A message longer than one
// packet travels in chunks (EncodeChunks, Assembler). The phone reads get_device_info, opens an
// encrypted session (pairing_client_hello, pairing_client_finished), asks for a Wi-Fi scan, and sends
// provision_v2 with the device tokens. The Controller checks them, hands them to the caller to save,
// and tells the phone auth_ok only once they are saved.
//
// Community pairing keeps setup secrets from someone listening. It does not prove who the phone is, so
// it cannot stop someone in the middle; the device is only open to it while Run is running.
//
// A Controller is driven by a Transport, which owns the GATT server. This package holds no Bluetooth
// code and builds everywhere.
package pairing
