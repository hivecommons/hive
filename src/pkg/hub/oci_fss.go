package hub

import (
	"bytes"
	"crypto"
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
	"os"
	"strings"
	"sync"
	"time"
)

// OCI File Storage Service API constants
const (
	// ociAPIVersion is the FSS REST API version path prefix.
	ociAPIVersion = "20171215"

	// ociFileSystemsPath is the REST path for file system operations.
	ociFileSystemsPath = "/" + ociAPIVersion + "/fileSystems"

	// ociExportsPath is the REST path for export operations.
	ociExportsPath = "/" + ociAPIVersion + "/exports"

	// ociHTTPTimeoutSec is the HTTP client timeout for OCI API calls.
	ociHTTPTimeoutSec = 30

	// ociSignatureVersion is the version field in the Authorization header.
	ociSignatureVersion = "1"

	// ociSigningAlgorithm is the algorithm identifier for the Authorization header.
	ociSigningAlgorithm = "rsa-sha256"

	// ociGetSignedHeaders lists the headers signed for GET/DELETE requests.
	ociGetSignedHeaders = "date (request-target) host"

	// ociPostSignedHeaders lists the headers signed for POST/PUT requests.
	ociPostSignedHeaders = "date (request-target) host content-length content-type x-content-sha256"

	// ociContentType is the Content-Type header value for OCI API POST requests.
	ociContentType = "application/json"
)

// OCI environment variable names
const (
	envOCITenancy       = "OCI_TENANCY_OCID"
	envOCIUser          = "OCI_USER_OCID"
	envOCIFingerprint   = "OCI_FINGERPRINT"
	envOCIPrivateKey    = "OCI_PRIVATE_KEY"
	envOCIRegion        = "OCI_FSS_REGION"
	envOCICompartmentID = "OCI_COMPARTMENT_ID"
	envOCIAvailDomain   = "OCI_AVAILABILITY_DOMAIN"
	envOCIMountTargetID = "OCI_MOUNT_TARGET_ID"
	envOCIExportSetID   = "OCI_EXPORT_SET_ID"
)

// Default values for OCI configuration (non-secret infrastructure identifiers).
const (
	defaultOCICompartmentID = "ocid1.compartment.oc1..aaaaaaaa6ry2pwgcmatdwrtoll7pjbwomt3zjqs7wy7wa6nmywljrbrjc7wa"
	defaultOCIAvailDomain   = "qKAe:US-ASHBURN-AD-1"
	defaultOCIMountTargetID = "ocid1.mounttarget.oc1.iad.aaaaaa4np2zu32denfqwillqojxwiotjmfsc2ylefuzqaaaa"
	defaultOCIExportSetID   = "ocid1.exportset.oc1.iad.aaaaaa4np2zu32ddnfqwillqojxwiotjmfsc2ylefuzqaaaa"
	defaultOCIRegion        = "us-ashburn-1"
)

// ociConfig holds the cached OCI API authentication and resource configuration.
type ociConfig struct {
	tenancy       string
	user          string
	fingerprint   string
	privateKey    *rsa.PrivateKey
	region        string
	compartmentID string
	availDomain   string
	mountTargetID string
	exportSetID   string
}

var (
	ociCfg     *ociConfig
	ociCfgOnce sync.Once
	ociCfgErr  error
)

// ociFSSEndpointFmt is the endpoint template for the FSS API; the %s
// placeholder is the OCI region identifier. It is a var (not a const) so tests
// can point the FSS calls at a local httptest server. Production never
// reassigns it.
var ociFSSEndpointFmt = "https://filestorage.%s.oraclecloud.com"

