/* CPA Suite layout layer: five top-level groups with sub-tab pills that
 * re-host the upstream panels unchanged (same renderers, same API calls).
 * Upstream files only carry one-line hooks; see docs/UPSTREAM_PATCHES.md. */

const SUITE_SETTINGS_PARTS = ["plans", "routes", "prices", "plugin-logs"];
const SUITE_PART_KEY = "cpa-suite.settings-part.v1";
const SUITE_VIEW_KEY = "cpa-suite.modules-view.v1";
function suiteModulesView() {
  try { if (sessionStorage.getItem(SUITE_VIEW_KEY) === "config") return "config"; } catch (_) {}
  return "modules";
}
ADMIN_TAB_IDS.splice(ADMIN_TAB_IDS.indexOf("settings"), 0, "forecast", "ranking", "alerts", "monitor"); // + 0.6 pages
const SUITE_GROUPS = [
  {
    id: "overview",
    label: () => m("suite.nav_overview"),
    subs: [{ id: "overview", tab: "overview", label: () => m("suite.nav_overview") }]
  },
  {
    id: "billing",
    label: () => m("suite.nav_billing"),
    subs: [
      { id: "keys", tab: "keys", label: () => "API Key" },
      { id: "plans", tab: "settings", part: "plans", label: () => m("ui.subscription_plans") },
      { id: "routes", tab: "settings", part: "routes", label: () => m("ui.routing_rules") },
      { id: "prices", tab: "settings", part: "prices", label: () => m("suite.nav_prices") },
      { id: "analysis", tab: "analysis", label: () => m("ui.analysis") }
    ]
  },
  {
    id: "upstream",
    label: () => m("suite.nav_upstream"),
    subs: [
      { id: "auth-files", tab: "auth-files", label: () => m("suite.nav_accounts") },
      { id: "forecast", tab: "forecast", label: () => m("suite.nav_forecast") }
    ]
  },
  {
    id: "monitor",
    label: () => m("suite.nav_monitor"),
    subs: [
      { id: "clients", tab: "clients", label: () => m("suite.clients") },
      { id: "request-events", tab: "request-events", label: () => m("ui.request_events") },
      { id: "errors", tab: "errors", label: () => m("ui.error_events") },
      { id: "alerts", tab: "alerts", label: () => m("suite.nav_alerts"), badge: () => suiteAlertBadge() },
      { id: "ranking", tab: "ranking", label: () => m("suite.nav_ranking"), badge: () => suiteAnomalyCount() },
      { id: "grok", tab: "monitor", label: () => m("suite.nav_grok") }
    ]
  },
  {
    id: "settings",
    label: () => m("ui.settings"),
    subs: [
      { id: "modules", tab: "modules", view: "modules", label: () => m("suite.nav_modules") },
      { id: "suite-config", tab: "modules", view: "config", label: () => m("suite.nav_config") },
      { id: "plugin-logs", tab: "settings", part: "plugin-logs", label: () => m("suite.nav_plugin_logs") }
    ]
  }
];

function suiteSettingsPart() {
  try {
    const part = sessionStorage.getItem(SUITE_PART_KEY);
    if (SUITE_SETTINGS_PARTS.includes(part)) return part;
  } catch (_) {}
  return "plans";
}

function suiteCurrentSub(tab) {
  const part = tab === "settings" ? suiteSettingsPart() : null;
  const view = tab === "modules" ? suiteModulesView() : null;
  for (const group of SUITE_GROUPS)
    for (const sub of group.subs)
      if (!sub.soon && sub.tab === tab && (sub.part || null) === part && (sub.view || null) === view) return { group, sub };
  return { group: SUITE_GROUPS[1], sub: SUITE_GROUPS[1].subs[0] };
}

function suiteGo(sub) {
  if (sub.soon) return;
  if (sub.part) { try { sessionStorage.setItem(SUITE_PART_KEY, sub.part); } catch (_) {} }
  if (sub.view) { try { sessionStorage.setItem(SUITE_VIEW_KEY, sub.view); } catch (_) {} suiteModules.painted = ""; }
  if (activeTab(false) === sub.tab) {
    suiteAfterAdminTab(sub.tab);
    if (sub.tab === "modules") renderSuiteModules();
    window.scrollTo({ top: 0 });
    return;
  }
  setAdminTab(sub.tab);
  window.scrollTo({ top: 0 });
  if (!refreshStalePage()) guard(() => loadPage(false));
}

