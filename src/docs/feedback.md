# Dashboard feedback

The spoke dashboard has a **Feedback** button in the avatar menu, a top-bar 🐛 button, and a **Report an Issue** item in the left sidebar. Use it to report a bug or request a feature. Each submission opens a **prefilled GitHub issue** in `hivecommons/hive` (or `hivecommons/docs` when you choose **Documentation**) in a new browser tab, which **you submit on GitHub as yourself**; the hive stores the full diagnostic bundle separately and links it from the issue.

Submissions include the title, description, feedback type, target repository, the submitting dashboard identity, and optional pasted or uploaded screenshots. Diagnostics are optional and previewed before submit. Tokens, bearer values, emails, and secret-like key/value text are redacted before sending.

When **Include diagnostics** is checked, the dashboard discloses the collected fields in the modal before submit and adds the same "Diagnostics included" table to the stored diagnostic bundle:

- Version — so maintainers can reproduce on the same dashboard release build.
- Commit — so maintainers can inspect the exact code revision.
- Release channel — so maintainers know which update stream you are running.
- ACMM level — so maintainers can match the dashboard policy mode.
- Hive ID — so hub operators can correlate this report with this hive only.
- Hosted flag — so maintainers know whether this is a hosted or self-managed dashboard.
- Hub-linked flag — so maintainers can follow the same relay path.
- Agent count — so maintainers can spot empty or unexpectedly large agent sets.
- Agent names, backend, model, and state — so maintainers can reproduce the affected agent configuration.
- Whether project org/repos were included — so the report shows if the optional project context was shared.
- Browser user-agent — so maintainers can reproduce browser-specific rendering bugs.
- Browser platform — so maintainers know the operating system/browser platform family.
- Browser language — so maintainers can reproduce locale-sensitive formatting.
- Screen size — so maintainers can reproduce layout issues at the same display size.
- Window size — so maintainers can reproduce the dashboard viewport.
- Dashboard section/path — so maintainers know where you opened the form.
- Optional project repo for each agent — only when **Include project org/repos in diagnostics** is checked.
- Optional project org for each agent — only when **Include project org/repos in diagnostics** is checked.
- Recent browser console errors — so maintainers can see client-side failures that happened before submit.
- Recent failed `/api` calls — so maintainers can see backend requests that failed before submit.

## Zero-scope handoff (issue #1927)

Signing in to the dashboard asks GitHub for **no OAuth scope** — the device flow only proves who you are. Nothing on the login path may request `public_repo` or `repo`; `TestDashboardLoginNeverRequestsRepoScope` in `src/pkg/github` scans the login sources and fails if a scope sneaks back in (this regressed once, in #11051, and showed every viewer a "read and write repository data" consent page).

Because the hive holds no user-scoped token, it does **not** file the issue. Submitting the form does two things:

1. **Bundle storage.** The spoke builds the full report (title, description, attribution line, "Diagnostics included" table, diagnostics, console errors, failed API calls, screenshots) and stores it with the **GitHub App installation token** — hub-less spokes mint it themselves; hub-linked spokes relay to the hub's `/api/feedback/ingest`, which mints the spoke's installation token. The bundle is committed to the `hive-feedback` branch of the target repository (never its default branch) under `feedback-bundles/<id>/report.md`, with screenshots alongside as `screenshot-N.png|jpg`. The branch is created from the default-branch head the first time it is needed. The result is a stable `https://github.com/<owner>/<repo>/blob/hive-feedback/...` URL. If no installation token is available the bundle is skipped and the modal shows a warning; the issue still opens.
2. **Prefilled issue.** The spoke returns `https://github.com/<owner>/<repo>/issues/new?title=…&body=…&labels=…` and the dashboard opens it in a new tab. The body is kept under **6 KB**: the attribution line, your description (truncated on a rune boundary if needed), a short footer, and `Diagnostic bundle: <url>` (the bundle line is never dropped). Labels are `kind/bug` or `enhancement` plus `user-feedback`. You review and press **Submit new issue** on GitHub, so GitHub shows **you** as the author.

If the browser blocks the new tab, or you are not signed in to GitHub, the modal shows the same URL as a button, a copyable link, and the bundle link — nothing fails silently. The modal's status line reads `Opens a prefilled GitHub issue for you to submit as yourself (attributed to @<user>) · diagnostic bundle stored by the hive's GitHub App · hive <hive-id> (<hub-linked: hub name | hub-less>)`.

The **GitHub username** field is optional: it is used only to attribute you in the prefilled text (`Opened by @<user> from hive <hive-id> (<hub name | hub-less>)`) and is remembered in browser local storage. A login verified by dashboard sign-in always wins over a typed one; a typed login is marked `(self-reported GitHub username, unverified)` and is never `@`-mentioned.

Credential guarantees:

- The hive **never** authors feedback issues — not with a user token, not as the App bot, and not with the operator PAT (`/data/gh-user-token`, `github.token`). The App installation token is used only to store the bundle. `/data/gh-user-token` remains the spoke owner's dashboard-login identity token; Hive validates it at most hourly.
- Hub-linked spokes send the hub only the report payload (bearer: the per-hive heartbeat key); the hub stores the bundle with the spoke's own App installation and returns `bundle_url`. It never uses the hub operator's PAT.

Screenshots must decode to a real PNG or JPEG image (the bytes are sniffed; the data-URI media type alone is not trusted). Accepted images are stored in the diagnostic bundle directory described above and embedded in `report.md`; they are not attached to the issue itself.

The 🐛 button also shows a notification pill for previously tracked feedback issues. The spoke keeps up to 50 issue references in `/data/feedback-submissions.json`, refreshes their activity through the hub relay or the App installation token (anonymously if neither is available — never with the operator PAT), and counts issues whose `updated_at` is newer than the last time the **My reports** tab was viewed. Because the user now files the issue on GitHub, new submissions are not added to this list automatically.
