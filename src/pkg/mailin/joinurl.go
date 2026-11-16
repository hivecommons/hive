package mailin

import (
	"net/url"
	"regexp"
	"strings"
)

// Platform names, the same strings pkg/meetings uses.
const (
	PlatformGoogleMeet = "google_meet"
	PlatformTeams      = "teams"
	PlatformZoom       = "zoom"
)

// maxURLLen bounds a join link.
const maxURLLen = 2048

// maxSecretLen bounds a native meeting id or passcode.
const maxSecretLen = 64

// urlRE finds https links in free text; joinPlatform then limits them to
// meeting hosts, so nothing else from DESCRIPTION is ever kept.
var urlRE = regexp.MustCompile(`https://[^\s<>"'\x60\\\]\[\)\(]+`)

// Teams and Zoom put the dial-in details in DESCRIPTION as plain lines.
var (
	meetingIDLineRE = regexp.MustCompile(`(?im)^\s*(?:Meeting ID|ID de (?:la )?reuni[oó]n|Besprechungs-ID)\s*:\s*([0-9][0-9 ]{5,30}[0-9])\s*$`)
	passcodeLineRE  = regexp.MustCompile(`(?im)^\s*(?:Passcode|Password|Kenncode|C[oó]digo de acceso)\s*:\s*([A-Za-z0-9._~-]{1,64})\s*$`)
	zoomPathIDRE    = regexp.MustCompile(`^/(?:j|w|s|my)/([0-9]{9,12})$`)
	meetCodeRE      = regexp.MustCompile(`^/([a-z]{3}-[a-z]{4}-[a-z]{3})$`)
	teamsMeetPathRE = regexp.MustCompile(`^/meet/([0-9]{9,20})$`)
)

// joinPlatform names the platform of an https meeting link from its host,
// "" for any other link.
func joinPlatform(link string) string {
	if len(link) > maxURLLen {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return ""
	}
	if p := u.Port(); p != "" && p != "443" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "meet.google.com":
		return PlatformGoogleMeet
	case host == "teams.microsoft.com", host == "teams.live.com":
		return PlatformTeams
	case host == "zoom.us", strings.HasSuffix(host, ".zoom.us"):
		return PlatformZoom
	}
	return ""
}

// JoinDetails is what hive needs to join a meeting.
type JoinDetails struct {
	URL             string
	Platform        string
	NativeMeetingID string
	Passcode        string
}

// cleanLink trims the punctuation free text puts around a link.
func cleanLink(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ".,;:!?>*'\"")
}

// findJoin picks the join link from the conference fields in order, then
// LOCATION, then a host-limited scan of DESCRIPTION, and reads the native
// meeting id and passcode where the platform uses them.
func findJoin(fields []string, location, description string) JoinDetails {
	var d JoinDetails
	for _, f := range fields {
		if l := cleanLink(f); joinPlatform(l) != "" {
			d.URL = l
			break
		}
	}
	if d.URL == "" {
		for _, text := range []string{location, description} {
			for _, m := range urlRE.FindAllString(text, -1) {
				if l := cleanLink(m); joinPlatform(l) != "" {
					d.URL = l
					break
				}
			}
			if d.URL != "" {
				break
			}
		}
	}
	if d.URL == "" {
		return d
	}
	d.Platform = joinPlatform(d.URL)
	u, _ := url.Parse(d.URL)
	switch d.Platform {
	case PlatformZoom:
		if m := zoomPathIDRE.FindStringSubmatch(u.Path); m != nil {
			d.NativeMeetingID = m[1]
		}
		d.Passcode = u.Query().Get("pwd")
	case PlatformTeams:
		if m := teamsMeetPathRE.FindStringSubmatch(u.Path); m != nil {
			d.NativeMeetingID = m[1]
			d.Passcode = u.Query().Get("p")
		}
	case PlatformGoogleMeet:
		if m := meetCodeRE.FindStringSubmatch(u.Path); m != nil {
			d.NativeMeetingID = m[1]
		}
	}
	if d.Platform != PlatformGoogleMeet {
		if d.NativeMeetingID == "" {
			if m := meetingIDLineRE.FindStringSubmatch(description); m != nil {
				d.NativeMeetingID = strings.Join(strings.Fields(m[1]), "")
			}
		}
		if d.Passcode == "" {
			if m := passcodeLineRE.FindStringSubmatch(description); m != nil {
				d.Passcode = m[1]
			}
		}
	}
	d.NativeMeetingID = clipBytes(d.NativeMeetingID, maxSecretLen)
	d.Passcode = clipBytes(d.Passcode, maxSecretLen)
	return d
}

func clipBytes(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
