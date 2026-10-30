package agent

import "strings"

const codexDisableUpdateCheckLine = "check_for_update_on_startup = false"

func codexConfigWithUpdateCheckDisabled(existing []byte) []byte {
	if len(existing) == 0 {
		return []byte(codexDisableUpdateCheckLine + "\n")
	}

	lines := strings.SplitAfter(string(existing), "\n")
	tableIndex := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			tableIndex = i
			break
		}
	}

	for i := 0; i < tableIndex; i++ {
		if !isCodexUpdateCheckSetting(lines[i]) {
			continue
		}
		lines[i] = codexDisableUpdateCheckLine + lineEnding(lines[i])
		return []byte(strings.Join(lines, ""))
	}

	if tableIndex < len(lines) {
		merged := make([]string, 0, len(lines)+1)
		merged = append(merged, lines[:tableIndex]...)
		merged = append(merged, codexDisableUpdateCheckLine+"\n")
		merged = append(merged, lines[tableIndex:]...)
		return []byte(strings.Join(merged, ""))
	}

	text := strings.Join(lines, "")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return []byte(text + codexDisableUpdateCheckLine + "\n")
}

func isCodexUpdateCheckSetting(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	key, rest, ok := strings.Cut(trimmed, "=")
	return ok && strings.TrimSpace(key) == "check_for_update_on_startup" && strings.TrimSpace(rest) != ""
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return ""
}
