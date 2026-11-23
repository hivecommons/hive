package auditchain

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalizeRFC8785(t *testing.T) {
	cases := []struct{ in, want string }{
		// Members sorted, whitespace dropped.
		{`{"b": 1, "a": [true, null, "x"]}`, `{"a":[true,null,"x"],"b":1}`},
		// Go's json.Marshal HTML-escapes; JCS writes those characters literally.
		{`{"d":"a<b&c>"}`, `{"d":"a<b&c>"}`},
		// Control characters: short forms where they exist, else lowercase \u00xx.
		{`"\u0007\n\t\u001F"`, `"\u0007\n\t\u001f"`},
		// Non-ASCII stays literal.
		{`"é "`, "\"é \""},
		// ECMAScript number formatting.
		{`[1.0, 1e3, -0, 0.000001, 1e-7, 1e21, 123456789012]`, `[1,1000,0,0.000001,1e-7,1e+21,123456789012]`},
		// Sorting is by UTF-16 code units: U+10000 (surrogate D800) sorts before U+FFFD.
		{"{\"�\":1,\"\U00010000\":2}", "{\"\U00010000\":2,\"�\":1}"},
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
}

func TestRowHashIgnoresFieldOrderAndEscaping(t *testing.T) {
	a, err := RowHash([]byte(`{"ts":"t","detail":"a<b","seq":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := RowHash([]byte(`{"seq":1,"detail":"a<b","ts":"t","row_hash":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("same row hashed differently: %s vs %s", a, b)
	}
	c, _ := RowHash([]byte(`{"ts":"t","detail":"a<c","seq":1}`))
	if c == a {
		t.Fatal("different content produced the same hash")
	}
	if len(a) != 64 {
		t.Fatalf("hash %q is not hex SHA-256", a)
	}
}

func sealedLog(t *testing.T, rows ...string) [][]byte {
	t.Helper()
	var head Link
	var out [][]byte
	for _, r := range rows {
		line, next, err := Seal([]byte(r), head)
		if err != nil {
			t.Fatalf("Seal(%s): %v", r, err)
		}
		if next.Seq != head.Seq+1 {
			t.Fatalf("seq %d after %d", next.Seq, head.Seq)
		}
		head = next
		out = append(out, line)
	}
	return out
}

func join(lines [][]byte) []byte { return append(bytes.Join(lines, []byte("\n")), '\n') }

func TestVerifyIntactChain(t *testing.T) {
	lines := sealedLog(t, `{"action":"a"}`, `{"action":"b"}`, `{"action":"c"}`)
	legacy := []byte(`{"action":"old"}`)
	r := VerifyLines("audit.jsonl", join(append([][]byte{legacy}, lines...)))
	if !r.OK() {
		t.Fatalf("intact chain reported problems: %v", r.Problems)
	}
	if r.Rows != 4 || r.Legacy != 1 || r.Chained != 3 || r.FirstSeq != 1 || r.Head.Seq != 3 || r.Anchor != "" {
		t.Fatalf("unexpected report: %+v", r)
	}
	want, _ := RowHash(lines[2])
	if r.Head.Hash != want {
		t.Fatalf("head %s, want %s", r.Head.Hash, want)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	cases := map[string]struct {
		mutate func([][]byte) [][]byte
		want   string
	}{
		"edited row": {
			mutate: func(l [][]byte) [][]byte {
				l[1] = bytes.Replace(l[1], []byte(`"b"`), []byte(`"B"`), 1)
				return l
			},
			want: "row_hash mismatch",
		},
		"dropped row": {
			mutate: func(l [][]byte) [][]byte { return append(l[:1:1], l[2:]...) },
			want:   "prev_hash does not match",
		},
		"reordered rows": {
			mutate: func(l [][]byte) [][]byte { l[1], l[2] = l[2], l[1]; return l },
			want:   "prev_hash does not match",
		},
		"unchained row inserted": {
			mutate: func(l [][]byte) [][]byte {
				return append(l[:2:2], append([][]byte{[]byte(`{"action":"forged"}`)}, l[2:]...)...)
			},
			want: "unchained row inside the chain",
		},
		"chain restarted": {
			mutate: func(l [][]byte) [][]byte {
				fresh, _, _ := Seal([]byte(`{"action":"rebuilt"}`), Link{})
				return append(l, fresh)
			},
			want: "chain restarted",
		},
		"malformed row": {
			mutate: func(l [][]byte) [][]byte { l[3] = l[3][:10]; return l },
			want:   "malformed row",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			lines := sealedLog(t, `{"action":"a"}`, `{"action":"b"}`, `{"action":"c"}`, `{"action":"d"}`)
			r := VerifyLines("", join(c.mutate(lines)))
			if r.OK() {
				t.Fatal("tampering went undetected")
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

func TestVerifyEditedRowReportedOnce(t *testing.T) {
	lines := sealedLog(t, `{"a":1}`, `{"a":2}`, `{"a":3}`, `{"a":4}`)
	lines[1] = bytes.Replace(lines[1], []byte(`"a":2`), []byte(`"a":9`), 1)
	r := VerifyLines("", join(lines))
	if len(r.Problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", r.Problems)
	}
}

func TestVerifyAcceptsRotatedOutPrefix(t *testing.T) {
	lines := sealedLog(t, `{"a":1}`, `{"a":2}`, `{"a":3}`)
	r := VerifyLines("", join(lines[1:]))
	if !r.OK() {
		t.Fatalf("chain whose oldest rows were rotated out reported problems: %v", r.Problems)
	}
	if r.FirstSeq != 2 || r.Anchor == "" {
		t.Fatalf("want anchor at seq 2, got %+v", r)
	}
}
