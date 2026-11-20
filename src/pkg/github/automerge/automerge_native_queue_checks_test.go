package automerge

import (
	"errors"
	"testing"
)

func TestNativeQueueChecksFailing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New(`effect outcome uncertain, reconciliation required: graphql: UNPROCESSABLE: Pull request has failing required statuses and Pull request Required status check "Audit GitHub Actions" is failing.`), true},
		{errors.New("graphql: UNPROCESSABLE: something else"), false},
		{errors.New("timeout"), false},
		{nil, false},
	} {
		if got := nativeQueueChecksFailing(tc.err); got != tc.want {
			t.Errorf("%v: got %v want %v", tc.err, got, tc.want)
		}
	}
}
