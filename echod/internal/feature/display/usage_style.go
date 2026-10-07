//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/HuskerMinion/techo5/echod/internal/feature/meters"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// The Usage clock style: the board of meters something on the network sends (feature/meters), drawn
// as a title and a card for each meter, with its share, a bar, and how long until it starts over. The
// time keeps a corner. It has its own dark ground and colors, whatever the theme, and a mark beside
// the title if the owner put a picture at usageMarkFile: the software carries none.

const styleUsage = "usage"

func init() {
	clockStyles = append(clockStyles, struct{ label, value string }{"Usage", styleUsage})
}

// usageMarkFile is the picture drawn beside the title, a PNG, if there is one.
var usageMarkFile = filepath.Join(layout.StateDir, "meters-mark.png")

// usageStale is how old a board is before the screen says when it came instead of its own words.
const usageStale = 15 * time.Minute

var (
	usageGround = color.RGBA{0x14, 0x14, 0x13, 0xff}
	usageCard   = color.RGBA{0x1f, 0x1e, 0x1d, 0xff}
	usageTrack  = color.RGBA{0x30, 0x30, 0x2e, 0xff}
	usageInk    = color.RGBA{0xfa, 0xf9, 0xf5, 0xff}
	usageSoft   = color.RGBA{0xc2, 0xc0, 0xb6, 0xff}
	usageFaint  = color.RGBA{0x87, 0x86, 0x7f, 0xff}
	usageFill   = color.RGBA{0x7a, 0xb5, 0x5c, 0xff}
	usageAccent = color.RGBA{0xd9, 0x77, 0x57, 0xff}
)

// usageBoard is where the board comes from; a test puts its own here.
var usageBoard = func() (meters.Board, bool) { return meters.Get().Board() }

// followMeters has a board that arrives drawn at once.
func (d *Display) followMeters() {
	meters.Get().Changed.Listen(func(struct{}) { d.wake() })
}

var (
	usageMarkOnce sync.Once
	usageMarkImg  image.Image
)

// usageMark is the owner's picture, read once: a file put there later shows after a restart.
func usageMark() image.Image {
	usageMarkOnce.Do(func() {
		f, err := os.Open(usageMarkFile)
		if err != nil {
			return
		}
		defer f.Close()
		cfg, err := png.DecodeConfig(f)
		if err != nil || cfg.Width > 1024 || cfg.Height > 1024 {
			slog.Warn("usage: the mark is not a PNG of 1024 pixels or fewer a side", "file", usageMarkFile, "err", err)
			return
		}
		if _, err := f.Seek(0, 0); err != nil {
			return
		}
		if img, err := png.Decode(f); err == nil {
			usageMarkImg = img
		}
	})
	return usageMarkImg
}

// untilReset is how long until a meter starts over, as the screen says it: days and hours, hours and
// minutes, or minutes.
func untilReset(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m >= 24*60:
		return fmt.Sprintf("%dd %dh", m/(24*60), m%(24*60)/60)
	case m >= 60:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	default:
		return fmt.Sprintf("%dm", max(m, 1))
	}
}

// since is how long ago a board came, in the largest unit that fits.
func since(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm ago", max(int(d/time.Minute), 1))
	}
}

// usageStyle draws the whole screen.
func (r *renderer) usageStyle(s scene) {
	draw.Draw(r.dst, r.dst.Rect, image.NewUniform(usageGround), image.Point{}, draw.Src)
	board, ok := usageBoard()

	head := r.styleFace(true, 54)
	title := board.Title
	if title == "" {
		title = "Usage"
	}
	base := r.s(66)
	r.text(head, title, (r.w-r.width(head, title))/2, base, usageInk)
	if mark := usageMark(); mark != nil {
		side := r.s(60)
		b := mark.Bounds()
		w := side * b.Dx() / max(b.Dy(), 1)
		at := image.Rect(r.margin, base-r.s(50), r.margin+w, base-r.s(50)+side)
		xdraw.CatmullRom.Scale(r.dst, at, mark, b, draw.Over, nil)
	}
	now := clockText(s.now)
	r.text(r.small, now, r.w-r.margin-r.width(r.small, now), base-r.s(8), usageSoft)

	foot := r.h - r.s(18)
	area := image.Rect(r.margin, r.s(96), r.w-r.margin, r.h-r.s(58))
	if !ok {
		const wait = "Waiting for numbers"
		r.text(r.title, wait, (r.w-r.width(r.title, wait))/2, area.Min.Y+area.Dy()/2, usageFaint)
		return
	}

	gap := r.s(12)
	n := len(board.Rows)
	each := (area.Dy() - gap*(n-1)) / n
	for i, row := range board.Rows {
		top := area.Min.Y + i*(each+gap)
		r.usageRow(row, image.Rect(area.Min.X, top, area.Max.X, top+each), s.now)
	}

	note, ink := board.Note, usageAccent
	if age := s.now.Sub(board.At); age >= usageStale {
		note, ink = "Updated "+since(age), usageFaint
	} else if note != "" {
		note = "* " + note
	}
	for _, t := range s.timers {
		if t.Active {
			r.timersLine(s, foot)
			return
		}
	}
	r.text(r.tiny, note, (r.w-r.width(r.tiny, note))/2, foot, ink)
}

// usageRow is one meter's card: its share and its name on one line, the bar under them, and under
// that how long until it starts over. A meter past that time has started over, whatever was last
// sent, and is drawn empty.
func (r *renderer) usageRow(row meters.Row, box image.Rectangle, now time.Time) {
	// Sizes follow the card, in the Show 5's pixels, so three or four meters still fit.
	h := box.Dy() * r.sDenOr1() / r.sNumOr1()
	r.roundFill(box, float64(r.s(18)), usageCard, usageCard)
	pad := r.s(20)
	figure := r.styleFace(true, h*34/100)
	words := r.styleFace(true, h*17/100)
	under := r.styleFace(false, h*17/100)

	share, when := row.Percent, ""
	if row.ResetsAt > 0 {
		if left := time.Unix(row.ResetsAt, 0).Sub(now); left > 0 {
			when = "Resets in " + untilReset(left)
		} else {
			share, when = 0, "Has started over"
		}
	}

	line := box.Min.Y + r.s(h*40/100)
	r.text(figure, fmt.Sprintf("%.0f%%", share), box.Min.X+pad, line, usageInk)
	if row.Label != "" {
		lw := r.width(words, row.Label)
		pill := image.Rect(box.Max.X-pad-lw-r.s(32), line-r.s(h*26/100), box.Max.X-pad, line+r.s(h*6/100))
		r.roundFill(pill, float64(pill.Dy())/2, usageTrack, usageTrack)
		r.text(words, row.Label, pill.Min.X+r.s(16), line-r.s(h*4/100), usageInk)
	}

	bar := image.Rect(box.Min.X+pad, line+r.s(h*12/100), box.Max.X-pad, line+r.s(h*24/100))
	rad := float64(bar.Dy()) / 2
	r.roundFill(bar, rad, usageTrack, usageTrack)
	if share > 0 {
		lit := bar
		lit.Max.X = bar.Min.X + max(bar.Dy(), int(float64(bar.Dx())*share/100))
		r.roundFill(lit, rad, usageFill, usageFill)
	}
	if when != "" {
		r.text(under, when, box.Min.X+pad, box.Max.Y-r.s(h*10/100), usageSoft)
	}
}
