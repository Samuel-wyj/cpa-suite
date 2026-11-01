/* CPA Suite 0.6/7 pages: passive degradation monitor (Grok / Codex) and the
 * page-only alert center. Data comes from /suite/monitor and /suite/alerts;
 * the monitor never sends probe requests. Results are heuristic hints.
 * 0.7: hard / soft signal groups and the "observe" verdict, alert level
 * tabs, cooldown tags, merged degradation alerts and the threshold settings. */

const suiteMonitor = { value: null, error: "", readAt: 0, task: null, painted: "" };
const suiteCandy = { value: null, error: "", readAt: 0, task: null, busy: "", poll: 0 };
const suiteAlerts = { value: null, error: "", readAt: 0, task: null, painted: "", busy: "" };
const suiteAlertCfg = { value: null, error: "", task: null, busy: false, saved: 0 };

function suiteLoadMonitor(force) {
  if (suiteMonitor.task || (!force && Date.now() - suiteMonitor.readAt < 60000)) return suiteMonitor.task;
  const generation = sessionGeneration;
  suiteMonitor.task = plugin("GET", "/suite/monitor").then((value) => {
    if (generation === sessionGeneration) Object.assign(suiteMonitor, { value, error: "", readAt: Date.now() });
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteMonitor.error = error.message || String(error);
    })
    .finally(() => {
      suiteMonitor.task = null;
      renderSuiteMonitor();
    });
  return suiteMonitor.task;
}

function suiteLoadCandy(force) {
  if (suiteCandy.task || (!force && Date.now() - suiteCandy.readAt < 60000)) return suiteCandy.task;
  const generation = sessionGeneration;
  suiteCandy.task = plugin("GET", "/suite/candy").then((value) => {
    if (generation === sessionGeneration) Object.assign(suiteCandy, { value, error: "", readAt: Date.now() });
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteCandy.error = error.message || String(error);
    })
    .finally(() => {
      suiteCandy.task = null;
      suiteMonitor.painted = "";
      renderSuiteMonitor();
      // While a run is in flight, poll every 3 s (only while the page is open).
      clearTimeout(suiteCandy.poll);
      if ((suiteCandy.value?.accounts || []).some((a) => a.running) && activeTab(false) === "monitor")
        suiteCandy.poll = setTimeout(() => suiteLoadCandy(true), 3000);
    });
  return suiteCandy.task;
}

function suiteCandyRun(a, model, effort) {
  const cost = a.cost_input_tokens || 0;
  const content = m("suite.candy_confirm_body", {
    v0: suiteProviderName(a.provider) + " · " + (a.name || a.auth_index),
    v1: model || a.default_model,
    v2: int(cost)
  });
  return openActionDialog(
    m("suite.candy_confirm_title"),
    content,
    undefined,
    async () => {
      try { await plugin("POST", "/suite/candy/run", { auth_index: a.auth_index, model, effort, confirm: true }); } finally {
        suiteLoadCandy(true);
      }
    },
    m("suite.candy_run_confirm")
  );
}

function suiteCandyCard() {
  const v = suiteCandy.value;
  if (!v) return null;
  const accounts = v.accounts || [];
  const rows = accounts.map((a) => {
    const modelInput = el("input", {
      type: "text",
      value: a.default_model,
      "aria-label": m("suite.candy_model"),
      "data-candy-model": a.auth_index
    });
    const effort = el(
      "select",
      { "aria-label": m("suite.candy_effort") },
      ["", "low", "medium", "high", "xhigh"].map((e) => el("option", { value: e, text: e || m("suite.candy_effort_default") }))
    );
    const wait = a.next_allowed_at && Date.parse(a.next_allowed_at) > Date.now();
    const allRuns = a.runs || [];
    const showAll = suiteCandy.more?.has(a.auth_index);
    const runs = showAll ? allRuns : allRuns.slice(0, 5);
    return el(
      "div",
      { class: "suite-candy-account" },
      el(
        "div",
        { class: "suite-candy-row" },
        el(
          "span",
          {},
          el("b", { class: "mask-blur", text: suiteProviderName(a.provider) + " · " + (a.name || a.auth_index) }),
          " ",
          el("span", { class: "small muted", text: m("suite.candy_cost", { v0: int(a.cost_input_tokens) }) })
        ),
        modelInput,
        effort,
        el("button", {
          type: "button",
          class: "btn small",
          "data-candy-run": a.auth_index,
          disabled: a.running || wait,
          "data-tip": wait ? m("suite.candy_wait", { v0: when(a.next_allowed_at) }) : null,
          text: a.running
            ? m("suite.candy_running")
            : wait ? m("suite.candy_wait_short", { v0: when(a.next_allowed_at) }) : m("suite.candy_run"),
          onclick: () => suiteCandyRun(a, modelInput.value.trim(), effort.value)
        })
      ),
      runs.length
        ? el("div", { class: "suite-scroll" }, el(
            "table",
            { class: "suite-table" },
            el(
              "thead",
              {},
              el(
                "tr",
                {},
                [
                  m("suite.candy_time"),
                  m("suite.candy_result"),
                  m("suite.candy_model"),
                  m("suite.candy_returned"),
                  m("suite.sig_reasoning"),
                  m("suite.candy_latency")
                ].map((h) => el("th", { text: h }))
              )
            ),
            el(
              "tbody",
              {},
              runs.map((r) =>
                el(
                  "tr",
                  {},
                  el("td", { class: "mono", text: when(r.started_at) }),
                  el(
                    "td",
                    {},
                    el("span", {
                      class: "tag " + (r.status === "running" ? "plain" : r.status !== "done" ? "warn" : r.passed ? "ok" : "bad"),
                      "data-tip": r.error || r.answer || null,
                      text:
                        r.status === "done" ? m(r.passed ? "suite.candy_pass" : "suite.candy_fail") : m("suite.candy_status_" + r.status)
                    })
                  ),
                  el("td", { class: "mono", text: r.model + (r.effort ? " · " + r.effort : "") }),
                  el("td", { class: "mono", text: r.response_model || "—" }),
                  el("td", { class: "mono", text: r.status === "done" ? int(r.reasoning_tokens) : "—" }),
                  el("td", { class: "mono", text: r.latency_ms ? (r.latency_ms / 1000).toFixed(1) + "s" : "—" })
                )
              )
            )
          ), allRuns.length > 5
            ? el("button", {
                type: "button",
                class: "btn small suite-more",
                "data-candy-more": a.auth_index,
                text: showAll ? m("suite.show_less") : m("suite.show_more", { v0: int(allRuns.length - 5) }),
                onclick: () => {
                  suiteCandy.more ||= new Set();
                  showAll ? suiteCandy.more.delete(a.auth_index) : suiteCandy.more.add(a.auth_index);
                  suiteMonitor.painted = "";
                  renderSuiteMonitor();
                }
              })
            : null)
        : el("div", { class: "small muted", text: m("suite.candy_none") })
    );
  });
  return suiteCard(
    m("suite.candy_title"),
    el("button", { type: "button", class: "btn small", text: m("ui.refresh"), onclick: () => suiteLoadCandy(true) }),
    el("div", {
      class: "notice warn show",
      text: m("suite.candy_hint", { v0: v.interval_minutes ?? 10, v1: int(v.cost?.codex ?? 14000), v2: int(v.cost?.xai ?? 300) })
    }),
    suiteCandy.error ? el("div", { class: "notice err show", text: suiteCandy.error }) : null,
    rows.length ? el("div", {}, rows) : el("div", { class: "empty", text: m("suite.candy_no_accounts") })
  );
}

