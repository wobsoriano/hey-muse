//go:build linux

// This file is a modified port of linux/src/musegadget/ble_server.py from Meta's Muse Gadget SDK,
// Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0.
// The GLib loop became one goroutine fed by the bus connection, and the service and advertisement are
// the caller's to name.

package bluez

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	gattMgrIfc  = "org.bluez.GattManager1"
	gattSvcIfc  = "org.bluez.GattService1"
	gattChrcIfc = "org.bluez.GattCharacteristic1"
	advMgrIfc   = "org.bluez.LEAdvertisingManager1"
	advIfc      = "org.bluez.LEAdvertisement1"

	gattAppPath = dbus.ObjectPath("/org/techo5/gatt")
	gattSvcPath = gattAppPath + "/service0"
	gattRXPath  = gattSvcPath + "/char0"
	gattTXPath  = gattSvcPath + "/char1"
	advPath     = dbus.ObjectPath("/org/techo5/advertisement0")

	// minMTU is the smallest ATT MTU there is; anything below it in a write's options is not one.
	minMTU = 23

	// callTimeout bounds each call to bluetoothd, so a stuck daemon cannot hold up teardown.
	callTimeout = 5 * time.Second

	// maxLinkEvents is how many writes may wait for the handler. A phone sends a few packets and
	// waits for the answer, so only something misbehaving gets near it.
	maxLinkEvents = 256
)

// ErrPeripheralClosed is what Send returns once the peripheral is closed.
var ErrPeripheralClosed = errors.New("bluez: the peripheral is closed")

// PeripheralConfig names what a Peripheral serves and advertises: one primary service with a
// characteristic the central writes (RX) and one the peripheral notifies (TX).
type PeripheralConfig struct {
	// LocalName is advertised, and is the adapter's Alias while the peripheral is up, because a
	// phone reads the GAP name as well as the advertised one.
	LocalName string

	ServiceUUID string
	RXUUID      string
	TXUUID      string
	RXFlags     []string // as BlueZ names them: "write", "write-without-response"
	TXFlags     []string // "read", "notify"

	// ManufacturerData is advertised as it is, by company id.
	ManufacturerData map[uint16][]byte

	// AssumedMTU is what MTU answers until a write says what was negotiated.
	AssumedMTU int

	// Stagger is the pause between the packets of one Send.
	Stagger time.Duration
}

func (c PeripheralConfig) validate() error {
	for name, v := range map[string]string{"local name": c.LocalName, "service UUID": c.ServiceUUID, "RX UUID": c.RXUUID, "TX UUID": c.TXUUID} {
		if v == "" {
			return fmt.Errorf("bluez: peripheral has no %s", name)
		}
	}
	if c.AssumedMTU < minMTU {
		return fmt.Errorf("bluez: assumed MTU %d is below the ATT minimum of %d", c.AssumedMTU, minMTU)
	}
	return nil
}

// objects is the tree bluetoothd reads through ObjectManager when the application registers.
func (c PeripheralConfig) objects() map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
	chrc := func(uuid string, flags []string) map[string]map[string]dbus.Variant {
		return map[string]map[string]dbus.Variant{gattChrcIfc: {
			"Service":     dbus.MakeVariant(gattSvcPath),
			"UUID":        dbus.MakeVariant(uuid),
			"Flags":       dbus.MakeVariant(append([]string{}, flags...)),
			"Descriptors": dbus.MakeVariant([]dbus.ObjectPath{}),
		}}
	}
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		gattSvcPath: {gattSvcIfc: {
			"UUID":            dbus.MakeVariant(c.ServiceUUID),
			"Primary":         dbus.MakeVariant(true),
			"Characteristics": dbus.MakeVariant([]dbus.ObjectPath{gattRXPath, gattTXPath}),
		}},
		gattRXPath: chrc(c.RXUUID, c.RXFlags),
		gattTXPath: chrc(c.TXUUID, c.TXFlags),
	}
}

// advertisement is the LEAdvertisement1 object's properties.
func (c PeripheralConfig) advertisement() map[string]dbus.Variant {
	data := map[uint16]dbus.Variant{}
	for id, b := range c.ManufacturerData {
		data[id] = dbus.MakeVariant(append([]byte{}, b...))
	}
	return map[string]dbus.Variant{
		"Type":             dbus.MakeVariant("peripheral"),
		"LocalName":        dbus.MakeVariant(c.LocalName),
		"ServiceUUIDs":     dbus.MakeVariant([]string{c.ServiceUUID}),
		"ManufacturerData": dbus.MakeVariant(data),
		"Includes":         dbus.MakeVariant([]string{}),
	}
}

