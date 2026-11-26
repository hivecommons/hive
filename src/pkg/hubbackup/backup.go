// Package hubbackup implements disaster-recovery backup for the Hive hub.
//
// It captures the minimum set of state required to reconstruct the hub and
// every registered spoke after total loss of the hub cluster:
//
//   - /data/saas/**            hub SaaS state (users, hives, keys, timeline)
//   - /data/hub-registry.json  the fleet registry
//   - Kubernetes Secrets       credentials that live OUTSIDE the PVC
//   - per-spoke config         hive.yaml.runtime, gh-app-key*.pem, hive-id
//
// Deliberately EXCLUDED as regenerable agent scratch: nous/, home/, beads/,
// logs/. Including them would grow a ~5MB archive into gigabytes.
//
// # Encryption
//
// The archive is a tar.gz sealed with AES-256-GCM. The key comes from governor
// config (governor.backup.key_file, settable from the dashboard by a hosted
// spoke owner) or, as a fallback, the HIVE_BACKUP_KEY environment variable. It
// has NO default — an unresolvable key is a hard error, never a silent
// plaintext write.
//
// HIVE_BACKUP_KEY is deliberately INDEPENDENT of /data/saas/hmac.key. hmac.key
// is itself part of the backup payload; deriving the backup key from it would
// make the archive undecryptable in exactly the scenario the backup exists for
// (the hub is gone). The operator MUST escrow HIVE_BACKUP_KEY outside the
// cluster. See docs/HUB_DISASTER_RECOVERY.md.
package hubbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Environment variable names. No secret has a default value.
const (
	// EnvBackupKey holds the hex-encoded AES-256 key used to seal the archive.
	// It is the deployment-level fallback; governor.backup.key_file takes
	// precedence so hosted owners can configure a key without env access.
	// No key from any source is a fatal error.
	EnvBackupKey = config.BackupKeyEnv

	// EnvDataDir overrides the hub data directory (test seam).
	EnvDataDir = "HIVE_BACKUP_DATA_DIR"

	// EnvBucket names the OCI Object Storage bucket receiving archives.
	EnvBucket = "HIVE_BACKUP_BUCKET"

	// EnvRetention overrides how many archives to retain.
	EnvRetention = "HIVE_BACKUP_RETENTION"
)

const (
	// restoreFileMode is the permission mask applied to every file extracted
	// from a backup archive (audit §8 / prior N18).
	//
	// The mode used to come straight from the tar header — os.FileMode(hdr.Mode)
	// — which is attacker-controlled for any archive the hub did not itself
	// produce. Verified empirically: with a permissive umask a header carrying
	// 0777/0666 restores a WORLD-WRITABLE file, so anyone on the box can then
	// rewrite restored hub state. (setuid/setgid do NOT survive os.WriteFile,
	// so that half of the original finding is moot — but the world bits are
	// real, and a umask is a deployment accident, not a security control.)
	//
	// Mask to the permission bits, then clear group/other write. An archive can
	// still ask for a more RESTRICTIVE mode; it can no longer ask for a looser
	// one.
	restoreFileMode = 0o777 &^ 0o022

	// restoreMaxFileBytes bounds a single extracted member. io.ReadAll(tr) was
	// unbounded, so a decompression bomb — a few KB of gzip expanding to
	// gigabytes — was read wholly into memory and OOM'd the hub. 256 MiB is far
	// above any real hub artifact (the largest is the knowledge vault) while
	// still bounding the blast radius.
	restoreMaxFileBytes = 256 << 20
)

// readArchiveMember reads one tar member with a hard size ceiling, so a
// decompression bomb fails with a clear error instead of exhausting memory.
//
// Reads one byte PAST the limit so an over-size member is detected rather than
// silently truncated — a truncated restore would be worse than a failed one.
func readArchiveMember(tr io.Reader, name string) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(tr, restoreMaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > restoreMaxFileBytes {
		return nil, fmt.Errorf("archive member %q exceeds the %d-byte restore limit", name, int64(restoreMaxFileBytes))
	}
	return content, nil
}