// Build the navigation and the suite-owned sections before app.js runs.
(function suiteBuildLayout() {
  const app = $("app");
  const row = app.querySelector(".page-tabs-row");
  const groups = el("nav", { id: "suite-nav", class: "suite-nav", "aria-label": m("suite.nav_label") });
  row.before(groups);
  const pills = el("div", { id: "suite-subnav", class: "suite-subnav", role: "tablist" });
  app.querySelector(".page-navigation").prepend(pills);
  const status = $("admin-data-status");
  status.after(
    el("section", { id: "tab-overview", class: "hidden" }, el("div", { id: "suite-overview" })),
    el("section", { id: "tab-modules", class: "hidden" }, el("div", { id: "suite-modules" })),
    el("section", { id: "tab-forecast", class: "hidden" }, el("div", { id: "suite-forecast" })),
    el("section", { id: "tab-ranking", class: "hidden" }, el("div", { id: "suite-ranking" })),
    el("section", { id: "tab-alerts", class: "hidden" }, el("div", { id: "suite-alerts" })),
    el("section", { id: "tab-monitor", class: "hidden" }, el("div", { id: "suite-monitor" }))
  );
  document.documentElement.classList.add("suite-layout");
})();

function suiteRenderNav(tab) {
  const { group, sub } = suiteCurrentSub(tab);
  $("suite-nav").replaceChildren(
    ...SUITE_GROUPS.map((g) =>
      el("button", {
        type: "button",
        class: "suite-group" + (g === group ? " active" : ""),
        "aria-current": g === group ? "page" : "false",
        "data-suite-group": g.id,
        text: g.label(),
        onclick: () => suiteGo(g === group ? sub : g.subs.find((s) => !s.soon))
      })
    )
  );
  $("suite-subnav").replaceChildren(
    ...group.subs.map((s) =>
      el(
        "button",
        {
          type: "button",
          role: "tab",
          class: "suite-pill" + (s === sub ? " active" : "") + (s.soon ? " soon" : ""),
          "aria-selected": String(s === sub),
          "data-suite-sub": s.id,
          disabled: !!s.soon,
          "data-tip": s.soon ? m("suite.coming_soon_tip") : null,
          onclick: () => suiteGo(s)
        },
        s.label(),
        s.soon ? el("span", { class: "suite-soon", text: m("suite.coming_soon") }) : null,
        s.badge?.() ? el("span", { class: "suite-badge", "data-suite-badge": s.id, text: String(s.badge()) }) : null
      )
    )
  );
}

// drop the DOM of hidden suite pages (their data stays cached),
// so switching tabs no longer piles up thousands of nodes.
const SUITE_PAINT_STATE = () => ({ overview: suiteOverview, modules: suiteModules, forecast: suiteForecast, ranking: suiteRanking, alerts: suiteAlerts, monitor: suiteMonitor });
function suiteUnmount(id, section) {
  if (id === "clients") return; // re-read on every visit already
  const state = SUITE_PAINT_STATE()[id];
  if (!state || !("painted" in state)) return;
  const host = section.querySelector(":scope > [id^='suite-']") || section.firstElementChild;
  if (!host || !host.childElementCount) return;
  host.replaceChildren();
  state.painted = "";
}

// Hooked after upstream setAdminTab: shows suite sections, narrows the
// upstream "settings" tab to one of its four panels, and syncs the nav.
function suiteAfterAdminTab(selected) {
  for (const id of ["clients", "overview", "modules", "forecast", "ranking", "alerts", "monitor"]) {
    const section = $("tab-" + id);
    section.classList.toggle("hidden", selected !== id);
    if (selected !== id) suiteUnmount(id, section);
  }
  if (selected !== "clients") suiteClients.shown = false; // re-read on the next visit
  const part = suiteSettingsPart();
  for (const name of SUITE_SETTINGS_PARTS) $("tab-" + name).classList.toggle("suite-part-hidden", selected === "settings" && name !== part);
  suiteRenderNav(selected);
  if (currentRole === "admin") suiteLoadAliases();
}

