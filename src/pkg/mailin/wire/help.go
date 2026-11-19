package wire

import "strings"

// Help for a mail that was refused or held (hivecommons/hive#18744): what
// the sender or the hive's owner does to get it through, and where the
// outcome is explained. The same text is in the SMTP refusal (which the
// sender's provider returns as a bounce) and next to the mail in the hive's
// Inbox. Hive sends no mail of its own, so these are the only two places a
// person learns what happened.

// Outcomes that are not refusals of the message's content, as help topics.
const (
	// ReasonUnknownMailbox: the address is not hive@<a hive's host>.
	ReasonUnknownMailbox = "unknown_mailbox"
	// ReasonNotAuthenticated: the sender did not pass DMARC alignment, so
	// the mail awaits the owner's approval.
	ReasonNotAuthenticated = "sender_not_authenticated"
	// ReasonNotOnAllowlist: the sender is authenticated but not a proven
	// calendar email of a user of the hive, so the mail awaits approval.
	ReasonNotOnAllowlist = "sender_not_on_allowlist"
)

const (
	// HelpPageURL is the docs section "Mailing your hive: what the replies
	// mean"; each reason has a heading there whose anchor is the reason with
	// dashes for underscores.
	HelpPageURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/inbox-voice-sources.md"
	// helpPageAnchor is the section itself, for a reason without a heading.
	helpPageAnchor = "mailing-your-hive-what-the-replies-mean"
	// ReportURL is where a sender who has no hive dashboard reports a problem.
	ReportURL = "https://github.com/hivecommons/hive/issues/new/choose"
)

// fixes says how to try again, per reason. Each has a heading in the docs
// section (TestHelpAnchorsAreInTheDocs keeps the two in step).
var fixes = map[string]string{
	ReasonEmpty:              "add text, a calendar invite or a file, and send it again",
	ReasonUnsupported:        "send the file as an accepted type (export it to PDF, for example)",
	ReasonContentMismatch:    "save the file again in its own format, without macros, and send it again",
	ReasonAttachmentTooLarge: "send a smaller file, or split the files over several mails",
	ReasonTooManyAttachments: "send the files over several mails",
	ReasonMessageTooLarge:    "send a smaller file, or split the files over several mails",
	ReasonMalformed:          "send it again from another mail program, and report a problem if it keeps failing",
	ReasonHiveVolume:         "nothing to change: the sending mail server retries by itself, or send it again tomorrow",
	ReasonHiveRefused:        "ask the hive's owner to check that mail-in is turned on and its mail storage has room, then send it again",
	ReasonMailInOff:          "the hive's owner turns mail-in files back on, then send it again",
	ReasonHiveStorageFull:    "the hive's owner declines or deletes mailed-in items to free space, then send it again",
	ReasonUnknownMailbox:     "send to " + LocalPart + "@ followed by the hive's dashboard host, and nothing else",
	ReasonNotAuthenticated:   "send from the mail provider's own app or web mail, or fix the account's outgoing server; the hive's owner can approve this one by hand",
	ReasonNotOnAllowlist:     "add the address under Settings, Meeting attendee, Email and prove it; the hive's owner can approve this one by hand",
}

// HelpReasons lists the reasons that have a fix and a docs heading.
func HelpReasons() []string {
	out := make([]string, 0, len(fixes))
	for r := range fixes {
		out = append(out, r)
	}
	return out
}

// Fix says how to try again after reason ("" for a reason without advice).
func Fix(reason string) string { return fixes[reason] }

// HelpAnchor is the docs heading anchor of reason.
func HelpAnchor(reason string) string {
	if _, ok := fixes[reason]; !ok {
		return helpPageAnchor
	}
	return strings.ReplaceAll(reason, "_", "-")
}

// HelpURL is the docs page section that explains reason.
func HelpURL(reason string) string { return HelpPageURL + "#" + HelpAnchor(reason) }

// HelpLines are the lines an SMTP refusal adds after its reason: the fix,
// the help page and where to report a problem. They say nothing about the
// hive or its users, so they are the same for every sender.
func HelpLines(reason string) []string {
	var lines []string
	if fix := Fix(reason); fix != "" {
		lines = append(lines, "To try again: "+fix+".")
	}
	return append(lines, "What this means: "+HelpURL(reason), "Report a problem: "+ReportURL)
}
