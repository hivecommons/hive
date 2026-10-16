package complexity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const src = `package p

type T[K any] struct{}

func (t *T[K]) M() {}
func (T[K]) V()    {}
func (x *q.R) Odd() {}

func Branchy(a, b int, ch chan int) int {
	if a > 0 && b > 0 || a < b {
		for i := 0; i < a; i++ {
		}
	}
	for range ch {
	}
	switch a {
	case 1:
	case 2:
	default:
	}
	select {
	case <-ch:
	default:
	}
	return 0
}

func Decl()
`

func TestMeasure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/p.go", src)
	writeFile(t, root, "a/p_test.go", "package p\nfunc TestX() {}\n")
	writeFile(t, root, "vendor/v.go", "package v\nfunc V() {}\n")
	writeFile(t, root, "a/bad.go", "not go")
	writeFile(t, root, "a/readme.md", "x")

	s, err := Measure(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if s.Files != 1 || s.Functions != 5 {
		t.Fatalf("files=%d functions=%d", s.Files, s.Functions)
	}
	// if, &&, ||, for, range, 2 cases, 1 comm = 8 branches + 1.
	if s.MaxCyclo != 9 || s.Hotspots[0].Name != "Branchy" || s.Hotspots[0].File != "a/p.go" {
		t.Fatalf("max=%d hotspots=%+v", s.MaxCyclo, s.Hotspots)
	}
	if s.TotalCyclo != 13 || s.MeanCyclo != 2.6 || s.OverThreshold != 1 || s.Threshold != 2 {
		t.Fatalf("%+v", s)
	}
	// Sorted cyclomatics are 1,1,1,1,9: nearest-rank p50 is the 3rd, p90 the 5th.
	if s.MedianCyclo != 1 || s.P90Cyclo != 9 {
		t.Fatalf("median=%d p90=%d", s.MedianCyclo, s.P90Cyclo)
	}
	names := map[string]bool{}
	for _, h := range s.Hotspots {
		names[h.Name] = true
	}
	for _, n := range []string{"T.M", "T.V", "Odd", "Decl"} {
		if !names[n] {
			t.Errorf("missing %s in %v", n, names)
		}
	}
}

func TestMeasureDefaultsAndEmpty(t *testing.T) {
	s, err := Measure(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Threshold != DefaultThreshold || s.Functions != 0 || s.MeanCyclo != 0 {
		t.Fatalf("%+v", s)
	}
	if _, err := Measure(filepath.Join(t.TempDir(), "missing"), 0); err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestHotspotsCapped(t *testing.T) {
	root := t.TempDir()
	body := "package p\n"
	for i := 0; i < 12; i++ {
		body += "func F" + string(rune('a'+i)) + "() {}\n"
	}
	writeFile(t, root, "p.go", body)
	writeFile(t, root, "q.go", "package p\nfunc Fa() {}\n")
	s, err := Measure(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Hotspots) != maxHotspots {
		t.Fatalf("hotspots=%d", len(s.Hotspots))
	}
	// Ties sort by file then name: p.go's 10 lexically-first functions fill the cap.
	if s.Hotspots[0].Name != "Fa" || s.Hotspots[1].Name != "Fb" || s.Hotspots[9].File != "p.go" {
		t.Fatalf("tie order: %+v", s.Hotspots)
	}
}

func TestCountFindings(t *testing.T) {
	out := "# pkg\n./a.go:3:4: bad thing\nb/c.go:10:2: other: x\nnote: not a finding\nfoo.go:x:y: no\nfoo.go:1\n\nREADME:1:2: no\n"
	if n := CountFindings(out); n != 2 {
		t.Fatalf("n=%d", n)
	}
}

func TestAddWarnings(t *testing.T) {
	var s Snapshot
	s.AddWarnings("vet", 2)
	s.AddWarnings("vet", 1)
	s.AddWarnings("lint", 4)
	if s.Warnings != 7 || s.WarningsByTool["vet"] != 3 {
		t.Fatalf("%+v", s)
	}
}

func TestPathValidation(t *testing.T) {
	for _, repo := range []string{"", "x", "/y", "x/", "a/b/c", "../b", "a/.."} {
		if _, err := Path("/r", repo, "2026-10"); err == nil {
			t.Errorf("repo %q accepted", repo)
		}
	}
	if _, err := Path("/r", "a/b", "2026-13"); err == nil {
		t.Error("bad month accepted")
	}
	p, err := Path("/r", "a/b", "2026-10")
	if err != nil || p != filepath.Join("/r", "a", "b", "complexity", "2026-10.json") {
		t.Fatalf("%q %v", p, err)
	}
}

func TestWriteLoadNoOverwrite(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC)
	s := Snapshot{SchemaVersion: SchemaVersion, Repo: "o/r", Month: Month(at.In(time.FixedZone("x", -4*3600))), CapturedAt: at, Functions: 3}
	p, err := Write(root, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || got.Functions != 3 || got.Month != "2026-10" || !got.CapturedAt.Equal(at) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := Write(root, s); !errors.Is(err, ErrExists) {
		t.Fatalf("err=%v", err)
	}
	if _, err := Write(root, Snapshot{Repo: "bad", Month: "2026-10"}); err == nil {
		t.Fatal("expected invalid repo error")
	}
	if _, err := Load(filepath.Join(root, "nope.json")); err == nil {
		t.Fatal("expected load error")
	}
}

func TestWriteMkdirFails(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "o", "file")
	if _, err := Write(root, Snapshot{Repo: "o/r", Month: "2026-10"}); err == nil {
		t.Fatal("expected mkdir error")
	}
}
