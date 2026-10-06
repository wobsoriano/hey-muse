package avatar

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const good = `{"cell": 2, "fps": 12.5, "grid": 0.5, "animations": {
	"idle": {"frames": 3, "columns": 2, "loop": true},
	"listening": {"frames": 3, "columns": 2, "loop": true},
	"thinking": {"frames": 3, "columns": 2, "loop": true},
	"talking": {"frames": 3, "columns": 2, "loop": false}}}`

func TestAManifestIsReadOrRefused(t *testing.T) {
	m, err := Parse([]byte(good))
	if err != nil || m.Cell != 2 || m.Animations[Talking].Frames != 3 || m.Interval() != 80*time.Millisecond {
		t.Fatalf("a good manifest read as %+v, %v", m, err)
	}
	for why, bad := range map[string]string{
		"not JSON":             `{`,
		"no cell":              strings.Replace(good, `"cell": 2`, `"cell": 0`, 1),
		"a huge cell":          strings.Replace(good, `"cell": 2`, `"cell": 4096`, 1),
		"no frame rate":        strings.Replace(good, `"fps": 12.5`, `"fps": 0`, 1),
		"a grid above one":     strings.Replace(good, `"grid": 0.5`, `"grid": 3`, 1),
		"an animation missing": strings.Replace(good, `"thinking"`, `"dreaming"`, 1),
		"no frames":            strings.Replace(good, `"frames": 3`, `"frames": 0`, 1),
		"no columns":           strings.Replace(good, `"columns": 2`, `"columns": 0`, 1),
		"a name that is a path": strings.Replace(good, `"animations": {`,
			`"animations": {"../../etc/passwd": {"frames": 1, "columns": 1},`, 1),
		"more than the player holds": strings.Replace(strings.ReplaceAll(good, `"frames": 3`, `"frames": 2000`),
			`"cell": 2`, `"cell": 256`, 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("a manifest with %s was accepted", why)
		}
	}
}

func TestEveryPhaseOfATurnHasItsAnimation(t *testing.T) {
	for phase, want := range map[string]string{
		"listening": Listening, "thinking": Thinking, "replying": Talking, "lingering": Idle,
	} {
		if got, ok := ForPhase(phase); !ok || got != want {
			t.Errorf("%s plays %q, %v; want %q", phase, got, ok, want)
		}
	}
	if name, ok := ForPhase("idle"); ok {
		t.Errorf("no turn plays %q", name)
	}
}

func TestFramesFollowTheClock(t *testing.T) {
	loop, once := Animation{Frames: 5, Columns: 2, Loop: true}, Animation{Frames: 5, Columns: 2}
	for _, c := range []struct {
		after      time.Duration
		loop, once int
	}{
		{-time.Second, 0, 0}, {0, 0, 0}, {79 * time.Millisecond, 0, 0}, {80 * time.Millisecond, 1, 1},
		{330 * time.Millisecond, 4, 4}, {400 * time.Millisecond, 0, 4}, {time.Hour, 0, 4},
	} {
		if got := loop.FrameAt(12.5, c.after); got != c.loop {
			t.Errorf("a loop after %v is on frame %d, want %d", c.after, got, c.loop)
		}
		if got := once.FrameAt(12.5, c.after); got != c.once {
			t.Errorf("a one-shot after %v is on frame %d, want %d", c.after, got, c.once)
		}
	}
	if r := loop.Rect(64, 3); r != image.Rect(64, 64, 128, 128) {
		t.Errorf("frame 3 of rows of 2 is at %v", r)
	}
	if s := loop.Size(64); s != image.Pt(128, 192) {
		t.Errorf("5 frames in rows of 2 make a sheet of %v", s)
	}
}

// An animation starts over when it becomes the one playing, and not because it was drawn again: a
// reply arriving a sentence at a time redraws the screen without the character starting its
// sentence over.
func TestAnAnimationRestartsOnlyWhenItChanges(t *testing.T) {
	m, _ := Parse([]byte(good))
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	var p Playhead
	if f := p.Frame(m, Listening, at); f != 0 {
		t.Fatalf("listening began on frame %d", f)
	}
	if f := p.Frame(m, Listening, at.Add(170*time.Millisecond)); f != 2 {
		t.Errorf("listening 170 ms in is on frame %d, want 2", f)
	}
	if f := p.Frame(m, Thinking, at.Add(170*time.Millisecond)); f != 0 {
		t.Errorf("thinking began on frame %d", f)
	}
	if f := p.Frame(m, Thinking, at.Add(260*time.Millisecond)); f != 1 {
		t.Errorf("thinking 90 ms in is on frame %d, want 1", f)
	}
}

