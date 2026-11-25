package forge

import (
	"context"
	"regexp"
	"strings"
)

// GiteaFamilyVersion returns the version string a Gitea-family instance
// reports at /api/v1/version. Gitea and Forgejo both serve that endpoint, so
// it is how a forge: gitea config can tell it is really pointed at Forgejo
// (see IsForgejoVersion). token and opts follow NewForge's Gitea rules: opts
// must carry the instance root, and token, when set, is sent as the
// Authorization header (an instance that requires sign-in for every API call
// rejects the request without it).
func GiteaFamilyVersion(ctx context.Context, token string, opts Options) (string, error) {
	f, err := newGiteaFamilyForge(KindGitea, token, opts)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := f.getJSON(ctx, "/version", nil, &v); err != nil {
		return "", err
	}
	return strings.TrimSpace(v.Version), nil
}

// legacyForgejoVersion matches the A.B.C+N form Forgejo 1.18.0-1 through
// 1.21.7-0 reported at /api/v1/version. Gitea never ends a version in "+N":
// a tagged release is bare A.B.C and a build past a tag is A.B.C+N-g<sha>.
var legacyForgejoVersion = regexp.MustCompile(`^\d+\.\d+\.\d+\+\d+$`)

// IsForgejoVersion reports whether version, as /api/v1/version returns it, is
// a Forgejo instance's. Forgejo 7.0.0 and later report X.Y.Z+gitea-A.B.C, the
// marker Forgejo documents for this check
// (https://forgejo.org/docs/latest/user/api/versions/); earlier Forgejo
// releases reported A.B.C+N.
func IsForgejoVersion(version string) bool {
	version = strings.TrimSpace(version)
	return strings.Contains(version, "+gitea-") || legacyForgejoVersion.MatchString(version)
}
