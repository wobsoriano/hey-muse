//go:build linux

package bluez

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

var testPeripheral = PeripheralConfig{
	LocalName:        "MuseGadgetA1B2C3",
	ServiceUUID:      "7fdd3d1c-38ea-46cf-8b46-314ecf5f240c",
	RXUUID:           "4d593029-28a2-4a6e-a1f0-3c2d5e8f9b01",
	TXUUID:           "d75dc4ca-7b2b-4e9c-8f0a-1d2e3f4a5b6c",
	RXFlags:          []string{"write", "write-without-response"},
	TXFlags:          []string{"read", "notify"},
	ManufacturerData: map[uint16][]byte{0xFFFF: {0x00}},
	AssumedMTU:       163,
}

// bluetoothd rejects an application whose tree is not exactly a{oa{sa{sv}}} with these properties in
// these types, and says only "No object received".
func TestPeripheralObjects(t *testing.T) {
	tree := testPeripheral.objects()
	if got := dbus.SignatureOf(tree).String(); got != "a{oa{sa{sv}}}" {
		t.Fatalf("tree signature %s", got)
	}
	want := map[dbus.ObjectPath]map[string]map[string]string{
		"/org/techo5/gatt/service0": {gattSvcIfc: {
			"UUID":            `"7fdd3d1c-38ea-46cf-8b46-314ecf5f240c"`,
			"Primary":         `true`,
			"Characteristics": `@ao ["/org/techo5/gatt/service0/char0", "/org/techo5/gatt/service0/char1"]`,
		}},
		"/org/techo5/gatt/service0/char0": {gattChrcIfc: {
			"Service":     `@o "/org/techo5/gatt/service0"`,
			"UUID":        `"4d593029-28a2-4a6e-a1f0-3c2d5e8f9b01"`,
			"Flags":       `["write", "write-without-response"]`,
			"Descriptors": `@ao []`,
		}},
		"/org/techo5/gatt/service0/char1": {gattChrcIfc: {
			"Service":     `@o "/org/techo5/gatt/service0"`,
			"UUID":        `"d75dc4ca-7b2b-4e9c-8f0a-1d2e3f4a5b6c"`,
			"Flags":       `["read", "notify"]`,
			"Descriptors": `@ao []`,
		}},
	}
	if got := render(tree); !reflect.DeepEqual(got, want) {
		t.Errorf("tree\n got %v\nwant %v", got, want)
	}
}

func render(tree map[dbus.ObjectPath]map[string]map[string]dbus.Variant) map[dbus.ObjectPath]map[string]map[string]string {
	out := map[dbus.ObjectPath]map[string]map[string]string{}
	for path, ifcs := range tree {
		out[path] = map[string]map[string]string{}
		for ifc, props := range ifcs {
			out[path][ifc] = map[string]string{}
			for k, v := range props {
				out[path][ifc][k] = v.String()
			}
		}
	}
	return out
}

func TestPeripheralAdvertisement(t *testing.T) {
	got := map[string]string{}
	for k, v := range testPeripheral.advertisement() {
		got[k] = v.Signature().String() + " " + v.String()
	}
	want := map[string]string{
		"Type":             `s "peripheral"`,
		"LocalName":        `s "MuseGadgetA1B2C3"`,
		"ServiceUUIDs":     `as ["7fdd3d1c-38ea-46cf-8b46-314ecf5f240c"]`,
		"ManufacturerData": `a{qv} @a{qv} {65535: <@ay [0x0]>}`,
		"Includes":         `as @as []`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("advertisement\n got %v\nwant %v", got, want)
	}
}

func TestPeripheralConfigValidate(t *testing.T) {
	if err := testPeripheral.validate(); err != nil {
		t.Fatal(err)
	}
	noName, lowMTU := testPeripheral, testPeripheral
	noName.LocalName = ""
	lowMTU.AssumedMTU = 22
	for _, c := range []PeripheralConfig{noName, lowMTU} {
		if c.validate() == nil {
			t.Errorf("validate accepted %+v", c)
		}
	}
}