// getEnvOrDefault returns the environment variable value, or fallback if unset/empty.
func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadOCIConfig reads OCI credentials and resource IDs from environment variables.
// It is called once and the result is cached for the lifetime of the process.
func loadOCIConfig() (*ociConfig, error) {
	ociCfgOnce.Do(func() {
		tenancy := os.Getenv(envOCITenancy)
		user := os.Getenv(envOCIUser)
		fingerprint := os.Getenv(envOCIFingerprint)
		keyPEM := os.Getenv(envOCIPrivateKey)

		if tenancy == "" || user == "" || fingerprint == "" || keyPEM == "" {
			ociCfgErr = fmt.Errorf("OCI credentials not configured — set %s, %s, %s, and %s",
				envOCITenancy, envOCIUser, envOCIFingerprint, envOCIPrivateKey)
			return
		}

		block, _ := pem.Decode([]byte(keyPEM))
		if block == nil {
			ociCfgErr = fmt.Errorf("failed to decode OCI private key PEM")
			return
		}

		parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			// Fall back to PKCS1 format
			parsedKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				ociCfgErr = fmt.Errorf("failed to parse OCI private key: %w", err)
				return
			}
		}

		rsaKey, ok := parsedKey.(*rsa.PrivateKey)
		if !ok {
			ociCfgErr = fmt.Errorf("OCI private key is not RSA")
			return
		}

		ociCfg = &ociConfig{
			tenancy:       tenancy,
			user:          user,
			fingerprint:   fingerprint,
			privateKey:    rsaKey,
			region:        getEnvOrDefault(envOCIRegion, defaultOCIRegion),
			compartmentID: getEnvOrDefault(envOCICompartmentID, defaultOCICompartmentID),
			availDomain:   getEnvOrDefault(envOCIAvailDomain, defaultOCIAvailDomain),
			mountTargetID: getEnvOrDefault(envOCIMountTargetID, defaultOCIMountTargetID),
			exportSetID:   getEnvOrDefault(envOCIExportSetID, defaultOCIExportSetID),
		}
	})
	return ociCfg, ociCfgErr
}

// ociKeyID returns the keyId value for the OCI Authorization header:
// "{tenancy}/{user}/{fingerprint}".
func ociKeyID(cfg *ociConfig) string {
	return cfg.tenancy + "/" + cfg.user + "/" + cfg.fingerprint
}

