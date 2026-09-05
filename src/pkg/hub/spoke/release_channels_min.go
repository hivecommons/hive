package spoke

const (
	ReleaseChannelStable    = "stable"
	ReleaseChannelCandidate = "candidate"
	ReleaseChannelEdge      = "edge"
)

var releaseChannels = []string{ReleaseChannelStable, ReleaseChannelCandidate, ReleaseChannelEdge}

func isReleaseChannel(tag string) bool {
	for _, c := range releaseChannels {
		if c == tag {
			return true
		}
	}
	return false
}