/* ---------- CPAMP key aliases as the display-name fallback ---------- */
const suiteAliases = { byScope: new Map(), state: "idle" };
function suiteAliasFor(scope) { return suiteAliases.byScope.get(scope) || ""; }
function suiteInCPAMP() {
  const root = document.documentElement;
  return root.getAttribute("data-cpamp-plugin-host") === "true" && !root.hasAttribute("data-cpamp-standalone") && window.parent !== window;
}
// Same source and matching as client-guard 0.1.2 and the upstream "Import labels"
// button, but read-only: nothing is written; a key's own label always wins.
async function suiteLoadAliases() {
  if (suiteAliases.state !== "idle" || !suiteInCPAMP()) return;
  suiteAliases.state = "loading";
  const generation = sessionGeneration;
  try {
    const result = await api("GET", "/v0/management/api-key-aliases", null, {
      raw: true,
      auxiliaryCredential: true,
      keepSessionOnForbidden: true
    });
    const items = Array.isArray(result?.items) ? result.items : [];
    const keys = await fetchConfiguredAPIKeys();
    const byHash = new Map();
    for (const key of keys) {
      const value = key.trim();
      if (value) byHash.set(await sha256Hex(value), await sha256Hex(CALLER_SCOPE_SALT + value));
    }
    if (generation !== sessionGeneration) return;
    for (const item of items) {
      const scope = byHash.get(String(item?.apiKeyHash || "").toLowerCase());
      const alias = typeof item?.alias === "string" ? item.alias.trim() : "";
      if (scope && alias) suiteAliases.byScope.set(scope, alias);
    }
    suiteAliases.state = "done";
    if (suiteAliases.byScope.size && resources.admin.keys.value) {
      resources.admin.keys.value = [...resources.admin.keys.value]; // repaint names
      renderPage();
    }
  } catch (_) { suiteAliases.state = "unavailable"; }
}

/* ---------- Overview (real data only) ---------- */
const suiteOverview = { today: null, week: null, error: "", readAt: 0, task: null, status: null, painted: "" };

function suiteLoadOverview(force) {
  if (suiteOverview.task || (!force && Date.now() - suiteOverview.readAt < 60000)) return;
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const read = (range) => plugin("GET", "/analysis?" + new URLSearchParams({ ...range, timezone }));
  const generation = sessionGeneration;
  suiteOverview.task = Promise.all([read(naturalTimeRange("today")), read(recentTimeRange("7d")), plugin("GET", "/suite/status")])
    .then(([today, week, status]) => {
      if (generation !== sessionGeneration) return;
      Object.assign(suiteOverview, { today, week, status, error: "", readAt: Date.now() });
    })
    .catch((error) => {
      if (error instanceof AuthError) return fail(error);
      if (!(error instanceof StaleRequestError)) suiteOverview.error = error.message || String(error);
    })
    .finally(() => {
      suiteOverview.task = null;
      renderSuiteOverview();
    });
  suiteLoadClients(false).catch(fail);
}

function suiteStat(label, value, sub, onclick) {
  return el(
    "div",
    { class: "card suite-stat" + (onclick ? " clickable" : ""), onclick: onclick || null },
    el("div", { class: "suite-stat-label", text: label }),
    el("div", { class: "suite-stat-value" }, value),
    sub ? el("div", { class: "suite-stat-sub", text: sub }) : null
  );
}

function suiteBars(items, valueOf, labelOf, format, limit = 6) {
  const top = items.slice(0, limit);
  if (!top.length) return el("div", { class: "empty", text: m("suite.no_usage_today") });
  const max = Math.max(...top.map(valueOf), 1e-9);
  return el(
    "div",
    { class: "suite-bars" },
    top.map((item) =>
      el(
        "div",
        { class: "suite-bar-row" },
        el("span", { class: "suite-bar-label mask-blur", text: labelOf(item), "data-tip": labelOf(item), "data-tip-overflow": "" }),
        el("span", { class: "suite-bar-track" }, el("i", { style: "width:" + Math.max(2, (valueOf(item) / max) * 100) + "%" })),
        el("span", { class: "suite-bar-value mono", text: format(item) })
      )
    )
  );
}

