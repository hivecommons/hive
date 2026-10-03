package dashboard

// FrontendAgentContinuous carries continuous-mode status fields embedded into
// FrontendAgent without shifting the route citations in server.go.
type FrontendAgentContinuous struct {
	ContinuousBackoffUntil string   `json:"continuousBackoffUntil,omitempty"`
	ContinuousBlocked      string   `json:"continuousBlocked,omitempty"`
	ContinuousModes        []string `json:"continuousModes,omitempty"`
	ContinuousKicks        int64    `json:"continuousKicks,omitempty"`
	ContinuousTokens       int64    `json:"continuousTokens,omitempty"`
}
