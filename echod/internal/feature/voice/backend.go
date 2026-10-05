package voice

import (
	esphome "github.com/ygelfand/go-esphome-device"

	"github.com/HuskerMinion/techo5/echod/internal/config"
)

// backend is what a turn runs against: Home Assistant's Assist pipeline over the ESPHome connection,
// the device's own direct pipeline (direct.go), or Muse (muse.go). Each reports back through the
// conversation's events - heard, reply text, reply audio, run end, error - so everything else about
// a turn, the listening, the ducking, the ring and the screen, is the same whichever answers it.
//
// Its methods are called in order from the conversation's send queue, one at a time.
type backend interface {
	// Ready is whether a turn can start now.
	Ready() bool
	// Start opens a turn; phrase is the wake word that opened it.
	Start(phrase string) error
	// Audio is one frame of the microphone: 16 kHz, 16-bit little-endian, mono.
	Audio(frame []byte) error
	// End says the speaker has finished.
	End() error
	// Stop closes the turn, whatever it was doing.
	Stop() error
	// Name is for the log.
	Name() string
}

// ha is Home Assistant's pipeline.
type ha struct{ vs *esphome.VoiceSatellite }

func (h ha) Ready() bool               { return h.vs.Subscribed() }
func (h ha) Start(phrase string) error { return h.vs.StartTurn(phrase, audioSettings()) }
func (h ha) Audio(frame []byte) error  { return h.vs.SendAudio(frame) }
func (h ha) End() error                { return h.vs.EndAudio() }
func (h ha) Stop() error               { return h.vs.StopTurn() }
func (h ha) Name() string              { return "home assistant" }

// backendFor is which one a new turn runs against: the direct pipeline when it is chosen and set up,
// Muse when it is chosen, Home Assistant otherwise.
func (c *conversation) backendFor() backend {
	b := config.Get().Brain
	switch {
	case b.Direct():
		return c.direct
	case b.Mode == config.BrainMuse:
		return c.muse
	}
	return c.ha
}
