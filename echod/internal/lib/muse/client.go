// Ported from Meta's Muse Gadget SDK (linux/src/musegadget/service.py, and the command spec
// shape of executor.py), Apache-2.0. Modified: rewritten in Go, with the state kept by the caller,
// rotated tokens held until they are saved, and the connection state reported as it changes.

package muse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const defaultNoiseHost = "hatch.metaaivm.com"

// Client keeps a paired device connected to its Muse. Make one with New.
type Client struct {
	cfg       Config
	log       *slog.Logger
	api       apiClient
	userAgent string
	register  map[string]any
	commands  map[string]Command
	t         timing
	// allowPlaintext lets the tests point at a server without TLS. Nothing outside them sets it.
	allowPlaintext bool

	mu      sync.Mutex
	state   ConnState
	session *session
	running bool

	// The rest belongs to the goroutine in Run.

	// unsaved is a State with rotated tokens that Save has not accepted yet. The tokens in the
	// store are dead and these are the only live ones, so nothing else happens until they are
	// saved.
	unsaved         *State
	lastRefresh     time.Time
	reportAttempted bool
	// rejected is whether the last round ended with the API rejecting the access token.
	rejected bool
}

type paramSpec struct {
	Type        ParamType `json:"type"`
	Description string    `json:"description"`
}

// commandSpec is one entry of commands_v2 in link.register, as the SDK's executor writes it.
type commandSpec struct {
	Description string               `json:"description"`
	Required    map[string]paramSpec `json:"required"`
	Optional    map[string]paramSpec `json:"optional"`
	TimeoutMS   int64                `json:"timeout_ms,omitempty"`
}

func commandSpecs(commands []Command) (map[string]commandSpec, map[string]Command, error) {
	specs := map[string]commandSpec{}
	byName := map[string]Command{}
	for _, c := range commands {
		switch {
		case c.Name == "" || c.Handler == nil:
			return nil, nil, fmt.Errorf("muse: command %q needs a name and a handler", c.Name)
		case c.Name == "device.ota":
			// The server pushes ESP32 firmware to whatever advertises it.
			return nil, nil, errors.New("muse: device.ota must never be advertised")
		}
		if _, dup := byName[c.Name]; dup {
			return nil, nil, fmt.Errorf("muse: command %q is listed twice", c.Name)
		}
		spec := commandSpec{
			Description: c.Description,
			Required:    map[string]paramSpec{},
			Optional:    map[string]paramSpec{},
			TimeoutMS:   c.Timeout.Milliseconds(),
		}
		for _, group := range []struct {
			params []Param
			into   map[string]paramSpec
		}{{c.Required, spec.Required}, {c.Optional, spec.Optional}} {
			for _, p := range group.params {
				_, required := spec.Required[p.Name]
				_, optional := spec.Optional[p.Name]
				if p.Name == "" || p.Type == "" || required || optional {
					return nil, nil, fmt.Errorf("muse: command %q has a bad parameter %q", c.Name, p.Name)
				}
				group.into[p.Name] = paramSpec{Type: p.Type, Description: p.Description}
			}
		}
		specs[c.Name], byName[c.Name] = spec, c
	}
	return specs, byName, nil
}

// New checks the configuration and returns a Client that is not yet running.
func New(cfg Config) (*Client, error) {
	if cfg.Store == nil {
		return nil, errors.New("muse: a Store is required")
	}
	specs, commands, err := commandSpecs(cfg.Commands)
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName, _ = os.Hostname()
	}
	if cfg.Version == "" {
		cfg.Version = "0"
	}
	// A transport of its own, not a clone of the default: the daemon replaces the default with one
	// that can stop checking certificates (feature/diag), and the tokens sent here never go unchecked.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       cfg.TLS.Clone(),
	}
	userAgent := fmt.Sprintf("musegadget/%s (%s %s) %s", cfg.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return &Client{
		cfg:       cfg,
		log:       cfg.Logger,
		api:       apiClient{http: &http.Client{Transport: transport}, userAgent: userAgent},
		userAgent: userAgent,
		commands:  commands,
		// The VM knows these values from the SDK's Linux client. The family must never be
		// "link": the server pushes ESP32 firmware updates to every link device.
		register: map[string]any{
			"display_name":        cfg.DisplayName,
			"platform":            "linux",
			"version":             cfg.Version,
			"device_family":       "homehub",
			"model_id":            "linux",
			"is_wakeup_supported": false,
			"commands_v2":         specs,
		},
		t: defaultTiming,
	}, nil
}

// State is the connection state right now.
func (c *Client) State() ConnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) setState(next ConnState) {
	c.mu.Lock()
	changed := c.state != next
	c.state = next
	c.mu.Unlock()
	if changed && c.cfg.OnState != nil {
		c.cfg.OnState(next)
	}
}