// LinkHandler hears what the central does. Both are called from one goroutine, in the order things
// happened on the link, and never from inside Send or Disconnect.
type LinkHandler interface {
	OnWrite(data []byte)
	OnDisconnect()
}

// writeOptions is what bluetoothd says about a write: the link's ATT MTU and which device wrote.
type writeOptions struct {
	mtu    int // 0 when not given
	device dbus.ObjectPath
}

func parseWriteOptions(opts map[string]dbus.Variant) writeOptions {
	var o writeOptions
	if mtu, ok := opts["mtu"].Value().(uint16); ok && mtu >= minMTU {
		o.mtu = int(mtu)
	}
	o.device, _ = opts["device"].Value().(dbus.ObjectPath)
	return o
}

type linkEventKind int

const (
	linkWrite linkEventKind = iota
	linkSubscribed
	linkUnsubscribed
	linkConnected
	linkDisconnected
)

// linkEvent is one thing that happened on the link, as bluetoothd reported it.
type linkEvent struct {
	kind   linkEventKind
	data   []byte          // linkWrite
	opts   writeOptions    // linkWrite
	device dbus.ObjectPath // linkConnected, linkDisconnected
}

// linkEventFrom picks the link's events out of everything the bus delivers.
func linkEventFrom(msg *dbus.Message) (linkEvent, bool) {
	path, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	ifc, _ := msg.Headers[dbus.FieldInterface].Value().(string)
	member, _ := msg.Headers[dbus.FieldMember].Value().(string)
	switch {
	case msg.Type == dbus.TypeMethodCall && ifc == gattChrcIfc && path == gattRXPath && member == "WriteValue":
		if len(msg.Body) != 2 {
			return linkEvent{}, false
		}
		data, ok := msg.Body[0].([]byte)
		opts, _ := msg.Body[1].(map[string]dbus.Variant)
		if !ok {
			return linkEvent{}, false
		}
		return linkEvent{kind: linkWrite, data: data, opts: parseWriteOptions(opts)}, true
	case msg.Type == dbus.TypeMethodCall && ifc == gattChrcIfc && path == gattTXPath && member == "StartNotify":
		return linkEvent{kind: linkSubscribed}, true
	case msg.Type == dbus.TypeMethodCall && ifc == gattChrcIfc && path == gattTXPath && member == "StopNotify":
		return linkEvent{kind: linkUnsubscribed}, true
	case msg.Type == dbus.TypeSignal && ifc == propsIfc && member == "PropertiesChanged":
		if len(msg.Body) < 2 {
			return linkEvent{}, false
		}
		if of, _ := msg.Body[0].(string); of != deviceIfc {
			return linkEvent{}, false
		}
		changed, _ := msg.Body[1].(map[string]dbus.Variant)
		connected, ok := changed["Connected"].Value().(bool)
		if !ok {
			return linkEvent{}, false
		}
		if connected {
			return linkEvent{kind: linkConnected, device: path}, true
		}
		return linkEvent{kind: linkDisconnected, device: path}, true
	case msg.Type == dbus.TypeSignal && ifc == objMgrIfc && member == "InterfacesAdded":
		// A central bluetoothd has not met before arrives as a new object that is already connected.
		if len(msg.Body) != 2 {
			return linkEvent{}, false
		}
		device, _ := msg.Body[0].(dbus.ObjectPath)
		ifcs, _ := msg.Body[1].(map[string]map[string]dbus.Variant)
		if connected, _ := ifcs[deviceIfc]["Connected"].Value().(bool); connected {
			return linkEvent{kind: linkConnected, device: device}, true
		}
	}
	return linkEvent{}, false
}

// adapterChange is one adapter property the peripheral set, and what it was before.
type adapterChange struct {
	prop     string
	was, set dbus.Variant
}

// Peripheral is a GATT server and its advertisement, up from Start until Close. It satisfies the Muse
// pairing Transport.
//
// godbus runs every incoming method call on a goroutine of its own, so two writes that arrive
// together could reach the handler swapped, and a chunked message would not reassemble. The
// peripheral therefore reads the link's events where the connection still has them in order, before
// dispatch, and hands them to the handler from one goroutine. The exported methods only acknowledge.
type Peripheral struct {
	cfg PeripheralConfig

	events  chan linkEvent
	done    chan struct{}
	closing sync.Once

	mu       sync.Mutex
	conn     *dbus.Conn
	adapter  dbus.BusObject
	started  bool
	changes  []adapterChange
	mtu      int
	device   dbus.ObjectPath // who wrote last; "" until a central writes
	notify   bool
	value    []byte // what TX last carried, for a read
	overflow bool
}

