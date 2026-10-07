//go:build !dot && !spot

package display

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/meters"
)

func countOf(img *image.RGBA, c color.RGBA) int {
	n := 0
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] == c.R && img.Pix[i+1] == c.G && img.Pix[i+2] == c.B {
			n++
		}
	}
	return n
}

// The Usage style fills each bar by its meter's share. A meter whose time to start over has passed
// is drawn empty, and a board that is old says when it came in place of its own words.
func TestTheUsageStyleDrawsTheBoard(t *testing.T) {
	now := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	board := meters.Board{Title: "Usage", Note: "Opus 5.5", At: now.Add(-time.Minute), Rows: []meters.Row{
		{Label: "Current", Percent: 21, ResetsAt: now.Add(4*time.Hour + 19*time.Minute).Unix()},
		{Label: "Weekly", Percent: 5, ResetsAt: now.Add(6*24*time.Hour + 5*time.Hour).Unix()},
	}}
	old := usageBoard
	defer func() { usageBoard = old }()
	usageBoard = func() (meters.Board, bool) { return board, true }

	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))
	s := scene{now: now}
	s.style.kind, s.style.chosen = styleUsage, true
	r.draw(s)
	lit, noted := countOf(r.dst, usageFill), countOf(r.dst, usageAccent)
	if lit == 0 || noted == 0 {
		t.Fatalf("%d pixels of bar and %d of the note", lit, noted)
	}
	if out := os.Getenv("USAGE_PREVIEW"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, r.dst); err != nil {
			t.Fatal(err)
		}
	}

	board.Rows[0].Percent, board.Rows[1].Percent = 42, 10
	r.draw(s)
	if more := countOf(r.dst, usageFill); more < lit*3/2 {
		t.Errorf("twice the share lit %d pixels, where once lit %d", more, lit)
	}

	later := s
	later.now = now.Add(7 * 24 * time.Hour)
	r.draw(later)
	if left := countOf(r.dst, usageFill); left != 0 {
		t.Errorf("%d pixels of bar are lit after both meters started over", left)
	}
	if countOf(r.dst, usageAccent) != 0 {
		t.Error("a week-old board still shows its note as news")
	}
}

func TestUntilReset(t *testing.T) {
	for d, want := range map[time.Duration]string{
		6*24*time.Hour + 5*time.Hour + 20*time.Minute: "6d 5h",
		4*time.Hour + 19*time.Minute:                  "4h 19m",
		19 * time.Minute:                              "19m",
		10 * time.Second:                              "1m",
	} {
		if got := untilReset(d); got != want {
			t.Errorf("%v reads %q, want %q", d, got, want)
		}
	}
}
