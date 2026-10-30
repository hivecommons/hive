package dashboard

import (
	"strings"
	"testing"
)

func TestContributeAppleMLXOnboarding(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		`<option value="mlx"`,
		`id="apple-mlx"`,
		`mlx:{name:'Apple MLX'`,
		`mlx:'pi'`,
		`contribute-mlx-serve`,
		`contribute-mlx-setup`,
		`--verify-tools`,
		`just contribute-mlx`,
		`Docker Desktop 4.34+`,
		`Intel Macs and Rosetta are unsupported`,
		`Model field`,
		`not Apple endorsement`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Apple MLX onboarding missing %q", want)
		}
	}
}
