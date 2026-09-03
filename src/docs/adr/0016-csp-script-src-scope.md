# ADR-0016: Scope `script-src` as two directives, close the element half with hashes

Status: Accepted

## Context

The dashboard's Content-Security-Policy carried one blanket `script-src 'self'
'unsafe-inline'`, on both CSP emitters — the Go spoke server
([securityHeaders](../../pkg/dashboard/server.go)) and the Node proxy
([proxy/server.js](../../proxy/server.js)). That directive was the residual of
[kubestellar/hive#3315](https://github.com/hivecommons/hive/issues/3315): the
token-injection half of that finding was fixed by #3844, but any XSS in the
dashboard origin could still execute an injected inline `<script>` and, for
example, read the pasted `HIVE_DASHBOARD_TOKEN` out of `localStorage`.
[#3848](https://github.com/hivecommons/hive/issues/3848) part 1 (and its
duplicate, [#3907](https://github.com/hivecommons/hive/issues/3907)) track
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
script-src      'self'                      ← CSP2 fallback (unsafe-inline dropped with #3848)
script-src-elem 'self' 'sha256-…' …         ← inline <script> elements: CLOSED
script-src-attr 'none'                      ← on*= attributes: CLOSED (#3848 event delegation)
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
- **The CSP2 fallback `script-src` must never carry the hashes.** Per CSP2,
  the presence of a hash source makes a browser ignore `'unsafe-inline'` in
  the same directive, so the hashes belong in `script-src-elem` and only
  there. Since the #3848 event-delegation refactor removed every inline
  handler attribute, the fallback also dropped `'unsafe-inline'`: nothing
  inline-attribute-based remains for any browser to permit.
- **`script-src-attr` is now `'none'` — the attribute half is CLOSED.** The
  #3848 event-delegation refactor replaced every inline `on*=` handler
  attribute (in `static/index.html` and in Go-generated HTML) with
  `data-action` / `data-*` attributes dispatched by central document-level
  listeners, so an injected handler attribute never executes. Unlike
  `style-src-attr` (a permanent acceptance, ADR-0015), this half was
  eliminable and has been eliminated. The former staged tripwire
  `TestCSPScriptSrcAttrUnsafeInlineIsStaged` was inverted into
  `TestCSPScriptSrcAttrUnsafeInlineIsAbsent`, which pins the closed state.
- Documents whose bytes this code never renders keep the blanket CSP2 policy:
  `/terminal` (ttyd's own UI, streamed through a reverse proxy) on both
  emitters, and on the Node proxy additionally the Go-rendered `/contribute`,
  `/leaderboard` and `/snapshot` documents, whose authoritative per-document
  policy is stamped by the Go upstream.

## Residual risk

What this ADR accepts, stated plainly:

- **Injected `on*=` handler attributes no longer execute in CSP3 browsers**:
  `script-src-attr` is `'none'` after the #3848 event-delegation refactor.
  Pre-CSP3 browsers enforce the `script-src 'self'` fallback, which also
  blocks inline handlers.
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
- With #3848's event-delegation refactor landed, `script-src-attr` is
  `'none'`, the fallback `script-src` dropped `'unsafe-inline'`, and the
  tripwire is inverted (`TestCSPScriptSrcAttrUnsafeInlineIsAbsent`) to pin
  the closed state. New UI handlers must be wired through the `data-action`
  dispatcher, never as inline attributes.
