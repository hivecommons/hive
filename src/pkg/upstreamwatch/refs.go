package upstreamwatch

import "strings"

// prRefPrefix and releaseRefPrefix open the two dedupe-key shapes a recorded
// ref can take ("upstream#<pr>" / "release:<tag>").
const (
	prRefPrefix      = "upstream#"
	releaseRefPrefix = "release:"
)

// UpstreamRef renders a stored ref in its upstream-qualified, human-readable
// form: "owner/repo#123" for a PR and "owner/repo@<tag>" for a release. It is
// MarkerRef's counterpart for a ref read back out of the store, where the
// original Item is long gone. An empty upstream yields an empty string: a ref
// alone names nothing a reader can follow.
func UpstreamRef(upstream, ref string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(ref, releaseRefPrefix):
		tag := strings.TrimPrefix(ref, releaseRefPrefix)
		if tag == "" {
			return ""
		}
		return upstream + "@" + tag
	case strings.HasPrefix(ref, prRefPrefix):
		num := strings.TrimPrefix(ref, prRefPrefix)
		if num == "" {
			return ""
		}
		return upstream + "#" + num
	default:
		return ""
	}
}

// UpstreamURL is the GitHub URL for a stored ref: the upstream pull request
// for "upstream#<pr>", the release page for "release:<tag>". Empty when the
// upstream or the ref shape is unknown.
func UpstreamURL(upstream, ref string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(ref, releaseRefPrefix):
		tag := strings.TrimPrefix(ref, releaseRefPrefix)
		if tag == "" {
			return ""
		}
		return "https://github.com/" + upstream + "/releases/tag/" + tag
	case strings.HasPrefix(ref, prRefPrefix):
		num := strings.TrimPrefix(ref, prRefPrefix)
		if num == "" {
			return ""
		}
		return "https://github.com/" + upstream + "/pull/" + num
	default:
		return ""
	}
}

// DiffURL is the upstream patch a porting agent reads before it starts: the
// `.diff` sibling of the upstream pull request URL. A release has no diff, so
// the result is empty for a release ref — the caller must treat empty as "no
// patch to offer" rather than guessing one.
func DiffURL(upstream, ref string) string {
	if !strings.HasPrefix(ref, prRefPrefix) {
		return ""
	}
	url := UpstreamURL(upstream, ref)
	if url == "" {
		return ""
	}
	return url + ".diff"
}

// ParseMarkerRef extracts the upstream-qualified ref from the hidden marker a
// filed fork issue carries ("<!-- upstream-ref: owner/repo#123 -->"), so a
// consumer holding only the issue body — the kick builder, for instance — can
// recover which upstream change the issue is about. It returns false when the
// body carries no well-formed marker.
func ParseMarkerRef(body string) (string, bool) {
	start := strings.Index(body, markerPrefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(markerPrefix):]
	end := strings.Index(rest, markerSuffix)
	if end < 0 {
		return "", false
	}
	ref := strings.TrimSpace(rest[:end])
	if ref == "" {
		return "", false
	}
	return ref, true
}

// MarkerDiffURL is the upstream patch URL for an upstream-qualified marker ref
// ("owner/repo#123"). It is empty for a release marker ("owner/repo@<tag>"),
// which has no diff, and for anything that is not a well-formed marker ref.
func MarkerDiffURL(markerRef string) string {
	upstream, num, ok := strings.Cut(strings.TrimSpace(markerRef), "#")
	if !ok || num == "" {
		return ""
	}
	if _, _, ok := SplitRepo(upstream); !ok {
		return ""
	}
	if strings.ContainsAny(num, "/#@ ") {
		return ""
	}
	return "https://github.com/" + upstream + "/pull/" + num + ".diff"
}
