package mint

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Caller authentication and entitlement for POST /mint (#3915).
//
// THE FINDING. /mint was gated by a shared bearer secret alone. A shared secret
// proves TRUSTED NETWORK POSITION, not WHO IS CALLING, and the mint placed no
// bound on what a holder could ask for: Minter.Mint validates only that subject
// and audience are non-empty and clamps the TTL, so any holder could mint a
// token for ANY subject, ANY audience and ANY scope — including a privileged
// hub subject — limited only by the TTL ceiling. Mints were logged by subject
// and audience with no record of who requested them.
//
// WHAT THIS FILE CHANGES, AND WHAT IT DELIBERATELY DOES NOT.
//
// Two of the three gaps are closed here without any new dependency:
//
//	identity    CallerAuthenticator is the seam the TODO(caller-auth) asked
//	            for. The shared secret becomes one implementation of it rather
//	            than the only mechanism, so a Kubernetes TokenReview or mTLS
//	            backend drops in behind the same interface without touching the
//	            handler.
//	blast radius Entitlements bound what a VERIFIED identity may mint. Where an
//	            entitlement set is configured the mint is deny-by-default: an
//	            identity may only mint the subjects, audiences and scopes it is
//	            granted, so possession of a credential stops being permission to
//	            mint anything.
//	audit       the authenticated identity is logged on every mint and every
//	            refusal.
//
// The third gap — verifying a Kubernetes ServiceAccount token via TokenReview —
// is NOT implemented here on purpose. It requires k8s.io/client-go, which this
// module does not depend on today, and adding a dependency of that weight is a
// maintainer call rather than something to slip into a security fix. The
// interface below is what makes that a contained, additive change when it is
// made. See the issue for the trade-off.

// Identity is a verified caller of /mint.
//
// Kind names the mechanism that vouched for the caller, so an audit line says
// how the caller was established and not merely that it was. Name is the
// identity within that mechanism and is what Entitlements are keyed on.
type Identity struct {
	// Kind is the authentication mechanism, e.g. "shared-secret" or
	// "serviceaccount".
	Kind string
	// Name is the caller within that mechanism. For a shared secret every
	// holder is indistinguishable, which is the point of the finding, so the
	// name is the constant SharedSecretIdentityName.
	Name string
}

// String renders an identity for logs: "kind:name".
func (i Identity) String() string {
	if i.Kind == "" && i.Name == "" {
		return "unknown"
	}
	return i.Kind + ":" + i.Name
}

// SharedSecretIdentityName is the Name every shared-secret caller gets.
//
// It is deliberately not a per-caller value: a shared secret CANNOT distinguish
// its holders, and inventing a name per request would make the audit log claim
// an identity the mechanism never established. An operator reading
// "shared-secret:any-holder" is being told exactly what was proven — that the
// caller held the secret — and nothing more.
const SharedSecretIdentityName = "any-holder"

// KindSharedSecret is the Identity.Kind for shared-secret callers.
const KindSharedSecret = "shared-secret"

// CallerAuthenticator establishes who is calling /mint.
//
// Implementations must be safe for concurrent use. An error means the caller is
// not authenticated; the handler answers 401 without distinguishing which of
// missing, malformed or wrong credential it was.
type CallerAuthenticator interface {
	// Authenticate returns the verified identity of the request's caller.
	Authenticate(r *http.Request) (Identity, error)
	// Name identifies the mechanism for logs and errors.
	Name() string
}

// ErrUnauthenticated is returned by a CallerAuthenticator when the caller
// cannot be established. Callers of Authenticate must not surface the specific
// reason to the client.
var ErrUnauthenticated = fmt.Errorf("mint: caller not authenticated")

// SharedSecretAuthenticator is the original gate, now behind the interface: a
// constant-time comparison against a shared bearer secret.
//
// It authenticates POSSESSION OF A SECRET, not a caller. Every holder gets the
// same identity. It remains the default so existing behaviour is byte-identical
// until an operator configures something stronger.
type SharedSecretAuthenticator struct {
	secret string
}

// NewSharedSecretAuthenticator builds the shared-secret gate. An empty secret
// is rejected — an empty secret would let anyone mint (fail closed).
func NewSharedSecretAuthenticator(secret string) (*SharedSecretAuthenticator, error) {
	if secret == "" {
		return nil, fmt.Errorf("mint: shared secret is required (fail closed)")
	}
	return &SharedSecretAuthenticator{secret: secret}, nil
}

// Name implements CallerAuthenticator.
func (a *SharedSecretAuthenticator) Name() string { return KindSharedSecret }

// Authenticate implements CallerAuthenticator.
func (a *SharedSecretAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, authScheme) {
		return Identity{}, ErrUnauthenticated
	}
	presented := strings.TrimPrefix(h, authScheme)
	// Constant-time compare; ConstantTimeCompare returns 0 on length mismatch
	// without leaking which byte differed.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(a.secret)) != 1 {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Kind: KindSharedSecret, Name: SharedSecretIdentityName}, nil
}

// Entitlement is what one verified identity may mint.
//
// Every field is an allow-list and an EMPTY LIST MEANS NOTHING IS ALLOWED for
// that dimension, not everything. That is the deny-by-default the finding asks
// for: an entitlement that grants an audience but forgets the scopes grants a
// token with no scopes, never a token with every scope.
//
// A "*" entry is an explicit, greppable wildcard for that dimension — for a
// caller that genuinely mints arbitrary subjects, an operator has to write it
// down rather than get it by omission.
type Entitlement struct {
	// Subjects the identity may mint tokens FOR.
	Subjects []string
	// Audiences the identity may mint tokens for.
	Audiences []string
	// Scopes the identity may request. A request for any scope outside this
	// set is refused rather than silently reduced — a caller that believes it
	// holds a scope it does not is a bug worth surfacing.
	Scopes []string
}

// Wildcard is the explicit "any value" entry inside an Entitlement dimension.
const Wildcard = "*"

// Entitlements maps a verified Identity.Name to what it may mint.
//
// A nil or empty map means entitlements are NOT configured, and the mint keeps
// its historical behaviour of allowing any subject/audience/scope. That is the
// only backwards-compatible default, and NewServer logs a warning when it is
// the case so the posture is visible rather than assumed. Once the map is
// non-empty it is authoritative: an identity absent from it may mint nothing.
type Entitlements map[string]Entitlement

// permits reports whether the identity may mint this request, and if not, why.
// The reason is for the server's own log; it is never returned to the client.
func (e Entitlements) permits(id Identity, subject, audience string, scopes []string) (bool, string) {
	if len(e) == 0 {
		return true, ""
	}
	ent, ok := e[id.Name]
	if !ok {
		return false, "identity has no entitlement entry"
	}
	if !allowed(ent.Subjects, subject) {
		return false, "subject not entitled"
	}
	if !allowed(ent.Audiences, audience) {
		return false, "audience not entitled"
	}
	for _, s := range scopes {
		if !allowed(ent.Scopes, s) {
			return false, "scope not entitled: " + s
		}
	}
	return true, ""
}

// allowed reports whether value is in list, treating Wildcard as any value. An
// empty list allows nothing.
func allowed(list []string, value string) bool {
	for _, v := range list {
		if v == Wildcard || v == value {
			return true
		}
	}
	return false
}

// identityNames returns the configured identity names, sorted, for logging.
func (e Entitlements) identityNames() []string {
	names := make([]string, 0, len(e))
	for k := range e {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