function suiteCard(title, action, ...body) {
  return el("div", { class: "card suite-card" }, el("div", { class: "suite-card-head" }, el("h2", { text: title }), action), ...body);
}

function suiteLink(text, sub) { return el("button", { type: "button", class: "link", text, onclick: () => suiteGo(sub) }); }

function suiteSub(id) {
  for (const g of SUITE_GROUPS) for (const s of g.subs) if (s.id === id) return s;
  return SUITE_GROUPS[1].subs[0];
}

function renderSuiteOverview() {
  if (currentRole !== "admin" || activeTab(false) !== "overview") return false;
  suiteLoadOverview(false);
  const target = $("suite-overview");
  const keys = resources.admin.keys.value;
  const stamp = [
    suiteOverview.readAt,
    suiteOverview.error,
    suiteClients.readAt,
    keys?.length,
    resources.admin.keys.readAt,
    document.documentElement.lang,
    suiteAliases.state,
    suiteForecast.readAt,
    suiteRanking.readAt
  ].join("|");
  if (suiteOverview.painted === stamp && target.childElementCount) return false;
  suiteOverview.painted = stamp;
  const { today, week, status } = suiteOverview;
  if (!today) {
    target.replaceChildren(
      el(
        "div",
        { class: "card" },
        el("div", {
          class: suiteOverview.error ? "notice err show" : "empty section-loading",
          text: suiteOverview.error || m("ui.loading")
        })
      )
    );
    return false;
  }
  const live = (keys || []).filter((key) => !key.deleted_at);
  const exhausted = live.filter((key) => key.blocked);
  const planned = live.filter((key) => !key.unlimited);
  const s = today.summary, w = week.summary;
  const client = suiteClients.value;
  const blockedToday = client?.state?.summary?.today_blocked;
  const stats = el(
    "div",
    { class: "suite-grid g4" },
    suiteStat(m("suite.today_cost"), usd(s.cost.total_usd), m("suite.last_7_days_value", { v0: usd(w.cost.total_usd) }), () =>
      suiteGo(suiteSub("analysis"))
    ),
    suiteStat(
      m("suite.today_requests_tokens"),
      int(s.requests),
      m("suite.tokens_success_value", { v0: tokens(s.total_tokens), v1: Number(s.success_rate || 0).toFixed(1) + "%" }),
      () => suiteGo(suiteSub("request-events"))
    ),
    suiteStat(
      "API Key",
      int(live.length),
      m("suite.keys_planned_exhausted_value", { v0: int(planned.length), v1: int(exhausted.length) }),
      () => suiteGo(suiteSub("keys"))
    ),
    suiteStat(
      m("suite.client_blocks_today"),
      client?.module?.active ? int(blockedToday || 0) : "—",
      client?.module?.active
        ? m("suite.client_requests_today_value", { v0: int(client.state.summary.today_requests || 0) })
        : client?.module?.reason || m("suite.module_not_running"),
      () => suiteGo(suiteSub("clients"))
    )
  );
  const keyName = (item) => {
    const view = (keys || []).find((key) => key.scope === item.key);
    return view ? apiKeyIdentity(view).primary : item.label || item.preview || item.key?.slice(0, 10) || "—";
  };
  const usage = el(
    "div",
    { class: "suite-grid g2" },
    suiteCard(
      m("suite.top_keys_today"),
      suiteLink(m("suite.view_analysis"), suiteSub("analysis")),
      suiteBars(
        [...(today.usage_distribution.api_keys || [])].sort((a, b) => b.cost_usd - a.cost_usd),
        (i) => i.cost_usd || i.requests / 1e6,
        keyName,
        (i) => usd(i.cost_usd) + DISPLAY_SEPARATOR + int(i.requests)
      )
    ),
    suiteCard(
      m("suite.top_models_today"),
      suiteLink(m("suite.view_events"), suiteSub("request-events")),
      suiteBars(
        [...(today.usage_distribution.models || [])].sort((a, b) => b.cost_usd - a.cost_usd),
        (i) => i.cost_usd || i.requests / 1e6,
        (i) => i.label || i.key,
        (i) => usd(i.cost_usd) + DISPLAY_SEPARATOR + tokens(i.total_tokens)
      )
    )
  );
  const tight = live.filter(
    (key) =>
      !key.unlimited && (key.blocked || (key.windows || []).some((win) => (win.dimensions || []).some((d) => Number(d.used_percent) >= 80)))
  )
    .slice(0, 6);
  const quota = suiteCard(
    m("suite.keys_near_limit"),
    suiteLink(m("suite.view_keys"), suiteSub("keys")),
    tight.length
      ? el(
          "div",
          { class: "suite-quota-list" },
          tight.map((view) =>
            el(
              "div",
              { class: "suite-quota-item" },
              el(
                "div",
                { class: "suite-quota-name mask-blur" },
                apiKeyIdentity(view).primary,
                view.blocked ? el("span", { class: "tag bad", text: m("ui.quota_exhausted") }) : null
              ),
              renderQuotaWindows(view)
            )
          )
        )
      : el("div", { class: "empty", text: m("suite.no_keys_near_limit") })
  );
  const info = status
    ? suiteCard(
        m("suite.system_status"),
        suiteLink(m("suite.nav_modules"), suiteSub("modules")),
        el(
          "div",
          { class: "suite-kv" },
          el("span", { text: m("suite.version") }),
          el("b", { class: "mono", text: status.version }),
          el("span", { text: m("suite.billing_mode") }),
          el("span", {
            class: "tag " + (status.watch_only ? "bad" : "ok"),
            text: status.watch_only ? m("suite.watch_only") : m("suite.status_active")
          }),
          el("span", { text: m("suite.clients_title") }),
          el("span", {
            class: "tag " + (client?.module?.active ? "ok" : "warn"),
            text: client?.module?.active ? m("suite.status_active") : m("suite.status_paused")
          }),
          el("span", { text: m("suite.other_plugins") }),
          el("span", { text: (status.coexisting || []).map((c) => c.plugin_id).join(", ") || "—" })
        )
      )
    : null;
  target.replaceChildren(stats, suiteOverviewInsights(), usage, el("div", { class: "suite-grid g2" }, quota, info));
  return false;
}

