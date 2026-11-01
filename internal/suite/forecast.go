package suite

// Quota forecast math (pure, unit-tested). Inputs come from the plugin glue:
// the latest official window reading for one upstream account and the USD
// cost of the requests that account served, priced by the upstream pricing
// engine. Method (same idea as cpa-quota-estimator's "capacity = spend / share
// of quota consumed", without its learned weights):
//
//	capacity$  = used$ / used%           (only from an official used%)
//	remaining$ = capacity$ − used$
//	burn$/h    = spend over a recent slice of the window / its length
//	exhaust    = now + remaining$ / burn$/h, compared with the reset time.
//
// The ratio is unstable while used% is small (Codex reports whole percents,
// so 1% means anything in [0.5, 1.5)); below MinRatioPercent we fall back to
// earlier cycles of the same window (labelled estimated) or say "not enough
// data" instead of printing a wild number.

import (
	"math"
	"sort"
	"time"
)

const (
	MinRatioPercent = 5.0  // below this used% the used$/used% ratio is not trusted
	MinRatioUSD     = 0.01 // and at least one cent must have been spent
)

// WindowInput is one quota window of one upstream account.
type WindowInput struct {
	Seconds       int64
	ResetAt       time.Time
	UsedPercent   *float64 // official, nil when unknown
	PercentSource string   // "header" (recorded from responses) or "api" (official quota endpoint)
	ObservedAt    time.Time

	UsedUSD  float64 // cost of requests served since the window started
	Unpriced int64   // requests in the window without a price (cost unknown)

	RecentUSD   float64       // cost over the recent slice
	RecentSlice time.Duration // length of that slice (clipped to the window start)

	PercentRate *float64  // official %/h over the recent slice, if two readings exist
	History     []float64 // capacity$ of earlier, reliable cycles (newest first)
	Now         time.Time
}

// WindowForecast is what the UI shows for one window.
type WindowForecast struct {
	Start            time.Time  `json:"start"`
	ResetAt          time.Time  `json:"reset_at"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent"`
	PercentSource    string     `json:"percent_source"`
	ObservedAt       time.Time  `json:"observed_at"`
	UsedUSD          float64    `json:"used_usd"`
	Unpriced         int64      `json:"unpriced_requests"`
	CapacityUSD      *float64   `json:"capacity_usd"`
	CapacitySource   string     `json:"capacity_source"` // official_ratio | estimated | ""
	CapacityNote     string     `json:"capacity_note,omitempty"`
	HistoryCycles    int        `json:"history_cycles"`
	RemainingUSD     *float64   `json:"remaining_usd"`
	BurnUSDPerHour   float64    `json:"burn_usd_per_hour"`
	BurnSlice        string     `json:"burn_slice"`
	ExhaustAt        *time.Time `json:"exhaust_at"`
	ExhaustBasis     string     `json:"exhaust_basis,omitempty"` // usd | percent
	Risk             string     `json:"risk"`                    // exhausted | high | medium | low | unknown
	Stale            bool       `json:"stale"`
}

// Capacity notes.
const (
	NoteNotEnoughData = "not_enough_data" // used% below MinRatioPercent and no history
	NoteNoPercent     = "no_official_percent"
	NoteNoUsage       = "no_priced_usage"
	NoteReset         = "window_reset" // the reading belongs to a window that already reset
	NoteHistory       = "from_history"
)

// RecentSlice picks the burn-rate slice for a window: a fifth of it, clamped
// to [1h, 24h] (5h → 1h, weekly → 24h).
func RecentSlice(seconds int64) time.Duration {
	d := time.Duration(seconds) * time.Second / 5
	return time.Duration(math.Min(math.Max(float64(d), float64(time.Hour)), float64(24*time.Hour)))
}

