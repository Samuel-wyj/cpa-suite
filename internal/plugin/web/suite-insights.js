/* CPA Suite read-only insight pages: upstream quota forecast (Codex, Grok)
 * and usage ranking with anomaly badges. Data comes from /suite/forecast and
 * /suite/ranking; nothing here writes or calls an upstream directly. */

const suiteForecast = { value: null, error: "", readAt: 0, task: null, painted: "", refreshing: "" };
const suiteRanking = { value: null, error: "", readAt: 0, task: null, painted: "", range: "today", metric: "usd", rangeRead: "" };

function suiteLoadForecast(force, refresh) {
  if (suiteForecast.task || (!force && Date.now() - suiteForecast.readAt < 60000)) return suiteForecast.task;
  const generation = sessionGeneration;
  suiteForecast.refreshing = refresh || "";
  suiteForecast.task = plugin("GET", "/suite/forecast" + (refresh ? "?" + new URLSearchParams({ refresh }) : "")).then((value) => {
    if (generation !== sessionGeneration) return;
    Object.assign(suiteForecast, { value, error: "", readAt: Date.now() });
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteForecast.error = error.message || String(error);
    })
    .finally(() => {
      suiteForecast.task = null;
      suiteForecast.refreshing = "";
      renderSuiteForecast();
      renderSuiteOverview();
    });
  return suiteForecast.task;
}

function suiteRankRange(mode) { return mode === "today" ? naturalTimeRange("today") : recentTimeRange(mode); }

function suiteLoadRanking(force) {
  const key = suiteRanking.range;
  if (suiteRanking.task || (!force && key === suiteRanking.rangeRead && Date.now() - suiteRanking.readAt < 60000)) return;
  const generation = sessionGeneration;
  suiteRanking.task = plugin("GET", "/suite/ranking?" + new URLSearchParams(suiteRankRange(key))).then((value) => {
    if (generation !== sessionGeneration) return;
    Object.assign(suiteRanking, { value, error: "", readAt: Date.now(), rangeRead: key });
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteRanking.error = error.message || String(error);
    })
    .finally(() => {
      suiteRanking.task = null;
      renderSuiteRanking();
      renderSuiteOverview();
      if ($("suite-nav")) suiteRenderNav(activeTab(false));
    });
}

function suiteAnomalyCount() { return (suiteRanking.value?.anomalies || []).length; }

const SUITE_RISK_TAG = { exhausted: "bad", high: "bad", medium: "warn", low: "ok", unknown: "plain" };
function suiteRiskTag(risk) {
  return el("span", { class: "tag " + (SUITE_RISK_TAG[risk] || "plain"), text: m("suite.risk_" + (risk || "unknown")) });
}
// the card shows the alert level (remaining threshold + pace); a
// fast pace with plenty left is only "pace fast", never "high risk".
function suiteWindowRiskTag(w) {
  if (w.risk === "exhausted" || w.alert_level) return suiteRiskTag(w.risk === "exhausted" ? "exhausted" : w.alert_level);
  if (w.risk === "high" || w.risk === "medium")
    return el("span", { class: "tag plain", "data-tip": m("suite.pace_only_tip"), text: m("suite.pace_only") });
  return suiteRiskTag(w.risk);
}
function suiteProviderName(p) { return p === "xai" ? "Grok" : p === "codex" ? "Codex" : p; }
function suiteWindowName(w) {
  if (w.key === "5h") return m("suite.window_5h");
  if (w.key === "weekly") return m("suite.window_weekly");
  return w.key;
}
function suitePct(v) { return v == null ? "—" : Number(v).toFixed(Number(v) < 10 ? 1 : 0) + "%"; }

const SUITE_CONF_TAG = { high: "ok", medium: "warn", low: "plain", insufficient: "plain" };
function suiteConfTag(w) {
  const c = w.confidence || "insufficient";
  const tip = [m("suite.conf_tip", { v0: int(w.samples || 0), v1: suitePct(w.sample_span || 0), v2: int(w.sample_cycles || 0) })];
  if (w.confidence_cap) tip.push(m("suite.conf_cap_" + w.confidence_cap));
  return el("span", { class: "tag " + (SUITE_CONF_TAG[c] || "plain"), "data-tip": tip.join("\n"), text: m("suite.conf_" + c) });
}
function suiteCoverageTag(cov) {
  if (!cov) return null;
  return el("span", {
    class: "tag " + (cov === "cpa_only" ? "ok" : cov === "mixed" ? "bad" : "plain"),
    "data-tip": m("suite.coverage_tip_" + cov),
    text: m("suite.coverage_" + cov)
  });
}
function suiteTok(v) { return v == null ? "—" : tokens(Math.round(v)); }