// Filesystem layout of hub state.
const (
	// DefaultDataDir is the hub PVC mount point.
	DefaultDataDir = "/data"

	// saasSubdir is the hub SaaS state directory, relative to the data dir.
	saasSubdir = "saas"

	// registryFile is the fleet registry filename, relative to the data dir.
	registryFile = "hub-registry.json"

	clustersFile             = "clusters.json"
	credentialKeyFile        = "hmac.key"
	heartbeatMasterFile      = "hub-secret.key"
	heartbeatGenerations     = "hub-generations.json"
	recoveryQuarantineSuffix = ".corrupt"

	// manifestName is the integrity manifest stored inside every archive.
	manifestName = "MANIFEST.json"
)

// Archive layout: top-level directories inside the tarball.
const (
	// hubPrefix holds files copied from the hub PVC.
	hubPrefix = "hub"

	// secretsPrefix holds Kubernetes Secrets serialized as JSON.
	secretsPrefix = "secrets"

	// spokesPrefix holds per-spoke config, one subdirectory per hive ID.
	spokesPrefix = "spokes"
)

// Crypto parameters.
const (
	// aesKeySize is the AES-256 key length in bytes.
	aesKeySize = 32

	// backupFormatVersion is bumped when the archive layout changes so a
	// restore can refuse an archive it does not understand.
	backupFormatVersion = 1
)

// Retention.
const (
	// DefaultRetention is how many archives to keep before pruning oldest.
	DefaultRetention = 30
)

// excludedDirs are regenerable agent working directories skipped from the
// hub PVC copy. They dominate disk usage but carry no unrecoverable state.
var excludedDirs = map[string]bool{
	"nous":  true,
	"home":  true,
	"beads": true,
	"logs":  true,
}

// FileEntry records one archived file for integrity verification.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the integrity and provenance record embedded in each archive.
type Manifest struct {
	FormatVersion int         `json:"format_version"`
	CreatedAt     time.Time   `json:"created_at"`
	HubDataDir    string      `json:"hub_data_dir"`
	Files         []FileEntry `json:"files"`

	// HubErrors records archive-relative paths of missing or unreadable hub state.
	HubErrors map[string]string `json:"hub_errors,omitempty"`

	// SpokeIDs lists every hive whose config was captured.
	SpokeIDs []string `json:"spoke_ids"`

	SpokesSkipped bool `json:"spokes_skipped,omitempty"`

	// SpokeErrors records hives that could NOT be captured, so a restore
	// operator learns about gaps from the archive itself rather than by
	// discovering missing spokes mid-recovery.
	SpokeErrors map[string]string `json:"spoke_errors,omitempty"`

	// SecretNames lists the Kubernetes Secrets captured.
	SecretNames []string `json:"secret_names,omitempty"`
}

// LoadKey reads and validates the AES-256 backup key from the environment.
// It returns an error when unset so callers fail loudly rather than writing
// an unencrypted archive.
//
// Callers that have a hive config in hand should prefer ResolveKey, which also
// honours the governor-config key file a hosted spoke owner can set without
// deployment-env access (#4129). LoadKey stays strictly env-only: the hub cron
// backup and the hive-backup CLI seal and open archives with the key an
// operator escrowed, and a key file that happened to exist on a mounted volume
// must never outrank it.
func LoadKey() ([]byte, error) {
	return ResolveKey(nil)
}

// ResolveKey validates the backup key located by cfg (governor config file
// first, HIVE_BACKUP_KEY as the fallback). A nil cfg resolves the environment
// only.
//
// Fail-closed is the whole point: every failure path returns an error and no
// key, so a caller can never proceed to write a plaintext archive.
func ResolveKey(cfg *config.BackupConfig) ([]byte, error) {
	key, _, err := ResolveKeyWithSource(cfg)
	return key, err
}

