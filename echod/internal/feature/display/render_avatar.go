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
// installed, a turn is drawn as the character alone, in the middle of the screen, listening, thinking
// and talking as the turn does. No words go with it: the answer is spoken. The sheets are not part of
// the software: the character is Meta's, so whoever wants it makes them from their own copy of Meta's
// SDK (tools/muse-avatar) and puts them in avatarDir. With none there, a turn looks as it always has.

// avatarDir is where an installed set lives: with the device's own state, which an update leaves
// alone.
const avatarDir = layout.StateDir + "/muse-avatar"

// avatarEdgeBase is the least room left above and below the character, in the Show 5's pixels.
const avatarEdgeBase = 20

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

// avatarPlace is where the character goes on this screen: its top left corner, and how many screen
// pixels to each of its own. It is as large as fits the height in whole pixels, which keeps its edges
// sharp, and is centered: six to one on a Show 5, where it stands 384 pixels of the 480.
func (r *renderer) avatarPlace(set *avatar.Set) (at image.Point, scale int) {
	scale = set.Scale(r.h - 2*r.s(avatarEdgeBase))
	side := set.Cell * scale
	return image.Pt((r.w-side)/2, (r.h-side)/2), scale
}

// turnAvatar draws the character for the turn's phase, and says whether the scene had one to draw.
func (r *renderer) turnAvatar(s scene) bool {
	name, turn := avatar.ForPhase(s.phase)
	if s.avatar == nil || !turn {
		return false
	}
	at, scale := r.avatarPlace(s.avatar)
	s.avatar.Draw(r.dst, at, scale, name, r.av.Frame(s.avatar.Manifest, name, s.now))
	return true
}
