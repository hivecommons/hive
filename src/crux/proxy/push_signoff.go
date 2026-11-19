package proxy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/crux/gitidentity"
)

// Push sign-off enforcement at the egress proxy (#19297).
//
// The push broker's guard (pushbroker.rejectForgedLaneSignoffs) only covers
// sandboxed kicks. In-pod agents push through the git credential helper and
// this proxy, which gated git-receive-pack on mode and repo but never looked
// at the pushed commits. This file applies the same two rules to the pack in
// a receive-pack body:
//
//  1. a commit authored AND committed by the lane must carry the lane's
//     Signed-off-by trailer;
//  2. a commit carrying the lane's Signed-off-by that the lane did not both
//     author and commit is a forged sign-off (DCO remediation belongs to the
//     author).
//
// Only commit objects stored whole (not deltified) are inspected: git never
// deltifies a freshly created commit against another object in practice, and
// a deltified commit cannot be read without the base object. Tag, tree and
// blob objects are skipped without being inflated into memory.

// maxPushSignoffBody bounds the push body buffered for inspection. A larger
// push is refused (fail closed) rather than forwarded uninspected.
const maxPushSignoffBody = 64 << 20

var pushSignedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by:\s*(.*?)\s*<([^<>]+)>\s*$`)

type pushCommit struct {
	sha                           string
	authorName, authorEmail       string
	committerName, committerEmail string
	message                       string
	merge                         bool
}

// parsePushCommits reads a git-receive-pack request body (pkt-line commands,
// flush, packfile) and returns the whole-object commits in the pack.
func parsePushCommits(body []byte) ([]pushCommit, error) {
	// Skip the command section.
	pos := 0
	for {
		if pos+4 > len(body) {
			return nil, fmt.Errorf("truncated command list")
		}
		var n int
		if _, err := fmt.Sscanf(string(body[pos:pos+4]), "%04x", &n); err != nil {
			return nil, fmt.Errorf("bad pkt-line length")
		}
		pos += 4
		if n == 0 {
			break
		}
		if n < 4 || pos+n-4 > len(body) {
			return nil, fmt.Errorf("bad pkt-line")
		}
		pos += n - 4
	}
	rest := body[pos:]
	if len(rest) == 0 {
		return nil, nil // delete-only push carries no pack
	}
	if len(rest) < 12 || string(rest[:4]) != "PACK" {
		return nil, fmt.Errorf("no packfile")
	}
	count := int(binary.BigEndian.Uint32(rest[8:12]))
	br := bufio.NewReader(bytes.NewReader(rest[12:]))
	var commits []pushCommit
	for i := 0; i < count; i++ {
		typ, size, err := readPackObjectHeader(br)
		if err != nil {
			return nil, err
		}
		switch typ {
		case 6: // OFS_DELTA: negative offset varint
			for {
				b, err := br.ReadByte()
				if err != nil {
					return nil, err
				}
				if b&0x80 == 0 {
					break
				}
			}
		case 7: // REF_DELTA: 20-byte base id (sha1 repos)
			if _, err := br.Discard(20); err != nil {
				return nil, err
			}
		case 1, 2, 3, 4:
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("pack object %d: %w", i, err)
		}
		if typ != 1 {
			if _, err := io.Copy(io.Discard, zr); err != nil {
				return nil, fmt.Errorf("pack object %d: %w", i, err)
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(zr, size+1))
		if err != nil || int64(len(data)) != size {
			return nil, fmt.Errorf("pack commit %d unreadable", i)
		}
		commits = append(commits, parseCommitObject(data))
	}
	return commits, nil
}

func readPackObjectHeader(br *bufio.Reader) (typ int, size int64, err error) {
	b, err := br.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	typ = int(b>>4) & 7
	size = int64(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if b, err = br.ReadByte(); err != nil {
			return 0, 0, err
		}
		if shift > 56 {
			return 0, 0, fmt.Errorf("pack object size overflow")
		}
		size |= int64(b&0x7f) << shift
		shift += 7
	}
	return typ, size, nil
}

func parseCommitObject(data []byte) pushCommit {
	var c pushCommit
	head, msg, _ := strings.Cut(string(data), "\n\n")
	c.message = msg
	parents := 0
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "parent "):
			parents++
		case strings.HasPrefix(line, "author "):
			c.authorName, c.authorEmail = splitPushIdent(line[len("author "):])
		case strings.HasPrefix(line, "committer "):
			c.committerName, c.committerEmail = splitPushIdent(line[len("committer "):])
		}
	}
	c.merge = parents > 1
	return c
}

func splitPushIdent(s string) (name, email string) {
	lt := strings.LastIndex(s, "<")
	gt := strings.LastIndex(s, ">")
	if lt < 0 || gt < lt {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(s[:lt]), strings.TrimSpace(s[lt+1 : gt])
}

func samePushIdent(name, email, wantName, wantEmail string) bool {
	return strings.EqualFold(strings.TrimSpace(name), wantName) && strings.EqualFold(strings.TrimSpace(email), wantEmail)
}

// pushSignoffViolation applies the broker's sign-off rules to commits for the
// lane (laneName, laneEmail). It returns an agent-facing reason, or "".
func pushSignoffViolation(laneName, laneEmail string, commits []pushCommit) string {
	for _, c := range commits {
		if c.merge {
			continue // DCO App and Prow skip merge commits, as does the broker
		}
		authored := samePushIdent(c.authorName, c.authorEmail, laneName, laneEmail)
		committed := samePushIdent(c.committerName, c.committerEmail, laneName, laneEmail)
		signed := false
		for _, m := range pushSignedOffByRE.FindAllStringSubmatch(c.message, -1) {
			if samePushIdent(m[1], m[2], laneName, laneEmail) {
				signed = true
				break
			}
		}
		switch {
		case authored && committed && !signed:
			return fmt.Sprintf("push refused: a commit authored and committed by %s <%s> has no %s Signed-off-by trailer; amend it with git commit -s --amend before pushing", laneName, laneEmail, laneName)
		case signed && !(authored && committed):
			return fmt.Sprintf("push refused: a commit authored by %s <%s> and committed by %s <%s> carries %s's Signed-off-by; leave DCO remediation to the author", c.authorName, c.authorEmail, c.committerName, c.committerEmail, laneName)
		}
	}
	return ""
}

// pushSignoffRefusal inspects a git-receive-pack request from an in-pod lane.
// The body is buffered and restored so it forwards unchanged. It fails closed
// on a body it cannot read, but never refuses a non-push request or a lane
// with no derivable git identity.
func pushSignoffRefusal(req *http.Request, agentName string) (string, bool) {
	if req == nil || req.Method != http.MethodPost || !isGitReceivePack(req.URL.Path) || req.Body == nil {
		return "", false
	}
	laneName, laneEmail, ok := gitidentity.AgentIdentity(agentName)
	if !ok {
		return "", false
	}
	if enc := req.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return "push refused: content-encoded push body cannot be checked for sign-off", true
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxPushSignoffBody+1))
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if err != nil || len(body) > maxPushSignoffBody {
		return "push refused: push body unreadable or too large to check for sign-off", true
	}
	commits, err := parsePushCommits(body)
	if err != nil {
		return "push refused: could not read the pushed commits to check sign-off (" + err.Error() + ")", true
	}
	if reason := pushSignoffViolation(laneName, laneEmail, commits); reason != "" {
		return reason, true
	}
	return "", false
}
