package hub

import "testing"

// TestImageTagIsMutable pins which image references a rollout restart can
// actually advance. Getting this wrong in the "mutable" direction is the
// expensive one: a SHA-pinned hive would restart forever without ever changing
// image (the torch-spyre failure), so anything uncertain must classify as
// immutable and take the image-patch path.
func TestImageTagIsMutable(t *testing.T) {
	cases := []struct {
		image string
		want  bool
		why   string
	}{
		{"ghcr.io/hivecommons/hive:v2-latest", true, "mutable branch tag"},
		{"ghcr.io/hivecommons/hive:v3-latest", true, "mutable branch tag"},
		{"ghcr.io/hivecommons/hive:mk-latest", true, "personal branch tag is republished too"},
		{"ghcr.io/hivecommons/hive:63d8902", false, "SHA pin — restart is a no-op"},
		{"ghcr.io/hivecommons/hive:v2.1.0", false, "release tag is immutable"},
		{"ghcr.io/hivecommons/hive:stable", true, "release channel is a retagged digest — mutable"},
		{"ghcr.io/hivecommons/hive:candidate", true, "release channel is a retagged digest — mutable"},
		{"ghcr.io/hivecommons/hive:edge", true, "release channel is a retagged digest — mutable"},
		{"ghcr.io/hivecommons/hive@sha256:abc123", false, "digest pin"},
		{"ghcr.io/hivecommons/hive", false, "no tag — do not guess"},
		{"", false, "empty"},
		// Registry ports must not be mistaken for tags.
		{"registry.internal:5000/kubestellar/hive", false, "port, not a tag"},
		{"registry.internal:5000/kubestellar/hive:v2-latest", true, "port plus mutable tag"},
		{"registry.internal:5000/kubestellar/hive:63d8902", false, "port plus SHA pin"},
	}
	for _, c := range cases {
		if got := imageTagIsMutable(c.image); got != c.want {
			t.Errorf("imageTagIsMutable(%q) = %v, want %v (%s)", c.image, got, c.want, c.why)
		}
	}
}
