package suite

import (
	"math"
	"sort"
	"time"
)

// Passive degradation monitor (0.6). Pure statistics over metadata that
// CPA's usage.handle already delivers for real traffic; nothing here sends a
// request. Every verdict is a heuristic: "deviates from this bucket's own
// history", never proof that the upstream degraded a model.

// MonitorSample is one upstream call, numbers only (no prompt/response text).
type MonitorSample struct {
	At            time.Time `json:"at"`
	AuthIndex     string    `json:"auth_index"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	ResponseModel string    `json:"response_model"`
	Effort        string    `json:"effort"`
	Stream        bool      `json:"stream"`
	Failed        bool      `json:"failed"`
	Status        int       `json:"status"`
	ErrKind       string    `json:"err_kind"` // rate_limit | server | auth | refusal | truncated | other
	Input         int64     `json:"input"`    // prompt tokens (0.7; 0 = not recorded)
	Output        int64     `json:"output"`
	Reasoning     int64     `json:"reasoning"`
	LatencyMS     int64     `json:"latency_ms"`
	TTFTMS        int64     `json:"ttft_ms"`
}

// Monitor thresholds (approved plan: last 24h vs the prior 7 days).
const (
	MonitorRecent        = 24 * time.Hour
	MonitorBaseline      = 7 * 24 * time.Hour
	MonitorMinSamples    = 50
	MonitorPValue        = 0.01
	MonitorReasoningDrop = -0.40 // relative change of the (input-stratified) median
	MonitorOutputDrop    = -0.35
	MonitorShareDrop     = -0.40 // reasoning / output per call
	MonitorSoftPValue    = 0.001
	MonitorCliffDelta    = -0.33 // medium effect size
	MonitorStratumMin    = 15
	MonitorSegment       = 6 * time.Hour
	MonitorSegmentMin    = 15
	MonitorSegmentsNeed  = 3
	MonitorErrorAbs      = 0.03 // +3 percentage points …
	MonitorErrorRatio    = 2.0  // … and at least 2×
	MonitorMismatchMed   = 0.01
	MonitorMismatchHigh  = 0.05
	MonitorRetention     = 14 * 24 * time.Hour
)

// MonitorSignal is one compared metric.
type MonitorSignal struct {
	ID       string   `json:"id"` // model_mismatch | reasoning | output | ttft | per_token | errors | truncated | refusal
	Recent   *float64 `json:"recent"`
	Baseline *float64 `json:"baseline"`
	Change   *float64 `json:"change,omitempty"` // relative (−0.4 = −40%) or absolute points for rates
	PValue   *float64 `json:"p_value,omitempty"`
	State    string   `json:"state"` // ok | deviates | insufficient | unavailable
	Detail   string   `json:"detail,omitempty"`
	Kind     string   `json:"kind,omitempty"`   // hard | soft | info
	Effect   *float64 `json:"effect,omitempty"` // Cliff's delta (soft)
	Strata   int      `json:"strata,omitempty"` // input strata compared (0 = unstratified)
	SegDev   int      `json:"segments_deviating,omitempty"`
	SegTotal int      `json:"segments_total,omitempty"`
}

// MonitorBucket groups calls that are comparable with each other.
type MonitorBucket struct {
	AuthIndex      string          `json:"auth_index"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	Effort         string          `json:"effort"`
	Stream         bool            `json:"stream"`
	RecentCount    int             `json:"recent_count"`
	BaselineCount  int             `json:"baseline_count"`
	Signals        []MonitorSignal `json:"signals"`
	Deviating      int             `json:"deviating"`
	Hard           int             `json:"hard"`     // deviating hard signals
	Soft           bool            `json:"soft"`     // reasoning / output / share deviates persistently
	Verdict        string          `json:"verdict"`  // ok | observe | suspect | insufficient
	Severity       string          `json:"severity"` // "" | medium | high
	ReasoningZero  bool            `json:"reasoning_unreported,omitempty"`
	CandyFailed    bool            `json:"candy_failed,omitempty"` // a failed candy test in the last 24 h upgraded observe → suspect
	ErrorBreakdown map[string]int  `json:"error_breakdown,omitempty"`
}

type bucketKey struct {
	auth, provider, model, effort string
	stream                        bool
}

