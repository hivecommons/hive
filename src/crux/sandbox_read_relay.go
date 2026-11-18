package crux

// SandboxGitHubReadRelay is the hive-hosted relay a sandbox Job sends its
// GitHub reads through (hivecommons/hive#18865). Enroll admits one kick and
// returns the proxy URL for it (carrying a per-kick relay key, never a GitHub
// credential), the PEM trust bundle the Job needs to accept the relay's
// certificates, and a release func that revokes the key. It is an interface so
// the runtime does not depend on hive's proxy.
type SandboxGitHubReadRelay interface {
	Enroll(agentName string) (proxyURL string, caBundlePEM []byte, release func(), err error)
}
