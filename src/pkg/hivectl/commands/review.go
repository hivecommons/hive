package commands

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl review evidence` — per-PR review evidence bundles
// (hivecommons/hive#11061). `evidence <owner/repo#N>` downloads a bundle
// from GET /api/review/evidence; `evidence verify <file>` checks one offline
// with no hive access at all.

const reviewEvidenceAPIPath = "/api/review/evidence"

func newReviewCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "review",
		Short:   "Review records: download and verify per-PR review evidence",
		Args:    argsNone(),
		Example: "  hivectl review evidence hivecommons/hive#11061 -o evidence.json\n  hivectl review evidence verify evidence.json --pubkey operator.pub",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newReviewEvidenceCommand(env))
	return cmd
}

// evidenceRef parses `owner/repo#N`, `owner/repo N` or a GitHub pull URL.
func evidenceRef(args []string) (string, int, error) {
	ref := strings.TrimSpace(strings.Join(args, "#"))
	ref = strings.TrimPrefix(ref, "https://github.com/")
	ref = strings.Replace(ref, "/pull/", "#", 1)
	repo, num, ok := strings.Cut(ref, "#")
	owner, name, okRepo := strings.Cut(strings.Trim(repo, "/"), "/")
	n, convErr := strconv.Atoi(strings.TrimSpace(num))
	if !ok || !okRepo || owner == "" || name == "" || strings.Contains(name, "/") || convErr != nil || n <= 0 {
		return "", 0, &usageError{message: fmt.Sprintf("expected owner/repo#N, got %q", ref)}
	}
	return owner + "/" + name, n, nil
}