// Forecast computes one window.
func Forecast(in WindowInput) WindowForecast {
	out := WindowForecast{ResetAt: in.ResetAt, PercentSource: in.PercentSource, ObservedAt: in.ObservedAt,
		UsedUSD: round6(in.UsedUSD), Unpriced: in.Unpriced, Risk: "unknown", HistoryCycles: len(in.History)}
	if in.Seconds > 0 && !in.ResetAt.IsZero() {
		out.Start = in.ResetAt.Add(-time.Duration(in.Seconds) * time.Second)
	}
	used := in.UsedPercent
	if used != nil && !in.ResetAt.IsZero() && !in.Now.Before(in.ResetAt) {
		// The reading is from a window that has since reset; the new window's
		// share is unknown until the next reading.
		used, out.Stale, out.CapacityNote = nil, true, NoteReset
		if in.Seconds > 0 {
			next := in.ResetAt
			for !in.Now.Before(next) {
				next = next.Add(time.Duration(in.Seconds) * time.Second)
			}
			out.Start, out.ResetAt = next.Add(-time.Duration(in.Seconds)*time.Second), next
		}
	}
	if used != nil {
		u := clamp(*used, 0, 100)
		r := 100 - u
		out.UsedPercent, out.RemainingPercent = &u, &r
	}
	if in.RecentSlice > 0 {
		out.BurnUSDPerHour = round6(in.RecentUSD / in.RecentSlice.Hours())
		out.BurnSlice = in.RecentSlice.Round(time.Minute).String()
	}
	switch {
	case used != nil && *out.UsedPercent >= MinRatioPercent && in.UsedUSD >= MinRatioUSD:
		c := in.UsedUSD / (*out.UsedPercent / 100)
		out.CapacityUSD, out.CapacitySource = ptr(round6(c)), "official_ratio"
	case len(in.History) > 0:
		c := median(in.History)
		out.CapacityUSD, out.CapacitySource = ptr(round6(c)), "estimated"
		if out.CapacityNote == "" {
			out.CapacityNote = NoteHistory
		}
	case used == nil && out.CapacityNote == "":
		out.CapacityNote = NoteNoPercent
	case used != nil && in.UsedUSD < MinRatioUSD:
		out.CapacityNote = NoteNoUsage
	case out.CapacityNote == "":
		out.CapacityNote = NoteNotEnoughData
	}
	if out.CapacityUSD != nil {
		remaining := *out.CapacityUSD - in.UsedUSD
		if out.UsedPercent != nil && out.CapacitySource == "estimated" {
			// With an official % but an estimated capacity, the official share wins.
			remaining = *out.CapacityUSD * *out.RemainingPercent / 100
		}
		out.RemainingUSD = ptr(round6(math.Max(0, remaining)))
	}
	// Exhaustion time.
	var hours float64 = -1
	if out.RemainingUSD != nil && out.BurnUSDPerHour > 0 {
		hours, out.ExhaustBasis = *out.RemainingUSD/out.BurnUSDPerHour, "usd"
	} else if out.RemainingPercent != nil && in.PercentRate != nil && *in.PercentRate > 0 {
		hours, out.ExhaustBasis = *out.RemainingPercent / *in.PercentRate, "percent"
	}
	switch {
	case out.UsedPercent != nil && *out.UsedPercent >= 100:
		out.Risk = "exhausted"
	case hours >= 0 && !out.ResetAt.IsZero():
		at := in.Now.Add(time.Duration(hours * float64(time.Hour)))
		out.ExhaustAt = &at
		left := out.ResetAt.Sub(in.Now)
		switch {
		case left <= 0 || !at.Before(out.ResetAt):
			out.Risk = "low"
		case at.Sub(in.Now) < left/2:
			out.Risk = "high"
		default:
			out.Risk = "medium"
		}
	case out.UsedPercent != nil && (out.BurnUSDPerHour == 0 && (in.PercentRate == nil || *in.PercentRate <= 0)):
		out.Risk = "low" // nothing is being consumed right now
	}
	return out
}

// CycleCapacity returns capacity$ for a finished cycle when its last official
// reading is reliable, so later windows can be estimated from history.
func CycleCapacity(usedPercent, usedUSD float64) (float64, bool) {
	if usedPercent < MinRatioPercent || usedUSD < MinRatioUSD || usedPercent > 100 {
		return 0, false
	}
	return usedUSD / (usedPercent / 100), true
}

func median(values []float64) float64 {
	v := append([]float64{}, values...)
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

func clamp(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }
func ptr(v float64) *float64          { return &v }
func round6(v float64) float64        { return math.Round(v*1e6) / 1e6 }
