package suite

// forecast engine: cycle detection, milestone capacity, coverage,
// three burn lines and the period ledger. Pure functions, unit-tested.
//
// The method follows ideas from cpa-quota-estimator v0.19.6
// (https://github.com/Autsunset/cpa-quota-estimator, MIT License,
// Copyright (c) 2026 Autsunset): milestone-interval capacity with a median and
// a P25–P75 band, confidence from sample count and span, sustainable /
// cumulative / recent burn lines with a pace label, strict reset confirmation
// with A→B→A rejection, and a per-account coverage mode. No code was copied;
// this is an independent implementation. Its weight learner is not used.

import (
	"math"
	"sort"
	"time"
)

// QuotaObs is one official used-% reading (header or quota endpoint).
type QuotaObs struct {
	At      time.Time // observation time (Codex headers: request time + TTFT)
	Used    float64
	ResetAt time.Time
	Source  string // header | api
}

// SpendEvent is one request served by the account (from request_events).
type SpendEvent struct {
	At       time.Time // request completion (request_events.at)
	Start    time.Time // request start (At - latency); zero = unknown
	USD      float64
	Tokens   int64
	Unpriced bool
}

// TimeRange marks a span whose upstream usage is not in request_events
// (suite candy tests); intervals overlapping it are not used for capacity.
type TimeRange struct{ From, To time.Time }

// Coverage modes.
const (
	CoverageCPAOnly = "cpa_only"
	CoverageMixed   = "mixed"
	CoverageUnknown = "unknown"
)

// Confidence levels.
const (
	ConfHigh         = "high"
	ConfMedium       = "medium"
	ConfLow          = "low"
	ConfInsufficient = "insufficient"
)

const (
	resetTolerance   = 90 * time.Second
	earlyResetMax    = 5.0 // an early reset starts at ≤5 %
	earlyResetSpan   = 60 * time.Second
	paceBand         = 2.0 // ±2 points of time progress
	ledgerMaxCycles  = 8
	poolMinSamples   = 5
	mixedMinJump     = 2.0              // a ≥2-point rise with no CPA request between readings
	mixedMinJumps    = 2                // and at least two such rises before calling it mixed
	mixedExplained   = 0.25             // a rise CPA spend explains < 25 % of is unexplained
	coverageSlack    = 30 * time.Second // upstream accounting / clock lag
	cpaOnlyMinChecks = 3
	// 
	priorCutover          = 3    // current-cycle samples before the prior is dropped
	priorSigma            = 0.35 // log random walk per cycle (cpa-quota-estimator)
	priorMaxCycles        = 3
	quotaChangeMinSamples = 5
	quotaChangeRatio      = 0.30
	riskEarlyFrac         = 0.25
	backtestMaxCycles     = 4
	backtestMinPoints     = 3
	backtestMaxMAPE       = 0.30
)

// Cycle is one detected quota period with its clean readings.
type Cycle struct {
	ResetAt time.Time
	Start   time.Time
	Early   bool // began with a confirmed early reset
	Obs     []QuotaObs
}

func sameReset(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	d := a.Sub(b)
	return d <= resetTolerance && d >= -resetTolerance
}

