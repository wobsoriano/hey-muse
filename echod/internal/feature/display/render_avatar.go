//go:build !dot && !spot

package display

import (
	"image"
	"log/slog"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
	"github.com/HuskerMinion/techo5/echod/internal/lib/avatar"
)

// The Muse character on the turn screen: when Muse is what answers and a set of its sprite sheets is
// installed, a turn is drawn with the character on the left, listening, thinking and talking as the
// turn does, and the words in a column beside it. The sheets are not part of the software: the
// character is Meta's, so whoever wants it makes them from their own copy of Meta's SDK
// (tools/muse-avatar) and puts them in avatarDir. With none there, a turn looks as it always has.

// avatarDir is where an installed set lives: with the device's own state, which an update leaves
// alone.
const avatarDir = layout.StateDir + "/muse-avatar"

// Where the character goes, in the Show 5's pixels: a square down the left of the screen, centered
// top to bottom, and the gap between it and the words. 320 is five screen pixels to each of the
// character's 64, the size Meta's own preview draws it at, and two thirds of the screen's height;
// the sheets' edges are clear, so the square sits closer to the screen's edge than the words do.
const (
	avatarSideBase = 320
	avatarEdgeBase = 20
	avatarGapBase  = 20
)

// loadAvatar reads the installed set, if there is one, and says which it was.
func loadAvatar() *avatar.Set {
	set, err := avatar.Load(avatarDir)
	if err != nil {
		slog.Info("no Muse avatar installed: turns are drawn without one", "dir", avatarDir, "err", err)
		return nil
	}
	slog.Info("Muse avatar loaded", "dir", avatarDir, "animations", len(set.Animations), "bytes", set.Bytes())
	return set
}

// avatarOn is whether turns are Muse's, whose character it is.
func avatarOn() bool { return config.Get().Brain.Mode == config.BrainMuse }

// avatarPlace is where the character goes on this screen: its top left corner, how many screen
// pixels to each of its own, and where the words beside it start. The scale is a whole number, so
// the character is centered in its square instead of filling it: on a Show 8 that is six, where the
// layout's own scale would ask for six and two thirds.
func (r *renderer) avatarPlace(set *avatar.Set) (at image.Point, scale, left int) {
	side, edge := r.s(avatarSideBase), r.s(avatarEdgeBase)
	scale = set.Scale(side)
	inset := (side - set.Cell*scale) / 2
	return image.Pt(edge+inset, (r.h-side)/2+inset), scale, edge + side + r.s(avatarGapBase)
}

// turnAvatar draws the character for the turn's phase, when the scene has one, and says where the
// words beside it start: at the margin, as ever, when there is no character.
func (r *renderer) turnAvatar(s scene) int {
	name, turn := avatar.ForPhase(s.phase)
	if s.avatar == nil || !turn {
		return r.margin
	}
	at, scale, left := r.avatarPlace(s.avatar)
	s.avatar.Draw(r.dst, at, scale, name, r.av.Frame(s.avatar.Manifest, name, s.now))
	return left
}
