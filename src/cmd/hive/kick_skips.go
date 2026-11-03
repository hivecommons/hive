package main

// firstScanDeferredSkips explains the due targets that
// Scheduler.FilterFirstScanDeferredAgents withheld, so the eval-complete log
// still accounts for every target that was due this cycle (#12846). Order
// follows the pre-filter due list.
func firstScanDeferredSkips(before, after []string) []string {
	if len(before) == len(after) {
		return nil
	}
	kept := make(map[string]struct{}, len(after))
	for _, target := range after {
		kept[target] = struct{}{}
	}
	var skipped []string
	for _, target := range before {
		if _, ok := kept[target]; ok {
			continue
		}
		skipped = append(skipped, target+" (deferred manual kick)")
	}
	return skipped
}