// writeSet writes the good manifest and its four sheets to a folder: each frame is clear at its top
// left and red, with the frame's number in the green, everywhere else.
func writeSet(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := Parse([]byte(good))
	pal := color.Palette{color.NRGBA{}}
	for i := range 3 {
		pal = append(pal, color.NRGBA{200, uint8(100 * i), 0, 255})
	}
	for name, a := range m.Animations {
		size := a.Size(m.Cell)
		img := image.NewPaletted(image.Rect(0, 0, size.X, size.Y), pal)
		for i := range a.Frames {
			r := a.Rect(m.Cell, i)
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					if x != r.Min.X || y != r.Min.Y {
						img.SetColorIndex(x, y, uint8(i+1))
					}
				}
			}
		}
		writePNG(t, filepath.Join(dir, name+".png"), img)
	}
	return dir
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestASetDrawsItsFramesScaledAndCutOut(t *testing.T) {
	s, err := Load(writeSet(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Bytes() != 4*4*4 {
		t.Errorf("four sheets of 4x4 take %d bytes", s.Bytes())
	}
	if s.Scale(7) != 3 || s.Scale(1) != 1 {
		t.Errorf("a cell of 2 fits a square of 7 at %d and of 1 at %d", s.Scale(7), s.Scale(1))
	}
	ground := color.RGBA{1, 2, 3, 255}
	canvas := func() *image.RGBA {
		dst := image.NewRGBA(image.Rect(0, 0, 10, 10))
		for i := 0; i < len(dst.Pix); i += 4 {
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = ground.R, ground.G, ground.B, ground.A
		}
		return dst
	}

	dst := canvas()
	s.Draw(dst, image.Pt(2, 2), 3, Talking, 2)
	for p, want := range map[image.Point]color.RGBA{
		{1, 1}: ground,             // outside the frame
		{2, 2}: ground,             // the frame's clear pixel
		{4, 4}: ground,             // and the far corner of it, scaled
		{5, 2}: {200, 200, 0, 255}, // frame 2's color
		{7, 3}: {100, 100, 0, 255}, // the grid's darker last column
		{6, 7}: {100, 100, 0, 255}, // and last row
		{8, 8}: ground,             // past the frame
	} {
		if got := dst.RGBAAt(p.X, p.Y); got != want {
			t.Errorf("the pixel at %v is %v, want %v", p, got, want)
		}
	}

	for why, draw := range map[string]func(*image.RGBA){
		"hanging off the canvas": func(d *image.RGBA) { s.Draw(d, image.Pt(6, 6), 3, Talking, 0) },
		"past its last frame":    func(d *image.RGBA) { s.Draw(d, image.Pt(0, 0), 3, Talking, 3) },
		"of no animation":        func(d *image.RGBA) { s.Draw(d, image.Pt(0, 0), 3, "dancing", 0) },
	} {
		dst := canvas()
		draw(dst)
		for i := 0; i < len(dst.Pix); i += 4 {
			if dst.Pix[i] != ground.R {
				t.Errorf("a frame %s was drawn", why)
				break
			}
		}
	}
}

func TestASetThatIsNotThereOrNotRightIsRefused(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "muse-avatar")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder that is not there read as %v", err)
	}

	missing := writeSet(t)
	os.Remove(filepath.Join(missing, "thinking.png"))
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), "thinking.png") {
		t.Errorf("a set with a sheet missing read as %v", err)
	}

	wrongSize := writeSet(t)
	writePNG(t, filepath.Join(wrongSize, "idle.png"), image.NewPaletted(image.Rect(0, 0, 640, 640), color.Palette{color.NRGBA{}}))
	if _, err := Load(wrongSize); err == nil || !strings.Contains(err.Error(), "640x640") {
		t.Errorf("a sheet of the wrong size read as %v", err)
	}

	truecolor := writeSet(t)
	writePNG(t, filepath.Join(truecolor, "idle.png"), image.NewRGBA(image.Rect(0, 0, 4, 4)))
	if _, err := Load(truecolor); err == nil || !strings.Contains(err.Error(), "paletted") {
		t.Errorf("a sheet with no palette read as %v", err)
	}
}

// A character cut out of a video has a soft edge: a color the palette makes half there is laid half
// over what is behind it, and one wholly there replaces it, as a drawn character's pixels do.
func TestASoftColorIsLaidOverWhatIsBehindIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"cell": 2, "fps": 12, "animations": {
		"idle": {"frames": 1, "columns": 1, "loop": true}, "listening": {"frames": 1, "columns": 1, "loop": true},
		"thinking": {"frames": 1, "columns": 1, "loop": true}, "talking": {"frames": 1, "columns": 1, "loop": true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	img := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{
		color.NRGBA{}, color.NRGBA{200, 100, 0, 0x80}, color.NRGBA{200, 100, 0, 0xff},
	})
	img.Pix = []uint8{0, 1, 2, 1}
	for _, name := range []string{Idle, Listening, Thinking, Talking} {
		writePNG(t, filepath.Join(dir, name+".png"), img)
	}
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	dst := image.NewRGBA(image.Rect(0, 0, 2, 2))
	behind := color.RGBA{0, 100, 200, 0xff}
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = behind.R, behind.G, behind.B, behind.A
	}
	set.Draw(dst, image.Point{}, 1, Idle, 0)
	if got := dst.RGBAAt(0, 0); got != behind {
		t.Errorf("a clear pixel changed what was behind it to %v", got)
	}
	if got, want := dst.RGBAAt(1, 0), (color.RGBA{100, 100, 100, 0xff}); got != want {
		t.Errorf("a half-there pixel over %v came out %v, want %v", behind, got, want)
	}
	if got, want := dst.RGBAAt(0, 1), (color.RGBA{200, 100, 0, 0xff}); got != want {
		t.Errorf("a solid pixel came out %v, want %v", got, want)
	}
}

// A set named by MUSE_AVATAR_SET, such as one made from videos, loads within the limits and draws.
func TestTheSetNamedLoads(t *testing.T) {
	dir := os.Getenv("MUSE_AVATAR_SET")
	if dir == "" {
		t.Skip("MUSE_AVATAR_SET names no set")
	}
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cell %d, %.1f MB decoded, %v a frame", set.Cell, float64(set.Bytes())/1048576, set.Interval())
	dst := image.NewRGBA(image.Rect(0, 0, 960, 480))
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = 0x0e, 0x1a, 0x18, 0xff
	}
	scale := set.Scale(440)
	side := set.Cell * scale
	set.Draw(dst, image.Pt((960-side)/2, (480-side)/2), scale, Talking, 20)
	if out := os.Getenv("MUSE_AVATAR_PNG"); out != "" {
		writePNG(t, out, dst)
	}
}