// DetectCycles groups readings into periods. A change of reset time that
// arrives when the old reset is due is accepted at once (scheduled rollover);
// any other change needs two consistent readings, and A→B→A blips are
// rejected. A drop of used % without a new reset time is an early reset only
// when three successful, non-decreasing readings spanning ≥60 s start at ≤5 %
// (or at most half the old peak). The last unconfirmed reading is returned as
// pending and is not used.
func DetectCycles(obs []QuotaObs, seconds int64) (cycles []Cycle, rejected int, pending *QuotaObs) {
	list := append([]QuotaObs{}, obs...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	window := time.Duration(seconds) * time.Second
	newCycle := func(r QuotaObs, early bool) {
		start := r.At
		if !r.ResetAt.IsZero() && window > 0 {
			start = r.ResetAt.Add(-window)
		}
		if early {
			start = r.At
		}
		cycles = append(cycles, Cycle{ResetAt: r.ResetAt, Start: start, Early: early, Obs: []QuotaObs{r}})
	}
	for i := 0; i < len(list); i++ {
		r := list[i]
		if len(cycles) == 0 {
			newCycle(r, false)
			continue
		}
		cur := &cycles[len(cycles)-1]
		prev := cur.Obs[len(cur.Obs)-1]
		if sameReset(r.ResetAt, cur.ResetAt) {
			if r.Used < prev.Used-1 {
				if confirmEarlyReset(list[i:], cur.ResetAt, peak(cur.Obs)) {
					newCycle(r, true)
				} else {
					rejected++
				}
				continue
			}
			cur.Obs = append(cur.Obs, r)
			continue
		}
		if i+1 < len(list) && sameReset(list[i+1].ResetAt, cur.ResetAt) {
			rejected++ // A→B→A
			continue
		}
		scheduled := !cur.ResetAt.IsZero() && !r.At.Before(cur.ResetAt.Add(-resetTolerance)) && r.ResetAt.After(cur.ResetAt)
		if scheduled || (i+1 < len(list) && sameReset(list[i+1].ResetAt, r.ResetAt)) {
			newCycle(r, false)
			continue
		}
		if i+1 == len(list) {
			p := r
			pending = &p
			continue
		}
		rejected++
	}
	return cycles, rejected, pending
}

func confirmEarlyReset(rest []QuotaObs, reset time.Time, oldPeak float64) bool {
	if len(rest) < 3 {
		return false
	}
	first := rest[0]
	if first.Used > earlyResetMax && first.Used > oldPeak/2 {
		return false
	}
	for k := 1; k < 3; k++ {
		if !sameReset(rest[k].ResetAt, reset) || rest[k].Used < rest[k-1].Used {
			return false
		}
	}
	return rest[2].At.Sub(first.At) >= earlyResetSpan
}

func peak(obs []QuotaObs) float64 {
	p := 0.0
	for _, o := range obs {
		p = math.Max(p, o.Used)
	}
	return p
}

// spendIndex answers cumulative spend queries over sorted events.
type spendIndex struct {
	at     []time.Time
	start  []time.Time
	maxDur time.Duration
	usd    []float64 // prefix sums
	tokens []int64
}

func newSpendIndex(events []SpendEvent) spendIndex {
	ev := append([]SpendEvent{}, events...)
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].At.Before(ev[j].At) })
	idx := spendIndex{at: make([]time.Time, len(ev)), start: make([]time.Time, len(ev)), usd: make([]float64, len(ev)+1), tokens: make([]int64, len(ev)+1)}
	for i, e := range ev {
		idx.at[i] = e.At
		idx.start[i] = e.Start
		if e.Start.IsZero() || e.Start.After(e.At) {
			idx.start[i] = e.At
		}
		if d := e.At.Sub(idx.start[i]); d > idx.maxDur {
			idx.maxDur = d
		}
		idx.usd[i+1] = idx.usd[i] + e.USD
		idx.tokens[i+1] = idx.tokens[i] + e.Tokens
	}
	return idx
}

// pos is the number of events strictly before t.
func (s spendIndex) pos(t time.Time) int {
	return sort.Search(len(s.at), func(i int) bool { return !s.at[i].Before(t) })
}

// between returns spend, tokens and request count in [a, b).
func (s spendIndex) between(a, b time.Time) (float64, int64, int) {
	i, j := s.pos(a), s.pos(b)
	if j < i {
		return 0, 0, 0
	}
	return s.usd[j] - s.usd[i], s.tokens[j] - s.tokens[i], j - i
}

// attributed returns the CPA spend booked inside [a, b): each request's cost
// spread evenly over its run time (start → completion). Events are stamped at
// completion, but upstream books a streaming request while it runs, so a
// request that started before b and completes later already moved reading b —
// the usual case with a coding agent's parallel, minutes-long calls. Counting
// only completions made such rises look like usage outside CPA.
func (s spendIndex) attributed(a, b time.Time) float64 {
	a, b = a.Add(-coverageSlack), b.Add(coverageSlack)
	usd := 0.0
	limit := b.Add(s.maxDur)
	for i := s.pos(a); i < len(s.at) && s.at[i].Before(limit); i++ {
		st, en := s.start[i], s.at[i]
		cost := s.usd[i+1] - s.usd[i]
		if !st.Before(b) || en.Before(a) {
			continue
		}
		dur := en.Sub(st)
		if dur <= 0 {
			usd += cost
			continue
		}
		lo, hi := st, en
		if lo.Before(a) {
			lo = a
		}
		if hi.After(b) {
			hi = b
		}
		usd += cost * float64(hi.Sub(lo)) / float64(dur)
	}
	return usd
}