// ResolveKeyWithSource additionally reports WHERE the key came from, in the
// safe-to-log "file:<path>" / "env:<NAME>" form. The value is never returned
// as a string and never appears in an error message.
func ResolveKeyWithSource(cfg *config.BackupConfig) ([]byte, string, error) {
	raw, source := cfg.ResolveKeyWithSource()
	if raw == "" {
		return nil, "", fmt.Errorf(
			"no backup encryption key is configured: refusing to write an "+
				"unencrypted backup (set one in Settings → Governor → Security → "+
				"Backup encryption key, or set %s on the deployment; generate one "+
				"with: openssl rand -hex 32)", EnvBackupKey)
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		// The decode error from hex reports the offending BYTE, so it is
		// deliberately not wrapped in: it would echo part of the key.
		return nil, "", fmt.Errorf("backup encryption key (%s) must be %d hex characters (%d-byte AES-256 key)",
			source, aesKeySize*2, aesKeySize)
	}
	if len(key) != aesKeySize {
		return nil, "", fmt.Errorf("backup encryption key (%s) decoded to %d bytes, want %d (AES-256)",
			source, len(key), aesKeySize)
	}
	return key, source, nil
}

// Seal encrypts plaintext with AES-256-GCM, returning nonce||ciphertext.
func Seal(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts data produced by Seal. A wrong key or any tampering fails the
// GCM authentication tag, so this doubles as an integrity check.
func Open(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short: %d bytes", len(data))
	}
	plaintext, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed (wrong backup encryption key — governor.backup.key_file or %s — or archive corrupted): %w",
			EnvBackupKey, err)
	}
	return plaintext, nil
}

// DataDir returns the hub data directory, honouring the test-seam override.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvDataDir)); v != "" {
		return v
	}
	return DefaultDataDir
}

// Retention returns the configured archive retention count.
func Retention() int {
	if v := strings.TrimSpace(os.Getenv(EnvRetention)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return DefaultRetention
}

// ArchiveName returns the object name for a backup taken at t.
func ArchiveName(t time.Time) string {
	return legacyArchivePrefix + t.UTC().Format("20060102T150405Z") + archiveSuffix
}

// builder accumulates files into a gzipped tar and records their digests.
type builder struct {
	tw    *tar.Writer
	gz    *gzip.Writer
	buf   *strings.Builder
	files []FileEntry
}

// addBytes writes one in-memory file into the archive and records its digest.
func (b *builder) addBytes(name string, mode int64, content []byte) error {
	sum := sha256.Sum256(content)
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	}
	if err := b.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := b.tw.Write(content); err != nil {
		return err
	}
	b.files = append(b.files, FileEntry{
		Path:   name,
		Size:   int64(len(content)),
		SHA256: hex.EncodeToString(sum[:]),
	})
	return nil
}

type archivedSaaS struct {
	gaps            map[string]string
	clusters        []byte
	hostedClusters  map[string]string
	heartbeatMaster bool
}