function suiteForever(t) { return !t || Date.parse(t) < 86400000; }

const SUITE_SEV_TAG = { high: "bad", medium: "warn", info: "plain", low: "plain" };
function suiteSevTag(sev) {
  return el("span", { class: "tag " + (SUITE_SEV_TAG[sev] || "plain"), text: m("suite.sev_" + (sev || "low")) });
}
const SUITE_VERDICT_TAG = { suspect: "bad", observe: "plain", ok: "ok", insufficient: "plain" };
function suiteVerdictTag(verdict, sev) {
  const text = m("suite.verdict_" + (verdict || "insufficient")) + (verdict === "suspect" && sev ? "（" + m("suite.sev_" + sev) + "）" : "");
  return el("span", { class: "tag " + (SUITE_VERDICT_TAG[verdict] || "plain"), text });
}

function suiteSignalValue(s) {
  if (s.recent == null) return "—";
  const rate = ["model_mismatch", "errors", "truncated", "refusal"].includes(s.id);
  const f = (v) => (v == null ? "—" : rate ? (v * 100).toFixed(1) + "%" : int(Math.round(v)));
  return f(s.recent) + " / " + f(s.baseline);
}

function suiteSignalRow(s) {
  const state = { deviates: "bad", notable: "warn", ok: "ok" }[s.state] || "plain";
  return el(
    "div",
    { class: "suite-sig-row" },
    el("span", { text: m("suite.sig_" + s.id) }),
    el("span", { class: "mono", text: suiteSignalValue(s) }),
    el("span", { class: "mono small muted", text: s.p_value == null ? "" : "p=" + Number(s.p_value).toPrecision(2) }),
    el("span", { class: "tag " + state, text: m("suite.sigstate_" + s.state), "data-tip": suiteSignalTip(s) })
  );
}

function suiteSignalTip(s) {
  const tip = [];
  if (s.detail) tip.push(m("suite.sigdetail_" + s.detail));
  if (s.change != null && s.kind === "soft") tip.push(m("suite.sig_change", { v0: (s.change * 100).toFixed(0) + "%" }));
  if (s.effect != null) tip.push(m("suite.sig_effect", { v0: Number(s.effect).toFixed(2) }));
  if (s.kind === "soft" && s.strata) tip.push(m("suite.sig_strata", { v0: int(s.strata) }));
  if (s.segments_total) tip.push(m("suite.sig_segments", { v0: int(s.segments_deviating || 0), v1: int(s.segments_total) }));
  return tip.length ? tip.join("\n") : null;
}

// Signals grouped: hard (decide), soft (need a hard one), info (never counted).
function suiteSignalGroups(signals) {
  const groups = [["hard", m("suite.sig_group_hard")], ["soft", m("suite.sig_group_soft")], ["info", m("suite.sig_group_info")]];
  return groups.flatMap(([kind, label]) => {
    const list = (signals || []).filter((s) => (s.kind || "info") === kind);
    return list.length ? [el("div", { class: "suite-sig-group small muted", text: label }), ...list.map(suiteSignalRow)] : [];
  });
}

function suiteSparkline(series, key) {
  const pts = (series || []).map((p) => p[key] || 0);
  if (pts.length < 2) return null;
  const max = Math.max(...pts, 1e-9);
  const d = pts.map((v, i) => (i ? "L" : "M") + ((i / (pts.length - 1)) * 120).toFixed(1) + "," + (28 - (v / max) * 26).toFixed(1))
    .join("");
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 120 30");
  svg.setAttribute("class", "suite-spark");
  svg.setAttribute("preserveAspectRatio", "none");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", d);
  svg.append(path);
  return el("span", { class: "suite-spark-wrap", "data-tip": m("suite.spark_" + key) }, svg);
}

function suiteBucketCard(b) {
  const head = [b.model || "—", b.effort ? m("suite.effort", { v0: b.effort }) : null, b.stream ? m("suite.stream") : m("suite.non_stream")]
    .filter(Boolean)
    .join(DISPLAY_SEPARATOR);
  const softOnly = b.verdict === "observe" && b.deviating > b.hard;
  const candyAcc = (suiteCandy.value?.accounts || []).find((a) => a.auth_index === b.auth_index);
  const candyOn = suiteCandy.value?.mode === "on" && candyAcc;
  const wait = candyAcc?.next_allowed_at && Date.parse(candyAcc.next_allowed_at) > Date.now();
  return el(
    "div",
    { class: "card suite-window" + (b.verdict === "observe" ? " suite-observe" : "") },
    el(
      "div",
      { class: "suite-card-actions" },
      el("strong", { text: head }),
      suiteVerdictTag(b.verdict, b.severity),
      b.candy_failed ? el("span", { class: "tag bad", text: m("suite.candy_confirmed") }) : null
    ),
    el("div", { class: "small muted", text: m("suite.bucket_counts", { v0: int(b.recent_count), v1: int(b.baseline_count) }) }),
    b.verdict === "observe" ? el("div", { class: "small muted", text: m(softOnly ? "suite.observe_soft" : "suite.observe_hard") }) : null,
    softOnly
      ? el(
          "div",
          { class: "suite-card-actions" },
          candyOn
            ? el("button", {
                type: "button",
                class: "btn small",
                "data-candy-confirm": b.auth_index + "|" + b.model,
                disabled: candyAcc.running || wait,
                text: candyAcc.running ? m("suite.candy_running") : m("suite.candy_confirm_btn"),
                onclick: () => suiteCandyRun(candyAcc, b.model, b.effort)
              })
            : el("span", { class: "small muted", text: m("suite.candy_confirm_off") })
        )
      : null,
    b.reasoning_unreported ? el("div", { class: "small muted", text: m("suite.reasoning_unreported") }) : null,
    el("div", { class: "suite-quota-list" }, suiteSignalGroups(b.signals))
  );
}