// NewPeripheral describes a peripheral. Nothing is on the air until Start.
func NewPeripheral(cfg PeripheralConfig) *Peripheral {
	return &Peripheral{
		cfg:    cfg,
		events: make(chan linkEvent, maxLinkEvents),
		done:   make(chan struct{}),
		mtu:    cfg.AssumedMTU,
	}
}

// Start registers the service and begins advertising. The adapter is powered on, takes the local
// name as its Alias, and stops being pairable, since setup bonds nothing; Close puts all three back.
// When Start fails nothing is left registered. A Peripheral starts once.
func (p *Peripheral) Start(ctx context.Context, h LinkHandler) error {
	if err := p.cfg.validate(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("bluez: a Peripheral starts once")
	}
	p.started = true
	p.mu.Unlock()

	if err := p.start(ctx, h); err != nil {
		return errors.Join(err, p.Close())
	}
	return nil
}

func (p *Peripheral) start(ctx context.Context, h LinkHandler) error {
	conn, err := SystemBus(dbus.WithIncomingInterceptor(p.intercept))
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()

	path, err := gattAdapter(ctx, conn)
	if err != nil {
		return err
	}
	adapter := conn.Object(service, path)
	p.mu.Lock()
	p.adapter = adapter
	p.mu.Unlock()

	for _, match := range [][]dbus.MatchOption{
		{dbus.WithMatchInterface(propsIfc), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchArg(0, deviceIfc)},
		{dbus.WithMatchInterface(objMgrIfc), dbus.WithMatchMember("InterfacesAdded")},
	} {
		if err := conn.AddMatchSignalContext(ctx, append(match, dbus.WithMatchSender(service))...); err != nil {
			return fmt.Errorf("bluez: match: %w", err)
		}
	}
	if err := p.export(conn); err != nil {
		return err
	}
	go p.pump(h)

	for _, want := range []struct {
		prop string
		v    any
	}{{"Powered", true}, {"Alias", p.cfg.LocalName}, {"Pairable", false}} {
		if err := p.setAdapter(ctx, want.prop, want.v); err != nil {
			return err
		}
	}
	// bluetoothd reads the tree back through GetManagedObjects before it answers.
	if err := adapter.CallWithContext(ctx, gattMgrIfc+".RegisterApplication", 0, gattAppPath, map[string]dbus.Variant{}).Err; err != nil {
		return fmt.Errorf("bluez: register gatt application: %w", err)
	}
	if err := adapter.CallWithContext(ctx, advMgrIfc+".RegisterAdvertisement", 0, advPath, map[string]dbus.Variant{}).Err; err != nil {
		return fmt.Errorf("bluez: register advertisement: %w", err)
	}
	slog.Info("bluetooth: advertising", "name", p.cfg.LocalName, "service", p.cfg.ServiceUUID, "adapter", string(path))
	return nil
}

// gattAdapter is the first adapter that can serve GATT and advertise.
func gattAdapter(ctx context.Context, conn *dbus.Conn) (dbus.ObjectPath, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := conn.Object(service, "/").CallWithContext(ctx, objMgrIfc+".GetManagedObjects", 0).Store(&objs); err != nil {
		return "", fmt.Errorf("bluez: managed objects: %w", err)
	}
	var found dbus.ObjectPath
	for path, ifcs := range objs {
		_, gatt := ifcs[gattMgrIfc]
		_, adv := ifcs[advMgrIfc]
		if gatt && adv && (found == "" || path < found) {
			found = path
		}
	}
	if found == "" {
		return "", errors.New("bluez: no adapter that can advertise")
	}
	return found, nil
}

// setAdapter sets one adapter property and remembers what it was, unless it already has the value.
func (p *Peripheral) setAdapter(ctx context.Context, prop string, v any) error {
	var was dbus.Variant
	if err := p.adapter.CallWithContext(ctx, propsIfc+".Get", 0, adapterIfc, prop).Store(&was); err != nil {
		return fmt.Errorf("bluez: read adapter %s: %w", prop, err)
	}
	if was.Value() == v {
		return nil
	}
	set := dbus.MakeVariant(v)
	if err := p.adapter.CallWithContext(ctx, propsIfc+".Set", 0, adapterIfc, prop, set).Err; err != nil {
		return fmt.Errorf("bluez: set adapter %s: %w", prop, err)
	}
	p.mu.Lock()
	p.changes = append(p.changes, adapterChange{prop: prop, was: was, set: set})
	p.mu.Unlock()
	return nil
}

