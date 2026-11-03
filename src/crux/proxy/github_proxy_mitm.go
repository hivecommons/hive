package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// proxyHTTP relays a MITM'd GitHub connection with ACMM enforcement applied.
// It is proxyHTTPHost with the GitHub host implied, which is what every
// pre-existing caller means.
func (p *GitHubProxy) proxyHTTP(client net.Conn, upstream net.Conn, agentName string, mode agent.AgentMode, caps agent.AgentCapabilities) {
	p.proxyHTTPHost(client, upstream, "", agentName, mode, caps)
}

// proxyHTTPHost is proxyHTTP with the terminated host carried through, so the
// request loop can pick the right rule engine (#4492 F).
//
// The host is threaded rather than inferred from the request because a MITM'd
// request line is a path, not an absolute URL — by this point the only record
// of which host the TLS session was forged for is the CONNECT that opened it.
// An empty host means GitHub, preserving every existing call site verbatim.
func (p *GitHubProxy) proxyHTTPHost(client net.Conn, upstream net.Conn, host string, agentName string, mode agent.AgentMode, caps agent.AgentCapabilities) {
	isLinear := IsLinearHost(host)
	clientBuf := newBufferedConn(client)

	for {
		// Bound every phase of this request/response exchange. Before this,
		// the relay had NO deadlines anywhere after the initial CONNECT read:
		// a client or upstream that stopped mid-exchange (stalled response,
		// half-written body, frozen keep-alive) blocked forever — the
		// multi-hour "egress wedge" where the agent's request never reached
		// GitHub and its session froze. Each phase gets its own deadline;
		// they are cleared once the phase completes so healthy keep-alive
		// connections are unaffected.
		_ = client.SetReadDeadline(time.Now().Add(httpReadTimeout))
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			_ = client.SetReadDeadline(time.Time{})
			p.logClientRead("proxy client request read timed out", err, "agent", agentName)
			return
		}

		blocked := false
		blockReason := ""
		// blockCode is the machine-readable reason (#12127) that rides on the
		// one-line 403 body and the X-Hive-Proxy-Reason header. Each gate that
		// sets blockReason sets it; a gate that sets neither falls back to a
		// generic code in ProxyDenialBody.
		blockCode := ""

		// Per-repo pause (#6203) and per-repo agent scope (#6204) are both
		// checked BEFORE the mode chain, and outside it, for the same two
		// reasons. Neither is an autonomy tier — a pause is a run-state, a
		// scope is roster membership — so each must refuse the write whatever
		// mode the agent holds and whichever branch below would otherwise have
		// allowed it. And each carries its own reason, so the agent is told the
		// repo is deliberately quiet, or that it is not defined for this repo,
		// instead of reading a mode error and going hunting for a permissions
		// bug that does not exist.
		//
		// The hive's own control-plane traffic (App-token mint, heartbeat) is
		// exempt from both, exactly as it is from the ACMM rules below:
		// stopping the hive from minting a token would take the whole spoke
		// down rather than quiet one repo or narrow one agent.
		//
		// Pause is checked first, so a repo the operator has frozen is refused
		// as paused even when the agent is also out of scope for it.
		blockJSON := false
		graphQLMutation := false
		// A direct write from a lane under write_surface.enforce (#9772) is
		// refused and audited as agent_write_refused. Checked after pause and
		// scope so their more specific reasons win when both apply.
		writeSurfaceKind := ""
		if agentName != internalCallerName {
			if reason, paused := p.repoPauseRefusal(req.Method, repoScopePath(req)); paused {
				blocked = true
				blockReason = reason
				blockCode = DenyReasonRepoPaused
			} else if reason, refused := p.agentRepoScopeRefusal(agentName, req.Method, repoScopePath(req)); refused {
				blocked = true
				blockReason = reason
				blockCode = DenyReasonAgentRepoScope
			} else if reason, refused := p.autoMergeRefusal(req.Method, repoScopePath(req)); refused {
				blocked = true
				blockReason = reason
				blockCode = DenyReasonRepoAutoMergeOff
				blockJSON = true
			} else if !isLinear {
				if reason, kind, refused := WriteSurfaceEnforceRefusal(p.writeSurfaceEnforcedFunc(), agentName, req.Method, req.URL.Path, req.URL.RawQuery); refused {
					blocked = true
					blockReason = reason
					blockCode = DenyReasonWriteSurfaceEnforced
					writeSurfaceKind = kind
				}
			}
		}

		if blocked {
			// Already refused by the pause or agent-scope check above — the
			// mode chain cannot un-block it, so skip it entirely.
		} else if isLinear && agentName == internalCallerName {
			// The hive's OWN Linear traffic — the read path #4178 shipped
			// (backlog enumeration in pkg/worksource/linear.go) and, later,
			// the OAuth/webhook plumbing. ACMM governs AGENT autonomy, so the
			// control plane is exempt here exactly as it is for GitHub below.
			// This branch must precede the enforcement one: without it, adding
			// Linear to NeedsMITM would newly gate hive-originated reads that
			// have always been ungated, breaking enumeration rather than
			// restricting an agent.
		} else if isLinear {
			// Linear is a single POST /graphql for everything, so the
			// (method, path) rule table cannot classify it and the GitHub
			// GraphQL classifier is the wrong posture — it ends in a
			// permissive default, backed by the REST table. Linear has no
			// table underneath. See linear_rules.go.
			blocked, blockReason = p.enforceLinear(req, agentName, mode, caps)
			if blocked {
				blockCode = DenyReasonLinear
			}
		} else if req.Method == "POST" && IsGraphQLPath(req.URL.Path) {
			body, readErr := io.ReadAll(io.LimitReader(req.Body, graphQLBodyLimit))
			if req.Body != nil {
				_ = req.Body.Close()
			}
			if readErr != nil {
				_ = client.SetReadDeadline(time.Time{})
				p.logTimeout("proxy GraphQL request body read timed out", readErr, "agent", agentName, "path", req.URL.Path)
				return
			}
			if reason, refused := p.graphQLAutoMergeRefusal(body); agentName != internalCallerName && refused {
				blocked = true
				blockReason = reason
				blockCode = DenyReasonRepoAutoMergeOff
				blockJSON = true
			} else if agentName != internalCallerName {
				if reason, refused := WriteSurfaceGraphQLRefusal(p.writeSurfaceEnforcedFunc(), agentName, body); refused {
					blocked = true
					blockReason = reason
					blockCode = DenyReasonWriteSurfaceEnforced
					writeSurfaceKind = WriteSurfaceKindGraphQL
				}
			}
			allowed, isMutation := GraphQLAllowedCaps(autonomyGraphQLMode(agentName, body, mode), caps, body)
			graphQLMutation = isMutation
			if !blocked && !allowed {
				blocked = true
				if isMutation {
					blockReason = "graphql mutation"
					blockCode = DenyReasonGraphQLMutation
				} else {
					blockReason = "graphql"
					blockCode = DenyReasonGraphQL
				}
			}
			req.Body = io.NopCloser(strings.NewReader(string(body)))
			req.ContentLength = int64(len(body))
		} else if agentName == internalCallerName {
			// The hive itself has no ACMM mode to enforce — ACMM governs AGENT
			// autonomy. Its control-plane calls (App token mint, heartbeat)
			// must not be gated as if they were agent writes. Repo-filter and
			// canary-egress checks below still apply.
		} else if !AllowedByModeCaps(autonomyModeForRepo(agentName, ExtractRepo(repoScopePath(req)), mode), caps, req.Method, req.URL.Path) {
			blocked = true
			blockCode = DenyReasonMode
			// An unidentified agent is silently treated as ADVISORY, which turns
			// a permissions bug into an indistinguishable "policy denial". Say so
			// loudly on writes: a write from an unknown caller means UID
			// attribution failed, not that the agent lacks the mode.
			if agentName == "" && writeMethods[req.Method] {
				p.logger.Warn("proxy: agent could not be identified — defaulting to ADVISORY and blocking a write; UID attribution failed (check the uid map and /proc/net/tcp visibility), this is NOT an agent-mode misconfiguration",
					"method", req.Method, "path", req.URL.Path)
			}
			// A hard-deny rule (e.g. direct PR creation) carries an agent-facing
			// directive explaining the sanctioned alternative — surface it so the
			// agent knows to use hive-open-pr rather than seeing only a mode error.
			if msg, denied := DeniedMessage(req.Method, req.URL.Path); denied && msg != "" {
				blockReason = msg
				blockCode = DenyReasonHardDeny
			}
		} else if len(p.allowedRepos) > 0 && !RepoFilterAllowed(p.allowedRepos, req.Method, repoScopePath(req)) {
			blocked = true
			blockReason = "repo not in hive config"
			blockCode = DenyReasonRepoNotInConfig
		}
		// Shape-gate agent-authored issue/comment REST writes. The issue-request
		// watcher validates the file-drop creation path, but an agent's
		// `gh issue create` becomes a raw POST /repos/{o}/{r}/issues that never
		// touches the watcher (issue #7014: a template placeholder title and a
		// body of literal \n escapes both reached GitHub this way). Enforce the
		// same shape rules here so both creation paths are covered. Skip the
		// control plane — its issue writes are already validated upstream.
		if !blocked && agentName != internalCallerName {
			if reason, deny, shapeErr := p.enforceIssueShapeFor(req, agentName); shapeErr != nil {
				// Fail closed: a body we could not read is a body we could not
				// shape-check.
				p.logTimeout("proxy issue-shape body read timed out", shapeErr, "agent", agentName, "path", req.URL.Path)
				_ = client.SetReadDeadline(time.Time{})
				return
			} else if deny {
				blocked = true
				if blockReason == "" {
					blockReason = reason
					blockCode = DenyReasonIssueShape
				}
			}
		}
		if reason, deny, ok := p.inspectCanaryEgress(agentName, req); ok {
			if deny {
				blocked = true
				if blockReason == "" {
					blockReason = reason
					blockCode = DenyReasonCanaryEgress
				}
			}
		}

		if !isLinear && agentName != internalCallerName {
			isGraphQL := req.Method == "POST" && IsGraphQLPath(req.URL.Path)
			isWrite := writeMethods[req.Method] && !gitUploadPackPath.MatchString(req.URL.Path)
			if isGraphQL {
				isWrite = graphQLMutation
			}
			if isWrite {
				reason := ""
				if blocked {
					reason = blockCode
					if reason == "" {
						reason = DenyReasonUnknown
					}
				}
				p.writes.record(mode.String(), reason)
			}
		}

		if blocked {
			detail := req.URL.Path
			if blockReason != "" {
				detail = blockReason
			}
			p.recordViolation(agentName, req.Method, detail)
			if writeSurfaceKind != "" {
				p.auditWriteRefused(agentName, writeSurfaceKind, req.Method, repoScopePath(req))
			}

			if blockCode == "" {
				blockCode = DenyReasonUnknown
			}
			respBody := ProxyDenialBody(agentName, mode, req.Method, detail, blockCode)
			contentType := "text/plain"
			if blockJSON {
				contentType = "application/json"
				payload, _ := json.Marshal(map[string]any{
					"error":  "repo_auto_merge_disabled",
					"reason": detail,
				})
				respBody = string(payload) + "\n"
			}
			resp := &http.Response{
				StatusCode: http.StatusForbidden,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}
			resp.Header.Set("Content-Type", contentType)
			resp.Header.Set("X-Hive-Proxy-Blocked", "true")
			resp.Header.Set(ProxyDenialHeader, blockCode)
			_ = client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
			writeErr := resp.Write(client)
			_ = client.SetWriteDeadline(time.Time{})
			if writeErr != nil {
				p.logTimeout("proxy blocked response write timed out", writeErr, "agent", agentName)
				return
			}

			if req.Body != nil {
				if _, drainErr := io.Copy(io.Discard, req.Body); drainErr != nil {
					p.logTimeout("proxy blocked request body drain timed out", drainErr, "agent", agentName, "path", req.URL.Path)
				}
				_ = req.Body.Close()
			}
			_ = client.SetReadDeadline(time.Time{})
			continue
		}

		if !isLinear {
			if refusal := p.githubBudgetRefusal(agentName, req.Method, req.URL.Path, agentGitHubRequestIsRead(req.Method, req.URL.Path, graphQLMutation)); refusal != nil {
				_ = p.writeAgentGitHubBudget429(client, *refusal)
				if req.Body != nil {
					_, _ = io.Copy(io.Discard, req.Body)
					_ = req.Body.Close()
				}
				_ = client.SetReadDeadline(time.Time{})
				continue
			}
		}

		// #1861: with injection enabled, the upstream credential is decided
		// HERE — after the ACMM/repo/canary gates, immediately before either
		// forwarding branch — so every byte that reaches GitHub has passed
		// through it. Blocked requests above never reach upstream and need no
		// rewrite.
		//
		// #9586: only for GitHub hosts. This loop also relays api.linear.app
		// (NeedsInspection), and rewriting there would attach the agent's
		// hub-held GITHUB token to a request bound for Linear - a real
		// credential handed to a third party - whenever the Linear lane below
		// does not overwrite the header (an ADVISORY agent, or a hive with no
		// Linear credential).
		if injectsGitHubAuthForHost(host) {
			p.rewriteGitHubAuth(req, agentName)
		}

		// Linear: the request passed the tier gate; make sure it reaches
		// Linear with the hive's CURRENT credential rather than whatever
		// (possibly rotated-out) copy the agent's process still holds.
		if isLinear && agentName != internalCallerName {
			p.injectLinearCredential(req, agentName, mode)
		}
		// Git smart HTTP uses chunked streaming that http.ReadResponse
		// can't handle reliably. After the ACMM check passes, forward
		// the request and switch to raw bidirectional streaming.
		if isGitPath(req.URL.Path) {
			_ = upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
			if err := req.Write(upstream); err != nil {
				_ = upstream.SetWriteDeadline(time.Time{})
				_ = client.SetReadDeadline(time.Time{})
				p.logTimeout("proxy git request relay timed out", err, "agent", agentName, "path", req.URL.Path)
				return
			}
			_ = upstream.SetWriteDeadline(time.Time{})
			_ = client.SetReadDeadline(time.Time{})
			// Use relayTunnel, NOT an inline copy pair: this branch used to run
			// its own unbounded io.Copy pair, which was the same wedge #3872
			// fixed in relayTunnel but left here. When GitHub FIN'd after the
			// response while the client kept its side open (keep-alive), the
			// client→upstream copy stayed blocked in client.Read() forever and
			// `<-done` never returned — the handler's deferred Closes never ran,
			// parking the upstream socket in CLOSE_WAIT for the life of the
			// process (issue #3875's FD pile). relayTunnel bounds each direction
			// once the other finishes.
			relayTunnel(client, upstream)
			return
		}

		// Forward to upstream. The client read deadline stays in force until
		// the body is fully drained (req.Write pipes req.Body from the client),
		// so a client stalling mid-body cannot hang the relay.
		_ = upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
		if err := req.Write(upstream); err != nil {
			_ = upstream.SetWriteDeadline(time.Time{})
			_ = client.SetReadDeadline(time.Time{})
			p.logTimeout("proxy request relay timed out", err, "agent", agentName, "path", req.URL.Path)
			return
		}
		_ = upstream.SetWriteDeadline(time.Time{})
		_ = client.SetReadDeadline(time.Time{})

		_ = upstream.SetReadDeadline(time.Now().Add(httpReadTimeout))
		resp, err := http.ReadResponse(newBufferedReader(upstream), req)
		_ = upstream.SetReadDeadline(time.Time{})
		if err != nil {
			p.logTimeout("proxy upstream response read timed out", err, "agent", agentName, "path", req.URL.Path)
			return
		}
		p.recordGitHubREST(agentName, req, resp)
		p.recordAgentGitHubBudget(agentName, req.URL.Path, resp.StatusCode, p.agentGitHubCap(agentName))

		// Bound the BODY relay too, not just the header read above. The
		// deadline was cleared once headers arrived, so a "reachable but slow"
		// GitHub — TLS and headers complete, body never arrives (the measured
		// #3875 signature: time_total=12s, 0 body bytes) — parked resp.Write
		// in upstream.Read() with no deadline. The client had long since given
		// up (10s collect budget) and FIN'd its side, but this goroutine never
		// noticed: it holds all three sockets of the exchange forever, and the
		// retry loop stacks a fresh parked handler on every attempt (~1k
		// FDs/min measured live). The wrapper re-arms a rolling per-Read
		// deadline, so a body that keeps flowing is never cut while a stall
		// longer than responseBodyStallTimeout errors out and lets the
		// handler's deferred Closes reclaim the sockets.
		resp.Body = &stallBoundedBody{body: resp.Body, conn: upstream, idle: p.bodyStallTimeoutDuration()}

		// Rolling per-Write deadline (see stallBoundedWriter): a response that
		// keeps flowing is never cut for being long, while a client that stops
		// draining still releases this handler within one stall window.
		if err := resp.Write(&stallBoundedWriter{conn: client, idle: httpWriteTimeout}); err != nil {
			_ = resp.Body.Close()
			_ = client.SetWriteDeadline(time.Time{})
			p.logTimeout("proxy response relay timed out", err, "agent", agentName, "path", req.URL.Path)
			return
		}
		_ = client.SetWriteDeadline(time.Time{})
		_ = resp.Body.Close()
	}
}
