/* CPA Suite: client module UI (port of client-guard 0.1.2) and the plain-http
 * login fallback. Kept in one file so upstream files only carry hook lines. */

/* ---------- Login persistence without WebCrypto (plain http) ---------- */
const SUITE_PAD_KEY = "cpa-suite.credential-pad.v1";
const SUITE_REMEMBER_PREFIX = "cpa-suite.credential.v1.";
const SUITE_OBF_PREFIX = "cpa-suite-obf:v1:";

// Same condition upstream uses for its encrypted vault: https keeps that path.
function suiteSecureStorage() { return !!(window.indexedDB && window.crypto?.subtle); }

function suitePad() {
  let pad = "";
  try { pad = localStorage.getItem(SUITE_PAD_KEY) || ""; } catch (_) {}
  if (!/^[0-9a-f]{64}$/.test(pad)) {
    pad = Array.from(crypto.getRandomValues(new Uint8Array(32)), (b) => b.toString(16).padStart(2, "0")).join("");
    try { localStorage.setItem(SUITE_PAD_KEY, pad); } catch (_) {}
  }
  return Uint8Array.from(pad.match(/../g), (h) => parseInt(h, 16));
}

// Obfuscation only (XOR with a random per-browser pad stored next to it):
// it keeps the key out of plain sight, it is not encryption.
function suiteObfuscate(text) {
  const pad = suitePad(), bytes = new TextEncoder().encode(text);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i] ^ pad[i % pad.length]);
  return SUITE_OBF_PREFIX + btoa(binary);
}

function suiteReveal(value) {
  if (typeof value !== "string" || !value.startsWith(SUITE_OBF_PREFIX)) return "";
  try {
    const pad = suitePad(), binary = atob(value.slice(SUITE_OBF_PREFIX.length));
    const bytes = Uint8Array.from(binary, (c, i) => c.charCodeAt(0) ^ pad[i % pad.length]);
    return new TextDecoder().decode(bytes).trim();
  } catch (_) { return ""; }
}

async function suiteSaveCredential(role, credential, remember) {
  const value = suiteObfuscate(credential);
  sessionStorage.setItem(SESSION_CREDENTIAL_PREFIX + role, value);
  try {
    if (remember) localStorage.setItem(SUITE_REMEMBER_PREFIX + role, value); else localStorage.removeItem(SUITE_REMEMBER_PREFIX + role);
  } catch (_) { if (remember) throw new UIError(m("suite.login_storage_unavailable")); }
}

async function suiteRecalledCredential(role, includeSession = false) {
  try {
    const session = includeSession ? sessionStorage.getItem(SESSION_CREDENTIAL_PREFIX + role) : null;
    return suiteReveal(session !== null ? session : localStorage.getItem(SUITE_REMEMBER_PREFIX + role));
  } catch (_) { return ""; }
}

function suiteDeleteCredential(role) { try { localStorage.removeItem(SUITE_REMEMBER_PREFIX + role); } catch (_) {} }

(function suiteLoginNote() {
  if (suiteSecureStorage()) return;
  const row = document.querySelector("#gate-form .remember-row");
  if (!row) return;
  const note = el("div", { id: "suite-login-note", class: "small muted", text: m("suite.login_http_note") });
  note.style.margin = "-4px 0 10px";
  row.after(note);
})();

/* ---------- Client module ---------- */
const suiteClients = { value: null, error: "", readAt: 0, task: null, painted: null, shown: false };
const SUITE_RESULT = {
  allowed: ["ok", () => m("suite.result_allowed")],
  unchecked: ["plain", () => m("suite.result_unchecked")],
  "not-in-list": ["warn", () => m("suite.result_not_in_list")],
  blocked: ["bad", () => m("suite.result_blocked")]
};
const SUITE_MODE = {
  inherit: () => m("suite.mode_inherit"),
  off: () => m("suite.mode_off"),
  "log-only": () => m("suite.mode_log_only"),
  enforce: () => m("suite.mode_enforce")
};

