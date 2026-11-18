package betty

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Weather from Open-Meteo (open-meteo.com, keyless): the place is geocoded,
// then the current conditions and today's and tomorrow's high and low are
// read. A weather question that names no place gets a note instead, so she
// asks where rather than guessing or researching.

// WeatherNeedsPlaceSource is the Snippet.Source of the note that a weather
// question named no place.
const WeatherNeedsPlaceSource = "weather needs a place"

// Open-Meteo request values.
const (
	weatherCurrentFields = "temperature_2m,apparent_temperature,weather_code,wind_speed_10m,relative_humidity_2m"
	weatherDailyFields   = "temperature_2m_max,temperature_2m_min"
	// weatherForecastDays covers today and tomorrow.
	weatherForecastDays = "2"
	// coordinatePrecision is the decimals sent for a latitude or longitude.
	coordinatePrecision = 4
	// temperaturePrecision is the decimals shown for a temperature.
	temperaturePrecision = 1
)

// Celsius to Fahrenheit: F = C * fahrenheitScale + fahrenheitOffset.
const (
	fahrenheitScale  = 9.0 / 5.0
	fahrenheitOffset = 32.0
)

var (
	// weatherIntent marks a weather question.
	weatherIntent = regexp.MustCompile(`(?i)\b(?:weather|temperature (?:outside|in|at|for|near|around)|forecast (?:for|in)|how (?:hot|cold|warm) is it|is it (?:raining|snowing|sunny))\b`)
	// weatherPrep introduces the place.
	weatherPrep = regexp.MustCompile(`(?i)\b(?:in|at|for|near|around)\s+(.+)$`)
	// weatherLead is what may come before "<place> weather".
	weatherLead = regexp.MustCompile(`(?i)^(?:what(?:'s|s| is| will)|how(?:'s|s| is| will))?\s*(?:the\s+)?(?:current\s+|today's\s+|todays\s+)?`)
	// weatherTail are closing words that are not part of the place.
	weatherTail = regexp.MustCompile(`(?i)(?:^|\s+)(?:right now|now|today|tonight|tomorrow|currently|at the moment|this (?:morning|afternoon|evening|week|weekend)|outside|like|be like|going to be|looking)$`)
	// weatherNoPlace are "places" only the speaker knows.
	weatherNoPlace = regexp.MustCompile(`(?i)^(?:here|home|town|area|city|office|me|us|there|(?:my|our|this|where)\b.*)$`)
)

// weatherPlace reports whether q is a weather question and the place it
// names; an empty place means none was named.
func weatherPlace(q string) (place string, ok bool) {
	loc := weatherIntent.FindStringIndex(q)
	if loc == nil {
		return "", false
	}
	if m := weatherPrep.FindStringSubmatch(q[loc[0]:]); m != nil {
		place = m[1]
	} else {
		// "what's the Boston weather": the place comes first.
		place = weatherLead.ReplaceAllString(q[:loc[0]], "")
	}
	place = strings.TrimSpace(place)
	for {
		cut := weatherTail.FindStringIndex(place)
		if cut == nil {
			break
		}
		place = strings.TrimSpace(place[:cut[0]])
	}
	place = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(place), "the "))
	if weatherNoPlace.MatchString(place) {
		place = ""
	}
	return place, true
}

// needsPlaceSnippet tells the model no place was named.
func needsPlaceSnippet() Snippet {
	return Snippet{Source: WeatherNeedsPlaceSource, Title: "No place was named",
		Text: "No forecast was fetched: the question names no place, and where the speaker is or lives is not known to the hive. " +
			"The answer is to ask which city or town they mean. Nothing here needs research."}
}

// weatherCodes are the WMO weather interpretation codes Open-Meteo returns.
var weatherCodes = map[int]string{
	0: "clear sky", 1: "mainly clear", 2: "partly cloudy", 3: "overcast",
	45: "fog", 48: "freezing fog",
	51: "light drizzle", 53: "drizzle", 55: "heavy drizzle", 56: "light freezing drizzle", 57: "freezing drizzle",
	61: "light rain", 63: "rain", 65: "heavy rain", 66: "light freezing rain", 67: "freezing rain",
	71: "light snow", 73: "snow", 75: "heavy snow", 77: "snow grains",
	80: "light rain showers", 81: "rain showers", 82: "heavy rain showers", 85: "light snow showers", 86: "snow showers",
	95: "thunderstorm", 96: "thunderstorm with hail", 99: "thunderstorm with heavy hail",
}

type geocodeResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Admin1    string  `json:"admin1"`
		Country   string  `json:"country"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"results"`
}

type forecastResponse struct {
	Current *struct {
		Time        string  `json:"time"`
		Temperature float64 `json:"temperature_2m"`
		Apparent    float64 `json:"apparent_temperature"`
		Code        int     `json:"weather_code"`
		Wind        float64 `json:"wind_speed_10m"`
		Humidity    float64 `json:"relative_humidity_2m"`
	} `json:"current"`
	Daily struct {
		Max []float64 `json:"temperature_2m_max"`
		Min []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

// degrees is a temperature in both scales: "8.4 C (47.1 F)".
func degrees(c float64) string {
	f := c*fahrenheitScale + fahrenheitOffset
	return strconv.FormatFloat(c, 'f', temperaturePrecision, 64) + " C (" + strconv.FormatFloat(f, 'f', temperaturePrecision, 64) + " F)"
}

// geocode finds a place; when nothing matches a name of several words, the
// last word is dropped once ("boston massachusetts" finds Boston).
func (d DirectRetriever) geocode(ctx context.Context, place string) (geocodeResponse, bool) {
	var g geocodeResponse
	search := func(name string) bool {
		g = geocodeResponse{}
		u := base(d.GeocodeURL, defaultGeocodeURL) + "/v1/search?count=1&language=en&format=json&name=" + url.QueryEscape(name)
		return d.getJSON(ctx, u, &g) && len(g.Results) > 0
	}
	if search(place) {
		return g, true
	}
	if i := strings.LastIndex(place, " "); i > 0 && ctx.Err() == nil && search(place[:i]) {
		return g, true
	}
	return g, false
}

// weather is the current weather and the day's range for a place.
func (d DirectRetriever) weather(ctx context.Context, place string) []Snippet {
	g, ok := d.geocode(ctx, place)
	if !ok {
		return nil
	}
	at := g.Results[0]
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(at.Latitude, 'f', coordinatePrecision, 64))
	q.Set("longitude", strconv.FormatFloat(at.Longitude, 'f', coordinatePrecision, 64))
	q.Set("current", weatherCurrentFields)
	q.Set("daily", weatherDailyFields)
	q.Set("forecast_days", weatherForecastDays)
	q.Set("timezone", "auto")
	var f forecastResponse
	if !d.getJSON(ctx, base(d.ForecastURL, defaultForecastURL)+"/v1/forecast?"+q.Encode(), &f) || f.Current == nil {
		return nil
	}
	name := at.Name
	for _, part := range []string{at.Admin1, at.Country} {
		if part != "" && part != at.Name {
			name += ", " + part
		}
	}
	var b strings.Builder
	b.WriteString("Now: " + degrees(f.Current.Temperature) + ", feels like " + degrees(f.Current.Apparent))
	if desc, ok := weatherCodes[f.Current.Code]; ok {
		b.WriteString(", " + desc)
	}
	b.WriteString(", wind " + strconv.FormatFloat(f.Current.Wind, 'f', 0, 64) + " km/h, humidity " + strconv.FormatFloat(f.Current.Humidity, 'f', 0, 64) + "%.")
	for i, day := range []string{"Today", "Tomorrow"} {
		if i < len(f.Daily.Max) && i < len(f.Daily.Min) {
			b.WriteString(" " + day + ": high " + degrees(f.Daily.Max[i]) + ", low " + degrees(f.Daily.Min[i]) + ".")
		}
	}
	if f.Current.Time != "" {
		b.WriteString(" Observed " + stripHTML(f.Current.Time) + " local time.")
	}
	b.WriteString(" According to " + SourceOpenMeteo + " (open-meteo.com).")
	return []Snippet{{Source: SourceOpenMeteo, Title: "Weather in " + clip(stripHTML(name), maxSnippetRunes), Text: b.String()}}
}
