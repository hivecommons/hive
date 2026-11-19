package inbox

// Input methods the Inbox lists but does not connect to (#18747).
//
// These are NOT connectors: nothing here is configured, synced or signed
// in, and no entry may share an id with a connector type. The catalogue is
// served as input_methods on GET /api/inbox and rendered as a quiet row
// under the source chips; the full list is in docs/inbox-input-methods.md.
//
// Adding an entry is one line in inputMethods. Every entry must be a real,
// currently available project or company, checked against its own site, and
// its note must state plain facts: no claim of a partnership or an
// integration. Names are shown as plain text with a link to the project's
// own page.

const (
	// InputWorksToday marks a tool that already enters text like a
	// keyboard, so it reaches the Inbox through the Quick note box.
	InputWorksToday = "works_today"
	// InputComingSoon marks an interface with no public API or export that
	// yields text or speech, so hive cannot read from it yet.
	InputComingSoon = "coming_soon"
)

// Input method kinds.
const (
	InputKindNeural       = "neural"
	InputKindSilentSpeech = "silent-speech"
	InputKindGaze         = "gaze"
	InputKindSwitch       = "switch"
	InputKindVoice        = "voice"
)

// InputMethod is one catalogue entry.
type InputMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is one of the InputKind* values.
	Kind string `json:"kind"`
	// State is InputWorksToday or InputComingSoon.
	State string `json:"state"`
	// Note is one factual sentence about what the project is.
	Note string `json:"note"`
	// URL is the project's own page (https).
	URL string `json:"url"`
	// Featured entries are shown in the Inbox row; the rest are listed in
	// the docs only, to keep the row readable.
	Featured bool `json:"featured,omitempty"`
}

var inputMethods = []InputMethod{
	{ID: "talon", Name: "Talon", Kind: InputKindVoice, State: InputWorksToday, Featured: true, URL: "https://talonvoice.com/", Note: "Voice dictation and commands, eye-tracking pointer control and noise input for macOS, Windows and Linux; free to use, not open source."},
	{ID: "optikey", Name: "OptiKey", Kind: InputKindGaze, State: InputWorksToday, Featured: true, URL: "https://github.com/OptiKey/OptiKey", Note: "Open-source on-screen keyboard for Windows, driven by gaze, a mouse or a webcam."},
	{ID: "acat", Name: "ACAT", Kind: InputKindSwitch, State: InputWorksToday, Featured: true, URL: "https://github.com/intel/acat", Note: "Assistive Context-Aware Toolkit, open source from Intel for Windows: switch and camera input with a scanning keyboard and word prediction."},
	{ID: "macos-switch-control", Name: "macOS Switch Control", Kind: InputKindSwitch, State: InputWorksToday, URL: "https://support.apple.com/guide/mac-help/use-switch-control-mh43607/mac", Note: "Built into macOS: a scanning on-screen keyboard operated by a switch or joystick."},
	{ID: "nerd-dictation", Name: "nerd-dictation", Kind: InputKindVoice, State: InputWorksToday, URL: "https://github.com/ideasman42/nerd-dictation", Note: "Open-source offline dictation for Linux."},
	{ID: "neuralink", Name: "Neuralink", Kind: InputKindNeural, State: InputComingSoon, Featured: true, URL: "https://neuralink.com/", Note: "Implanted brain-computer interface in clinical trials for computer control and speech; the device is investigational."},
	{ID: "synchron", Name: "Synchron", Kind: InputKindNeural, State: InputComingSoon, Featured: true, URL: "https://synchron.com/", Note: "Implanted brain-computer interface in clinical trials; it has been shown controlling an iPad as an accessibility input."},
	{ID: "paradromics", Name: "Paradromics", Kind: InputKindNeural, State: InputComingSoon, Featured: true, URL: "https://www.paradromics.com/", Note: "Implanted brain-computer interface in a clinical study designed to turn brain signals into text, a digital voice or device commands."},
	{ID: "precision-neuroscience", Name: "Precision Neuroscience", Kind: InputKindNeural, State: InputComingSoon, Featured: true, URL: "https://precisionneuro.io/", Note: "Investigational brain-surface electrode array (Layer 7) for controlling devices."},
	{ID: "blackrock-neurotech", Name: "Blackrock Neurotech", Kind: InputKindNeural, State: InputComingSoon, URL: "https://blackrockneurotech.com/", Note: "Implantable electrode arrays used in research; its public research SDK streams raw neural signals, not text."},
	{ID: "brain-to-text-ucdavis", Name: "Brain-to-Text (UC Davis Neuroprosthetics Lab)", Kind: InputKindNeural, State: InputComingSoon, URL: "https://github.com/Neuroprosthetics-Lab/nejm-brain-to-text", Note: "Published research code and data that decode attempted speech into text from implanted electrodes; it runs offline on a recorded dataset."},
	{ID: "speechbci-stanford", Name: "speechBCI (Willett et al.)", Kind: InputKindNeural, State: InputComingSoon, URL: "https://github.com/fwillett/speechBCI", Note: "Published research code and data for decoding attempted speech into text from implanted electrodes; it runs offline on a recorded dataset."},
	{ID: "silent-speech-emg", Name: "silent_speech (Gaddy and Klein)", Kind: InputKindSilentSpeech, State: InputComingSoon, URL: "https://github.com/dgaddy/silent_speech", Note: "Open-source research code that turns facial muscle (EMG) signals from silently mouthed speech into text and audio; it needs an electrode rig."},
	{ID: "alterego", Name: "AlterEgo", Kind: InputKindSilentSpeech, State: InputComingSoon, URL: "https://www.alterego.io/", Note: "Silent-speech wearable; its site offers a waitlist and no public API or SDK."},
}

// InputMethods returns a copy of the catalogue, works-today entries first.
func InputMethods() []InputMethod {
	return append([]InputMethod(nil), inputMethods...)
}