// addTree copies the SaaS tree and retains the captured cluster map for spoke selection.
func (b *builder) addTree(srcDir, dstPrefix string, skipExcluded bool) (archivedSaaS, error) {
	saas := archivedSaaS{gaps: make(map[string]string), hostedClusters: make(map[string]string)}
	gaps := saas.gaps
	var needsKey, capturedKey bool
	var capturedGenerations, quarantinedGenerations, quarantinedClusters bool
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		if (d == nil || !d.IsDir()) && isSaaSWriteTemp(rel) {
			return nil
		}
		name := filepath.Join(dstPrefix, rel)
		switch rel {
		case heartbeatGenerations + recoveryQuarantineSuffix:
			quarantinedGenerations = true
		case clustersFile + recoveryQuarantineSuffix:
			quarantinedClusters = true
		}
		if err != nil {
			gaps[name] = hubReadError(err)
			return nil
		}
		if rel == "." {
			if !d.IsDir() {
				gaps[name] = "expected a directory"
			}
			return nil
		}
		if d.IsDir() {
			switch rel {
			case clustersFile, credentialKeyFile, heartbeatMasterFile, heartbeatGenerations:
				gaps[name] = "not a regular file"
				return filepath.SkipDir
			}
			if skipExcluded && excludedDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			gaps[name] = "not a regular file"
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			gaps[name] = hubReadError(readErr)
			return nil
		}
		switch rel {
		case clustersFile:
			saas.clusters = content
		case credentialKeyFile:
			capturedKey = true
			if len(content) != aesKeySize {
				gaps[name] = "credential key has the wrong length"
			}
		case heartbeatMasterFile:
			saas.heartbeatMaster = saas.heartbeatMaster || len(bytes.TrimSpace(content)) > 0
		case heartbeatGenerations:
			capturedGenerations = true
			if !hasCurrentMasterGeneration(content) {
				gaps[name] = "no usable current heartbeat master generation"
			}
		default:
			dir := filepath.Dir(rel)
			if filepath.Dir(dir) == "hives" && filepath.Base(rel) == "meta.json" {
				var hive struct {
					ClusterID string `json:"cluster_id"`
				}
				if err := json.Unmarshal(content, &hive); err != nil {
					gaps[name] = "invalid hosted hive metadata"
				} else {
					saas.hostedClusters[filepath.Base(dir)] = hive.ClusterID
				}
			}
			if !needsKey && dir == "users" && filepath.Ext(rel) == ".json" {
				// Hubs that have never stored a GitHub token may have no key yet.
				var user struct {
					EncryptedToken string `json:"encrypted_token"`
				}
				needsKey = json.Unmarshal(content, &user) != nil || user.EncryptedToken != ""
			}
		}
		info, infoErr := d.Info()
		mode := int64(0o600)
		if infoErr == nil {
			mode = int64(info.Mode().Perm())
		}
		return b.addBytes(name, mode, content)
	})
	keyPath := filepath.Join(dstPrefix, credentialKeyFile)
	if needsKey && !capturedKey && gaps[keyPath] == "" {
		gaps[keyPath] = "credential key is missing from the archive"
	}
	generationsPath := filepath.Join(dstPrefix, heartbeatGenerations)
	if quarantinedGenerations && !capturedGenerations && gaps[generationsPath] == "" {
		gaps[generationsPath] = "rotation state was quarantined and has no replacement"
	}
	clustersPath := filepath.Join(dstPrefix, clustersFile)
	if quarantinedClusters && saas.clusters == nil && gaps[clustersPath] == "" {
		gaps[clustersPath] = "cluster registry was quarantined and has no replacement"
	}
	return saas, err
}

