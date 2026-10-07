//go:build !dot && !spot

package display

import (
	"image"
	"path/filepath"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
)

// The splash is the name, centered. Where Muse answers and its character is installed, the character
// stands over the name and both still fit, with room under them for the two waiting lines.
func TestTheSplashIsTheNameWithTheCharacterOverIt(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	set := standInAvatar(t)
	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))

	alone := r.splashPlace(set)
	if alone.set != nil {
		t.Error("the character is drawn where Muse does not answer")
	}
	if mid := alone.nameX + r.width(alone.face, markHey+markMuse)/2; mid < 478 || mid > 482 {
		t.Errorf("the name is centered on %d of 960", mid)
	}

	if err := config.Set().Brain().Set(config.Brain{Mode: config.BrainMuse}); err != nil {
		t.Fatal(err)
	}
	with := r.splashPlace(set)
	side := set.Cell * with.scale
	if with.set == nil || with.at.Y < 0 || with.at.Y+side >= with.baseline-with.face.Metrics().CapHeight.Round() {
		t.Errorf("the character at %v, %d high, and the name on %d overlap or leave the screen", with.at, side, with.baseline)
	}
	if with.below+26+32 > 480 {
		t.Errorf("the mark ends at %d, which leaves no room for the waiting lines", with.below)
	}

	r.drawSplash(set, time.Unix(0, 0), waitingAfter)
	seen := map[[3]uint8]bool{}
	for i := 0; i < len(r.dst.Pix); i += 4 {
		seen[[3]uint8{r.dst.Pix[i], r.dst.Pix[i+1], r.dst.Pix[i+2]}] = true
	}
	if !seen[[3]uint8{cream.R, cream.G, cream.B}] || !seen[[3]uint8{teal.R, teal.G, teal.B}] {
		t.Error("the name is not drawn in its two colors")
	}
}
