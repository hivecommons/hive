package outcomebench

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// IBM Plex Mono is distributed under the SIL OFL in fonts/OFL.txt.
//
//go:embed fonts/IBMPlexMono-Regular.ttf
var regularTTF []byte

//go:embed fonts/IBMPlexMono-Bold.ttf
var boldTTF []byte

var cardFonts = sync.OnceValue(func() [2]*opentype.Font {
	var fonts [2]*opentype.Font
	for i, data := range [][]byte{regularTTF, boldTTF} {
		f, err := opentype.Parse(data)
		if err != nil {
			panic(err)
		} // embedded, reviewed font assets
		fonts[i] = f
	}
	return fonts
})

type drawOp struct {
	Kind                     string
	X, Y, W, H, Radius, Size int
	Fill, Text               string
	Bold, End                bool
}

type drawing struct {
	Title  string
	Height int
	Ops    []drawOp
}

// SVG and PNG consume the same ordered operations, in display pixels.
func (d *drawing) SVG() string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s" font-family="%s">`+"\n<title>%s</title>\n", cardW*cardScale, d.Height*cardScale, cardW, d.Height, esc(d.Title), cardFont, esc(d.Title))
	// Font changes also invalidate PNG source hashes. Version the raster recipe
	// here if its rendering changes without changing the drawing operations.
	fmt.Fprintf(&b, "<!-- raster: v1; regular: %x; bold: %x -->\n", sha256.Sum256(regularTTF), sha256.Sum256(boldTTF))
	for _, op := range d.Ops {
		switch op.Kind {
		case "text":
			extra := ""
			if op.Bold {
				extra += ` font-weight="700"`
			}
			if op.End {
				extra += ` text-anchor="end"`
			}
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="%d"%s>%s</text>`+"\n", op.X, op.Y, op.Fill, op.Size, extra, esc(op.Text))
		case "rect":
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="%d" fill="%s"/>`+"\n", op.X, op.Y, op.W, op.H, op.Radius, op.Fill)
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}

func ink(hex string) *image.Uniform {
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic(err)
	}
	return image.NewUniform(color.RGBA{r, g, b, 255})
}

func (d *drawing) PNG() ([]byte, error) {
	dst := image.NewRGBA(image.Rect(0, 0, cardW*cardScale, d.Height*cardScale))
	type faceKey struct {
		size int
		bold bool
	}
	faces := map[faceKey]font.Face{}
	defer func() {
		for _, face := range faces {
			_ = face.Close()
		}
	}()
	for _, op := range d.Ops {
		switch op.Kind {
		case "rect":
			roundedRect(dst, op)
		case "text":
			key := faceKey{op.Size, op.Bold}
			face := faces[key]
			if face == nil {
				idx := 0
				if op.Bold {
					idx = 1
				}
				var err error
				face, err = opentype.NewFace(cardFonts()[idx], &opentype.FaceOptions{Size: float64(op.Size * cardScale), DPI: 72, Hinting: font.HintingNone})
				if err != nil {
					return nil, err
				}
				faces[key] = face
			}
			pen := font.Drawer{Dst: dst, Src: ink(op.Fill), Face: face, Dot: fixed.P(op.X*cardScale, op.Y*cardScale)}
			if op.End {
				pen.Dot.X -= pen.MeasureString(op.Text)
			}
			pen.DrawString(op.Text)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, dst); err != nil {
		return nil, err
	}
	// Insert a standard ancillary tEXt chunk before IEND, leaving pixels alone.
	raw := b.Bytes()
	payload := []byte(fmt.Sprintf("outcomebench-source\x00%x", sha256.Sum256([]byte(d.SVG()))))
	chunk := make([]byte, len(payload)+12)
	binary.BigEndian.PutUint32(chunk, uint32(len(payload)))
	copy(chunk[4:], "tEXt")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	out := append([]byte{}, raw[:len(raw)-12]...)
	out = append(out, chunk...)
	return append(out, raw[len(raw)-12:]...), nil
}

func roundedRect(dst *image.RGBA, op drawOp) {
	x, y, w, h, r := float32(op.X*cardScale), float32(op.Y*cardScale), float32(op.W*cardScale), float32(op.H*cardScale), float32(op.Radius*cardScale)
	if w <= 0 || h <= 0 {
		return
	}
	if r > w/2 {
		r = w / 2
	}
	if r > h/2 {
		r = h / 2
	}
	z := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	const k = 0.55228475
	z.MoveTo(x+r, y)
	z.LineTo(x+w-r, y)
	z.CubeTo(x+w-r+k*r, y, x+w, y+r-k*r, x+w, y+r)
	z.LineTo(x+w, y+h-r)
	z.CubeTo(x+w, y+h-r+k*r, x+w-r+k*r, y+h, x+w-r, y+h)
	z.LineTo(x+r, y+h)
	z.CubeTo(x+r-k*r, y+h, x, y+h-r+k*r, x, y+h-r)
	z.LineTo(x, y+r)
	z.CubeTo(x, y+r-k*r, x+r-k*r, y, x+r, y)
	z.ClosePath()
	z.DrawOp = draw.Over
	z.Draw(dst, dst.Bounds(), ink(op.Fill), image.Point{})
}

// CardPNGs renders the README raster assets without a browser or system fonts.
func (r Report) CardPNGs() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, th := range cardThemes {
		for _, name := range CardNames() {
			if d, ok := r.cardDrawing(name, th); ok {
				data, err := d.PNG()
				if err != nil {
					return nil, err
				}
				out[strings.TrimSuffix(r.CardFile(name, th.Name), ".svg")+".png"] = data
			}
		}
	}
	return out, nil
}

// ValidateCardPNG checks provenance and size, not rasterizer-specific pixels.
func ValidateCardPNG(data []byte, svg string) error {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	var w, h int
	if _, err := fmt.Sscanf(svg, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d"`, &w, &h); err != nil {
		return err
	}
	if cfg.Width != w || cfg.Height != h {
		return fmt.Errorf("PNG dimensions %dx%d, want %dx%d", cfg.Width, cfg.Height, w, h)
	}
	want := fmt.Sprintf("outcomebench-source\x00%x", sha256.Sum256([]byte(svg)))
	for pos := 8; pos+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		if n > len(data)-pos-12 {
			return fmt.Errorf("truncated PNG chunk")
		}
		chunk := data[pos+4 : pos+8+n]
		if string(chunk[:4]) == "tEXt" && string(chunk[4:]) == want {
			if binary.BigEndian.Uint32(data[pos+8+n:pos+12+n]) != crc32.ChecksumIEEE(chunk) {
				return fmt.Errorf("invalid source chunk CRC")
			}
			return nil
		}
		pos += n + 12
	}
	return fmt.Errorf("PNG source hash missing or stale")
}