function suiteLoadClients(force = false) {
  if (suiteClients.task) return suiteClients.task;
  if (!force && suiteClients.value && Date.now() - suiteClients.readAt < 30000) return Promise.resolve();
  const generation = sessionGeneration;
  suiteClients.task = plugin("GET", "/suite/clients").then((value) => {
    if (generation !== sessionGeneration) return;
    Object.assign(suiteClients, { value, error: "", readAt: Date.now() });
  })
    .catch((error) => {
      if (error instanceof AuthError) throw error;
      if (!(error instanceof StaleRequestError)) suiteClients.error = error.message || String(error);
    })
    .finally(() => {
      suiteClients.task = null;
      suiteFillKeyCells();
      if (currentRole === "admin" && activeTab(false) === "clients") renderSuiteClients();
    });
  return suiteClients.task;
}

function suiteRules() { return suiteClients.value?.state?.rules || null; }
function suiteActive() { return !!suiteClients.value?.module?.active; }

function suiteKeyName(scope) {
  if (scope === "anonymous") return m("suite.anonymous");
  const view = (resources.admin.keys.value || []).find((key) => key.scope === scope);
  const label = suiteRules()?.keys?.[scope]?.label;
  return view ? apiKeyIdentity(view).primary : label || scope.slice(0, 10) + "…";
}

function suitePatterns(list) { return (list || []).length ? list.join(", ") : m("suite.empty_allow"); }

// Mirrors client-guard effective(): inherit = the whole default rule; an own
// rule with an empty list keeps its mode and uses the default list.
function suiteEffective(scope) {
  const rules = suiteRules();
  if (!rules) return null;
  const own = rules.keys?.[scope];
  if (!own || !own.mode || own.mode === "inherit")
    return { mode: rules.default.mode, allow: rules.default.allow || [], inherited: true, listInherited: true };
  if (!(own.allow || []).length) return { mode: own.mode, allow: rules.default.allow || [], inherited: false, listInherited: true };
  return { mode: own.mode, allow: own.allow, inherited: false, listInherited: false };
}

function suiteRuleSummary(scope) {
  const e = suiteEffective(scope);
  if (!e) return "";
  const mode = SUITE_MODE[e.mode]?.() || e.mode;
  if (e.mode === "off") return mode + (e.inherited ? m("suite.inherited_suffix") : "");
  return (
    mode + (e.inherited ? m("suite.inherited_suffix") : "") + DISPLAY_SEPARATOR + suitePatterns(e.allow) +
    (!e.inherited && e.listInherited ? m("suite.default_list_suffix") : "")
  );
}

/* Key list hooks (keys.js) */
function suiteKeyHeaders() { return [m("suite.allowed_clients")]; }
function suiteKeyCells(view) {
  const summary = el("span", { class: "small suite-client-summary", "data-scope": view.scope });
  const edit = el("button", {
    class: "link suite-client-edit",
    type: "button",
    text: m("suite.edit"),
    disabled: !!view.deleted_at,
    onclick: () => guard(() => suiteOpenRuleEditor(view.scope))
  });
  const cell = el(
    "td",
    { class: "suite-client-cell" },
    el("div", { class: "row", style: "gap:6px;flex-wrap:nowrap;align-items:baseline" }, summary, edit)
  );
  suiteFillKeyCell(summary, edit);
  suiteLoadClients(false).catch(fail);
  return [cell];
}
function suiteFillKeyCell(summary, edit) {
  const module = suiteClients.value?.module;
  if (!module) {
    summary.textContent = suiteClients.error ? "—" : m("ui.loading");
    if (suiteClients.error) setTextAttribute(summary, "data-tip", suiteClients.error);
    edit.disabled = true;
    return;
  }
  if (!module.active) {
    setText(summary, module.paused ? m("suite.module_paused_short") : m("suite.mode_off"));
    setTextAttribute(summary, "data-tip", module.reason || module.error || "");
    edit.disabled = true;
    return;
  }
  setText(summary, suiteRuleSummary(summary.dataset.scope));
  edit.disabled = edit.closest("tr")?.classList.contains("key-deleted") || false;
}
function suiteFillKeyCells() {
  for (const summary of document.querySelectorAll(".suite-client-summary")) {
    const edit = summary.parentElement.querySelector(".suite-client-edit");
    if (edit) suiteFillKeyCell(summary, edit);
  }
}

