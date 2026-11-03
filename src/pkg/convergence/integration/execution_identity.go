package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const executionIdentityMaxBytes = 4096

type executionPrivateIdentity struct {
	Version int    `json:"version"`
	Key     []byte `json:"key"`
}

// ExecutionIdentity is installation-local, not a configurable hive name.
// Cloning this private state/credential is not a supported HA/failover model.
type ExecutionIdentity struct {
	ID        string
	PublicKey []byte
}

func OpenExecutionIdentity(path string) (ExecutionIdentity, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return ExecutionIdentity{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		_, key, generationErr := ed25519.GenerateKey(rand.Reader)
		if generationErr != nil {
			_ = file.Close()
			return ExecutionIdentity{}, generationErr
		}
		body, encodeErr := json.Marshal(executionPrivateIdentity{Version: ExecutionSchemaVersion, Key: key})
		if encodeErr == nil {
			_, encodeErr = file.Write(append(body, '\n'))
		}
		if encodeErr == nil {
			encodeErr = file.Sync()
		}
		closeErr := file.Close()
		if encodeErr != nil {
			return ExecutionIdentity{}, encodeErr
		}
		if closeErr != nil {
			return ExecutionIdentity{}, closeErr
		}
		directory, openErr := os.Open(filepath.Dir(path))
		if openErr != nil {
			return ExecutionIdentity{}, openErr
		}
		syncErr := directory.Sync()
		_ = directory.Close()
		if syncErr != nil {
			return ExecutionIdentity{}, syncErr
		}
	} else if !os.IsExist(err) {
		return ExecutionIdentity{}, err
	}
	stat, err := os.Lstat(path)
	if err != nil {
		return ExecutionIdentity{}, err
	}
	if stat.Mode().Perm()&0077 != 0 || !stat.Mode().IsRegular() || stat.Size() > executionIdentityMaxBytes {
		return ExecutionIdentity{}, fmt.Errorf("execution identity requires a private bounded regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ExecutionIdentity{}, err
	}
	var identity executionPrivateIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		return ExecutionIdentity{}, err
	}
	if identity.Version != ExecutionSchemaVersion || len(identity.Key) != ed25519.PrivateKeySize {
		return ExecutionIdentity{}, fmt.Errorf("invalid execution identity; do not overwrite or automatically steal ownership")
	}
	key := ed25519.PrivateKey(identity.Key)
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return ExecutionIdentity{}, fmt.Errorf("execution identity public key unavailable")
	}
	sum := sha256.Sum256(public)
	return ExecutionIdentity{ID: hex.EncodeToString(sum[:]), PublicKey: append([]byte(nil), public...)}, nil
}
