package hub

import (
	"fmt"
	"strings"
)

// Canonical identity for a HUMAN hub/spoke user.
//
// Historically a user WAS their GitHub login: that one bare string was the
// session-cookie subject, the on-disk user key, the map key for hive grants, and
// the admin comparison value. To support additional login providers (Google,
// IBMid, Red Hat) without collisions or cross-provider impersonation, an identity
// is now a canonical `provider:subject` pair.
//
// This is strictly about WHO A HUMAN IS for login + access control. It has
// nothing to do with the GitHub App installation token (kubestellar-hive[bot])
// that performs the hive's actual GitHub work, nor with agent/backend logins.
//
// Two representations exist:
//
//	wire form      "github:clubanderson", "google:1078...":
//	               used in cookies, X-Hive-User, SaaSUser.Hives keys, Owner
//	               fields, authorized_users. The signed-cookie machinery signs an
//	               opaque string, so it carries a canonical id with no change.
//	filename form  "github.clubanderson", "google.1078...":
//	               path-safe on-disk key that stays inside safeNamePattern
//	               (^[a-zA-Z0-9._-]+$), so isValidName and the load/save traversal
//	               guards keep protecting the storage layer unchanged.

// identityProviders is the fixed, closed set of login providers. A provider name
// contains no '.', which is what lets the filename form split provider from
// subject on the FIRST dot. Extend this set (not the parsing) to add a provider.
var identityProviders = map[string]bool{
	"github": true,
	"google": true,
	"ibmid":  true,
	"redhat": true,
	// Generic/BYO OIDC provider (operator-configured: Okta, Auth0, Entra,
	// Keycloak, …). Its subjects are namespaced as custom:<sub>, distinct from
	// every branded provider, so an operator's IdP can never collide identities
	// with github:/google:/etc.
	"custom": true,
	// Microsoft Entra ID (Azure AD), single- or multi-tenant. Multi-tenant subjects
	// are namespaced per Entra tenant inside the OIDC layer before this sees them.
	"microsoft": true,
}

// legacyProvider is the provider a bare, un-namespaced key is assumed to belong
// to. Every user before multi-provider login was a GitHub login, so a key with
// no "provider:" prefix means GitHub. This is the backward-compat shim: existing
// user files, cookies, and grants keep resolving with no data migration.
const legacyProvider = "github"

// canonicalSeparator joins provider and subject in the WIRE form.
const canonicalSeparator = ":"

// maxCanonicalLen bounds the wire form, matching isValidName's len<=100 budget so
// a canonical id round-trips through the existing name validation.
const maxCanonicalLen = 100

// makeCanonical builds the wire form "provider:subject" from a provider name and
// the provider's stable subject (the OIDC `sub`, or the GitHub login). It
// validates the provider is known, the subject is non-empty and free of the
// separator and path-hostile characters, and the total length fits. The subject
// charset is deliberately the same safe set used on disk so the value survives
// both the cookie/header path and the filename encoding.
func makeCanonical(provider, subject string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	subject = strings.TrimSpace(subject)
	if !identityProviders[provider] {
		return "", fmt.Errorf("unknown identity provider %q", provider)
	}
	if subject == "" {
		return "", fmt.Errorf("empty subject for provider %q", provider)
	}
	if strings.ContainsAny(subject, ":/\\") || strings.Contains(subject, "..") {
		return "", fmt.Errorf("subject %q contains a reserved character", subject)
	}
	id := provider + canonicalSeparator + subject
	if len(id) > maxCanonicalLen {
		return "", fmt.Errorf("canonical id exceeds %d bytes", maxCanonicalLen)
	}
	return id, nil
}

// parseCanonical splits a wire-form canonical id into provider and subject. A
// bare, un-namespaced string is treated as a legacy GitHub login (the shim), so
// callers may pass either form. Returns ok=false only for a malformed value
// (empty, unknown provider, empty subject).
func parseCanonical(id string) (provider, subject string, ok bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", false
	}
	before, after, found := strings.Cut(id, canonicalSeparator)
	if !found {
		// Bare login → legacy GitHub identity.
		return legacyProvider, id, true
	}
	provider = strings.ToLower(before)
	subject = after
	if !identityProviders[provider] || subject == "" {
		return "", "", false
	}
	return provider, subject, true
}

// canonicalizeLegacy normalizes any identity string to its canonical wire form:
// an already-namespaced key (contains the separator) is returned as-is; a bare
// key is promoted to "github:<key>". This is applied at BOTH sides of every
// ownership/admin comparison so a legacy stored value ("clubanderson") matches an
// authenticated canonical id ("github:clubanderson") and vice-versa — the core of
// the zero-downtime migration shim.
func canonicalizeLegacy(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.Contains(id, canonicalSeparator) {
		return id
	}
	return legacyProvider + canonicalSeparator + id
}