/* Rule editor (one key or "__default"), reusing the action dialog. */
async function suiteOpenRuleEditor(target) {
  await suiteLoadClients(true);
  if (!suiteActive()) throw new UIError(suiteClients.value?.module?.reason || m("suite.module_not_running"));
  const rules = suiteRules();
  const isDefault = target === "__default";
  const current = isDefault ? rules.default : rules.keys?.[target] || { mode: "inherit", allow: [] };
  const mode = el(
    "select",
    { "aria-label": m("suite.mode") },
    (isDefault ? ["off", "log-only", "enforce"] : ["inherit", "off", "log-only", "enforce"]).map((value) =>
      el("option", { value, selected: (current.mode || "inherit") === value, text: SUITE_MODE[value]() })
    )
  );
  const allow = el("textarea", {
    class: "mono",
    rows: "5",
    style: "width:100%;box-sizing:border-box",
    placeholder: m("suite.allow_placeholder")
  });
  allow.value = (current.allow || []).join("\n");
  const label = isDefault
    ? null
    : el("input", {
        type: "text",
        value: current.label || "",
        placeholder: m("suite.label_placeholder"),
        style: "width:100%;box-sizing:border-box"
      });
  const append = (patterns) => {
    const lines = allow.value.split("\n").map((s) => s.trim()).filter(Boolean);
    for (const p of patterns) if (!lines.includes(p)) lines.push(p);
    allow.value = lines.join("\n");
  };
  const presets = suiteClients.value.state.presets || [];
  const categories = [...new Set(presets.map((p) => p.category))];
  const seen = isDefault ? [] : suiteClients.value.state.seen?.[target] || [];
  const content = el(
    "div",
    { class: "suite-rule-editor", style: "display:grid;gap:10px;text-align:left" },
    el("div", {
      class: "small muted",
      text: isDefault ? m("suite.default_rule_for_all") : m("suite.rule_for_key", { v0: suiteKeyName(target) })
    }),
    el("label", { class: "field" }, el("span", { text: m("suite.mode") }), el("span", { class: "native-select" }, mode)),
    el("div", { class: "small muted", text: isDefault ? m("suite.default_mode_hint") : m("suite.key_mode_hint") }),
    el("label", { class: "field" }, el("span", { text: isDefault ? m("suite.default_allow_list") : m("suite.key_allow_list") }), allow),
    el("div", { class: "small muted", text: m("suite.pattern_syntax") }),
    label ? el("label", { class: "field" }, el("span", { text: m("suite.label") }), label) : null,
    seen.length
      ? el(
          "div",
          {},
          el("div", { class: "small", text: m("suite.seen_clients_of_key") }),
          seen.slice(0, 8).map((c) =>
            el(
              "div",
              { class: "row small", style: "gap:6px" },
              el("span", { class: "tag " + (c.blocked ? "bad" : "plain"), text: c.client || "?" }),
              el("span", { class: "muted", text: m("suite.count_value", { v0: int(c.count), v1: int(c.blocked) }) }),
              c.suggest
                ? el("button", {
                    class: "link",
                    type: "button",
                    text: m("suite.add_pattern_value", { v0: c.suggest }),
                    onclick: () => append([c.suggest])
                  })
                : null
            )
          )
        )
      : null,
    el(
      "details",
      {},
      el("summary", { class: "small", text: m("suite.presets") }),
      categories.map((category) =>
        el(
          "div",
          { class: "row small", style: "gap:6px;margin:4px 0" },
          el("b", { text: category }),
          presets.filter((p) => p.category === category).map((p) =>
            el("button", {
              type: "button",
              class: "link",
              text: p.name,
              "data-tip": (p.warning ? p.warning + " " : "") + p.note,
              disabled: !(p.patterns || []).length || !!p.warning,
              onclick: () => append(p.patterns)
            })
          )
        )
      )
    )
  );
  await openActionDialog(
    m("suite.edit_client_rule"),
    content,
    undefined,
    async () => {
      const values = allow.value.split(/\n/).map((s) => s.trim()).filter(Boolean);
      await suiteSaveRules((next) => {
        if (isDefault) next.default = { ...next.default, mode: mode.value, allow: values };
        else {
          const rule = { mode: mode.value, allow: values };
          if (label.value.trim()) rule.label = label.value.trim();
          if (rule.mode === "inherit" && !values.length && !rule.label) delete next.keys[target]; else next.keys[target] = rule;
        }
      });
      notify(m("suite.rules_saved"));
    },
    m("suite.save")
  );
}

