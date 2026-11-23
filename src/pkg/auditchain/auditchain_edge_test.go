package auditchain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLinkHelpers(t *testing.T) {
	var zero Link
	if !zero.IsZero() {
		t.Fatal("zero Link is not IsZero")
	}
	if seq, prev := zero.Next(); seq != 1 || prev != "" {
		t.Fatalf("zero.Next() = %d, %q; want 1, \"\"", seq, prev)
	}
	l := Link{Seq: 4, Hash: "h"}
	if l.IsZero() {
		t.Fatal("non-zero Link reported IsZero")
	}
	if seq, prev := l.Next(); seq != 5 || prev != "h" {
		t.Fatalf("Next() = %d, %q; want 5, \"h\"", seq, prev)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, in := range []string{``, `{`, `null`, `[1]`, `{"a":1} {"b":2}`, `{"a":1e400}`} {
		if _, err := RowHash([]byte(in)); err == nil {
			t.Errorf("RowHash(%q) succeeded, want error", in)
		}
		if _, _, err := Seal([]byte(in), Link{}); err == nil {
			t.Errorf("Seal(%q) succeeded, want error", in)
		}
	}
	for _, in := range []string{``, `{`, `[1e400]`, `{"a":[1e400]}`, `{"a":{"b":1e400}}`} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("Canonicalize(%q) succeeded, want error", in)
		}
	}
}

func TestCanonicalEdgeValues(t *testing.T) {
	cases := []struct{ in, want string }{
		{`[false, true]`, `[false,true]`},
		{`{"ab":1,"a":2}`, `{"a":2,"ab":1}`},
		{`"q\"b\\s\b\f\r"`, `"q\"b\\s\b\f\r"`},
		{`[1e22, -1.5e-9, 0.5]`, `[1e+22,-1.5e-9,0.5]`},
	}
	for _, c := range cases {
		got, err := Canonicalize([]byte(c.in))
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("Canonicalize(%s) = %s, want %s", c.in, got, c.want)
		}
	}
	if _, err := appendCanonical(nil, 3); err == nil {
		t.Error("appendCanonical(int) succeeded, want unsupported-value error")
	}
	for _, n := range []json.Number{"NaN", "Inf", "x"} {
		if _, err := canonicalNumber(n); err == nil {
			t.Errorf("canonicalNumber(%q) succeeded, want error", n)
		}
	}
}

func TestSealReplacesChainFields(t *testing.T) {
	first, head, err := Seal([]byte(`{"a":1,"seq":99,"prev_hash":"stale","row_hash":"stale"}`), Link{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first), "stale") || head.Seq != 1 {
		t.Fatalf("genesis row kept caller's chain fields: %s (head %+v)", first, head)
	}
	second, next, err := Seal([]byte(`{"a":2,"prev_hash":"stale"}`), head)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), `"prev_hash":"`+head.Hash+`"`) || next.Seq != 2 {
		t.Fatalf("second row not linked to the first: %s", second)
	}
	if got, _ := RowHash(second); got != next.Hash {
		t.Fatalf("returned head %s does not match row hash %s", next.Hash, got)
	}
}

func TestProblemString(t *testing.T) {
	cases := []struct {
		p    Problem
		want string
	}{
		{Problem{Line: 3, Reason: "r"}, "3: r"},
		{Problem{File: "f", Line: 3, Reason: "r"}, "f:3: r"},
		{Problem{File: "f", Line: 3, Seq: 7, Reason: "r"}, "f:3 (seq 7): r"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

func TestVerifyChainFieldProblems(t *testing.T) {
	lines := sealedLog(t, `{"a":1}`, `{"a":2}`, `{"a":3}`)
	cases := map[string]struct {
		data string
		want string
	}{
		"missing seq": {
			data: `{"a":1,"row_hash":"x"}`,
			want: "no valid seq",
		},
		"unhashable row": {
			data: `{"a":1e400,"seq":1,"row_hash":"x"}`,
			want: "cannot hash row",
		},
		"genesis not at seq 1": {
			data: mustSeal(t, `{"a":1,"seq":5}`, 5),
			want: "chain starts without prev_hash at seq 5",
		},
		"seq gap": {
			data: string(lines[0]) + "\n" + relinkSeq(t, lines[1], lines[0], 3),
			want: "seq 3 follows seq 1",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := VerifyLines("f", []byte(c.data+"\n\n"))
			if r.OK() {
				t.Fatalf("no problem reported for %s", c.data)
			}
			var all []string
			for _, p := range r.Problems {
				all = append(all, p.String())
			}
			if !strings.Contains(strings.Join(all, "\n"), c.want) {
				t.Fatalf("problems %q do not mention %q", all, c.want)
			}
		})
	}
}

func TestVerifierReportIsACopy(t *testing.T) {
	var v Verifier
	v.Add("", 1, []byte(`not json`))
	r := v.Report()
	r.Problems[0].Reason = "changed"
	if v.Report().Problems[0].Reason == "changed" {
		t.Fatal("Report shares its Problems slice with the verifier")
	}
}

// mustSeal returns a validly hashed row with no prev_hash at the given seq.
func mustSeal(t *testing.T, row string, seq int64) string {
	t.Helper()
	out, _, err := Seal([]byte(row), Link{Seq: seq - 1})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// relinkSeq rewrites row to follow prev with the given seq, and rehashes it.
func relinkSeq(t *testing.T, row, prev []byte, seq int64) string {
	t.Helper()
	prevHash, err := RowHash(prev)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := Seal(row, Link{Seq: seq - 1, Hash: prevHash})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
