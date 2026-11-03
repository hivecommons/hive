package evidence

import (
	"strings"
	"testing"
	"time"
)

func TestControlWindowValidationAndSchema(t *testing.T) {
	root := loadSchema(t)
	for _, arm := range []string{"", ControlWindowOn, ControlWindowOff, "unknown", "ON"} {
		t.Run(arm, func(t *testing.T) {
			b := sample()
			b.ControlWindow = arm
			valid := arm == "" || arm == ControlWindowOn || arm == ControlWindowOff
			if err := b.Validate(); (err == nil) != valid {
				t.Fatalf("Validate() = %v, valid=%v", err, valid)
			}
			if errs := validateSchema(root, root, toGeneric(t, b), "$"); (len(errs) == 0) != valid {
				t.Fatalf("schema errors = %v, valid=%v", errs, valid)
			}
			if valid {
				if err := Seal(b, nil); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFoldControlWindowAcrossHeads(t *testing.T) {
	for _, tc := range []struct {
		name string
		arms []string
		want string
	}{
		{"legacy", []string{"", ""}, ""},
		{"preserved", []string{ControlWindowOff, ""}, ControlWindowOff},
		{"same", []string{ControlWindowOn, ControlWindowOn}, ControlWindowOn},
		{"conflicting", []string{ControlWindowOn, ControlWindowOff, ControlWindowOn}, ""},
		{"invalid", []string{"bad", ControlWindowOn}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bundles []*Bundle
			for i, arm := range tc.arms {
				b := sample()
				b.ControlWindow = arm
				b.CreatedAt = b.CreatedAt.Add(time.Duration(i) * time.Hour)
				bundles = append(bundles, b)
			}
			for _, reverse := range []bool{false, true} {
				if reverse {
					for i, j := 0, len(bundles)-1; i < j; i, j = i+1, j-1 {
						bundles[i], bundles[j] = bundles[j], bundles[i]
					}
				}
				for _, p := range FoldPRs(bundles, "", "") {
					if p.ControlWindow != tc.want {
						t.Fatalf("reverse=%v: got %q, want %q", reverse, p.ControlWindow, tc.want)
					}
				}
			}
		})
	}
}

func TestControlWindowIncludedInSealedHash(t *testing.T) {
	b := sample()
	if err := Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	before := b.Hash
	b.ControlWindow = ControlWindowOff
	if err := Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(before, b.Hash) {
		t.Fatal("control assignment must be covered by the bundle hash")
	}
}