function suiteCapacityCell(w) {
  if (w.capacity_usd == null)
    return el("span", {
      class: "tag plain",
      "data-tip": m("suite.note_" + (w.capacity_note || "not_enough_data")),
      text: w.capacity_note === "mixed_usage" ? m("suite.note_short_mixed") : m("suite.not_enough_data")
    });
  const src = w.capacity_source;
  return el(
    "span",
    {},
    el("b", { class: "mono", text: usd(w.capacity_usd) }),
    w.capacity_p25 != null && w.capacity_p75 != null && w.capacity_p75 - w.capacity_p25 >= 0.005
      ? el("span", {
          class: "small muted mono",
          "data-tip": m("suite.band_tip"),
          text: " (" + usd(w.capacity_p25) + "–" + usd(w.capacity_p75) + ")"
        })
      : null,
    " ",
    el("span", {
      class: "tag " + (src === "milestones" ? "ok" : src === "official_ratio" ? "ok" : "warn"),
      "data-tip":
        src === "milestones"
          ? m("suite.capacity_milestones_tip")
          : src === "official_ratio" ? m("suite.capacity_official_tip") : m("suite.capacity_estimated_tip", { v0: int(w.history_cycles) }),
      text: src === "milestones" ? m("suite.milestones") : src === "official_ratio" ? m("suite.official") : m("suite.estimated")
    }),
    " ",
    suiteConfTag(w),
    suiteBasisTag(w),
    w.quota_change != null
      ? el("span", {
          class: "tag warn",
          "data-tip": m("suite.quota_change_tip", { v0: usd(w.prior_usd || 0) }),
          text: m("suite.quota_change", { v0: (w.quota_change > 0 ? "+" : "") + (w.quota_change * 100).toFixed(0) + "%" })
        })
      : null,
    w.backtest && w.backtest.points
      ? el("span", {
          class: "tag " + (w.backtest.mape > 0.3 ? "warn" : "plain"),
          "data-tip": m("suite.backtest_tip", { v0: int(w.backtest.cycles), v1: int(w.backtest.points) }),
          text: m("suite.backtest", { v0: "±" + (w.backtest.mape * 100).toFixed(0) + "%" })
        })
      : null
  );
}

// where the capacity comes from (this cycle / prior blend / prior only).
function suiteBasisTag(w) {
  const b = w.capacity_basis;
  if (!b || b === "ratio") return null;
  return el("span", {
    class: "tag " + (b === "cycle" ? "ok" : "plain"),
    "data-tip": m("suite.basis_tip_" + b, { v0: int(w.prior_cycles || 0), v1: usd(w.prior_usd || 0) }),
    text: m("suite.basis_" + b)
  });
}

// used % at the reset at the anchored recent pace, with the band
// between the cumulative and recent pace.
function suiteProjectionCell(w) {
  const b = w.burn;
  if (!b || w.remaining_percent == null) return "—";
  const p = b.projected_at_reset;
  // past 100 % the percentage itself is meaningless (287 %);
  // show "runs out before reset, around <time>" and keep the number in the tip.
  if (p >= 100) {
    const at = b.exhaust_recent || b.exhaust_cumulative;
    return el(
      "span",
      { class: "suite-projection" },
      el("span", {
        class: "tag " + (w.alert_level ? "bad" : "warn"),
        "data-tip":
          m("suite.projection_tip", { v0: Number(b.anchor_hours || 0).toFixed(b.anchor_hours < 2 ? 1 : 0) }) + " · " +
          m("suite.projected_at_reset") +
          " " +
          (p >= 999 ? "≥999%" : suitePct(p)),
        text: m("suite.projection_over")
      }),
      at ? el("span", { class: "small muted", text: m("suite.projection_exhaust_at", { v0: when(at) }) }) : null
    );
  }
  const cls = p >= 85 ? "warn" : "ok";
  return el(
    "span",
    { class: "suite-projection" },
    el("b", { class: "mono", text: suitePct(p) }),
    b.projected_high - b.projected_low >= 1
      ? el("span", {
          class: "small muted mono",
          "data-tip": m("suite.projection_band_tip"),
          text: "(" + suitePct(b.projected_low) + "–" + (b.projected_high >= 100 ? "≥100%" : suitePct(b.projected_high)) + ")"
        })
      : null,
    el("span", {
      class: "tag " + cls,
      "data-tip": m("suite.projection_tip", { v0: Number(b.anchor_hours || 0).toFixed(b.anchor_hours < 2 ? 1 : 0) }),
      text: m("suite.projection_ok")
    })
  );
}

