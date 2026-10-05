"""The phone's side of setup, built from the Muse Gadget SDK's own pairing and framing functions.

Run by interop_test.go as: sdk_phone.py <sdk linux/src> <mtu>. It prints READY once the SDK is
imported, then one "W <hex>" line per GATT write, and reads one "N <hex>" line per notification. It
ends with DONE, or exits nonzero with the reason on stderr.
"""

import hashlib
import json
import os
import sys

sys.path.insert(0, sys.argv[1])
MTU = int(sys.argv[2])

from cryptography.hazmat.primitives import serialization  # noqa: E402
from cryptography.hazmat.primitives.asymmetric import ec  # noqa: E402
from cryptography.hazmat.primitives.ciphers.aead import AESGCM  # noqa: E402

from musegadget import pairing  # noqa: E402
from musegadget.ble_framing import ChunkAssembler, encode_chunks  # noqa: E402

print("READY", flush=True)

assembler = ChunkAssembler()


def send(command):
    for packet in encode_chunks(json.dumps(command).encode(), MTU):
        print("W", packet.hex(), flush=True)


def receive():
    while True:
        line = sys.stdin.readline()
        if not line:
            sys.exit("the device went away")
        message = assembler.feed(bytes.fromhex(line.split()[1]))
        if message is not None:
            return message


def expect(got, want):
    if got != want:
        sys.exit(f"got {got!r}, want {want!r}")


send({"action": "get_device_info"})
info = json.loads(receive())
expect((info["type"], info["model"], info["pairing_protocol"], info["pairing_auth"], info["pairing_policy"]),
       ("device_info", pairing.PAIRING_MODEL, pairing.PAIRING_VERSION, pairing.AUTH_COMMUNITY, pairing.POLICY_APP))

send({"action": "wifi_scan"})
expect(receive(), b"error_encryption_required")

key = ec.generate_private_key(ec.SECP256R1())
public = key.public_key().public_bytes(serialization.Encoding.X962, serialization.PublicFormat.UncompressedPoint)
nonce = os.urandom(16)
send({
    "action": "pairing_client_hello", "version": pairing.PAIRING_VERSION,
    "pairing_auth": pairing.AUTH_COMMUNITY, "pairing_policy": pairing.POLICY_APP,
    "mobile_pub": pairing.b64url_encode(public), "mobile_nonce": pairing.b64url_encode(nonce),
})
ready = json.loads(receive())
expect(ready["type"], "pairing_ready")
expect((ready["device_id"], ready["node_id"], ready["mac"]), (info["device_id"], info["node_id"], info["mac"]))
transcript = pairing.build_transcript(
    community=True, auth_epoch=0, policy=pairing.POLICY_APP,
    device_id=ready["device_id"], node_id=ready["node_id"], mac=ready["mac"],
    firmware_version=ready["firmware_version"],
    mobile_pub=pairing.b64url_encode(public), device_pub=ready["device_pub"],
    mobile_nonce=pairing.b64url_encode(nonce), device_nonce=ready["device_nonce"],
)
transcript_hash = hashlib.sha256(transcript.encode()).digest()
expect(ready["transcript_hash"], pairing.b64url_encode(transcript_hash))
peer = ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), pairing.b64url_decode(ready["device_pub"]))
tx_key, rx_key, session_id = pairing.derive_session_keys(
    key.exchange(ec.ECDH(), peer), nonce, pairing.b64url_decode(ready["device_nonce"]), transcript_hash,
)
session = pairing.b64url_encode(session_id)
expect(ready["session_id"], session)
tx, rx = AESGCM(tx_key), AESGCM(rx_key)
counters = {"tx": 0, "rx": 0}


def seal(command):
    counter = counters["tx"]
    counters["tx"] += 1
    sealed = tx.encrypt(pairing.record_nonce(0, counter), json.dumps(command).encode(),
                        pairing.record_aad(session, 0, counter))
    return {"action": "pairing_encrypted", "session_id": session, "counter": str(counter),
            "ciphertext": pairing.b64url_encode(sealed[:-16]), "tag": pairing.b64url_encode(sealed[-16:])}


def receive_record():
    envelope = json.loads(receive())
    counter = counters["rx"]
    counters["rx"] += 1
    expect((envelope["type"], envelope["session_id"], envelope["counter"]), ("pairing_encrypted", session, str(counter)))
    sealed = pairing.b64url_decode(envelope["ciphertext"], 16384) + pairing.b64url_decode(envelope["tag"])
    return json.loads(rx.decrypt(pairing.record_nonce(1, counter), sealed, pairing.record_aad(session, 1, counter)))


send(seal({"action": "pairing_client_finished"}))
expect(receive_record(), {"type": "status", "status": "pairing_confirmed", "sdk_token": "mgst_interop"})

send(seal({"action": "wifi_scan"}))
expect(receive_record(), {"type": "wifi_scan_result", "networks": [{"ssid": "HomeNet", "rssi": -40, "secure": False}]})

send(seal({
    "action": "provision_v2", "ssid": "HomeNet", "password": "",
    "access_token": "interop-access", "refresh_token": "interop-refresh", "token_type": "device",
    "username": "someone", "api_url": "https://legacy-api.example", "api_url_v2": "https://api.example",
    "noise_host": "noise.example",
}))
for status in ("wifi_connecting", "wifi_connected", "auth_ok"):
    expect(receive_record(), {"type": "status", "status": status})

print("DONE", flush=True)
