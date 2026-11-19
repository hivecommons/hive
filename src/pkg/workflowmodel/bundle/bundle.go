// Package bundle defines the portable interchange format for workflow models
// (#17620 phase 7). A bundle carries a model, the profile the publisher
// compiled from it, and a sha256 of the canonical model JSON so a receiving
// spoke can verify the model was not altered in transit and that the published
// profile is exactly what a local compile produces. Importers never trust the
// published profile: Verify recompiles and compares structurally, and callers
// are expected to recompile again when they store the model.
//
// The package depends only on pkg/workflowmodel so it can be shared by the
// spoke dashboard, the hub marketplace and CLI tooling.
package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// APIVersion identifies the bundle envelope format.
const APIVersion = "workflow.hivecommons.dev/v1alpha1"

// Kind is the envelope kind.
const Kind = "WorkflowBundle"

// Provenance tiers a bundle may carry. They extend the model provenance
// vocabulary with the marketplace-only `local-published` tier used for
// bundles a spoke publishes from its own library.
const (
	TierBuiltIn            = string(workflowmodel.ProvenanceBuiltIn)
	TierCurated            = string(workflowmodel.ProvenanceCurated)
	TierIngestedUnreviewed = string(workflowmodel.ProvenanceIngestedUnreviewed)
	TierLocal              = string(workflowmodel.ProvenanceLocal)
	TierLocalPublished     = "local-published"
)

// Errors returned by Verify and Decode.
var (
	ErrBadVersion      = errors.New("bundle: unsupported apiVersion")
	ErrBadKind         = errors.New("bundle: unsupported kind")
	ErrMissingModel    = errors.New("bundle: missing model")
	ErrMissingProfile  = errors.New("bundle: missing profile")
	ErrHashMismatch    = errors.New("bundle: hash does not match canonical model")
	ErrProfileMismatch = errors.New("bundle: published profile does not match local compile")
	ErrBadTier         = errors.New("bundle: unknown provenance tier")
)

// Bundle is the portable envelope.
type Bundle struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Model      *workflowmodel.Model   `json:"model"`
	Profile    *workflowmodel.Profile `json:"profile"`
	// Hash is the sha256 (hex) of the canonical model JSON; see CanonicalHash.
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy,omitempty"`
	// Tier is the provenance tier the publisher asserts. Receivers apply
	// their own policy (the hub only honours curated/built-in from admins).
	Tier string `json:"tier,omitempty"`
}

// Key is the catalogue key `name@version`.
func (b *Bundle) Key() string {
	if b == nil || b.Model == nil {
		return ""
	}
	return b.Model.Metadata.Name + "@" + b.Model.Spec.Meta.Version
}

// KnownTier reports whether t is a provenance tier the bundle format defines.
func KnownTier(t string) bool {
	switch t {
	case TierBuiltIn, TierCurated, TierIngestedUnreviewed, TierLocal, TierLocalPublished:
		return true
	}
	return false
}

// Export validates and compiles m, then wraps it with its profile and hash.
// It refuses models that do not compile, so a bundle always carries a
// profile consistent with its model at creation time.
func Export(m *workflowmodel.Model, by string, now time.Time) (*Bundle, error) {
	if m == nil {
		return nil, ErrMissingModel
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	profile, _, err := workflowmodel.Compile(m)
	if err != nil {
		return nil, err
	}
	tier := strings.TrimSpace(string(m.Spec.Meta.Provenance))
	if tier == "" {
		tier = TierLocal
	}
	if !KnownTier(tier) {
		return nil, fmt.Errorf("%w: %q", ErrBadTier, tier)
	}
	return &Bundle{
		APIVersion: APIVersion,
		Kind:       Kind,
		Model:      m,
		Profile:    profile,
		Hash:       CanonicalHash(m),
		CreatedAt:  now.UTC(),
		CreatedBy:  strings.TrimSpace(by),
		Tier:       tier,
	}, nil
}

// Encode serialises b as canonical JSON (object keys sorted recursively, no
// insignificant whitespace), so equal bundles produce byte-identical output
// and hashes over encoded bundles are stable.
func Encode(b *Bundle) ([]byte, error) {
	if b == nil {
		return nil, ErrMissingModel
	}
	return canonicalJSON(b)
}

// Decode parses a bundle and checks the envelope fields. It does not verify
// hashes or profiles; call Verify for that.
func Decode(data []byte) (*Bundle, error) {
	var b Bundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("bundle: decode: %w", err)
	}
	if b.APIVersion != APIVersion {
		return nil, fmt.Errorf("%w: %q", ErrBadVersion, b.APIVersion)
	}
	if b.Kind != "" && b.Kind != Kind {
		return nil, fmt.Errorf("%w: %q", ErrBadKind, b.Kind)
	}
	b.Kind = Kind
	if b.Model == nil {
		return nil, ErrMissingModel
	}
	if b.Profile == nil {
		return nil, ErrMissingProfile
	}
	return &b, nil
}

// Verify checks that the bundle is internally consistent: the envelope is
// well-formed, the hash matches the canonical model JSON, the model validates
// and compiles locally, and the compiled profile structurally equals the
// published one. A mismatch is reported as ErrProfileMismatch.
func Verify(b *Bundle) error {
	if b == nil || b.Model == nil {
		return ErrMissingModel
	}
	if b.APIVersion != APIVersion {
		return fmt.Errorf("%w: %q", ErrBadVersion, b.APIVersion)
	}
	if b.Kind != "" && b.Kind != Kind {
		return fmt.Errorf("%w: %q", ErrBadKind, b.Kind)
	}
	if b.Profile == nil {
		return ErrMissingProfile
	}
	if b.Tier != "" && !KnownTier(b.Tier) {
		return fmt.Errorf("%w: %q", ErrBadTier, b.Tier)
	}
	if got := CanonicalHash(b.Model); got != b.Hash {
		return fmt.Errorf("%w (have %s, want %s)", ErrHashMismatch, short(b.Hash), short(got))
	}
	if err := b.Model.Validate(); err != nil {
		return err
	}
	local, _, err := workflowmodel.Compile(b.Model)
	if err != nil {
		return err
	}
	want, err := canonicalJSON(local)
	if err != nil {
		return err
	}
	have, err := canonicalJSON(b.Profile)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, have) {
		return ErrProfileMismatch
	}
	return nil
}

// CanonicalHash is the sha256 (hex) of the canonical JSON of the model's
// spec and metadata, independent of key order or formatting in the source
// document. It differs from Model.Hash, which covers the spec only, because
// the catalogue key includes the name.
func CanonicalHash(m *workflowmodel.Model) string {
	if m == nil {
		return ""
	}
	data, err := canonicalJSON(struct {
		APIVersion string                 `json:"apiVersion"`
		Kind       string                 `json:"kind"`
		Metadata   workflowmodel.Metadata `json:"metadata"`
		Spec       workflowmodel.Spec     `json:"spec"`
	}{m.APIVersion, m.Kind, m.Metadata, m.Spec})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON marshals v, re-decodes into generic values and re-encodes
// with sorted keys and no whitespace. Go's encoding/json already sorts map
// keys, so a round trip through interface{} normalises struct field order.
func canonicalJSON(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v interface{}) error {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []interface{}:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "<empty>"
	}
	return h
}
