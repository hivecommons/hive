//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
)

type windowsScheduledTaskDefinition struct {
	Triggers struct {
		Logon []struct {
			Enabled    *bool  `xml:"Enabled"`
			UserID     string `xml:"UserId"`
			Repetition struct {
				Interval          string `xml:"Interval"`
				StopAtDurationEnd bool   `xml:"StopAtDurationEnd"`
			} `xml:"Repetition"`
		} `xml:"LogonTrigger"`
		Time []struct {
			Enabled       *bool  `xml:"Enabled"`
			StartBoundary string `xml:"StartBoundary"`
			Repetition    struct {
				Interval          string `xml:"Interval"`
				StopAtDurationEnd bool   `xml:"StopAtDurationEnd"`
			} `xml:"Repetition"`
		} `xml:"TimeTrigger"`
	} `xml:"Triggers"`
	Principals struct {
		Principal []struct {
			UserID    string `xml:"UserId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principal"`
	} `xml:"Principals"`
	Settings struct {
		Enabled                 *bool  `xml:"Enabled"`
		MultipleInstancesPolicy string `xml:"MultipleInstancesPolicy"`
		StartWhenAvailable      bool   `xml:"StartWhenAvailable"`
		ExecutionTimeLimit      string `xml:"ExecutionTimeLimit"`
		RestartOnFailure        struct {
			Interval string `xml:"Interval"`
			Count    int    `xml:"Count"`
		} `xml:"RestartOnFailure"`
	} `xml:"Settings"`
	Actions struct {
		Exec []struct {
			Command          string `xml:"Command"`
			Arguments        string `xml:"Arguments"`
			WorkingDirectory string `xml:"WorkingDirectory"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

type windowsDaemonServiceManager struct {
	run         func(context.Context, ...string) ([]byte, error)
	currentUser func() (*user.User, error)
}

func newPlatformDaemonServiceManager() daemonServiceManager {
	return &windowsDaemonServiceManager{run: runWindowsServiceCommand, currentUser: user.Current}
}

func platformDaemonServiceName(stateDir string) string {
	return "Hive-Integrated-" + stableDaemonServiceSuffix(stateDir)
}

func windowsDaemonTaskXML(spec daemonServiceSpec, account *user.User) ([]byte, error) {
	if account == nil || strings.TrimSpace(account.Uid) == "" {
		return nil, errors.New("current Windows user SID is unavailable")
	}
	if err := validateDaemonServiceSpec(spec); err != nil {
		return nil, err
	}
	escape := func(value string) string {
		var output bytes.Buffer
		_ = xml.EscapeText(&output, []byte(value))
		return output.String()
	}
	arguments := windowsDaemonArguments(spec)
	description := "Hive production testing scheduler for " + spec.StateDir
	task := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>` + escape(account.Username) + `</Author>
    <Description>` + escape(description) + `</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + escape(account.Uid) + `</UserId>
    </LogonTrigger>
    <TimeTrigger>
      <StartBoundary>` + spec.InstalledAt.Add(-time.Minute).UTC().Format("2006-01-02T15:04:05Z") + `</StartBoundary>
      <Enabled>true</Enabled>
      <Repetition>
        <Interval>PT1M</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + escape(account.Uid) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>255</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + escape(spec.Executable) + `</Command>
      <Arguments>` + escape(arguments) + `</Arguments>
      <WorkingDirectory>` + escape(filepath.Dir(spec.Executable)) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
	encoded := utf16.Encode([]rune(task))
	result := make([]byte, 2+len(encoded)*2)
	result[0], result[1] = 0xff, 0xfe
	for index, value := range encoded {
		binary.LittleEndian.PutUint16(result[2+index*2:], value)
	}
	return result, nil
}

func windowsDaemonArguments(spec daemonServiceSpec) string {
	return strings.Join([]string{
		quoteWindowsCommandLineArg("daemon"),
		quoteWindowsCommandLineArg("--state-dir"),
		quoteWindowsCommandLineArg(spec.StateDir),
		quoteWindowsCommandLineArg("--interval"),
		quoteWindowsCommandLineArg(fmt.Sprintf("%ds", spec.IntervalSeconds)),
		quoteWindowsCommandLineArg("--expected-hive-commit"),
		quoteWindowsCommandLineArg(spec.HiveCommit),
		quoteWindowsCommandLineArg("--expected-executable-sha256"),
		quoteWindowsCommandLineArg(spec.ExecutableSHA256),
		quoteWindowsCommandLineArg("--log-file"),
		quoteWindowsCommandLineArg(filepath.Join(spec.StateDir, "integrated", "daemon.log")),
	}, " ")
}

func quoteWindowsCommandLineArg(value string) string {
	if value != "" && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == '"' }) {
		return value
	}
	var result strings.Builder
	result.WriteByte('"')
	backslashes := 0
	for _, r := range value {
		if r == '\\' {
			backslashes++
			continue
		}
		if r == '"' {
			result.WriteString(strings.Repeat("\\", backslashes*2+1))
			result.WriteRune(r)
			backslashes = 0
			continue
		}
		result.WriteString(strings.Repeat("\\", backslashes))
		backslashes = 0
		result.WriteRune(r)
	}
	result.WriteString(strings.Repeat("\\", backslashes*2))
	result.WriteByte('"')
	return result.String()
}

func (manager *windowsDaemonServiceManager) Install(ctx context.Context, spec daemonServiceSpec) error {
	account, err := manager.currentUser()
	if err != nil {
		return fmt.Errorf("resolve current Windows user: %w", err)
	}
	definition, err := windowsDaemonTaskXML(spec, account)
	if err != nil {
		return err
	}
	definitionPath := filepath.Join(spec.StateDir, "integrated", "daemon-task.xml")
	if err := os.MkdirAll(filepath.Dir(definitionPath), 0o700); err != nil {
		return fmt.Errorf("create Windows scheduled task definition directory: %w", err)
	}
	if err := os.WriteFile(definitionPath, definition, 0o600); err != nil {
		return fmt.Errorf("write Windows scheduled task definition: %w", err)
	}
	if output, err := manager.run(ctx, "/Create", "/TN", spec.Name, "/XML", definitionPath, "/F"); err != nil {
		_ = os.Remove(definitionPath)
		return serviceCommandError("register Windows scheduled task", output, err)
	}
	return nil
}

func (manager *windowsDaemonServiceManager) Start(ctx context.Context, spec daemonServiceSpec) error {
	if output, err := manager.run(ctx, "/Run", "/TN", spec.Name); err != nil {
		return serviceCommandError("start Windows scheduled task", output, err)
	}
	return nil
}

func (manager *windowsDaemonServiceManager) Inspect(ctx context.Context, spec daemonServiceSpec) (daemonServiceStatus, error) {
	status := daemonServiceStatus{
		SchemaVersion: daemonServiceSchema, Platform: "windows", Name: spec.Name,
		StartupMode:    "interactive-user-logon",
		DefinitionPath: filepath.Join(spec.StateDir, "integrated", "daemon-task.xml"),
		HiveCommit:     spec.HiveCommit, ExecutableSHA256: spec.ExecutableSHA256,
	}
	output, exists, err := manager.query(ctx, spec)
	if err != nil {
		return status, err
	}
	if !exists {
		return status, nil
	}
	status.Installed = true
	account, err := manager.currentUser()
	if err != nil {
		return status, fmt.Errorf("resolve current Windows user for service inspection: %w", err)
	}
	definition, err := parseWindowsScheduledTask(output)
	if err != nil {
		return status, fmt.Errorf("parse registered Windows scheduled task: %w", err)
	}
	if err := validateWindowsScheduledTask(definition, spec, account); err != nil {
		return status, err
	}
	status.Enabled = windowsTaskBoolDefaultTrue(definition.Settings.Enabled)
	// InteractiveToken deliberately reuses the signed-in user's GitHub and
	// Codex credential stores without persisting a Windows password. It resumes
	// at user logon, but cannot truthfully claim unattended pre-login reboot
	// persistence.
	status.RebootPersistent = false
	status.LoginPersistent = true
	status.RestartOnFailure = true
	return status, nil
}

func (manager *windowsDaemonServiceManager) query(ctx context.Context, spec daemonServiceSpec) ([]byte, bool, error) {
	output, err := manager.run(ctx, "/Query", "/TN", spec.Name, "/XML")
	if err == nil {
		return output, true, nil
	}
	if windowsScheduledTaskMissing(output) {
		return nil, false, nil
	}
	return nil, false, serviceCommandError("inspect Windows scheduled task", output, err)
}

func parseWindowsScheduledTask(data []byte) (windowsScheduledTaskDefinition, error) {
	text, err := decodeWindowsTaskXML(data)
	if err != nil {
		return windowsScheduledTaskDefinition{}, err
	}
	for _, encoding := range []string{"UTF-16", "utf-16", "UTF-16LE", "utf-16le"} {
		text = strings.Replace(text, `encoding="`+encoding+`"`, `encoding="UTF-8"`, 1)
	}
	var definition windowsScheduledTaskDefinition
	if err := xml.Unmarshal([]byte(text), &definition); err != nil {
		return windowsScheduledTaskDefinition{}, err
	}
	return definition, nil
}

func decodeWindowsTaskXML(data []byte) (string, error) {
	if len(data) >= 2 && ((data[0] == 0xff && data[1] == 0xfe) || (data[0] == 0xfe && data[1] == 0xff)) {
		if (len(data)-2)%2 != 0 {
			return "", errors.New("UTF-16 task XML has an odd byte count")
		}
		littleEndian := data[0] == 0xff
		units := make([]uint16, (len(data)-2)/2)
		for index := range units {
			if littleEndian {
				units[index] = binary.LittleEndian.Uint16(data[2+index*2:])
			} else {
				units[index] = binary.BigEndian.Uint16(data[2+index*2:])
			}
		}
		return string(utf16.Decode(units)), nil
	}
	if len(data) >= 4 && data[1] == 0 && data[3] == 0 {
		if len(data)%2 != 0 {
			return "", errors.New("UTF-16LE task XML has an odd byte count")
		}
		units := make([]uint16, len(data)/2)
		for index := range units {
			units[index] = binary.LittleEndian.Uint16(data[index*2:])
		}
		return string(utf16.Decode(units)), nil
	}
	return string(data), nil
}

func validateWindowsScheduledTask(definition windowsScheduledTaskDefinition, spec daemonServiceSpec, account *user.User) error {
	if account == nil || strings.TrimSpace(account.Uid) == "" {
		return errors.New("current Windows user SID is unavailable")
	}
	if len(definition.Triggers.Logon) != 1 || !windowsTaskBoolDefaultTrue(definition.Triggers.Logon[0].Enabled) || !windowsTaskUserMatches(definition.Triggers.Logon[0].UserID, account) {
		return fmt.Errorf("registered Windows task logon/recovery trigger drifted from Hive's user binding (triggers=%+v expected_user=%s)", definition.Triggers.Logon, account.Uid)
	}
	if len(definition.Triggers.Time) != 1 || !windowsTaskBoolDefaultTrue(definition.Triggers.Time[0].Enabled) || !windowsTaskBoundaryMatches(definition.Triggers.Time[0].StartBoundary, spec.InstalledAt.Add(-time.Minute)) || definition.Triggers.Time[0].Repetition.Interval != "PT1M" {
		return fmt.Errorf("registered Windows task heartbeat trigger drifted from Hive's immediate crash-recovery policy (triggers=%+v)", definition.Triggers.Time)
	}
	if len(definition.Principals.Principal) != 1 || !windowsTaskUserMatches(definition.Principals.Principal[0].UserID, account) || definition.Principals.Principal[0].LogonType != "InteractiveToken" || (definition.Principals.Principal[0].RunLevel != "" && definition.Principals.Principal[0].RunLevel != "LeastPrivilege") {
		return fmt.Errorf("registered Windows task principal drifted from Hive's least-privilege user binding (principals=%+v)", definition.Principals.Principal)
	}
	settings := definition.Settings
	if settings.MultipleInstancesPolicy != "IgnoreNew" || !settings.StartWhenAvailable || settings.ExecutionTimeLimit != "PT0S" || settings.RestartOnFailure.Interval != "PT1M" || settings.RestartOnFailure.Count != 255 {
		return errors.New("registered Windows task restart settings drifted from Hive's persistent service policy")
	}
	if len(definition.Actions.Exec) != 1 {
		return errors.New("registered Windows task does not have exactly one Hive action")
	}
	action := definition.Actions.Exec[0]
	if !strings.EqualFold(filepath.Clean(action.Command), filepath.Clean(spec.Executable)) || action.Arguments != windowsDaemonArguments(spec) || !strings.EqualFold(filepath.Clean(action.WorkingDirectory), filepath.Clean(filepath.Dir(spec.Executable))) {
		return errors.New("registered Windows task action drifted from the exact Hive executable and repository state binding")
	}
	return nil
}

func windowsTaskBoolDefaultTrue(value *bool) bool {
	return value == nil || *value
}

func windowsTaskUserMatches(value string, account *user.User) bool {
	return account != nil && (strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(account.Uid)) || strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(account.Username)))
}

