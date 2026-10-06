// Package avatar plays a character from sprite sheets: one PNG per animation, its frames side by
// side in a grid, and a manifest saying how many there are and how fast they go. It is what puts the
// Muse character on the Show's turn screen (feature/display/render_avatar.go).
//
// The sheets are made on a computer by tools/muse-avatar and copied to the device; none are built
// in. It imports nothing of the device, so it builds and is tested anywhere.
package avatar

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"time"
)

// The animations a set has to have, by the names the manifest gives them.
const (
	Idle      = "idle"
	Listening = "listening"
	Thinking  = "thinking"
	Talking   = "talking"
)

// phases is which animation each phase of a turn plays, the phases as the screen names them: the
// conversation's own, and lingering for the answer left up after it.
var phases = map[string]string{
	"listening": Listening,
	"thinking":  Thinking,
	"replying":  Talking,
	"lingering": Idle,
}

// ForPhase is the animation a turn's phase plays, and whether it plays one at all: idle, when there
// is no turn, does not.
func ForPhase(phase string) (string, bool) {
	name, ok := phases[phase]
	return name, ok
}

// Limits on what a manifest may ask for. A set is a megabyte or so; the ceiling is there so a
// manifest that is wrong, or somebody else's, cannot take the memory of a device with one gigabyte.
const (
	maxCell     = 512
	maxFrames   = 2000
	maxFPS      = 60
	MaxBytes    = 128 << 20
	manifestMax = 64 << 10
)

// Manifest is manifest.json: what the sheets beside it hold.
type Manifest struct {
	// Cell is a frame's width and height in pixels, as drawn by the character's own renderer.
	Cell int `json:"cell"`
	// FPS is how many frames a second the animations were captured at.
	FPS float64 `json:"fps"`
	// Grid is how bright the last row and column of each pixel are drawn once a frame is scaled up,
	// 0 to 1: the grid the character is seen through on a gadget's own display. 0 or 1 is no grid.
	Grid float64 `json:"grid,omitempty"`
	// Animations are the sheets, by name: idle.png holds "idle".
	Animations map[string]Animation `json:"animations"`
}

// Animation is one sheet: Frames frames, Columns to a row, left to right and then down.
type Animation struct {
	Frames  int  `json:"frames"`
	Columns int  `json:"columns"`
	Loop    bool `json:"loop"`
}

// Parse reads a manifest and refuses one the player could not trust: a set missing an animation a
// turn needs, or sizes that make no picture or too large a one.
func Parse(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if m.Cell < 1 || m.Cell > maxCell {
		return Manifest{}, fmt.Errorf("manifest: a cell of %d pixels", m.Cell)
	}
	if !(m.FPS > 0 && m.FPS <= maxFPS) {
		return Manifest{}, fmt.Errorf("manifest: %v frames a second", m.FPS)
	}
	if !(m.Grid >= 0 && m.Grid <= 1) {
		return Manifest{}, fmt.Errorf("manifest: a grid of %v", m.Grid)
	}
	for _, name := range phases {
		if _, ok := m.Animations[name]; !ok {
			return Manifest{}, fmt.Errorf("manifest: no %q animation", name)
		}
	}
	total := 0
	for name, a := range m.Animations {
		if !plainName(name) {
			return Manifest{}, fmt.Errorf("manifest: an animation named %q", name)
		}
		if a.Frames < 1 || a.Frames > maxFrames || a.Columns < 1 {
			return Manifest{}, fmt.Errorf("manifest: %q has %d frames in rows of %d", name, a.Frames, a.Columns)
		}
		total += a.Size(m.Cell).X * a.Size(m.Cell).Y
		if total > MaxBytes {
			return Manifest{}, errors.New("manifest: the sheets are more than the player will hold")
		}
	}
	return m, nil
}

// plainName is whether name is safe to make a file name of: an animation's name is one.
func plainName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Size is how big the animation's sheet is, in pixels, for cells of cell.
func (a Animation) Size(cell int) image.Point {
	rows := (a.Frames + a.Columns - 1) / a.Columns
	return image.Pt(a.Columns*cell, rows*cell)
}

// Rect is where frame i sits in the sheet, for cells of cell.
func (a Animation) Rect(cell, i int) image.Rectangle {
	x, y := (i%a.Columns)*cell, (i/a.Columns)*cell
	return image.Rect(x, y, x+cell, y+cell)
}

// FrameAt is the frame showing once the animation has run for elapsed at fps: around again when it
// loops, held on the last frame when it does not.
func (a Animation) FrameAt(fps float64, elapsed time.Duration) int {
	i := int(elapsed.Seconds() * fps)
	if i < 0 {
		return 0
	}
	if a.Loop {
		return i % a.Frames
	}
	return min(i, a.Frames-1)
}

// Interval is how long one frame stays up: how often a screen showing the character has to redraw.
func (m Manifest) Interval() time.Duration {
	return time.Duration(float64(time.Second) / m.FPS)
}

// Playhead is where a character is in its animation: which one, and since when. An animation starts
// from its first frame when it becomes the one playing and runs by the clock from there, so a frame
// drawn late skips ahead instead of slowing the character down.
type Playhead struct {
	name  string
	since time.Time
}

// Frame is the frame of the named animation to draw at now.
func (p *Playhead) Frame(m Manifest, name string, now time.Time) int {
	if name != p.name || now.Before(p.since) {
		p.name, p.since = name, now
	}
	return m.Animations[name].FrameAt(m.FPS, now.Sub(p.since))
}

// Until is how long from now the frame after the one Frame last gave is due: what a screen that has
// just drawn waits before it draws again. Waiting a whole frame's length instead adds the time the
// drawing took to every frame, and the character then skips one now and again to keep to the clock.
func (p *Playhead) Until(m Manifest, now time.Time) time.Duration {
	every := m.Interval()
	if p.name == "" || now.Before(p.since) || every <= 0 {
		return every
	}
	return every - now.Sub(p.since)%every
}
