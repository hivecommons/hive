package outcomebench

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// The README embeds PNGs, not SVGs: GitHub serves SVG text in whatever
// monospace the reader's machine has, and filters render differently per
// browser. The PNG is drawn here from the same ops as the SVG with an
// embedded face (IBM Plex Mono, SIL OFL 1.1, fonts/OFL.txt), at cardScale
// so GitHub downsamples it crisp. No external renderer is involved.

//go:embed fonts/IBMPlexMono-Regular.ttf fonts/IBMPlexMono-Medium.ttf fonts/IBMPlexMono-Bold.ttf
var fontFS embed.FS

var (
	fontOnce sync.Once
	fontErr  error
	fonts    = map[int]*sfnt.Font{}
)

func loadFonts() error {
	fontOnce.Do(func() {
		for weight, file := range map[int]string{400: "Regular", 500: "Medium", 700: "Bold"} {
			b, err := fontFS.ReadFile("fonts/IBMPlexMono-" + file + ".ttf")
			if err != nil {
				fontErr = err
				return
			}
			f, err := sfnt.Parse(b)
			if err != nil {
				fontErr = fmt.Errorf("parsing %s: %w", file, err)
				return
			}
			fonts[weight] = f
		}
	})
	return fontErr
}

// PNGSourceKey is the tEXt keyword under which a card PNG records the
// SHA-256 of the SVG it was rendered alongside.
const PNGSourceKey = "hive-card-source"

// SourceHash is the hex SHA-256 of a card's SVG, the value stored in its
// PNG so a stale PNG can be told from a fresh one.
func SourceHash(svg string) string {
	sum := sha256.Sum256([]byte(svg))
	return hex.EncodeToString(sum[:])
}

// PNG rasterises the card at cardScale and tags it with SourceHash(SVG()).
func (c *card) PNG() ([]byte, error) {
	if err := loadFonts(); err != nil {
		return nil, err
	}
	const s = cardScale
	img := image.NewNRGBA(image.Rect(0, 0, cardW*s, c.h*s))
	bg, border := hexColor(c.th.BG), hexColor(c.th.Border)
	fillRoundRect(img, 0, 0, float64(cardW*s), float64(c.h*s), cardRadius*s, border)
	fillRoundRect(img, s, s, float64(cardW*s-2*s), float64(c.h*s-2*s), cardRadius*s-s, bg)
	faces := map[[2]int]font.Face{}
	face := func(weight, px int) (font.Face, error) {
		k := [2]int{weight, px}
		if f, ok := faces[k]; ok {
			return f, nil
		}
		ft, ok := fonts[weight]
		if !ok {
			return nil, fmt.Errorf("no embedded face for weight %d", weight)
		}
		f, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: float64(px * s), DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			return nil, err
		}
		faces[k] = f
		return f, nil
	}
	for _, o := range c.ops {
		switch o.kind {
		case 't':
			f, err := face(o.style.Weight, o.px)
			if err != nil {
				return nil, err
			}
			drawText(img, f, o, s)
		case 'l':
			draw.Draw(img, image.Rect(o.x*s, o.y*s, (o.x+o.w)*s, o.y*s+s), image.NewUniform(hexColor(o.stroke)), image.Point{}, draw.Over)
		case 'c':
			fillCircle(img, float64(o.x*s), float64(o.y*s), float64(o.rx*s), hexColor(o.fill))
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return addPNGText(buf.Bytes(), PNGSourceKey, SourceHash(c.SVG()))
}

// drawText draws one text op glyph by glyph so letter-spacing and end
// anchoring work the same as in the SVG.
func drawText(dst draw.Image, f font.Face, o op, s int) {
	track := fixed.Int26_6(o.style.Track * float64(s) * 64)
	runes := []rune(o.s)
	width := fixed.Int26_6(0)
	for i, r := range runes {
		adv, _ := f.GlyphAdvance(r)
		width += adv
		if i < len(runes)-1 {
			width += track
		}
	}
	x := fixed.I(o.x * s)
	if o.style.End {
		x -= width
	}
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(hexColor(o.fill)), Face: f, Dot: fixed.Point26_6{X: x, Y: fixed.I(o.y * s)}}
	for _, r := range runes {
		d.DrawString(string(r))
		d.Dot.X += track
	}
}