// hasCurrentMasterGeneration mirrors the hub loader's requirement that a
// persisted rotation has a current generation with a nonblank master.
func hasCurrentMasterGeneration(data []byte) bool {
	var state struct {
		Current     int       `json:"current"`
		RotatedAt   time.Time `json:"rotated_at"`
		Generations []struct {
			ID          int       `json:"id"`
			Secret      string    `json:"secret"`
			Created     time.Time `json:"created"`
			VerifyUntil time.Time `json:"verify_until"`
		} `json:"generations"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return false
	}
	for _, gen := range state.Generations {
		if gen.ID == state.Current && strings.TrimSpace(gen.Secret) != "" {
			return true
		}
	}
	return false
}

func hasArchivedHubSecret(item SecretItem) bool {
	if item.Name != "hive-hub-secrets" {
		return false
	}
	var secret struct {
		Kind string            `json:"kind"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(item.JSON, &secret); err != nil || secret.Kind != "Secret" {
		return false
	}
	master, err := base64.StdEncoding.DecodeString(secret.Data["HIVE_HUB_SECRET"])
	return err == nil && len(bytes.TrimSpace(master)) > 0
}

// isSaaSWriteTemp recognizes files left in place only until an atomic rename.
func isSaaSWriteTemp(rel string) bool {
	name := filepath.Base(rel)
	if strings.HasSuffix(name, ".tmp") {
		return true
	}
	switch filepath.Dir(rel) {
	case "app-keys":
		return strings.HasPrefix(name, ".") && strings.Contains(name, ".pem.tmp") && filepath.Ext(name) != ".pem"
	case "consents":
		return strings.HasPrefix(name, ".consent-") && filepath.Ext(name) != ".json"
	default:
		return false
	}
}

func hubReadError(err error) string {
	if pathErr, ok := err.(*os.PathError); ok {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// Build seals the captured recovery files and returns the archive and its manifest.
func Build(key []byte, collector SpokeCollector, secrets SecretCollector, logger *slog.Logger) ([]byte, *Manifest, error) {
	return build(key, collector, secrets, useSpokeCollector, nil, logger)
}

func build(key []byte, collector SpokeCollector, secrets SecretCollector, capture spokeCapture, validateRegistry func([]byte) error, logger *slog.Logger) ([]byte, *Manifest, error) {
	dataDir := DataDir()

	var raw strings.Builder
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	b := &builder{tw: tw, gz: gz, buf: &raw}

	man := &Manifest{
		FormatVersion: backupFormatVersion,
		CreatedAt:     time.Now().UTC(),
		HubDataDir:    dataDir,
		SpokeErrors:   map[string]string{},
		SpokesSkipped: capture == skipSpokeCapture,
	}

	// 1. Hub SaaS state — users, hives, hmac.key, hub-secret.key, app-keys.
	saasDir := filepath.Join(dataDir, saasSubdir)
	saasPrefix := filepath.Join(hubPrefix, saasSubdir)
	saas, err := b.addTree(saasDir, saasPrefix, false)
	if err != nil {
		return nil, nil, fmt.Errorf("archive saas dir: %w", err)
	}
	gaps := saas.gaps
	man.HubErrors = gaps

	// 2. Fleet registry.
	regPath := filepath.Join(dataDir, registryFile)
	regArchivePath := filepath.Join(hubPrefix, registryFile)
	registry, regErr := os.ReadFile(regPath)
	if regErr == nil {
		if err := b.addBytes(regArchivePath, 0o644, registry); err != nil {
			return nil, nil, fmt.Errorf("archive registry: %w", err)
		}
	} else {
		gaps[regArchivePath] = hubReadError(regErr)
	}
	if regErr == nil && validateRegistry != nil {
		if err := validateRegistry(registry); err != nil {
			gaps[regArchivePath] = err.Error()
		}
	}
	clusters, hives := backupTargets(saas, registry, logger)
	if capture == collectArchivedSpokes {
		clustersPath := filepath.Join(saasPrefix, clustersFile)
		if gaps[clustersPath] != "" || gaps[regArchivePath] != "" {
			return nil, nil, fmt.Errorf("enumerate spokes: %v", gaps)
		}
		logger.Info("backup: enumerated archived fleet", "clusters", len(clusters), "hives", len(hives))
		collector = KubectlSpokeCollector{Clusters: clusters, Hives: hives}
	}

	// 3. Kubernetes Secrets that live outside the PVC. Without these the
	//    archive is NOT restorable, so a failure here is fatal, not a warning.
	if secrets != nil {
		items, err := secrets.Collect(logger)
		if err != nil {
			return nil, nil, fmt.Errorf("collect kubernetes secrets: %w", err)
		}
		for _, it := range items {
			if err := b.addBytes(filepath.Join(secretsPrefix, it.Name+".json"), 0o600, it.JSON); err != nil {
				return nil, nil, fmt.Errorf("archive secret %s: %w", it.Name, err)
			}
			man.SecretNames = append(man.SecretNames, it.Name)
			saas.heartbeatMaster = saas.heartbeatMaster || hasArchivedHubSecret(it)
		}
	}
	masterPath := filepath.Join(saasPrefix, heartbeatMasterFile)
	if !saas.heartbeatMaster && gaps[masterPath] == "" {
		gaps[masterPath] = "no bootstrap master in hub-secret.key or the archived hub secret"
	}
	for name, reason := range gaps {
		logger.Warn("backup: incomplete hub state", "path", name, "reason", reason)
	}

	// 4. Per-spoke config. Individual spoke failures are recorded in the
	//    manifest rather than aborting the fleet-wide backup.
	if collector != nil {
		spokes, err := collector.Collect(logger)
		if err != nil {
			logger.Error("backup: spoke collection failed", "err", err)
			man.SpokeErrors["_collector"] = err.Error()
		}
		for _, sp := range spokes {
			if sp.Err != "" {
				man.SpokeErrors[sp.ID] = sp.Err
				continue
			}
			for name, content := range sp.Files {
				if err := b.addBytes(filepath.Join(spokesPrefix, sp.ID, name), 0o600, content); err != nil {
					return nil, nil, fmt.Errorf("archive spoke %s/%s: %w", sp.ID, name, err)
				}
			}
			man.SpokeIDs = append(man.SpokeIDs, sp.ID)
		}
	}

	// 5. Manifest last, so it covers every preceding file. Copying the
	//    accumulated digests in is what makes Verify meaningful — an empty
	//    Files list would make verification silently vacuous.
	man.Files = b.files

	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal manifest: %w", err)
	}
	manHdr := &tar.Header{Name: manifestName, Mode: 0o644, Size: int64(len(manJSON)), ModTime: time.Now()}
	if err := tw.WriteHeader(manHdr); err != nil {
		return nil, nil, err
	}
	if _, err := tw.Write(manJSON); err != nil {
		return nil, nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, nil, fmt.Errorf("close gzip: %w", err)
	}

	sealed, err := Seal(key, []byte(raw.String()))
	if err != nil {
		return nil, nil, fmt.Errorf("seal archive: %w", err)
	}
	return sealed, man, nil
}

// Verify decrypts an archive and checks the archived files against the
// embedded manifest. The manifest also records any known collection gaps.
func Verify(key, sealed []byte) (*Manifest, error) {
	plain, err := Open(key, sealed)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(strings.NewReader(string(plain)))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }() // read-only reader; nothing to lose on close error

	tr := tar.NewReader(gz)
	digests := map[string]string{}
	var man *Manifest

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		content, err := readArchiveMember(tr, hdr.Name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", hdr.Name, err)
		}
		if hdr.Name == manifestName {
			man = &Manifest{}
			if err := json.Unmarshal(content, man); err != nil {
				return nil, fmt.Errorf("parse manifest: %w", err)
			}
			continue
		}
		sum := sha256.Sum256(content)
		digests[hdr.Name] = hex.EncodeToString(sum[:])
	}

	if man == nil {
		return nil, fmt.Errorf("archive has no %s", manifestName)
	}
	if man.FormatVersion != backupFormatVersion {
		return nil, fmt.Errorf("archive format version %d, this build understands %d",
			man.FormatVersion, backupFormatVersion)
	}
	for _, f := range man.Files {
		got, ok := digests[f.Path]
		if !ok {
			return nil, fmt.Errorf("manifest lists %s but archive does not contain it", f.Path)
		}
		if got != f.SHA256 {
			return nil, fmt.Errorf("checksum mismatch for %s", f.Path)
		}
	}
	return man, nil
}