function renderSuiteMonitor() {
  if (currentRole !== "admin" || activeTab(false) !== "monitor") return false;
  suiteLoadMonitor(false);
  suiteLoadCandy(false);
  const target = $("suite-monitor");
  const v = suiteMonitor.value;
  const stamp = [suiteMonitor.readAt, suiteMonitor.error, suiteCandy.readAt, document.documentElement.lang].join("|");
  if (suiteMonitor.painted === stamp && target.childElementCount) return false;
  suiteMonitor.painted = stamp;
  if (!v)
    return (
      target.replaceChildren(
        el(
          "div",
          { class: "card" },
          el("div", {
            class: suiteMonitor.error ? "notice err show" : "empty section-loading",
            text: suiteMonitor.error || m("ui.loading")
          })
        )
      ),
      false
    );
  if (v.mode === "off") return (suiteModuleOff(target, "grok_monitor"), false);
  const t = v.thresholds || {};
  suiteMonitor.open ||= new Map();
  const cards = (v.accounts || []).map((a) => {
    const flagged = a.verdict === "suspect" || a.verdict === "observe";
    const key = a.provider + "|" + a.auth_index;
    const open = suiteMonitor.open.has(key) ? suiteMonitor.open.get(key) : flagged;
    const buckets = a.buckets || [];
    const flaggedBuckets = buckets.filter((b) => b.verdict === "suspect" || b.verdict === "observe").length;
    return el(
      "details",
      { class: "card suite-card suite-fold suite-monitor-acc", "data-monitor-acc": a.auth_index, open: open || null, ontoggle: (e) => { const was = open; suiteMonitor.open.set(key, e.target.open); if (e.target.open && !was) { suiteMonitor.painted = ""; renderSuiteMonitor(); } } },
      el(
        "summary",
        { class: "suite-card-head" },
        el("h2", { class: "mask-blur", text: suiteProviderName(a.provider) + DISPLAY_SEPARATOR + (a.name || a.auth_index) }),
        el(
          "span",
          { class: "suite-card-actions" },
          el("span", { class: "small muted", text: m("suite.monitor_buckets", { v0: int(buckets.length), v1: int(flaggedBuckets) }) }),
          suiteSparkline(a.series, "requests"),
          suiteSparkline(a.series, "reasoning_median"),
          suiteSparkline(a.series, "error_rate"),
          suiteVerdictTag(a.verdict, a.severity)
        )
      ),
      open ? el("div", { class: "suite-grid g2" }, buckets.map(suiteBucketCard)) : null
    );
  });
  target.replaceChildren(
    suiteCard(
      m("suite.nav_grok"),
      el("button", { type: "button", class: "btn small", text: m("ui.refresh"), onclick: () => suiteLoadMonitor(true) }),
      el("div", {
        class: "small muted",
        text: m("suite.monitor_hint", { v0: t.recent_hours ?? 24, v1: t.baseline_days ?? 7, v2: t.min_samples ?? 50 })
      }),
      el("div", { class: "notice warn show", text: m("suite.monitor_honesty") }),
      suiteCandy.value && suiteCandy.value.mode !== "on"
        ? el("div", { class: "small muted", "data-suite-candy-off": "", text: m("suite.candy_off_hint") })
        : null
    ),
    ...[suiteCandy.value?.mode === "on" ? suiteCandyCard() : null].filter(Boolean),
    ...(cards.length ? cards : [el("div", { class: "card" }, el("div", { class: "empty", text: m("suite.monitor_empty") }))])
  );
  return false;
}

function suiteAlertText(a) {
  const p = a.payload || {};
  const who = p.name || a.object;
  if (a.source === "quota" && p.projected_at_reset != null)
    return m("suite.alert_quota7", {
      v0: who,
      v1: suiteWindowName({ key: p.window }),
      v2: suitePct(p.remaining_percent),
      v3: suitePct(p.projected_at_reset)
    });
  if (a.source === "usage" && p.kind === "key_new") return m("suite.alert_usage_new", { v0: who, v1: usd(p.recent || 0) });
  if (a.source === "usage" && p.active_days)
    return m("suite.alert_usage7", {
      v0: who,
      v1: p.metric === "usd" ? usd(p.recent || 0) : int(p.recent || 0),
      v2: Number(p.ratio || 0).toFixed(1) + "×",
      v3: int(p.active_days)
    });
  return m("suite.alert_" + a.source, {
    v0: who,
    v1: p.window ? suiteWindowName({ key: p.window }) : p.model || p.metric || "",
    v2:
      p.count ??
      (p.remaining_percent != null
        ? suitePct(p.remaining_percent)
        : p.ratio != null ? Number(p.ratio).toFixed(1) + "×" : (p.signals || []).map((s) => m("suite.sig_" + s)).join("、"))
  });
}

/* ---------- 0.7 alert center: server-side paging, filters, grouping, bulk ---------- */
const SUITE_ALERT_PAGE = 50;
const SUITE_HIST_PAGE = 20;
function suiteAlertQueryDefault() { return { status: "open", severity: "", source: "", q: "", since: "", group: false, offset: 0 }; }
Object.assign(suiteAlerts, { q: suiteAlertQueryDefault(), sel: new Set(), rev: 0, hist: { value: null, offset: 0, kind: "", task: null, error: "" }, open: new Map() });
const suiteAlertCount = { n: 0, highest: "", readAt: 0, task: null };

function suiteAlertsTouch() { suiteAlerts.rev++; suiteAlerts.painted = ""; }

function suiteAlertParams(extra) {
  const q = suiteAlerts.q, p = new URLSearchParams();
  for (const k of ["status", "severity", "source", "q", "since"]) if (q[k]) p.set(k, q[k]);
  if (q.group) p.set("group", "1");
  for (const [k, v] of Object.entries(extra || {})) if (v !== "" && v != null) p.set(k, String(v));
  return p;
}

function suiteAlertsApply(value) {
  Object.assign(suiteAlerts, { value, error: "", readAt: Date.now() });
  if (value && value.badge != null) Object.assign(suiteAlertCount, { n: value.badge, highest: value.highest || "", readAt: Date.now() });
  suiteAlertsTouch();
}

function suiteLoadAlerts(force) {
  if (suiteAlerts.task || (!force && Date.now() - suiteAlerts.readAt < 60000)) return suiteAlerts.task;
  const generation = sessionGeneration;
  const q = suiteAlerts.q;
  const params = suiteAlertParams({ offset: q.offset, limit: SUITE_ALERT_PAGE, refresh: force === true ? 1 : "" });
  suiteAlerts.task = plugin("GET", "/suite/alerts/list?" + params).then((value) => {
    if (generation === sessionGeneration) suiteAlertsApply(value);
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) (suiteAlerts.error = error.message || String(error)), suiteAlertsTouch();
    })
    .finally(() => {
      suiteAlerts.task = null;
      renderSuiteAlerts();
      if ($("suite-nav")) suiteRenderNav(activeTab(false));
    });
  return suiteAlerts.task;
}

function suiteAlertsReload() {
  suiteAlerts.readAt = 0;
  suiteAlerts.open.clear();
  if (suiteAlerts.task) suiteAlerts.task.then(() => suiteLoadAlerts(false));
  else suiteLoadAlerts(false);
  suiteAlertsTouch();
  renderSuiteAlerts();
}

function suiteAlertSetQuery(patch) {
  Object.assign(suiteAlerts.q, patch, "offset" in patch ? {} : { offset: 0 });
  suiteAlerts.sel.clear();
  suiteAlertsReload();
}

// Badge: a 100-byte count, not the alert list.
function suiteAlertBadge() {
  if (currentRole !== "admin") return 0;
  if (!suiteAlertCount.task && Date.now() - suiteAlertCount.readAt > 60000) {
    const generation = sessionGeneration;
    suiteAlertCount.readAt = Date.now();
    suiteAlertCount.task = plugin("GET", "/suite/alerts/count").then((v) => {
      if (generation !== sessionGeneration) return;
      const changed = v.badge !== suiteAlertCount.n;
      Object.assign(suiteAlertCount, { n: v.badge || 0, highest: v.highest || "" });
      if (changed && $("suite-nav")) suiteRenderNav(activeTab(false));
    })
      .catch((error) => { if (error instanceof AuthError) fail(error); })
      .finally(() => { suiteAlertCount.task = null; });
  }
  return suiteAlertCount.n || 0;
}