// EvaluateMonitor buckets samples and compares [now-24h, now) with the 7 days before.
func EvaluateMonitor(samples []MonitorSample, now time.Time) []MonitorBucket {
	recentFrom, baseFrom := now.Add(-MonitorRecent), now.Add(-MonitorRecent-MonitorBaseline)
	type pair struct{ recent, base []MonitorSample }
	groups := map[bucketKey]*pair{}
	for _, s := range samples {
		if s.At.Before(baseFrom) || !s.At.Before(now) {
			continue
		}
		k := bucketKey{s.AuthIndex, s.Provider, s.Model, s.Effort, s.Stream}
		g := groups[k]
		if g == nil {
			g = &pair{}
			groups[k] = g
		}
		if s.At.Before(recentFrom) {
			g.base = append(g.base, s)
		} else {
			g.recent = append(g.recent, s)
		}
	}
	out := make([]MonitorBucket, 0, len(groups))
	for k, g := range groups {
		if len(g.recent) == 0 {
			continue
		}
		out = append(out, evaluateBucket(k, g.recent, g.base, recentFrom))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank(a) != rank(b) {
			return rank(a) > rank(b)
		}
		if a.RecentCount != b.RecentCount {
			return a.RecentCount > b.RecentCount
		}
		return a.Model+a.Effort < b.Model+b.Effort
	})
	return out
}

func rank(b MonitorBucket) float64 {
	switch {
	case b.Severity == "high":
		return 3
	case b.Severity == "medium":
		return 2
	case b.Verdict == "observe":
		return 1.5
	case b.Verdict == "ok":
		return 1
	}
	return 0
}

