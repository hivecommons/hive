# ADR-0016: Scope `script-src` as two directives, close the element half with hashes

Status: Accepted

## Context

The dashboard's Content-Security-Policy carried one blanket `script-src 'self'
'unsafe-inline'`, on both CSP emitters — the Go spoke server
([securityHeaders](../../pkg/dashboard/server.go)) and the Node proxy
([proxy/server.js](../../proxy/server.js)). That directive was the residual of
[kubestellar/hive#3315](https://github.com/kubestellar/hive/issues/3315): the
token-injection half of that finding was fixed by #3844, but any XSS in the
dashboard origin could still execute an injected inline `<script>` and, for
example, read the pasted `HIVE_DASHBOARD_TOKEN` out of `localStorage`.
[#3848](https://github.com/kubestellar/hive/issues/3848) part 1 (and its
duplicate, [#3907](https://github.com/kubestellar/hive/issues/3907)) track
closing it.

[ADR-0015](0015-csp-style-src-scope.md) established the decomposition for
`style-src`, and the identical asymmetry decides `script-src`:

| Inline script form | Count | Covered by a hash? |
| --- | --- | --- |
| `on*="…"` handler attributes (`static/index.html`) | 426 | **No** |
| `on*=` handlers built as strings, injected via 169 `innerHTML` sites | ~145 | **No** |
| Inline `<script>` elements across every served document | 9 | Yes |

CSP hashes and nonces apply to *elements*, never to event-handler attributes
(`script-src-attr` accepts only `'none'`, `'unsafe-inline'`, or
`'unsafe-hashes'` plus per-attribute-value hashes — rejected here for the same
maintainability reasons ADR-0015 rejected it for styles). So the element half
is closable today and the attribute half is not: it requires an
event-delegation refactor of the 21k-line SPA, which remains the open scope of
#3848.

## Decision

`script-src` becomes three directives, on both emitters:

```text
script-src      'self' 'unsafe-inline'      ← CSP2 fallback, UNCHANGED
script-src-elem 'self' 'sha256-…' …         ← inline <script> elements: CLOSED
script-src-attr 'unsafe-inline'             ← on*= attributes: STAGED (#3848)
```

- **`script-src-elem` carries a sha256 hash for every inline `<script>` this
  server actually serves, and no `'unsafe-inline'`.** In every CSP3 browser an
  injected inline `<script>` matches no hash and does not execute.
- **Hashes, not nonces — deliberately.** The SPA document is pre-gzipped once
  at startup with a strong ETag (#3863). A per-response nonce requires
  rewriting the document per request, which forfeits both the 4× transfer win
  and the 304 revalidation path. A hash is a pure function of the bytes already
  being served: for the byte-stable documents (the embedded SPA, the
  device-flow login page) the allowlist is computed once at startup; for
  documents whose script content varies per response (`/contribute`, whose
  `hubURL` derives from the Host header; `/snapshot`, built at runtime) the
  handler stamps the allowlist from the finished document before the first
  write (`applyDocumentScriptSrcElem`).
- **The CSP2 fallback `script-src` keeps `'unsafe-inline'` and must never
  carry the hashes.** Per CSP2, the presence of a hash source makes a browser
  ignore `'unsafe-inline'` in the same directive — so a browser that
  understands hashes but not `script-src-elem`/`-attr` (Firefox < 108) would
  block all 426+ inline handlers and blank the dashboard. Keeping the fallback
  hash-free means pre-CSP3 browsers enforce exactly what they enforced before
  this change: no regression, no new protection.
- **`script-src-attr 'unsafe-inline'` is STAGED, not accepted.** Unlike
  `style-src-attr` (a permanent acceptance, ADR-0015), the handler attributes
  *can* be eliminated by refactoring the SPA to delegated `addEventListener`
  wiring, and #3848 remains open for exactly that. The tripwire test
  `TestCSPScriptSrcAttrUnsafeInlineIsStaged` fails the moment that refactor
  lands, and must then be inverted — never relaxed.
- Documents whose bytes this code never renders keep the blanket CSP2 policy:
  `/terminal` (ttyd's own UI, streamed through a reverse proxy) on both
  emitters, and on the Node proxy additionally the Go-rendered `/contribute`,
  `/leaderboard` and `/snapshot` documents, whose authoritative per-document
  policy is stamped by the Go upstream.

## Residual risk

What this ADR accepts, stated plainly:

- **Injected `on*=` handler attributes still execute in every browser** while
  `script-src-attr 'unsafe-inline'` stands — markup injection such as
  `<img src=x onerror=…>` remains an executable XSS primitive. Closing it is
  the remaining scope of #3848.
- **Pre-CSP3 browsers (e.g. Firefox < 108) gain nothing**: they enforce only
  the unchanged fallback.
- The dashboard credential still lives in `localStorage` (operator-pasted);
  moving it out is tracked separately in #3315's recommendation trail.
- The hub SaaS SPA ([pkg/hub/saas.go](../../pkg/hub/saas.go)) serves no CSP
  header at all today; it is outside both emitters this ADR covers.

## Consequences

- An XSS that injects an inline `<script>` element is neutralized in all
  current browsers — the highest-value primitive is closed on both emitters.
- Every handler that renders a NEW inline `<script>` into a served document
  must either be byte-stable (then its document belongs in the startup set) or
  call `applyDocumentScriptSrcElem` with the finished document; the
  hash-coverage tests in `csp_script_src_test.go` fail otherwise.
- When #3848's event-delegation refactor lands, `script-src-attr` drops
  `'unsafe-inline'`, the fallback `script-src` can finally drop it too, and
  the tripwire is inverted to pin the closed state.
