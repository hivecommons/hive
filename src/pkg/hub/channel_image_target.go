package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	channelImageWalkbackDepth = 50
	channelImageNegativeTTL   = 3 * time.Minute
)

type spokeImageTagAvailability struct {
	exists bool
	at     time.Time
}

var (
	spokeImageTagAvailabilityMu    sync.Mutex
	spokeImageTagAvailabilityCache = map[string]spokeImageTagAvailability{}
)

var listChannelBranchCommits = func(branch string, logger *slog.Logger) []branchSHAInfo {
	client := &http.Client{Timeout: commitBehindCompareTimeout}
	return listRecentBranchCommits(client, branch, channelImageWalkbackDepth, logger)
}

var channelSpokeImageTagExists = cachedSpokeImageTagExists

func cachedSpokeImageTagExists(tag string, logger *slog.Logger) (exists bool, verified bool) {
	tag = shortSHA(tag)
	if tag == "" {
		return false, false
	}
	now := time.Now()
	spokeImageTagAvailabilityMu.Lock()
	if v, ok := spokeImageTagAvailabilityCache[tag]; ok && (v.exists || now.Sub(v.at) < channelImageNegativeTTL) {
		spokeImageTagAvailabilityMu.Unlock()
		return v.exists, true
	}
	spokeImageTagAvailabilityMu.Unlock()

	exists, verified = probeSpokeImageTag(tag, logger)
	if verified {
		spokeImageTagAvailabilityMu.Lock()
		spokeImageTagAvailabilityCache[tag] = spokeImageTagAvailability{exists: exists, at: now}
		spokeImageTagAvailabilityMu.Unlock()
	}
	return exists, verified
}

func probeSpokeImageTag(tag string, logger *slog.Logger) (exists bool, verified bool) {
	client := &http.Client{Timeout: channelResolveTimeout}
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + ghcrRepoSpoke + ":pull")
	if err != nil {
		if logger != nil {
			logger.Warn("channel image target: GHCR token request failed", "repo", ghcrRepoSpoke, "tag", tag, "error", err)
		}
		return false, false
	}
	defer func() { _ = tokenResp.Body.Close() }()
	if tokenResp.StatusCode == http.StatusTooManyRequests {
		if logger != nil {
			logger.Warn("channel image target: GHCR token request rate-limited", "repo", ghcrRepoSpoke, "tag", tag)
		}
		return false, false
	}
	if tokenResp.StatusCode != http.StatusOK {
		if logger != nil {
			logger.Warn("channel image target: GHCR token request returned non-OK",
				"repo", ghcrRepoSpoke, "tag", tag, "status", tokenResp.StatusCode)
		}
		return false, false
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		if logger != nil {
			logger.Warn("channel image target: GHCR token response was not decodable",
				"repo", ghcrRepoSpoke, "tag", tag, "error", err)
		}
		return false, false
	}

	req, err := http.NewRequest(http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, ghcrRepoSpoke, tag), nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		if logger != nil {
			logger.Warn("channel image target: GHCR manifest HEAD failed", "repo", ghcrRepoSpoke, "tag", tag, "error", err)
		}
		return false, false
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusNotFound:
		return false, true
	case http.StatusTooManyRequests:
		if logger != nil {
			logger.Warn("channel image target: GHCR manifest HEAD rate-limited", "repo", ghcrRepoSpoke, "tag", tag)
		}
		return false, false
	default:
		if logger != nil {
			logger.Warn("channel image target: GHCR manifest HEAD returned non-OK",
				"repo", ghcrRepoSpoke, "tag", tag, "status", resp.StatusCode)
		}
		return false, false
	}
}

func channelPublishedImageTarget(branch, channel, floatingSHA string, logger *slog.Logger) behindTarget {
	target := behindTarget{SHA: shortSHA(floatingSHA), Ref: ":" + channel, Channel: true, FloatingSHA: shortSHA(floatingSHA)}
	if branch == "" || target.FloatingSHA == "" {
		return target
	}
	commits := listChannelBranchCommits(branch, logger)
	if commits == nil {
		target.VerificationUnavailable = true
		return target
	}
	var best branchSHAInfo
	bestPending := 0
	for i, c := range commits {
		sha := shortSHA(c.SHA)
		if sha == "" {
			continue
		}
		exists, verified := channelSpokeImageTagExists(sha, logger)
		if !verified {
			target.VerificationUnavailable = true
			return target
		}
		if exists && best.SHA == "" {
			best = branchSHAInfo{SHA: sha, Message: c.Message}
			bestPending = i
		}
		if sameCommit(sha, target.FloatingSHA) {
			if best.SHA != "" {
				target.SHA = best.SHA
				target.PendingImageCommits = bestPending
				return target
			}
			target.PendingImageCommits = i
			return target
		}
	}
	target.VerificationUnavailable = true
	return target
}