function suiteAlertPost(path, body, after) {
  suiteAlerts.busy = path;
  suiteAlertsTouch();
  renderSuiteAlerts();
  return plugin("POST", "/suite/alerts/" + path, body).then((r) => {
    after?.(r);
    suiteAlerts.sel.clear();
    suiteAlertCount.readAt = 0;
    suiteAlerts.hist.value = null;
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      suiteAlerts.error = error.message || String(error);
    })
    .finally(() => {
      suiteAlerts.busy = "";
      suiteAlertsReload();
    });
}
// Old single-item actions keep their endpoints.
function suiteAlertAction(path, body) { return suiteAlertPost(path, body); }

function suiteDangerConfirm(title, body, fn, label) {
  const btn = $("action-confirm");
  btn.classList.add("danger");
  const p = openActionDialog(title, body, undefined, fn, label);
  p.finally(() => btn.classList.remove("danger"));
  return p;
}

function suiteLoadAlertHistory() {
  const h = suiteAlerts.hist;
  if (h.task) return h.task;
  const p = new URLSearchParams({ offset: String(h.offset), limit: String(SUITE_HIST_PAGE) });
  if (h.kind) p.set("kind", h.kind);
  h.task = plugin("GET", "/suite/alerts/history?" + p).then((v) => Object.assign(h, { value: v, error: "" }))
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) h.error = error.message || String(error);
    })
    .finally(() => {
      h.task = null;
      suiteAlertsTouch();
      renderSuiteAlerts();
    });
  return h.task;
}

async function suiteAlertExport(button) {
  button.disabled = true;
  try {
    const p = suiteAlertParams();
    if (!suiteAlerts.q.status || suiteAlerts.q.status === "open") p.set("status", suiteAlerts.q.status || "open");
    p.delete("group");
    const resp = await fetch(location.origin + PLUGIN_BASE + "/suite/alerts/export.csv?" + p, {
      cache: "no-store",
      headers: { Authorization: "Bearer " + authKey }
    });
    if (resp.status === 401) throw new AuthError(m("ui.invalid_management_key_or_access_denied"));
    if (!resp.ok) throw new UIError(m("ui.server_error_http_value", { v0: resp.status }));
    const text = await resp.text();
    const name = /filename="([^"]+)"/.exec(resp.headers.get("Content-Disposition") || "")?.[1] || "cpa-suite-alerts.csv";
    downloadCSV(name, text);
  } catch (error) {
    if (error instanceof AuthError) return fail(error);
    notify(error.message || String(error), "error");
  } finally {
    button.disabled = false;
  }
}

function suiteAlertWho(a) { return a.payload?.name || (a.source === "usage" ? suiteScopeName(a.object) : a.object); }

function suiteMuteMenu(a) {
  return el(
    "span",
    { class: "suite-card-actions" },
    [60, 1440, 0].map((min) =>
      el("button", {
        type: "button",
        class: "btn small",
        disabled: !!suiteAlerts.busy,
        text: m(min === 0 ? "suite.mute_forever" : min === 60 ? "suite.mute_1h" : "suite.mute_1d"),
        onclick: () => suiteAlertPost("mute", { source: a.source, object: a.object, minutes: min })
      })
    )
  );
}

function suiteAlertRow(a, selectable) {
  const check = selectable && a.status === "active" && !a.ack_at
    ? el("input", {
        type: "checkbox",
        class: "suite-alert-check",
        "aria-label": m("suite.alert_select"),
        "data-alert-select": a.id,
        checked: suiteAlerts.sel.has(a.id),
        onchange: (e) => {
          e.target.checked ? suiteAlerts.sel.add(a.id) : suiteAlerts.sel.delete(a.id);
          suiteAlertsTouch();
          renderSuiteAlerts();
        }
      })
    : selectable ? el("span", { class: "suite-alert-check" }) : null;
  return el(
    "div",
    { class: "suite-alert-row" + (selectable ? " selectable" : "") + (a.ack_at || a.muted_until ? " muted" : ""), "data-alert-id": a.id },
    check,
    el("span", { class: "suite-alert-tags" }, suiteSevTag(a.severity), el("span", { class: "tag plain", text: m("suite.src_" + a.source) })),
    el(
      "span",
      { class: "suite-alert-main" },
      el("span", { class: "mask-blur", text: suiteAlertText(a) }),
      el("span", { class: "small muted", text: m("suite.alert_times", { v0: when(a.first_at), v1: when(a.last_at), v2: int(a.count) }) }),
      suiteAlertBuckets(a)
    ),
    a.status !== "active"
      ? el(
          "span",
          { class: "suite-card-actions" },
          el("span", { class: "small muted", text: m("suite.resolved_at", { v0: when(a.resolved_at) }) }),
          a.cooldown_until
            ? el("span", { class: "tag plain", "data-tip": m("suite.cooldown_tip", { v0: when(a.cooldown_until) }), text: m("suite.cooldown") })
            : null
        )
      : el(
          "span",
          { class: "suite-card-actions" },
          a.ack_at
            ? el("span", { class: "small muted", text: m("suite.acked_by", { v0: a.ack_by, v1: when(a.ack_at) }) })
            : el("button", {
                type: "button",
                class: "btn small",
                disabled: !!suiteAlerts.busy,
                text: m("suite.ack"),
                onclick: () => suiteAlertPost("ack", { id: a.id })
              }),
          a.muted_until
            ? el("span", { class: "small muted", text: suiteForever(a.muted_until) ? m("suite.mute_forever") : m("suite.muted_until", { v0: when(a.muted_until) }) })
            : suiteMuteMenu(a)
        )
  );
}

