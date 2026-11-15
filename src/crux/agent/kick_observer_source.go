package agent

import "strings"

// v6-only kick-source helpers (hivecommons/hive#7483). They live outside
// kick_observer.go so that file stays byte-identical to v5's and the v5 -> v6
// top-up carries it without a conflict.

// kickObserverSourcePrefix separates the archive reason from the kick source
// in a kick-log-archived detail, so the mention responder can tell which
// kicks it summoned (hivecommons/hive#7483).
const kickObserverSourcePrefix = " source="

// kickObserverArchiveDetail builds a kick-log-archived detail: the archive
// reason, then the kick source after kickObserverSourcePrefix when there is
// one.
func kickObserverArchiveDetail(reason, source string) string {
	reason = strings.TrimSpace(reason)
	source = strings.TrimSpace(source)
	if source == "" {
		return reason
	}
	if reason == "" {
		return kickObserverSourcePrefix + source
	}
	return reason + kickObserverSourcePrefix + source
}

// KickObserverDetailSource returns the kick source carried in an archive
// detail, or "" when it has none.
func KickObserverDetailSource(detail string) string {
	if _, source, ok := strings.Cut(detail, kickObserverSourcePrefix); ok {
		return strings.TrimSpace(source)
	}
	return ""
}

// KickObserverDetailReason returns the archive reason without the source.
func KickObserverDetailReason(detail string) string {
	reason, _, _ := strings.Cut(detail, kickObserverSourcePrefix)
	return strings.TrimSpace(reason)
}