func TestParseWriteOptions(t *testing.T) {
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	cases := []struct {
		name string
		opts map[string]dbus.Variant
		want writeOptions
	}{
		{"both", map[string]dbus.Variant{"mtu": dbus.MakeVariant(uint16(185)), "device": dbus.MakeVariant(dev), "link": dbus.MakeVariant("LE")}, writeOptions{185, dev}},
		{"none", nil, writeOptions{}},
		{"mtu below the ATT minimum", map[string]dbus.Variant{"mtu": dbus.MakeVariant(uint16(5))}, writeOptions{}},
		{"mtu of another type", map[string]dbus.Variant{"mtu": dbus.MakeVariant("185")}, writeOptions{}},
		{"device as a string", map[string]dbus.Variant{"device": dbus.MakeVariant(string(dev))}, writeOptions{}},
	}
	for _, c := range cases {
		if got := parseWriteOptions(c.opts); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func message(typ dbus.Type, path dbus.ObjectPath, ifc, member string, body ...any) *dbus.Message {
	return &dbus.Message{Type: typ, Body: body, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath:      dbus.MakeVariant(path),
		dbus.FieldInterface: dbus.MakeVariant(ifc),
		dbus.FieldMember:    dbus.MakeVariant(member),
	}}
}

func write(data string, opts map[string]dbus.Variant) *dbus.Message {
	return message(dbus.TypeMethodCall, gattRXPath, gattChrcIfc, "WriteValue", []byte(data), opts)
}

func connected(device dbus.ObjectPath, on bool) *dbus.Message {
	return message(dbus.TypeSignal, device, propsIfc, "PropertiesChanged", deviceIfc,
		map[string]dbus.Variant{"Connected": dbus.MakeVariant(on)}, []string{})
}

func TestLinkEventFrom(t *testing.T) {
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_AA")
	cases := []struct {
		name string
		msg  *dbus.Message
		want linkEventKind
		ok   bool
	}{
		{"write", write("x", nil), linkWrite, true},
		{"write to TX", message(dbus.TypeMethodCall, gattTXPath, gattChrcIfc, "WriteValue", []byte("x"), map[string]dbus.Variant{}), 0, false},
		{"write with no body", message(dbus.TypeMethodCall, gattRXPath, gattChrcIfc, "WriteValue"), 0, false},
		{"subscribe", message(dbus.TypeMethodCall, gattTXPath, gattChrcIfc, "StartNotify"), linkSubscribed, true},
		{"unsubscribe", message(dbus.TypeMethodCall, gattTXPath, gattChrcIfc, "StopNotify"), linkUnsubscribed, true},
		{"connected", connected(phone, true), linkConnected, true},
		{"disconnected", connected(phone, false), linkDisconnected, true},
		{"new device, connected", message(dbus.TypeSignal, "/", objMgrIfc, "InterfacesAdded", phone,
			map[string]map[string]dbus.Variant{deviceIfc: {"Connected": dbus.MakeVariant(true)}}), linkConnected, true},
		{"new device, only heard", message(dbus.TypeSignal, "/", objMgrIfc, "InterfacesAdded", phone,
			map[string]map[string]dbus.Variant{deviceIfc: {"Connected": dbus.MakeVariant(false)}}), 0, false},
		{"signal strength only", message(dbus.TypeSignal, phone, propsIfc, "PropertiesChanged", deviceIfc,
			map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-60))}, []string{}), 0, false},
		{"adapter property", message(dbus.TypeSignal, "/org/bluez/hci0", propsIfc, "PropertiesChanged", adapterIfc,
			map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)}, []string{}), 0, false},
		{"read", message(dbus.TypeMethodCall, gattTXPath, gattChrcIfc, "ReadValue", map[string]dbus.Variant{}), 0, false},
	}
	for _, c := range cases {
		got, ok := linkEventFrom(c.msg)
		if ok != c.ok || got.kind != c.want {
			t.Errorf("%s: got kind %d ok %v, want kind %d ok %v", c.name, got.kind, ok, c.want, c.ok)
		}
	}
}