// One source+object group; expanding loads its alerts (same filters).
function suiteAlertGroupRow(g) {
  const key = g.source + "|" + g.object;
  const state = suiteAlerts.open.get(key);
  const who = g.payload?.name || (g.source === "usage" ? suiteScopeName(g.object) : g.object);
  const toggle = () => {
    if (suiteAlerts.open.has(key)) suiteAlerts.open.delete(key);
    else {
      const st = { items: null, error: "" };
      suiteAlerts.open.set(key, st);
      const p = suiteAlertParams({ limit: 100 });
      p.delete("group");
      p.set("source", g.source);
      p.set("object", g.object);
      p.set("summary", "0");
      plugin("GET", "/suite/alerts/list?" + p).then((v) => { st.items = v.items || []; })
        .catch((error) => { if (error instanceof AuthError) return fail(error); st.error = error.message || String(error); })
        .finally(() => { suiteAlertsTouch(); renderSuiteAlerts(); });
    }
    suiteAlertsTouch();
    renderSuiteAlerts();
  };
  return el(
    "div",
    { class: "suite-alert-group" + (state ? " open" : ""), "data-alert-group": key },
    el(
      "div",
      { class: "suite-alert-row suite-alert-group-head" },
      el("button", { type: "button", class: "suite-expander", "aria-expanded": String(!!state), "aria-label": m("suite.alert_expand"), text: state ? "▾" : "▸", onclick: toggle }),
      el("span", { class: "suite-alert-tags" }, suiteSevTag(g.severity), el("span", { class: "tag plain", text: m("suite.src_" + g.source) })),
      el(
        "span",
        { class: "suite-alert-main", onclick: toggle },
        el("span", { class: "mask-blur", text: who }),
        el("span", {
          class: "small muted",
          text: m("suite.alert_group_line", { v0: int(g.alerts), v1: int(g.active), v2: int(g.count), v3: when(g.last_at) }) +
            (g.rules?.length ? DISPLAY_SEPARATOR + g.rules.slice(0, 4).join(", ") + (g.rules.length > 4 ? "…" : "") : "")
        })
      ),
      el(
        "span",
        { class: "suite-card-actions" },
        g.muted_until
          ? el("button", { type: "button", class: "btn small", disabled: !!suiteAlerts.busy, text: m("suite.unmute"), onclick: () => suiteAlertPost("unmute", { source: g.source, object: g.object, minutes: 0 }) })
          : suiteMuteMenu(g)
      )
    ),
    state
      ? el(
          "div",
          { class: "suite-alert-group-body" },
          state.error
            ? el("div", { class: "notice err show", text: state.error })
            : state.items
              ? state.items.length ? state.items.map((a) => suiteAlertRow(a, false)) : el("div", { class: "empty", text: m("suite.alerts_none") })
              : el("div", { class: "empty section-loading", text: m("ui.loading") })
        )
      : null
  );
}

function suitePager(offset, size, total, go) {
  if (total <= size && offset === 0) return null;
  const from = total ? offset + 1 : 0, to = Math.min(offset + size, total);
  return el(
    "div",
    { class: "suite-pager" },
    el("span", { class: "small muted mono", text: m("suite.pager", { v0: int(from), v1: int(to), v2: int(total) }) }),
    el("button", { type: "button", class: "btn small", "data-pager": "prev", disabled: offset <= 0, text: m("suite.prev"), onclick: () => go(Math.max(0, offset - size)) }),
    el("button", { type: "button", class: "btn small", "data-pager": "next", disabled: to >= total, text: m("suite.next"), onclick: () => go(offset + size) })
  );
}

function suiteAlertSummary(v) {
  const s = v.summary || {};
  const card = (id, label, value, patch, tone) =>
    el(
      "button",
      { type: "button", class: "card suite-stat clickable suite-alert-stat" + (tone ? " " + tone : ""), "data-alert-stat": id, onclick: () => suiteAlertSetQuery(patch) },
      el("span", { class: "suite-stat-label", text: label }),
      el("span", { class: "suite-stat-value", text: int(value || 0) })
    );
  const top = (s.top || []).length
    ? el(
        "div",
        { class: "suite-alert-top small" },
        el("span", { class: "muted", text: m("suite.alert_top") }),
        s.top.map((t) =>
          el("button", {
            type: "button",
            class: "tag plain mask-blur",
            text: m("suite.src_" + t.source) + " · " + (t.source === "usage" ? suiteScopeName(t.object) : t.object) + " ×" + int(t.count),
            onclick: () => suiteAlertSetQuery({ status: "active", source: t.source, q: t.object, group: false })
          })
        )
      )
    : null;
  return el(
    "div",
    {},
    el(
      "div",
      { class: "suite-grid g6 suite-alert-stats" },
      card("open", m("suite.as_open"), s.badge, { status: "open", severity: "" }, s.badge ? "warn" : ""),
      card("high", m("suite.sev_high"), s.high, { status: "active", severity: "high" }, s.high ? "bad" : ""),
      card("medium", m("suite.sev_medium"), s.medium, { status: "active", severity: "medium" }),
      card("new", m("suite.as_new24h"), s.new_24h, { status: "all", severity: "", since: "24h" }),
      card("muted", m("suite.as_muted"), s.muted, { status: "muted", severity: "" }),
      card("resolved", m("suite.as_resolved"), s.resolved, { status: "resolved", severity: "" })
    ),
    top
  );
}

function suiteAlertToolbar() {
  const q = suiteAlerts.q;
  const sel = (key, options, label) =>
    el(
      "select",
      { "data-alert-q": key, "aria-label": label, onchange: (e) => suiteAlertSetQuery({ [key]: e.target.value }) },
      options.map(([v, t]) => el("option", { value: v, text: t, selected: q[key] === v }))
    );
  const search = el("input", {
    type: "search",
    value: q.q,
    placeholder: m("suite.alert_search"),
    "aria-label": m("suite.alert_search"),
    "data-alert-search": "",
    oninput: (e) => {
      clearTimeout(suiteAlerts.searchTimer);
      suiteAlerts.searchTimer = setTimeout(() => suiteAlertSetQuery({ q: e.target.value.trim() }), 400);
    }
  });
  return el(
    "div",
    { class: "suite-toolbar" },
    el(
      "div",
      { class: "suite-segment", role: "group" },
      ["open", "active", "acked", "muted", "resolved", "all"].map((id) =>
        el("button", {
          type: "button",
          class: "suite-pill" + (id === q.status ? " active" : ""),
          "data-alert-status": id,
          text: m("suite.alert_status_" + id),
          onclick: () => suiteAlertSetQuery({ status: id })
        })
      )
    ),
    sel("severity", [["", m("suite.alert_any_severity")], ["high", m("suite.sev_high")], ["medium", m("suite.sev_medium")], ["info", m("suite.sev_info")]], m("suite.alert_any_severity")),
    sel("source", [["", m("suite.alert_any_source")], ...SUITE_ALERT_SOURCES.map((s) => [s, m("suite.src_" + s)])], m("suite.alert_any_source")),
    sel("since", [["", m("suite.alert_any_time")], ["1h", m("suite.since_1h")], ["24h", m("suite.since_24h")], ["168h", m("suite.since_7d")]], m("suite.alert_any_time")),
    search,
    el(
      "label",
      { class: "suite-check" },
      el("input", { type: "checkbox", checked: q.group, "data-alert-group-toggle": "", onchange: (e) => suiteAlertSetQuery({ group: e.target.checked }) }),
      el("span", { text: m("suite.alert_group_by") })
    ),
    JSON.stringify(q) !== JSON.stringify(suiteAlertQueryDefault())
      ? el("button", { type: "button", class: "btn small", "data-alert-reset": "", text: m("suite.alert_clear_filters"), onclick: () => suiteAlertSetQuery(suiteAlertQueryDefault()) })
      : null
  );
}

