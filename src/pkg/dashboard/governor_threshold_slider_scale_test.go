package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGovernorThresholdScaleHelpersMatchGaugeAndSlider(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — threshold scale helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		jsFunc(t, html, "governorTrackMax"),
		jsFunc(t, html, "governorPct"),
		jsFunc(t, html, "governorSegmentBoundaries"),
		jsFunc(t, html, "governorPressureBarBoundaries"),
		jsFunc(t, html, "governorThresholdSliderBoundaries"),
		jsFunc(t, html, "governorScaleThreshold"),
	}, "\n") + `
function assertEqual(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(name + ': got ' + g + ', want ' + w);
}
const linear7 = {
  quiet: governorScaleThreshold(2, 7, 'linear'),
  busy: governorScaleThreshold(5, 7, 'linear'),
  surge: governorScaleThreshold(10, 7, 'linear'),
};
const gaugeLinear = governorPressureBarBoundaries(linear7, 40);
const sliderLinear = governorThresholdSliderBoundaries(linear7, 40);
assertEqual('linear seven repos', sliderLinear, gaugeLinear);
assertEqual('linear boundaries', sliderLinear, {max:80, quietPct:17.5, busyPct:43.75, surgePct:87.5, pressurePct:50});

const oneRepo = {quiet: governorScaleThreshold(2, 1, 'none'), busy: governorScaleThreshold(5, 1, 'none'), surge: governorScaleThreshold(10, 1, 'none')};
const gaugeOne = governorPressureBarBoundaries(oneRepo, 0);
const sliderOne = governorThresholdSliderBoundaries(oneRepo, 0);
assertEqual('one repo scaling off', sliderOne, gaugeOne);
assertEqual('one-repo boundaries', sliderOne, {max:20, quietPct:10, busyPct:25, surgePct:50, pressurePct:0});
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node threshold helper test failed: %v\n%s", err, out)
	}
}

func TestGovernorThresholdLabelStaggerAvoidsOverlap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — threshold label stagger helper was NOT executed by this run")
	}
	html := indexHTML(t)
	script := jsFunc(t, html, "governorStaggerLabels") + `
const labels = governorStaggerLabels([
  {key:'quiet', pct:2},
  {key:'busy', pct:5},
  {key:'surge', pct:8},
  {key:'max', pct:11},
], 6);
if (labels.length !== 4) throw new Error('missing labels: '+JSON.stringify(labels));
if (new Set(labels.map(l => l.lane)).size < 2) throw new Error('close labels were not staggered: '+JSON.stringify(labels));
for (let i = 0; i < labels.length; i++) {
  for (let j = i + 1; j < labels.length; j++) {
    if (labels[i].lane === labels[j].lane && Math.abs(labels[i].pct - labels[j].pct) < 6) {
      throw new Error('overlap on lane '+labels[i].lane+': '+JSON.stringify(labels));
    }
  }
}
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node threshold label stagger test failed: %v\n%s", err, out)
	}
}

func TestGovernorThresholdSliderDoesNotUseHardCodedMax(t *testing.T) {
	html := indexHTML(t)
	if strings.Contains(html, "const THRESH_BAR_MAX = 200") {
		t.Fatal("threshold slider still defines a hard-coded 200 track max")
	}
	for _, id := range []string{"thresh-in-quiet", "thresh-in-busy", "thresh-in-surge"} {
		idx := strings.Index(html, `id="`+id+`"`)
		if idx < 0 {
			t.Fatalf("threshold slider input %q missing", id)
		}
		start := idx - 220
		if start < 0 {
			start = 0
		}
		end := idx + 220
		if end > len(html) {
			end = len(html)
		}
		if strings.Contains(html[start:end], `max="200"`) {
			t.Fatalf("threshold slider input %q still hard-codes max=\"200\"", id)
		}
	}
	if strings.Contains(html, "(pct / 100) * 200") || strings.Contains(html, "/ 200) * 100") {
		t.Fatal("threshold slider still converts between values and percentages with a hard-coded 200")
	}
}
