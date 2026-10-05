package avatar

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
)

// Set is a character ready to draw: its manifest, and every sheet decoded once. The sheets stay as
// they are stored, one byte a pixel at the character's own size, and are scaled as they are drawn;
// kept at the size they are shown they would be twenty-five times the memory.
type Set struct {
	Manifest
	sheets map[string]*sheet
}

// sheet is one animation's frames, with its colors worked out for drawing: ink for a pixel, shade
// for the grid's darker edge of it, and seen for whether the color shows at all.
type sheet struct {
	pix   *image.Paletted
	ink   [256]color.RGBA
	shade [256]color.RGBA
	seen  [256]bool
}

// Load reads the set in dir. An error that Is os.ErrNotExist means there is no set there at all.
func Load(dir string) (*Set, error) {
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, manifestMax))
	f.Close()
	if err != nil {
		return nil, err
	}
	m, err := Parse(b)
	if err != nil {
		return nil, err
	}
	s := &Set{Manifest: m, sheets: make(map[string]*sheet, len(m.Animations))}
	for name, a := range m.Animations {
		sh, err := loadSheet(filepath.Join(dir, name+".png"), a.Size(m.Cell), m.Grid)
		if err != nil {
			return nil, fmt.Errorf("%s.png: %w", name, err)
		}
		s.sheets[name] = sh
	}
	return s, nil
}

// loadSheet decodes one sheet, which has to be the size its manifest says: the header is read first,
// so a picture that is not is turned away before any of it is decoded.
func loadSheet(path string, want image.Point, grid float64) (*sheet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if cfg.Width != want.X || cfg.Height != want.Y {
		return nil, fmt.Errorf("is %dx%d, and its manifest says %dx%d", cfg.Width, cfg.Height, want.X, want.Y)
	}
	if _, ok := cfg.ColorModel.(color.Palette); !ok {
		return nil, errors.New("is not a paletted PNG")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	pix := img.(*image.Paletted)
	sh := &sheet{pix: pix}
	if grid == 0 {
		grid = 1
	}
	for i, c := range pix.Palette {
		n := color.NRGBAModel.Convert(c).(color.NRGBA)
		// Clear or solid: a sheet's edges are pixels, and a pixel half there has nothing to blend with.
		sh.seen[i] = n.A >= 0x80
		sh.ink[i] = color.RGBA{n.R, n.G, n.B, 0xff}
		sh.shade[i] = color.RGBA{uint8(float64(n.R) * grid), uint8(float64(n.G) * grid), uint8(float64(n.B) * grid), 0xff}
	}
	return sh, nil
}

// Bytes is how much memory the decoded sheets take.
func (s *Set) Bytes() int {
	n := 0
	for _, sh := range s.sheets {
		n += len(sh.pix.Pix)
	}
	return n
}

// Scale is the largest whole number of screen pixels to a sprite pixel at which a frame fits in a
// square of side, and never less than one: whole, so every pixel of the character stays square.
func (s *Set) Scale(side int) int {
	return max(1, side/s.Cell)
}

// Draw puts frame i of the named animation on dst with its top left corner at at, every pixel of it
// scale wide. Clear pixels leave dst as it was. A frame that would not land wholly on dst is not
// drawn.
func (s *Set) Draw(dst *image.RGBA, at image.Point, scale int, name string, i int) {
	sh, a := s.sheets[name], s.Animations[name]
	box := image.Rectangle{Min: at, Max: at.Add(image.Pt(s.Cell*scale, s.Cell*scale))}
	if sh == nil || scale < 1 || i < 0 || i >= a.Frames || !box.In(dst.Rect) {
		return
	}
	src := a.Rect(s.Cell, i)
	// Too small to see a grid through: at one or two pixels across, the edge would be the pixel.
	edge := scale
	if scale >= 3 && s.Grid > 0 && s.Grid < 1 {
		edge = scale - 1
	}
	for sy := range s.Cell {
		row := sh.pix.Pix[sh.pix.PixOffset(src.Min.X, src.Min.Y+sy):]
		for sx := range s.Cell {
			c := row[sx]
			if !sh.seen[c] {
				continue
			}
			ink, shade := sh.ink[c], sh.shade[c]
			for dy := range scale {
				o := dst.PixOffset(at.X+sx*scale, at.Y+sy*scale+dy)
				px := dst.Pix[o : o+scale*4 : o+scale*4]
				for dx := range scale {
					v := ink
					if dx >= edge || dy >= edge {
						v = shade
					}
					px[dx*4], px[dx*4+1], px[dx*4+2], px[dx*4+3] = v.R, v.G, v.B, 0xff
				}
			}
		}
	}
}