function suiteAlertBulkBar(items) {
  const n = suiteAlerts.sel.size;
  const selectable = items.filter((a) => a.status === "active" && !a.ack_at);
  const allOn = selectable.length && selectable.every((a) => suiteAlerts.sel.has(a.id));
  const objects = () => {
    const seen = new Map();
    for (const a of items) if (suiteAlerts.sel.has(a.id)) seen.set(a.source + "|" + a.object, { source: a.source, object: a.object });
    return [...seen.values()];
  };
  return el(
    "div",
    { class: "suite-bulk" + (n ? " on" : "") },
    el(
      "label",
      { class: "suite-check" },
      el("input", {
        type: "checkbox",
        checked: !!allOn,
        disabled: !selectable.length,
        "data-alert-select-all": "",
        onchange: (e) => {
          for (const a of selectable) e.target.checked ? suiteAlerts.sel.add(a.id) : suiteAlerts.sel.delete(a.id);
          suiteAlertsTouch();
          renderSuiteAlerts();
        }
      }),
      el("span", { text: n ? m("suite.alert_selected", { v0: int(n) }) : m("suite.alert_select_page") })
    ),
    n
      ? el(
          "span",
          { class: "suite-card-actions" },
          el("button", { type: "button", class: "btn small primary", "data-bulk": "ack", disabled: !!suiteAlerts.busy, text: m("suite.bulk_ack"), onclick: () => suiteAlertPost("bulk", { action: "ack", ids: [...suiteAlerts.sel] }, (r) => notify(m("suite.bulk_done", { v0: int(r.changed) }))) }),
          [60, 1440].map((min) =>
            el("button", {
              type: "button",
              class: "btn small",
              "data-bulk": "mute" + min,
              disabled: !!suiteAlerts.busy,
              text: m(min === 60 ? "suite.bulk_mute_1h" : "suite.bulk_mute_1d"),
              onclick: () => suiteAlertPost("bulk", { action: "mute", objects: objects(), minutes: min }, (r) => notify(m("suite.bulk_muted", { v0: int(r.changed) })))
            })
          )
        )
      : null
  );
}

function suiteAlertHistoryCard() {
  const h = suiteAlerts.hist;
  if (!h.value && !h.task && !h.error) suiteLoadAlertHistory();
  const items = h.value?.items || [];
  const kinds = ["", "opened", "reopened", "escalated", "resolved", "acked", "muted", "unmuted"];
  return el(
    "details",
    { class: "card suite-card suite-fold", "data-alert-history": "", open: suiteAlerts.histOpen || null, ontoggle: (e) => { suiteAlerts.histOpen = e.target.open; } },
    el("summary", { class: "suite-card-head" }, el("h2", { text: m("suite.alerts_history") }), el("span", { class: "small muted", text: h.value ? m("suite.history_total", { v0: int(h.value.total) }) : "" })),
    el(
      "div",
      { class: "suite-toolbar" },
      el(
        "select",
        { "aria-label": m("suite.history_kind"), "data-history-kind": "", onchange: (e) => { Object.assign(h, { kind: e.target.value, offset: 0 }); suiteLoadAlertHistory(); } },
        kinds.map((k) => el("option", { value: k, text: k ? m("suite.ev_" + k) : m("suite.history_any_kind"), selected: h.kind === k }))
      )
    ),
    h.error ? el("div", { class: "notice err show", text: h.error }) : null,
    items.length
      ? el(
          "div",
          { class: "suite-quota-list" },
          items.map((e) =>
            el(
              "div",
              { class: "suite-history-row" },
              el("span", { class: "small muted mono", text: when(e.at) }),
              el("span", { class: "suite-alert-tags" }, el("span", { class: "tag plain", text: m("suite.ev_" + e.kind) }), e.severity ? suiteSevTag(e.severity) : null),
              el(
                "span",
                { class: "small" },
                e.source ? el("span", { class: "muted", text: m("suite.src_" + e.source) + " · " }) : null,
                el("span", { class: "mask-blur", text: e.source ? suiteAlertWho(e) + (e.rule ? " · " + e.rule : "") : e.alert_id.split("|").join(" · ") })
              ),
              el("span", { class: "small muted", text: e.by || "" })
            )
          )
        )
      : h.value ? el("div", { class: "empty", text: m("suite.history_none") }) : el("div", { class: "empty section-loading", text: m("ui.loading") }),
    h.value ? suitePager(h.offset, SUITE_HIST_PAGE, h.value.total, (o) => { h.offset = o; suiteLoadAlertHistory(); }) : null
  );
}

function renderSuiteAlerts() {
  if (currentRole !== "admin" || activeTab(false) !== "alerts") return false;
  suiteLoadAlerts(false);
  const target = $("suite-alerts");
  const v = suiteAlerts.value;
  const stamp = [suiteAlerts.rev, suiteAlerts.busy, !!suiteAlerts.task, document.documentElement.lang].join("|");
  if (suiteAlerts.painted === stamp && target.childElementCount) return false;
  suiteAlerts.painted = stamp;
  if (!v)
    return (
      target.replaceChildren(
        el("div", { class: "card" }, el("div", { class: suiteAlerts.error ? "notice err show" : "empty section-loading", text: suiteAlerts.error || m("ui.loading") }))
      ),
      false
    );
  if (v.mode === "off") return (suiteModuleOff(target, "alerts"), false);
  const focused = document.activeElement?.hasAttribute?.("data-alert-search");
  const q = suiteAlerts.q;
  const items = v.items || [];
  const groups = v.groups || [];
  const sources = Object.entries(v.sources || {}).map(([s, on]) =>
    el("span", { class: "tag " + (on ? "ok" : "plain"), text: m("suite.src_" + s) + (on ? "" : " · " + m("suite.off")) })
  );
  const list = q.group
    ? groups.length ? el("div", { class: "suite-quota-list" }, groups.map(suiteAlertGroupRow)) : el("div", { class: "empty", text: m("suite.alerts_none") })
    : items.length ? el("div", { class: "suite-alert-list" }, items.map((a) => suiteAlertRow(a, true))) : el("div", { class: "empty", text: m("suite.alerts_none") });
  const mutes = v.mutes || [];
  target.replaceChildren(
    suiteCard(
      m("suite.nav_alerts"),
      el(
        "span",
        { class: "suite-card-actions" },
        el("button", { type: "button", class: "btn small", "data-alert-export": "", text: m("suite.alert_export"), onclick: (e) => suiteAlertExport(e.currentTarget) }),
        el("button", {
          type: "button",
          class: "btn small",
          "data-alert-clear": "",
          disabled: !!suiteAlerts.busy || !(v.summary?.resolved > 0),
          text: m("suite.alert_clear_resolved"),
          onclick: () =>
            suiteDangerConfirm(m("suite.alert_clear_resolved"), m("suite.alert_clear_body", { v0: int(v.summary?.resolved || 0) }), () =>
              suiteAlertPost("clear-resolved", {}, (r) => notify(m("suite.alert_cleared", { v0: int(r.removed) }))), m("suite.alert_clear_confirm"))
        }),
        el("button", { type: "button", class: "btn small", disabled: !!suiteAlerts.task, text: m("ui.refresh"), onclick: () => { suiteAlerts.readAt = 0; suiteLoadAlerts(true); } })
      ),
      el("div", { class: "small muted" }, m("suite.alerts_hint", { v0: v.retention_days ?? 90 }), " ", suiteLink(m("suite.alert_settings_link"), suiteSub("suite-config"))),
      el("div", { class: "suite-card-actions" }, sources),
      suiteAlerts.error ? el("div", { class: "notice err show", text: suiteAlerts.error }) : null,
      suiteAlertSummary(v)
    ),
    suiteCard(
      m(q.group ? "suite.alerts_groups" : "suite.alerts_list", { v0: int(v.total || 0) }),
      suiteAlerts.task ? el("span", { class: "small muted", text: m("ui.loading") }) : null,
      suiteAlertToolbar(),
      q.status === "open" ? el("div", { class: "small muted", text: m("suite.alerts_info_hint") }) : null,
      q.group ? null : suiteAlertBulkBar(items),
      list,
      suitePager(v.offset || 0, v.limit || SUITE_ALERT_PAGE, v.total || 0, (o) => { suiteAlertSetQuery({ offset: o }); window.scrollTo({ top: 0 }); })
    ),
    el(
      "details",
      { class: "card suite-card suite-fold", "data-alert-mutes": "", open: suiteAlerts.mutesOpen || null, ontoggle: (e) => { suiteAlerts.mutesOpen = e.target.open; } },
      el("summary", { class: "suite-card-head" }, el("h2", { text: m("suite.alerts_mutes") }), el("span", { class: "tag plain", text: int(mutes.length) })),
      mutes.length
        ? el(
            "div",
            { class: "suite-quota-list" },
            mutes.map((x) =>
              el(
                "div",
                { class: "suite-alert-row" },
                el("span", { class: "suite-alert-tags" }, el("span", { class: "tag plain", text: m("suite.src_" + x.source) })),
                el("span", { class: "mask-blur" }, x.source === "usage" ? suiteScopeName(x.object) : x.object),
                el("span", { class: "suite-card-actions" },
                  el("span", { class: "small muted", text: suiteForever(x.until) ? m("suite.mute_forever") : m("suite.muted_until", { v0: when(x.until) }) }),
                  el("button", { type: "button", class: "btn small", disabled: !!suiteAlerts.busy, text: m("suite.unmute"), onclick: () => suiteAlertPost("unmute", { source: x.source, object: x.object }) }))
              )
            )
          )
        : el("div", { class: "empty", text: m("suite.mutes_none") })
    ),
    suiteAlertHistoryCard()
  );
  if (focused) {
    const input = target.querySelector("[data-alert-search]");
    input?.focus();
    input?.setSelectionRange?.(input.value.length, input.value.length);
  }
  return false;
}