async function suiteSaveRules(change) {
  await suiteLoadClients(true);
  const rules = JSON.parse(JSON.stringify(suiteRules() || { default: { mode: "log-only", allow: [] }, keys: {} }));
  rules.keys = rules.keys || {};
  change(rules);
  const saved = await plugin("PUT", "/suite/clients/rules", rules);
  if (saved?.warning) notify(saved.warning, "err");
  await suiteLoadClients(true);
}

// One-click add from a recorded request (same choices as client-guard 0.1.2).
async function suiteQuickAdd(scope, pattern, toDefault) {
  const rules = suiteRules();
  const name = toDefault ? m("suite.default_rule") : suiteKeyName(scope);
  const own = rules?.keys?.[scope];
  const inherits = !toDefault && (!own || !own.mode || own.mode === "inherit");
  await openActionDialog(
    m("suite.quick_add_title"),
    m(inherits ? "suite.quick_add_inherit_confirm" : "suite.quick_add_confirm", { v0: pattern, v1: name }),
    undefined,
    async () => {
      await suiteSaveRules((next) => {
        if (toDefault) {
          next.default.allow = [...new Set([...(next.default.allow || []), pattern])];
          return;
        }
        const rule = next.keys[scope] || { mode: "inherit", allow: [] };
        if (!rule.mode || rule.mode === "inherit") rule.mode = next.default.mode;
        // An empty own list means "use the default list"; keep that list when adding.
        const base = (rule.allow || []).length ? rule.allow : next.default.allow || [];
        rule.allow = [...new Set([...base, pattern])];
        next.keys[scope] = rule;
      });
      notify(m("suite.rules_saved"));
    },
    m("suite.add")
  );
}

// Replace outdated preset patterns in saved rules (user-confirmed; nothing is rewritten automatically).
async function suiteUpdateOutdated(outdated) {
  await openActionDialog(
    m("suite.outdated_title"),
    m("suite.outdated_confirm", { v0: outdated.length }),
    undefined,
    async () => {
      await suiteSaveRules((next) => {
        const fix = (rule) => {
          if (!rule?.allow) return;
          rule.allow = [...new Set(rule.allow.map((p) => outdated.find((o) => o.pattern === p.trim())?.replacement || p))];
        };
        fix(next.default);
        Object.values(next.keys || {}).forEach(fix);
      });
      notify(m("suite.outdated_done"));
    },
    m("suite.outdated_update")
  );
}