func evaluateBucket(k bucketKey, recent, base []MonitorSample, recentFrom time.Time) MonitorBucket {
	b := MonitorBucket{AuthIndex: k.auth, Provider: k.provider, Model: k.model, Effort: k.effort, Stream: k.stream,
		RecentCount: len(recent), BaselineCount: len(base), ErrorBreakdown: map[string]int{}}
	for _, s := range recent {
		if s.Failed {
			b.ErrorBreakdown[s.ErrKind]++
		}
	}
	enough := len(recent) >= MonitorMinSamples && len(base) >= MonitorMinSamples
	ok := func(list []MonitorSample) []MonitorSample {
		var out []MonitorSample
		for _, s := range list {
			if !s.Failed {
				out = append(out, s)
			}
		}
		return out
	}
	rOK, bOK := ok(recent), ok(base)

	// Model mismatch: the response reported a different model than was sent.
	mm := func(list []MonitorSample) (float64, int) {
		n, bad := 0, 0
		for _, s := range list {
			if s.ResponseModel == "" {
				continue
			}
			n++
			if !SameModel(s.Model, s.ResponseModel) {
				bad++
			}
		}
		if n == 0 {
			return 0, 0
		}
		return float64(bad) / float64(n), n
	}
	rm, rn := mm(rOK)
	bm, _ := mm(bOK)
	mismatch := MonitorSignal{ID: "model_mismatch", Recent: fp(rm), Baseline: fp(bm), State: "ok"}
	switch {
	case rn == 0:
		mismatch = MonitorSignal{ID: "model_mismatch", State: "unavailable", Detail: "no_response_model"}
	case rn < MonitorMinSamples:
		mismatch.State = "insufficient"
	case rm > MonitorMismatchMed && rm > bm*2:
		mismatch.State = "deviates"
	}
	mismatch.Kind = "hard"
	b.Signals = append(b.Signals, mismatch)

	// Soft signals (0.7): reasoning and output length depend on the task
	// mix, so they are compared within input-length strata, need a medium
	// effect size besides a small p-value, and must persist across the 6-hour
	// segments of the last 24 h. Together they count as ONE soft signal.
	bSoft := cleanBaseline(bOK)
	rr, br := field(rOK, func(s MonitorSample) float64 { return float64(s.Reasoning) }), field(bSoft, func(s MonitorSample) float64 { return float64(s.Reasoning) })
	reasoningKnown := !(allZero(rr) && allZero(br))
	var soft []MonitorSignal
	if reasoningKnown {
		soft = append(soft, softSignal("reasoning", rOK, bSoft, func(s MonitorSample) (float64, bool) { return float64(s.Reasoning), true }, MonitorReasoningDrop, enough, recentFrom))
	} else {
		b.ReasoningZero = true
		soft = append(soft, MonitorSignal{ID: "reasoning", State: "unavailable", Detail: "reasoning_unreported", Kind: "soft"})
	}
	soft = append(soft, softSignal("output", rOK, bSoft, func(s MonitorSample) (float64, bool) { return float64(s.Output), true }, MonitorOutputDrop, enough, recentFrom))
	if reasoningKnown {
		soft = append(soft, softSignal("reasoning_share", rOK, bSoft, func(s MonitorSample) (float64, bool) {
			if s.Output <= 0 {
				return 0, false
			}
			return float64(s.Reasoning) / float64(s.Output), true
		}, MonitorShareDrop, enough, recentFrom))
	}
	softStrong, softAny := false, false
	for _, sg := range soft {
		softStrong = softStrong || sg.State == "deviates"
		softAny = softAny || sg.State == "deviates" || sg.State == "notable"
	}
	b.Signals = append(b.Signals, soft...)

	// Latency signals are informative only (network and queueing dominate); never counted.
	ttft := compareMedian("ttft", positive(rOK, func(s MonitorSample) float64 { return float64(s.TTFTMS) }), positive(bOK, func(s MonitorSample) float64 { return float64(s.TTFTMS) }), 0, enough)
	ttft.State, ttft.Kind = infoState(ttft.State), "info"
	b.Signals = append(b.Signals, ttft)
	perToken := func(s MonitorSample) float64 {
		if s.Output <= 0 || s.LatencyMS <= 0 {
			return 0
		}
		return float64(s.LatencyMS-s.TTFTMS) / float64(s.Output)
	}
	pt := compareMedian("per_token", positive(rOK, perToken), positive(bOK, perToken), 0, enough)
	pt.State, pt.Kind = infoState(pt.State), "info"
	b.Signals = append(b.Signals, pt)

	// Hard: error rate (all failure kinds), stream disconnects, error-shaped refusals.
	rate := func(list []MonitorSample, kinds ...string) float64 {
		if len(list) == 0 {
			return 0
		}
		n := 0
		for _, s := range list {
			if s.Failed && (len(kinds) == 0 || contains(kinds, s.ErrKind)) {
				n++
			}
		}
		return float64(n) / float64(len(list))
	}
	re, be := rate(recent), rate(base)
	errSig := MonitorSignal{ID: "errors", Recent: fp(re), Baseline: fp(be), Change: fp(re - be), State: "ok", Kind: "hard"}
	if !enough {
		errSig.State = "insufficient"
	} else if re-be >= MonitorErrorAbs && re >= be*MonitorErrorRatio {
		errSig.State = "deviates"
	}
	b.Signals = append(b.Signals, errSig)
	for _, id := range []string{"truncated", "refusal"} {
		rt, bt := rate(recent, id), rate(base, id)
		s := MonitorSignal{ID: id, Recent: fp(rt), Baseline: fp(bt), Change: fp(rt - bt), State: "ok", Detail: "partial", Kind: "hard"}
		if !enough {
			s.State = "insufficient"
		} else if rt-bt >= MonitorErrorAbs && rt >= bt*MonitorErrorRatio {
			s.State = "deviates"
		}
		b.Signals = append(b.Signals, s)
	}

	for _, s := range b.Signals {
		if s.Kind == "hard" && s.State == "deviates" {
			b.Hard++
		}
	}
	b.Soft = softStrong
	b.Deviating = b.Hard
	if softAny {
		b.Deviating++
	}
	switch {
	case !enough:
		b.Verdict = "insufficient"
	case rm >= MonitorMismatchHigh && mismatch.State == "deviates":
		b.Verdict, b.Severity = "suspect", "high"
	case b.Hard >= 1 && softStrong:
		b.Verdict, b.Severity = "suspect", "medium"
	case b.Hard >= 1 || softAny:
		b.Verdict = "observe"
	default:
		b.Verdict = "ok"
	}
	if len(b.ErrorBreakdown) == 0 {
		b.ErrorBreakdown = nil
	}
	return b
}