// Merged degradation alert: one per account, buckets expandable.
function suiteAlertBuckets(a) {
  const list = a.payload?.buckets || [];
  if (list.length < 2) return null;
  return el(
    "details",
    { class: "suite-alert-more small" },
    el("summary", { text: m("suite.alert_more", { v0: int(list.length) }) }),
    list.map((b) =>
      el(
        "div",
        { class: "suite-alert-sub muted" },
        suiteSevTag(b.severity),
        " ",
        el("span", {
          class: "mono",
          text: [b.model, b.effort, b.stream ? m("suite.stream") : m("suite.non_stream")].filter(Boolean).join(DISPLAY_SEPARATOR)
        }),
        " · ",
        el("span", { text: (b.signals || []).map((s) => m("suite.sig_" + s)).join("、") })
      )
    )
  );
}

function suiteLoadAlertSettings(force) {
  if (suiteAlertCfg.task || (!force && suiteAlertCfg.value)) return suiteAlertCfg.task;
  suiteAlertCfg.task = plugin("GET", "/suite/alerts/settings").then((value) => Object.assign(suiteAlertCfg, { value, error: "" }))
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteAlertCfg.error = error.message || String(error);
    })
    .finally(() => {
      suiteAlertCfg.task = null;
      suiteConfigRepaint();
    });
  return suiteAlertCfg.task;
}

function suiteSaveAlertSettings(body) {
  suiteAlertCfg.busy = true;
  suiteConfigRepaint();
  return plugin("PUT", "/suite/alerts/settings", body).then((value) => {
    Object.assign(suiteAlertCfg, { value, error: "", saved: Date.now() });
    suiteAlerts.readAt = 0;
    suiteAlertCount.readAt = 0;
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      suiteAlertCfg.error = error.message || String(error);
    })
    .finally(() => {
      suiteAlertCfg.busy = false;
      suiteConfigRepaint();
    });
}

const SUITE_ALERT_FIELDS = [
  [
    "quota",
    [
      "quota_remain_5h",
      "quota_remain_weekly",
      "quota_high_5h",
      "quota_high_weekly",
      "quota_early_frac",
      "quota_quiet_5h_minutes",
      "quota_quiet_weekly_hours"
    ]
  ],
  [
    "usage",
    [
      "usage_min_active_days",
      "usage_ratio",
      "usage_min_delta_usd",
      "usage_min_base_usd",
      "usage_high_ratio",
      "usage_high_delta_usd",
      "usage_new_key_usd",
      "usage_min_delta_requests",
      "usage_active_usd",
      "usage_active_requests"
    ]
  ],
  ["debounce", ["open_after_evaluations", "close_after_minutes", "reopen_cooldown_hours"]]
];
const SUITE_ALERT_SOURCES = ["quota", "usage", "blocks", "monitor", "auth_errors"];

