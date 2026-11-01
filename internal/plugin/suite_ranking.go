package plugin

// CPA Suite: usage ranking by API key and by upstream account, plus page-only
// anomaly flags (no notifications in this build). Read-only queries against
// the upstream request_events table and the client module's daily counters.

import (
	"database/sql"
	"net/http"
	"sort"
	"time"

	"cpa-key-billing/internal/suite"
)

const routeSuiteRanking = "/suite/ranking"

// Key usage thresholds live in suite.AlertSettings (0.7). Client block
// spikes: today vs the 7 days before.
const (
	blockSpikeRatio = 3.0
	blockSpikeHighX = 6.0
	blockSpikeMin   = 20
	blockSpikeHigh  = 200
)

type rankRow struct {
	ID       string  `json:"id"`
	Provider string  `json:"provider,omitempty"`
	Name     string  `json:"name,omitempty"`
	USD      float64 `json:"usd"`
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
	Failed   int64   `json:"failed"`
}

type anomaly struct {
	Kind     string  `json:"kind"` // key_usage | key_new | client_blocks
	Scope    string  `json:"scope,omitempty"`
	Metric   string  `json:"metric"`
	Recent   float64 `json:"recent"`
	Baseline float64 `json:"baseline"`
	Ratio    float64 `json:"ratio,omitempty"`
	// ActiveDays: baseline days with real use (0.7).
	ActiveDays int    `json:"active_days,omitempty"`
	Severity   string `json:"severity"`
}

func (a *App) suiteRanking(req ManagementRequest) ManagementResponse {
	mode := a.suite.ModuleMode("ranking")
	out := map[string]any{"mode": mode, "keys": []rankRow{}, "accounts": []rankRow{}, "anomalies": []anomaly{}}
	if mode != suite.ModeOn {
		return JSONResponse(http.StatusOK, out)
	}
	now := time.Now().UTC()
	to, errTo := time.Parse(time.RFC3339Nano, req.Query.Get("to"))
	from, errFrom := time.Parse(time.RFC3339Nano, req.Query.Get("from"))
	if errTo != nil || errFrom != nil || !from.Before(to) || to.Sub(from) > 92*24*time.Hour {
		to, from = now, now.Add(-24*time.Hour)
	}
	out["from"], out["to"] = from, to
	names := map[string]string{}
	if files, err := a.listHostAuthFiles(); err == nil {
		for _, f := range files {
			names[f.AuthIndex] = f.Name
		}
	}
	err := a.suite.ReadMain(func(db *sql.DB) error {
		keys, err := rankQuery(db, "scope", from, to)
		if err != nil {
			return err
		}
		accounts, err := rankQuery(db, "auth_index", from, to)
		if err != nil {
			return err
		}
		for i := range accounts {
			accounts[i].Name = names[accounts[i].ID]
		}
		out["keys"], out["accounts"] = keys, accounts
		anomalies, err := keyAnomalies(db, now, a.suite.AlertSettings().Settings)
		if err != nil {
			return err
		}
		out["anomalies"] = append(anomalies, blockAnomalies(a.suite.ClientDaily(), now)...)
		return nil
	})
	if err != nil {
		out["error"] = err.Error()
	}
	return JSONResponse(http.StatusOK, out)
}

func rankQuery(db *sql.DB, column string, from, to time.Time) ([]rankRow, error) {
	provider := "''"
	if column == "auth_index" {
		provider = "MAX(provider)"
	}
	rows, err := db.Query(`SELECT `+column+`, `+provider+`, COALESCE(SUM(total_usd),0),
		COALESCE(SUM(uncached_input_tokens + cache_read_tokens + cache_write_tokens + billed_output_tokens),0), COUNT(*), COALESCE(SUM(failed),0)
		FROM request_events WHERE at >= ? AND at < ? AND `+column+` != '' GROUP BY `+column, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []rankRow{}
	for rows.Next() {
		var r rankRow
		if err := rows.Scan(&r.ID, &r.Provider, &r.USD, &r.Tokens, &r.Requests, &r.Failed); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].USD != list[j].USD {
			return list[i].USD > list[j].USD
		}
		return list[i].Requests > list[j].Requests
	})
	return list, rows.Err()
}

// keyDay is one baseline day of a key (days counted back from the recent window).
type keyDay struct {
	USD  float64
	Reqs int64
}

