package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

const (
	CadenceModeInterval  = "interval"
	CadenceModeTimes     = "times"
	CadenceModeCron      = "cron"
	CadenceCatchUpWindow = 10 * time.Minute
)

const cadenceObjectPrefix = "tod:"

var cadenceParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

type Cadence string

type cadenceObject struct {
	Interval string   `yaml:"interval,omitempty" json:"interval,omitempty"`
	Times    []string `yaml:"times,omitempty" json:"times,omitempty"`
	Days     []string `yaml:"days,omitempty" json:"days,omitempty"`
	TZ       string   `yaml:"tz,omitempty" json:"tz,omitempty"`
	Cron     string   `yaml:"cron,omitempty" json:"cron,omitempty"`
}

func NewIntervalCadence(interval string) Cadence { return Cadence(strings.TrimSpace(interval)) }

func (c Cadence) object() cadenceObject {
	raw := string(c)
	if !strings.HasPrefix(raw, cadenceObjectPrefix) {
		return cadenceObject{Interval: strings.TrimSpace(raw)}
	}
	var obj cadenceObject
	_ = json.Unmarshal([]byte(strings.TrimPrefix(raw, cadenceObjectPrefix)), &obj)
	obj.Interval = strings.TrimSpace(obj.Interval)
	obj.TZ = strings.TrimSpace(obj.TZ)
	obj.Cron = strings.TrimSpace(obj.Cron)
	return obj
}

func (c Cadence) Mode() string {
	obj := c.object()
	if obj.Cron != "" {
		return CadenceModeCron
	}
	if len(obj.Times) > 0 {
		return CadenceModeTimes
	}
	return CadenceModeInterval
}

func (c Cadence) Interval() string { return c.object().Interval }

func (c Cadence) IsPaused() bool {
	if c.Mode() != CadenceModeInterval {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.Interval())) {
	case "pause", "paused", "off", "0", "on demand":
		return true
	default:
		return false
	}
}

func (c Cadence) String() string {
	if c.Mode() == CadenceModeInterval {
		return c.Interval()
	}
	b, err := yaml.Marshal(c.object())
	if err != nil {
		return string(c)
	}
	return strings.TrimSpace(string(b))
}

func (c Cadence) MarshalYAML() (interface{}, error) {
	if c.Mode() == CadenceModeInterval {
		return c.Interval(), nil
	}
	return c.object(), nil
}

func (c *Cadence) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*c = NewIntervalCadence(value.Value)
		return nil
	}
	var obj cadenceObject
	if err := value.Decode(&obj); err != nil {
		return err
	}
	next, err := cadenceFromObject(obj)
	if err != nil {
		return err
	}
	*c = next
	return nil
}

func (c Cadence) MarshalJSON() ([]byte, error) {
	if c.Mode() == CadenceModeInterval {
		return json.Marshal(c.Interval())
	}
	return json.Marshal(c.object())
}

func (c *Cadence) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = NewIntervalCadence(s)
		return nil
	}
	var obj cadenceObject
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	next, err := cadenceFromObject(obj)
	if err != nil {
		return err
	}
	*c = next
	return nil
}

func cadenceFromObject(obj cadenceObject) (Cadence, error) {
	obj.Interval = strings.TrimSpace(obj.Interval)
	obj.TZ = strings.TrimSpace(obj.TZ)
	obj.Cron = strings.TrimSpace(obj.Cron)
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	c := Cadence(cadenceObjectPrefix + string(b))
	return c, c.Validate()
}

func (c Cadence) Validate() error {
	obj := c.object()
	hasInterval := obj.Interval != ""
	hasTimes := len(obj.Times) > 0
	hasCron := obj.Cron != ""
	modes := 0
	if hasInterval {
		modes++
	}
	if hasTimes {
		modes++
	}
	if hasCron {
		modes++
	}
	if modes > 1 {
		return fmt.Errorf("cadence must set exactly one of interval, times, or cron")
	}
	if modes == 0 && (strings.HasPrefix(string(c), cadenceObjectPrefix) || obj.TZ != "" || len(obj.Days) > 0) {
		return fmt.Errorf("cadence object must set interval, non-empty times, or cron")
	}
	if !hasTimes && !hasCron {
		return nil
	}
	if obj.TZ == "" {
		return fmt.Errorf("cadence timezone (tz) is required for times or cron")
	}
	if _, err := time.LoadLocation(obj.TZ); err != nil {
		return fmt.Errorf("invalid cadence timezone %q: %w", obj.TZ, err)
	}
	if _, err := normalizedDays(obj.Days); err != nil {
		return err
	}
	if hasTimes {
		for _, tod := range obj.Times {
			if _, _, err := parseTimeOfDay(tod); err != nil {
				return err
			}
		}
		for _, spec := range c.cronSpecs() {
			if _, err := cadenceParser.Parse(spec); err != nil {
				return fmt.Errorf("invalid cadence time schedule %q: %w", spec, err)
			}
		}
		return nil
	}
	if _, err := cadenceParser.Parse(c.cronSpec(obj.Cron)); err != nil {
		return fmt.Errorf("invalid cadence cron %q: %w", obj.Cron, err)
	}
	return nil
}

