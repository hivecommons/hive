package hub

import (
	"strings"
	"time"
)

const (
	ssoClockSkew     = 30 * time.Second
	infoTerminalKey  = "hive-terminal-v1"
	EnvTerminalKey   = "HIVE_TERMINAL_KEY"
	mutableTagSuffix = "-latest"
)

func imageTagIsMutable(image string) bool {
	if image == "" || strings.Contains(image, "@") {
		return false
	}
	idx := strings.LastIndex(image, ":")
	if idx < 0 || strings.Contains(image[idx+1:], "/") {
		return false
	}
	tag := image[idx+1:]
	if isReleaseChannel(tag) {
		return true
	}
	return strings.HasSuffix(tag, mutableTagSuffix)
}