// fillRoundRect fills an axis-aligned rounded rectangle with antialiasing.
func fillRoundRect(dst *image.NRGBA, x, y, w, h float64, r float64, col color.Color) {
	const k = 0.5522847498 // cubic Bézier circle constant
	z := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	z.MoveTo(float32(x+r), float32(y))
	z.LineTo(float32(x+w-r), float32(y))
	z.CubeTo(float32(x+w-r+r*k), float32(y), float32(x+w), float32(y+r-r*k), float32(x+w), float32(y+r))
	z.LineTo(float32(x+w), float32(y+h-r))
	z.CubeTo(float32(x+w), float32(y+h-r+r*k), float32(x+w-r+r*k), float32(y+h), float32(x+w-r), float32(y+h))
	z.LineTo(float32(x+r), float32(y+h))
	z.CubeTo(float32(x+r-r*k), float32(y+h), float32(x), float32(y+h-r+r*k), float32(x), float32(y+h-r))
	z.LineTo(float32(x), float32(y+r))
	z.CubeTo(float32(x), float32(y+r-r*k), float32(x+r-r*k), float32(y), float32(x+r), float32(y))
	z.ClosePath()
	z.Draw(dst, dst.Bounds(), image.NewUniform(col), image.Point{})
}

func fillCircle(dst *image.NRGBA, cx, cy, r float64, col color.Color) {
	const k = 0.5522847498
	z := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	z.MoveTo(float32(cx+r), float32(cy))
	z.CubeTo(float32(cx+r), float32(cy+r*k), float32(cx+r*k), float32(cy+r), float32(cx), float32(cy+r))
	z.CubeTo(float32(cx-r*k), float32(cy+r), float32(cx-r), float32(cy+r*k), float32(cx-r), float32(cy))
	z.CubeTo(float32(cx-r), float32(cy-r*k), float32(cx-r*k), float32(cy-r), float32(cx), float32(cy-r))
	z.CubeTo(float32(cx+r*k), float32(cy-r), float32(cx+r), float32(cy-r*k), float32(cx+r), float32(cy))
	z.ClosePath()
	z.Draw(dst, dst.Bounds(), image.NewUniform(col), image.Point{})
}

// hexColor parses "#rrggbb"; anything else is opaque magenta so a typo is
// visible in the output rather than silently black.
func hexColor(s string) color.NRGBA {
	var r, g, b uint8
	if len(s) == 7 && s[0] == '#' {
		if _, err := fmt.Sscanf(s[1:], "%02x%02x%02x", &r, &g, &b); err == nil {
			return color.NRGBA{R: r, G: g, B: b, A: 255}
		}
	}
	return color.NRGBA{R: 255, A: 255, B: 255}
}

// addPNGText inserts a tEXt chunk before IEND.
func addPNGText(data []byte, key, val string) ([]byte, error) {
	const iendLen = 12
	if len(data) < iendLen || string(data[len(data)-8:len(data)-4]) != "IEND" {
		return nil, errors.New("png: no IEND chunk")
	}
	payload := append(append([]byte(key), 0), []byte(val)...)
	chunk := make([]byte, 0, 12+len(payload))
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(payload)))
	chunk = append(chunk, "tEXt"...)
	chunk = append(chunk, payload...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := make([]byte, 0, len(data)+len(chunk))
	out = append(out, data[:len(data)-iendLen]...)
	out = append(out, chunk...)
	out = append(out, data[len(data)-iendLen:]...)
	return out, nil
}

// PNGText returns the value of a tEXt chunk with the given keyword, or "".
func PNGText(data []byte, key string) string {
	const sig = 8
	for i := sig; i+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		if i+12+n > len(data) {
			return ""
		}
		if typ == "tEXt" {
			body := data[i+8 : i+8+n]
			if j := bytes.IndexByte(body, 0); j >= 0 && string(body[:j]) == key {
				return string(body[j+1:])
			}
		}
		if typ == "IEND" {
			return ""
		}
		i += 12 + n
	}
	return ""
}

// PNGSize reads the IHDR dimensions.
func PNGSize(data []byte) (w, h int, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}
