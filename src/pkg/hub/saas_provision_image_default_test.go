package hub

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A hosted spoke MUST be provisioned on v4.
//
// AUDIT F2 FOLLOW-UP. verifyHeartbeatBearer no longer accepts the fleet-wide
// bearer (deleted in the F2 cutover). The per-hive bearer reaches a spoke either
// by injection at provision time, or by SpokeHeartbeatKey() self-deriving it
// from HIVE_HUB_SECRET + HIVE_ID — and that self-derive path exists ONLY in the
// v4 tree. A v2-tagged spoke can therefore do neither: it cannot authenticate to
// this hub at all.
//
// Both image-tag defaults previously read "v2-latest", so a cluster with no
// explicit image_tag (which is the live configuration — the hub's data carries
// no imageTag anywhere) would provision a hive that is dead on arrival, with no
// error at provision time. These tests assert the INVARIANT in source, because
// the failure mode is a default quietly reverting, not a logic bug.

// TestProvisioningDefaultsToV4 is the regression: no default may name a v2 tag.
func TestProvisioningDefaultsToV4(t *testing.T) {
	src := readProvisionSource(t)

	// Every ImageTag default and every fallback assignment must be v4.
	for _, bad := range []string{
		`ImageTag: "v2-latest"`,
		`ImageTag:     "v2-latest"`,
		`imageTag = "v2-latest"`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("saas_provision.go still defaults to v2 (%q) — a spoke provisioned on v2 "+
				"cannot heartbeat since the F2 fleet-wide lane was deleted", bad)
		}
	}

	if !strings.Contains(src, `imageTag = "v4-latest"`) {
		t.Error("the image-tag fallback is not v4-latest")
	}
}

// TestNoV2ImageTagAssignmentsRemain is the blunt backstop: catch any NEW v2
// default someone adds later, in a form the exact-string checks above miss.
func TestNoV2ImageTagAssignmentsRemain(t *testing.T) {
	src := readProvisionSource(t)
	re := regexp.MustCompile(`(?i)image_?tag\s*[:=]\s*"v2-latest"`)
	if m := re.FindAllString(src, -1); len(m) > 0 {
		t.Errorf("found %d v2-latest image-tag assignment(s): %v", len(m), m)
	}
}

// TestV4TagIsNotHardcodedOverClusterConfig is the positive control. The fix must
// only change the FALLBACK — an explicitly configured cluster.ImageTag has to
// keep winning. Without this, "hardcode v4 everywhere" would satisfy both tests
// above while removing the operator's ability to pin a tag at all.
func TestV4TagIsNotHardcodedOverClusterConfig(t *testing.T) {
	src := readProvisionSource(t)
	if !strings.Contains(src, "imageTag := cluster.ImageTag") {
		t.Error("cluster.ImageTag must still be consulted first — the fix is to the fallback only, " +
			"not a hardcoded tag that ignores cluster configuration")
	}
}

func readProvisionSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("saas_provision.go")
	if err != nil {
		t.Fatalf("read saas_provision.go: %v", err)
	}
	return string(b)
}
