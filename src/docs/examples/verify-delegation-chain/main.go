// Command verify-delegation-chain is a STANDALONE third-party verifier for a
// hive delegation chain.
//
// It is the executable form of the claim this feature makes: a tenant can
// verify which chain of authorizations produced an action inside a fleet
// SOMEONE ELSE OPERATES, without trusting that operator at verification time.
//
// WHAT IT DEMONSTRATES, AND WHAT IT DELIBERATELY DOES NOT USE:
//
//   - No hive credentials. No session cookie, no bearer token, no API key.
//   - No hive packages. This file imports only the Go standard library, so it
//     is a genuine independent implementation rather than a call into the same
//     code that minted the chain. If hive's verification logic were wrong, this
//     program would not share the bug.
//   - No request-time call to the hub. The key document is fetched once (or
//     read from a file) and can be cached indefinitely; verification then works
//     offline, during a hub outage, or after leaving the platform.
//
// That last property is the one that matters. If verification required asking
// the operator, the operator could stop answering — or answer differently — for
// a tenant they were in dispute with, which is precisely the situation
// cryptographic evidence exists for.
//
// USAGE
//
//	# Fetch the published keys and verify a chain:
//	go run ./main.go -keys https://<hub>/api/hub/delegation-keys -token "<chain-token>"
//
//	# Or verify fully offline against a saved key document:
//	curl -s https://<hub>/api/hub/delegation-keys > keys.json
//	go run ./main.go -keys ./keys.json -token "$(cat chain.txt)"
//
// Exits 0 and prints the chain when it verifies; exits 1 and prints why when it
// does not. See src/docs/delegation-chain.md for the full model.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// chainVersion is the token format this verifier understands. A chain carrying
// a different version is REJECTED rather than best-effort parsed: the version
// is inside the signature, so a mismatch means the token was minted under
// different rules than this program knows, and guessing at the difference is
// how a verifier ends up asserting something it did not check.
const chainVersion = "hive-delegation-v1"

// b64 is base64url WITHOUT padding, matching how hive encodes both halves of
// the token.
var b64 = base64.RawURLEncoding

