package outcomebench

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func TestCardPNGProvenance(t *testing.T) {
	d := statCard(cardThemes[0], "Example", "Accuracy", "One figure, drawn with the embedded font.", stat{"3 in 4", "came back fixed"}, "Test data.")
	data, err := d.PNG()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCardPNG(data, d.SVG()); err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCardPNG(data, strings.Replace(d.SVG(), "3 in 4", "2 in 3", 1)); err == nil {
		t.Error("accepted stale source")
	}
	if err := ValidateCardPNG(data[:20], d.SVG()); err == nil {
		t.Error("accepted truncated PNG")
	}
	var plain bytes.Buffer
	if err := png.Encode(&plain, image.NewRGBA(image.Rect(0, 0, cardW*cardScale, d.Height*cardScale))); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCardPNG(plain.Bytes(), d.SVG()); err == nil {
		t.Error("accepted PNG without source hash")
	}
	if err := ValidateCardPNG(data, strings.Replace(d.SVG(), `height="1200"`, `height="1202"`, 1)); err == nil {
		t.Error("accepted wrong dimensions")
	}
	bad := append([]byte{}, data...)
	i := bytes.Index(bad, []byte("outcomebench-source"))
	bad[i] = 'X'
	if err := ValidateCardPNG(bad, d.SVG()); err == nil {
		t.Error("accepted corrupt source chunk")
	}
}

// Measure using the actual embedded faces: a fallback font must never decide
// whether committed card copy fits. Also exercise all optional stat cards.
func TestCardLayout(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "benchmarks", "outcomes-2026-10.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed Report
	if err := json.Unmarshal(data, &committed); err != nil {
		t.Fatal(err)
	}
	since, until, _ := ParseWindow("2026-10")
	fixture := Compute(fixture(), 0, Options{Since: since, Until: until, Now: now, ReadmeMinBase: 10, Spend: &Spend{USD: 26, MergedPRs: 13}})
	for _, rep := range []Report{committed, fixture} {
		for _, th := range cardThemes {
			for _, name := range CardNames() {
				d, ok := rep.cardDrawing(name, th)
				if !ok {
					continue
				}
				heroes := 0
				for _, op := range d.Ops {
					if op.Kind != "text" {
						continue
					}
					if op.Size == bigPx {
						heroes++
					}
					idx := 0
					if op.Bold {
						idx = 1
					}
					face, err := opentype.NewFace(cardFonts()[idx], &opentype.FaceOptions{Size: float64(op.Size), DPI: 72})
					if err != nil {
						t.Fatal(err)
					}
					width := font.MeasureString(face, op.Text)
					_ = face.Close()
					left := fixed.I(op.X)
					if op.End {
						left -= width
					}
					if left < fixed.I(cardPad) || left+width > fixed.I(cardW-cardPad) || op.Y-op.Size < cardPad || op.Y > d.Height-cardPad {
						t.Errorf("%s/%s: text outside padding: %+v, width=%v, height=%d", name, th.Name, op, width, d.Height)
					}
				}
				switch name {
				case CardAccuracy, CardChecksGreen, CardPromptInjection, CardCost:
					if heroes != 1 || d.Height != 600 {
						t.Errorf("%s: %d hero figures, height=%d", name, heroes, d.Height)
					}
				}
			}
		}
	}
}
