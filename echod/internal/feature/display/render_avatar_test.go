//go:build !dot && !spot

package display

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/avatar"
)

// standInColor is what the stand-in character is made of: nothing else on a turn screen is this.
var standInColor = color.NRGBA{0, 200, 255, 255}

// standInAvatar is a set to test the layout with, since the real one is nobody's to keep here: every
// frame of every animation a solid square, so anything drawn over the character shows.
func standInAvatar(t testing.TB) *avatar.Set {
	t.Helper()
	dir := t.TempDir()
	manifest := `{"cell": 64, "fps": 12.5, "animations": {`
	for i, name := range []string{avatar.Idle, avatar.Listening, avatar.Thinking, avatar.Talking} {
		if i > 0 {
			manifest += ","
		}
		manifest += fmt.Sprintf(`%q: {"frames": 2, "columns": 2, "loop": true}`, name)
		img := image.NewPaletted(image.Rect(0, 0, 128, 64), color.Palette{standInColor})
		writeAvatarFile(t, filepath.Join(dir, name+".png"), func(f *os.File) error { return png.Encode(f, img) })
	}
	writeAvatarFile(t, filepath.Join(dir, "manifest.json"), func(f *os.File) error {
		_, err := f.WriteString(manifest + "}}")
		return err
	})
	set, err := avatar.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func writeAvatarFile(t testing.TB, path string, write func(*os.File) error) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := write(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// avatarScenes are a Muse turn's pages, without the character: the caller gives them one.
func avatarScenes(now time.Time) map[string]scene {
	long := "The Apollo program was the United States human spaceflight program that landed the first humans on the Moon, " +
		"from 1969 to 1972, with six successful landings and twelve astronauts walking on its surface."
	return map[string]scene{
		"listening": {now: now, phase: "listening", since: now.Add(-300 * time.Millisecond)},
		"thinking":  {now: now, phase: "thinking", since: now.Add(-700 * time.Millisecond), heard: "What's the weather tomorrow?"},
		"replying": {now: now, phase: "replying", since: now, heard: "What's the weather tomorrow?",
			reply: "Tomorrow will be sunny, with a high of 74 and a low of 51."},
		"lingering": {now: now, phase: "lingering", since: now, heard: "Tell me about the Apollo program", reply: long + " " + long},
	}
}

// The words of a turn go beside the character and never over it, on both panels and however long
// the answer is. AVATAR_PREVIEW names a folder to write the pages to, to look at without a device,
// and AVATAR_SPRITES a set made by tools/muse-avatar to draw them with in place of the stand-in.
func TestTheAvatarTurnKeepsTheWordsOffTheCharacter(t *testing.T) {
	standIn := standInAvatar(t)
	shown := standIn
	if dir := os.Getenv("AVATAR_SPRITES"); dir != "" {
		set, err := avatar.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		shown = set
	}
	preview := os.Getenv("AVATAR_PREVIEW")
	now := time.Date(2026, 10, 5, 14, 7, 0, 0, time.Local)
	for _, panel := range []struct {
		name       string
		wide, high int
		scale      int
	}{
		{"", showWide, showHigh, 5},
		{"-show8", show8Wide, show8High, 6},
	} {
		for name, s := range avatarScenes(now) {
			s.avatar = standIn
			img := image.NewRGBA(image.Rect(0, 0, panel.wide, panel.high))
			r := newRenderer(img)
			r.draw(s)
			at, scale, left := r.avatarPlace(standIn)
			if scale != panel.scale {
				t.Fatalf("%s%s: the character is drawn at %d pixels to one, want %d", name, panel.name, scale, panel.scale)
			}
			box := image.Rectangle{Min: at, Max: at.Add(image.Pt(64*scale, 64*scale))}
			if !box.In(img.Rect) {
				t.Fatalf("%s%s: the character is at %v, off the screen", name, panel.name, box)
			}
			want := color.RGBA{standInColor.R, standInColor.G, standInColor.B, 255}
			for y := box.Min.Y; y < box.Max.Y; y++ {
				for x := box.Min.X; x < box.Max.X; x++ {
					if got := img.RGBAAt(x, y); got != want {
						t.Fatalf("%s%s: something is drawn over the character at %d,%d: %v", name, panel.name, x, y, got)
					}
				}
			}
			for y := 0; y < panel.high; y++ {
				for x := box.Max.X; x < left; x++ {
					if got := img.RGBAAt(x, y); got == want {
						t.Fatalf("%s%s: the character runs into the words' column at %d,%d", name, panel.name, x, y)
					}
				}
			}
			if preview == "" {
				continue
			}
			// Drawn once a second and a half earlier, so the picture is of the character mid-animation.
			s.avatar = shown
			r = newRenderer(image.NewRGBA(img.Rect))
			early := s
			early.now = s.now.Add(-1500 * time.Millisecond)
			r.draw(early)
			r.draw(s)
			writeAvatarFile(t, filepath.Join(preview, "avatar-"+name+panel.name+".png"), func(f *os.File) error { return png.Encode(f, r.dst) })
			s.avatar = nil
			r = newRenderer(image.NewRGBA(img.Rect))
			r.draw(s)
			writeAvatarFile(t, filepath.Join(preview, "plain-"+name+panel.name+".png"), func(f *os.File) error { return png.Encode(f, r.dst) })
		}
	}
}

// The character is for a turn: a scene that is not one draws no character, whatever it carries.
func TestNoAvatarOutsideATurn(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 7, 0, 0, time.Local)
	plain := image.NewRGBA(image.Rect(0, 0, showWide, showHigh))
	newRenderer(plain).draw(scene{now: now, phase: "idle"})
	with := image.NewRGBA(plain.Rect)
	newRenderer(with).draw(scene{now: now, phase: "idle", avatar: standInAvatar(t)})
	if string(plain.Pix) != string(with.Pix) {
		t.Error("the clock is drawn differently with a character installed")
	}
}

// One replying frame with the character, the page a Muse turn spends longest on.
func BenchmarkAvatarFrame(b *testing.B) {
	for _, panel := range []struct {
		name       string
		wide, high int
	}{
		{"show5", showWide, showHigh},
		{"show8", show8Wide, show8High},
	} {
		b.Run(panel.name, func(b *testing.B) {
			s := avatarScenes(time.Now())["replying"]
			s.avatar = standInAvatar(b)
			r := newRenderer(image.NewRGBA(image.Rect(0, 0, panel.wide, panel.high)))
			for i := range b.N {
				s.now = s.now.Add(80 * time.Millisecond * time.Duration(i%2))
				r.draw(s)
			}
		})
	}
}
