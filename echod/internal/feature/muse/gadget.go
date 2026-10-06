// Package gadget is the device as a Muse gadget: paired with somebody's Muse through the Muse app,
// and connected to it whenever Muse is what answers the voice turns (config.BrainMuse). It is the
// one owner of the connection and of the pairing, which it keeps in its own file. The turn itself
// is feature/voice/muse.go; what Muse may do on the device is feature/assistant's to say.
//
// The package is not called muse because lib/muse is, and the two meet in every file that uses
// both.
package gadget

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"sync"

	"github.com/HuskerMinion/techo5/echod/internal/component"
	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
	"github.com/HuskerMinion/techo5/echod/internal/lib/hook"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

func init() {
	component.Register(component.Network, Get(), component.Order(68))
}

// StatePath is where the pairing lives: the identity, the device tokens and whether the SDK token
// has been reported. Not state.json, which the diagnostics bundle summarizes and other features
// write; this file holds nothing but Muse's and is read by nothing but this package.
const StatePath = layout.StateDir + "/muse.json"

// Phase is where the device stands with Muse. It is the whole of the feature's state: being paired,
// pairing, connecting and being online are phases rather than flags, and reconcile is the one place
// that moves between them.
type Phase int

const (
	// Unpaired: no credentials, so nothing runs until the Muse app pairs the device.
	Unpaired Phase = iota
	// Pairing: the setup service is on the air and a phone may be talking to it.
	Pairing
	// Standby: paired, but Muse is not what answers, so nothing is connected.
	Standby
	// Connecting: reaching Muse.
	Connecting
	// Online: registered with Muse, so Ask works and commands arrive.
	Online
	// Offline: paired and chosen, and between tries. Status.Err says why.
	Offline
)

func (p Phase) String() string {
	switch p {
	case Pairing:
		return "pairing"
	case Standby:
		return "standby"
	case Connecting:
		return "connecting"
	case Online:
		return "online"
	case Offline:
		return "offline"
	}
	return "unpaired"
}

// Status is what a page shows about the device and Muse.
type Status struct {
	Phase Phase
	// Username is who the device is paired as, once it is.
	Username string
	// BLEName is what the Muse app lists the device as while pairing, like MuseGadgetA1B2C3, once the
	// device has an identity.
	BLEName string
	// Err is why the device is Offline, or what went wrong reading the pairing. Never a token.
	Err error
	// Progress is how far the last pairing came: the open one in Pairing, otherwise the one before,
	// so that a failure can still be read after the window has closed.
	Progress pairing.Progress
	// CanPair is whether this build can put the setup service on the air at all.
	CanPair bool
}

// client is what Run drives: *muse.Client, or a test's stand-in.
type client interface {
	Run(ctx context.Context) error
	Ask(ctx context.Context, in muse.AskInput, on func(muse.ReplyEvent)) (muse.Reply, error)
}

// newClient makes the client. A test puts a fake here.
var newClient = func(cfg muse.Config) (client, error) { return muse.New(cfg) }

// plan is what a client is built from. Two clients built from equal plans are the same client, so
// reconcile compares the running one's plan with what the settings say now rather than remembering
// what changed.
type plan struct {
	sdkToken string
	// commands counts SetCommands calls: the list itself is not comparable.
	commands int
}

// running is the client that is up, if one is. stopping is set once it has been told to stop and
// stays set until Run has returned: no new client starts until then, so one identity never holds
// two sessions, and a save from a client on its way out cannot land over a newer state.
type running struct {
	plan     plan
	c        client
	stop     context.CancelFunc
	stopping bool
	// done closes once Run has returned and the feature has been reconciled without it.
	done chan struct{}
}

type Feature struct {
	store muse.FileStore

	// Changed fires on every change of Status, on whichever goroutine made it, so listeners must not
	// block.
	Changed hook.Hook[struct{}]

	mu sync.Mutex
	// ctx is Run's while it runs; the client and a pairing live under it.
	ctx context.Context
	// st is the file as last read or written. Every write goes through saveLocked so it is never
	// stale.
	st       muse.State
	phase    Phase
	err      error
	progress pairing.Progress

	commands []muse.Command
	gen      int

	client *running
	pair   *openPairing
}

var (
	once   sync.Once
	shared *Feature
)

func Get() *Feature {
	once.Do(func() { shared = build(StatePath) })
	return shared
}

// build is the feature on its own file, for a test.
func build(path string) *Feature {
	return &Feature{store: muse.FileStore{Path: path}}
}

func (f *Feature) Name() string { return "muse" }

// Run reads the pairing, keeps the client up while it should be, and takes everything down with ctx.
func (f *Feature) Run(ctx context.Context) error {
	f.mu.Lock()
	f.ctx = ctx
	st, err := f.store.Load()
	switch {
	case err == nil:
		f.st = st
	case errors.Is(err, fs.ErrNotExist):
	default:
		slog.Error("muse: could not read the pairing", "err", err)
		f.err = err
	}
	f.reconcileLocked()
	f.mu.Unlock()
	f.Changed.Emit(struct{}{})

	<-ctx.Done()
	f.mu.Lock()
	f.ctx = nil
	pair, client := f.pair, f.client
	if pair != nil {
		pair.cancel()
	}
	f.reconcileLocked()
	f.mu.Unlock()
	// Not back to the supervisor until nothing of this run is left running.
	if pair != nil {
		<-pair.done
	}
	if client != nil {
		<-client.done
	}
	return nil
}