func overlaps(a, b time.Time, ex []TimeRange) bool {
	for _, r := range ex {
		if r.From.Before(b) && r.To.After(a) {
			return true
		}
	}
	return false
}

// Milestone is the first reading at which a new all-time-high whole percent
// of a cycle was seen.
type Milestone struct {
	At      time.Time `json:"at"`
	Percent float64   `json:"percent"`
}

func milestones(obs []QuotaObs) []Milestone {
	var out []Milestone
	best := -1.0
	for _, o := range obs {
		p := math.Floor(o.Used + 1e-9)
		if p > best {
			if best >= 0 || p >= 0 {
				out = append(out, Milestone{At: o.At, Percent: p})
			}
			best = p
		}
	}
	return out
}

// CapacitySamples turns milestone intervals into "spend per 100 %" samples.
type capacitySamples struct {
	usd, tokens []float64
	span        float64
}

func cycleSamples(c Cycle, spend spendIndex, exclude []TimeRange) capacitySamples {
	var s capacitySamples
	ms := milestones(c.Obs)
	for k := 1; k < len(ms); k++ {
		a, b := ms[k-1], ms[k]
		dp := b.Percent - a.Percent
		if dp <= 0 || overlaps(a.At, b.At, exclude) {
			continue
		}
		usd, tok, n := spend.between(a.At, b.At)
		if n == 0 || usd <= 0 {
			continue
		}
		s.usd = append(s.usd, usd*100/dp)
		s.tokens = append(s.tokens, float64(tok)*100/dp)
		s.span += dp
	}
	return s
}

// Coverage looks for usage the account received outside CPA: rises of at
// least two points between consecutive readings that the CPA spend booked in
// that interval explains less than a quarter of (the interval's share of the
// cycle's booked CPA spend, converted to the cycle's rise in points). Two such rises are needed: a single one is too
// easily header jitter from parallel requests.
func Coverage(cycles []Cycle, spend spendIndex, exclude []TimeRange) string {
	checks, jumps := 0, 0
	for _, c := range cycles {
		if len(c.Obs) < 2 {
			continue
		}
		first, last := c.Obs[0], c.Obs[len(c.Obs)-1]
		cycleRise := math.Floor(last.Used+1e-9) - math.Floor(first.Used+1e-9)
		cycleUSD := spend.attributed(first.At, last.At)
		for k := 1; k < len(c.Obs); k++ {
			a, b := c.Obs[k-1], c.Obs[k]
			rise := math.Floor(b.Used+1e-9) - math.Floor(a.Used+1e-9)
			if rise < 1 || overlaps(a.At, b.At, exclude) {
				continue
			}
			booked := spend.attributed(a.At, b.At)
			// explained: the interval's share of the cycle's CPA spend, in points
			explained := 0.0
			if cycleUSD > 0 && cycleRise > 0 {
				explained = booked / cycleUSD * cycleRise
			}
			if rise >= mixedMinJump && explained < mixedExplained*rise {
				if jumps++; jumps >= mixedMinJumps {
					return CoverageMixed
				}
				continue
			}
			if booked > 0 {
				checks++
			}
		}
	}
	if checks >= cpaOnlyMinChecks {
		return CoverageCPAOnly
	}
	return CoverageUnknown
}

// Confidence from sample count and the percent span they cover.
func Confidence(n int, span float64) string {
	switch {
	case n >= 5 && span >= 5:
		return ConfHigh
	case n >= 2 && span >= 2:
		return ConfMedium
	case n >= 1:
		return ConfLow
	}
	return ConfInsufficient
}

var confRank = map[string]int{ConfInsufficient: 0, ConfLow: 1, ConfMedium: 2, ConfHigh: 3}

func capConf(c, max string) string {
	if confRank[c] > confRank[max] {
		return max
	}
	return c
}

// Quantile with linear interpolation (q in [0,1]).
func Quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v := append([]float64{}, values...)
	sort.Float64s(v)
	pos := q * float64(len(v)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return v[lo] + (v[hi]-v[lo])*(pos-float64(lo))
}

