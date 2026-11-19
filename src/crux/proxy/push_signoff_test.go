package proxy

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func testPackBody(t *testing.T, objs ...[2]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	cmd := "0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/x\x00 report-status"
	fmt.Fprintf(&buf, "%04x%s0000", len(cmd)+4, cmd)
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(len(objs)))
	for _, o := range objs {
		typ, data := o[0].(int), []byte(o[1].(string))
		size := len(data)
		b := byte(typ<<4) | byte(size&0x0f)
		size >>= 4
		for size > 0 {
			pack.WriteByte(b | 0x80)
			b = byte(size & 0x7f)
			size >>= 7
		}
		pack.WriteByte(b)
		zw := zlib.NewWriter(&pack)
		_, _ = zw.Write(data)
		_ = zw.Close()
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	buf.Write(pack.Bytes())
	return buf.Bytes()
}

func testCommit(author, committer, msg string, parents int) string {
	var sb strings.Builder
	sb.WriteString("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n")
	for i := 0; i < parents; i++ {
		sb.WriteString("parent 2222222222222222222222222222222222222222\n")
	}
	fmt.Fprintf(&sb, "author %s 1700000000 +0000\ncommitter %s 1700000000 +0000\n\n%s\n", author, committer, msg)
	return sb.String()
}

func pushReq(body []byte) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "https://github.com/o/r.git/git-receive-pack", bytes.NewReader(body))
	return r
}

func TestPushSignoffRefusal(t *testing.T) {
	lane := "scanner <scanner@hive.invalid>"
	person := "Andy <andy@example.com>"
	signed := "msg\n\nSigned-off-by: scanner <scanner@hive.invalid>"
	cases := []struct {
		name    string
		commit  string
		refused bool
	}{
		{"lane own signed", testCommit(lane, lane, signed, 1), false},
		{"lane own unsigned", testCommit(lane, lane, "msg", 1), true},
		{"forged signoff on person commit", testCommit(person, lane, signed, 1), true},
		{"forged signoff author person committer person", testCommit(person, person, signed, 1), true},
		{"person commit unsigned by lane", testCommit(person, person, "msg", 1), false},
		{"lane merge unsigned", testCommit(lane, lane, "merge", 2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := testPackBody(t, [2]any{1, tc.commit}, [2]any{3, "blob data"})
			req := pushReq(body)
			_, refused := pushSignoffRefusal(req, "scanner")
			if refused != tc.refused {
				t.Fatalf("refused = %v, want %v", refused, tc.refused)
			}
			got, _ := io.ReadAll(req.Body)
			if !bytes.Equal(got, body) {
				t.Fatalf("body not restored")
			}
		})
	}
}

func TestPushSignoffRefusalFailClosedAndSkips(t *testing.T) {
	if _, refused := pushSignoffRefusal(pushReq([]byte("0000garbage")), "scanner"); !refused {
		t.Fatal("unparseable push must be refused")
	}
	body := testPackBody(t, [2]any{1, testCommit("scanner <scanner@hive.invalid>", "scanner <scanner@hive.invalid>", "msg", 1)})
	if _, refused := pushSignoffRefusal(pushReq(body), ""); refused {
		t.Fatal("unnamed agent must not be inspected")
	}
	get, _ := http.NewRequest(http.MethodGet, "https://github.com/o/r.git/info/refs", nil)
	if _, refused := pushSignoffRefusal(get, "scanner"); refused {
		t.Fatal("non-push must pass")
	}
}
