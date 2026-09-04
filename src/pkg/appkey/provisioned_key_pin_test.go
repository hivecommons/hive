package appkey

import (
	"os"
	"path/filepath"
	"testing"
)

// testRSAPrivateKeyPEM is a throwaway key generated for this test only — it
// authenticates nothing. resolveAppKeyFile gates on a parseable fingerprint,
// so the bytes must be a real RSA key rather than a placeholder string.
const testRSAPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDUE14M+R+F+Rd7
aq9Qb9gQEVX7hbZm8/ujygzB7ztu7Sx+KZhj8gYs4HppK4B/Q5xBQxwWV+0/WM8q
hnn3x2Ycw8Wiz8B2C8nGvdhkEs8En0DOdoEZ8mi3XNZNwFuyTkFr9vDdVT9hlv9i
9A6Qdc9ozr1MdHxwmca8RG5xWUj6P1G2UtpwACFGqFfnrNCT5w1dQImumCiidCaP
paEmmgg4X1CElz3Z9KhWiaLAaZHaCuj5Km3tqn0xwDvSRO+tB2d9GOphW7sq85Gt
yWVytUYEVkdL0x94rhJ+HmLY12oYJQj1txr1SMd6NHKmSQuI95nEgNdEMQgoJquD
i397C7sLAgMBAAECggEAI17pkEtalRsy7ewguk81H5TsnMsz3V7zCOHRl+ThKkKX
aaFhX8YFfqWf9PuC7nbl0EKzpAxdLvQOdV7BZ/CTWNfUFjAFPwr/R8zxEtvKOFCh
W+4K4Tt7eJ2cxpH/GTGRGsMwcBHgRNQM20GuTiy//5B/pQlGmfcj3NGjA/eqwsXP
ucI/A+N/xwDcdnqiHZ25w1CBiMZatQaoeHP19Qktr7nlmVG2Udq0Y3/zZcZwE5fc
2k6La4yGNVVyKWzb0IZqwSnwtRkmmCKJrq3ECZVDrzBve4jST5z9x1WjzOnnpt7U
caBSKg1E3K2VZp0rM5t4gQkn7vNPYy4UFMeGi/wR3QKBgQD/n89V1LZHieqmUrV9
8O8IkZRt/E0HBtnQw2wJSt29TMdBZ8xm7Rs5RqBDFtOBuLZJyshhFGu8zWBpH+0O
SMBxYqAPiGOFGpB3m6NH8aFjhRkzfSDQpv9NtDFsrNac/+Bq9b57i/QzUUE5QXz8
Mdk0NDgoCWKGTYnZsH3u0TjQlQKBgQDUYyudGfuBH/6j3HGfnVcBFR0C4Cpgurtt
vimWVgkGf7pc3Tik7qD1+OqPD4dVuV4SxST8sqYr3H7YByMwoG2HWFJUsV5VKPbM
DSxXEGrB8X5hyY9tJoFhxSZT1Hyi91nFOwgwz2Y9bzCnpF74eakFceRN2AwtgShy
n2JpIQVVHwKBgQC08TRcNyOH5BIbBXS+3yr0T8hXSj5j+O95nLr+oOXwt0Zb/9Nq
D/AzTNDobGHu8wblmQrZ3RCeJmpWP2kXsVu3Zu6R0CNR9onIgHzF0j5BKde64Jm3
2F3jbOeHW5jWrTD3xVe+MET9hki69KY6BjcPgt81R99b3cr0MsARqjujOQKBgCc4
IOej0qOnitgrbvfwkA5tHaxYRLsUAGRlhzxxqrz+fSWE3F7oieSiEH5WecFEt7Bz
oz7epnzW/L1bpA3oshEaKCnnjune5KQNkrCJIY2q0JGyLMAVKjMpusgkJtfZIUSg
gASzZ8fUboGmgrsTjDirLWOKj8UfYp63++454Mg1AoGAYh6Tkh8RxlKzBWaGBg5r
D9ORLzfPZui0jADyXec18icdJSWgXhIhC6p1rb1SSAMsxsQ51AOR0unIeQ/eYHnU
wdpaOjVWzXbzKMptPWdh5tBPOK9gIlyH3ppOAVgAWUjuWOJoIkS+fQ3OjP3AEF2U
JGT4DUmsTBF8RD1/Aqff5Hc=
-----END PRIVATE KEY-----`

// writeTestAppKey writes a syntactically valid RSA private key so
// AppKeyFingerprintFromFile accepts it — resolveAppKeyFile requires a
// fingerprint, not mere existence, before preferring a per-app-id file.
func writeTestAppKey(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(testRSAPrivateKeyPEM), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestResolveAppKeyFileProvisionedGenericPinYieldsToPerAppID is the spoke half
// of the a-ks-wec2 outage.
//
// The provisioning template hardcodes key_file: /secrets/gh-app-key.pem for
// every App-using hive (saas_provision.go). That value is written by the
// PLATFORM, not chosen by an operator, and it does not name the App it holds —
// on the a-ks-wec2 pool it held a key matching NEITHER real App. While
// resolveAppKeyFile treated it as an explicit override, it short-circuited
// per-app-id selection, so the hub could correct app_id forever and the spoke
// would still sign with the wrong key and get "404 Integration not found".
func TestResolveAppKeyFileProvisionedGenericPinYieldsToPerAppID(t *testing.T) {
	dir := isolateAppKeyLookup(t)

	origDir := testKeys.DataDir
	testKeys.DataDir = dir
	t.Cleanup(func() { testKeys.DataDir = origDir })

	const appID int64 = 5686
	perApp := filepath.Join(dir, "gh-app-key-5686.pem")
	writeTestAppKey(t, perApp)

	// Exactly what provisioning writes into hive.yaml.
	got := testKeys.Resolve(testKeys.ProvisionedKeyPath, "", appID)
	if got != perApp {
		t.Errorf("testKeys.Resolve(%q) = %q, want the per-app-id key %q — "+
			"the provisioned generic pin must not block per-app-id selection",
			testKeys.ProvisionedKeyPath, got, perApp)
	}
}

// TestResolveAppKeyFileRealOperatorOverrideStillWins is the guard: only the two
// PLATFORM-WRITTEN generic paths yield. A path an operator actually typed (a
// hive on a third App with a bespoke key location) must still win outright.
func TestResolveAppKeyFileRealOperatorOverrideStillWins(t *testing.T) {
	dir := isolateAppKeyLookup(t)

	origDir := testKeys.DataDir
	testKeys.DataDir = dir
	t.Cleanup(func() { testKeys.DataDir = origDir })

	const appID int64 = 5686
	writeTestAppKey(t, filepath.Join(dir, "gh-app-key-5686.pem"))

	bespoke := filepath.Join(dir, "operator-chosen.pem")
	writeTestAppKey(t, bespoke)

	if got := testKeys.Resolve(bespoke, "", appID); got != bespoke {
		t.Errorf("testKeys.Resolve(%q) = %q, want the operator's own path", bespoke, got)
	}
}

// TestResolveAppKeyFileProvisionedPinKeptWithoutPerAppKey makes sure the change
// cannot BLANK a working setup: with no per-app-id key on disk, the provisioned
// path is still the best answer available and must be returned unchanged.
func TestResolveAppKeyFileProvisionedPinKeptWithoutPerAppKey(t *testing.T) {
	dir := isolateAppKeyLookup(t)

	origDir := testKeys.DataDir
	testKeys.DataDir = dir
	t.Cleanup(func() { testKeys.DataDir = origDir })

	if got := testKeys.Resolve(testKeys.ProvisionedKeyPath, "", 5686); got != testKeys.ProvisionedKeyPath {
		t.Errorf("resolveAppKeyFile = %q, want %q kept when no per-app-id key exists",
			got, testKeys.ProvisionedKeyPath)
	}
}