// Wake says the settings changed. The setup page calls it after saving the voice assistant, so the
// client comes up or goes down with the choice rather than at the next restart.
func (f *Feature) Wake() {
	f.reconcile()
	f.Changed.Emit(struct{}{})
}

// SetCommands is what Muse may ask the device to do (feature/assistant). A client that is already up
// is replaced, since the list goes to Muse as the device registers.
func (f *Feature) SetCommands(cmds []muse.Command) {
	f.mu.Lock()
	f.commands = cmds
	f.gen++
	f.mu.Unlock()
	f.reconcile()
}

// Ready is whether a turn can be asked now.
func (f *Feature) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase == Online
}

// Paired is whether the Muse app has paired the device, connected just now or not.
func (f *Feature) Paired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.Credentials != nil
}

// Ask says a voice note to Muse and waits for the answer; on hears it arrive (muse.Client.Ask).
func (f *Feature) Ask(ctx context.Context, wav []byte, on func(muse.ReplyEvent)) (muse.Reply, error) {
	f.mu.Lock()
	r := f.client
	f.mu.Unlock()
	if r == nil {
		return muse.Reply{}, muse.ErrOffline
	}
	return r.c.Ask(ctx, muse.VoiceNote(wav), on)
}

func (f *Feature) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Status{Phase: f.phase, Err: f.err, Progress: f.progress, CanPair: openPeripheral != nil}
	if f.st.Credentials != nil {
		s.Username = f.st.Credentials.Username
	}
	if f.st.Identity.Valid() {
		s.BLEName = f.st.Identity.BLEName()
	}
	return s
}

func (f *Feature) reconcile() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcileLocked()
}

// reconcileLocked makes what runs match what should: a client exactly when the feature is running,
// Muse is what answers, the device is paired and no pairing is open, and built from the current
// settings. Everything that changes one of those ends here, and the phase is settled here too.
func (f *Feature) reconcileLocked() {
	b := config.Get().Brain
	want := f.ctx != nil && b.Mode == config.BrainMuse && f.st.Credentials != nil && f.pair == nil
	p := plan{sdkToken: b.Muse.SDKToken, commands: f.gen}
	if r := f.client; r != nil && !r.stopping && (!want || r.plan != p) {
		r.stopping = true
		r.stop()
	}
	if want && f.client == nil {
		f.startLocked(p)
	}
	switch {
	case f.pair != nil:
		f.phase = Pairing
	case f.st.Credentials == nil:
		f.phase = Unpaired
	case f.client == nil:
		f.phase = Standby
	}
}

// startLocked brings a client up on the plan, and arranges for it to be reconciled away when Run
// returns, whether it was stopped, or Muse unpaired the device, or it gave up.
func (f *Feature) startLocked(p plan) {
	ctx, cancel := context.WithCancel(f.ctx)
	r := &running{plan: p, stop: cancel, done: make(chan struct{})}
	c, err := newClient(muse.Config{
		Store:       stateStore{f: f, r: r},
		Commands:    f.commands,
		DisplayName: config.Get().Device.Name,
		Version:     layout.Version,
		SDKToken:    p.sdkToken,
		Logger:      slog.Default(),
		OnState:     func(s muse.ConnState) { f.connState(r, s) },
	})
	if err != nil {
		cancel()
		slog.Error("muse: could not make the client", "err", err)
		f.err = err
		return
	}
	r.c = c
	f.client = r
	f.phase, f.err = Connecting, nil
	safe.Go("muse client", func() {
		defer close(r.done)
		err := c.Run(ctx)
		f.mu.Lock()
		if f.client == r {
			f.client = nil
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, muse.ErrUnpaired) {
			f.err = err
		}
		f.reconcileLocked()
		f.mu.Unlock()
		f.Changed.Emit(struct{}{})
	})
}

// connState is the client saying where the connection stands, mapped onto the phase. A client that
// has been replaced says nothing.
func (f *Feature) connState(r *running, s muse.ConnState) {
	f.mu.Lock()
	if f.client != r {
		f.mu.Unlock()
		return
	}
	switch s.Phase {
	case muse.Connecting:
		f.phase, f.err = Connecting, nil
	case muse.Online:
		f.phase, f.err = Online, nil
	case muse.Offline:
		f.phase, f.err = Offline, s.Err
	}
	f.mu.Unlock()
	f.Changed.Emit(struct{}{})
}

// saveLocked writes st and keeps it as what is on disk.
func (f *Feature) saveLocked(st muse.State) error {
	if err := f.store.Save(st); err != nil {
		return err
	}
	f.st = st
	return nil
}

// stateStore is the client's view of the file. A rotation kills the tokens it used, so the client
// saves before it goes on, and the feature sees every save; one from a client that is on its way
// out is refused, so a rotation racing an Unpair cannot bring the credentials back.
type stateStore struct {
	f *Feature
	r *running
}

func (s stateStore) Load() (muse.State, error) { return s.f.store.Load() }

func (s stateStore) Save(st muse.State) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.client != s.r || s.r.stopping {
		return errors.New("muse: the client is being stopped")
	}
	if err := s.f.saveLocked(st); err != nil {
		return err
	}
	// A save with no credentials is Muse having removed the device: the client is about to end
	// with ErrUnpaired, and the phase says so from here.
	s.f.reconcileLocked()
	return nil
}
