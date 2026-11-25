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

// SandboxInferenceRelay is the hive-hosted listener a sandbox Job's Claude CLI
// reaches the inference translator through (hivecommons/hive#19619). Enroll
// admits one kick of agentName and returns the base URL the Job sets as
// ANTHROPIC_BASE_URL, the per-kick key it sends as its API key (worth nothing
// to any gateway), the PEM trust bundle for the listener's certificate, and a
// release func that revokes the key.
type SandboxInferenceRelay interface {
	Enroll(agentName string) (baseURL, apiKey string, caBundlePEM []byte, release func(), err error)
}
