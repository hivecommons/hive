# ADR-0015: Scope `style-src` as two directives and accept inline style attributes

Status: Accepted

## Context

The dashboard's Content-Security-Policy carried one blanket `style-src 'self'
'unsafe-inline'` ([securityHeaders](../../pkg/dashboard/server.go)). That single
token covered two different things with two different futures, and hid the fact
that only one of them is closable.

[kubestellar/hive#3848](https://github.com/hivecommons/hive/issues/3848) asked
for the two to be separated and for the remaining allowance to be either removed
or accepted **with a written rationale**. This is that rationale.

Measured on `v4` at the time of writing:

| Inline style form | Count | Covered by a nonce or hash? |
| --- | --- | --- |
| `style="…"` attributes | 2061 | **No** |
| `<style>` elements | 7 | Yes |

The asymmetry is the whole decision. CSP's nonce and hash mechanisms apply to
*elements* — `<style>` and `<script>`. There is no nonce form and no hash form
for an attribute-level style. `style-src-attr` accepts only `'none'`,
`'unsafe-inline'`, or `'unsafe-hashes'` plus hashes; and `'unsafe-hashes'` would
mean enumerating a hash for each of 2061 distinct attribute values and
regenerating them on every UI edit, which is neither reviewable nor
maintainable. So for the attributes there is no policy that both permits them
and constrains them: the realistic choice is `'unsafe-inline'` or deleting every
inline style in the UI.

A second constraint shapes the timing. `script-src` still carries
`'unsafe-inline'` — that is the other half of #3848 and is staged behind an
event-delegation refactor (437 inline `on*=` attributes in `static/index.html`,
plus ~145 more built as strings inside the page's own JavaScript and injected
through 169 `innerHTML`/`insertAdjacentHTML` sites; a nonce cannot cover any of
them). **While inline script is permitted, tightening inline style buys no real
security**: an attacker who can inject a `<style>` element into a page can
inject a `<script>` element into the same page, and the script is strictly the
more capable primitive. Hardening the weaker one first would be motion, not
protection.

## Decision

Split the single `style-src` into the two directives CSP Level 3 provides, and
state a different verdict for each.

```
style-src      'self' 'unsafe-inline'   ← CSP2 fallback, unchanged
style-src-elem 'self' 'unsafe-inline'   ← the 7 <style> elements: CLOSABLE, staged
style-src-attr 'unsafe-inline'          ← the 2061 attributes: ACCEPTED
```

- **`style-src-attr 'unsafe-inline'` is accepted, not staged.** It is not a
  compromise awaiting a refactor; it is the end state unless the UI stops using
  style attributes. No tripwire test guards it, because there is nothing to
  trip.

- **`style-src-elem 'self' 'unsafe-inline'` is staged.** `<style>` elements
  accept hashes, and the hive serves only seven of them, all with deterministic
  content — so this one can be closed with hashes computed once at startup. It
  is deliberately sequenced *after* `script-src`, for the reason above.
  `TestCSPStyleSrcElemUnsafeInlineIsStaged` pins that staged state and instructs
  whoever closes it to invert the assertion, mirroring the `script-src` tripwire
  that #3844 established.

- **The `style-src` fallback stays.** Browsers without CSP3 support ignore
  `style-src-elem` / `style-src-attr` and fall back to it, so the effective
  policy is identical on old and new browsers. This split therefore names the
  decision without changing what any browser enforces today.

## Consequences

**Easier.** Closing the `<style>` half becomes a bounded, one-directive change
with a test already watching it, instead of an unscoped "remove unsafe-inline
from style-src" that would also have to answer for 2061 attributes. The two
halves can now move independently.

**Honest.** The policy states which allowance is permanent and which is
temporary. A reader of the header — or of an external CSP audit — can no longer
mistake the accepted attribute allowance for unfinished work, or the staged
element allowance for a settled decision.

**Residual risk, stated plainly.** With `style-src-attr 'unsafe-inline'`, an
attacker who achieves HTML injection can style injected markup. That enables UI
redressing (overlaying or hiding page content to mislead an operator) and, on
some engines, CSS-based inference of page content. It does **not** enable script
execution. This risk is accepted for as long as the UI uses inline style
attributes.

It is worth being clear that this residual is currently dominated by a larger
one: while `script-src 'unsafe-inline'` remains, HTML injection already yields
script execution, which subsumes everything the style allowance permits. The
style residual only becomes the *governing* risk once #3848's `script-src` half
lands — which is precisely why the element half is sequenced behind it rather
than in front of it.

**Harder.** Nothing meaningfully. The extra directives lengthen the header by
about 70 bytes on every response.