/* ---------- Settings → modules ---------- */
function suiteModuleSwitch(mod) {
  const tag = el("span", {
    class: "tag " + (!mod.available ? "plain" : mod.mode === "off" ? "plain" : mod.mode === "observe" ? "warn" : "ok"),
    text: !mod.available ? m("suite.coming_soon") : mod.mode
  });
  const source = el("div", {
    class: "small muted",
    "data-module-source": mod.source || "default",
    text: m("suite.module_source_" + (mod.source || "default"))
  });
  if (!mod.available || mod.locked || (mod.modes || []).length < 2) return el("div", {}, tag, source);
  const select = el(
    "select",
    { "data-module-switch": mod.id, onchange: () => suiteSwitchModule(mod, select) },
    mod.modes.map((mode) => {
      const option = el("option", { value: mode, text: mode });
      option.selected = mode === mod.mode;
      return option;
    })
  );
  return el("div", {}, select, source);
}

async function suiteSwitchModule(mod, select) {
  const mode = select.value;
  const text =
    mod.id === "candy_test" && mode === "on"
      ? m("suite.module_candy_confirm")
      : mod.id === "core" && mode === "observe"
        ? m("suite.module_core_confirm")
        : m("suite.module_switch_confirm", { v0: mod.name, v1: mod.mode, v2: mode });
  let done = false;
  await openActionDialog(
    m("suite.nav_modules"),
    text,
    undefined,
    async () => {
      await plugin("PUT", "/suite/modules", { id: mod.id, mode, confirm: true });
      done = true;
      notify(m("suite.module_switched"));
    },
    m("suite.save")
  );
  if (!done) select.value = mod.mode;
  suiteModules.value = null;
  suiteModules.painted = "";
  renderSuiteModules();
}

