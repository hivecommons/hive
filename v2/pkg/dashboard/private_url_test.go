package dashboard

import (
	"context"
	"testing"
)

func allowPrivateURLHostsForTest(t *testing.T, hosts ...string) {
	t.Helper()
	allowed := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		allowed[host] = struct{}{}
	}
	origResolver := privateURLResolver
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		if _, ok := allowed[host]; !ok {
			t.Fatalf("resolved unexpected host %q", host)
		}
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = origResolver })
}
