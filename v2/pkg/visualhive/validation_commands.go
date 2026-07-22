package visualhive

import (
	"errors"
	"fmt"
	"strings"
)

// ExactHeadValidationCommand requests the managed exact-PR-head Visual Hive verdict.
const ExactHeadValidationCommand = "visual-hive run --ci"

// ResolveValidationCommandArgv binds producer-requested validation to argv
// already installed by Hive. The one canonical Visual Hive request is a
// verifier-owned exact-PR-head check; the local Worker only checks its diff.
// Every other request must exactly match one installed argv rendering.
func ResolveValidationCommandArgv(required []string, configured [][]string) ([][]string, error) {
	result := make([][]string, 0, len(required))
	for _, expected := range required {
		if expected == ExactHeadValidationCommand {
			result = append(result, []string{"git", "diff", "--check"})
			continue
		}
		matched := false
		for _, command := range configured {
			if len(command) > 0 && strings.Join(command, " ") == expected {
				result = append(result, append([]string(nil), command...))
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("verified validation command %q is not the current installed argv", expected)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("normal Worker requires at least one exact installed validation command")
	}
	return result, nil
}
