package proxy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
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
// A commit is the lane's when its name is the lane and its email is either
// identity the pod can be given (agentGitIdentity in crux/agent/manager_env.go):
// "<lane>@<HIVE_GIT_BOT_EMAIL_DOMAIN>", or on App-authored hives the App bot's
// noreply address "<botUserID>+<botLogin>@users.noreply.github.com".
//
// Whole commit objects are parsed directly. A deltified object is resolved
// when its base is in the same pack and is a commit; a delta against a base
// outside the pack (thin pack) cannot be resolved, so it is refused if it
// visibly begins a commit ("tree " literal). Tag, tree and blob objects are
// hashed while streamed, never held in memory.
//
// Limits: this is a best-effort guard against accidental sign-off mistakes,
// not the enforcement point. A crafted thin delta that starts with a copy op
// instead of a "tree " literal is not inspected, and neither are commits made
// through the REST git/commits API or GraphQL createCommitOnBranch. GitHub-side
// DCO remains the enforcement.

// maxPushSignoffBody bounds the push body buffered for inspection. A larger
// push is refused (fail closed) rather than forwarded uninspected.
const maxPushSignoffBody = 64 << 20

var pushSignedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by:\s*(.*?)\s*<([^<>]+)>\s*$`)

type pushCommit struct {
	authorName, authorEmail       string
	committerName, committerEmail string
	message                       string
	merge                         bool
}

const (
	maxPushDelta   = 1 << 20 // largest delta or resolved commit held in memory
	packTypeCommit = 1
)

type packObj struct {
	typ  int    // resolved object type, 0 when unknown
	data []byte // kept for commits only
}

var packTypeNames = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// skipPushCommands advances past the pkt-line command list, and past the
// push-options section when the first command advertises it. It returns the
// offset of the first byte after them.
func skipPushCommands(body []byte) (int, error) {
	pos := 0
	section := func(first bool) (pushOptions bool, err error) {
		for i := 0; ; i++ {
			if pos+4 > len(body) {
				return false, fmt.Errorf("truncated command list")
			}
			var n int
			if _, err := fmt.Sscanf(string(body[pos:pos+4]), "%04x", &n); err != nil {
				return false, fmt.Errorf("bad pkt-line length")
			}
			pos += 4
			if n == 0 {
				return pushOptions, nil
			}
			if n < 4 || pos+n-4 > len(body) {
				return false, fmt.Errorf("bad pkt-line")
			}
			if first && i == 0 {
				line := body[pos : pos+n-4]
				if nul := bytes.IndexByte(line, 0); nul >= 0 {
					for _, c := range strings.Fields(string(line[nul+1:])) {
						if c == "push-options" {
							pushOptions = true
						}
					}
				}
			}
			pos += n - 4
		}
	}
	opts, err := section(true)
	if err != nil {
		return 0, err
	}
	if opts {
		if _, err := section(false); err != nil {
			return 0, err
		}
	}
	return pos, nil
}

// parsePushCommits reads a git-receive-pack request body (pkt-line commands,
// flush, optional push-options, packfile) and returns the commits in the pack.
func parsePushCommits(body []byte) ([]pushCommit, error) {
	pos, err := skipPushCommands(body)
	if err != nil {
		return nil, err
	}
	rest := body[pos:]
	if len(rest) == 0 {
		return nil, nil // delete-only push carries no pack
	}
	if len(rest) < 12 || string(rest[:4]) != "PACK" {
		return nil, fmt.Errorf("no packfile")
	}
	count := int(binary.BigEndian.Uint32(rest[8:12]))
	packBytes := rest[12:]
	rd := bytes.NewReader(packBytes)
	br := bufio.NewReader(rd)
	offset := func() int64 { return int64(len(packBytes) - rd.Len() - br.Buffered()) }
	byOff := map[int64]*packObj{}
	bySha := map[string]*packObj{}
	var commits []pushCommit
	for i := 0; i < count; i++ {
		start := offset()
		typ, size, err := readPackObjectHeader(br)
		if err != nil {
			return nil, err
		}
		baseOff := int64(-1)
		var baseSha string
		switch typ {
		case 6: // OFS_DELTA: negative offset varint
			b, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				if b, err = br.ReadByte(); err != nil {
					return nil, err
				}
				off = ((off + 1) << 7) | int64(b&0x7f)
				if off > int64(len(packBytes)) {
					return nil, fmt.Errorf("bad delta offset")
				}
			}
			baseOff = start - off
		case 7: // REF_DELTA: 20-byte base id (sha1 repos)
			id := make([]byte, 20)
			if _, err := io.ReadFull(br, id); err != nil {
				return nil, err
			}
			baseSha = string(id)
		case 1, 2, 3, 4:
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("pack object %d: %w", i, err)
		}
		obj := &packObj{}
		var sha string
		switch typ {
		case 2, 3, 4:
			h := sha1.New()
			fmt.Fprintf(h, "%s %d\x00", packTypeNames[typ], size)
			if _, err := io.Copy(h, zr); err != nil {
				return nil, fmt.Errorf("pack object %d: %w", i, err)
			}
			obj.typ = typ
			sha = string(h.Sum(nil))
		case packTypeCommit:
			data, err := io.ReadAll(io.LimitReader(zr, size+1))
			if err != nil || int64(len(data)) != size {
				return nil, fmt.Errorf("pack commit %d unreadable", i)
			}
			obj.typ, obj.data = packTypeCommit, data
			sha = hashPackObject("commit", data)
			commits = append(commits, parseCommitObject(data))
		default: // 6, 7
			var base *packObj
			if typ == 6 {
				base = byOff[baseOff]
			} else {
				base = bySha[baseSha]
			}
			switch {
			case base != nil && base.typ != 0 && base.typ != packTypeCommit:
				// Delta of a tree, blob or tag stays one; skip it.
				if _, err := io.Copy(io.Discard, zr); err != nil {
					return nil, fmt.Errorf("pack object %d: %w", i, err)
				}
				obj.typ = base.typ
			case base != nil && base.typ == packTypeCommit:
				if size > maxPushDelta {
					return nil, fmt.Errorf("pack object %d: delta of a commit too large to check", i)
				}
				delta, err := io.ReadAll(io.LimitReader(zr, size+1))
				if err != nil || int64(len(delta)) != size {
					return nil, fmt.Errorf("pack object %d: delta unreadable", i)
				}
				data, err := applyPackDelta(base.data, delta)
				if err != nil {
					return nil, fmt.Errorf("pack object %d: %w", i, err)
				}
				obj.typ, obj.data = packTypeCommit, data
				sha = hashPackObject("commit", data)
				commits = append(commits, parseCommitObject(data))
			default:
				// Base outside the pack (thin push) or of unknown type: the
				// result cannot be typed. Refuse one that visibly starts a
				// commit; otherwise it is overwhelmingly a tree or blob.
				head := make([]byte, 64)
				n, err := io.ReadFull(zr, head)
				if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
					return nil, fmt.Errorf("pack object %d: %w", i, err)
				}
				if deltaLiteralStartsCommit(head[:n]) {
					return nil, fmt.Errorf("pack object %d: deltified commit against a base outside the pack", i)
				}
				if _, err := io.Copy(io.Discard, zr); err != nil {
					return nil, fmt.Errorf("pack object %d: %w", i, err)
				}
			}
		}
		_ = zr.Close()
		byOff[start] = obj
		if sha != "" {
			bySha[sha] = obj
		}
	}
	return commits, nil
}

func hashPackObject(kind string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return string(h.Sum(nil))
}

// deltaLiteralStartsCommit reports whether a delta's first instruction
// inserts literal bytes beginning "tree ", the first line of any commit.
func deltaLiteralStartsCommit(delta []byte) bool {
	p := 0
	for v := 0; v < 2; v++ { // source and target size varints
		for {
			if p >= len(delta) {
				return false
			}
			b := delta[p]
			p++
			if b&0x80 == 0 {
				break
			}
		}
	}
	if p >= len(delta) {
		return false
	}
	op := delta[p]
	p++
	if op == 0 || op&0x80 != 0 {
		return false
	}
	lit := delta[p:]
	if int(op) < len(lit) {
		lit = lit[:op]
	}
	return bytes.HasPrefix(lit, []byte("tree "))
}

func deltaVarint(d []byte, p *int) (int, error) {
	v, shift := 0, uint(0)
	for {
		if *p >= len(d) || shift > 28 {
			return 0, fmt.Errorf("bad delta header")
		}
		b := d[*p]
		*p++
		v |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return v, nil
		}
	}
}

// applyPackDelta applies a git delta to base, bounded by maxPushDelta.
func applyPackDelta(base, delta []byte) ([]byte, error) {
	p := 0
	src, err := deltaVarint(delta, &p)
	if err != nil {
		return nil, err
	}
	dst, err := deltaVarint(delta, &p)
	if err != nil {
		return nil, err
	}
	if src != len(base) || dst > maxPushDelta {
		return nil, fmt.Errorf("bad delta sizes")
	}
	out := make([]byte, 0, dst)
	for p < len(delta) {
		op := delta[p]
		p++
		switch {
		case op&0x80 != 0:
			var off, n int
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if p >= len(delta) {
						return nil, fmt.Errorf("truncated delta")
					}
					off |= int(delta[p]) << (8 * i)
					p++
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if p >= len(delta) {
						return nil, fmt.Errorf("truncated delta")
					}
					n |= int(delta[p]) << (8 * i)
					p++
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off < 0 || n < 0 || off+n > len(base) || len(out)+n > dst {
				return nil, fmt.Errorf("bad delta copy")
			}
			out = append(out, base[off:off+n]...)
		case op != 0:
			n := int(op)
			if p+n > len(delta) || len(out)+n > dst {
				return nil, fmt.Errorf("bad delta insert")
			}
			out = append(out, delta[p:p+n]...)
			p += n
		default:
			return nil, fmt.Errorf("bad delta opcode")
		}
	}
	if len(out) != dst {
		return nil, fmt.Errorf("delta size mismatch")
	}
	return out, nil
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

// pushLaneIdentity is a lane's commit name and every email the pod can be
// given for it.
type pushLaneIdentity struct {
	name   string
	emails []string
}

// matches reports whether name/email is one of the lane's identities.
func (l pushLaneIdentity) matches(name, email string) bool {
	if !strings.EqualFold(strings.TrimSpace(name), l.name) {
		return false
	}
	email = strings.TrimSpace(email)
	for _, want := range l.emails {
		if strings.EqualFold(email, want) {
			return true
		}
	}
	return false
}

// pushAppBotIdentity returns the App bot's GitHub user id and login, or a
// zero id when the hive has no App bot (or it is not resolvable yet).
type pushAppBotIdentity func() (userID int64, login string)

// lanePushIdentity resolves the identities agentName may commit as: always
// the HIVE_GIT_BOT_EMAIL_DOMAIN address, plus the App bot noreply address
// when appBot resolves one. ok is false when the lane has no git identity.
func lanePushIdentity(agentName string, appBot pushAppBotIdentity) (pushLaneIdentity, bool) {
	name, email, ok := gitidentity.AgentIdentity(agentName)
	if !ok {
		return pushLaneIdentity{}, false
	}
	lane := pushLaneIdentity{name: name, emails: []string{email}}
	if appBot != nil {
		if userID, login := appBot(); userID > 0 {
			if _, appEmail, ok := gitidentity.AppLinkedAgentIdentity(agentName, userID, login); ok && !strings.EqualFold(appEmail, email) {
				lane.emails = append(lane.emails, appEmail)
			}
		}
	}
	return lane, true
}

// pushSignoffViolation applies the broker's sign-off rules to commits for the
// lane. It returns an agent-facing reason, or "".
func pushSignoffViolation(lane pushLaneIdentity, commits []pushCommit) string {
	for _, c := range commits {
		if c.merge {
			continue // DCO App and Prow skip merge commits, as does the broker
		}
		authored := lane.matches(c.authorName, c.authorEmail)
		committed := lane.matches(c.committerName, c.committerEmail)
		signed := false
		for _, m := range pushSignedOffByRE.FindAllStringSubmatch(c.message, -1) {
			if lane.matches(m[1], m[2]) {
				signed = true
				break
			}
		}
		laneName := lane.name
		switch {
		case authored && committed && !signed:
			return fmt.Sprintf("push refused: a commit authored and committed by %s <%s> has no %s Signed-off-by trailer; amend it with git commit -s --amend before pushing", c.committerName, c.committerEmail, laneName)
		case signed && !(authored && committed):
			return fmt.Sprintf("push refused: a commit authored by %s <%s> and committed by %s <%s> carries %s's Signed-off-by; leave DCO remediation to the author", c.authorName, c.authorEmail, c.committerName, c.committerEmail, laneName)
		}
	}
	return ""
}

// pushSignoffRefusal inspects a git-receive-pack request from an in-pod lane.
// The body is buffered and restored so it forwards unchanged. It fails closed
// on a body it cannot read, but never refuses a non-push request or a lane
// with no derivable git identity. appBot (may be nil) supplies the App bot
// identity lanes commit as on App-authored hives.
func pushSignoffRefusal(req *http.Request, agentName string, appBot pushAppBotIdentity) (reason string, refused, closeConn bool) {
	if req == nil || req.Method != http.MethodPost || !isGitReceivePack(req.URL.Path) || req.Body == nil {
		return "", false, false
	}
	lane, ok := lanePushIdentity(agentName, appBot)
	if !ok {
		return "", false, false
	}
	if enc := req.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return "push refused: content-encoded push body cannot be checked for sign-off", true, false
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxPushSignoffBody+1))
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if err != nil || len(body) > maxPushSignoffBody {
		// Only a prefix was read; the rest of the upload is still on the
		// connection and must not be parsed as the next request.
		return "push refused: push body unreadable or too large to check for sign-off", true, true
	}
	commits, err := parsePushCommits(body)
	if err != nil {
		return "push refused: could not read the pushed commits to check sign-off (" + err.Error() + ")", true, false
	}
	if reason := pushSignoffViolation(lane, commits); reason != "" {
		return reason, true, false
	}
	return "", false, false
}
