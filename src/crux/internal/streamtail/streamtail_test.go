package streamtail

import (
	"strings"
	"testing"
)

func TestFresh(t *testing.T) {
	cases := []struct {
		prev, cur, want []string
	}{
		{nil, []string{"a", "b"}, []string{"a", "b"}},
		{[]string{"a", "b"}, []string{"a", "b"}, nil},
		{[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"c"}},
		{[]string{"a", "b", "c"}, []string{"b", "c", "d", "e"}, []string{"d", "e"}},
		{[]string{"a", "b"}, []string{"x", "y"}, []string{"x", "y"}},
		{[]string{"a", "b"}, []string{"b"}, nil},
		{[]string{"a", "b", "c"}, []string{"c"}, nil},
	}
	for _, c := range cases {
		if got := Fresh(c.prev, c.cur); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("Fresh(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}
