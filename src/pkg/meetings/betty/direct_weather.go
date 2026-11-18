package betty

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Weather from Open-Meteo (open-meteo.com, keyless): the place is geocoded,
// then the current conditions and today's and tomorrow's high and low are
// read. A weather question that names no place gets a note instead, so she
// asks where rather than guessing or researching.

// WeatherNeedsPlaceSource is the Snippet.Source of the note that a weather
// question named no place.
const WeatherNeedsPlaceSource = "weather needs a place"

// WeatherUnavailableSource is the Snippet.Source of the note that a weather
// question named a place but no live conditions could be read for it.
const WeatherUnavailableSource = "weather unavailable"

// unavailableSnippet tells the model live weather could not be read, so it
// states no temperature and does not start research.
func unavailableSnippet() Snippet {
	return Snippet{Source: WeatherUnavailableSource, Title: "No live weather",
		Text: "Live weather could not be read for this place right now. Web pages about weather are cached forecasts from other days and are not used. " +
			"The answer is one short sentence: you cannot get current conditions right now. State no temperature or conditions. Nothing here needs research."}
}

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

type geocodeResult struct {
	Name    string `json:"name"`
	Admin1  string `json:"admin1"`
	Country string `json:"country"`
	// CountryCode is the ISO 3166 alpha-2 code ("US").
	CountryCode string  `json:"country_code"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

type geocodeResponse struct {
	Results []geocodeResult `json:"results"`
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

// usStates are the full names of the US states and DC, lower case, to the
// two-letter postal code.
var usStates = map[string]string{
	"alabama": "al", "alaska": "ak", "arizona": "az", "arkansas": "ar", "california": "ca", "colorado": "co",
	"connecticut": "ct", "delaware": "de", "florida": "fl", "georgia": "ga", "hawaii": "hi", "idaho": "id",
	"illinois": "il", "indiana": "in", "iowa": "ia", "kansas": "ks", "kentucky": "ky", "louisiana": "la",
	"maine": "me", "maryland": "md", "massachusetts": "ma", "michigan": "mi", "minnesota": "mn",
	"mississippi": "ms", "missouri": "mo", "montana": "mt", "nebraska": "ne", "nevada": "nv",
	"new hampshire": "nh", "new jersey": "nj", "new mexico": "nm", "new york": "ny", "north carolina": "nc",
	"north dakota": "nd", "ohio": "oh", "oklahoma": "ok", "oregon": "or", "pennsylvania": "pa",
	"rhode island": "ri", "south carolina": "sc", "south dakota": "sd", "tennessee": "tn", "texas": "tx",
	"utah": "ut", "vermont": "vt", "virginia": "va", "washington": "wa", "west virginia": "wv",
	"wisconsin": "wi", "wyoming": "wy", "district of columbia": "dc",
}

// splitRegion splits "carmel new york" or "carmel, ny" into the place name
// and a US state's full name. The Open-Meteo geocoder matches names only, so
// the whole phrase finds nothing. A postal code counts only after a comma
// ("carmel, in"); without one it would swallow words like "in" and "or". With
// no state, region is empty.
func splitRegion(place string) (name, region string) {
	place = strings.ToLower(strings.TrimSpace(place))
	if i := strings.LastIndex(place, ","); i > 0 {
		tail := strings.TrimSpace(place[i+1:])
		for full, code := range usStates {
			if tail == full || tail == code {
				return strings.TrimSpace(place[:i]), full
			}
		}
	}
	words := strings.Fields(strings.ReplaceAll(place, ",", " "))
	for n := 2; n >= 1; n-- {
		if len(words) <= n {
			continue
		}
		if _, ok := usStates[strings.Join(words[len(words)-n:], " ")]; ok {
			return strings.Join(words[:len(words)-n], " "), strings.Join(words[len(words)-n:], " ")
		}
	}
	return strings.Join(words, " "), ""
}

// geocodeCandidates is how many matches one name search asks for, so a
// small place with a common name ("Carmel") is not hidden behind a bigger one.
const geocodeCandidates = "10"

// geocode finds a place. A trailing US state is split off and used to pick
// among the matches ("carmel new york" is Carmel in New York, not Carmel in
// Indiana); when the whole name finds nothing, trailing words are dropped
// one at a time. A named state that no match carries is a miss, never a
// different place's weather.
func (d DirectRetriever) geocode(ctx context.Context, place string) (geocodeResult, bool) {
	name, region := splitRegion(place)
	search := func(name string) (geocodeResult, bool) {
		var g geocodeResponse
		u := base(d.GeocodeURL, defaultGeocodeURL) + "/v1/search?count=" + geocodeCandidates + "&language=en&format=json&name=" + url.QueryEscape(name)
		if !d.getJSON(ctx, u, &g) {
			return geocodeResult{}, false
		}
		for _, r := range g.Results {
			if region == "" || strings.EqualFold(r.Admin1, region) {
				return r, true
			}
		}
		return geocodeResult{}, false
	}
	for name != "" && ctx.Err() == nil {
		if r, ok := search(name); ok {
			return r, true
		}
		i := strings.LastIndex(name, " ")
		if i <= 0 {
			break
		}
		name = name[:i]
	}
	return geocodeResult{}, false
}

// Observation freshness and the National Weather Service endpoint.
const (
	defaultNWSURL = "https://api.weather.gov"
	// maxObservationAge is how old a station reading may be and still be
	// called "now".
	maxObservationAge = 90 * time.Minute
)

// nwsObservation is a station's latest reading.
type nwsObservation struct {
	Station, Description string
	At                   time.Time
	TempC                float64
}

type nwsPoints struct {
	Properties struct {
		ObservationStations string `json:"observationStations"`
	} `json:"properties"`
}

type nwsStations struct {
	Features []struct {
		ID         string `json:"id"`
		Properties struct {
			StationIdentifier string `json:"stationIdentifier"`
		} `json:"properties"`
	} `json:"features"`
}

type nwsLatest struct {
	Properties struct {
		Timestamp       string `json:"timestamp"`
		TextDescription string `json:"textDescription"`
		Temperature     struct {
			Value *float64 `json:"value"`
		} `json:"temperature"`
	} `json:"properties"`
}

// observation reads the latest reading of the nearest National Weather
// Service station (US places only): it is a measurement, where Open-Meteo's
// "current" is a model value that can be several degrees off when a front
// moves through. A reading older than maxObservationAge is not used.
func (d DirectRetriever) observation(ctx context.Context, lat, lon float64) (nwsObservation, bool) {
	root := base(d.NWSURL, defaultNWSURL)
	var pts nwsPoints
	coords := strconv.FormatFloat(lat, 'f', coordinatePrecision, 64) + "," + strconv.FormatFloat(lon, 'f', coordinatePrecision, 64)
	if !d.getJSON(ctx, root+"/points/"+coords, &pts) || pts.Properties.ObservationStations == "" {
		return nwsObservation{}, false
	}
	var st nwsStations
	if !d.getJSON(ctx, d.nwsLocal(pts.Properties.ObservationStations), &st) || len(st.Features) == 0 || st.Features[0].ID == "" {
		return nwsObservation{}, false
	}
	var latest nwsLatest
	if !d.getJSON(ctx, d.nwsLocal(st.Features[0].ID)+"/observations/latest", &latest) || latest.Properties.Temperature.Value == nil {
		return nwsObservation{}, false
	}
	at, err := time.Parse(time.RFC3339, latest.Properties.Timestamp)
	if err != nil {
		return nwsObservation{}, false
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	if age := now().Sub(at); age > maxObservationAge || age < -maxObservationAge {
		return nwsObservation{}, false
	}
	return nwsObservation{Station: st.Features[0].Properties.StationIdentifier, At: at, TempC: *latest.Properties.Temperature.Value,
		Description: stripHTML(latest.Properties.TextDescription)}, true
}

// nwsLocal points a URL the National Weather Service returned at the
// configured endpoint when one is set (tests); otherwise it is unchanged.
func (d DirectRetriever) nwsLocal(u string) string {
	if d.NWSURL == "" {
		return u
	}
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "/"); j >= 0 {
			return strings.TrimRight(d.NWSURL, "/") + u[i+3+j:]
		}
	}
	return u
}

// weather is the current weather and the day's range for a place. For a
// US place the nearest station's reading is the "now"; the forecast model
// supplies the day's range, and stands in for "now" (marked as a model
// estimate) when no fresh reading exists.
func (d DirectRetriever) weather(ctx context.Context, place string) []Snippet {
	at, ok := d.geocode(ctx, place)
	if !ok {
		return nil
	}
	var (
		obs   nwsObservation
		haveO bool
		wg    sync.WaitGroup
	)
	if strings.EqualFold(at.CountryCode, "US") {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obs, haveO = d.observation(ctx, at.Latitude, at.Longitude)
		}()
	}
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(at.Latitude, 'f', coordinatePrecision, 64))
	q.Set("longitude", strconv.FormatFloat(at.Longitude, 'f', coordinatePrecision, 64))
	q.Set("current", weatherCurrentFields)
	q.Set("daily", weatherDailyFields)
	q.Set("forecast_days", weatherForecastDays)
	q.Set("timezone", "auto")
	var f forecastResponse
	haveF := d.getJSON(ctx, base(d.ForecastURL, defaultForecastURL)+"/v1/forecast?"+q.Encode(), &f) && f.Current != nil
	wg.Wait()
	if !haveF && !haveO {
		return nil
	}
	name := at.Name
	for _, part := range []string{at.Admin1, at.Country} {
		if part != "" && part != at.Name {
			name += ", " + part
		}
	}
	var b strings.Builder
	source := SourceOpenMeteo
	switch {
	case haveO:
		source = SourceNWS
		b.WriteString("Now (measured at station " + stripHTML(obs.Station) + " at " + obs.At.UTC().Format("15:04") + " UTC): " + degrees(obs.TempC))
		if obs.Description != "" {
			b.WriteString(", " + strings.ToLower(obs.Description))
		}
		b.WriteString(".")
	default:
		b.WriteString("Now (forecast-model estimate, not a station reading, can be off by several degrees): " + degrees(f.Current.Temperature) + ", feels like " + degrees(f.Current.Apparent))
		if desc, ok := weatherCodes[f.Current.Code]; ok {
			b.WriteString(", " + desc)
		}
		b.WriteString(", wind " + strconv.FormatFloat(f.Current.Wind, 'f', 0, 64) + " km/h, humidity " + strconv.FormatFloat(f.Current.Humidity, 'f', 0, 64) + "%.")
	}
	if haveF {
		for i, day := range []string{"Today", "Tomorrow"} {
			if i < len(f.Daily.Max) && i < len(f.Daily.Min) {
				b.WriteString(" " + day + " (forecast): high " + degrees(f.Daily.Max[i]) + ", low " + degrees(f.Daily.Min[i]) + ".")
			}
		}
	}
	if !haveO && haveF && f.Current.Time != "" {
		b.WriteString(" Model time " + stripHTML(f.Current.Time) + " local time.")
	}
	b.WriteString(" According to " + source + ".")
	link := "https://open-meteo.com/"
	if haveO {
		link = "https://forecast.weather.gov/MapClick.php?lat=" + strconv.FormatFloat(at.Latitude, 'f', coordinatePrecision, 64) +
			"&lon=" + strconv.FormatFloat(at.Longitude, 'f', coordinatePrecision, 64)
	}
	return []Snippet{{Source: source, Title: "Weather in " + clip(stripHTML(name), maxSnippetRunes), Text: b.String(), URL: link}}
}
