package hub

import (
	"fmt"
	"regexp"
	"strings"
)

// Image-tag validation for every path that writes a container image onto a
// running Deployment (hub self-upgrade and spoke upgrade/branch-switch).
//
// Why this exists: on 2026-07-27 the hub's own Deployment was patched to
// `ghcr.io/hivecommons/hive-hub:target1`. `target1` is not a tag that has ever
// been published — it is a TEST FIXTURE SHA (see setV2Latest(t, "target1") in
// saas_edge_coverage_test.go) that reached a real cluster because the hub's
// unit tests shell out to a real `kubectl` while KUBERNETES_SERVICE_HOST is
// still set in the pod environment. The new ReplicaSet went
// ImagePullBackOff, the OLD ReplicaSet kept serving, and the hub therefore
// looked healthy for ~20 minutes while silently running stale code. Nothing
// alerted; it was found only because an expected UI fix was missing.
//
// The lesson generalizes past that one fixture: writing an unresolvable tag
// takes a component down SILENTLY, because Kubernetes keeps the old ReplicaSet
// up. Refusing to patch is strictly safer — the component keeps running its
// last good image and the refusal is loud and actionable. So every writer now
// validates the tag first and refuses anything that is not a recognizable
// build artifact.

const (
	// minImageTagSHALen / maxImageTagSHALen bound a git SHA tag. The hub
	// normalizes branch SHAs to StandardSHALen (7) via shortSHA, but a spoke or
	// an admin may legitimately supply a longer prefix or the full 40-character
	// SHA, so accept the whole abbreviation range git itself allows.
	minImageTagSHALen = StandardSHALen
	maxImageTagSHALen = 40

	// mutableChannelTagSuffix is the suffix CI (.github/workflows/docker.yml)
	// appends when it republishes a floating per-branch tag in place, e.g.
	// v2-latest / v3-latest / mk-latest / dd-latest. The branch part is
	// branchToTag(branch), so it may contain '-' from a slashed branch name
	// (feat/x -> feat-x-latest) — which is why the channel form is matched by
	// pattern rather than against a fixed allowlist of branch names.
	mutableChannelTagSuffix = mutableTagSuffix

	// maxImageTagLen is the OCI limit on a tag: 128 characters total. A value
	// longer than this could never resolve on any registry.
	maxImageTagLen = 128
)

// imageTagSHAPattern matches an immutable git-SHA tag: lowercase hex only, of a
// length git accepts as an abbreviation. Anchored, so an identifier that merely
// CONTAINS hex characters (the `target1` class of bug — a bare identifier, or a
// leaked `$target`/`${target}1` template variable) cannot slip through.
var imageTagSHAPattern = regexp.MustCompile(
	fmt.Sprintf(`^[0-9a-f]{%d,%d}$`, minImageTagSHALen, maxImageTagSHALen),
)

// imageTagChannelPattern matches a mutable channel tag: a branchToTag-sanitized
// branch name followed by "-latest". The branch part allows the characters a
// git branch can carry once '/' has been replaced by '-' (alphanumerics, '-',
// '_', '.') and must start alphanumeric so a stray leading dash cannot pass.
var imageTagChannelPattern = regexp.MustCompile(
	`^[0-9A-Za-z][0-9A-Za-z._-]*` + regexp.QuoteMeta(mutableChannelTagSuffix) + `$`,
)

// validateImageTag reports whether tag is a shape we are willing to write onto
// a live Deployment: either an immutable git-SHA tag or a mutable
// "<branch>-latest" channel tag. Anything else — empty, an identifier like
// `target1`, an unsubstituted `$target`, or a value over the OCI length limit —
// is refused with an error naming the exact offending value.
//
// This is deliberately a shape check, not an existence check. Existence is
// verified separately and over the network (hubImageExists); shape is free,
// deterministic, and catches the class of bug that produced `target1` — a value
// that was never a tag at all.
func validateImageTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("refusing to patch deployment image: tag is empty")
	}
	if strings.TrimSpace(tag) != tag {
		return fmt.Errorf("refusing to patch deployment image: tag %q has leading/trailing whitespace", tag)
	}
	if len(tag) > maxImageTagLen {
		return fmt.Errorf("refusing to patch deployment image: tag %q is %d chars, over the %d-char OCI limit",
			tag, len(tag), maxImageTagLen)
	}
	if imageTagSHAPattern.MatchString(tag) {
		return nil
	}
	if imageTagChannelPattern.MatchString(tag) {
		return nil
	}
	// A release channel IS the tag verbatim ("stable"/"candidate"/"edge") —
	// a closed set validated by membership, not shape. Without this, the
	// switch handler builds the correct channel tag via upgradeTargetTag and
	// then refuses its own work ("branch does not map to a valid image tag").
	if isReleaseChannel(tag) {
		return nil
	}
	return fmt.Errorf(
		"refusing to patch deployment image: tag %q is neither a %d-%d char hex git SHA nor a %q channel tag "+
			"(an unresolvable tag silently strands the component on its old ReplicaSet)",
		tag, minImageTagSHALen, maxImageTagSHALen, mutableChannelTagSuffix)
}

// imageRefTag returns the tag portion of a full image reference
// (ghcr.io/org/repo:tag). It mirrors imageTagIsMutable's parsing: only the part
// after the LAST colon is a tag, and only when no '/' follows it — otherwise
// that colon introduces a registry port (host:5000/repo). A digest pin
// (repo@sha256:...) has no tag to validate.
func imageRefTag(image string) (string, bool) {
	if strings.Contains(image, "@") {
		return "", false // digest pin — immutable by construction
	}
	idx := strings.LastIndex(image, ":")
	if idx < 0 || strings.Contains(image[idx+1:], "/") {
		return "", false
	}
	return image[idx+1:], true
}

// validateImageRef validates the tag embedded in a full image reference. A
// reference with no parseable tag is refused too: an untagged image resolves to
// :latest, which is never what an upgrade path means to write.
func validateImageRef(image string) error {
	if image == "" {
		return fmt.Errorf("refusing to patch deployment image: image reference is empty")
	}
	tag, ok := imageRefTag(image)
	if !ok {
		if strings.Contains(image, "@") {
			return nil // digest pin is a valid, fully-resolved reference
		}
		return fmt.Errorf("refusing to patch deployment image: image %q has no tag", image)
	}
	if err := validateImageTag(tag); err != nil {
		return fmt.Errorf("%w (image %q)", err, image)
	}
	return nil
}
