package hubbackup

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// decodeBase64 decodes standard base64, tolerating embedded whitespace from
// shell `base64` output.
func decodeBase64(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	return base64.StdEncoding.DecodeString(clean)
}

// OCI Object Storage API and signing constants. These mirror the signing
// scheme already proven in pkg/hub/oci_fss.go.
const (
	// ociObjectStorageHost is the Object Storage endpoint template.
	ociObjectStorageHost = "objectstorage.%s.oraclecloud.com"

	// ociObjectPathTemplate is the REST path for a single object.
	ociObjectPathTemplate = "/n/%s/b/%s/o/%s"

	// ociListPathTemplate is the REST path for listing a bucket's objects.
	ociListPathTemplate = "/n/%s/b/%s/o"

	// ociNamespacePath resolves the tenancy's Object Storage namespace.
	ociNamespacePath = "/n/"

	// ociSignatureVersion is the version field in the Authorization header.
	ociSignatureVersion = "1"

	// ociSigningAlgorithm is the signature algorithm identifier.
	ociSigningAlgorithm = "rsa-sha256"

	// ociGetSignedHeaders lists headers signed for GET/DELETE.
	ociGetSignedHeaders = "date (request-target) host"

	// ociPutSignedHeaders lists headers signed for PUT.
	ociPutSignedHeaders = "date (request-target) host content-length content-type x-content-sha256"

	// ociUploadContentType is the Content-Type for archive uploads.
	ociUploadContentType = "application/octet-stream"

	// ociHTTPTimeout bounds a single Object Storage request. Archives are a
	// few MB, so this is generous.
	ociHTTPTimeout = 180 * time.Second
)

// OCI credential environment variables. Values come from the existing
// oci-api-key Secret; none has a default.
const (
	envOCITenancy     = "OCI_TENANCY_OCID"
	envOCIUser        = "OCI_USER_OCID"
	envOCIFingerprint = "OCI_FINGERPRINT"
	envOCIPrivateKey  = "OCI_PRIVATE_KEY"
	envOCIRegion      = "OCI_REGION"
)

// defaultOCIRegion is the region hosting the hub's tenancy. Non-secret.
const defaultOCIRegion = "us-ashburn-1"

// envOCIEndpoint redirects Object Storage requests away from the real regional
// host. TEST SEAM ONLY — it is never set in production, and TestMain's
// non-loopback dial guard means a test that forgets it cannot reach OCI either.
// It exists so Run and VerifyLatest, which construct their own ObjectStore
// internally, can be exercised end to end.
const envOCIEndpoint = "HIVE_BACKUP_OCI_ENDPOINT"

// ObjectStore uploads and lists encrypted archives in OCI Object Storage.
type ObjectStore struct {
	tenancy     string
	user        string
	fingerprint string
	privateKey  *rsa.PrivateKey
	region      string
	namespace   string
	bucket      string
	client      *http.Client
	// endpointOverride redirects requests away from the real OCI host. Test
	// seam only — see baseURL().
	endpointOverride string
}

// NewObjectStore builds a client from environment credentials.
func NewObjectStore(bucket string) (*ObjectStore, error) {
	tenancy := os.Getenv(envOCITenancy)
	user := os.Getenv(envOCIUser)
	fingerprint := os.Getenv(envOCIFingerprint)
	keyPEM := os.Getenv(envOCIPrivateKey)
	if tenancy == "" || user == "" || fingerprint == "" || keyPEM == "" {
		return nil, fmt.Errorf("OCI credentials incomplete: need %s, %s, %s, %s",
			envOCITenancy, envOCIUser, envOCIFingerprint, envOCIPrivateKey)
	}
	if bucket == "" {
		return nil, fmt.Errorf("%s is not set: no backup destination", EnvBucket)
	}

	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, fmt.Errorf("%s is not valid PEM", envOCIPrivateKey)
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		parsed, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("parse OCI private key: %w", err2)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("OCI private key is not RSA")
		}
		key = rsaKey
	}

	region := os.Getenv(envOCIRegion)
	if region == "" {
		region = defaultOCIRegion
	}

	os_ := &ObjectStore{
		tenancy: tenancy, user: user, fingerprint: fingerprint,
		privateKey: key, region: region, bucket: bucket,
		client:           &http.Client{Timeout: ociHTTPTimeout},
		endpointOverride: strings.TrimSpace(os.Getenv(envOCIEndpoint)),
	}
	ns, err := os_.resolveNamespace()
	if err != nil {
		return nil, fmt.Errorf("resolve Object Storage namespace: %w", err)
	}
	os_.namespace = ns
	return os_, nil
}

