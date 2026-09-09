package hub

import (
	"encoding/base64"
)

var (
	ssoB64        = base64.RawURLEncoding
	k8sTokenPath  = serviceAccountDir + "/token"
	k8sCACertPath = serviceAccountDir + "/ca.crt"
)