func (c Cadence) NextAfter(after time.Time) (time.Time, bool) {
	if err := c.Validate(); err != nil {
		return time.Time{}, false
	}
	if c.Mode() == CadenceModeInterval {
		d, err := time.ParseDuration(c.Interval())
		if err != nil || d <= 0 {
			return time.Time{}, false
		}
		return after.Add(d), true
	}
	var best time.Time
	for _, spec := range c.cronSpecs() {
		schedule, err := cadenceParser.Parse(spec)
		if err != nil {
			continue
		}
		next := schedule.Next(after)
		if best.IsZero() || next.Before(best) {
			best = next
		}
	}
	return best, !best.IsZero()
}

func (c Cadence) DueOccurrence(lastKick time.Time, now time.Time, catchUpWindow time.Duration) (time.Time, bool) {
	if c.Mode() == CadenceModeInterval {
		next, ok := c.NextAfter(lastKick)
		if lastKick.IsZero() {
			return now, ok
		}
		return next, ok && !next.After(now)
	}
	if catchUpWindow <= 0 {
		catchUpWindow = CadenceCatchUpWindow
	}
	cursor := now.Add(-catchUpWindow).Add(-time.Nanosecond)
	for {
		next, ok := c.NextAfter(cursor)
		if !ok || next.After(now) {
			return time.Time{}, false
		}
		if lastKick.IsZero() || next.After(lastKick) {
			return next, true
		}
		cursor = next
	}
}

func (c Cadence) HumanSummary() string {
	obj := c.object()
	switch c.Mode() {
	case CadenceModeTimes:
		parts := make([]string, 0, len(obj.Times))
		for _, tod := range obj.Times {
			h, m, err := parseTimeOfDay(tod)
			if err != nil {
				parts = append(parts, tod)
				continue
			}
			parts = append(parts, time.Date(2000, 1, 1, h, m, 0, 0, time.UTC).Format("3:04 PM"))
		}
		return fmt.Sprintf("At %s, %s (%s)", joinHuman(parts), daysSummary(obj.Days), obj.TZ)
	case CadenceModeCron:
		return fmt.Sprintf("Cron %q (%s)", obj.Cron, obj.TZ)
	default:
		return obj.Interval
	}
}

func (c Cadence) ShortLabel(after time.Time) string {
	obj := c.object()
	switch c.Mode() {
	case CadenceModeTimes:
		tz := c.TimezoneAbbrev(after)
		if tz == "" {
			tz = obj.TZ
		}
		return "🕘 " + strings.Join(obj.Times, ",") + " " + tz
	case CadenceModeCron:
		tz := c.TimezoneAbbrev(after)
		if tz == "" {
			tz = obj.TZ
		}
		return "🕘 cron " + tz
	default:
		return obj.Interval
	}
}

func (c Cadence) TimezoneAbbrev(after time.Time) string {
	next, ok := c.NextAfter(after)
	if !ok {
		return ""
	}
	return next.Format("MST")
}

func (c Cadence) cronSpecs() []string {
	obj := c.object()
	switch c.Mode() {
	case CadenceModeTimes:
		dow := "*"
		if days, err := normalizedDays(obj.Days); err == nil && len(days) > 0 {
			dow = strings.Join(days, ",")
		}
		specs := make([]string, 0, len(obj.Times))
		for _, tod := range obj.Times {
			h, m, err := parseTimeOfDay(tod)
			if err != nil {
				continue
			}
			specs = append(specs, c.cronSpec(fmt.Sprintf("%d %d * * %s", m, h, dow)))
		}
		return specs
	case CadenceModeCron:
		return []string{c.cronSpec(obj.Cron)}
	default:
		return nil
	}
}

func (c Cadence) cronSpec(expr string) string {
	return fmt.Sprintf("CRON_TZ=%s %s", c.object().TZ, strings.TrimSpace(expr))
}

func parseTimeOfDay(v string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid cadence time %q (want HH:MM)", v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("invalid cadence time %q (hour must be 00-23)", v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid cadence time %q (minute must be 00-59)", v)
	}
	return h, m, nil
}

func normalizedDays(days []string) ([]string, error) {
	if len(days) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(days))
	for _, day := range days {
		key := strings.ToLower(strings.TrimSpace(day))
		switch key {
		case "sun", "sunday", "0", "7":
			key = "0"
		case "mon", "monday", "1":
			key = "1"
		case "tue", "tues", "tuesday", "2":
			key = "2"
		case "wed", "wednesday", "3":
			key = "3"
		case "thu", "thur", "thurs", "thursday", "4":
			key = "4"
		case "fri", "friday", "5":
			key = "5"
		case "sat", "saturday", "6":
			key = "6"
		default:
			return nil, fmt.Errorf("invalid cadence day %q", day)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

func daysSummary(days []string) string {
	norm, err := normalizedDays(days)
	if err != nil || len(norm) == 0 {
		return "every day"
	}
	if strings.Join(norm, ",") == "1,2,3,4,5" {
		return "Mon–Fri"
	}
	names := map[string]string{"0": "Sun", "1": "Mon", "2": "Tue", "3": "Wed", "4": "Thu", "5": "Fri", "6": "Sat"}
	out := make([]string, 0, len(norm))
	for _, d := range norm {
		out = append(out, names[d])
	}
	return strings.Join(out, ", ")
}

func joinHuman(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}
