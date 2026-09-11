package hub

import (
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/imageref"
)

const (
	ssoClockSkew = 30 * time.Second

	// infoTerminalKey is the domain-separation label for the terminal signing
	// sub-key. Provisioning (provisionTerminalKey) MUST use the SAME label the
	// spoke's self-derive lane uses, or a hub-provisioned key and a
	// self-derived one stop agreeing and terminal assertions fail to verify.
	// This aliases the spoke constant rather than repeating its value so the
	// two cannot drift: copying the literal here is exactly what #6719
	// reported, and a copy keeps the invariant true only by coincidence.
	infoTerminalKey = spoke.InfoTerminalKey

	// EnvTerminalKey is the env var carrying the per-hive terminal signing key.
	// Aliased from the spoke side for the same reason: the hub writes this
	// variable and the spoke reads it, so a divergent spelling would silently
	// deliver the key to a name nothing reads.
	EnvTerminalKey = spoke.EnvTerminalKey

	mutableTagSuffix = "-latest"
)

func imageTagIsMutable(image string) bool {
	return imageref.IsMutable(image)
}