// ociSignRequest signs an *http.Request per the OCI HTTP Signature specification.
// For POST requests it also sets Content-Type, Content-Length, and x-content-sha256.
func ociSignRequest(req *http.Request, body []byte, cfg *ociConfig) error {
	req.Header.Set("date", time.Now().UTC().Format(http.TimeFormat))

	requestTarget := strings.ToLower(req.Method) + " " + req.URL.RequestURI()
	signedHeaders := ociGetSignedHeaders

	if req.Method == http.MethodPost || req.Method == http.MethodPut {
		bodyHash := sha256.Sum256(body)
		req.Header.Set("x-content-sha256", base64.StdEncoding.EncodeToString(bodyHash[:]))
		req.Header.Set("content-type", ociContentType)
		req.Header.Set("content-length", fmt.Sprintf("%d", len(body)))
		signedHeaders = ociPostSignedHeaders
	}

	// Build the signing string: each header on its own line as "name: value".
	var signingParts []string
	for _, h := range strings.Split(signedHeaders, " ") {
		switch h {
		case "(request-target)":
			signingParts = append(signingParts, "(request-target): "+requestTarget)
		case "host":
			signingParts = append(signingParts, "host: "+req.Host)
		default:
			signingParts = append(signingParts, h+": "+req.Header.Get(h))
		}
	}
	signingString := strings.Join(signingParts, "\n")

	hash := sha256.Sum256([]byte(signingString))
	sig, err := rsa.SignPKCS1v15(nil, cfg.privateKey, crypto.SHA256, hash[:])
	if err != nil {
		return fmt.Errorf("RSA sign: %w", err)
	}

	authHeader := fmt.Sprintf(
		`Signature version="%s",keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		ociSignatureVersion,
		ociKeyID(cfg),
		ociSigningAlgorithm,
		signedHeaders,
		base64.StdEncoding.EncodeToString(sig),
	)
	req.Header.Set("Authorization", authHeader)
	return nil
}

// createOCIFileSystem creates a new OCI File System in the configured compartment
// and availability domain. It returns the OCID of the newly created file system.
func createOCIFileSystem(displayName string, logger *slog.Logger) (string, error) {
	cfg, err := loadOCIConfig()
	if err != nil {
		return "", fmt.Errorf("load OCI config: %w", err)
	}

	endpoint := fmt.Sprintf(ociFSSEndpointFmt, cfg.region)

	payload := map[string]string{
		"compartmentId":      cfg.compartmentID,
		"availabilityDomain": cfg.availDomain,
		"displayName":        displayName,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal file system request: %w", err)
	}

	reqURL := endpoint + ociFileSystemsPath
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Host = req.URL.Host

	if err := ociSignRequest(req, body, cfg); err != nil {
		return "", fmt.Errorf("sign request: %w", err)
	}

	client := &http.Client{Timeout: ociHTTPTimeoutSec * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OCI FSS API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("OCI FSS API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse OCI FSS response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("OCI FSS response missing file system ID")
	}

	logger.Info("OCI file system created", "displayName", displayName, "fileSystemID", result.ID)
	return result.ID, nil
}

// createOCIExport creates an NFS export for the given file system on the
// configured export set, making it accessible via NFS at the specified path.
// It returns the OCID of the newly created export.
func createOCIExport(fileSystemID, exportPath string, logger *slog.Logger) (string, error) {
	cfg, err := loadOCIConfig()
	if err != nil {
		return "", fmt.Errorf("load OCI config: %w", err)
	}

	endpoint := fmt.Sprintf(ociFSSEndpointFmt, cfg.region)

	payload := map[string]string{
		"exportSetId":  cfg.exportSetID,
		"fileSystemId": fileSystemID,
		"path":         exportPath,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal export request: %w", err)
	}

	reqURL := endpoint + ociExportsPath
	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Host = req.URL.Host

	if err := ociSignRequest(req, body, cfg); err != nil {
		return "", fmt.Errorf("sign request: %w", err)
	}

	client := &http.Client{Timeout: ociHTTPTimeoutSec * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OCI export API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("OCI export API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse OCI export response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("OCI export response missing export ID")
	}

	logger.Info("OCI NFS export created", "fileSystemID", fileSystemID, "exportPath", exportPath, "exportID", result.ID)
	return result.ID, nil
}

// deleteOCIExport deletes an OCI NFS export by its OCID.
// DELETE requests use the same signing headers as GET (date, request-target, host).
func deleteOCIExport(exportID string, logger *slog.Logger) error {
	cfg, err := loadOCIConfig()
	if err != nil {
		return fmt.Errorf("load OCI config: %w", err)
	}

	endpoint := fmt.Sprintf(ociFSSEndpointFmt, cfg.region)
	reqURL := endpoint + ociExportsPath + "/" + exportID

	req, err := http.NewRequest(http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Host = req.URL.Host

	if err := ociSignRequest(req, nil, cfg); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	client := &http.Client{Timeout: ociHTTPTimeoutSec * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("OCI export delete API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 204 No Content is the expected success response for DELETE.
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OCI export delete API returned %d: %s", resp.StatusCode, string(respBody))
	}

	logger.Info("OCI NFS export deleted", "exportID", exportID)
	return nil
}

// deleteOCIFileSystem deletes an OCI File System by its OCID.
// The file system must have no remaining exports before deletion.
func deleteOCIFileSystem(fileSystemID string, logger *slog.Logger) error {
	cfg, err := loadOCIConfig()
	if err != nil {
		return fmt.Errorf("load OCI config: %w", err)
	}

	endpoint := fmt.Sprintf(ociFSSEndpointFmt, cfg.region)
	reqURL := endpoint + ociFileSystemsPath + "/" + fileSystemID

	req, err := http.NewRequest(http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Host = req.URL.Host

	if err := ociSignRequest(req, nil, cfg); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	client := &http.Client{Timeout: ociHTTPTimeoutSec * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("OCI file system delete API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 204 No Content is the expected success response for DELETE.
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OCI file system delete API returned %d: %s", resp.StatusCode, string(respBody))
	}

	logger.Info("OCI file system deleted", "fileSystemID", fileSystemID)
	return nil
}
