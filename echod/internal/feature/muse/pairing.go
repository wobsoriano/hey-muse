package gadget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
	"github.com/HuskerMinion/techo5/echod/internal/lib/wifi"
)

// Handler is what a peripheral hands the phone's side of the link to: each write to the RX
// characteristic, and the phone going away. The pairing Controller is one.
type Handler interface {
	OnWrite(data []byte)
	OnDisconnect()
}

// openPeripheral puts the Muse setup service on the air, and is nil on a build with no Bluetooth
// peripheral, which StartPairing then says.
//
// What it has to do: advertise name, with pairing.ServiceUUID among the service UUIDs and
// pairing.ManufacturerID carrying pairing.ManufacturerData, and serve a GATT service of that UUID
// with two characteristics: RX (pairing.RXUUID, pairing.RXFlags), each write to which goes to
// h.OnWrite whole, and TX (pairing.TXUUID, pairing.TXFlags), which notifies the packets that
// Transport.Send is given, in order, pairing.ChunkStagger apart. h.OnDisconnect is called when the
// phone drops, and Transport.MTU is the negotiated MTU of the connection there is, or
// pairing.AssumedMTU before one. h is never called from inside Send or Disconnect.
//
// It returns once advertising, with the close that takes the advertisement and the service down and
// puts the adapter back as it was. close is called exactly once, after the Controller has returned,
// and has to work when ctx has already ended. ctx ending should also drop the phone.
var openPeripheral func(ctx context.Context, name string, h Handler) (pairing.Transport, func(), error)

// errNoPeripheral is StartPairing on a build with no way to reach the Muse app.
var errNoPeripheral = errors.New("pairing with the Muse app is not available on this build")

// openPairing is the pairing that is open, if one is.
type openPairing struct {
	cancel context.CancelFunc
	// done closes once the Controller has returned and the peripheral is down.
	done chan struct{}
}

// link is the Transport the Controller is made with before the peripheral exists: the Controller
// only drives it from Run, which starts after the peripheral has answered.
type link struct{ pairing.Transport }

// StartPairing puts the device on the air for the Muse app, for pairing.DefaultWindow or until
// CancelPairing. A pairing already open is left to run. The client is stopped meanwhile: one
// identity holds one session, and the tokens it holds are about to be replaced.
func (f *Feature) StartPairing() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case openPeripheral == nil:
		return errNoPeripheral
	case f.ctx == nil:
		return errors.New("muse: not running")
	case f.pair != nil:
		return nil
	}
	sdk := config.Get().Brain.Muse.SDKToken
	if sdk == "" {
		return errors.New("save the Muse SDK token first: the app will not pair without one")
	}
	// The identity has to be on disk before it is advertised: the app remembers the device by it,
	// and a device that came back from a restart as somebody else would never be found again.
	if !f.st.Identity.Valid() {
		id, err := muse.NewIdentity()
		if err != nil {
			return err
		}
		st := f.st
		st.Identity = id
		if err := f.saveLocked(st); err != nil {
			return fmt.Errorf("could not keep the device's identity: %w", err)
		}
	}
	id := f.st.Identity

	ctx, cancel := context.WithCancel(f.ctx)
	l := &link{}
	ctrl, err := pairing.New(pairing.Config{
		Device:    pairing.Device{NodeID: id.NodeID(), BLEName: id.BLEName(), MAC: id.MAC, Version: layout.Version},
		Transport: l,
		SDKToken:  sdk,
		Network:   pairing.AlreadyOnline{SSID: currentSSID},
		Save:      f.savePairing,
		Progress:  f.progressed,
	})
	if err != nil {
		cancel()
		return err
	}
	t, closePeripheral, err := openPeripheral(ctx, id.BLEName(), ctrl)
	if err != nil {
		cancel()
		return fmt.Errorf("could not advertise to the Muse app: %w", err)
	}
	l.Transport = t

	p := &openPairing{cancel: cancel, done: make(chan struct{})}
	f.pair = p
	f.progress = pairing.Progress{}
	f.err = nil
	f.reconcileLocked()
	slog.Info("muse pairing: open", "name", id.BLEName(), "for", pairing.DefaultWindow)

	safe.Go("muse pairing", func() {
		defer close(p.done)
		err := ctrl.Run(ctx)
		if err == nil {
			// The phone is still reading auth_ok.
			time.Sleep(pairing.ShutdownGrace)
		}
		closePeripheral()
		cancel()
		f.mu.Lock()
		if f.pair == p {
			f.pair = nil
		}
		f.reconcileLocked()
		f.mu.Unlock()
		if err != nil {
			slog.Info("muse pairing: closed", "err", err)
		}
		f.Changed.Emit(struct{}{})
	})
	return nil
}

// CancelPairing takes the device off the air. Nothing happens when no pairing is open.
func (f *Feature) CancelPairing() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pair != nil {
		f.pair.cancel()
	}
}

// Unpair forgets the tokens and keeps the identity, so the device pairs again as itself. Muse is
// not told: the app's device list is where the owner removes it.
func (f *Feature) Unpair() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pair != nil {
		f.pair.cancel()
	}
	if err := f.saveLocked(muse.State{Identity: f.st.Identity}); err != nil {
		return err
	}
	f.err = nil
	f.reconcileLocked()
	slog.Info("muse: unpaired")
	return nil
}

// savePairing is the Controller's Save: the tokens the app sent, stamped the way the SDK stamps
// them, written before the phone is told auth_ok. The client starts once the Controller has
// returned and the peripheral is down.
func (f *Feature) savePairing(r pairing.Result) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	st.Credentials = &muse.Credentials{
		AccessToken:        r.AccessToken,
		RefreshToken:       r.RefreshToken,
		TokenType:          r.TokenType,
		Username:           r.Username,
		APIURL:             r.APIURL,
		APIURLV2:           r.APIURLV2,
		NoiseHost:          r.NoiseHost,
		AccessTokenSavedAt: time.Now().Unix(),
	}
	// A fresh pairing has told Muse nothing yet.
	st.SDKTokenReported = false
	return f.saveLocked(st)
}

func (f *Feature) progressed(p pairing.Progress) {
	f.mu.Lock()
	f.progress = p
	f.mu.Unlock()
	f.Changed.Emit(struct{}{})
}

// currentSSID is the network the device is on, for the app to preselect when the phone is on the
// same one.
func currentSSID() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return wifi.Current(ctx).SSID
}
