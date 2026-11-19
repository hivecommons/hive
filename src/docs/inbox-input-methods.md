# Input methods and the Inbox

The Inbox is a web page, so anything that can move a pointer and enter text
in a browser can use it. This page says plainly which input methods reach the
Inbox today, which cannot yet, and why. It backs the quiet row of chips under
the Inbox's source chips ("Works today" and "Neural input (coming soon)").

Hive is not affiliated with any company or project named here. A listing
reflects interest in the input method, not a partnership, an announced
integration or a commitment from the project. Names are shown as plain text
with a link to the project's own page. Facts were checked against each
project's own site or repository on 2026-11-17.

## What the row does

- The chips are **not sources and not connectors**. There is nothing to
  configure, connect or sign in to, and they never appear under Knowledge →
  Integrations, in `knowledge.connectors` or in `inbox.sources`.
- Clicking or focusing a chip shows a short explanation under the row; Escape
  or **Close** hides it. Nothing else happens.
- The row shows a handful of entries; **more** opens this page, which has the
  full list.

## Works today

These tools enter text like a keyboard, or dictate into the focused text box,
so they already reach the Inbox: open **＋ Add** and use
[Quick note](inbox-uploads.md#quick-note). Nothing needs to be set up on the
hive.

| Name | Kind | What it is | Licence |
| --- | --- | --- | --- |
| [Talon](https://talonvoice.com/) | voice, gaze | Voice dictation and commands, eye-tracking pointer control and noise input for macOS, Windows and Linux | free to use, not open source |
| [OptiKey](https://github.com/OptiKey/OptiKey) | gaze | On-screen keyboard for Windows, driven by gaze, a mouse or a webcam | GPL-3.0 |
| [ACAT](https://github.com/intel/acat) (Assistive Context-Aware Toolkit) | switch | Windows toolkit from Intel: switch and camera input with a scanning keyboard and word prediction | Apache-2.0 |
| [macOS Switch Control](https://support.apple.com/guide/mac-help/use-switch-control-mh43607/mac) | switch | Built into macOS: a scanning on-screen keyboard operated by a switch or joystick | part of macOS |
| [nerd-dictation](https://github.com/ideasman42/nerd-dictation) | voice | Offline dictation for Linux | GPL-3.0 |

The operating system's own dictation and any other tool that types into the
focused field work the same way. Speech can also arrive as audio: record in
the dashboard ([Recording into the Inbox](inbox-recording.md)) or drop an
audio file, and the hive transcribes it when
[transcription](inbox-transcription.md) is on.

The Inbox's small controls (item checkbox, small buttons, pills and the
"+N / less" links) answer over an area of at least 40px (`--tap-target`), so
they are easier to hit with gaze, dwell, switch scanning or an imprecise
pointer. Their visible size is unchanged.

## Neural input

Brain-computer interfaces are the reason this page exists, so the facts first:

- **No vendor publishes an API, SDK or export that yields text or speech.**
  No connector can be built today, and hive does not ship a placeholder one.
- People in the clinical trials use these devices to drive an ordinary
  computer (pointer, on-screen keyboard, or a speech app on a laptop). They
  reach the Inbox through the dashboard like anyone else, and Quick note
  takes whatever text entry they already use.
- The devices are investigational. Nothing here is a statement about what
  they can do beyond what each company says on its own site.
- When a project publishes a way to read text or speech, hive will add it as
  a source. Until then it is listed as "coming soon".

### Coming soon / exploring

| Name | Kind | What it is | Why it is not a source yet |
| --- | --- | --- | --- |
| [Neuralink](https://neuralink.com/) | neural | Implanted brain-computer interface in clinical trials for computer control and speech | no public API, SDK or export |
| [Synchron](https://synchron.com/) | neural | Implanted brain-computer interface in clinical trials; it has been shown controlling an iPad as an accessibility input | no public API, SDK or export |
| [Paradromics](https://www.paradromics.com/) | neural | Implanted brain-computer interface in a clinical study designed to turn brain signals into text, a digital voice or device commands | no public API, SDK or export |
| [Precision Neuroscience](https://precisionneuro.io/) | neural | Investigational brain-surface electrode array (Layer 7) for controlling devices | no public API, SDK or export |
| [Blackrock Neurotech](https://blackrockneurotech.com/) | neural | Implantable electrode arrays used in research | its public research SDK ([CereLink](https://github.com/CerebusOSS/CereLink), BSD-2-Clause) streams raw neural signals, not text |
| [Brain-to-Text](https://github.com/Neuroprosthetics-Lab/nejm-brain-to-text) (UC Davis Neuroprosthetics Lab) | neural | Published research code and data that decode attempted speech into text from implanted electrodes | runs offline on a recorded dataset; needs an implant and a lab to produce new text |
| [speechBCI](https://github.com/fwillett/speechBCI) (Willett et al.) | neural | Published research code and data for decoding attempted speech into text from implanted electrodes | runs offline on a recorded dataset |
| [silent_speech](https://github.com/dgaddy/silent_speech) (Gaddy and Klein) | silent speech | Research code (MIT licence) that turns facial muscle (EMG) signals from silently mouthed speech into text and audio | needs an electrode rig; single-speaker research data |
| [AlterEgo](https://www.alterego.io/) | silent speech | Silent-speech wearable | its site offers a waitlist and no public API or SDK |

Open brain-signal toolkits such as [BrainFlow](https://github.com/brainflow-dev/brainflow),
[Lab Streaming Layer](https://github.com/sccn/labstreaminglayer),
[OpenViBE](https://openvibe.inria.fr/), [BCI2000](https://www.bci2000.org/),
[MNE-Python](https://github.com/mne-tools/mne-python) and
[MetaBCI](https://github.com/TBC-TJU/MetaBCI) are not listed in the Inbox:
they stream or analyse raw signals and classified intents, not free text, so
there is nothing for an inbox to read. A speller built on one of them types
on screen like any other keyboard and is covered by "Works today".

## Adding or moving an entry

The catalogue is one Go slice, `inputMethods` in
`src/pkg/inbox/input_methods.go`; `GET /api/inbox` returns it as
`input_methods` (`id`, `name`, `kind`, `state`, `note`, `url`, `featured`).
Adding a project is one line there and one row here.

- `state` is `works_today` or `coming_soon`; `kind` is `neural`,
  `silent-speech`, `gaze`, `switch` or `voice`.
- `featured` puts the entry in the Inbox row (at most 8, so the row stays
  readable); the rest are listed on this page only.
- `note` is one factual sentence, checked against the project's own site. It
  must not suggest a partnership, an integration or an endorsement; a test
  rejects those words.
- `url` is an https link to the project's own page.
- An id must not match a connector type or an Inbox source: an input method
  is never a connector.

When a "coming soon" project publishes an API or export that yields text or
audio, remove it from the catalogue and build a real connector for it
([Inbox and voice sources](inbox-voice-sources.md)).