function suiteAlertSettingsCard() {
  suiteLoadAlertSettings(false);
  const v = suiteAlertCfg.value;
  if (!v)
    return suiteCard(
      m("suite.alert_settings"),
      null,
      el("div", { class: suiteAlertCfg.error ? "notice err show" : "empty section-loading", text: suiteAlertCfg.error || m("ui.loading") })
    );
  const s = v.settings || {}, d = v.defaults || {}, locked = new Set(v.locked || []);
  const inputs = {};
  const field = (key) => {
    const input = el("input", {
      type: "number",
      min: "0",
      step: key === "quota_early_frac" ? "0.05" : "any",
      value: String(s[key] ?? ""),
      disabled: locked.has(key) || suiteAlertCfg.busy,
      "data-alert-setting": key
    });
    inputs[key] = input;
    return el(
      "label",
      {
        class: "field",
        "data-tip": (locked.has(key) ? m("suite.alert_locked") + "\n" : "") + m("suite.alert_default", { v0: String(d[key] ?? "—") })
      },
      el("span", { text: m("suite.as_" + key) + (locked.has(key) ? " 🔒" : "") }),
      input
    );
  };
  const sevSelect = (src) => {
    const sel = el(
      "select",
      { "data-alert-minsev": src, disabled: locked.has("min_severity") || suiteAlertCfg.busy },
      ["info", "medium", "high"].map((x) =>
        el("option", { value: x, text: m("suite.sev_" + x), selected: (s.min_severity?.[src] || "info") === x })
      )
    );
    inputs["min:" + src] = sel;
    return el("label", { class: "field" }, el("span", { text: m("suite.src_" + src) }), sel);
  };
  const usageCap = el(
    "select",
    { "data-alert-setting": "usage_severity", disabled: locked.has("usage_severity") || suiteAlertCfg.busy },
    ["info", "medium", "high"].map((x) =>
      el("option", { value: x, text: m("suite.sev_" + x), selected: (s.usage_severity || "info") === x })
    )
  );
  const collect = () => {
    const out = {};
    for (const [k, input] of Object.entries(inputs)) {
      if (input.disabled || k.startsWith("min:")) continue;
      const n = Number(input.value);
      if (input.value !== "" && Number.isFinite(n) && n !== s[k]) out[k] = n;
    }
    if (!locked.has("usage_severity") && usageCap.value !== s.usage_severity) out.usage_severity = usageCap.value;
    if (!locked.has("min_severity")) {
      const ms = {};
      for (const src of SUITE_ALERT_SOURCES)
        if (inputs["min:" + src].value !== (s.min_severity?.[src] || "info")) ms[src] = inputs["min:" + src].value;
      if (Object.keys(ms).length) out.min_severity = ms;
    }
    return out;
  };
  return suiteCard(
    m("suite.alert_settings"),
    el(
      "span",
      { class: "suite-card-actions" },
      el("button", {
        type: "button",
        class: "btn small",
        disabled: suiteAlertCfg.busy,
        text: m("suite.alert_reset"),
        onclick: () =>
          openActionDialog(
            m("suite.alert_reset"),
            m("suite.alert_reset_body"),
            undefined,
            () => suiteSaveAlertSettings({ reset: true }),
            m("suite.alert_reset")
          )
      }),
      el("button", {
        type: "button",
        class: "btn small primary",
        "data-alert-save": "",
        disabled: suiteAlertCfg.busy,
        text: m("suite.alert_save"),
        onclick: () => {
          const changes = collect();
          if (Object.keys(changes).length) suiteSaveAlertSettings({ settings: changes });
        }
      })
    ),
    el("div", { class: "small muted", text: m("suite.alert_settings_hint") }),
    suiteAlertCfg.error ? el("div", { class: "notice err show", text: suiteAlertCfg.error }) : null,
    suiteAlertCfg.saved && Date.now() - suiteAlertCfg.saved < 8000
      ? el("div", { class: "notice ok show", text: m("suite.alert_saved") })
      : null,
    ...SUITE_ALERT_FIELDS.map(([group, keys]) =>
      el(
        "div",
        { class: "suite-settings-group" },
        el("div", { class: "suite-sig-group small muted", text: m("suite.asg_" + group) }),
        el(
          "div",
          { class: "suite-settings-grid" },
          keys.map(field),
          group === "usage" ? el("label", { class: "field" }, el("span", { text: m("suite.as_usage_severity") }), usageCap) : null
        )
      )
    ),
    el(
      "div",
      { class: "suite-settings-group" },
      el("div", { class: "suite-sig-group small muted", text: m("suite.asg_min_severity") }),
      el("div", { class: "suite-settings-grid" }, SUITE_ALERT_SOURCES.map(sevSelect))
    )
  );
}

/* ---------- 0.7: settings › alerts & data retention ---------- */
const suiteRetention = { value: null, error: "", task: null, busy: false };
function suiteConfigRepaint() {
  suiteAlerts.painted = "";
  suiteModules.painted = "";
  renderSuiteAlerts();
  renderSuiteModules();
}
function suiteLoadRetention(force) {
  if (suiteRetention.task || (!force && suiteRetention.value)) return suiteRetention.task;
  suiteRetention.task = plugin("GET", "/suite/retention").then((value) => Object.assign(suiteRetention, { value, error: "" }))
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteRetention.error = error.message || String(error);
    })
    .finally(() => { suiteRetention.task = null; suiteConfigRepaint(); });
  return suiteRetention.task;
}
function suiteSaveRetention(body) {
  suiteRetention.busy = true;
  suiteConfigRepaint();
  return plugin("PUT", "/suite/retention", body).then((value) => {
    Object.assign(suiteRetention, { value, error: "" });
    suiteAlerts.readAt = 0;
    suiteAlertCount.readAt = 0;
    suiteMonitor.readAt = 0;
    notify(m("suite.retention_saved"));
  })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      suiteRetention.error = error.message || String(error);
    })
    .finally(() => { suiteRetention.busy = false; suiteConfigRepaint(); });
}
const SUITE_RETENTION_FIELDS = [
  ["alerts_days", ["alerts", "alert_events"]],
  ["monitor_days", ["monitor_samples"]],
  ["candy_days", ["candy_runs"]],
  ["quota_days", ["quota_samples"]]
];
function suiteRetentionCard() {
  suiteLoadRetention(false);
  const v = suiteRetention.value;
  if (!v)
    return suiteCard(m("suite.retention"), null,
      el("div", { class: suiteRetention.error ? "notice err show" : "empty section-loading", text: suiteRetention.error || m("ui.loading") }));
  const inputs = {};
  const rows = SUITE_RETENTION_FIELDS.map(([key, tables]) => {
    const b = v.bounds?.[key] || {};
    const input = el("input", {
      type: "number", min: String(b.min), max: String(b.max), step: "1",
      value: String(v.retention?.[key] ?? ""), disabled: suiteRetention.busy, "data-retention": key
    });
    inputs[key] = input;
    return el("label", { class: "field", "data-tip": m("suite.retention_range", { v0: b.min, v1: b.max, v2: b.default }) },
      el("span", { text: m("suite.ret_" + key) }),
      input,
      el("span", { class: "small muted", text: tables.map((t) => m("suite.ret_rows_" + t, { v0: int(v.counts?.[t] || 0) })).join(DISPLAY_SEPARATOR) }));
  });
  const collect = () => {
    const out = {};
    for (const [k, input] of Object.entries(inputs)) {
      const n = Math.round(Number(input.value));
      const b = v.bounds?.[k] || {};
      if (!Number.isFinite(n) || n < b.min || n > b.max) throw new UIError(m("suite.retention_bad", { v0: m("suite.ret_" + k), v1: b.min, v2: b.max }));
      out[k] = n;
    }
    return out;
  };
  return suiteCard(
    m("suite.retention"),
    el("span", { class: "suite-card-actions" },
      el("button", { type: "button", class: "btn small", disabled: suiteRetention.busy, text: m("suite.alert_reset"),
        onclick: () => suiteDangerConfirm(m("suite.retention"), m("suite.retention_reset_body"), () => suiteSaveRetention({ reset: true }), m("suite.alert_reset")) }),
      el("button", { type: "button", class: "btn small primary", "data-retention-save": "", disabled: suiteRetention.busy, text: m("suite.save"),
        onclick: () => {
          let r;
          try { r = collect(); } catch (e) { return notify(e.message, "error"); }
          const shrink = Object.keys(r).some((k) => r[k] < (v.retention?.[k] ?? 0));
          if (!shrink) return suiteSaveRetention({ retention: r });
          suiteDangerConfirm(m("suite.retention"), m("suite.retention_shrink_body"), () => suiteSaveRetention({ retention: r }), m("suite.save"));
        } })),
    el("div", { class: "small muted", text: m("suite.retention_hint") }),
    suiteRetention.error ? el("div", { class: "notice err show", text: suiteRetention.error }) : null,
    el("div", { class: "suite-settings-grid suite-retention-grid" }, rows)
  );
}
function renderSuiteConfig(target) {
  target.replaceChildren(suiteAlertSettingsCard(), suiteRetentionCard());
}
