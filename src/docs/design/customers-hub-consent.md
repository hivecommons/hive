# Customers: opt-in, consent-first import of hub-held person fields (#19088)

Status: **Decided; design only.** Spec for issue
[#19088](https://github.com/hivecommons/hive/issues/19088). The operator decided
the model in that issue (option 1, below), so this page records it and cuts it
into phases; it does not reopen it. No code or behaviour changes with this
record. Every phase ships behind its own issue and is off by default. It is the
"later proposal to send customer data to a hub" that
[the Customers design](customers.md) (section 12) reserved for its own review,
and it answers the first open question in that page's section 15. Related
records: [Customers](customers.md) for the spoke-side profile this feeds,
[telemetry](../telemetry.md) for the outbound-path review this falls under, and
[master key rotation](master-key-rotation.md) for the per-hive key derivation
reused for matching.

Scope: how three fields a person entered about themselves on the hub (real name,
organization, country) can reach a hive's Customers profile for that same
person, only after that person consents for that hive. Out of scope: any hub
data the person did not enter, any other field, any change to who may see
Customers on a spoke, and email-based affiliation (see
[Decision 4](#summary-of-the-decisions)).

Line references are to `v5` at `6fecd5f5` and give orientation. The identifier
next to each one is the stable reference. A statement marked **(unverified)**
was not confirmed against code and is listed again under
[Verification notes](#verification-notes).

## Summary of the decisions

The operator's decision on #19088, followed exactly:

1. **The person consents, per hive, on a hub page.** Not the hub operator by
   policy, not the spoke owner's switch. The spoke owner's switch is required
   and is never sufficient.
2. **The hub consent page and the per-person, per-hive consent record are built
   first, before any spoke code.** Without them there is nothing to consent to.
3. **Only values the person entered themselves (`user` provenance) cross to a
   hive. Admin-entered values never cross**, with or without consent. Inferred
   values never cross either.
4. **Email-derived affiliation stays spoke-local in v1.** It is revisited only
   after consent records exist.
5. **Default off until the person opts in.** Off on every hub, off on every
   spoke, off for every person.
6. **Any setting this needs gets a home in the dashboard Settings UI.**
7. **Never imported, not an option:** hub `notes`, email, Slack id, and the
   hub's cached affiliation.

**Decisions 3 and 4 are changed in part on 2026-11-19.** See
[Decision change (2026-11-19)](#decision-change-2026-11-19): country and
organization now cross labeled, without per-person consent, to a hive's grant
holders.

## Decision change (2026-11-19)

Issue [#19306](https://github.com/hivecommons/hive/issues/19306). The operator,
on 2026-11-19: "derived info is fine to pass - labeled. i think country and
affiliation are public info. forge, if public, is fine too. anything private
domain is not ok to pass".

What changes:

- **Country and organization are public information.** The country and the
  organization the hub holds or derives may cross to a hive for that hive's
  grant holders without a consent record. Every value carries a label naming
  where it came from, and the hive shows it in the organization pill tooltip
  and the profile details. A person who is not a grant holder of the asking
  hive, or is blocked, is the same silence as an unknown person.
- **Labels** (`customerMatchSource*` in
  `src/pkg/hub/customer_match_public.go`, stored on the hive as the field rule
  `ProfileSourceHub*` in `src/pkg/customers/hub_import.go`):

  | Label on the wire | Hive rule | Shown as | When |
  |---|---|---|---|
  | `consent` | `hub_consent` | from hub: entered by the person, shared with consent | real name, only with consent (unchanged) |
  | `person` | `hub_person` | from hub: entered by the person | organization or country the person entered on the hub |
  | `admin` | `hub_admin` | from hub: set by a hub admin | an admin set it on the person's behalf |
  | `email_domain` | `hub_email_domain` | from hub: derived from an organization email domain | organization only, see below |
  | `browser_language` | `hub_browser_language` | from hub: inferred from browser language | country guessed at sign-in from Accept-Language |
  | `unrecorded` | `hub_unrecorded` | from hub: source not recorded | a value saved before the hub recorded who supplied it |

- **Email-domain affiliation passes only for an organization's public domain.**
  The hub derives an organization from the person's email domain only when
  their organization is empty and was never explicitly cleared, and only when
  the domain (or its parent, `us.ibm.com` under `ibm.com`) is on the hub
  operator's list `HIVE_HUB_CUSTOMERS_ORG_DOMAINS` (`ibm.com=IBM,redhat.com=Red
  Hat`). The list is empty by default, so nothing is derived until the operator
  names domains. Free-mail and code-host domains (the same list the spoke's
  likely-affiliation suggestion uses, `customers.IsFreeMailDomain`) are refused
  even if listed. A personal or vanity domain, or any domain not on the list,
  is dropped. The hub had no classification that tells a personal domain from
  an organization's (the spoke's "Likely: IBM (from email domain ibm.com)"
  suggestion only excludes free-mail), so a configured list is the only way a
  domain counts. The email address and the domain never cross; only the
  organization name and the `email_domain` label do.
- **Public forge profile data is fine.** Filling from a person's public GitHub,
  GitHub Enterprise, GitLab, Gitea or Linear profile is separate work
  (`customers.profile_fill`).
- **Precedence on the hive**, highest first: set by hand on the hive >
  entered by the person on the hub (`hub_consent`, `hub_person`) > public forge
  profile (automatic) > hub derived or admin-set (`hub_admin`,
  `hub_email_domain`, `hub_browser_language`, `hub_unrecorded`). A manual edit
  or manual clear on the hive is never overwritten. A derived or admin-set hub
  value only fills an empty field or replaces an earlier hub value; the hub
  stays authoritative for its own values. `customers.HubValueYieldsToForge`
  tells the profile fill which hub values a forge value may replace.
- **Everything else keeps the consent-first rule.** Real name still crosses
  only with consent and only when the person entered it. Location is not held
  by the hub and never crosses. Notes, email, email domain and Slack ids never
  cross at all.

Wire and compatibility:

- A hive asks for labeled public fields with the request header
  `X-Hive-Customer-Match-Labels: 1` (`customerMatchLabelsHeader`). A header,
  not a body field, because the request body rejects unknown fields and a newer
  hive asking an older hub would otherwise fail outright.
- Each answer entry gains `sources`, a map from field name to label. A hive
  that did not send the header gets the consent-only answer it always got, so
  an older hive can never store a derived value as if the person consented.
- Public deliveries are not consent deliveries: they are not stamped on a
  consent record and no record is created for a person who never consented.
  The per-request audit line adds a `public` count. A public value the hub
  stops sending is removed on the hive by the existing 72 hour staleness
  expiry (`HubImportMaxAge`); a withdrawn consent still clears the real name at
  once.

Disclosure text. The hub profile-sharing page (phase 4,
[#19253](https://github.com/hivecommons/hive/pull/19253)) must say this beside
the per-hive switches:

```text
Your country and organization are treated as public. Each hive you hold a
grant on can show them on its Customers page without asking you, labeled with
where they came from: entered by you, set by a hub administrator, inferred
from your browser language, or, for organization only, derived from a known
organization email domain such as ibm.com. A personal or free-mail email
domain is never used. Your email address is never sent. Your real name is sent
only to the hives you switch on below. Notes, email and Slack ids are never
sent. To remove a country or organization from every hive, clear it here;
hives drop it within 72 hours.
```

On the hive, every hub value shows its label ("from hub: ...") in the
organization pill tooltip and beside the field in the profile details.

What must be switched on before a hive shows hub flags and organizations (all
off today):

1. On the hub: `HIVE_HUB_CUSTOMERS_SHARE_ENABLED=true` (the match endpoint
   answers `404` without it), and optionally `HIVE_HUB_CUSTOMERS_ORG_DOMAINS`
   for email-domain organizations.
2. Hub-side provisioning of the per-hive match key to the hive (#19246), or
   the hive holding a hub secret it can derive the key from.
3. On the hive: `customers.hub_import.enabled` (or
   `HIVE_CUSTOMERS_HUB_IMPORT_ENABLED`) and a hub registration.
4. The people must be confirmed customers on the hive with a confirmed GitHub
   login, and grant holders of that hive on the hub.

The sections below add only what the decision leaves open: the data model, the
wire shape, revocation, audit and the phases. Where a choice is mine rather than
the operator's it is marked **(design choice)** and listed under
[Open questions](#open-questions).

## What exists today

Every row was read in code.

| Thing | Where | What it does | Gap for this design |
|---|---|---|---|
| Hub user record | `SaaSUser` (`src/pkg/hub/saas.go:140`) | One JSON file per person under `saasUsersDir` (`src/pkg/hub/saas.go:12`). Holds `FullName` (`src/pkg/hub/saas.go:200`), `Notes` (`src/pkg/hub/saas.go:202`), `Company` (`src/pkg/hub/saas.go:210`), `Country` and `CountrySource` (`src/pkg/hub/saas.go:281`), `Affiliation` (`src/pkg/hub/saas.go:320`), `Email` and `LinkedGitHubLogin` (`src/pkg/hub/saas.go:180`). | No consent state anywhere on it. |
| Country provenance | `countryProvenanceRank` (`src/pkg/hub/user_country.go:48`), `mayOverwriteCountry` (`src/pkg/hub/user_country.go:101`) | `user` beats `admin` beats `inferred` beats unset; an admin edit cannot overwrite a `user` country. | Only country has this. |
| Name and organization provenance | `FullName` and `Company` in `SaaSUser` | The struct comments call both admin-entered CRM text. `applyRequestContactToUser` (`src/pkg/hub/saas_provisioning.go:280`) also fills a blank `FullName` from the get-started request form the person typed. | **No provenance field.** A name a person typed and a name an admin typed are the same bytes. |
| Self-service write | `handleMyCountry` (`src/pkg/hub/user_country.go:310`), routed at `src/pkg/hub/saas.go:391` | Identity from the session only, JSON body, so a country is never in a URL. The one field a non-admin may write on their own record. | Name and organization have no such route. |
| Per-hive access rows | `HiveAccessEntry` (`src/pkg/hub/saas_access.go:78`) | Carries `FullName` and `SlackID` to a hive owner for every granted user, `Notes` only to a hub admin. | Existing disclosure to an owner that is not consent-based; unchanged here (see [Risks](#risks)). |
| Hub to spoke channel | `handleHeartbeat` (`src/pkg/hub/server.go:1906`) | The spoke's periodic beat; the hub can answer in the reply. | No endpoint where a spoke asks about a person, none returning person fields. |
| Per-hive bearer | `verifyHeartbeatBearer` (`src/pkg/hub/hub_keys.go:165`) | Bearer bound to the claimed hive id; the only accepted credential. | Reused as the auth for the match endpoint. |
| Per-hive key derivation | `derivePerHiveKey` (`src/pkg/hub/hub_keys.go:39`) | Derives a per-hive key from the hub secret, a domain label and the hive id. | A new label gives the match key. |
| Hub user deletion | `handleAdminDeleteUser` (`src/pkg/hub/saas_users_admin.go:628`) | Removes the user file; audit line `audit: admin deleted user`. | Nothing else keyed to the person would be removed. |
| Spoke profile | `ProfileField` (`src/pkg/customers/model.go:291`), `Source` values `auto` and `manual` (`src/pkg/customers/model.go:65`), `MaxProfileFieldLen` (`src/pkg/customers/model.go:87`) | Real name, organization, location, country, each with provenance. The type comment says nothing here comes from the hub. | No third source. |
| Spoke forget | `Store.tombstoneHash` (`src/pkg/customers/tombstone.go:11`) | A forgotten identity leaves a per-hive salted hash so a sweep cannot rebuild the person. | Import must honour it. |
| Spoke settings home | Customer avatars switch (`src/pkg/dashboard/static-src/js/config-features.js:893`), `CustomersConfig` (`src/pkg/config/customers.go:13`) | Config key, env override and a Settings, Features toggle with a help mark. | The pattern to follow for the new switch. |

Two findings shape the design:

1. **Only country carries provenance on the hub today.** The decision says only
   `user` values cross, so the first hub change is to give name and organization
   a source the way country has one. Without it the rule cannot be enforced.
2. **The hub has no place where a person states their own name or
   organization.** Today an admin types them, or a request form copies a name
   once. So the consent page has to double as the place a person sets the three
   fields, or there is nothing `user`-sourced to share.

## Threat model and what is protected

What we protect: a person's control over which hive sees which of three facts
about them, and the guarantee that nothing a third party said about them crosses
as if they had said it.

| Actor | Risk | In scope |
|---|---|---|
| Hive owner or spoke code that wants more than consented | Asks the hub about people or fields it was not given. | Yes. The hub answers only per consent, per field. |
| Hub admin | Enters an organization or country for someone, or tries to consent for them. | Yes. Admin values never cross; an admin cannot grant, edit or see a consent as the person. |
| Person | Changes their mind. | Yes. Withdrawal is immediate on the hub and propagates to the spoke. |
| Another hive or person probing for who exists on the hub | Uses the match reply as an oracle. | Yes. Unknown and unconsented are indistinguishable. |
| Hostile hive owner after receipt | Keeps or exports what was shared. | No. The hub cannot recall copies; the consent text says so. |
| Malicious hub operator | Reads the hub's own data. | No. |

## Model

### Provenance rule (the one that decides what crosses)

A value crosses only when **all** hold at answer time, evaluated on the hub, per
field, per request:

1. The person has an effective consent record for this hive that includes the
   field.
2. The field's hub provenance is `user`: the person entered it themselves, on
   the consent page or on the get-started request form (`full_name_source`,
   `company_source` and `country_source` all equal `user`).
3. The value is non-empty after trimming and within the length cap.

Consequences, stated so they cannot be misread:

- `admin` and `inferred` values never cross, even with consent. An operator's
  attribution is a claim by someone else; an inferred country is a guess about a
  browser.
- A **legacy** name or organization has no source and counts as not
  user-entered. There is no backfill, so existing records stay byte-identical
  on disk, the property the `SaaSUser` struct is built around. The person
  makes it shareable by typing it again on the consent page. Merely ticking a
  box next to an admin-typed value does not make it theirs. **(design choice)**
- If an admin later edits a `user` field, the edit is refused for that field,
  the same rule country already has (`mayOverwriteCountry`). A field that stops
  being `user`-sourced stops crossing and the spoke is told to clear it.
- A value is read live at answer time, never copied into the consent record, so
  a person's later correction reaches the hive on the next pull and the record
  never holds a stale value.

### Data model: the consent record

One record per person and hive, in its own store (for example
`/data/saas/consents/`, one file per person), **not** inside `SaaSUser`. Reasons:
`SaaSUser` is read and written by admin paths that must never be able to touch a
consent, and a separate file keeps its byte-identical round trip intact.
Written atomically, mode 0600, like the user records.

| Field | Meaning |
|---|---|
| `person` | The same stem the user file uses (`saveSaaSUserPath`, `src/pkg/hub/saas_users_admin.go:112`), so deleting a user finds their records. Never an email. |
| `hive_id` | The hive consented to. Must be a hive the person holds a grant on when the consent is made. |
| `fields` | `{real_name, organization, country}`, each a boolean. All false until the person turns one on. |
| `text_version` | The version of the disclosure text the person saw when they last confirmed. |
| `granted_at`, `updated_at` | Times of the first grant and the last change. |
| `withdrawn_at` | Set when the person withdraws every field; the record is then a tombstone. |
| `delivered` | Per field, the last revision the hive was sent, plus `first_delivered_at` and `last_delivered_at`. Written by the match endpoint. |

Semantics:

- **Effective consent** at answer time requires: record present and not
  withdrawn, `text_version` current (a material text change suspends sharing
  until the person reconfirms), the person still holds a grant on the hive (an
  expired or revoked grant suspends it), the person is not blocked or deleted,
  and the hub's sharing switch is on.
- A **withdrawn** record is kept only while a clearing reply may still be owed
  (it has `delivered` set), and then for a bounded retention, a named constant
  (this design proposes 90 days). After that it is deleted, and the spoke's own
  staleness expiry (below) covers a hive that never synced.
- No admin route writes, edits or grants a consent. An admin sees aggregate
  counts per hive, never a person's field values through this feature.

## The consent page

A signed-in hub page, linked from the Fleet page and from each hive card,
behind the hub's sharing switch (default off; with it off the page and routes do
not exist). The hub session is the identity; the body carries none, as in
`handleMyCountry`.

Flow:

1. **Your details.** The three fields, each with its current value and a plain
   label of where it came from ("you entered this", "an administrator entered
   this", "guessed from your browser"). The person can type or clear each; a
   save stamps `user`. A value that is not `user`-sourced shows "not shared
   until you enter it yourself".
2. **Hives.** Each hive the person holds a grant on, with three switches, all
   **off and never pre-checked**. A hive with no grant is not listed, and the
   route refuses a consent for one.
3. **Disclosure**, shown beside the switches and versioned by `text_version`:
   what is sent (only the fields switched on, only values you entered, only
   about you); to whom (this hive: its owner and anyone with Customers access
   there can see and export it); what is never sent (notes, email, Slack id,
   affiliation, anything an administrator or a guess supplied); how to withdraw
   and how fast (immediately here, on the hive at its next sync); and what
   withdrawal cannot undo (a copy the hive already exported or saw).
4. **Explicit action.** Turning a switch on and confirming is one POST that
   writes the record and an audit line. Turning it off is one POST that
   withdraws. Both need the session and are same-origin only **(unverified:**
   the hub's cross-site request defence for state-changing routes was not
   traced; phase 1 must use whatever the existing self-service routes use**)**.
5. **What was sent.** Per hive, when the hive last received each field (from
   `delivered`), so the person can see consent being used.

Deleting a hub user deletes every consent record of theirs in the same
operation. Hub account deletion is a withdrawal for every hive.

## What crosses the wire

A per-hive request from the hive, answered by the hub. Both ends are named
constants for size and rate; none of this is final until phase 2.

- **Auth.** The existing per-hive bearer, verified against the claimed hive id
  (`verifyHeartbeatBearer`), on a sibling endpoint to the heartbeat. A hive can
  only ask as itself.
- **Request.** A bounded batch of match hashes. Each is
  `HMAC(match_key, normalised GitHub login)`, where `match_key` is derived per
  hive with `derivePerHiveKey` under a new domain-separation label, never the
  heartbeat bearer itself **(unverified:** how the spoke receives or derives it
  was not traced; the key must be one the hub can recompute and the spoke
  holds**)**. The spoke sends hashes only for **confirmed customers**, never
  for a suggested person and never plaintext.
- **Answer.** For each hash that matches a person with effective consent:
  `{match_hash, rev, real_name?, organization?, country?}`, each field present
  only if consented and `user`-sourced. For a hash the hive has been
  delivered before and which is now withdrawn or no longer shareable:
  `{match_hash, cleared: [fields]}`. **Everything else is silent.** An unknown
  person, an unconsented person and a person never delivered to are
  indistinguishable, so the reply cannot tell a hive whether someone exists on
  the hub. The `cleared` entry is the only place the hub says anything about a
  past consent, and only for a hive that already holds that person's data.
- **Matching scope in v1: GitHub login only** (the primary `github:` identity or
  the linked GitHub login). Email is not matched in v1: the hub's own comment
  on `Email` says it is display only and never a key because emails are
  reassignable, and a spoke's confirmed email matching a different person's
  reassigned address would hand them the wrong person's name. This narrows
  "GitHub login or confirmed email" in the issue's sketch. **(design choice)**
  Whether a linked GitHub login is proven by the hub's link flow is
  **(unverified)**; phase 2 must confirm before matching on it.
- **Only consented persons are hashed for comparison.** The hub never hashes its
  whole user list against a spoke's batch, and never stores or logs a
  non-matching hash.
- **Limits.** Batch size, request size, response size and a per-hive rate limit,
  plus a short hub-side cache, all named constants.
- **Spoke cadence.** Pull on a bounded interval and when a customer is
  accepted; the exact interval is a constant in phase 3. The hub does not push.

## The spoke side

- **Provenance `hub`**, a third `Source` beside `auto` and `manual`, shown as
  "from the hub". **`manual` always wins and is never overwritten. `hub`
  outranks `auto`** (the person's own statement beats public profile text) and
  is overwritten by nothing but a person (which makes it `manual`) or the next
  sanctioned import. **(design choice for the `auto` ordering)**
- **Import only fills an existing customer.** It never creates a person, never
  promotes a suggestion, and never touches identities, tags, goals, notes or
  satisfaction. It reads exactly three fields from the reply and treats them as
  untrusted text: length-capped to `MaxProfileFieldLen`, control characters
  stripped, rendered escaped. Hub values do not enter the kick context or the
  persona snippet, which carry no profile fields today.
- **Forget.** Forgetting a person on the spoke already deletes them and leaves a
  hash tombstone (`tombstoneHash`); import consults it and never rebuilds them.
- **Staleness.** A `hub` value not refreshed within a maximum age, a named
  constant, is cleared the same way a `cleared` reply clears it, so a hive that
  loses touch with the hub does not hold a withdrawn value forever.
- **Settings.** `customers.hub_import.enabled`, default off, env
  `HIVE_CUSTOMERS_HUB_IMPORT_ENABLED` overriding it, and a switch in Settings,
  Features beside Customer avatars, with the usual help mark and an "overridden
  by the environment" line. It also requires the hive to be registered with a
  hub. The hub's per-person consent decides what arrives; the switch only says
  this hive asks. The help text states that the request carries hashed GitHub
  logins of confirmed customers and nothing else.
- **Match key provisioning.** Hub-hosted spokes receive the per-hive match key
  as `HIVE_CUSTOMER_MATCH_KEY` from the hub (provisioning and the per-hive env
  reconcile, current master generation). Self-service (`byo-`) hives hold no
  master and get no provisioning, so they cannot use the hub import in v1.

## Revocation

- **Person withdraws:** the hub stops answering for them at once. The next
  pull returns `cleared` for the fields previously delivered; the spoke removes
  only the `hub`-provenance fields named, and each returns to automatic (so
  the GitHub-derived value may refill it, which is the pre-import state).
  A field the person had since edited on the spoke is `manual` and is left
  alone.
- **Per-field withdrawal** works the same way for that field only.
- **Provenance lost:** an admin edit is refused on a `user` field, but if a
  field is cleared or replaced by the person, the answer changes on the next
  pull like any other edit.
- **Grant lost:** an expired or revoked hive grant suspends the consent; the
  next pull clears.
- **Hub user deleted:** a withdrawal for every hive, as above.
- **Spoke forgets:** deleted locally with a hash tombstone; the hub is not told,
  and the next import cannot recreate the person.
- **Latency is stated, not hidden.** The consent page says withdrawal is
  immediate on the hub and takes effect on a hive at its next sync, and that a
  copy already exported cannot be recalled.

## What is never imported

Not an option, in any phase of this design:

| Hub data | Why |
|---|---|
| `Notes` | Admin-private text that can name other people. Withheld even from hive owners today. |
| `Email` | An identifier, not profile context; reassignable. Not imported in v1. |
| `SlackID` | An identifier. Not imported in v1. |
| `Affiliation`, `ProfileBio`, `ProfileLocation`, `PublicActivity`, `TopRepo` | Cached from a public GitHub footprint by the hub; not entered by the person on the hub. Spoke email affiliation stays spoke-local in v1. |
| `DisplayName` | Asserted by an identity provider, not entered on the hub. |
| Any `admin` or `inferred` value of the three fields | See the provenance rule. |
| Engagement counters, login times, anything else on the record | Not profile context. |

## Privacy and audit

- **Default off at every layer:** hub switch, spoke switch, per-person,
  per-hive, per-field. Nothing is pre-checked and nothing is inferred as
  consent.
- **Data minimisation:** three fields, only the ones switched on, only values
  the person entered, only to the hive they chose, only for a customer the hive
  has confirmed.
- **No oracle:** unknown and unconsented are the same silence; a `cleared`
  entry exists only for a hive already holding the data.
- **What the hub learns:** the hashes of confirmed-customer GitHub logins a
  hive posts, compared only against consented persons. For a consenting person
  the hub therefore learns that this hive has them as a customer. The consent
  text says so. Non-matching hashes are never stored or logged.
- **Hub audit:** one `audit:` line per grant, change and withdrawal (person,
  hive id, field names, never values), in the style of the existing audit lines
  (`src/pkg/hub/saas_users_admin.go:660`). Per request: hive id and counts. Per
  delivery: stamped on the record, shown to the person, not a global log line.
- **Spoke audit:** one line per import or clear (person id, field names, never
  values).
- **Telemetry review:** this adds an outbound request from the spoke to the hub.
  It is off by default and consent-gated, which is what
  [telemetry](../telemetry.md) asks for; phase 4 updates that page and the
  Customers page, whose "nothing is read from the hub" statements become "not
  unless the person opted in".
- **Retention:** consent records follow the person (deleted with the user);
  withdrawn markers are bounded; the spoke clears stale `hub` values.

## Phases

Each phase is its own issue, off by default, and carries no spoke code until
phase 3.

| Phase | Issue | Scope |
|---|---|---|
| 1 | [#19160](https://github.com/hivecommons/hive/issues/19160) | **Hub, first:** `full_name_source` and `company_source` provenance, self-service profile fields, the consent record store, the consent page and API, the hub switch, audit lines, deletion cascade. Sends nothing to any hive. |
| 2 | [#19162](https://github.com/hivecommons/hive/issues/19162) | Hub: the per-hive match endpoint, match key, answer and `cleared` semantics, limits, delivery stamps. No spoke consumer yet. |
| 3 | [#19163](https://github.com/hivecommons/hive/issues/19163) | Spoke: `hub` provenance, the import client, tombstone and staleness handling, `customers.hub_import.enabled` with env and the Settings, Features switch. |
| 4 | [#19164](https://github.com/hivecommons/hive/issues/19164) | Disclosure text, docs and telemetry page updates, the person's "what was sent" view, admin aggregate counts, live verification. |

Phase 1 is deliberately useful on its own (a person can set their details and
record consent) and strictly before any spoke change, as decided. Phases 2 to 4
are ordered by dependency and each is blocked by the one before.

## Test strategy

- **Hub:** an admin edit cannot overwrite a `user` field; a legacy value is not
  shareable; a consent cannot be written for another person or for a hive with
  no grant; withdrawal and user deletion remove or mark the record; with the
  switch off the routes are absent and the heartbeat is unchanged.
- **Match endpoint:** unknown and unconsented hashes get identical answers;
  `admin` and `inferred` values never appear; `cleared` only follows a delivery;
  the bearer is bound to the claimed hive and one hive cannot read another's
  consent; limits hold.
- **Spoke:** `manual` is never overwritten; `hub` overwrites `auto`; no person
  is ever created; switch off means zero requests; `cleared` and staleness both
  revert to automatic; a forgotten person is not rebuilt; only three fields are
  read from a reply.
- **Live:** grant, receive, edit on the spoke, withdraw, see the clear, delete
  the hub user, and confirm an admin-typed organization never crosses.

## Risks

- **Few will opt in at first.** Accepted by the decision; the page should make
  the benefit clear rather than nag.
- **Existing disclosure to hive owners is not consent-based.** Per-hive access
  rows already give an owner a granted user's `FullName` and `SlackID`
  (`HiveAccessEntry`). This design does not change that, and the consent text
  must not claim a hive otherwise cannot see a name. Whether those rows should
  follow the same consent is a separate question for the operator.
- **Unverified identity link.** If a linked GitHub login is not proven, matching
  on it could attach the wrong person's data; phase 2 verifies it first.
- **Hub learns a customer relationship** for consenting people (see Privacy).
- **Copies cannot be recalled.** Said plainly on the page.
- **Scope creep.** Email matching, notes and affiliation reconciliation are
  named exclusions; each would be its own decision.

## Open questions

Marked **(design choice)** above; the operator may overrule any without
changing the decided model:

1. Must a person retype an admin-typed value to make it shareable, rather than
   tick a box beside it? This design says retype (strict reading of "entered
   themselves").
2. Which hives can a person consent to: only those they hold a hub grant on
   (this design), or any registered hive reached by a link from its owner?
3. Match on GitHub login only in v1, email later once the hub holds a verified
   address.
4. `hub` outranks `auto` on the spoke.
5. A material disclosure-text change suspends sharing until reconfirmed.

## Exit criteria

#19088 closes when this page is merged and phases 1 to 4 exist as issues
(done: the table above). Implementation then proceeds through those issues.

## Verification notes

Verified by reading code on `v5` at `6fecd5f5`: the `SaaSUser` fields and
comments; that only country has a source field; the country precedence helpers;
that `FullName` is also filled from the request form; the self-service country
route; the access rows carrying `FullName` and `SlackID`; the per-hive heartbeat
bearer and its derivation; the admin user delete and its audit line; the
spoke's `Source` values, `ProfileField`, tombstone hash and the Customer avatars
switch pattern.

Not verified, and the reason:

- **Hub cross-site request defence for state-changing routes.** Not traced.
- **How a spoke obtains the per-hive match key.** The derivation helper was
  read; the spoke-side delivery was not.
- **Whether a linked GitHub login is proven.** The struct comment says it is an
  attached identity; the link flow was not read.
- **A self-service user-deletion path on the hub.** Only the admin delete was
  found.
- **Any existing hub privacy notice** other than [telemetry](../telemetry.md).
  None was found in the hub's pages.
- **Interval, retention, batch and rate numbers.** Proposals, not measurements.

## References

- Issue [#19088](https://github.com/hivecommons/hive/issues/19088) and the
  decision comment; related
  [#19085](https://github.com/hivecommons/hive/issues/19085) (spoke-local
  country, affiliation, notes) and
  [#18973](https://github.com/hivecommons/hive/issues/18973).
- [Customers: a profile per person and a customer graph](customers.md),
  [telemetry](../telemetry.md),
  [master key rotation](master-key-rotation.md),
  [hosted hub](../hosted-hub.md).
