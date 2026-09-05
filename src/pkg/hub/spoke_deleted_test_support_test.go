package hub

import (
	"encoding/base64"
	"time"
)

const ssoTokenTTL = 90 * time.Second

var (
	ssoB64        = base64.RawURLEncoding
	k8sTokenPath  = serviceAccountDir + "/token"
	k8sCACertPath = serviceAccountDir + "/ca.crt"
)