// Inline SVG: official used % (solid blue), USD share (dashed), and three
// projections from now: sustainable (green), cumulative average (purple),
// recent (orange).
function suiteForecastChart(w) {
  const start = Date.parse(w.start), end = Date.parse(w.reset_at);
  if (!(end > start)) return null;
  const W = 320, H = 72, xt = (ms) => ((Math.min(Math.max(ms, start), end) - start) / (end - start)) * W;
  const x = (t) => xt(Date.parse(t));
  const yp = (v) => H - (Math.min(Math.max(v, 0), 100) / 100) * (H - 4);
  const usdMax = Math.max(w.capacity_usd || 0, ...(w.usd_points || []).map((p) => p.value), 1e-9);
  const line = (pts, y) => pts.map((p) => x(p.at).toFixed(1) + "," + (H - y(p.value)).toFixed(1)).join(" ");
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 " + W + " " + H);
  svg.setAttribute("class", "suite-chart");
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", m("suite.chart_label"));
  const add = (tag, attrs) => {
    const node = document.createElementNS(ns, tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
    svg.append(node);
  };
  add("rect", { x: 0, y: 0, width: W, height: H, class: "suite-chart-bg" });
  const now = Date.now();
  const nowX = xt(now);
  add("line", { x1: nowX, x2: nowX, y1: 0, y2: H, class: "suite-chart-now" });
  if ((w.usd_points || []).length > 1)
    add("polyline", { points: line(w.usd_points, (v) => (v / usdMax) * (H - 4)), class: "suite-chart-usd" });
  const b = w.burn, u = w.used_percent;
  const ray = (cls, fromMs, fromV, perHour) => {
    if (!(perHour > 0)) return;
    let toMs = end, toV = fromV + (perHour * (end - fromMs)) / 3600000;
    if (toV > 100) {
      toMs = fromMs + ((100 - fromV) / perHour) * 3600000;
      toV = 100;
    }
    add("line", { x1: xt(fromMs), y1: yp(fromV), x2: xt(toMs), y2: yp(toV), class: cls });
  };
  if (b && u != null) {
    ray("suite-chart-sus", now, u, b.sustainable_per_hour);
    ray("suite-chart-cum", start, 0, b.cumulative_per_hour);
    ray("suite-chart-rec", now, u, b.recent_per_hour);
  }
  if ((w.percent_points || []).length) {
    const pts = [{ at: w.start, value: 0 }, ...w.percent_points];
    add("polyline", { points: line(pts, (v) => (v / 100) * (H - 4)), class: "suite-chart-pct" });
  }
  return el(
    "div",
    { class: "suite-chart-wrap" },
    svg,
    el(
      "div",
      { class: "suite-chart-legend small muted" },
      el("span", { class: "suite-legend-pct", text: m("suite.chart_percent") }),
      el("span", { class: "suite-legend-usd", text: m("suite.chart_usd") }),
      b ? el("span", { class: "suite-legend-sus", text: m("suite.line_sustainable") }) : null,
      b ? el("span", { class: "suite-legend-cum", text: m("suite.line_cumulative") }) : null,
      b ? el("span", { class: "suite-legend-rec", text: m("suite.line_recent") }) : null,
      el("span", { text: when(w.start) + " → " + when(w.reset_at) })
    )
  );
}

const SUITE_PACE_TAG = { fast: "warn", slow: "ok", on_track: "plain" };
// "fast" is red only when the window actually alerts; early in a
// window (quiet period) or with plenty left it is a neutral note.
function suitePaceCell(b, used, w) {
  if (!b) return "—";
  const cls = b.pace === "fast" ? (w && w.alert_level ? "bad" : w && w.quiet ? "plain" : "warn") : SUITE_PACE_TAG[b.pace] || "plain";
  return el(
    "span",
    {},
    el("span", { class: "tag " + cls, text: m("suite.pace_" + b.pace) }),
    " ",
    el("span", { class: "small muted", text: m("suite.pace_detail", { v0: suitePct(used), v1: suitePct(b.time_progress) }) })
  );
}
function suiteRate(v) { return v == null ? "—" : Number(v).toFixed(v < 1 ? 2 : 1) + "%/h"; }

function suiteWindowCard(w) {
  const kv = (label, value) => [
    el("span", { text: label }),
    typeof value === "string" ? el("span", { class: "mono", text: value }) : value
  ];
  const b = w.burn;
  const notes = [];
  if (w.rejected_readings) notes.push(m("suite.rejected_n", { v0: int(w.rejected_readings) }));
  if (w.pending)
    notes.push(m("suite.pending_reading", { v0: suitePct(w.pending.Used ?? w.pending.used), v1: when(w.pending.At ?? w.pending.at) }));
  return el(
    "div",
    { class: "suite-window" },
    el(
      "div",
      { class: "suite-window-head" },
      el("b", { text: suiteWindowName(w) }),
      suiteWindowRiskTag(w),
      suiteCoverageTag(w.coverage),
      w.stale ? el("span", { class: "tag plain", "data-tip": m("suite.note_window_reset"), text: m("suite.stale") }) : null
    ),
    el(
      "div",
      { class: "suite-kv" },
      ...kv(m("suite.window_range"), when(w.start) + " → " + when(w.reset_at)),
      ...kv(
        m("suite.official_remaining"),
        w.remaining_percent == null
          ? el("span", { class: "tag plain", text: m("suite.unknown") })
          : el(
              "span",
              {},
              el("b", { class: "mono", text: suitePct(w.remaining_percent) }),
              " ",
              el("span", {
                class: "small muted",
                text: m("suite.percent_source_" + (w.percent_source || "api")) + DISPLAY_SEPARATOR + when(w.observed_at)
              })
            )
      ),
      ...kv(m("suite.projected_at_reset"), suiteProjectionCell(w)),
      ...kv(
        m("suite.used_usd"),
        usd(w.used_usd) + DISPLAY_SEPARATOR + tokens(w.used_tokens || 0) +
          (w.unpriced_requests ? "  (" + m("suite.unpriced_n", { v0: int(w.unpriced_requests) }) + ")" : "")
      ),
      ...kv(m("suite.capacity"), suiteCapacityCell(w)),
      ...kv(
        m("suite.capacity_tokens"),
        w.capacity_tokens == null
          ? "—"
          : suiteTok(w.capacity_tokens) +
              (w.capacity_tokens_p25 != null && w.capacity_tokens_p75 > w.capacity_tokens_p25
                ? " (" + suiteTok(w.capacity_tokens_p25) + "–" + suiteTok(w.capacity_tokens_p75) + ")"
                : "")
      ),
      ...kv(
        m("suite.remaining_usd"),
        (w.remaining_usd == null ? "—" : usd(w.remaining_usd)) +
          (w.remaining_tokens != null ? DISPLAY_SEPARATOR + suiteTok(w.remaining_tokens) : "")
      ),
      ...kv(m("suite.pace"), suitePaceCell(b, w.used_percent, w)),
      ...kv(
        m("suite.burn_lines"),
        b
          ? el(
              "span",
              { class: "suite-burn-lines mono" },
              el("span", { class: "suite-legend-sus", text: suiteRate(b.sustainable_per_hour) }),
              el("span", { class: "suite-legend-cum", text: suiteRate(b.cumulative_per_hour) }),
              el("span", {
                class: "suite-legend-rec",
                "data-tip": m("suite.recent_basis_" + b.recent_basis),
                text: suiteRate(b.recent_per_hour)
              })
            )
          : w.burn_usd_per_hour ? usd(w.burn_usd_per_hour) + "/h" : "—"
      ),
      ...kv(
        m("suite.exhaust_at"),
        w.risk === "exhausted"
          ? m("suite.risk_exhausted")
          : b && (b.exhaust_recent || b.exhaust_cumulative)
            ? el(
                "span",
                { class: "suite-burn-lines" },
                el("span", {
                  class: "suite-legend-rec mono",
                  text:
                    b.exhaust_recent && Date.parse(b.exhaust_recent) < Date.parse(w.reset_at)
                      ? when(b.exhaust_recent)
                      : m("suite.not_before_reset")
                }),
                el("span", {
                  class: "suite-legend-cum mono small",
                  text:
                    b.exhaust_cumulative && Date.parse(b.exhaust_cumulative) < Date.parse(w.reset_at)
                      ? when(b.exhaust_cumulative)
                      : m("suite.not_before_reset")
                })
              )
            : w.exhaust_at ? el("span", { class: "mono", text: when(w.exhaust_at) }) : m("suite.not_before_reset")
      )
    ),
    notes.length ? el("div", { class: "small muted", text: notes.join(DISPLAY_SEPARATOR) }) : null,
    suiteForecastChart(w)
  );
}

// the ledger is built only when opened, 20 rows at a time.
function suiteLedgerTable(a) {
  const allRows = a.ledger || [];
  const months = a.monthly || [];
  if (!allRows.length && !months.length) return null;
  suiteForecast.ledgerOpen ||= new Map();
  const key = a.provider + "|" + a.auth_index;
  const shown = suiteForecast.ledgerOpen.get(key) || 0;
  const box = el("details", { class: "suite-ledger", "data-ledger": a.auth_index, open: shown ? true : null });
  const summary = el("summary", { text: m("suite.ledger_title", { v0: int(allRows.length), v1: int(months.length) }) });
  const fill = (n) => {
    suiteForecast.ledgerOpen.set(key, n);
    box.replaceChildren(summary, suiteLedgerBody(a, allRows.slice(0, n), months, allRows.length, () => fill(n + 20)));
  };
  box.append(summary);
  box.addEventListener("toggle", () => {
    if (box.open && box.childElementCount === 1) fill(Math.max(shown, 20));
    else if (!box.open) { suiteForecast.ledgerOpen.delete(key); box.replaceChildren(summary); }
  });
  if (shown) fill(shown);
  return box;
}
function suiteLedgerBody(a, rows, months, total, more) {
  const table = (head, body) =>
    el("table", { class: "suite-table" }, el("thead", {}, el("tr", {}, head.map((h) => el("th", { text: h })))), el("tbody", {}, body));
  return el(
    "div",
    { class: "suite-scroll" },
    months.length
      ? el(
          "div",
          {},
          el("div", { class: "small muted", text: m("suite.monthly_hint") }),
          table(
            [m("suite.month"), "USD", "Tokens", m("suite.requests"), m("suite.cycles"), m("suite.avg_peak"), m("suite.exhausted_cycles")],
            months.map((r) =>
              el(
                "tr",
                {},
                [
                  r.month,
                  usd(r.usd),
                  tokens(r.tokens),
                  int(r.requests),
                  int(r.cycles),
                  r.cycles ? suitePct(r.avg_peak_percent) : "—",
                  int(r.exhausted_cycles)
                ].map((v) => el("td", { class: "mono", text: String(v) }))
              )
            )
          )
        )
      : null,
    rows.length
      ? table(
          [m("suite.period"), m("suite.peak"), "USD", "Tokens", m("suite.capacity"), m("suite.confidence")],
          rows.map((r) =>
            el(
              "tr",
              {},
              el("td", {
                class: "mono",
                text: suiteWindowName({ key: r.window }) + " " + when(r.start) + " → " + when(r.end) + (r.early ? " *" : "")
              }),
              el("td", { class: "mono", text: suitePct(r.peak_percent) }),
              el("td", { class: "mono", text: usd(r.usd) }),
              el("td", { class: "mono", text: tokens(r.tokens) }),
              el("td", { class: "mono", text: r.capacity_usd == null ? "—" : usd(r.capacity_usd) }),
              el("td", {}, suiteConfTag({ confidence: r.confidence, samples: r.samples }))
            )
          )
        )
      : null,
    rows.length < total
      ? el("button", { type: "button", class: "btn small suite-more", "data-ledger-more": a.auth_index, text: m("suite.show_more", { v0: int(total - rows.length) }), onclick: more })
      : null
  );
}

// a card that folds; its body is built only while open.
function suiteFoldCard(title, opts, actions, body) {
  const box = el("details", { class: "card suite-card suite-fold", open: opts.open || null, ...(opts.attrs || {}) });
  const summary = el("summary", { class: "suite-card-head" }, el("h2", { class: "mask-blur", text: title }), actions || null);
  // A button in the summary must not fold the card.
  summary.addEventListener("click", (e) => { if (e.target.closest("button") && e.target.closest("button") !== summary) e.preventDefault(); });
  box.append(summary);
  if (opts.open) box.append(...[].concat(body()).filter(Boolean));
  let was = !!opts.open;
  box.addEventListener("toggle", () => {
    if (box.open === was) return;
    was = box.open;
    if (box.open && box.childElementCount === 1) box.append(...[].concat(body()).filter(Boolean));
    opts.ontoggle?.(box.open);
  });
  return box;
}

function suiteModuleOff(target, mode) {
  target.replaceChildren(
    el(
      "div",
      { class: "card" },
      el("div", { class: "empty", text: m("suite.module_off", { v0: mode }) }),
      suiteLink(m("suite.nav_modules"), suiteSub("modules"))
    )
  );
}

function renderSuiteForecast() {
  if (currentRole !== "admin" || activeTab(false) !== "forecast") return false;
  suiteLoadForecast(false);
  const target = $("suite-forecast");
  const v = suiteForecast.value;
  const stamp = [suiteForecast.readAt, suiteForecast.error, suiteForecast.refreshing, document.documentElement.lang].join("|");
  if (suiteForecast.painted === stamp && target.childElementCount) return false;
  suiteForecast.painted = stamp;
  if (!v) {
    target.replaceChildren(
      el(
        "div",
        { class: "card" },
        el("div", {
          class: suiteForecast.error ? "notice err show" : "empty section-loading",
          text: suiteForecast.error || m("ui.loading")
        })
      )
    );
    return false;
  }
  if (v.mode === "off") return (suiteModuleOff(target, "forecast"), false);
  const hint = el("div", {
    class: "small muted",
    text: m(v.mode === "passive" ? "suite.forecast_hint_passive" : "suite.forecast_hint", { v0: v.min_ratio_percent })
  });
  const order = { exhausted: 0, high: 1, medium: 2, unknown: 3, low: 4 };
  const level = (w) => (w.risk === "exhausted" ? "exhausted" : w.alert_level || (w.risk === "unknown" ? "unknown" : "low"));
  const worst = (a) => (a.windows || []).reduce((acc, w) => ((order[level(w)] ?? 5) < (order[acc] ?? 5) ? level(w) : acc), "low");
  suiteForecast.open ||= new Map();
  const accounts = (v.accounts || []).slice().sort((x, y) => (order[worst(x)] ?? 5) - (order[worst(y)] ?? 5));
  const summaryRows = [];
  for (const a of accounts) for (const w of a.windows || []) summaryRows.push({ a, w });
  summaryRows.sort((x, y) => (order[level(x.w)] ?? 5) - (order[level(y.w)] ?? 5));
  const atRisk = summaryRows.filter(({ w }) => ["exhausted", "high", "medium"].includes(level(w)));
  const overview = suiteCard(
    m("suite.forecast_table", { v0: int(accounts.length), v1: int(atRisk.length) }),
    null,
    summaryRows.length
      ? el("div", { class: "suite-scroll" }, el("table", { class: "suite-table suite-forecast-table" },
          el("thead", {}, el("tr", {}, [m("suite.account"), m("suite.window"), m("suite.left"), "USD", m("suite.risk")].map((h) => el("th", { text: h })))),
          el("tbody", {}, summaryRows.slice(0, suiteForecast.tableAll ? summaryRows.length : 12).map(({ a, w }) =>
            el("tr", { class: "clickable", onclick: () => {
                suiteForecast.open.set(a.provider + "|" + a.auth_index, true);
                suiteForecast.painted = "";
                renderSuiteForecast();
                document.querySelector('[data-forecast-acc="' + CSS.escape(a.auth_index) + '"]')?.scrollIntoView({ block: "start" });
              } },
              el("td", { class: "mask-blur", text: suiteProviderName(a.provider) + " " + (a.name || a.auth_index) }),
              el("td", { text: suiteWindowName(w) }),
              el("td", { class: "mono", text: suitePct(w.remaining_percent) }),
              el("td", { class: "mono", text: w.remaining_usd == null ? "—" : usd(w.remaining_usd) }),
              el("td", {}, suiteWindowRiskTag(w))
            ))))) 
      : el("div", { class: "empty", text: m("suite.no_forecast_accounts") }),
    summaryRows.length > 12
      ? el("button", { type: "button", class: "btn small suite-more", text: suiteForecast.tableAll ? m("suite.show_less") : m("suite.show_more", { v0: int(summaryRows.length - 12) }),
          onclick: () => { suiteForecast.tableAll = !suiteForecast.tableAll; suiteForecast.painted = ""; renderSuiteForecast(); } })
      : null
  );
  const cards = accounts.map((a) => {
    const f = a.fetch;
    const canRefresh = v.mode === "on" && (!f || Date.parse(f.manual_refresh_after) <= Date.now());
    const key = a.provider + "|" + a.auth_index;
    const lv = worst(a);
    const open = suiteForecast.open.has(key) ? suiteForecast.open.get(key) : ["exhausted", "high", "medium"].includes(lv);
    return suiteFoldCard(
      suiteProviderName(a.provider) + DISPLAY_SEPARATOR + (a.name || a.auth_index),
      { open, attrs: { "data-forecast-acc": a.auth_index }, ontoggle: (o) => suiteForecast.open.set(key, o) },
      el(
        "span",
        { class: "suite-card-actions" },
        a.plan ? el("span", { class: "tag plain", text: a.plan }) : null,
        f
          ? el("span", {
              class: "small muted",
              "data-tip": f.error || "",
              text: m(f.ok ? "suite.fetched_at" : "suite.fetch_failed_at", { v0: when(f.at) })
            })
          : null,
        v.mode === "on"
          ? el("button", {
              type: "button",
              class: "btn small",
              "data-suite-refresh": a.auth_index,
              disabled: !canRefresh || !!suiteForecast.task,
              "data-tip": canRefresh ? m("suite.refresh_tip") : m("suite.refresh_wait", { v0: when(f.manual_refresh_after) }),
              text: suiteForecast.refreshing === a.auth_index ? m("ui.loading") : m("suite.refresh_official"),
              onclick: () => suiteLoadForecast(true, a.auth_index)
            })
          : null
      ),
      () => [
        a.email ? el("div", { class: "small muted mask-blur", text: a.email }) : null,
        a.provider === "xai" && a.windows?.length ? el("div", { class: "small muted", text: m("suite.grok_sampled_note") }) : null,
        a.windows?.length
          ? el("div", { class: "suite-grid g2" }, a.windows.map(suiteWindowCard))
          : el("div", { class: "empty", text: m("suite.note_" + (a.note || "no_reading")) }),
        suiteLedgerTable(a)
      ]
    );
  });
  target.replaceChildren(
    suiteCard(
      m("suite.nav_forecast"),
      el("button", { type: "button", class: "btn small", text: m("ui.refresh"), onclick: () => suiteLoadForecast(true) }),
      hint
    ),
    ...(summaryRows.length ? [overview] : []),
    ...(cards.length ? cards : [el("div", { class: "card" }, el("div", { class: "empty", text: m("suite.no_forecast_accounts") }))])
  );
  return false;
}

// Compact overview card: every window, worst risk first.
function suiteOverviewInsights() {
  suiteLoadForecast(false);
  if (!suiteRanking.value) suiteLoadRanking(false);
  const v = suiteForecast.value;
  const order = { exhausted: 0, high: 1, medium: 2, unknown: 3, low: 4 };
  const rows = [];
  if (v && v.mode !== "off") for (const a of v.accounts || []) for (const w of a.windows || []) rows.push({ a, w });
  const level = (w) => (w.risk === "exhausted" ? "exhausted" : w.alert_level || (w.risk === "unknown" ? "unknown" : "low"));
  rows.sort((x, y) => (order[level(x.w)] ?? 5) - (order[level(y.w)] ?? 5));
  const anomalies = suiteAnomalyCount();
  return suiteCard(
    m("suite.forecast_summary"),
    el(
      "span",
      { class: "suite-card-actions" },
      anomalies
        ? el("button", {
            type: "button",
            class: "tag bad",
            "data-suite-anomaly-badge": "",
            text: m("suite.anomalies_n", { v0: int(anomalies) }),
            onclick: () => suiteGo(suiteSub("ranking"))
          })
        : null,
      suiteLink(m("suite.view_forecast"), suiteSub("forecast"))
    ),
    !v
      ? el("div", { class: "empty section-loading", text: m("ui.loading") })
      : v.mode === "off"
        ? el("div", { class: "empty", text: m("suite.module_off", { v0: "forecast" }) })
        : rows.length
          ? el(
              "div",
              { class: "suite-quota-list" },
              rows.slice(0, 6).map(({ a, w }) =>
                el(
                  "div",
                  { class: "suite-forecast-row" },
                  el("span", { class: "mask-blur", text: suiteProviderName(a.provider) + " " + (a.name || a.auth_index) }),
                  el("span", { class: "small muted", text: suiteWindowName(w) }),
                  el("span", { class: "mono", text: m("suite.left_pct", { v0: suitePct(w.remaining_percent) }) }),
                  el("span", { class: "mono", text: w.remaining_usd == null ? m("suite.not_enough_data") : usd(w.remaining_usd) }),
                  suiteWindowRiskTag(w)
                )
              )
            )
          : el("div", { class: "empty", text: m("suite.no_forecast_accounts") })
  );
}

const SUITE_RANK_RANGES = ["today", "7d", "30d"];
const SUITE_RANK_METRICS = ["usd", "tokens", "requests"];
function suiteRankValue(row, metric) { return metric === "usd" ? row.usd : metric === "tokens" ? row.tokens : row.requests; }
function suiteRankFormat(row) { return usd(row.usd) + DISPLAY_SEPARATOR + tokens(row.tokens) + DISPLAY_SEPARATOR + int(row.requests); }
function suiteScopeName(scope) {
  const view = (resources.admin.keys.value || []).find((key) => key.scope === scope);
  return view ? apiKeyIdentity(view).primary : suiteAliasFor(scope) || scope.slice(0, 12);
}

function suiteAnomalyText(a) {
  if (a.kind === "client_blocks") return m("suite.anomaly_blocks", { v0: int(a.recent), v1: Number(a.baseline).toFixed(1) });
  const fmt = (v) => (a.metric === "usd" ? usd(v) : int(v));
  if (a.kind === "key_new") return m("suite.anomaly_key_new", { v0: suiteScopeName(a.scope), v1: fmt(a.recent) });
  return m(a.active_days ? "suite.anomaly_key7" : "suite.anomaly_key", {
    v0: suiteScopeName(a.scope),
    v1: fmt(a.recent),
    v2: fmt(a.baseline),
    v3: Number(a.ratio).toFixed(1),
    v4: int(a.active_days || 0)
  });
}

function renderSuiteRanking() {
  if (currentRole !== "admin" || activeTab(false) !== "ranking") return false;
  suiteLoadRanking(false);
  const target = $("suite-ranking");
  const v = suiteRanking.value;
  const stamp = [
    suiteRanking.readAt,
    suiteRanking.error,
    suiteRanking.range,
    suiteRanking.metric,
    resources.admin.keys.readAt,
    suiteAliases.state,
    document.documentElement.lang
  ].join("|");
  if (suiteRanking.painted === stamp && target.childElementCount) return false;
  suiteRanking.painted = stamp;
  if (!v) {
    target.replaceChildren(
      el(
        "div",
        { class: "card" },
        el("div", { class: suiteRanking.error ? "notice err show" : "empty section-loading", text: suiteRanking.error || m("ui.loading") })
      )
    );
    return false;
  }
  if (v.mode === "off") return (suiteModuleOff(target, "ranking"), false);
  const pills = (list, current, key, label) =>
    el(
      "div",
      { class: "suite-segment", role: "group" },
      list.map((id) =>
        el("button", {
          type: "button",
          class: "suite-pill" + (id === current ? " active" : ""),
          "data-suite-rank": key + ":" + id,
          text: label(id),
          onclick: () => {
            suiteRanking[key] = id;
            if (key === "range") suiteLoadRanking(true);
            renderSuiteRanking();
          }
        })
      )
    );
  const metric = suiteRanking.metric;
  const sorted = (rows) => [...(rows || [])].sort((a, b) => suiteRankValue(b, metric) - suiteRankValue(a, metric));
  const bars = (rows, label) =>
    rows.length
      ? suiteBars(sorted(rows), (r) => suiteRankValue(r, metric) || 1e-9, label, suiteRankFormat, 10)
      : el("div", { class: "empty", text: m("suite.no_usage_range") });
  const anomalies = v.anomalies || [];
  target.replaceChildren(
    suiteCard(
      m("suite.nav_ranking"),
      el(
        "span",
        { class: "suite-card-actions" },
        pills(SUITE_RANK_RANGES, suiteRanking.range, "range", (id) => m("suite.range_" + id)),
        pills(SUITE_RANK_METRICS, metric, "metric", (id) => m("suite.metric_" + id))
      ),
      el("div", { class: "small muted", text: m("suite.ranking_hint") })
    ),
    suiteCard(
      m("suite.anomalies"),
      el("span", { class: "small muted", text: m("suite.anomalies_hint") }),
      anomalies.length
        ? el(
            "div",
            { class: "suite-quota-list", "data-suite-anomalies": "" },
            anomalies.map((a) =>
              el(
                "div",
                { class: "suite-anomaly mask-blur" },
                el("span", { class: "tag " + (a.severity === "high" ? "bad" : "warn"), text: m("suite.severity_" + a.severity) }),
                " ",
                suiteAnomalyText(a)
              )
            )
          )
        : el("div", { class: "empty", text: m("suite.no_anomalies") })
    ),
    el(
      "div",
      { class: "suite-grid g2" },
      suiteCard(m("suite.rank_keys"), suiteLink(m("suite.view_analysis"), suiteSub("analysis")), bars(v.keys, (r) => suiteScopeName(r.id))),
      suiteCard(
        m("suite.rank_accounts"),
        suiteLink(m("suite.nav_accounts"), suiteSub("auth-files")),
        bars(v.accounts, (r) => suiteProviderName(r.provider || "") + " " + (r.name || r.id))
      )
    )
  );
  return false;
}
