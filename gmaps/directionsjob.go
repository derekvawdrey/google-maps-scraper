package gmaps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gosom/scrapemate"

	"github.com/gosom/google-maps-scraper/exiter"
)

// DirectionsJob loads a Google Maps directions URL for an origin/destination
// pair in a given travel mode (default "transit") and extracts the fare and
// trip summary that Google Maps renders in the directions panel.
//
// It mirrors the structure of PlaceJob: BrowserActions drives Playwright and
// stashes the extracted data in resp.Meta, and Process turns that into a
// FareResult that the configured ResultWriter (CSV/JSON) emits.
type DirectionsJob struct {
	scrapemate.Job

	Origin      string
	Destination string
	TravelMode  string
	PreferBus   bool
	ExitMonitor exiter.Exiter
}

type DirectionsJobOptions func(*DirectionsJob)

// NewDirectionsJob builds a directions job. mode defaults to "transit"; valid
// Google Maps travel modes are driving, walking, bicycling, transit and
// two-wheeler. "Bus" is not a top-level mode — it is a sub-filter inside
// transit, handled best-effort via PreferBus.
func NewDirectionsJob(id, origin, destination, mode, langCode string, preferBus bool, opts ...DirectionsJobOptions) *DirectionsJob {
	const (
		defaultPrio       = scrapemate.PriorityMedium
		defaultMaxRetries = 2
	)

	if mode == "" {
		mode = "transit"
	}

	if id == "" {
		id = uuid.New().String()
	}

	job := &DirectionsJob{
		Job: scrapemate.Job{
			ID:         id,
			Method:     http.MethodGet,
			URL:        buildDirectionsURL(origin, destination, mode, langCode),
			MaxRetries: defaultMaxRetries,
			Priority:   defaultPrio,
		},
		Origin:      origin,
		Destination: destination,
		TravelMode:  mode,
		PreferBus:   preferBus,
	}

	for _, opt := range opts {
		opt(job)
	}

	return job
}

// WithDirectionsJobExitMonitor lets the run terminate as soon as every route
// has been processed (each job is one "seed").
func WithDirectionsJobExitMonitor(e exiter.Exiter) DirectionsJobOptions {
	return func(j *DirectionsJob) {
		j.ExitMonitor = e
	}
}

func buildDirectionsURL(origin, destination, mode, langCode string) string {
	v := url.Values{}
	v.Set("api", "1")
	v.Set("origin", origin)
	v.Set("destination", destination)
	v.Set("travelmode", mode)

	if langCode != "" {
		v.Set("hl", langCode)
	}

	return "https://www.google.com/maps/dir/?" + v.Encode()
}

func (j *DirectionsJob) UseInResults() bool { return true }

func (j *DirectionsJob) ProcessOnFetchError() bool { return true }

func (j *DirectionsJob) BrowserActions(_ context.Context, page scrapemate.BrowserPage) scrapemate.Response {
	var resp scrapemate.Response

	pageResponse, err := page.Goto(j.GetURL(), scrapemate.WaitUntilDOMContentLoaded)
	if err != nil {
		resp.Error = err

		return resp
	}

	clickRejectCookiesIfRequired(page)

	// Wait for the transit trip cards to render. Google computes the route
	// client-side, so this can take a few seconds. We do not hard-fail if the
	// selector never appears — some routes have no transit option, and the
	// panel-text fallback in the extractor still tries to find a fare.
	_ = page.WaitForSelector(tripCardSelector, 15*time.Second)

	if j.PreferBus {
		if _, busErr := page.Eval(preferBusJS); busErr == nil {
			// Give Maps a moment to recompute routes for the bus filter.
			_ = page.WaitForSelector(tripCardSelector, 8*time.Second)
		}
	}

	// Fares often populate slightly after the cards appear.
	page.WaitForTimeout(2500 * time.Millisecond)

	rawAny, err := page.Eval(directionsExtractJS)
	if err != nil {
		resp.Error = err

		return resp
	}

	raw, ok := rawAny.(string)
	if !ok {
		resp.Error = fmt.Errorf("directions extractor returned %T, want string", rawAny)

		return resp
	}

	if os.Getenv("FARES_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[fares-debug] %s -> %s\n%s\n", j.Origin, j.Destination, raw)
	}

	if resp.Meta == nil {
		resp.Meta = make(map[string]any)
	}

	resp.Meta["fare_json"] = []byte(raw)

	resp.URL = page.URL()
	resp.StatusCode = pageResponse.StatusCode
	resp.Headers = pageResponse.Headers

	return resp
}

func (j *DirectionsJob) Process(_ context.Context, resp *scrapemate.Response) (any, []scrapemate.IJob, error) {
	defer func() {
		resp.Document = nil
		resp.Body = nil
		resp.Meta = nil
	}()

	// Each directions job is exactly one seed; mark it complete on every path
	// so the exiter can terminate the run once all routes are done.
	defer j.markComplete()

	if resp.Error != nil {
		return nil, nil, resp.Error
	}

	raw, ok := resp.Meta["fare_json"].([]byte)
	if !ok {
		return nil, nil, fmt.Errorf("missing extracted directions data")
	}

	var data directionsExtract
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, nil, fmt.Errorf("failed to parse directions data: %w", err)
	}

	res := &FareResult{
		Origin:      j.Origin,
		Destination: j.Destination,
		TravelMode:  j.TravelMode,
		URL:         firstNonBlank(data.URL, resp.URL),
	}

	// Prefer the first trip card that has a fare; otherwise fall back to the
	// first trip's duration and a panel-wide fare match.
	for i := range data.Trips {
		t := data.Trips[i]
		if t.Fare != "" {
			res.Fare = t.Fare
			res.Duration = t.Duration
			res.RouteSummary = oneLine(t.Text)

			break
		}
	}

	if res.Duration == "" && len(data.Trips) > 0 {
		res.Duration = data.Trips[0].Duration
		res.RouteSummary = oneLine(data.Trips[0].Text)
	}

	if res.Fare == "" {
		res.Fare = data.PanelFare
	}

	if res.Duration == "" {
		res.Duration = data.PanelDuration
	}

	if res.Fare == "" {
		res.Note = "no fare shown by Google Maps for this route/mode"
	}

	return res, nil, nil
}

