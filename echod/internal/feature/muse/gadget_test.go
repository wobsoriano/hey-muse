package gadget

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

// fakeClient stands in for muse.Client: it reports whatever states it is told to, and ends however
// it is told to, or when ctx does.
type fakeClient struct {
	cfg   muse.Config
	serve func(ctx context.Context, cfg muse.Config) error
	ended chan struct{}
}

func (c *fakeClient) Run(ctx context.Context) error {
	defer close(c.ended)
	return c.serve(ctx, c.cfg)
}

func (c *fakeClient) Ask(context.Context, muse.AskInput, func(muse.ReplyEvent)) (muse.Reply, error) {
	return muse.Reply{Text: "hello"}, nil
}

// online is a client that comes up and stays up.
func online(ctx context.Context, cfg muse.Config) error {
	cfg.OnState(muse.ConnState{Phase: muse.Connecting})
	cfg.OnState(muse.ConnState{Phase: muse.Online})
	<-ctx.Done()
	return ctx.Err()
}

// clients collects every client made, in order.
type clients struct {
	mu   sync.Mutex
	list []*fakeClient
}

func (c *clients) nth(i int) *fakeClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list[i]
}

func (c *clients) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.list)
}

// start is a feature on its own files, running, with Muse chosen and the fake client in place.
func start(t *testing.T, mode config.BrainMode, serve func(context.Context, muse.Config) error) (*Feature, *clients) {
	t.Helper()
	dir := t.TempDir()
	config.Use(filepath.Join(dir, "state.json"))
	if err := config.Set().Brain().Set(config.Brain{Mode: mode}); err != nil {
		t.Fatal(err)
	}
	if err := config.Set().Brain().SetSDKToken("mgst_test"); err != nil {
		t.Fatal(err)
	}
	made := &clients{}
	was := newClient
	newClient = func(cfg muse.Config) (client, error) {
		c := &fakeClient{cfg: cfg, serve: serve, ended: make(chan struct{})}
		made.mu.Lock()
		made.list = append(made.list, c)
		made.mu.Unlock()
		return c, nil
	}
	t.Cleanup(func() { newClient = was })

	f := build(filepath.Join(dir, "muse.json"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return f, made
}

// await waits for the status to satisfy ok, and fails with what it was instead.
func await(t *testing.T, f *Feature, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := f.Status()
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: phase %s, err %v", what, s.Phase, s.Err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func phase(p Phase) func(Status) bool { return func(s Status) bool { return s.Phase == p } }

var testResult = pairing.Result{AccessToken: "access-one", RefreshToken: "refresh-one", TokenType: "device", Username: "someone"}

// A device nobody has paired runs nothing, and pairing brings the client up with the tokens on
// disk, stamped. Tokens the client rotates reach the same file and nothing of them reaches
// state.json.
func TestPairingThenRotation(t *testing.T) {
	rotated := make(chan error, 1)
	// The feature may start the client more than once as the settings settle, and only the first run
	// is listened to: a later one with nobody to hear it must not hold up the shutdown.
	report := func(err error) {
		select {
		case rotated <- err:
		default:
		}
	}
	f, made := start(t, config.BrainMuse, func(ctx context.Context, cfg muse.Config) error {
		cfg.OnState(muse.ConnState{Phase: muse.Online})
		st, err := cfg.Store.Load()
		if err != nil {
			report(err)
			return err
		}
		st.Credentials.AccessToken, st.Credentials.RefreshToken = "access-two", "refresh-two"
		report(cfg.Store.Save(st))
		<-ctx.Done()
		return ctx.Err()
	})
	await(t, f, "unpaired", phase(Unpaired))
	if made.count() != 0 {
		t.Fatal("a client was made for an unpaired device")
	}
	if _, err := f.Ask(context.Background(), nil, nil); !errors.Is(err, muse.ErrOffline) {
		t.Errorf("Ask while unpaired: %v", err)
	}

	before := time.Now().Unix()
	if err := f.savePairing(testResult); err != nil {
		t.Fatal(err)
	}
	f.Wake()
	s := await(t, f, "online", phase(Online))
	if s.Username != "someone" || !f.Ready() {
		t.Errorf("online as %q, ready %v", s.Username, f.Ready())
	}
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	st, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Credentials == nil || st.Credentials.AccessToken != "access-two" || st.Credentials.AccessTokenSavedAt < before {
		t.Errorf("the rotated tokens did not reach disk: %+v", st.Credentials)
	}
	if st.SDKTokenReported {
		t.Error("a fresh pairing claims the SDK token was reported")
	}
	if got := f.Status().Username; got != "someone" {
		t.Errorf("after the rotation the device is %q", got)
	}
	if reply, err := f.Ask(context.Background(), nil, nil); err != nil || reply.Text != "hello" {
		t.Errorf("Ask = %+v, %v", reply, err)
	}

	settings, err := os.ReadFile(filepath.Join(filepath.Dir(f.store.Path), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-one", "access-two", "refresh-one", "refresh-two", "someone"} {
		if strings.Contains(string(settings), secret) {
			t.Errorf("state.json holds %q", secret)
		}
	}
}

// Choosing another assistant takes the client down, and choosing Muse again brings a new one up.
// Muse taking the device away leaves it unpaired, with the identity kept for pairing again.
func TestModeAndUnpairingDriveTheClient(t *testing.T) {
	f, made := start(t, config.BrainMuse, online)
	if err := f.savePairing(testResult); err != nil {
		t.Fatal(err)
	}
	f.Wake()
	await(t, f, "online", phase(Online))

	if err := config.Set().Brain().Set(config.Brain{Mode: config.BrainDirect}); err != nil {
		t.Fatal(err)
	}
	f.Wake()
	await(t, f, "standby", phase(Standby))
	select {
	case <-made.nth(0).ended:
	case <-time.After(time.Second):
		t.Fatal("the client kept running after Muse stopped being the assistant")
	}
	if f.Ready() {
		t.Error("ready in standby")
	}

	if err := config.Set().Brain().Set(config.Brain{Mode: config.BrainMuse}); err != nil {
		t.Fatal(err)
	}
	f.Wake()
	await(t, f, "online again", phase(Online))
	if made.count() != 2 {
		t.Fatalf("%d clients made, want 2", made.count())
	}

	// What Muse does when it removes the device: the client saves the identity alone and ends.
	c := made.nth(1)
	st, _ := c.cfg.Store.Load()
	if err := c.cfg.Store.Save(muse.State{Identity: st.Identity}); err != nil {
		t.Fatal(err)
	}
	await(t, f, "unpaired", phase(Unpaired))
	if got, _ := f.store.Load(); got.Credentials != nil {
		t.Errorf("after Muse removed the device: %+v", got)
	}
}

// Changing what the device can do replaces the client, since the list is sent as it registers.
func TestSetCommandsReplacesTheClient(t *testing.T) {
	f, made := start(t, config.BrainMuse, online)
	if err := f.savePairing(testResult); err != nil {
		t.Fatal(err)
	}
	f.Wake()
	await(t, f, "online", phase(Online))
	f.SetCommands([]muse.Command{{Name: "echo.stop", Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }}})
	await(t, f, "online with the commands", func(s Status) bool { return s.Phase == Online && made.count() == 2 })
	if n := len(made.nth(1).cfg.Commands); n != 1 {
		t.Errorf("the new client carries %d commands", n)
	}
}

// fakePeripheral is the seam filled in: it remembers what it was asked to advertise and whether it
// was taken down.
type fakePeripheral struct {
	mu     sync.Mutex
	name   string
	closed int
	// identityOnDisk is whether the file already held a valid identity when the service went up.
	identityOnDisk bool
}

func (p *fakePeripheral) Send([][]byte) error { return nil }
func (p *fakePeripheral) MTU() int            { return pairing.AssumedMTU }
func (p *fakePeripheral) Disconnect() error   { return nil }

func (p *fakePeripheral) open(f *Feature) {
	openPeripheral = func(ctx context.Context, name string, h Handler) (pairing.Transport, func(), error) {
		st, err := f.store.Load()
		p.mu.Lock()
		p.name = name
		p.identityOnDisk = err == nil && st.Identity.Valid() && st.Identity.BLEName() == name
		p.mu.Unlock()
		return p, func() { p.mu.Lock(); p.closed++; p.mu.Unlock() }, nil
	}
}

// Pairing needs a peripheral the build may not have, writes the identity before advertising, takes
// the client down while open, and comes back when canceled.
func TestPairingLifecycle(t *testing.T) {
	was := openPeripheral
	t.Cleanup(func() { openPeripheral = was })
	openPeripheral = nil

	f, made := start(t, config.BrainMuse, online)
	if err := f.StartPairing(); err == nil || !strings.Contains(err.Error(), "this build") {
		t.Fatalf("without a peripheral: %v", err)
	}
	if f.Status().CanPair {
		t.Error("CanPair without a peripheral")
	}

	p := &fakePeripheral{}
	p.open(f)
	if err := config.Set().Brain().SetSDKToken(""); err != nil {
		t.Fatal(err)
	}
	if err := f.StartPairing(); err == nil || !strings.Contains(err.Error(), "SDK token") {
		t.Fatalf("without an SDK token: %v", err)
	}
	if err := config.Set().Brain().SetSDKToken("mgst_test"); err != nil {
		t.Fatal(err)
	}

	if err := f.savePairing(testResult); err != nil {
		t.Fatal(err)
	}
	f.Wake()
	await(t, f, "online", phase(Online))

	if err := f.StartPairing(); err != nil {
		t.Fatal(err)
	}
	if err := f.StartPairing(); err != nil {
		t.Errorf("starting again while open: %v", err)
	}
	s := await(t, f, "pairing", phase(Pairing))
	p.mu.Lock()
	name, onDisk := p.name, p.identityOnDisk
	p.mu.Unlock()
	if !onDisk || name != s.BLEName || !strings.HasPrefix(name, pairing.BLENamePrefix) {
		t.Errorf("advertised %q (identity on disk first: %v), status says %q", name, onDisk, s.BLEName)
	}
	select {
	case <-made.nth(0).ended:
	case <-time.After(time.Second):
		t.Fatal("the client stayed up through pairing")
	}
	if s.Progress.State != pairing.StateWaiting {
		t.Errorf("progress %v", s.Progress)
	}

	f.CancelPairing()
	s = await(t, f, "online again", phase(Online))
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed != 1 {
		t.Errorf("the peripheral was closed %d times", closed)
	}
	if s.Progress.State != pairing.StateFailed || s.Progress.Reason != pairing.ReasonCanceled {
		t.Errorf("after canceling, progress %v", s.Progress)
	}

	// Unpairing keeps the identity, so the app finds the same device next time.
	id := f.Status().BLEName
	if err := f.Unpair(); err != nil {
		t.Fatal(err)
	}
	s = await(t, f, "unpaired", phase(Unpaired))
	if s.BLEName != id || s.Username != "" {
		t.Errorf("after unpairing: %+v", s)
	}
}