func windowsTaskBoundaryMatches(value string, expected time.Time) bool {
	actual, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		actual, err = time.ParseInLocation("2006-01-02T15:04:05", strings.TrimSpace(value), time.Local)
	}
	if err != nil {
		return false
	}
	delta := actual.Sub(expected.UTC().Truncate(time.Second))
	return delta >= -time.Second && delta <= time.Second
}

func windowsScheduledTaskMissing(output []byte) bool {
	message := strings.ToLower(strings.TrimSpace(string(output)))
	return strings.Contains(message, "cannot find the file specified") || strings.Contains(message, "cannot find the task") || strings.Contains(message, "the specified task name")
}

func (manager *windowsDaemonServiceManager) Uninstall(ctx context.Context, spec daemonServiceSpec) error {
	_, exists, err := manager.query(ctx, spec)
	if err != nil || !exists {
		return err
	}
	if output, disableErr := manager.run(ctx, "/Change", "/TN", spec.Name, "/Disable"); disableErr != nil {
		return serviceCommandError("disable Windows scheduled task", output, disableErr)
	}
	// /End may report that a registered task is not currently running. Deletion
	// is the authoritative prevention of future logon/failure restarts.
	_, _ = manager.run(ctx, "/End", "/TN", spec.Name)
	if output, deleteErr := manager.run(ctx, "/Delete", "/TN", spec.Name, "/F"); deleteErr != nil {
		return serviceCommandError("delete Windows scheduled task", output, deleteErr)
	}
	_ = os.Remove(filepath.Join(spec.StateDir, "integrated", "daemon-task.xml"))
	return nil
}

func runWindowsServiceCommand(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "schtasks.exe", arguments...)
	command.Stdin = nil
	command.Env = daemonEnvironment(os.Environ())
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
	return command.CombinedOutput()
}