func (p *Peripheral) export(conn *dbus.Conn) error {
	tree := p.cfg.objects()
	exports := []struct {
		v    any
		path dbus.ObjectPath
		ifc  string
	}{
		{gattApp{tree}, gattAppPath, objMgrIfc},
		{properties(tree[gattSvcPath]), gattSvcPath, propsIfc},
		{properties(tree[gattRXPath]), gattRXPath, propsIfc},
		{properties(tree[gattTXPath]), gattTXPath, propsIfc},
		{rxChrc{}, gattRXPath, gattChrcIfc},
		{txChrc{p}, gattTXPath, gattChrcIfc},
		{properties{advIfc: p.cfg.advertisement()}, advPath, propsIfc},
		{advert{}, advPath, advIfc},
	}
	for _, e := range exports {
		if err := conn.Export(e.v, e.path, e.ifc); err != nil {
			return fmt.Errorf("bluez: export %s: %w", e.path, err)
		}
	}
	return nil
}

// intercept runs on the connection's reader, so it must not block: a full queue drops the event.
func (p *Peripheral) intercept(msg *dbus.Message) {
	ev, ok := linkEventFrom(msg)
	if !ok {
		return
	}
	select {
	case p.events <- ev:
	default:
		p.mu.Lock()
		first := !p.overflow
		p.overflow = true
		p.mu.Unlock()
		if first {
			slog.Warn("bluetooth: the central is writing faster than setup reads; dropping")
		}
	}
}

func (p *Peripheral) pump(h LinkHandler) {
	for {
		select {
		case <-p.done:
			return
		case ev := <-p.events:
			p.handle(ev, h)
		}
	}
}

func (p *Peripheral) handle(ev linkEvent, h LinkHandler) {
	switch ev.kind {
	case linkWrite:
		p.mu.Lock()
		newCentral := ev.opts.device != "" && ev.opts.device != p.device
		if newCentral {
			p.device = ev.opts.device
		}
		newMTU := ev.opts.mtu != 0 && ev.opts.mtu != p.mtu
		if ev.opts.mtu != 0 {
			p.mtu = ev.opts.mtu
		}
		mtu := p.mtu
		p.mu.Unlock()
		if newCentral || newMTU {
			slog.Info("bluetooth: central is writing", "device", string(ev.opts.device), "mtu", mtu, "mtu_negotiated", ev.opts.mtu != 0)
		}
		slog.Debug("bluetooth: write", "bytes", len(ev.data), "mtu", mtu)
		h.OnWrite(ev.data)
	case linkSubscribed, linkUnsubscribed:
		p.mu.Lock()
		p.notify = ev.kind == linkSubscribed
		p.mu.Unlock()
		slog.Info("bluetooth: central changed its subscription", "subscribed", ev.kind == linkSubscribed)
	case linkConnected:
		slog.Info("bluetooth: device connected", "device", string(ev.device))
	case linkDisconnected:
		// Before the first write nobody has said which device is the central, so any device
		// leaving counts, as it does in the SDK: a reset the handler did not need is harmless, a
		// missed one leaves a dead session open.
		p.mu.Lock()
		ours := p.device == "" || p.device == ev.device
		if ours {
			p.device = ""
			p.mtu = p.cfg.AssumedMTU
			p.notify = false
		}
		p.mu.Unlock()
		slog.Info("bluetooth: device disconnected", "device", string(ev.device), "central", ours)
		if ours {
			h.OnDisconnect()
		}
	}
}

// Send notifies each packet on TX, in order, Stagger apart.
func (p *Peripheral) Send(packets [][]byte) error {
	for i, packet := range packets {
		if i > 0 && p.cfg.Stagger > 0 {
			select {
			case <-p.done:
				return ErrPeripheralClosed
			case <-time.After(p.cfg.Stagger):
			}
		}
		p.mu.Lock()
		conn, subscribed := p.conn, p.notify
		p.value = bytes.Clone(packet)
		p.mu.Unlock()
		select {
		case <-p.done:
			return ErrPeripheralClosed
		default:
		}
		if conn == nil {
			return errors.New("bluez: the peripheral is not started")
		}
		if !subscribed {
			return errors.New("bluez: the central is not subscribed to notifications")
		}
		changed := map[string]dbus.Variant{"Value": dbus.MakeVariant(packet)}
		if err := conn.Emit(gattTXPath, propsIfc+".PropertiesChanged", gattChrcIfc, changed, []string{}); err != nil {
			return fmt.Errorf("bluez: notify: %w", err)
		}
	}
	return nil
}

// MTU is the ATT MTU of the current link, or the assumed one until a write has reported it.
func (p *Peripheral) MTU() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mtu
}

