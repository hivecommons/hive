package policies

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// seedManifestFileName records, per seeded template, the sha256 pkg/policies
// itself wrote the file with at the last seed. It lives inside the seeded
// directory so it travels with the templates rather than needing a separate
// volume or config knob.
const seedManifestFileName = ".embedded-seed-manifest.json"

// sha256Hex is the content fingerprint used throughout this file: cheap,
// collision-safe enough for "did this change since we wrote it", and already
// hex so it drops straight into JSON and log lines.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// loadSeedManifest reads the manifest from dir. A missing file is not an
// error — the ordinary state before the first ReconcileSeededDefaults run —
// and returns an empty manifest.
func loadSeedManifest(dir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, seedManifestFileName))
	if err != nil {
		return map[string]string{}
	}
	manifest := map[string]string{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return map[string]string{}
	}
	return manifest
}

func saveSeedManifest(dir string, manifest map[string]string) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, seedManifestFileName), data, 0o644)
}

// ReconcileSeededDefaults writes every embedded default policy template
// (pkg/policies/defaults) into dir, and keeps them in sync with the running
// image on every later boot — WITHOUT clobbering a genuine user edit.
//
// The problem this solves (hivecommons/hive#9428): dir is also
// userSavedPolicyDir/clonedPoliciesDir in pkg/scheduler, which the kick
// template loader checks BEFORE the embedded default. A file that lands
// there any other way — an old init path, a bulk seed, a stale copy left
// behind by a prior image — is indistinguishable on disk from a real
// operator edit, so it wins forever and silently shadows every embedded
// policy update shipped afterwards (as happened to brainstorm-advisory.md
// before its own force-rewrite in cmd/hive/main.go, and to every OTHER
// template with no such special case).
//
// The fix generalizes that one special case using a small on-disk manifest
// (seedManifestFileName) recording the sha256 this function itself wrote for
// each template at the last seed:
//   - No file on disk yet → seed it and record the hash. (embedded-only case)
//   - File matches the current embedded default → nothing to do (just heal
//     the manifest entry if missing).
//   - File differs from the current embedded default AND matches the hash
//     THIS function recorded last time → nothing has touched it since our
//     last seed, so the embedded default moved on without it: it is a stale
//     seeded copy. Refresh it and update the manifest. (stale-seed case)
//   - File differs from the current embedded default AND from the last
//     recorded seed hash → something else wrote it since (the dashboard
//     prompt editor, an operator, or unknown legacy provenance). Leave it
//     alone and log it so an operator can see why the rendered prompt
//     differs from the repo default. (user-edit case)
//
// Errors reading or writing an individual template are logged and skipped;
// ReconcileSeededDefaults only returns an error when dir itself cannot be
// created or the embedded default set cannot be listed, since a partial seed
// is still better than none.
func ReconcileSeededDefaults(dir string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := DefaultPolicies.ReadDir("defaults")
	if err != nil {
		return err
	}

	manifest := loadSeedManifest(dir)
	manifestChanged := false

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		embedded, err := DefaultPolicies.ReadFile("defaults/" + name)
		if err != nil {
			logger.Warn("policies: failed to read embedded default", "name", name, "error", err)
			continue
		}
		embeddedHash := sha256Hex(embedded)
		target := filepath.Join(dir, name)

		existing, err := os.ReadFile(target)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if werr := os.WriteFile(target, embedded, 0o644); werr != nil {
				logger.Warn("policies: failed to seed default policy", "name", name, "path", target, "error", werr)
				continue
			}
			manifest[name] = embeddedHash
			manifestChanged = true
			logger.Info("policies: seeded default policy", "name", name, "path", target)
			continue
		case err != nil:
			logger.Warn("policies: failed to read seeded policy", "name", name, "path", target, "error", err)
			continue
		}

		existingHash := sha256Hex(existing)
		if existingHash == embeddedHash {
			if manifest[name] != embeddedHash {
				manifest[name] = embeddedHash
				manifestChanged = true
			}
			continue
		}

		recordedHash, known := manifest[name]
		if !known {
			logger.Warn("policies: seeded policy differs from the embedded default with no seed record; leaving it in place — resave it from the dashboard to take a new edit, or delete it to pick up the embedded default",
				"name", name, "path", target)
			continue
		}
		if recordedHash != existingHash {
			logger.Debug("policies: leaving edited policy in place", "name", name, "path", target)
			continue
		}

		// Unchanged since our last seed, but the embedded default moved on:
		// a stale seeded copy shadowing the update (hivecommons/hive#9428).
		if werr := os.WriteFile(target, embedded, 0o644); werr != nil {
			logger.Warn("policies: failed to refresh stale seeded policy", "name", name, "path", target, "error", werr)
			continue
		}
		manifest[name] = embeddedHash
		manifestChanged = true
		logger.Info("policies: refreshed stale seeded policy that shadowed an embedded default update", "name", name, "path", target)
	}

	if manifestChanged {
		if err := saveSeedManifest(dir, manifest); err != nil {
			logger.Warn("policies: failed to save seed manifest", "dir", dir, "error", err)
		}
	}
	return nil
}
