package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

var standaloneChannelRevision = cachedStandaloneChannelRevision
var standaloneRevisionCache = struct {
	sync.Mutex
	entries map[string]struct {
		sha string
		at  time.Time
	}
}{entries: make(map[string]struct {
	sha string
	at  time.Time
})}

// isStandaloneReleaseChannel mirrors imageref.IsReleaseChannel; pkg/dashboard
// keeps its internal-import count ratcheted down, so the three tags are not
// imported from there.
func isStandaloneReleaseChannel(tag string) bool {
	return tag == "stable" || tag == "candidate" || tag == "edge"
}

// standaloneReleaseChannel returns the release channel named by a tag-form
// image ref, or "" for digest refs, pins and branch tags.
func standaloneReleaseChannel(ref string) string {
	if strings.Contains(ref, "@") {
		return ""
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ""
	}
	if tag := ref[i+1:]; isStandaloneReleaseChannel(tag) {
		return tag
	}
	return ""
}

func resolveStandaloneChannelTarget(ref string) upgradeTarget {
	channel := standaloneReleaseChannel(ref)
	t := upgradeTarget{Source: "channel", Channel: channel, Ref: ref}
	t.SHA = standaloneChannelRevision(channel)
	t.Short = shortSHADashboard(t.SHA)
	t.Resolved = t.SHA != ""
	return t
}

func cachedStandaloneChannelRevision(channel string) string {
	standaloneRevisionCache.Lock()
	prev := standaloneRevisionCache.entries[channel]
	standaloneRevisionCache.Unlock()
	if time.Since(prev.at) < ghcrCacheTTL {
		return prev.sha
	}
	sha := standaloneChannelRevisionWithClient(ghcrCheckClient, ghcrCheckBaseURL, channel)
	// Do not serve stale targets after a failed registry lookup.
	standaloneRevisionCache.Lock()
	standaloneRevisionCache.entries[channel] = struct {
		sha string
		at  time.Time
	}{sha, time.Now()}
	standaloneRevisionCache.Unlock()
	return sha
}

// Read the actual channel image's OCI revision, never a git branch head. The
// multi-platform index may include attestations; only the running platform's
// image manifest has the config whose labels describe what will be installed.
func standaloneChannelRevisionWithClient(client *http.Client, baseURL, channel string) string {
	if !isStandaloneReleaseChannel(channel) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*ghcrCheckTimeout)
	defer cancel()
	get := func(path, token string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer closeHTTPBody(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("registry HTTP %d", resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
	}
	var token struct {
		Token string `json:"token"`
	}
	if get("/token?scope=repository:hivecommons/hive:pull", "", &token) != nil {
		return ""
	}
	type manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	var m manifest
	if get("/v2/hivecommons/hive/manifests/"+channel, token.Token, &m) != nil {
		return ""
	}
	if len(m.Manifests) > 0 {
		digest := ""
		for _, d := range m.Manifests {
			if d.Platform.OS == "linux" && d.Platform.Architecture == runtime.GOARCH {
				digest = d.Digest
				break
			}
		}
		if digest == "" {
			return ""
		}
		m = manifest{}
		if get("/v2/hivecommons/hive/manifests/"+digest, token.Token, &m) != nil {
			return ""
		}
	}
	if m.Config.Digest == "" {
		return ""
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if get("/v2/hivecommons/hive/blobs/"+m.Config.Digest, token.Token, &cfg) != nil {
		return ""
	}
	sha := cfg.Config.Labels["org.opencontainers.image.revision"]
	if !standaloneUpgradeTargetRE.MatchString(sha) {
		return ""
	}
	return sha
}
