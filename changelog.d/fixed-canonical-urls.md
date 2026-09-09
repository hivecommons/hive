- Fixed the hub advertising the retired `hive.kubestellar.io` domain to search
  engines, social unfurlers and API users. Every public page's `og:url` still
  named the old host — including the metadata every shared link to `/dashboard`
  renders from — and no page carried a `rel="canonical"` at all. That matters
  more than it looks: the legacy host's redirect to `hive.hivecommons.dev`
  **drops the path**, so every legacy URL lands on the homepage rather than the
  page that was linked, and with `og:url` as the only canonical signal the hub
  was pointing crawlers at a domain it no longer serves. All seven pages now
  carry a self-referencing canonical on the current host, and `og:url` agrees
  with it. The `/fleet` page gets one for a second reason: `/my-hives` redirects
  to it, so two paths served one page with nothing naming the preferred one.
- Fixed the copy-pasteable `curl` examples in the API documentation, which named
  the retired host and so returned the homepage's HTML instead of JSON (#5925).