// cleanBaseline drops 6-hour baseline segments that were themselves abnormal
// (error rate > 20 % or model mismatch > 5 % with ≥ MonitorSegmentMin calls)
// so the soft comparison is not made against a degraded baseline.
func cleanBaseline(base []MonitorSample) []MonitorSample {
	type seg struct{ n, bad, mm, mmN int }
	segs := map[int64]*seg{}
	key := func(t time.Time) int64 { return t.UnixNano() / int64(MonitorSegment) }
	for _, s := range base {
		g := segs[key(s.At)]
		if g == nil {
			g = &seg{}
			segs[key(s.At)] = g
		}
		g.n++
		if s.ResponseModel != "" {
			g.mmN++
			if !SameModel(s.Model, s.ResponseModel) {
				g.mm++
			}
		}
	}
	out := make([]MonitorSample, 0, len(base))
	for _, s := range base {
		g := segs[key(s.At)]
		if g.n >= MonitorSegmentMin && g.mmN > 0 && float64(g.mm)/float64(g.mmN) > MonitorMismatchHigh {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Input strata (prompt tokens): <2K, 2–16K, 16–64K, >64K.
func stratum(input int64) int {
	switch {
	case input <= 0:
		return -1
	case input < 2000:
		return 0
	case input < 16000:
		return 1
	case input < 64000:
		return 2
	}
	return 3
}

type softVals struct {
	all    []float64
	strata [4][]float64
	known  int
}

func collectSoft(list []MonitorSample, f func(MonitorSample) (float64, bool)) softVals {
	var v softVals
	for _, s := range list {
		x, ok := f(s)
		if !ok {
			continue
		}
		v.all = append(v.all, x)
		if st := stratum(s.Input); st >= 0 {
			v.strata[st] = append(v.strata[st], x)
			v.known++
		}
	}
	return v
}

// CliffDelta is P(a > b) - P(a < b) (negative: a tends to be smaller).
func CliffDelta(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	bs := append([]float64(nil), b...)
	sort.Float64s(bs)
	var gt, lt int
	for _, x := range a {
		lo := sort.SearchFloat64s(bs, x)
		hi := sort.Search(len(bs), func(i int) bool { return bs[i] > x })
		lt += lo
		gt += len(bs) - hi
	}
	return float64(lt-gt) / float64(len(a)*len(b))
}

// stratifiedChange compares medians per stratum (both sides ≥ MonitorStratumMin)
// and weights by the recent calls; falls back to all calls when no stratum
// qualifies or input tokens were not recorded.
func stratifiedChange(r, b softVals, threshold float64) (change, effect float64, strata, down int, ok bool) {
	var wsum float64
	if r.known*2 >= len(r.all) && b.known*2 >= len(b.all) {
		for i := 0; i < 4; i++ {
			rs, bs := r.strata[i], b.strata[i]
			if len(rs) < MonitorStratumMin || len(bs) < MonitorStratumMin {
				continue
			}
			bm := Median(bs)
			if bm <= 0 {
				continue
			}
			c := Median(rs)/bm - 1
			w := float64(len(rs))
			change += c * w
			effect += CliffDelta(rs, bs) * w
			wsum += w
			strata++
			if c <= threshold/2 {
				down++
			}
		}
	}
	if strata > 0 {
		return change / wsum, effect / wsum, strata, down, true
	}
	bm := Median(b.all)
	if bm <= 0 || len(r.all) == 0 {
		return 0, 0, 0, 0, false
	}
	return Median(r.all)/bm - 1, CliffDelta(r.all, b.all), 0, 0, true
}

// softSignal: deviates when the stratified median drops by at least
// threshold with Cliff's delta ≤ MonitorCliffDelta, p < MonitorSoftPValue,
// ≥ 2 strata falling when ≥ 2 were compared, and the drop holds in at least
// MonitorSegmentsNeed of the 6-hour segments. A drop that fails only the
// persistence test is "notable" (observe, never an alert by itself).
func softSignal(id string, rOK, bOK []MonitorSample, f func(MonitorSample) (float64, bool), threshold float64, enough bool, recentFrom time.Time) MonitorSignal {
	s := MonitorSignal{ID: id, State: "ok", Kind: "soft"}
	r, b := collectSoft(rOK, f), collectSoft(bOK, f)
	if len(r.all) == 0 || len(b.all) == 0 {
		s.State = "insufficient"
		return s
	}
	s.Recent, s.Baseline = fp(Median(r.all)), fp(Median(b.all))
	if !enough {
		s.State = "insufficient"
		return s
	}
	change, effect, strata, down, ok := stratifiedChange(r, b, threshold)
	if !ok {
		s.State = "insufficient"
		return s
	}
	s.Change, s.Effect, s.Strata = fp(round6(change)), fp(round6(effect)), strata
	if strata == 0 {
		s.Detail = "unstratified"
	}
	p := MannWhitneyLess(r.all, b.all)
	s.PValue = fp(p)
	if !(change <= threshold && effect <= MonitorCliffDelta && p < MonitorSoftPValue && (strata < 2 || down >= 2)) {
		return s
	}
	// Persistence over the 6-hour segments of the recent window.
	for i := 0; i < int(MonitorRecent/MonitorSegment); i++ {
		from, to := recentFrom.Add(time.Duration(i)*MonitorSegment), recentFrom.Add(time.Duration(i+1)*MonitorSegment)
		var seg []MonitorSample
		for _, x := range rOK {
			if !x.At.Before(from) && x.At.Before(to) {
				seg = append(seg, x)
			}
		}
		sv := collectSoft(seg, f)
		if len(sv.all) < MonitorSegmentMin {
			continue
		}
		s.SegTotal++
		if c, _, _, _, ok := stratifiedChange(sv, b, threshold); ok && c <= threshold {
			s.SegDev++
		}
	}
	switch {
	case s.SegTotal >= MonitorSegmentsNeed && s.SegDev >= MonitorSegmentsNeed:
		s.State = "deviates"
	case s.SegTotal < MonitorSegmentsNeed && s.SegTotal > 0 && s.SegDev == s.SegTotal:
		s.State, s.Detail = "notable", "weak_persistence"
	default:
		s.State, s.Detail = "notable", "not_persistent"
	}
	return s
}

func infoState(state string) string {
	if state == "deviates" {
		return "notable"
	}
	return state
}

// compareMedian flags a relative drop of the median when it also passes a
// one-sided Mann-Whitney test. threshold 0 = informative comparison only.
func compareMedian(id string, recent, base []float64, threshold float64, enough bool) MonitorSignal {
	s := MonitorSignal{ID: id, State: "ok"}
	if len(recent) == 0 || len(base) == 0 {
		s.State = "insufficient"
		return s
	}
	rm, bm := Median(recent), Median(base)
	s.Recent, s.Baseline = fp(rm), fp(bm)
	if bm > 0 {
		s.Change = fp(rm/bm - 1)
	}
	if !enough {
		s.State = "insufficient"
		return s
	}
	p := MannWhitneyLess(recent, base)
	s.PValue = fp(p)
	if s.Change != nil && p < MonitorPValue && (threshold == 0 && math.Abs(*s.Change) >= 0.3 || threshold < 0 && *s.Change <= threshold) {
		s.State = "deviates"
	}
	if threshold == 0 && s.Change != nil && p >= MonitorPValue {
		// two-sided for informative signals
		if q := MannWhitneyLess(base, recent); q < MonitorPValue && math.Abs(*s.Change) >= 0.3 {
			s.PValue, s.State = fp(q), "deviates"
		}
	}
	return s
}

// MannWhitneyLess returns the one-sided p-value for "a tends to be smaller
// than b" (normal approximation with tie correction).
func MannWhitneyLess(a, b []float64) float64 {
	n1, n2 := float64(len(a)), float64(len(b))
	if n1 == 0 || n2 == 0 {
		return 1
	}
	type v struct {
		x     float64
		first bool
	}
	all := make([]v, 0, len(a)+len(b))
	for _, x := range a {
		all = append(all, v{x, true})
	}
	for _, x := range b {
		all = append(all, v{x, false})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].x < all[j].x })
	var r1, tie float64
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].x == all[i].x {
			j++
		}
		avg := float64(i+j+1) / 2 // ranks are 1-based
		t := float64(j - i)
		tie += t*t*t - t
		for k := i; k < j; k++ {
			if all[k].first {
				r1 += avg
			}
		}
		i = j
	}
	u := r1 - n1*(n1+1)/2
	n := n1 + n2
	sigma := math.Sqrt(n1 * n2 / 12 * ((n + 1) - tie/(n*(n-1))))
	if sigma == 0 {
		return 1
	}
	z := (u - n1*n2/2 + 0.5) / sigma // continuity correction
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// SameModel compares a requested and a reported model name loosely: case,
// a provider prefix and a dated snapshot suffix are ignored.
func SameModel(requested, reported string) bool {
	norm := func(s string) string {
		s = toLower(s)
		if i := lastIndex(s, '/'); i >= 0 {
			s = s[i+1:]
		}
		// strip -YYYY-MM-DD or -YYYYMMDD snapshot suffixes
		for _, n := range []int{11, 9} {
			if len(s) > n && s[len(s)-n] == '-' && isDateish(s[len(s)-n+1:]) {
				s = s[:len(s)-n]
				break
			}
		}
		return s
	}
	a, b := norm(requested), norm(reported)
	return a == b || hasPrefix(b, a+"-") || hasPrefix(a, b+"-")
}

func isDateish(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return len(s) >= 8
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func lastIndex(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func field(list []MonitorSample, f func(MonitorSample) float64) []float64 {
	out := make([]float64, 0, len(list))
	for _, s := range list {
		out = append(out, f(s))
	}
	return out
}

func positive(list []MonitorSample, f func(MonitorSample) float64) []float64 {
	var out []float64
	for _, s := range list {
		if v := f(s); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func allZero(v []float64) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// Median of a copy of v (0 for empty).
func Median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	m := len(c) / 2
	if len(c)%2 == 1 {
		return c[m]
	}
	return (c[m-1] + c[m]) / 2
}

func fp(v float64) *float64 { return &v }