/* Clients page */
function renderSuiteClients() {
  if (currentRole !== "admin" || activeTab(false) !== "clients") {
    suiteClients.shown = false;
    return false;
  }
  const target = $("suite-clients");
  const keysRead = resources.admin.keys.readAt || 0;
  const entering = !suiteClients.shown;
  suiteClients.shown = true;
  // Fresh data on entering the page and after the page refresh reloads the keys.
  if (!suiteClients.task && (entering || !suiteClients.value || suiteClients.readAt + 1000 < keysRead)) suiteLoadClients(true).catch(fail);
  const stamp = [suiteClients.readAt, suiteClients.error, !!suiteClients.task, keysRead, locale()].join("|");
  if (suiteClients.painted === stamp) return false;
  suiteClients.painted = stamp;
  if (!suiteClients.value) {
    target.replaceChildren(
      el(
        "div",
        { class: "card" },
        el("div", { class: suiteClients.error ? "notice err show" : "empty section-loading", text: suiteClients.error || m("ui.loading") })
      )
    );
    return false;
  }
  const { module, state } = suiteClients.value;
  const toolbar = el(
    "div",
    { class: "row section-actions" },
    el("button", { type: "button", text: m("ui.refresh"), onclick: () => guard(() => suiteLoadClients(true)) }),
    el("button", {
      type: "button",
      text: m("suite.clear_stats"),
      disabled: !module.active,
      onclick: () =>
        openActionDialog(m("suite.clear_stats"), m("suite.clear_stats_confirm"), undefined, async () => {
          await plugin("POST", "/suite/clients/clear-stats", {});
          await suiteLoadClients(true);
          notify(m("suite.stats_cleared"));
        })
    })
  );
  const statusTag = module.active
    ? el("span", { class: "tag ok", text: module.mode === "observe" ? m("suite.status_observe") : m("suite.status_active") })
    : el("span", {
        class: "tag " + (module.paused ? "warn" : "plain"),
        text: module.paused ? m("suite.status_paused") : m("suite.mode_off")
      });
  const imp = module.import;
  const status = el(
    "div",
    { class: "card" },
    el("div", { class: "row spread section-toolbar" }, el("h2", { text: m("suite.clients_title") }), toolbar),
    el(
      "div",
      { class: "row", style: "gap:8px;flex-wrap:wrap" },
      statusTag,
      el("span", { class: "small muted", text: m("suite.ported_from_value", { v0: module.source }) })
    ),
    module.reason ? el("div", { class: "notice warn show", text: module.reason }) : null,
    module.error ? el("div", { class: "notice err show", text: module.error }) : null,
    imp
      ? el("div", {
          class: "small muted",
          text: m("suite.import_record_value", {
            v0: imp.source_dir,
            v1: new Date(imp.imported_at).toLocaleString(locale()),
            v2: int(imp.key_rules),
            v3: int(imp.recent_count),
            v4: imp.source_unchanged ? m("suite.source_unchanged") : m("suite.source_changed")
          })
        })
      : null,
    state
      ? el(
          "div",
          { class: "row", style: "gap:16px;flex-wrap:wrap;margin-top:8px" },
          [
            [m("suite.today_requests"), state.summary.today_requests],
            [m("suite.today_blocked"), state.summary.today_blocked],
            [m("suite.today_not_in_list"), state.summary.today_not_in_list],
            [m("suite.total_requests"), state.summary.total_requests],
            [m("suite.total_blocked"), state.summary.total_blocked],
            [m("suite.keys_seen"), state.summary.keys_seen],
            [m("suite.clients_seen"), state.summary.clients_seen],
            [m("suite.enforced_keys"), state.summary.enforced_keys]
          ].map(([name, value]) => el("div", {}, el("div", { class: "small muted", text: name }), el("b", { text: int(value || 0) })))
        )
      : null
  );
  if (!state) {
    target.replaceChildren(status);
    return false;
  }
  const def = el(
    "div",
    { class: "card" },
    el(
      "div",
      { class: "row spread settings-section-header" },
      el("h2", { text: m("suite.default_rule") }),
      el("button", {
        type: "button",
        text: m("suite.edit"),
        disabled: !module.active,
        onclick: () => guard(() => suiteOpenRuleEditor("__default"))
      })
    ),
    el("div", {
      text:
        (SUITE_MODE[state.rules.default.mode]?.() || state.rules.default.mode) + DISPLAY_SEPARATOR +
        suitePatterns(state.rules.default.allow)
    }),
    el("div", { class: "small muted", text: m("suite.default_mode_hint") })
  );
  const outdated = state.outdated_patterns || [];
  const outdatedCard = outdated.length
    ? el(
        "div",
        { class: "card suite-warn", "data-suite-outdated": String(outdated.length) },
        el(
          "div",
          { class: "row spread settings-section-header" },
          el("h2", { text: m("suite.outdated_title") }),
          el("button", {
            type: "button",
            text: m("suite.outdated_update"),
            disabled: !module.active,
            "data-suite-outdated-update": "1",
            onclick: () => guard(() => suiteUpdateOutdated(outdated))
          })
        ),
        el("div", { class: "small", text: m("suite.outdated_hint") }),
        el(
          "ul",
          { class: "small" },
          outdated.map((o) =>
            el(
              "li",
              {},
              (o.scope === "__default" ? m("suite.default_rule") : suiteKeyName(o.scope)) + "：",
              el("code", { text: o.pattern }),
              " → ",
              el("code", { text: o.replacement })
            )
          )
        )
      )
    : null;
  const mismatches = state.family_mismatches || [];
  const mismatchCard = mismatches.length
    ? el(
        "div",
        { class: "card suite-warn", "data-suite-mismatch": String(mismatches.length) },
        el("div", { class: "row spread settings-section-header" }, el("h2", { text: m("suite.mismatch_title") })),
        el("div", { class: "small", text: m("suite.mismatch_hint") }),
        el(
          "ul",
          { class: "small" },
          mismatches.map((x) =>
            el(
              "li",
              {},
              suiteKeyName(x.scope) + "：" + x.client + "（" + x.preset + "，" + int(x.blocked) + " × 403）",
              el("div", { class: "mono small muted", text: x.sample_ua }),
              el("button", {
                type: "button",
                class: "btn small",
                disabled: !module.active,
                "data-suite-mismatch-add": x.scope,
                text: m("suite.mismatch_add") + " " + x.suggest,
                onclick: () => guard(() => suiteQuickAdd(x.scope, x.suggest, false))
              })
            )
          )
        )
      )
    : null;
  const recentBody = el("div", {});
  renderCollection(recentBody, (state.recent || []).slice(0, 300), {
    headers: [m("suite.time"), "API Key", m("suite.client"), "User-Agent", m("suite.result"), m("suite.matched"), m("ui.actions")],
    render: (e) => {
      const [cls, label] = SUITE_RESULT[e.result] || ["plain", () => e.result];
      return el(
        "tr",
        {},
        el("td", { class: "small" }, new Date(e.time).toLocaleString(locale())),
        el("td", {}, suiteKeyName(e.scope)),
        el("td", {}, e.client || "—", e.preset ? el("span", { class: "small muted" }, " (" + e.preset + ")") : null),
        el(
          "td",
          {
            class: "small mono",
            style: "max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap",
            "data-tip": e.ua || ""
          },
          e.ua || "—"
        ),
        el("td", {}, el("span", { class: "tag " + cls, text: label() })),
        el("td", { class: "small mono" }, e.matched || ""),
        el(
          "td",
          { class: "small" },
          e.suggest && module.active
            ? [
                el("button", {
                  class: "link",
                  type: "button",
                  text: m("suite.add_to_key"),
                  "data-tip": e.suggest,
                  onclick: () => guard(() => suiteQuickAdd(e.scope, e.suggest, false))
                }),
                " ",
                el("button", {
                  class: "link",
                  type: "button",
                  text: m("suite.add_to_default"),
                  "data-tip": e.suggest,
                  onclick: () => guard(() => suiteQuickAdd(e.scope, e.suggest, true))
                })
              ]
            : ""
        )
      );
    }
  });
  const recent = el(
    "div",
    { class: "card" },
    el(
      "div",
      { class: "row spread settings-section-header" },
      el("h2", { text: m("suite.recent_clients") }),
      el("span", {
        class: "small muted",
        text: m("suite.recent_count_value", { v0: int((state.recent || []).length), v1: int(state.config.recent_limit) })
      })
    ),
    recentBody
  );
  const presetBody = el("div", {});
  renderCollection(presetBody, state.presets || [], {
    headers: [m("suite.category"), m("suite.client"), m("suite.patterns"), m("suite.confidence"), m("suite.note")],
    render: (p) =>
      el(
        "tr",
        {},
        el("td", {}, p.category),
        el("td", {}, p.name),
        el("td", { class: "small mono" }, (p.patterns || []).join(", ") || "—"),
        el("td", {}, el("span", { class: "tag " + (p.confidence === "guess" ? "warn" : "plain"), text: p.confidence })),
        el("td", { class: "small" }, p.warning ? el("b", {}, p.warning + " ") : null, p.note)
      )
  });
  const presetsCard = el(
    "div",
    { class: "card" },
    el("div", { class: "row spread settings-section-header" }, el("h2", { text: m("suite.presets") })),
    presetBody
  );
  target.replaceChildren(...[status, outdatedCard, mismatchCard, def, recent, presetsCard].filter(Boolean));
  return false;
}
