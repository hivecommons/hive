# Dashboard feedback

The spoke dashboard has a **Feedback** button in the avatar menu, a floating button, a top-bar 🐛 button, and a **Report an Issue** item in the left sidebar. Use it to report a bug or request a feature; each submission becomes a GitHub issue in `hivecommons/hive`, or `hivecommons/docs` when you choose **Documentation**.

Submissions include the title, description, feedback type, target repository, and optional pasted or uploaded screenshots. Diagnostics are optional and previewed before submit. Tokens, bearer values, emails, and secret-like key/value text are redacted before sending.

When **Include diagnostics** is checked, the dashboard discloses the collected fields in the modal before submit and adds the same "Diagnostics included" table to the GitHub issue:

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

Hub-linked spokes relay feedback to the hub so the hub can create the issue with its GitHub credentials. Standalone spokes use the signed-in dashboard user's GitHub auth when available; otherwise the dashboard opens a prefilled GitHub issue URL and asks you to paste screenshots manually.

Screenshots must decode to a real PNG or JPEG image (the bytes are sniffed; the data-URI media type alone is not trusted). Accepted images are committed to a dedicated `feedback-screenshots` branch of the target repository — never its default branch — under `feedback-screenshots/<issue>/`, and linked from an issue comment. The branch is created from the default-branch head the first time it is needed.

The 🐛 button also shows a notification pill for feedback filed from this hive. The spoke stores created issue references in `/data/feedback-submissions.json`, polls GitHub activity through the hub relay or the signed-in user's token, and counts issues whose `updated_at` is newer than the last time the **My reports** tab was viewed. Fallback-URL submissions are not tracked until GitHub issue creation succeeds through the dashboard.