// LedgerCycle is one finished period kept after its reset.
type LedgerCycle struct {
	Window         string    `json:"window"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	Early          bool      `json:"early,omitempty"`
	PeakPercent    float64   `json:"peak_percent"`
	USD            float64   `json:"usd"`
	Tokens         int64     `json:"tokens"`
	Requests       int       `json:"requests"`
	CapacityUSD    *float64  `json:"capacity_usd,omitempty"`
	CapacityTokens *float64  `json:"capacity_tokens,omitempty"`
	Samples        int       `json:"samples"`
	Confidence     string    `json:"confidence"`
}

// Forecast6Input is everything known about one window of one account.
type Forecast6Input struct {
	Provider string
	Window   string
	Seconds  int64
	Obs      []QuotaObs
	Spend    []SpendEvent
	Exclude  []TimeRange
	Now      time.Time
}

// Burn lines (all in percent points per hour) and the pace label.
type BurnLines struct {
	TimeProgress      float64    `json:"time_progress"`
	PaceDelta         float64    `json:"pace_delta"`
	Pace              string     `json:"pace"` // fast | slow | on_track
	Sustainable       float64    `json:"sustainable_per_hour"`
	Cumulative        float64    `json:"cumulative_per_hour"`
	Recent            float64    `json:"recent_per_hour"`
	RecentBasis       string     `json:"recent_basis"` // milestones | usd | cumulative
	ExhaustCumulative *time.Time `json:"exhaust_cumulative,omitempty"`
	ExhaustRecent     *time.Time `json:"exhaust_recent,omitempty"`
	AnchorHours       float64    `json:"anchor_hours"`
	ElapsedHours      float64    `json:"elapsed_hours"`
	ProjectedAtReset  float64    `json:"projected_at_reset"` // used % at reset at the recent pace
	ProjectedLow      float64    `json:"projected_low"`      // min / max of recent and cumulative pace
	ProjectedHigh     float64    `json:"projected_high"`
}

// Forecast6Result extends WindowForecast with the 0.6 fields.
type Forecast6Result struct {
	WindowForecast
	CapacityP25     *float64        `json:"capacity_p25,omitempty"`
	CapacityP75     *float64        `json:"capacity_p75,omitempty"`
	CapacityTokens  *float64        `json:"capacity_tokens,omitempty"`
	TokensP25       *float64        `json:"capacity_tokens_p25,omitempty"`
	TokensP75       *float64        `json:"capacity_tokens_p75,omitempty"`
	RemainingTokens *float64        `json:"remaining_tokens,omitempty"`
	UsedTokens      int64           `json:"used_tokens"`
	Samples         int             `json:"samples"`
	SampleSpan      float64         `json:"sample_span"`
	SampleCycles    int             `json:"sample_cycles"`
	Confidence      string          `json:"confidence"`
	ConfidenceCap   string          `json:"confidence_cap,omitempty"` // why it was capped
	Coverage        string          `json:"coverage"`
	Burn            *BurnLines      `json:"burn,omitempty"`
	Rejected        int             `json:"rejected_readings"`
	Pending         *QuotaObs       `json:"pending,omitempty"`
	Milestones      []Milestone     `json:"milestones"`
	CapacityBasis   string          `json:"capacity_basis,omitempty"` // cycle | blend | prior | ratio
	PriorUSD        *float64        `json:"prior_usd,omitempty"`
	PriorCycles     int             `json:"prior_cycles,omitempty"`
	QuotaChange     *float64        `json:"quota_change,omitempty"` // capacity vs previous cycles when > ±30 %
	Backtest        *BacktestResult `json:"backtest,omitempty"`
	Ledger          []LedgerCycle   `json:"-"`
	CycleObs        []QuotaObs      `json:"-"`
}

// Forecast6 computes one window with the 0.6 method, falling back to the
// ratio when no milestone interval exists yet.
func Forecast6(in Forecast6Input) Forecast6Result {
	cycles, rejected, pending := DetectCycles(in.Obs, in.Seconds)
	out := Forecast6Result{Rejected: rejected, Pending: pending, Milestones: []Milestone{}, Coverage: CoverageUnknown, Confidence: ConfInsufficient}
	if len(cycles) == 0 {
		out.WindowForecast = WindowForecast{Risk: "unknown", CapacityNote: NoteNoPercent}
		return out
	}
	spend := newSpendIndex(in.Spend)
	window := time.Duration(in.Seconds) * time.Second
	cur := cycles[len(cycles)-1]
	latest := cur.Obs[len(cur.Obs)-1]
	reset := cur.ResetAt
	if reset.IsZero() && window > 0 {
		reset = latest.At.Add(window)
	}
	start := cur.Start
	if !cur.Early && window > 0 {
		start = reset.Add(-window)
	}
	// Base (0.5) forecast for USD burn, staleness and the fallback ratio.
	used := latest.Used
	base := WindowInput{Seconds: in.Seconds, ResetAt: reset, UsedPercent: &used, PercentSource: latest.Source, ObservedAt: latest.At, Now: in.Now}
	baseStart := start
	if !in.Now.Before(reset) && window > 0 { // rolled over without a new reading
		next := reset
		for !in.Now.Before(next) {
			next = next.Add(window)
		}
		baseStart = next.Add(-window)
	}
	usdNow, tokNow, _ := spend.between(baseStart, in.Now.Add(time.Second))
	base.UsedUSD = usdNow
	for _, e := range in.Spend {
		if e.Unpriced && !e.At.Before(baseStart) && !e.At.After(in.Now) {
			base.Unpriced++
		}
	}
	slice := RecentSlice(in.Seconds)
	sliceStart := in.Now.Add(-slice)
	if sliceStart.Before(baseStart) {
		sliceStart = baseStart
	}
	if span := in.Now.Sub(sliceStart); span >= 5*time.Minute {
		base.RecentUSD, _, _ = spend.between(sliceStart, in.Now.Add(time.Second))
		base.RecentSlice = span
	}
	for k := len(cycles) - 2; k >= 0 && len(base.History) < 6; k-- {
		c := cycles[k]
		cu, _, _ := spend.between(c.Start, c.Obs[len(c.Obs)-1].At.Add(time.Second))
		if capacity, ok := CycleCapacity(peak(c.Obs), cu); ok {
			base.History = append(base.History, capacity)
		}
	}
	out.WindowForecast = Forecast(base)
	out.UsedTokens = tokNow
	if !out.Stale {
		out.Milestones = append(out.Milestones, milestones(cur.Obs)...)
		out.CycleObs = cur.Obs
	}

	// Coverage over the recent cycles.
	recent := cycles
	if len(recent) > ledgerMaxCycles {
		recent = recent[len(recent)-ledgerMaxCycles:]
	}
	out.Coverage = Coverage(recent, spend, in.Exclude)

	// Capacity (0.7): the current cycle's milestone samples once it has
	// priorCutover of them; before that a log-normal blend with the previous
	// cycles' capacity (random-walk prior, σ = priorSigma per cycle), so a
	// fresh reset starts from last week's value instead of jumping around.
	curS := cycleSamples(cur, spend, in.Exclude)
	prior := priorCapacity(cycles[:len(cycles)-1], spend, in.Exclude)
	out.Samples, out.SampleSpan = len(curS.usd), round6(curS.span)
	if len(curS.usd) > 0 {
		out.SampleCycles = 1
	}
	if prior != nil {
		out.PriorUSD, out.PriorCycles = ptr(round6(prior.usd)), prior.cycles
		out.SampleCycles += prior.cycles
	}
	switch {
	case out.Coverage == CoverageMixed:
		out.CapacityUSD, out.RemainingUSD, out.CapacitySource, out.CapacityNote = nil, nil, "", "mixed_usage"
		out.Confidence = ConfInsufficient
	case len(curS.usd) >= priorCutover || (prior == nil && len(curS.usd) > 0):
		out.CapacityUSD, out.CapacitySource, out.CapacityBasis = ptr(round6(Quantile(curS.usd, 0.5))), "milestones", "cycle"
		out.CapacityP25, out.CapacityP75 = ptr(round6(Quantile(curS.usd, 0.25))), ptr(round6(Quantile(curS.usd, 0.75)))
		out.CapacityTokens = ptr(math.Round(Quantile(curS.tokens, 0.5)))
		out.TokensP25, out.TokensP75 = ptr(math.Round(Quantile(curS.tokens, 0.25))), ptr(math.Round(Quantile(curS.tokens, 0.75)))
		out.CapacityNote = ""
		out.Confidence = Confidence(len(curS.usd), curS.span)
		if prior != nil && len(curS.usd) >= quotaChangeMinSamples {
			if r := *out.CapacityUSD/prior.usd - 1; math.Abs(r) > quotaChangeRatio {
				out.QuotaChange = ptr(round6(r))
			}
		}
	case prior != nil:
		usd, ulo, uhi := blendLog(prior.usd, curS.usd)
		tok, tlo, thi := blendLog(prior.tokens, curS.tokens)
		out.CapacityUSD, out.CapacityP25, out.CapacityP75 = ptr(round6(usd)), ptr(round6(ulo)), ptr(round6(uhi))
		if prior.tokens > 0 {
			out.CapacityTokens, out.TokensP25, out.TokensP75 = ptr(math.Round(tok)), ptr(math.Round(tlo)), ptr(math.Round(thi))
		}
		out.CapacitySource, out.CapacityNote = "milestones", ""
		if len(curS.usd) == 0 {
			out.CapacityBasis, out.Confidence = "prior", ConfLow
		} else {
			out.CapacityBasis = "blend"
			out.Confidence = capConf(Confidence(len(curS.usd)+prior.samples, curS.span+prior.span), ConfMedium)
		}
		out.ConfidenceCap = "prior"
	case out.CapacityUSD != nil:
		out.Confidence = ConfLow // ratio / history fallback
		out.CapacityBasis = "ratio"
		if out.UsedPercent != nil && *out.UsedPercent > 0 {
			out.CapacityTokens = ptr(math.Round(float64(tokNow) / (*out.UsedPercent / 100)))
		}
	}
	// Rolling backtest over finished cycles: predicted vs actual peak.
	if bt := Backtest(cycles[:len(cycles)-1], in.Seconds); bt.Points > 0 {
		out.Backtest = &bt
		if bt.Points >= backtestMinPoints && bt.MAPE > backtestMaxMAPE && confRank[out.Confidence] > confRank[ConfMedium] {
			out.Confidence, out.ConfidenceCap = ConfMedium, "backtest_error"
		}
	}
	if out.Coverage == CoverageUnknown && confRank[out.Confidence] > confRank[ConfMedium] {
		out.Confidence, out.ConfidenceCap = ConfMedium, "coverage_unknown"
	}
	if in.Provider == "xai" && confRank[out.Confidence] > confRank[ConfMedium] {
		out.Confidence, out.ConfidenceCap = ConfMedium, "sampled_endpoint"
	}
	if out.CapacityUSD != nil && out.RemainingPercent != nil {
		out.RemainingUSD = ptr(round6(*out.CapacityUSD * *out.RemainingPercent / 100))
	}
	if out.CapacityTokens != nil && out.RemainingPercent != nil {
		out.RemainingTokens = ptr(math.Round(*out.CapacityTokens * *out.RemainingPercent / 100))
	}

	// Burn lines.
	if out.UsedPercent != nil && !out.Stale && window > 0 {
		elapsed := in.Now.Sub(start)
		left := reset.Sub(in.Now)
		if elapsed > 0 && left > 0 {
			b := &BurnLines{TimeProgress: round6(clamp(float64(elapsed)/float64(window)*100, 0, 100))}
			u := *out.UsedPercent
			b.PaceDelta = round6(u - b.TimeProgress)
			switch {
			case b.PaceDelta > paceBand:
				b.Pace = "fast"
			case b.PaceDelta < -paceBand:
				b.Pace = "slow"
			default:
				b.Pace = "on_track"
			}
			b.Sustainable = round6(*out.RemainingPercent / left.Hours())
			b.Cumulative = round6(u / elapsed.Hours())
			if u > 0 {
				at := start.Add(time.Duration(float64(elapsed) * 100 / u))
				b.ExhaustCumulative = &at
			}
			// Recent (0.7, as cpa-quota-estimator): anchor on the last
			// milestone at least AnchorSpan before the latest one, so a short
			// burst no longer extrapolates over the whole window.
			anchor := AnchorSpan(in.Seconds)
			switch rate, ok := anchoredRate(out.Milestones, anchor); {
			case ok:
				b.Recent, b.RecentBasis = round6(rate), "milestones"
			case out.BurnUSDPerHour > 0 && out.CapacityUSD != nil && *out.CapacityUSD > 0 && in.Now.Sub(start) >= anchor:
				b.Recent, b.RecentBasis = round6(out.BurnUSDPerHour*100 / *out.CapacityUSD), "usd"
			default:
				b.Recent, b.RecentBasis = b.Cumulative, "cumulative"
			}
			b.AnchorHours = round6(anchor.Hours())
			if b.Recent > 0 {
				at := in.Now.Add(time.Duration(*out.RemainingPercent / b.Recent * float64(time.Hour)))
				b.ExhaustRecent = &at
			}
			lh := left.Hours()
			b.ProjectedAtReset = round6(math.Min(999, u+b.Recent*lh))
			lo, hi := math.Min(b.Recent, b.Cumulative), math.Max(b.Recent, b.Cumulative)
			b.ProjectedLow, b.ProjectedHigh = round6(math.Min(999, u+lo*lh)), round6(math.Min(999, u+hi*lh))
			b.ElapsedHours = round6(elapsed.Hours())
			out.Burn = b
			// Exhaustion and risk follow the anchored recent line.
			if u < 100 {
				out.ExhaustAt, out.ExhaustBasis = b.ExhaustRecent, "recent_"+b.RecentBasis
				switch {
				case b.ExhaustRecent == nil || !b.ExhaustRecent.Before(reset):
					out.Risk = "low"
				case reset.Sub(*b.ExhaustRecent) >= time.Duration(float64(window)*riskEarlyFrac):
					out.Risk = "high"
				default:
					out.Risk = "medium"
				}
			}
		}
	}

	// Ledger of finished cycles.
	for k := 0; k < len(cycles); k++ {
		c := cycles[k]
		end := c.ResetAt
		if k+1 < len(cycles) && cycles[k+1].Early {
			end = cycles[k+1].Start
		}
		if end.IsZero() || end.After(in.Now) {
			continue
		}
		cs := c.Start
		usd, tok, n := spend.between(cs, end)
		e := LedgerCycle{Window: in.Window, Start: cs, End: end, Early: c.Early, PeakPercent: round6(peak(c.Obs)), USD: round6(usd), Tokens: tok, Requests: n}
		s := cycleSamples(c, spend, in.Exclude)
		e.Samples = len(s.usd)
		switch {
		case len(s.usd) > 0:
			e.CapacityUSD, e.CapacityTokens = ptr(round6(Quantile(s.usd, 0.5))), ptr(math.Round(Quantile(s.tokens, 0.5)))
			e.Confidence = Confidence(len(s.usd), s.span)
		default:
			if capacity, ok := CycleCapacity(e.PeakPercent, usd); ok {
				e.CapacityUSD, e.Confidence = ptr(round6(capacity)), ConfLow
			} else {
				e.Confidence = ConfInsufficient
			}
		}
		out.Ledger = append(out.Ledger, e)
	}
	return out
}

// AnchorSpan is how far back the recent pace looks: 24 h for weekly and
// longer windows, 90 min for the 5-hour window, window/7 otherwise.
func AnchorSpan(seconds int64) time.Duration {
	w := time.Duration(seconds) * time.Second
	switch {
	case w >= 7*24*time.Hour:
		return 24 * time.Hour
	case w == 5*time.Hour:
		return 90 * time.Minute
	}
	d := w / 7
	return time.Duration(math.Min(math.Max(float64(d), float64(time.Hour)), float64(24*time.Hour)))
}

// anchoredRate: percent points per hour between the latest milestone and the
// last milestone at or before latest-span (else the first one). Needs a
// ≥10 min, positive interval.
func anchoredRate(ms []Milestone, span time.Duration) (float64, bool) {
	if len(ms) < 2 {
		return 0, false
	}
	latest := ms[len(ms)-1]
	anchor := ms[0]
	for i := 0; i < len(ms)-1; i++ {
		if !ms[i].At.After(latest.At.Add(-span)) {
			anchor = ms[i]
		}
	}
	dt := latest.At.Sub(anchor.At)
	dp := latest.Percent - anchor.Percent
	if dt < 10*time.Minute || dp <= 0 {
		return 0, false
	}
	return dp / dt.Hours(), true
}

type priorCap struct {
	usd, tokens, span float64
	samples, cycles   int
}

// priorCapacity: weighted (3:2:1, newest first) geometric mean of the
// previous cycles' median capacity.
func priorCapacity(prev []Cycle, spend spendIndex, exclude []TimeRange) *priorCap {
	var p priorCap
	var wsum, lu, lt, wt float64
	for k := len(prev) - 1; k >= 0 && p.cycles < priorMaxCycles && k >= len(prev)-ledgerMaxCycles; k-- {
		s := cycleSamples(prev[k], spend, exclude)
		if len(s.usd) == 0 {
			continue
		}
		w := float64(priorMaxCycles - p.cycles)
		lu += w * math.Log(Quantile(s.usd, 0.5))
		wsum += w
		if t := Quantile(s.tokens, 0.5); t > 0 {
			lt += w * math.Log(t)
			wt += w
		}
		p.samples += len(s.usd)
		p.span += s.span
		p.cycles++
	}
	if p.cycles == 0 {
		return nil
	}
	p.usd = math.Exp(lu / wsum)
	if wt > 0 {
		p.tokens = math.Exp(lt / wt)
	}
	return &p
}

// blendLog combines a prior value (σ = priorSigma) with current samples
// (each σ = priorSigma) in log space; returns median, P25, P75.
func blendLog(prior float64, samples []float64) (float64, float64, float64) {
	if prior <= 0 {
		return 0, 0, 0
	}
	prec0 := 1 / (priorSigma * priorSigma)
	mu, prec := math.Log(prior)*prec0, prec0
	var pos []float64
	for _, v := range samples {
		if v > 0 {
			pos = append(pos, v)
		}
	}
	if len(pos) > 0 {
		pd := float64(len(pos)) / (priorSigma * priorSigma)
		mu += math.Log(Quantile(pos, 0.5)) * pd
		prec += pd
	}
	mu /= prec
	sd := 1 / math.Sqrt(prec)
	return math.Exp(mu), math.Exp(mu - 0.674*sd), math.Exp(mu + 0.674*sd)
}

// BacktestResult: how well the anchored pace predicted the final peak of
// finished cycles, checked at 25 %, 50 % and 75 % of each window.
type BacktestResult struct {
	Cycles int     `json:"cycles"`
	Points int     `json:"points"`
	MAPE   float64 `json:"mape"` // mean |predicted-actual| / actual
}

// Backtest replays the last backtestMaxCycles finished cycles.
func Backtest(finished []Cycle, seconds int64) BacktestResult {
	var r BacktestResult
	window := time.Duration(seconds) * time.Second
	if window <= 0 {
		return r
	}
	span := AnchorSpan(seconds)
	var sum float64
	for k := len(finished) - 1; k >= 0 && r.Cycles < backtestMaxCycles; k-- {
		c := finished[k]
		if c.ResetAt.IsZero() || len(c.Obs) < 3 {
			continue
		}
		start := c.ResetAt.Add(-window)
		if c.Early {
			start = c.Start
		}
		actual := math.Min(100, peak(c.Obs))
		if actual < 5 {
			continue
		}
		used := false
		for _, f := range []float64{0.25, 0.5, 0.75} {
			t := start.Add(time.Duration(float64(c.ResetAt.Sub(start)) * f))
			var seen []QuotaObs
			for _, o := range c.Obs {
				if !o.At.After(t) {
					seen = append(seen, o)
				}
			}
			if len(seen) == 0 {
				continue
			}
			u := seen[len(seen)-1].Used
			rate, ok := anchoredRate(milestones(seen), span)
			if el := t.Sub(start).Hours(); !ok && el > 0 {
				rate = u / el
			}
			pred := math.Min(100, u+rate*c.ResetAt.Sub(t).Hours())
			sum += math.Abs(pred-actual) / actual
			r.Points++
			used = true
		}
		if used {
			r.Cycles++
		}
	}
	if r.Points > 0 {
		r.MAPE = round6(sum / float64(r.Points))
	}
	return r
}