// canonicalEqual reports whether two identity strings denote the same user,
// tolerant of legacy (bare) vs canonical form. Use this in place of raw string /
// EqualFold(Owner,…) comparisons.
//
// AUDIT F17. This used to be a flat strings.EqualFold over the whole canonical
// id, justified by "a case-fold on an opaque sub is harmless because two subs
// that differ only by case are not issued". That holds for GitHub, whose logins
// are genuinely case-insensitive and cannot collide. It is NOT guaranteed for
// any OIDC provider: `sub` is defined as a case-SENSITIVE opaque string, and an
// operator-run IdP (custom:, and Entra/Okta/Auth0/Keycloak behind it) may issue
// subs that differ only by case — a base64/base64url sub does this routinely.
// Under the old fold, `custom:AbC` and `custom:abc` were the same user, so a
// self-registerable IdP account was a path to another tenant's hives.
//
// So the comparison is now provider-aware:
//
//   - the PROVIDER label always folds — it is a fixed keyword from
//     identityProviders, and "GitHub:" vs "github:" is just wire noise.
//   - a github SUBJECT folds — GitHub logins are case-insensitive, and folding
//     is required for the legacy shim (a stored "ClubAnderson" must still match
//     an authenticated "github:clubanderson").
//   - every OTHER provider's subject compares EXACTLY. This is the strict
//     direction: it can only ever refuse a match the old code accepted, and the
//     only matches it refuses are ones that were never the same principal.
func canonicalEqual(a, b string) bool {
	ca, cb := canonicalizeLegacy(a), canonicalizeLegacy(b)
	if ca == "" || cb == "" {
		return ca == cb
	}
	pa, sa, oka := parseCanonical(ca)
	pb, sb, okb := parseCanonical(cb)
	if !oka || !okb {
		// Unparseable / unknown-provider ids are not identities we can reason
		// about per-provider. Compare them exactly — the conservative choice.
		return ca == cb
	}
	if pa != pb {
		return false
	}
	if pa == legacyProvider {
		return strings.EqualFold(sa, sb)
	}
	return sa == sb
}

// filenameHexUpper is the alphabet for the "-XX-" escape used by
// encodeUserFilename. Uppercase hex stays inside safeNamePattern.
const filenameHexUpper = "0123456789ABCDEF"

// encodeUserFilename maps a canonical (or legacy) identity to a path-safe on-disk
// stem that matches safeNamePattern (^[a-zA-Z0-9._-]+$). The result is
// "<provider>.<encodedSubject>": the provider name has no dot, so the first dot
// unambiguously separates it from the subject. Any subject byte outside
// [A-Za-z0-9_-] (dot included, to keep the split unambiguous) is escaped as
// "-XX-" hex. github/google/ibmid/redhat subjects are already in this set, so in
// practice the encoding is the identity for real ids and files read cleanly as
// "github.clubanderson", "google.1078...".
//
// A bare legacy key encodes to the SAME stem the pre-migration code wrote
// (a plain GitHub login is [a-zA-Z0-9-] with optional dots), so the load path can
// dual-read: try this encoding, then the legacy "<login>" stem. See loadSaaSUser.
func encodeUserFilename(id string) (string, error) {
	provider, subject, ok := parseCanonical(id)
	if !ok {
		return "", fmt.Errorf("cannot encode invalid identity %q", id)
	}
	var b strings.Builder
	b.WriteString(provider)
	b.WriteByte('.')
	for i := 0; i < len(subject); i++ {
		c := subject[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('-')
		b.WriteByte(filenameHexUpper[c>>4])
		b.WriteByte(filenameHexUpper[c&0x0f])
		b.WriteByte('-')
	}
	stem := b.String()
	if !isValidName(stem) {
		return "", fmt.Errorf("encoded filename %q is not path-safe", stem)
	}
	return stem, nil
}

// decodeUserFilename is the inverse of encodeUserFilename: it turns an on-disk
// stem back into a canonical wire-form id. Used when enumerating the user store
// so a listed user carries its canonical identity. Returns ok=false for a stem
// that is not a valid encoding (e.g. a legacy bare "<login>" stem with no
// provider dot — the caller falls back to treating it as a legacy GitHub login).
func decodeUserFilename(stem string) (string, bool) {
	provider, encSubject, found := strings.Cut(stem, ".")
	if !found || !identityProviders[strings.ToLower(provider)] {
		return "", false
	}
	provider = strings.ToLower(provider)
	var b strings.Builder
	for i := 0; i < len(encSubject); {
		c := encSubject[i]
		if c == '-' && i+3 < len(encSubject) && encSubject[i+3] == '-' {
			hi := hexVal(encSubject[i+1])
			lo := hexVal(encSubject[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 4
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	id, err := makeCanonical(provider, b.String())
	if err != nil {
		return "", false
	}
	return id, true
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}