// Ask says one thing to Muse and waits for the whole answer. on, which may be nil, hears the
// answer arrive: Heard for a voice note, ReplyText as the text grows, Settled when it looks
// complete. It runs on the calling goroutine, so once Ask has returned, for a canceled ctx or
// anything else, it is never called again.
//
// One Ask can be open at a time. Canceling ctx stops the waiting, but a message Muse has already
// taken stays said.
func (c *Client) Ask(ctx context.Context, in AskInput, on func(ReplyEvent)) (Reply, error) {
	switch in.kind {
	case askText:
		if strings.TrimSpace(in.text) == "" {
			return Reply{}, errors.New("muse: nothing to ask")
		}
		if len(in.text) > maxAskText {
			return Reply{}, errors.New("muse: the message is too long")
		}
	case askVoice:
		if len(in.wav) < 12 || string(in.wav[:4]) != "RIFF" || string(in.wav[8:12]) != "WAVE" {
			return Reply{}, errors.New("muse: the voice note is not a WAV file")
		}
		if len(in.wav) > maxVoiceNote {
			return Reply{}, errors.New("muse: the voice note is too long")
		}
	default:
		return Reply{}, errors.New("muse: an ask is made with Text or VoiceNote")
	}
	if on == nil {
		on = func(ReplyEvent) {}
	}
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == nil {
		return Reply{}, ErrOffline
	}
	return s.ask(ctx, in, on)
}

// backoff spaces the reconnects: doubling from the base to the cap, and never under the floor.
type backoff struct {
	t        timing
	failures int
	floor    time.Duration
}

func (b *backoff) next() time.Duration {
	d := b.t.backoffMax
	if b.failures < 16 {
		d = min(b.t.backoffBase<<b.failures, b.t.backoffMax)
	}
	b.failures++
	return max(d, b.floor)
}

func (b *backoff) reset() { b.failures, b.floor = 0, 0 }

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Run keeps the device connected until ctx ends, and returns its error, or until the device turns
// out to be unpaired, and returns ErrUnpaired. Each round fetches the VMs with the device token,
// which also yields a fresh bearer, connects to the default one and serves until the connection
// ends. Failures back off, and a session that stayed up a while clears the backoff. The device
// token is rotated before it expires, and at once if the API rejects it.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("muse: already running")
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	pace := backoff{t: c.t}
	for {
		if err := ctx.Err(); err != nil {
			c.setState(ConnState{Phase: Offline})
			return err
		}
		wait, err := c.round(ctx, &pace)
		if errors.Is(err, ErrUnpaired) {
			return err
		}
		if err != nil && ctx.Err() == nil {
			c.log.Warn("muse: offline", "err", err, "retry_in", wait.Round(time.Millisecond))
			c.setState(ConnState{Phase: Offline, Err: err})
		}
		sleep(ctx, wait)
	}
}

// round is one try at being connected, from the stored State to the end of a session. It returns
// how long to wait before the next and what went wrong, or ErrUnpaired when there is to be no next.
func (c *Client) round(ctx context.Context, pace *backoff) (time.Duration, error) {
	st, err := c.load()
	if err != nil {
		return pace.next(), err
	}
	if st.Credentials == nil {
		c.setState(ConnState{Phase: Unpaired})
		return 0, ErrUnpaired
	}
	c.setState(ConnState{Phase: Connecting})

	root, err := apiRoot(st.Credentials, c.allowPlaintext)
	if err != nil {
		return c.t.backoffMax, err
	}
	if st, err = c.refresh(ctx, root, st, false); err != nil {
		return c.refreshFailed(st, err, pace)
	}
	vms, status, err := c.api.fetchVMs(ctx, root, st.Credentials.AccessToken)
	if status == http.StatusUnauthorized {
		c.log.Warn("muse: device token rejected by the API; refreshing")
		if _, err := c.refresh(ctx, root, st, true); err != nil {
			return c.refreshFailed(st, err, pace)
		}
		// The new token is tried on the next round, at once the first time. A server that rejects
		// every token it hands out must not have the device rotate them in a tight loop.
		if c.rejected {
			return pace.next(), nil
		}
		c.rejected = true
		return 0, nil
	}
	c.rejected = false
	if err == nil && len(vms) == 0 {
		err = errors.New("muse: no VM is leased to this device")
	}
	if err != nil {
		return pace.next(), err
	}
	chosen := vms[0]
	for _, v := range vms {
		if v.isDefault {
			chosen = v
			break
		}
	}

	out, lasted, err := c.serve(ctx, chosen, st)
	if lasted >= c.t.healthy {
		pace.reset()
	}
	switch out {
	case outcomeStopped:
		return 0, nil
	case outcomeUnpaired:
		c.log.Warn("muse: Muse removed this device")
		return 0, c.unpair(st)
	case outcomeAuthRejected, outcomeForbidden:
		// Either way the next round fetches the VMs again, and with them a new bearer.
		pace.floor = c.t.authFloor
	}
	return pace.next(), err
}

func (c *Client) refreshFailed(st State, err error, pace *backoff) (time.Duration, error) {
	switch {
	case errors.Is(err, ErrUnpaired):
		return 0, c.unpair(st)
	case errors.Is(err, errRefreshFailed):
		return c.t.tokenRetry, err
	}
	return pace.next(), err
}

