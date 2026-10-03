package config

import (
	"strings"
	"testing"
)

// hivecommons/hive#10299: the relay's quota guard evaluates a reading, and the
// standalone contributor launch paths started nothing that produced one — only
// a full `hive` process did. A contributor running the image or local mode
// therefore got "not guarding anything" and an admit, unless it maintained a
// custom reader. These guards pin the wiring that closes that: each standalone
// launch path must START the publisher, and the image must SHIP it.

// The image must build the publisher and ship the binary. A COPY with no
// builder stage (or a builder stage nothing copies from) leaves the entrypoint
// launching something that is not there.
func TestContributorDockerfileShipsQuotaPublisher(t *testing.T) {
	dockerfile := readRepoFile(t, "src", "Dockerfile.contributor")
	for _, want := range []string{
		"AS quota-publisher-builder",
		"go build -o /hive-quota-publisher ./cmd/hive-quota-publisher",
		"COPY --from=quota-publisher-builder /hive-quota-publisher /usr/local/bin/hive-quota-publisher",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q — the contributor image would ship no quota publisher "+
				"and the relay's guard would have no reading source (hivecommons/hive#10299)", want)
		}
	}
}

// The container entrypoint (`just contribute-hive <cli>` and the isolated
// Compose example both run it) must start the publisher and stop it with the
// contributor: a survivor keeps refreshing the pool's presence marker, telling
// the next relay a reading is coming that nothing will write.
func TestContributorEntrypointStartsAndStopsQuotaPublisher(t *testing.T) {
	entrypoint := readRepoFile(t, "bin", "contributor-agent.sh")
	if !strings.Contains(entrypoint, "hive-quota-publisher &") {
		t.Fatal("bin/contributor-agent.sh must start hive-quota-publisher alongside the relay (hivecommons/hive#10299)")
	}
	if !strings.Contains(entrypoint, `kill "$QUOTA_PUBLISHER_PID"`) {
		t.Fatal("bin/contributor-agent.sh must stop the quota publisher on shutdown — a surviving publisher keeps a stale presence marker fresh")
	}
	if !strings.Contains(entrypoint, "HIVE_CONTRIBUTOR_QUOTA_PUBLISH=0") {
		t.Fatal("bin/contributor-agent.sh must name the opt-out in its startup output")
	}
}

// Local mode (`just contribute-hive <cli> local`) runs no hive process either,
// so it needs the same publisher. It must degrade to a note rather than a
// failed launch when no binary and no Go toolchain exist.
func TestJustfileLocalModeStartsQuotaPublisher(t *testing.T) {
	justfile := readRepoFile(t, "Justfile")
	for _, want := range []string{
		"QUOTA_PUBLISHER_BIN",
		`"$QUOTA_PUBLISHER_BIN" &`,
		"./cmd/hive-quota-publisher",
		"HIVE_CONTRIBUTOR_QUOTA_PUBLISH=0",
	} {
		if !strings.Contains(justfile, want) {
			t.Fatalf("Justfile local mode missing %q — `just contribute-hive <cli> local` would publish no quota readings (hivecommons/hive#10299)", want)
		}
	}
	if !strings.Contains(justfile, "NOTE: no hive-quota-publisher binary and no Go toolchain") {
		t.Fatal("local mode must say so, not fail, when it cannot provide a publisher")
	}
}

// The behaviour and its opt-out have to be findable where contributors already
// read about the guard, or the only way to discover them is this test.
func TestContributorDocsDescribeStandalonePublisher(t *testing.T) {
	doc := readRepoFile(t, "src", "docs", "contributor-relay.md")
	for _, want := range []string{"hive-quota-publisher", "HIVE_CONTRIBUTOR_QUOTA_PUBLISH"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("src/docs/contributor-relay.md must document %q", want)
		}
	}
}
