//go:build !dot && !spot

package display

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"time"

	"golang.org/x/image/font"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/lib/avatar"
)

// The HeyMuse mark, drawn while the device comes up: the name, and over it the Muse character where
// Muse answers and a set of its pictures is installed, with signal arcs pulsing outward both ways
// until the device can answer. The name is drawn in the device's own type and the character is the
// owner's copy (render_avatar.go), so the software carries no picture of either.

const (
	// markHey and markMuse are the name's two halves, set as one word in two colors.
	markHey, markMuse = "Hey", "Muse"

	// markAvatarShare is how much of the screen's height the character may take, in hundredths:
	// what leaves room for the name under it and, on a slow start, the two lines under that.
	markAvatarShare = 55

	// markGapBase is the room between the character and the name, and arcClearBase how far past
	// the mark's edge the arcs begin, both in the Show 5's pixels.
	markGapBase  = 14
	arcClearBase = 24

	// The pulse: arcs leave the mark and fade out arcReach pixels on, arcCount of them in flight,
	// one full sweep every arcPeriod.
	arcReach  = 280.0
	arcCount  = 3
	arcPeriod = 2400 * time.Millisecond
	arcWidth  = 5.0
	// arcSpread is how far above and below the horizontal the arcs reach, in radians.
	arcSpread = 38 * math.Pi / 180

	// waitingAfter is when the splash starts saying what it is waiting for: long enough that a
	// device which is already adopted is up and gone before it shows, short enough that nobody
	// sits watching a screen that looks stuck.
	waitingAfter = 12 * time.Second

	// splashMin is the least the splash is shown, so a fast connection still shows the mark.
	splashMin = 4 * time.Second

	// noAddressWait is how long after the start a device with no network address waits before the Wi-Fi
	// page opens by itself, ending the splash: long enough for a lease on a slow network, short enough
	// that a fresh unit, or one in a house it has no network for, is not left on the splash.
	noAddressWait = 45 * time.Second

	// noHomeAssistantWait is how long a device with no Home Assistant access waits on the splash for
	// Home Assistant to add it before showing the clock: a device already in a Home Assistant is
	// usually listening well inside it.
	noHomeAssistantWait = 60 * time.Second
)

var (
	// navy is the mark's own background, so it sits on the screen without a rectangle.
	navy = color.RGBA{28, 31, 36, 255}
	teal = color.RGBA{20, 147, 180, 255}
)

// splash is where the mark sits on this screen: the character, if there is one to draw, the name's
// baseline, where the arcs leave from, and the first row under it all.
type splash struct {
	set      *avatar.Set // nil when the name stands alone
	face     font.Face   // the name's: large alone, smaller under the character
	at       image.Point // the character's top left corner
	scale    int
	nameX    int
	baseline int
	cx, cy   float64 // arc center
	from     float64 // how far from the center the arcs start
	below    int
}

// splashPlace lays the mark out, centered: the name alone, or the character with the name under it.
func (r *renderer) splashPlace(set *avatar.Set) splash {
	s := splash{face: r.mark}
	if set == nil || !avatarOn() {
		s.face = r.big
	}
	m := s.face.Metrics()
	rise, drop := m.CapHeight.Round(), m.Descent.Round()
	if rise <= 0 {
		rise = m.Ascent.Round()
	}
	wide := r.width(s.face, markHey+markMuse)
	s.nameX = (r.w - wide) / 2
	if s.face == r.big {
		s.baseline = (r.h + rise) / 2
		s.cx, s.cy = float64(r.w)/2, float64(s.baseline)-float64(rise)/2
		s.from = float64(wide)/2 + float64(r.s(arcClearBase))
		s.below = s.baseline + drop
		return s
	}
	s.set, s.scale = set, set.Scale(r.h*markAvatarShare/100)
	side := set.Cell * s.scale
	top := (r.h - side - r.s(markGapBase) - rise) / 2
	s.at = image.Pt((r.w-side)/2, top)
	s.baseline = top + side + r.s(markGapBase) + rise
	s.cx, s.cy = float64(r.w)/2, float64(top)+float64(side)/2
	s.from = float64(side)/2 + float64(r.s(arcClearBase))
	s.below = s.baseline + drop
	return s
}

