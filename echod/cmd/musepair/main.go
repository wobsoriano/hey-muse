//go:build linux

// Command musepair runs one Muse gadget pairing on the device: it advertises the setup service
// through bluetoothd, holds the conversation with the Muse app, and writes the pairing to a file.
//
//	musepair -sdk-token-file token.txt -out /data/muse-dev/state.json
//
// The output is the JSON of a muse.State, which musecheck reads. It is written only once Muse has
// accepted the tokens, and never partly. The adapter's name and modes are put back on the way out,
// whether setup finished, failed, or was interrupted.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/bluez"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

var _ pairing.Transport = (*bluez.Peripheral)(nil)

const version = "musepair"

func main() {
	level := slog.LevelInfo
	if os.Getenv("MUSEPAIR_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		slog.Error("musepair: failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("musepair", flag.ContinueOnError)
	tokenFile := fs.String("sdk-token-file", "", "file holding the mgst_ SDK token from gadgets.muse.ai")
	out := fs.String("out", "", "where to write the pairing, as the JSON of a muse.State")
	nodeID := fs.String("node-id", "", "node id, homelink- plus six hex digits (default: a fresh one)")
	bleName := fs.String("ble-name", "", "BLE name, MuseGadget plus the same six digits in upper case (default: from the node id)")
	window := fs.Duration("window", pairing.DefaultWindow, "how long setup stays open")
	noVerify := fs.Bool("no-verify", false, "accept the tokens without asking Muse; for radio tests only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tokenFile == "" || *out == "" {
		return errors.New("-sdk-token-file and -out are required")
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	sdkToken := strings.TrimSpace(string(raw))
	if sdkToken == "" {
		return fmt.Errorf("%s is empty", *tokenFile)
	}
	device, err := identity(*nodeID, *bleName)
	if err != nil {
		return err
	}

	radio := bluez.NewPeripheral(bluez.PeripheralConfig{
		LocalName:        device.BLEName,
		ServiceUUID:      pairing.ServiceUUID,
		RXUUID:           pairing.RXUUID,
		TXUUID:           pairing.TXUUID,
		RXFlags:          pairing.RXFlags,
		TXFlags:          pairing.TXFlags,
		ManufacturerData: map[uint16][]byte{pairing.ManufacturerID: {pairing.ManufacturerData}},
		AssumedMTU:       pairing.AssumedMTU,
		Stagger:          pairing.ChunkStagger,
	})
	cfg := pairing.Config{
		Device:    device,
		Transport: radio,
		SDKToken:  sdkToken,
		Network:   loggedNetwork{pairing.AlreadyOnline{}},
		Save:      func(r pairing.Result) error { return save(*out, device, r, time.Now()) },
		Window:    *window,
		Progress: func(p pairing.Progress) {
			attrs := []any{"state", p.State.String()}
			if p.Reason != pairing.ReasonNone {
				attrs = append(attrs, "reason", p.Reason.String())
			}
			if p.Err != nil {
				attrs = append(attrs, "err", p.Err)
			}
			slog.Info("musepair: setup", attrs...)
		},
	}
	if *noVerify {
		slog.Warn("musepair: -no-verify: tokens will be saved without asking Muse")
		cfg.Verify = func(context.Context, pairing.Result) error { return nil }
	}
	ctl, err := pairing.New(cfg)
	if err != nil {
		return err
	}

	slog.Info("musepair: starting", "node_id", device.NodeID, "ble_name", device.BLEName, "window", *window, "out", *out)
	// Close also runs when Start fails partway, so nothing stays registered.
	defer func() {
		if err := radio.Close(); err != nil {
			slog.Warn("musepair: bluetooth teardown", "err", err)
		}
	}()
	if err := radio.Start(ctx, ctl); err != nil {
		return err
	}
	if err := ctl.Run(ctx); err != nil {
		return err
	}
	slog.Info("musepair: paired", "out", *out)
	// The final auth_ok is still on its way to the phone.
	time.Sleep(pairing.ShutdownGrace)
	return nil
}

var nodeSuffix = regexp.MustCompile(`^[0-9a-f]{6}$`)

// identity is who the device says it is. A muse.State keeps only the MAC and derives both names from
// its last six digits, so given names must be ones a MAC can stand for.
func identity(nodeID, bleName string) (pairing.Device, error) {
	var mac [6]byte
	if _, err := rand.Read(mac[:]); err != nil {
		return pairing.Device{}, err
	}
	// Unicast and locally administered, so it can never be mistaken for a vendor's address.
	mac[0] = mac[0]&0xFC | 0x02

	if nodeID == "" && bleName != "" {
		suffix, ok := strings.CutPrefix(bleName, pairing.BLENamePrefix)
		if !ok {
			return pairing.Device{}, fmt.Errorf("-ble-name %q does not start with %q", bleName, pairing.BLENamePrefix)
		}
		nodeID = pairing.NodeIDPrefix + strings.ToLower(suffix)
	}
	if nodeID != "" {
		suffix, ok := strings.CutPrefix(nodeID, pairing.NodeIDPrefix)
		if !ok || !nodeSuffix.MatchString(suffix) {
			return pairing.Device{}, fmt.Errorf("-node-id %q is not %s plus six lower-case hex digits", nodeID, pairing.NodeIDPrefix)
		}
		if _, err := hex.Decode(mac[3:], []byte(suffix)); err != nil {
			return pairing.Device{}, err
		}
	}
	suffix := hex.EncodeToString(mac[3:])
	d := pairing.Device{
		NodeID:  pairing.NodeIDPrefix + suffix,
		BLEName: pairing.BLENamePrefix + strings.ToUpper(suffix),
		MAC:     fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]),
		Version: version,
	}
	if bleName != "" && bleName != d.BLEName {
		return pairing.Device{}, fmt.Errorf("-ble-name %q does not go with node id %q (want %q)", bleName, d.NodeID, d.BLEName)
	}
	return d, nil
}

// state is the JSON of a muse.State for a device that has just paired. It is spelled out here so this
// command builds without the client.
type state struct {
	Identity struct {
		MAC string `json:"mac"`
	} `json:"identity"`
	Credentials credentials `json:"credentials"`
}

type credentials struct {
	pairing.Result
	AccessTokenSavedAt int64 `json:"access_token_saved_at"` // Unix seconds
}

// save writes beside the file and renames over it, so a crash cannot leave half a pairing, and does
// not return until the new file would survive a power cut: the phone is told auth_ok on its word.
func save(path string, d pairing.Device, r pairing.Result, now time.Time) error {
	var st state
	st.Identity.MAC = d.MAC
	st.Credentials = credentials{Result: r, AccessTokenSavedAt: now.Unix()}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp makes the file 0600, which is what tokens want.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

// loggedNetwork says in the log what the phone was offered and asked for; the Controller reports
// only changes of State, and the Wi-Fi steps are not one.
type loggedNetwork struct{ pairing.Network }

func (n loggedNetwork) Scan(ctx context.Context) ([]pairing.WiFiNetwork, error) {
	networks, err := n.Network.Scan(ctx)
	if err != nil {
		slog.Warn("musepair: wifi scan", "err", err)
	} else {
		slog.Info("musepair: answering the phone's wifi scan", "networks", len(networks))
	}
	return networks, err
}

func (n loggedNetwork) Join(ctx context.Context, ssid, password string) error {
	err := n.Network.Join(ctx, ssid, password)
	slog.Info("musepair: phone chose a network", "online", err == nil)
	return err
}