func (j *DirectionsJob) markComplete() {
	if j.ExitMonitor != nil {
		j.ExitMonitor.IncrSeedCompleted(1)
	}
}

// FareResult is the output row for a directions/fare lookup.
type FareResult struct {
	Origin       string `json:"origin"`
	Destination  string `json:"destination"`
	TravelMode   string `json:"travel_mode"`
	Fare         string `json:"fare"`
	Duration     string `json:"duration"`
	RouteSummary string `json:"route_summary"`
	URL          string `json:"url"`
	Note         string `json:"note,omitempty"`
}

func (r *FareResult) CsvHeaders() []string {
	return []string{"origin", "destination", "travel_mode", "fare", "duration", "route_summary", "url", "note"}
}

func (r *FareResult) CsvRow() []string {
	return []string{r.Origin, r.Destination, r.TravelMode, r.Fare, r.Duration, r.RouteSummary, r.URL, r.Note}
}

type directionsExtract struct {
	URL           string     `json:"url"`
	Trips         []tripData `json:"trips"`
	PanelText     string     `json:"panelText"`
	PanelFare     string     `json:"panelFare"`
	PanelDuration string     `json:"panelDuration"`
}

type tripData struct {
	Fare     string `json:"fare"`
	Duration string `json:"duration"`
	Text     string `json:"text"`
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}

	return ""
}

func oneLine(s string) string {
	var parts []string

	for _, f := range strings.FieldsFunc(cleanText(s), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if f = strings.TrimSpace(f); f != "" {
			parts = append(parts, f)
		}
	}

	out := strings.Join(parts, " | ")

	const maxLen = 300
	if r := []rune(out); len(r) > maxLen {
		out = string(r[:maxLen])
	}

	return out
}

// cleanText drops Google's Material-icon private-use glyphs and zero-width
// characters, and normalizes the exotic spaces Maps uses (NBSP, narrow NBSP,
// thin/hair space) to a plain space so the output is clean text.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xE000 && r <= 0xF8FF, r >= 0xF0000: // private-use (icon) planes
			return -1
		case r == 0x200B, r == 0xFEFF: // zero-width space / BOM
			return -1
		case r == 0x00A0, r == 0x202F, r == 0x2009, r == 0x200A: // NBSP, narrow NBSP, thin/hair space
			return ' '
		default:
			return r
		}
	}, s)
}

// tripCardSelector matches the per-route cards in the transit directions panel.
// Google obfuscates class names, so we lean on the more stable id prefix and a
// couple of structural fallbacks.
const tripCardSelector = `div[id^="section-directions-trip-"], [data-trip-index], div[role="radiogroup"] [role="radio"]`

// directionsExtractJS reads the transit trip cards and pulls a fare + duration
// out of each. It relies on the (fairly stable) "section-directions-trip-" id
// prefix rather than obfuscated class names, and falls back to a currency-regex
// scan of the whole directions panel. Returns a JSON string.
const directionsExtractJS = `
(function() {
	function pick(re, s) { var m = (s || '').match(re); return m ? m[0] : ''; }
	var fareRe = /¥[\d,]+|[\d,]+\s*円|\$[\d.]+|€[\d.,]+|£[\d.]+|₩[\d,]+/;
	var durRe = /\d+\s*(?:hr|h|hour|hours|min|mins|minute|minutes|時間|分)(?:\s*\d+\s*(?:min|mins|分))?/i;
	var out = { url: location.href, trips: [], panelText: '', panelFare: '', panelDuration: '' };

	var selectors = ['div[id^="section-directions-trip-"]', '[data-trip-index]', 'div[role="radiogroup"] > div'];
	var seen = [];
	for (var s = 0; s < selectors.length; s++) {
		var nodes = document.querySelectorAll(selectors[s]);
		for (var n = 0; n < nodes.length; n++) {
			var el = nodes[n];
			if (seen.indexOf(el) !== -1) continue;
			seen.push(el);
			var t = (el.innerText || '').trim();
			if (!t) continue;
			out.trips.push({ fare: pick(fareRe, t), duration: pick(durRe, t), text: t.slice(0, 600) });
		}
	}

	var panel = document.querySelector('div[role="main"]');
	out.panelText = panel ? (panel.innerText || '').slice(0, 6000) : '';
	out.panelFare = pick(fareRe, out.panelText);
	out.panelDuration = pick(durRe, out.panelText);
	return JSON.stringify(out);
})()
`

// preferBusJS is a best-effort attempt to bias the transit results toward
// buses. Google Maps' transit-options UI is heavily obfuscated and changes
// often, so this is intentionally lenient and never fatal.
const preferBusJS = `
(function() {
	function clickByLabel(matchers) {
		var els = document.querySelectorAll('button,[role=button],[role=checkbox],[role=radio],label');
		for (var i = 0; i < els.length; i++) {
			var el = els[i];
			var lbl = (((el.getAttribute && el.getAttribute('aria-label')) || el.textContent || '')).toLowerCase();
			for (var j = 0; j < matchers.length; j++) {
				if (lbl.indexOf(matchers[j]) !== -1) { el.click(); return true; }
			}
		}
		return false;
	}
	clickByLabel(['options', 'preferences', '設定']);
	return clickByLabel(['bus', 'バス']);
})()
`