// load reads the State, once any rotated tokens still waiting to be saved have been.
func (c *Client) load() (State, error) {
	if c.unsaved != nil {
		if err := c.cfg.Store.Save(*c.unsaved); err != nil {
			return State{}, fmt.Errorf("muse: could not save the rotated tokens: %w", err)
		}
		c.unsaved = nil
		c.log.Info("muse: rotated tokens saved")
	}
	st, err := c.cfg.Store.Load()
	if err != nil {
		return State{}, fmt.Errorf("muse: could not load the state: %w", err)
	}
	if st.Credentials != nil && !st.Identity.Valid() {
		return State{}, errors.New("muse: the state has credentials but no identity")
	}
	return st, nil
}

// unpair forgets the credentials, keeping the identity so the device pairs again as itself.
func (c *Client) unpair(st State) error {
	c.unsaved = nil
	if err := c.cfg.Store.Save(State{Identity: st.Identity}); err != nil {
		// Nothing is lost: the dead credentials are found dead again on the next start.
		c.log.Error("muse: could not save the unpaired state", "err", err)
	}
	c.setState(ConnState{Phase: Unpaired})
	return ErrUnpaired
}

func (c *Client) serve(ctx context.Context, v vm, st State) (outcome, time.Duration, error) {
	host := st.Credentials.NoiseHost
	if host == "" {
		host = defaultNoiseHost
	}
	vmID := v.id
	if vmID == "" {
		vmID = v.name
	}
	url, err := noiseURL(host, vmID, c.allowPlaintext)
	if err != nil {
		return outcomeClosed, 0, err
	}
	register := map[string]any{"node_id": st.Identity.NodeID()}
	for key, value := range c.register {
		register[key] = value
	}
	s := newSession(sessionParams{
		url:        url,
		bearer:     v.authToken,
		userAgent:  c.userAgent,
		tls:        c.cfg.TLS,
		nodeID:     st.Identity.NodeID(),
		register:   register,
		commands:   c.commands,
		log:        c.log,
		t:          c.t,
		registered: func() { c.setState(ConnState{Phase: Online}) },
	})
	c.log.Info("muse: connecting", "vm", vmID)
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
	out, err := s.run(ctx)
	c.mu.Lock()
	c.session = nil
	c.mu.Unlock()

	var lasted time.Duration
	if !s.registeredAt.IsZero() {
		lasted = time.Since(s.registeredAt)
	}
	c.log.Info("muse: session ended", "registered_for", lasted.Round(time.Second), "err", err)
	return out, lasted, err
}

// errRefreshFailed marks a refresh that was needed and did not happen, so the old token should
// not be tried again for a while.
var errRefreshFailed = errors.New("muse: token refresh failed")

// refresh returns the State to connect with, rotating the tokens first if they are due. force is
// for when the API has just rejected the access token.
//
// A rotation kills the pair it used, so the new pair goes to the Store before anything else: only
// a State that Save has accepted is returned. If Save fails the new pair waits in c.unsaved, and
// load keeps trying it before every round.
func (c *Client) refresh(ctx context.Context, root string, st State, force bool) (State, error) {
	age := time.Since(time.Unix(st.Credentials.AccessTokenSavedAt, 0))
	// The SDK token reaches Muse only in a refresh body, so a device with one that has not been
	// reported refreshes early, once, to say it.
	reportDue := c.cfg.SDKToken != "" && !st.SDKTokenReported && !c.reportAttempted
	// A negative age is a clock that has not been set, which says nothing about the token.
	due := force || age >= c.t.refreshAge || age < 0
	if !due && !reportDue {
		return st, nil
	}
	if !force && !c.lastRefresh.IsZero() && time.Since(c.lastRefresh) < c.t.tokenRetry {
		return st, nil
	}
	c.lastRefresh = time.Now()
	if reportDue {
		c.reportAttempted = true
		c.log.Info("muse: refreshing the device token to report the SDK token")
	}
	pair, status, err := c.api.refresh(ctx, root, st.Credentials.RefreshToken, st.Identity.NodeID(), c.cfg.SDKToken)
	if pair != nil {
		creds := *st.Credentials
		creds.AccessToken, creds.RefreshToken = pair.AccessToken, pair.RefreshToken
		creds.AccessTokenSavedAt = time.Now().Unix()
		next := st
		next.Credentials = &creds
		next.SDKTokenReported = st.SDKTokenReported || c.cfg.SDKToken != ""
		if err := c.cfg.Store.Save(next); err != nil {
			c.unsaved = &next
			return st, fmt.Errorf("muse: could not save the rotated tokens: %w", err)
		}
		c.log.Info("muse: device token rotated")
		return next, nil
	}
	switch {
	case !due:
		// Only reporting the SDK token: nothing has rejected the current token, so a refusal
		// here must never unpair the device.
		c.log.Warn("muse: the SDK token report failed; keeping the pairing", "err", err)
		return st, nil
	case status == http.StatusUnauthorized:
		c.log.Error("muse: pairing revoked; the device has to be paired again")
		return st, ErrUnpaired
	case force:
		return st, fmt.Errorf("%w: %w", errRefreshFailed, err)
	}
	// Not rejected yet, so the current token is used while it still works.
	c.log.Warn("muse: token refresh failed; keeping the current token", "err", err)
	return st, nil
}