// Disconnect drops the central that last wrote. With none it does nothing.
func (p *Peripheral) Disconnect() error {
	p.mu.Lock()
	conn, device := p.conn, p.device
	p.mu.Unlock()
	if conn == nil || device == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := conn.Object(service, device).CallWithContext(ctx, deviceIfc+".Disconnect", 0).Err; err != nil && !gone(err) {
		return fmt.Errorf("bluez: disconnect %s: %w", device, err)
	}
	return nil
}

// gone is bluetoothd saying the thing to undo is not there, which is what undoing wanted.
func gone(err error) bool {
	for _, name := range []string{"DoesNotExist", "UnknownObject", "NotConnected"} {
		if strings.Contains(err.Error(), name) {
			return true
		}
	}
	return false
}

// Close stops advertising, drops the central, unregisters the service, and gives the adapter back the
// properties Start changed. It may be called more than once and after a failed Start. A step that
// fails does not stop the ones after it, and closing the connection makes bluetoothd drop whatever
// was still registered.
func (p *Peripheral) Close() error {
	var errs []error
	p.closing.Do(func() {
		close(p.done)
		p.mu.Lock()
		conn, adapter, changes := p.conn, p.adapter, p.changes
		p.mu.Unlock()
		if conn == nil {
			return
		}
		defer conn.Close()
		if adapter == nil {
			return
		}
		call := func(what, method string, args ...any) {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			if err := adapter.CallWithContext(ctx, method, 0, args...).Err; err != nil && !gone(err) {
				errs = append(errs, fmt.Errorf("bluez: %s: %w", what, err))
			}
		}
		call("unregister advertisement", advMgrIfc+".UnregisterAdvertisement", advPath)
		if err := p.Disconnect(); err != nil {
			errs = append(errs, err)
		}
		call("unregister gatt application", gattMgrIfc+".UnregisterApplication", gattAppPath)
		for i := len(changes) - 1; i >= 0; i-- {
			c := changes[i]
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			var now dbus.Variant
			err := adapter.CallWithContext(ctx, propsIfc+".Get", 0, adapterIfc, c.prop).Store(&now)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("bluez: read adapter %s: %w", c.prop, err))
			case now.Value() != c.set.Value():
				// Someone else has set it since, the earbuds feature opening its own pairing
				// mode for one. Theirs is the newer intent.
				slog.Info("bluetooth: adapter property changed during setup; leaving it", "property", c.prop)
			default:
				if err := adapter.CallWithContext(ctx, propsIfc+".Set", 0, adapterIfc, c.prop, c.was).Err; err != nil {
					errs = append(errs, fmt.Errorf("bluez: restore adapter %s: %w", c.prop, err))
				}
			}
			cancel()
		}
		slog.Info("bluetooth: peripheral closed", "name", p.cfg.LocalName)
	})
	return errors.Join(errs...)
}

// gattApp is the application root: org.freedesktop.DBus.ObjectManager over the tree.
type gattApp struct {
	tree map[dbus.ObjectPath]map[string]map[string]dbus.Variant
}

func (a gattApp) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return a.tree, nil
}

// properties is org.freedesktop.DBus.Properties over fixed values, by interface.
type properties map[string]map[string]dbus.Variant

func (p properties) GetAll(ifc string) (map[string]dbus.Variant, *dbus.Error) {
	if props, ok := p[ifc]; ok {
		return props, nil
	}
	return map[string]dbus.Variant{}, nil
}

func (p properties) Get(ifc, name string) (dbus.Variant, *dbus.Error) {
	if v, ok := p[ifc][name]; ok {
		return v, nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{ifc + "." + name})
}

// rxChrc acknowledges writes. Their content was taken by intercept.
type rxChrc struct{}

func (rxChrc) WriteValue([]byte, map[string]dbus.Variant) *dbus.Error { return nil }

// txChrc acknowledges subscriptions, which intercept also took, and answers reads.
type txChrc struct{ p *Peripheral }

func (c txChrc) ReadValue(map[string]dbus.Variant) ([]byte, *dbus.Error) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	return append([]byte{}, c.p.value...), nil
}

func (txChrc) StartNotify() *dbus.Error { return nil }
func (txChrc) StopNotify() *dbus.Error  { return nil }

// advert is org.bluez.LEAdvertisement1.
type advert struct{}

// Release is bluetoothd dropping the advertisement itself, when the adapter is powered off under it.
func (advert) Release() *dbus.Error {
	slog.Warn("bluetooth: bluetoothd released the advertisement")
	return nil
}