// Extract decrypts an archive and writes its contents under destDir.
func Extract(key, sealed []byte, destDir string) (*Manifest, error) {
	return extract(key, sealed, destDir, os.WriteFile)
}

// ExtractNewFiles decrypts an archive and requires each member to create a new
// file. This rejects archive paths that alias on the extraction filesystem.
func ExtractNewFiles(key, sealed []byte, destDir string) (*Manifest, error) {
	return extract(key, sealed, destDir, writeNewArchiveFile)
}

func extract(key, sealed []byte, destDir string, writeFile func(string, []byte, os.FileMode) error) (*Manifest, error) {
	if _, err := Verify(key, sealed); err != nil {
		return nil, err
	}
	plain, err := Open(key, sealed)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(strings.NewReader(string(plain)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }() // read-only reader; nothing to lose on close error

	tr := tar.NewReader(gz)
	var man *Manifest
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		content, err := readArchiveMember(tr, hdr.Name)
		if err != nil {
			return nil, err
		}
		// Reject path traversal before touching the filesystem.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("refusing unsafe archive path %q", hdr.Name)
		}
		out := filepath.Join(destDir, clean)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		if err := writeFile(out, content, os.FileMode(hdr.Mode)&restoreFileMode); err != nil { // #nosec G115 -- only the low permission bits survive the restoreFileMode mask, so truncating the tar mode cannot widen it
			return nil, err
		}
		if hdr.Name == manifestName {
			man = &Manifest{}
			if err := json.Unmarshal(content, man); err != nil {
				return nil, err
			}
		}
	}
	return man, nil
}

func writeNewArchiveFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