func newReviewEvidenceCommand(env *commandEnv) *cobra.Command {
	var (
		head    string
		outFile string
		asZip   bool
	)
	cmd := &cobra.Command{
		Use:   "evidence owner/repo#N",
		Short: "Download a PR's review evidence bundle (owner or merger role)",
		Long: "Download the review evidence bundle Hive recorded for a pull request: the newest\n" +
			"head by default, or --head SHA (a unique prefix of 7+ characters works). The bundle\n" +
			"is printed as JSON exactly as Hive sealed it; -o/--output FILE saves it instead (on\n" +
			"this command -o names a file, not an output format). --zip downloads the bundle with\n" +
			"the verdict reports and review link it references and requires -o.\n\n" +
			"Check a saved bundle offline with `hivectl review evidence verify FILE`.",
		Args: wrapArgs(cobra.RangeArgs(1, 2)),
		Example: "  hivectl review evidence hivecommons/hive#11061\n" +
			"  hivectl review evidence hivecommons/hive#11061 --head 0123abc -o evidence.json\n" +
			"  hivectl review evidence hivecommons/hive#11061 --zip -o evidence.zip\n" +
			"  hivectl review evidence verify evidence.json --pubkey operator.pub",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, number, err := evidenceRef(args)
			if err != nil {
				return err
			}
			if asZip && (outFile == "" || outFile == "-") {
				return &usageError{message: "--zip writes a binary archive; pass -o FILE"}
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			query := url.Values{"repo": {repo}, "number": {strconv.Itoa(number)}}
			if head = strings.TrimSpace(head); head != "" {
				query.Set("head", head)
			}
			if asZip {
				query.Set("format", "zip")
			}
			data, _, err := client.Raw(cmd.Context(), http.MethodGet, reviewEvidenceAPIPath, query, nil)
			if err != nil {
				return evidenceAPIError(err)
			}
			if outFile == "" || outFile == "-" {
				if !bytes.HasSuffix(data, []byte("\n")) {
					data = append(data, '\n')
				}
				_, err := cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(outFile, data, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", outFile, err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d bytes)\n", outFile, len(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&head, "head", "", "head SHA (or unique prefix) of the bundle to fetch; default newest")
	// Shadows the global --output/-o on this command only: the bundle is
	// always JSON, so the flag names the file to save it to.
	cmd.Flags().StringVarP(&outFile, "output", "o", "", "write the bundle to FILE instead of stdout")
	cmd.Flags().BoolVar(&asZip, "zip", false, "download a ZIP with the bundle and the artifacts it references (needs -o)")
	cmd.AddCommand(newReviewEvidenceVerifyCommand(env))
	return cmd
}

// evidenceAPIError keeps the API error (and its exit code) but says plainly
// when the hive reports the bundle as expired rather than never recorded.
func evidenceAPIError(err error) error {
	var apiErr *hivectl.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		return err
	}
	var body struct {
		Error   string `json:"error"`
		Expired bool   `json:"expired"`
	}
	if json.Unmarshal(apiErr.Body, &body) != nil || !body.Expired {
		return err
	}
	return &hivectl.APIError{StatusCode: apiErr.StatusCode, Message: "evidence expired: " + body.Error, Body: apiErr.Body}
}

func newReviewEvidenceVerifyCommand(env *commandEnv) *cobra.Command {
	var pubkey string
	cmd := &cobra.Command{
		Use:   "verify FILE",
		Short: "Verify a saved evidence bundle (JSON or ZIP) offline",
		Long: "Recompute the bundle's canonical hash and check its Ed25519 signature, with no\n" +
			"hive access. The public key is --pubkey (hex or base64, or a file holding it);\n" +
			"without it, the key carried by the download is used (manifest.json in a ZIP, or a\n" +
			"public_key field in the JSON). A carried key only proves the bundle is unchanged\n" +
			"since it was signed with that key — compare it with the key your operator publishes.\n" +
			"FILE may be - for stdin. Exits non-zero unless the bundle is signed and verifies.",
		Args:    wrapArgs(cobra.ExactArgs(1)),
		Example: "  hivectl review evidence verify evidence.json --pubkey 3b6a27bc...\n  hivectl review evidence verify evidence.zip",
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readEvidenceInput(env.in, args[0])
			if err != nil {
				return err
			}
			bundleJSON, carriedKey, err := extractEvidenceBundle(data)
			if err != nil {
				return err
			}
			var b evidence.Bundle
			if err := json.Unmarshal(bundleJSON, &b); err != nil {
				return fmt.Errorf("parsing bundle: %w", err)
			}
			result, verr := verifyEvidenceBundle(&b, pubkey, carriedKey)
			if err := env.print(cmd, result); err != nil {
				return err
			}
			return verr
		},
	}
	cmd.Flags().StringVar(&pubkey, "pubkey", "", "operator's Ed25519 public key: hex, base64, or a file containing either")
	return cmd
}

func readEvidenceInput(in io.Reader, path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

// extractEvidenceBundle returns the bundle JSON and any public key the file
// carries: a ZIP's bundle.json and manifest public_key, or a plain JSON
// bundle and its top-level public_key field.
func extractEvidenceBundle(data []byte) ([]byte, string, error) {
	if !bytes.HasPrefix(data, []byte("PK")) {
		var carried struct {
			PublicKey string `json:"public_key"`
		}
		_ = json.Unmarshal(data, &carried)
		return data, carried.PublicKey, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, "", fmt.Errorf("reading evidence archive: %w", err)
	}
	var bundle, manifest []byte
	for _, f := range zr.File {
		var dst *[]byte
		switch {
		case f.Name == "bundle.json" || strings.HasSuffix(f.Name, "/bundle.json"):
			dst = &bundle
		case f.Name == "manifest.json" || strings.HasSuffix(f.Name, "/manifest.json"):
			dst = &manifest
		default:
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "", fmt.Errorf("reading %s: %w", f.Name, err)
		}
		*dst, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, "", fmt.Errorf("reading %s: %w", f.Name, err)
		}
	}
	if bundle == nil {
		return nil, "", errors.New("evidence archive has no bundle.json")
	}
	var m struct {
		PublicKey string `json:"public_key"`
	}
	_ = json.Unmarshal(manifest, &m)
	return bundle, m.PublicKey, nil
}

// parseEvidencePublicKey accepts hex or base64 key text, or a path to a file
// holding it.
func parseEvidencePublicKey(value string) (ed25519.PublicKey, error) {
	text := strings.TrimSpace(value)
	if data, err := os.ReadFile(text); err == nil {
		text = strings.TrimSpace(string(data))
	}
	raw, err := hex.DecodeString(text)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(text)
	}
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes, hex or base64 encoded", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// verifyEvidenceBundle reports what was checked. The error is non-nil unless
// the hash matches and the signature verifies, so scripts can gate on the
// exit code alone.
func verifyEvidenceBundle(b *evidence.Bundle, pubkey, carried string) (map[string]any, error) {
	result := map[string]any{"id": b.ID, "hash": b.Hash, "signed": b.Signed, "verified": false}
	computed, _ := evidence.Hash(b) // only fails for a nil bundle
	hashOK := computed == b.Hash
	result["hash_ok"] = hashOK
	if !hashOK {
		return result, errors.New("hash mismatch: the bundle changed after it was sealed")
	}
	if !b.Signed || b.Signature == "" {
		return result, errors.New("bundle is unsigned: only its hash was checked")
	}
	keyText, source := strings.TrimSpace(pubkey), "flag"
	if keyText == "" {
		keyText, source = strings.TrimSpace(carried), "embedded"
	}
	if keyText == "" {
		return result, &usageError{message: "no public key: pass --pubkey (the bundle carries none)"}
	}
	result["public_key_source"] = source
	pub, err := parseEvidencePublicKey(keyText)
	if err != nil {
		return result, &usageError{message: err.Error()}
	}
	result["public_key"] = hex.EncodeToString(pub)
	if err := evidence.Verify(b, pub); err != nil {
		return result, err
	}
	result["verified"] = true
	return result, nil
}
