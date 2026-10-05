//go:build linux

package gadget

import (
	"context"

	"github.com/HuskerMinion/techo5/echod/internal/lib/bluez"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

// On the device the Muse setup service goes on the air through bluetoothd, beside the earbuds and the
// Home Assistant proxy it already serves.
func init() {
	openPeripheral = func(ctx context.Context, name string, h Handler) (pairing.Transport, func(), error) {
		p := bluez.NewPeripheral(bluez.PeripheralConfig{
			LocalName:        name,
			ServiceUUID:      pairing.ServiceUUID,
			RXUUID:           pairing.RXUUID,
			TXUUID:           pairing.TXUUID,
			RXFlags:          pairing.RXFlags,
			TXFlags:          pairing.TXFlags,
			ManufacturerData: map[uint16][]byte{pairing.ManufacturerID: {pairing.ManufacturerData}},
			AssumedMTU:       pairing.AssumedMTU,
			Stagger:          pairing.ChunkStagger,
		})
		if err := p.Start(ctx, h); err != nil {
			return nil, nil, err
		}
		return p, func() { _ = p.Close() }, nil
	}
}