type recorder struct{ got chan string }

func (r recorder) OnWrite(data []byte) { r.got <- "write " + string(data) }
func (r recorder) OnDisconnect()       { r.got <- "disconnect" }

func (r recorder) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-r.got:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("the handler heard nothing")
		return ""
	}
}

func (r recorder) quiet(t *testing.T) {
	t.Helper()
	select {
	case s := <-r.got:
		t.Fatalf("the handler heard %q", s)
	case <-time.After(50 * time.Millisecond):
	}
}

func startPump(t *testing.T) (*Peripheral, recorder) {
	p := NewPeripheral(testPeripheral)
	r := recorder{got: make(chan string, maxLinkEvents)}
	go p.pump(r)
	t.Cleanup(func() { p.Close() })
	return p, r
}

// The chunks of one message must reach the handler in the order the bus delivered them, with a
// disconnect in its place among them.
func TestPeripheralKeepsLinkOrder(t *testing.T) {
	p, r := startPump(t)
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_AA")
	var want []string
	for i := range 100 {
		p.intercept(write(fmt.Sprint(i), nil))
		want = append(want, fmt.Sprint("write ", i))
		if i == 50 {
			p.intercept(connected(phone, false))
			want = append(want, "disconnect")
		}
	}
	for i, w := range want {
		if got := r.next(t); got != w {
			t.Fatalf("event %d: got %q, want %q", i, got, w)
		}
	}
}

func TestPeripheralTracksTheCentral(t *testing.T) {
	p, r := startPump(t)
	phone, earbuds := dbus.ObjectPath("/org/bluez/hci0/dev_AA"), dbus.ObjectPath("/org/bluez/hci0/dev_BB")
	if got := p.MTU(); got != 163 {
		t.Fatalf("MTU before a write: %d", got)
	}
	p.intercept(message(dbus.TypeMethodCall, gattTXPath, gattChrcIfc, "StartNotify"))
	p.intercept(write("a", map[string]dbus.Variant{"mtu": dbus.MakeVariant(uint16(256)), "device": dbus.MakeVariant(phone)}))
	if got := r.next(t); got != "write a" {
		t.Fatal(got)
	}
	if got := p.MTU(); got != 256 {
		t.Errorf("MTU after a write: %d", got)
	}

	// Earbuds dropping their own link is not the phone leaving.
	p.intercept(connected(earbuds, false))
	p.intercept(connected(phone, true))
	r.quiet(t)
	if got := p.MTU(); got != 256 {
		t.Errorf("MTU after another device left: %d", got)
	}

	p.intercept(connected(phone, false))
	if got := r.next(t); got != "disconnect" {
		t.Fatal(got)
	}
	if got := p.MTU(); got != 163 {
		t.Errorf("MTU after the phone left: %d", got)
	}
	if err := p.Disconnect(); err != nil {
		t.Errorf("Disconnect with no central: %v", err)
	}
}

// A full queue must not block the connection's reader.
func TestPeripheralDropsWhenFull(t *testing.T) {
	p := NewPeripheral(testPeripheral)
	done := make(chan struct{})
	go func() {
		for range maxLinkEvents + 10 {
			p.intercept(write("x", nil))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("intercept blocked on a full queue")
	}
	if len(p.events) != maxLinkEvents {
		t.Errorf("queued %d, want %d", len(p.events), maxLinkEvents)
	}
}

func TestPeripheralBeforeStartAndAfterClose(t *testing.T) {
	p := NewPeripheral(testPeripheral)
	if err := p.Send([][]byte{{1}}); err == nil {
		t.Error("Send before Start succeeded")
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close before Start: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := p.Send([][]byte{{1}}); err != ErrPeripheralClosed {
		t.Errorf("Send after Close: %v", err)
	}
}