func (o *ObjectStore) host() string {
	return fmt.Sprintf(ociObjectStorageHost, o.region)
}

// baseURL returns the scheme+host requests are sent to. endpointOverride is a
// TEST SEAM ONLY: it is never set in production, where every request goes to
// the real regional Object Storage host. Tests point it at an httptest server
// so Put/Get/List/Delete/Prune and the signing path can be exercised end to
// end without OCI credentials or network access.
func (o *ObjectStore) baseURL() string {
	if o.endpointOverride != "" {
		return o.endpointOverride
	}
	return "https://" + o.host()
}

// sign applies the OCI HTTP Signature to a request.
func (o *ObjectStore) sign(req *http.Request, body []byte) error {
	req.Header.Set("date", time.Now().UTC().Format(http.TimeFormat))
	signedHeaders := ociGetSignedHeaders

	if req.Method == http.MethodPut || req.Method == http.MethodPost {
		sum := sha256.Sum256(body)
		checksum := base64.StdEncoding.EncodeToString(sum[:])
		req.Header.Set("x-content-sha256", checksum)
		if req.Method == http.MethodPut {
			req.Header.Set("opc-checksum-algorithm", "SHA256")
			req.Header.Set("opc-content-sha256", checksum)
		}
		req.Header.Set("content-type", ociUploadContentType)
		req.Header.Set("content-length", fmt.Sprintf("%d", len(body)))
		signedHeaders = ociPutSignedHeaders
	}

	requestTarget := strings.ToLower(req.Method) + " " + req.URL.RequestURI()
	var parts []string
	for _, h := range strings.Split(signedHeaders, " ") {
		switch h {
		case "(request-target)":
			parts = append(parts, "(request-target): "+requestTarget)
		case "host":
			parts = append(parts, "host: "+req.Host)
		default:
			parts = append(parts, h+": "+req.Header.Get(h))
		}
	}
	hash := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	sig, err := rsa.SignPKCS1v15(rand.Reader, o.privateKey, crypto.SHA256, hash[:])
	if err != nil {
		return fmt.Errorf("RSA sign: %w", err)
	}
	keyID := o.tenancy + "/" + o.user + "/" + o.fingerprint
	req.Header.Set("Authorization", fmt.Sprintf(
		`Signature version="%s",keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		ociSignatureVersion, keyID, ociSigningAlgorithm, signedHeaders,
		base64.StdEncoding.EncodeToString(sig)))
	return nil
}

// do signs and executes a request, returning the response body.
func (o *ObjectStore) do(method, path string, body []byte) ([]byte, error) {
	url := o.baseURL() + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Host = o.host()
	if err := o.sign(req, body); err != nil {
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read OCI %s response: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OCI %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

// resolveNamespace looks up the tenancy's Object Storage namespace.
func (o *ObjectStore) resolveNamespace() (string, error) {
	body, err := o.do(http.MethodGet, ociNamespacePath, nil)
	if err != nil {
		return "", err
	}
	var ns string
	if err := json.Unmarshal(body, &ns); err != nil {
		return "", fmt.Errorf("parse namespace response: %w", err)
	}
	return ns, nil
}

// Put uploads an object.
func (o *ObjectStore) Put(name string, data []byte) error {
	path := fmt.Sprintf(ociObjectPathTemplate, o.namespace, o.bucket, name)
	_, err := o.do(http.MethodPut, path, data)
	return err
}

// Get downloads an object.
func (o *ObjectStore) Get(name string) ([]byte, error) {
	path := fmt.Sprintf(ociObjectPathTemplate, o.namespace, o.bucket, name)
	return o.do(http.MethodGet, path, nil)
}

// Delete removes an object.
func (o *ObjectStore) Delete(name string) error {
	path := fmt.Sprintf(ociObjectPathTemplate, o.namespace, o.bucket, name)
	_, err := o.do(http.MethodDelete, path, nil)
	return err
}

const (
	legacyArchivePrefix  = "hive-hub-backup-"
	fleetArchivePrefix   = "hive-hub-fleet-backup-"
	partialArchivePrefix = "hive-hub-partial-backup-"
	hubOnlyArchivePrefix = "hive-hub-only-backup-"
	stagingArchivePrefix = "hive-hub-staging-"
	archiveSuffix        = ".tar.gz.enc"
)

func retainedArchiveName(prefix string, at time.Time, data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%s%s-%x%s", prefix, at.UTC().Format("20060102T150405.000000000Z"), digest[:16], archiveSuffix)
}

// publishArchive uploads under a staging name, verifies the bytes returned by
// storage, then publishes those exact bytes under a unique retention name.
func (o *ObjectStore) publishArchive(name string, data []byte, logger *slog.Logger) error {
	staged := stagingArchivePrefix + name
	defer func() {
		if err := o.Delete(staged); err != nil {
			logger.Warn("backup: remove staged archive failed", "object", staged, "err", err)
		}
	}()

	if err := o.Put(staged, data); err != nil {
		return fmt.Errorf("upload staged archive: %w", err)
	}
	fetched, err := o.Get(staged)
	if err != nil {
		return fmt.Errorf("read-back failed: %w", err)
	}
	if !bytes.Equal(fetched, data) {
		return fmt.Errorf("read-back differs from the uploaded archive")
	}
	if err := o.Put(name, fetched); err != nil {
		return fmt.Errorf("publish verified archive: %w", err)
	}
	return nil
}

// listResponse models the Object Storage list payload.
type listResponse struct {
	Objects []struct {
		Name string `json:"name"`
	} `json:"objects"`
	NextStartWith string `json:"nextStartWith"`
}

func archivePrefix(name string) string {
	if strings.HasSuffix(name, archiveSuffix) {
		for _, prefix := range []string{legacyArchivePrefix, fleetArchivePrefix, partialArchivePrefix, hubOnlyArchivePrefix} {
			if strings.HasPrefix(name, prefix) {
				return prefix
			}
		}
	}
	return ""
}

func archiveSortKey(name string) string {
	prefix := archivePrefix(name)
	suffix := strings.TrimPrefix(name, prefix)
	if prefix == legacyArchivePrefix {
		return strings.Replace(suffix, "Z", ".000000000Z", 1)
	}
	return suffix
}

// List returns published and legacy archives in capture order, newest first.
func (o *ObjectStore) List() ([]string, error) {
	basePath := fmt.Sprintf(ociListPathTemplate, o.namespace, o.bucket)
	path := basePath
	var names []string
	seen := make(map[string]bool)
	var start string
	for {
		body, err := o.do(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var lr listResponse
		if err := json.Unmarshal(body, &lr); err != nil {
			return nil, fmt.Errorf("parse list response: %w", err)
		}
		for _, object := range lr.Objects {
			if archivePrefix(object.Name) != "" && !seen[object.Name] {
				seen[object.Name] = true
				names = append(names, object.Name)
			}
		}
		if lr.NextStartWith == "" {
			break
		}
		if lr.NextStartWith <= start {
			return nil, fmt.Errorf("object list pagination did not advance")
		}
		start = lr.NextStartWith
		path = basePath + "?start=" + url.QueryEscape(start)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := archiveSortKey(names[i]), archiveSortKey(names[j])
		if left == right {
			return names[i] > names[j]
		}
		return left > right
	})
	return names, nil
}

// Prune keeps each backup history independently. Legacy archives are protected
// until enough newer full fleet backups exist.
func (o *ObjectStore) Prune(keep int, logger *slog.Logger) error {
	if keep < 1 {
		return fmt.Errorf("backup retention must be positive")
	}
	names, err := o.List()
	if err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, name := range names {
		prefix := archivePrefix(name)
		if prefix == legacyArchivePrefix {
			if counts[fleetArchivePrefix] < keep {
				continue
			}
		} else {
			counts[prefix]++
			if counts[prefix] <= keep {
				continue
			}
		}
		if err := o.Delete(name); err != nil {
			logger.Warn("backup: prune failed", "object", name, "err", err)
			continue
		}
		logger.Info("backup: pruned old archive", "object", name)
	}
	return nil
}
