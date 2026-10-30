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
		`id="pi-mlx-note" class="mlx-note"`,
		`.mlx-note{display:none;margin-bottom:var(--sp-5);font-size:var(--fs-sm);color:var(--text-muted)}`,
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
		if strings.Contains(body, `<div id="pi-mlx-note" style=`) {
			t.Error("MLX help must use tokenized CSS rather than add inline styling debt")
		}
	}
}