func keyAnomalies(db *sql.DB, now time.Time, st suite.AlertSettings) ([]anomaly, error) {
	recentFrom, baseFrom := now.Add(-24*time.Hour), now.Add(-8*24*time.Hour)
	day := int64(24 * time.Hour)
	rows, err := db.Query(`SELECT scope, CASE WHEN at >= ? THEN -1 ELSE (at - ?) / ? END AS d, COALESCE(SUM(total_usd),0), COUNT(*)
		FROM request_events WHERE at >= ? AND scope != '' GROUP BY scope, d`,
		recentFrom.UnixNano(), baseFrom.UnixNano(), day, baseFrom.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type keyStat struct {
		usd  float64
		reqs int64
		days []keyDay
	}
	stats := map[string]*keyStat{}
	var order []string
	for rows.Next() {
		var scope string
		var d, reqs int64
		var usd float64
		if err := rows.Scan(&scope, &d, &usd, &reqs); err != nil {
			return nil, err
		}
		k := stats[scope]
		if k == nil {
			k = &keyStat{}
			stats[scope] = k
			order = append(order, scope)
		}
		if d < 0 {
			k.usd, k.reqs = usd, reqs
		} else {
			k.days = append(k.days, keyDay{USD: usd, Reqs: reqs})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)
	var list []anomaly
	for _, scope := range order {
		k := stats[scope]
		list = append(list, evaluateKeyAnomaly(scope, k.usd, k.reqs, k.days, st)...)
	}
	return list, nil
}

// evaluateKeyAnomaly compares the last 24 h with the key's average over its
// ACTIVE baseline days (0.7): a key needs UsageMinActiveDays active days
// before a ratio means anything, and the jump must also be large in absolute
// terms. Keys without enough history are only noted (info) when they spend a
// lot at once.
func evaluateKeyAnomaly(scope string, usd float64, reqs int64, days []keyDay, st suite.AlertSettings) []anomaly {
	var active int
	var sumUSD float64
	var sumReqs int64
	for _, d := range days {
		if d.USD >= st.UsageActiveUSD || float64(d.Reqs) >= st.UsageActiveReqs {
			active++
			sumUSD += d.USD
			sumReqs += d.Reqs
		}
	}
	if float64(active) < st.UsageMinActiveDays {
		if usd >= st.UsageNewKeyUSD {
			return []anomaly{{Kind: "key_new", Scope: scope, Metric: "usd", Recent: usd, ActiveDays: active, Severity: "info"}}
		}
		return nil
	}
	avgUSD, avgReqs := sumUSD/float64(active), float64(sumReqs)/float64(active)
	if avgUSD >= st.UsageMinBaseUSD && usd/avgUSD >= st.UsageRatio && usd-avgUSD >= st.UsageMinDeltaUSD {
		ratio := usd / avgUSD
		sev := "medium"
		if ratio >= st.UsageHighRatio && usd-avgUSD >= st.UsageHighDeltaUSD {
			sev = "high"
		}
		return []anomaly{{Kind: "key_usage", Scope: scope, Metric: "usd", Recent: usd, Baseline: avgUSD, Ratio: ratio, ActiveDays: active, Severity: sev}}
	}
	if avgReqs >= st.UsageActiveReqs && float64(reqs)/avgReqs >= st.UsageRatio && float64(reqs)-avgReqs >= st.UsageMinDeltaReqs {
		ratio := float64(reqs) / avgReqs
		sev := "medium"
		if ratio >= st.UsageHighRatio {
			sev = "high"
		}
		return []anomaly{{Kind: "key_usage", Scope: scope, Metric: "requests", Recent: float64(reqs), Baseline: avgReqs, Ratio: ratio, ActiveDays: active, Severity: sev}}
	}
	return nil
}

// blockAnomalies: today's client blocks vs the 7 days before (Beijing dates,
// as the client module counts them).
func blockAnomalies(daily map[string]suite.ClientDay, now time.Time) []anomaly {
	if daily == nil {
		return nil
	}
	zone := time.FixedZone("CST", 8*3600)
	today := now.In(zone)
	recent := float64(daily[today.Format("2006-01-02")].Blocked)
	base := 0.0
	for i := 1; i <= 7; i++ {
		base += float64(daily[today.AddDate(0, 0, -i).Format("2006-01-02")].Blocked)
	}
	base /= 7
	if recent < blockSpikeMin || (base > 0 && recent < blockSpikeRatio*base) {
		return nil
	}
	a := anomaly{Kind: "client_blocks", Metric: "blocked", Recent: recent, Baseline: base, Severity: "medium"}
	if base > 0 {
		a.Ratio = recent / base
	}
	if recent >= blockSpikeHigh || (base > 0 && a.Ratio >= blockSpikeHighX) {
		a.Severity = "high"
	}
	return []anomaly{a}
}
