package crux

// CanaryLeak is one detection of a planted canary token in text an agent is
// about to send off-host: which agent the token was planted for, the token
// itself and the surface it was seen on (for example "github:POST
// /repos/o/r/issues"). The registry that plants tokens and recognises their
// encodings is host-side (pkg/ioscan aliases this type); the proxy only
// carries the value to the host's leak handler (hivecommons/hive#13536).
type CanaryLeak struct {
	Agent  string
	Token  string
	Source string
}
