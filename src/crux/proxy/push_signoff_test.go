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
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	lane := "scanner <scanner@hive.kubestellar.io>"
	person := "Andy <andy@example.com>"
	signed := "msg\n\nSigned-off-by: scanner <scanner@hive.kubestellar.io>"
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
		// GitHub's own squash commit: a person's authorship, committed by
		// GitHub, carrying the lane's trailer from the squashed commits.
		{"github squash commit with lane signoff", testCommit(person, "GitHub <noreply@github.com>", signed, 1), false},
		// Only GitHub's exact committer address is skipped.
		{"lookalike github committer", testCommit(person, "GitHub <noreply@github.com.example>", signed, 1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := testPackBody(t, [2]any{1, tc.commit}, [2]any{3, "blob data"})
			req := pushReq(body)
			_, refused, _ := pushSignoffRefusal(req, "scanner", nil)
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
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	if _, refused, _ := pushSignoffRefusal(pushReq([]byte("0000garbage")), "scanner", nil); !refused {
		t.Fatal("unparseable push must be refused")
	}
	body := testPackBody(t, [2]any{1, testCommit("scanner <scanner@hive.kubestellar.io>", "scanner <scanner@hive.kubestellar.io>", "msg", 1)})
	if _, refused, _ := pushSignoffRefusal(pushReq(body), "", nil); refused {
		t.Fatal("unnamed agent must not be inspected")
	}
	get, _ := http.NewRequest(http.MethodGet, "https://github.com/o/r.git/info/refs", nil)
	if _, refused, _ := pushSignoffRefusal(get, "scanner", nil); refused {
		t.Fatal("non-push must pass")
	}
}

func packObjBytes(typ int, extra []byte, payload []byte) []byte {
	var out bytes.Buffer
	size := len(payload)
	b := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	out.WriteByte(b)
	out.Write(extra)
	zw := zlib.NewWriter(&out)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	return out.Bytes()
}

func deltaVarintBytes(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

// insertOnlyDelta builds a delta that rewrites base into target with literals.
func insertOnlyDelta(baseLen int, target string) []byte {
	d := append(deltaVarintBytes(baseLen), deltaVarintBytes(len(target))...)
	for t := []byte(target); len(t) > 0; {
		n := min(len(t), 127)
		d = append(d, byte(n))
		d = append(d, t[:n]...)
		t = t[n:]
	}
	return d
}

func wrapPack(objs ...[]byte) []byte {
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(len(objs)))
	for _, o := range objs {
		pack.Write(o)
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes()
}

func pktCmd(caps string) []byte {
	cmd := "0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/x\x00" + caps
	return []byte(fmt.Sprintf("%04x%s0000", len(cmd)+4, cmd))
}

func TestPushSignoffRefusalDeltifiedCommit(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	lane := "scanner <scanner@hive.kubestellar.io>"
	signed := "msg\n\nSigned-off-by: scanner <scanner@hive.kubestellar.io>"
	base := testCommit(lane, lane, signed, 1)
	bad := testCommit(lane, lane, "unsigned", 1)
	good := testCommit(lane, lane, signed+"\n\nmore", 1)
	for _, tc := range []struct {
		name    string
		target  string
		refused bool
	}{{"unsigned delta commit", bad, true}, {"signed delta commit", good, false}} {
		t.Run(tc.name, func(t *testing.T) {
			baseObj := packObjBytes(1, nil, []byte(base))
			// OFS offset encoding of len(baseObj) (git big-endian varint).
			n := len(baseObj)
			off := []byte{byte(n & 0x7f)}
			for n >>= 7; n > 0; n >>= 7 {
				n--
				off = append([]byte{byte(n&0x7f) | 0x80}, off...)
			}
			deltaObj := packObjBytes(6, off, insertOnlyDelta(len(base), tc.target))
			body := append(pktCmd(" report-status"), wrapPack(baseObj, deltaObj)...)
			if _, refused, _ := pushSignoffRefusal(pushReq(body), "scanner", nil); refused != tc.refused {
				t.Fatalf("refused = %v, want %v", refused, tc.refused)
			}
		})
	}
}

func TestPushSignoffRefusalThinDeltaStartingCommit(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	lane := "scanner <scanner@hive.kubestellar.io>"
	target := testCommit(lane, lane, "unsigned", 1)
	deltaObj := packObjBytes(7, make([]byte, 20), insertOnlyDelta(10, target))
	body := append(pktCmd(" report-status"), wrapPack(deltaObj)...)
	if _, refused, _ := pushSignoffRefusal(pushReq(body), "scanner", nil); !refused {
		t.Fatal("thin delta that inserts a commit header must be refused")
	}
	blob := packObjBytes(7, make([]byte, 20), insertOnlyDelta(10, "plain file text"))
	body = append(pktCmd(" report-status"), wrapPack(blob)...)
	if _, refused, _ := pushSignoffRefusal(pushReq(body), "scanner", nil); refused {
		t.Fatal("thin blob delta must pass")
	}
}

func TestPushSignoffRefusalPushOptions(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	lane := "scanner <scanner@hive.kubestellar.io>"
	pack := packObjBytes(1, nil, []byte(testCommit(lane, lane, "msg\n\nSigned-off-by: scanner <scanner@hive.kubestellar.io>", 1)))
	opt := "ci.skip"
	body := append(pktCmd(" report-status push-options"), []byte(fmt.Sprintf("%04x%s0000", len(opt)+4, opt))...)
	body = append(body, wrapPack(pack)...)
	if reason, refused, _ := pushSignoffRefusal(pushReq(body), "scanner", nil); refused {
		t.Fatalf("push with push-options must be inspected, not refused: %s", reason)
	}
}

func TestPushSignoffRefusalOversizeClosesConnection(t *testing.T) {
	body := append(pktCmd(" report-status"), make([]byte, maxPushSignoffBody+1)...)
	_, refused, closeConn := pushSignoffRefusal(pushReq(body), "scanner", nil)
	if !refused || !closeConn {
		t.Fatalf("oversize push: refused=%v closeConn=%v, want both true", refused, closeConn)
	}
}

// Lanes on App-authored hives commit as the App bot's noreply address
// (agentGitIdentity), not <lane>@HIVE_GIT_BOT_EMAIL_DOMAIN; the check must
// treat both as the lane's own identity.
func TestPushSignoffRefusalAcceptsBothLaneIdentities(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	appBot := func() (int64, string) { return 280983584, "hivecommons-hive[bot]" }
	domainLane := "scanner <scanner@hive.kubestellar.io>"
	appLane := "scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>"
	person := "Andy <andy@example.com>"
	signedBy := func(ident string) string { return "msg\n\nSigned-off-by: " + ident }
	cases := []struct {
		name    string
		appBot  pushAppBotIdentity
		commit  string
		refused bool
	}{
		{"domain identity signed", appBot, testCommit(domainLane, domainLane, signedBy(domainLane), 1), false},
		{"domain identity unsigned", appBot, testCommit(domainLane, domainLane, "msg", 1), true},
		// Shaped like db007d0: author, committer and sign-off are the App noreply.
		{"app identity signed", appBot, testCommit(appLane, appLane, signedBy(appLane), 1), false},
		{"app identity unsigned", appBot, testCommit(appLane, appLane, "msg", 1), true},
		{"app identity forged on person commit", appBot, testCommit(person, appLane, signedBy(appLane), 1), true},
		{"app identity unsigned without app bot", nil, testCommit(appLane, appLane, "msg", 1), false},
		{"app identity unsigned with unresolved app bot", func() (int64, string) { return 0, "" }, testCommit(appLane, appLane, "msg", 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := testPackBody(t, [2]any{1, tc.commit})
			reason, refused, _ := pushSignoffRefusal(pushReq(body), "scanner", tc.appBot)
			if refused != tc.refused {
				t.Fatalf("refused = %v (%s), want %v", refused, reason, tc.refused)
			}
		})
	}
}

func TestGitHubProxyAppBotIdentityFunc(t *testing.T) {
	p := &GitHubProxy{}
	if p.pushAppBotIdentity() != nil {
		t.Fatal("no App bot identity is installed by default")
	}
	p.SetAppBotIdentityFunc(func() (int64, string) { return 7, "bot[bot]" })
	if id, login := p.pushAppBotIdentity()(); id != 7 || login != "bot[bot]" {
		t.Fatalf("pushAppBotIdentity = %d, %q", id, login)
	}
	p.SetAppBotIdentityFunc(nil)
	if p.pushAppBotIdentity() != nil {
		t.Fatal("nil must clear the App bot identity")
	}
}

// The sign-off guard is wired for GitHub hosts only (#19297): Forgejo lane
// identities are not modelled, so a Forgejo push must not reach it.
func TestPushSignoffGuardGitHubHostsOnly(t *testing.T) {
	if !IsGitHubHost("github.com") {
		t.Fatal("github.com must be a GitHub host so in-pod pushes are checked")
	}
	if IsGitHubHost("forgejo.example.com") {
		t.Fatal("a Forgejo host must not be treated as a GitHub host")
	}
}
