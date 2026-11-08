package github

// SharedState returns the value this Client keeps under name, building it
// with build on first use. Every later call with the same name returns that
// same value for the life of the Client, which is the life of the hive
// process.
//
// It exists for packages that build short-lived workers over the Client but
// need memory that outlives them. The automerge sweeps run the queued,
// trusted-author and contributor lanes every minute on a one-shot engine
// (automerge.SweepContributorLaneAutoMerges and friends), so a "once per PR
// head" mark kept on the engine was empty again on the next tick. Those
// marks now live here, owned and typed by the package that uses them.
//
// The value must be safe for concurrent use: the long-lived self-authored
// loop and the per-minute sweeps run on different goroutines. A nil Client
// returns a fresh value that nothing else shares.
func (c *Client) SharedState(name string, build func() any) any {
	if c == nil {
		return build()
	}
	c.sharedStateMu.Lock()
	defer c.sharedStateMu.Unlock()
	if v, ok := c.sharedState[name]; ok {
		return v
	}
	v := build()
	if c.sharedState == nil {
		c.sharedState = map[string]any{}
	}
	c.sharedState[name] = v
	return v
}
