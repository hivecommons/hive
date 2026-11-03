package agent

import "github.com/hivecommons/hive/crux/agent/panesignal"

// Blocking-prompt matching lives in agent/panesignal; the names below keep the
// package-internal and exported call sites unchanged.

const blockingPromptTailLines = panesignal.BlockingPromptTailLines

var blockingPrompts = panesignal.BlockingPrompts

func paneTail(pane string, n int) string { return panesignal.PaneTail(pane, n) }

func blockingPromptKey(backend, pane string) (key, label string, ok bool) {
	return panesignal.BlockingPromptKey(backend, pane)
}

func PaneShowsBlockingPrompt(backend, pane string) bool {
	return panesignal.ShowsBlockingPrompt(backend, pane)
}

func paneHasBlockingPrompt(pane string) bool { return panesignal.HasBlockingPrompt(pane) }

func backendHasBlockingPrompts(backend string) bool {
	return panesignal.BackendHasBlockingPrompts(backend)
}
