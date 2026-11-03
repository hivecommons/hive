package agent

// backendDefersStartupKick sends the bootstrap through readiness-gated kick
// delivery instead of embedding it in the launch command. Goose alone needs
// an embedded --text prompt to remain interactive.
func backendDefersStartupKick(backend string) bool {
	switch backend {
	case "claude", "copilot", "gemini", "pi", bobBackend, "agy", "omp", "cline":
		return true
	default:
		return false
	}
}