const suiteModules = { value: null, task: null, painted: "" };
function renderSuiteModules() {
  if (currentRole !== "admin" || activeTab(false) !== "modules") return false;
  const target = $("suite-modules");
  if (!suiteModules.value && !suiteModules.task) {
    suiteModules.task = plugin("GET", "/suite/status").then((value) => { suiteModules.value = value; }).catch(fail).finally(() => {
      suiteModules.task = null;
      renderSuiteModules();
    });
  }
  if (suiteModulesView() === "config") {
    const stamp = ["config", !!suiteAlertCfg.value, suiteAlertCfg.busy, suiteAlertCfg.error, suiteAlertCfg.saved, !!suiteRetention.value, suiteRetention.busy, suiteRetention.error, JSON.stringify(suiteRetention.value?.counts || {}), document.documentElement.lang].join("|");
    if (suiteModules.painted === stamp && target.childElementCount) return false;
    suiteModules.painted = stamp;
    renderSuiteConfig(target);
    return false;
  }
  const st = suiteModules.value;
  const stamp = ["modules", !!st, document.documentElement.lang].join("|");
  if (suiteModules.painted === stamp && target.childElementCount) return false;
  suiteModules.painted = stamp;
  if (!st) {
    target.replaceChildren(el("div", { class: "card" }, el("div", { class: "empty section-loading", text: m("ui.loading") })));
    return false;
  }
  const table = el("div", {});
  renderCollection(table, st.modules, {
    headers: [m("suite.module"), m("suite.mode"), m("suite.allowed_modes"), m("suite.note")],
    render: (mod) =>
      el(
        "tr",
        {},
        el("td", {}, el("b", { text: mod.name }), el("div", { class: "small muted mono", text: mod.id })),
        el("td", {}, suiteModuleSwitch(mod)),
        el("td", { class: "small mono" }, (mod.modes || []).join(" / ")),
        el("td", { class: "small" }, mod.description)
      )
  });
  const imp = st.import;
  target.replaceChildren(
    suiteCard(
      m("suite.nav_modules"),
      el("span", { class: "small muted mono", text: st.version }),
      el("div", { class: "small muted", text: m("suite.modules_hint") }),
      table
    ),
    suiteCard(
      m("suite.about"),
      null,
      el(
        "div",
        { class: "suite-kv", id: "suite-about" },
        el("span", { text: m("suite.about_version") }),
        el("span", { class: "mono", text: "CPA Suite " + st.version }),
        el("span", { text: m("suite.about_upstream") }),
        el("span", { class: "mono small", text: "cpa-key-billing " + st.upstream_base }),
        el("span", { text: m("suite.billing_import") }),
        el("span", {
          class: "small",
          text: imp
            ? imp.source + DISPLAY_SEPARATOR + when(imp.imported_at) + DISPLAY_SEPARATOR +
              ["api_keys", "plans", "routes"].map((k) => k + " " + int((imp.rows || {})[k] || 0)).join(", ")
            : "—"
        }),
        el("span", { text: m("suite.about_import_note") }),
        el("span", { class: "small", text: st.import_note || "—" }),
        el("span", { text: m("suite.about_cg_import") }),
        el("span", {
          class: "small",
          text: st.client_guard?.import
            ? (st.client_guard.import.source_dir || "") + DISPLAY_SEPARATOR + int(st.client_guard.import.key_rules || 0) + " key rules"
            : "—"
        })
      )
    ),
    suiteCard(
      m("suite.system_status"),
      null,
      el(
        "div",
        { class: "suite-kv" },
        el("span", { text: m("suite.billing_mode") }),
        el("span", {
          class: "tag " + (st.watch_only ? "bad" : "ok"),
          text: st.watch_only ? m("suite.watch_only") : m("suite.status_active")
        }),
        el("span", { text: m("suite.state_file") }),
        el("span", { class: "mono small", text: st.state_file }),
        el("span", { text: m("suite.ext_file") }),
        el("span", { class: "mono small", text: st.ext_file }),
        el("span", { text: m("suite.plugins_dir") }),
        el("span", { class: "mono small", text: st.plugins_dir }),
        el("span", { text: m("suite.billing_import") }),
        el("span", { class: "small", text: imp ? imp.source + DISPLAY_SEPARATOR + when(imp.imported_at) : "—" }),
        el("span", { text: m("suite.other_plugins") }),
        el("span", { class: "small", text: (st.coexisting || []).map((c) => c.plugin_id).join(", ") || "—" }),
        el("span", { text: m("suite.conflicts") }),
        el("span", { class: "small", text: (st.conflicts || []).map((c) => c.plugin_id + ": " + c.reason).join("; ") || "—" })
      )
    )
  );
  return false;
}
