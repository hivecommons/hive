# Betty on Zoom without a Zoom app: dial-in, desktop companion, live stream

**Status: design only.** Recommendation accepted in
[#18596](https://github.com/hivecommons/hive/issues/18596); nothing on this
page is built yet. The operator page for meetings as they work today is
[`meetings.md`](../meetings.md); read it first. Related: meetings
[#18284](https://github.com/hivecommons/hive/issues/18284), Betty
[#18391](https://github.com/hivecommons/hive/issues/18391).

## 1. The wall

Betty cannot join Zoom as a web-client bot any more. The Zoom web client
answers a Vexa bot with "automated bots aren't allowed ... must use Zoom RTMS",
and Zoom's developer documentation now says the Meeting SDK "is reserved for
human use cases and does not support bots or AI notetakers". From March 2,
2026 an app that joins meetings outside its own account must be authorized
(OBF or ZAK tokens, or RTMS).

We accept that wall. This page compares the ways Betty can still take part in
a Zoom meeting:

- **without** a Zoom Marketplace app or RTMS, and
- **without** evading Zoom's bot detection: no stealth browsers, no
  fingerprint spoofing, no anti-bot bypass of any kind.

Every option has to keep the three rules the meetings design already lives by:

1. **Consent first.** Nothing from Betty before the consent notice, and the
   notice must be delivered, or Betty leaves.
2. **Hive is the policy point.** Voice rules, chat rate limits, the research
   cap and the LLM budget are hive's checks, whatever the platform.
3. **Transcripts are erased upstream** after hand-off to the Inbox.

## 2. Options

### 2.1 Phone dial-in (PSTN through a telephony provider)

Betty places an outbound call to the meeting's dial-in number, sends
`<meeting id>#`, `#` (no participant id) and `<passcode>#` as DTMF, streams the
call audio to the shared transcription service and plays Kokoro TTS back into
the call. A Quorum `phone` platform would own the call.

- **Account tier.** Telephone dial-in is a paid-plan feature: Zoom's free-plan
  page lists toll dial-in among what a paid upgrade adds, and Zoom Community
  moderators confirm the host must be licensed. Betty could phone into
  meetings hosted on **paid** accounts, never into meetings the owner hosts on
  a free (Basic) account, which are also capped at 40 minutes.
- **SIP instead of PSTN** needs Zoom's Conference Room Connector (H.323/SIP),
  a paid per-account add-on; it does not help a free account either.
- **Cost** (Twilio US pay-as-you-go, October 2026 price page): outbound call
  $0.0140/min plus Media Streams $0.0044/min, $0.0184/min or about **$1.10 per
  meeting-hour**, plus $1.15/month for a number (or a verified caller id).
  Telnyx and other SIP trunks are in the same range.
- **Compliance.** A phone participant is an ordinary, visible participant
  (shown as a phone number; it waits in the waiting room unless the host lets
  dial-ins bypass it). Zoom's Terms do not single out phone participants, but
  an automated phone participant is close to what Zoom's bot rules aim at, so
  Betty would only dial in when the host invited her.
- **Consent.** A phone line has no chat, so the notice must be **spoken** on
  entry. Hive can confirm the audio played, not that anyone heard it: weaker
  than the confirmed chat delivery rule 1 asks for.
- **UX.** Audio only, one mixed stream (no speaker names), DTMF fragile if the
  dial-in prompts change. Betty can speak.
- **Effort.** Medium: a Media Streams websocket bridge in Quorum, DTMF
  sequencing, spoken consent, one new secret.
- **Needs from the owner.** A telephony account and number, billing, and
  meetings hosted on a paid plan. Not startable today.

### 2.2 Desktop companion (recommended)

A small desktop app on the computer of the host (or any invited human)
captures **their own Zoom client's audio** and streams it to their hive as a
live meeting. The pattern is the one Plaud Desktop ships: the app notices a
Zoom, Teams or Meet call starting, asks "Let Betty listen to this call?" with
the consent reminder, captures the **call app's audio** (everyone else) and
the **microphone** (the user), and streams both. Betty's existing pipeline
(transcript, Spek notes, research, replies) consumes it unchanged. Replies show
in a small companion window and in the hive's Meetings panel and Inbox. Betty
speaking into the call through a virtual microphone is out of scope for v1.

- **Capture without a virtual driver.** On macOS, ScreenCaptureKit captures
  one application's audio (`SCStreamConfiguration.capturesAudio`,
  `excludesCurrentProcessAudio`; the microphone through `captureMicrophone`
  on macOS 15 and later), and Core Audio process taps (`CATapDescription`,
  `AudioHardwareCreateProcessTap`, macOS 14.2 and later, with an
  `NSAudioCaptureUsageDescription`) tap a single process. Windows has WASAPI
  per-process loopback; Linux has PipeWire per-node capture. BlackHole or
  VB-Cable are needed only for the later "Betty speaks through the user's
  microphone" step.
- **Compliance.** Nothing joins the meeting and nothing touches Zoom's servers
  or bot detection: it is the user's own client audio on the user's own
  machine, like Plaud Desktop or Zoom's own local recording. The legal duty
  is the one Zoom's Terms (section 7) put on every user: comply with
  recording-consent laws.
- **Consent.** Zoom does **not** notify participants, so the user's
  attestation is the consent gate:
  - every call needs an explicit "I have told the participants" attestation,
    versioned like the Inbox recorder's `consent_version`
    ([`inbox-recording.md`](../inbox-recording.md#consent));
  - hive records who attested, when, and which notice version;
  - a persistent red "Betty is listening" indicator shows while capturing,
    and one click stops it;
  - the companion offers the consent notice on the clipboard to paste into
    the meeting chat.
- **Privacy and security.**
  - Audio leaves the machine only while a capture the user accepted is
    running, only to the user's own hive, over TLS, as raw 16 kHz mono PCM.
  - The companion authenticates with a **per-user, revocable companion
    token** minted from the dashboard by an owner or read-write user. Only its
    SHA-256 is stored, as for the phone-widget token
    (`src/pkg/dashboard/widget_token.go`). Unlike the widget token, which is
    one per hive and read-only, the companion token belongs to one user and
    acts as that user **at their live role**: a revoked or downgraded user
    stops working at once, and read-only users cannot stream.
  - The token is admitted on the companion routes only and is carried in its
    own header, never in `Authorization`, so a hosted hive's hub never takes
    it for a GitHub token.
  - Nothing is stored on the laptop beyond the token, kept in the OS
    keychain.
- **UX.** Works on the free Zoom account and on every platform the user's
  client joins (Zoom, Teams, Meet, Webex, Slack huddles). Two channels give
  "you" versus "the call" attribution; the call side is one mixed stream, with
  no per-person names. Without headphones the microphone picks up the
  speakers: v1 documents headphones and drops microphone segments that
  duplicate a call segment.
- **Cost.** None: it uses the existing transcription service.
- **Effort.** Medium-high: a desktop app, then one capture module per
  operating system.

### 2.3 Zoom "Live on Custom Live Streaming Service" (RTMP)

The host clicks More, Live on Custom Live Streaming Service, and Zoom pushes
the meeting to an RTMP URL that Betty transcribes.

- **Account tier.** Needs a Pro, Business, Education or Enterprise host and
  the admin settings "Allow livestreaming of meetings" and "Custom Live
  Streaming Service". **Not available on the free account.**
- **Compliance.** Fully sanctioned and visible: Zoom tells everyone the
  meeting is live.
- **Consent and replies.** Listen-only: no chat and no voice, so Betty can
  neither post the consent notice nor answer in the meeting.
- **UX and effort.** The host starts it in every meeting; 10 to 30 seconds of
  stream latency; the hive needs a public RTMP ingest endpoint, a new exposed
  surface. Cost: a Zoom Pro licence plus ingest bandwidth.

### 2.4 Other documented paths

- **Zoom RTMS or a Marketplace app.** Zoom's sanctioned route for AI
  notetakers; excluded by the brief because it needs a Zoom app.
- **Zoom cloud recording and transcript, then import.** A paid feature,
  available only after the meeting; the existing Zoom cloud-recordings
  connector ([`meeting-recordings.md`](meeting-recordings.md)) already brings
  those into the Inbox.

## 3. Comparison

| | Phone dial-in | Desktop companion | RTMP live stream |
|---|---|---|---|
| Works with the owner's free Zoom account | No (host must be paid) | **Yes** | No (Pro or higher) |
| Close to Zoom's bot rules | Yes (automated participant) | No (user's own client) | No (Zoom feature) |
| Consent delivery | Spoken only, unconfirmable | Per-call attestation, indicator, clipboard notice | None in the meeting |
| Live Betty answers | Yes, by voice | Yes, in the companion window and panel (voice later) | Transcript only, 10 to 30 s late |
| Speaker attribution | None | You versus the call | None |
| Cost | About $1.10 per meeting-hour plus $1.15/month | $0 | Zoom Pro plus ingest |
| Needs an owner purchase | Yes (telephony account) | No | Yes (Zoom Pro) |
| Other platforms at no cost | Any with dial-in | Zoom, Teams, Meet, Webex, Slack | Zoom only |

## 4. Recommendation

Build **the desktop companion (2.2)**, macOS first. It is the only option that
works with a free Zoom account, needs no purchase, stays clear of Zoom's bot
rules because nothing joins the meeting, and reuses Betty's live pipeline as
is.

Keep **phone dial-in (2.1)** as a later add-on for meetings hosted on paid
accounts, if the owner wants Betty to speak without the user's machine; it
needs a telephony account and spoken-consent rules first. **Drop the RTMP live
stream (2.3)**: listen-only, paid, and a new public ingest.

## 5. How it fits what hive already has

The dashboard's **● Record** ([`inbox-recording.md`](../inbox-recording.md))
already captures the microphone plus a meeting tab's audio in Chromium through
`getDisplayMedia`, behind a versioned consent notice, for owners and
read-write users only. Its ingest (`POST /api/inbox/recordings`, ordered
`chunks?seq=N`, `finalize`) is **post-meeting**: chunks must be WebM or MP4
media, they are concatenated at finalize and transcribed afterwards, so Betty
cannot react during the call.

The companion keeps Record's consent model and the shape of its chunk
protocol (ordered `seq`, 409 with `expected_seq` on a gap, an idempotent
resend) but streams to a **live meeting** route, so Betty answers during the
call. When the meeting ends it lands in the Inbox as an ordinary `meeting`
item, like a Vexa or Discord meeting, so no separate Record upload is needed.

## 6. Streaming contract (proposed)

The contract is to be fixed in the Quorum contract (`docs/contract.md` in
hivecommons/quorum, a new `companion` platform) and in
[`meetings.md`](../meetings.md) when it ships. Companion to hive:

| Route | Body | Answer |
|---|---|---|
| `POST /api/companion/meetings` | `{title, app, consent_version, consent_attested: true}` | Creates a live meeting on platform `companion` (hive calls Quorum `meeting.join` with `platform: "companion"`), records who attested and when; returns `{id, sample_rate: 16000, max_chunk_bytes}`. A stale `consent_version` or a missing attestation is refused. |
| `POST /api/companion/meetings/{id}/audio?channel=call\|mic&seq=N` | Raw 16 kHz mono s16le PCM | Per channel, in order; 409 `{expected_seq}` on a gap; a resent chunk already stored answers 200. |
| `GET /api/companion/meetings/{id}/replies?after=N` | | Betty's lines for the window, the consent notice first. |
| `POST /api/companion/meetings/{id}/end` | | Ends the meeting. "betty leave" said in the call ends it too. |

- **Auth.** `X-Hive-Companion-Token` (per user, revocable, companion routes
  only). On hosted hives the hub's auth gate waves `/api/companion/` through
  to the spoke, which does the real check: the same pattern as the phone
  widget path (`spokeWidgetSummaryPath` in `src/pkg/hub/saas_auth.go`).
- **Quorum.** The `companion` adapter segments each channel with an energy
  VAD, transcribes with the shared transcription service, and emits ordinary
  `segment` events (`speaker` is the user for `mic`, "Call" for `call`).
  `chat.post` queues the line for the companion window and reports it
  `confirmed` once the companion has fetched it, which is what satisfies
  rule 1. `say` answers `platform_unavailable` in v1.
- **Open in the contract.** `quorum.contract/v1` has no tool that carries
  audio from hive into Quorum; the `companion` platform adds one. Its name
  and framing are Quorum's to fix, and hive's routes forward to it.

## 7. Distribution (owner decision)

- One Tauri codebase in a new repository, `hivecommons/quorum-companion`
  (Apache-2.0, DCO); macOS first, then Windows, then Linux.
- **No Apple Developer ID.** Builds are ad-hoc signed (`codesign -s -`) and
  shipped as a GitHub Release `.dmg` or `.zip` and a Homebrew cask in
  `hivecommons/homebrew-hive`.
- First launch needs System Settings, Privacy & Security, **Open Anyway**,
  then the **Screen & System Audio Recording** and **Microphone**
  permissions. Homebrew 5 removed `--no-quarantine`, so the cask
  documentation shows the Open Anyway step.
- An ad-hoc signature has no stable designated requirement, so macOS may
  forget the audio-capture grant after each update. Mitigation: keep the
  bundle id (`dev.hivecommons.quorum-companion`) and the install path fixed;
  an optional self-signed certificate (below) gives a stable requirement.

## 8. Prototype PR set

Tracked in [#18596](https://github.com/hivecommons/hive/issues/18596):

1. **quorum:** the `companion` platform (audio ingest, VAD, transcription,
   reply outbox), the contract and tests.
2. **hive:** the `companion` meeting platform, the Quorum client calls, the
   `/api/companion/*` routes and their documentation.
3. **hive:** the per-user companion token and the hub passthrough for
   `/api/companion/`.
4. **hivecommons/quorum-companion** (new repository): Tauri macOS v1 (call
   detection, consent prompt, ScreenCaptureKit capture of the call app and the
   microphone, streaming, replies window), ad-hoc signed CI builds.
5. **homebrew-hive:** the cask, once the first release exists.

Step 2 depends on step 1's contract; step 3 can land beside it.

## 9. Owner steps

No secrets go in issues or pull requests.

- Nothing to buy for the companion.
- Optional, for macOS permissions that survive updates: create a self-signed
  code-signing certificate and add it to `hivecommons/quorum-companion` as the
  Actions secrets `MACOS_SIGNING_P12` and `MACOS_SIGNING_P12_PASSWORD`. Until
  then releases are ad-hoc signed.
- For phone dial-in later: a telephony account (Twilio or equivalent) and
  meetings hosted on a paid Zoom plan.

## 10. Sources

- Zoom free plan features (dial-in is a paid upgrade, 40-minute limit):
  <https://www.zoom.com/en/products/virtual-meetings/features/free-video-conferencing/>
- Zoom Audio Conferencing (toll dial-in, add-ons):
  <https://www.zoom.com/en/products/virtual-meetings/features/audio-conferencing/>
- Zoom Community, dial-in needs a licensed host:
  <https://community.zoom.com/t5/Meetings/Zoom-Dial-In-says-quot-This-Meeting-Does-Not-Allow-Phone-Dial-In/m-p/34368>
  and <https://community.zoom.com/phone-4/generate-call-in-phone-numbers-for-free-account-81809>
- Joining a Zoom meeting by phone (DTMF order):
  <https://support.zoom.us/hc/en-us/articles/201362663>
- Zoom Conference Room Connector (H.323/SIP, paid add-on):
  <https://explore.zoom.us/en/products/zoom-rooms/room-connector/> and
  <https://library.zoom.com/advanced-enterprise-services/conference-room-connector-explainer/web-controls-and-dialing-procedures>
- Zoom Meeting SDK bot policy and the March 2, 2026 authorization change:
  <https://developers.zoom.us/docs/meeting-sdk/linux/>,
  <https://devforum.zoom.us/t/question-on-linux-sdk-usage-policy/141545>,
  <https://developers.zoom.us/docs/rtms/>
- Zoom Terms of Service (section 7, recording-consent duty):
  <https://www.zoom.com/en/trust/terms/>
- Zoom custom live streaming (Pro or higher, admin setting):
  <https://support.zoom.us/hc/en-us/articles/115001777826-Live-streaming-meetings-or-webinars-using-a-custom-service>
  and <https://it.umn.edu/services-technologies/how-tos/zoom-livestream-youtube-or-custom>
- Twilio US voice pricing (outbound $0.0140/min, Media Streams $0.0044/min,
  number $1.15/month): <https://www.twilio.com/en-us/voice/pricing/us>
- Plaud Desktop (bot-free meeting capture with meeting detection):
  <https://global.plaud.ai/pages/plaud-desktop> and
  <https://support.plaud.ai/hc/en-us/articles/53793763082009-Meeting-detection>
- Apple ScreenCaptureKit (`SCStreamConfiguration`, `captureMicrophone`):
  <https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration>
  and <https://developer.apple.com/documentation/screencapturekit/capturing-screen-content-in-macos>
- Apple Core Audio taps (macOS 14.2 and later):
  <https://developer.apple.com/documentation/coreaudio/capturing-system-audio-with-core-audio-taps>
- Ad-hoc signatures and lost permissions after updates (Apple DTS):
  <https://developer.apple.com/forums/thread/795739>
- Homebrew 5.0.0 (`--no-quarantine` removed, Gatekeeper checks):
  <https://brew.sh/2025/11/12/homebrew-5.0.0>