// principal is one party in the chain.
type principal struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	HiveID         string `json:"hive_id,omitempty"`
	AppID          int64  `json:"app_id,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
	Via            string `json:"via,omitempty"`
}

func (p principal) String() string {
	s := p.Type + ":" + p.ID
	if p.HiveID != "" {
		s += "@" + p.HiveID
	}
	if p.Via != "" {
		s += "(" + p.Via + ")"
	}
	return s
}

// isHuman reports whether this principal is a natural person.
//
// Exactly ONE type means a human. This is why the chain carries a typed
// principal instead of a bare login: "kubestellar-hive[bot]" and "clubanderson"
// are both plausible-looking identifiers, and no string inspection distinguishes
// them reliably across forges. A consumer asking "did a person authorize this?"
// must read the type.
func (p principal) isHuman() bool { return p.Type == "user" }

// chain is the signed payload. Field tags match hive's wire format exactly.
type chain struct {
	Version    string      `json:"v"`
	Subject    principal   `json:"sub"`
	Actors     []principal `json:"act,omitempty"`
	Action     string      `json:"action,omitempty"`
	HiveID     string      `json:"hive_id,omitempty"`
	Generation int         `json:"g,omitempty"`
	IssuedAt   int64       `json:"iat"`
	Expiry     int64       `json:"exp"`
}

// publishedKey mirrors one entry of the published key document.
type publishedKey struct {
	Generation int    `json:"generation"`
	PublicKey  string `json:"public_key"`
	Algorithm  string `json:"algorithm"`
	Curve      string `json:"curve"`
	Current    bool   `json:"current"`
}

type keyDocument struct {
	Version      string         `json:"version"`
	Enabled      bool           `json:"enabled"`
	Keys         []publishedKey `json:"keys"`
	ChainVersion string         `json:"chain_version"`
	GeneratedAt  string         `json:"generated_at"`
}

func main() {
	keysRef := flag.String("keys", "", "URL or file path of the published key document (/api/hub/delegation-keys)")
	token := flag.String("token", "", "the delegation chain token to verify")
	flag.Parse()

	if *keysRef == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: verify-delegation-chain -keys <url|path> -token <chain-token>")
		os.Exit(2)
	}

	doc, err := loadKeyDocument(*keysRef)
	if err != nil {
		fail("could not load the published key document: %v", err)
	}
	if doc.ChainVersion != "" && doc.ChainVersion != chainVersion {
		fail("the hub publishes chain format %q but this verifier understands %q",
			doc.ChainVersion, chainVersion)
	}
	if len(doc.Keys) == 0 {
		fail("the published document contains no keys (chain minting enabled=%v)", doc.Enabled)
	}

	c, gen, err := verify(*token, doc.Keys, time.Now())
	if err != nil {
		fail("%v", err)
	}

	// Success: print the chain ROOT-FIRST, which is the order a person reasons
	// about authorization in — who allowed this, and how did it reach the
	// action.
	fmt.Println("CHAIN VERIFIED")
	fmt.Printf("  action:      %s\n", orNone(c.Action))
	fmt.Printf("  hive:        %s\n", orNone(c.HiveID))
	fmt.Printf("  generation:  %d\n", gen)
	fmt.Printf("  issued:      %s\n", time.Unix(c.IssuedAt, 0).UTC().Format(time.RFC3339))
	fmt.Printf("  expires:     %s\n", time.Unix(c.Expiry, 0).UTC().Format(time.RFC3339))

	root, ok := rootOf(c)
	if !ok {
		fail("the chain verified but carries no root; treat it as unusable evidence")
	}
	fmt.Printf("  root:        %s\n", root)
	// The headline fact, stated explicitly rather than left to the reader to
	// infer from an identifier that may well look like a username.
	if root.isHuman() {
		fmt.Printf("  authorized by a HUMAN: yes (%s)\n", root.ID)
	} else {
		fmt.Printf("  authorized by a HUMAN: no — this is machine authority (%s)\n", root.Type)
	}

	fmt.Println("  delegation path (root first):")
	for i := len(c.Actors) - 1; i >= 0; i-- {
		fmt.Printf("    %s%s\n", strings.Repeat("  ", len(c.Actors)-1-i), c.Actors[i])
	}
	fmt.Printf("    %s%s\n", strings.Repeat("  ", len(c.Actors)), c.Subject)
}

// verify checks the token against every published key and returns the chain.
//
// Trial verification across the whole set is what makes a rotation invisible to
// a third party: a chain minted just before the hub rotated still verifies
// under the previous generation's key, which remains published for the duration
// of its acceptance window.
func verify(token string, keys []publishedKey, now time.Time) (chain, int, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return chain{}, 0, fmt.Errorf("malformed token: expected <body>.<signature>")
	}
	body, sigB64 := parts[0], parts[1]

	sig, err := b64.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return chain{}, 0, fmt.Errorf("malformed signature")
	}

	for _, k := range keys {
		pub, derr := hex.DecodeString(strings.TrimSpace(k.PublicKey))
		// The length check is MANDATORY, not defensive tidiness:
		// ed25519.Verify PANICS on a public key of the wrong size, so an
		// unvalidated key from a fetched document would crash this program.
		if derr != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		// The signature covers the base64 BODY STRING, not the raw JSON. This
		// is what lets a verifier check the signature without having to
		// re-serialize JSON byte-identically to the minter.
		if !ed25519.Verify(ed25519.PublicKey(pub), []byte(body), sig) {
			continue
		}

		// Only NOW is the payload trusted enough to parse. Parsing before
		// authenticating would give this program a pre-auth attack surface.
		raw, berr := b64.DecodeString(body)
		if berr != nil {
			return chain{}, 0, fmt.Errorf("signature valid but payload is undecodable")
		}
		var c chain
		if jerr := json.Unmarshal(raw, &c); jerr != nil {
			return chain{}, 0, fmt.Errorf("signature valid but claims are unparseable: %v", jerr)
		}
		if c.Version != chainVersion {
			return chain{}, 0, fmt.Errorf("unexpected chain version %q (want %q)", c.Version, chainVersion)
		}
		// Clock window, with the same 30s skew tolerance hive applies — a
		// third party's clock is not the minter's.
		const skew = 30
		n := now.Unix()
		if c.IssuedAt > n+skew {
			return chain{}, 0, fmt.Errorf("chain is not yet valid")
		}
		if c.Expiry < n-skew {
			return chain{}, 0, fmt.Errorf("chain expired at %s",
				time.Unix(c.Expiry, 0).UTC().Format(time.RFC3339))
		}
		return c, k.Generation, nil
	}
	return chain{}, 0, fmt.Errorf("chain REJECTED: no published key verifies this signature " +
		"(tampered, forged, or minted by a different hub)")
}

// rootOf returns the party whose authority is not derived from anything else in
// the chain: the innermost actor, or the subject for a one-link chain.
func rootOf(c chain) (principal, bool) {
	if len(c.Actors) > 0 {
		return c.Actors[len(c.Actors)-1], true
	}
	if c.Subject.Type == "" {
		return principal{}, false
	}
	return c.Subject, true
}

// loadKeyDocument reads the published material from a URL or a local file.
// The file path is supported so verification can be demonstrated — and run —
// entirely offline.
func loadKeyDocument(ref string) (keyDocument, error) {
	var data []byte
	var err error

	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		client := &http.Client{Timeout: 15 * time.Second}
		// No credentials are attached. The endpoint is anonymous by design.
		resp, herr := client.Get(ref)
		if herr != nil {
			return keyDocument{}, herr
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return keyDocument{}, fmt.Errorf("hub returned HTTP %d", resp.StatusCode)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	} else {
		data, err = os.ReadFile(ref)
	}
	if err != nil {
		return keyDocument{}, err
	}

	var doc keyDocument
	if jerr := json.Unmarshal(data, &doc); jerr != nil {
		return keyDocument{}, fmt.Errorf("parsing key document: %w", jerr)
	}
	return doc, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "CHAIN NOT VERIFIED: "+format+"\n", args...)
	os.Exit(1)
}