// drawSplash paints the splash as it stands elapsed into its animation.
func (r *renderer) drawSplash(set *avatar.Set, now time.Time, elapsed time.Duration) {
	draw.Draw(r.dst, r.dst.Rect, image.NewUniform(navy), image.Point{}, draw.Src)
	s := r.splashPlace(set)
	r.arcs(s, elapsed)
	if s.set != nil {
		s.set.Draw(r.dst, s.at, s.scale, avatar.Idle, r.av.Frame(s.set.Manifest, avatar.Idle, now))
	}
	r.text(s.face, markHey, s.nameX, s.baseline, cream)
	r.text(s.face, markMuse, s.nameX+r.width(s.face, markHey), s.baseline, teal)
	if elapsed >= waitingAfter {
		r.splashWaiting(s)
	}
}

// splashWaiting says what the splash is waiting for, once it has been up long enough that waiting is
// the explanation.
//
// The mark stays until Home Assistant subscribes, which does not happen until somebody accepts the
// device there. Between flashing a unit and adopting it that can be minutes, or as long as it takes
// to walk to a computer, and the screen said nothing at all - so it reads as a device that has hung
// on its first boot. Somebody sat in front of one for ten minutes before finding out that accepting
// the ESPHome prompt was what freed it (techo5-checkers issue #2). It costs two lines to say so.
//
// Not said from the first frame: a device that is adopted already reaches Home Assistant in a couple
// of seconds, and telling that owner to go and do something they did not need to do would be worse
// than saying nothing.
func (r *renderer) splashWaiting(s splash) {
	what, where := "Waiting for Home Assistant", "Settings > Devices & services > ESPHome"
	if config.Get().Brain.Mode == config.BrainMuse {
		what, where = "Waiting for Muse", "Pair it in the Muse app if this stays"
	}
	// Under the mark, in what room is left. If a panel leaves less than the two lines need, they sit
	// on the bottom edge instead of climbing onto the name.
	const gap, lead = 26, 32
	y := s.below + gap
	if bottom := r.h - 6; y+lead > bottom {
		y = bottom - lead
	}
	r.text(r.tiny, what, (r.w-r.width(r.tiny, what))/2, y, teal)
	r.text(r.tiny, where, (r.w-r.width(r.tiny, where))/2, y+lead, dim)
}

// arcs draws arcCount rings expanding from the mark to both sides, each fading as it travels.
// Only the band the arcs can reach is scanned, and only pixels near a ring are touched.
func (r *renderer) arcs(s splash, elapsed time.Duration) {
	phase := math.Mod(elapsed.Seconds()/arcPeriod.Seconds(), 1)
	var radii [arcCount]float64
	var fades [arcCount]float64
	for i := range arcCount {
		p := math.Mod(phase+float64(i)/arcCount, 1)
		radii[i] = s.from + p*arcReach
		fades[i] = (1 - p) * (1 - p) // brighter near the mark, gone at the edge
	}
	reach := s.from + arcReach
	x0 := max(int(s.cx-reach-arcWidth), 0)
	x1 := min(int(s.cx+reach+arcWidth), r.w-1)
	y0 := max(int(s.cy-reach*math.Sin(arcSpread)-arcWidth), 0)
	y1 := min(int(s.cy+reach*math.Sin(arcSpread)+arcWidth), r.h-1)
	pix := r.dst.Pix
	for y := y0; y <= y1; y++ {
		dy := float64(y) - s.cy
		for x := x0; x <= x1; x++ {
			dx := float64(x) - s.cx
			if math.Abs(dy) > math.Abs(dx)*math.Tan(arcSpread) {
				continue // outside the two sideways cones
			}
			d := math.Hypot(dx, dy)
			for i := range arcCount {
				w := math.Abs(d - radii[i])
				if w > arcWidth {
					continue
				}
				a := (1 - w/arcWidth) * fades[i]
				// soften the cone edges
				edge := 1 - math.Abs(dy)/(math.Abs(dx)*math.Tan(arcSpread)+1e-9)
				a *= math.Min(edge*4, 1)
				o := r.dst.PixOffset(x, y)
				pix[o] = blend(pix[o], teal.R, a)
				pix[o+1] = blend(pix[o+1], teal.G, a)
				pix[o+2] = blend(pix[o+2], teal.B, a)
			}
		}
	}
}

func blend(under, over uint8, a float64) uint8 {
	return uint8(float64(under)*(1-a) + float64(over)*a + 0.5)
}
