function renderAccountProfile() {
  const profile = resources.account.profile.value;
  const tracked = profile?.tracked === true;
  $("account-untracked").classList.toggle("hidden", !profile || tracked);
  $("account-content").classList.toggle("hidden", !tracked);
  if (tracked) $("account-identity").replaceChildren(displayText(apiKeyIdentity(profile.identity || {}).text));
  else closeSharedTimePopover(true);
  return tracked;
}

function renderAccountSubscription() {
  const data = resources.account.subscription;
  const subscription = data.value?.subscription || {};
  const concurrency = data.value?.concurrency || {};
  setText(
    $("account-concurrency"),
    !data.value ? "—" : int(concurrency.current) + " / " + (Number(concurrency.limit || 0) > 0 ? int(concurrency.limit) : "∞")
  );
  setText(
    $("account-plan-name"),
    !data.value
      ? m("ui.subscription_quota")
      : displayLabel(subscription.name || (subscription.unlimited ? m("ui.no_subscription_plan") : m("ui.subscription_quota")))
  );
  $("account-plan-status").className = "tag " + (subscription.blocked ? "bad" : "plain");
  setText(
    $("account-plan-status"),
    !data.value ? "" : subscription.unlimited ? m("ui.unlimited") : subscription.blocked ? m("ui.quota_exhausted") : m("ui.active")
  );
  const target = $("account-windows");
  if (renderDataPending(target, [data], m("ui.loading_subscription_status"))) return;
  target.replaceChildren(renderQuotaWindows(subscription, true));
  renderAccountUsage();
}

// "My remaining quota" from /usage (own plan + shared upstream
// accounts this key can be routed to), same data cc-switch shows.
let accountUsageState = { at: 0, value: null, error: "" };
async function renderAccountUsage(force = false) {
  const target = $("account-usage");
  if (!target) return;
  if (force || Date.now() - accountUsageState.at > 30000) {
    accountUsageState.at = Date.now();
    try {
      accountUsageState.value = await accountAPI("/usage");
      accountUsageState.error = "";
    } catch (err) { accountUsageState.error = err.message || String(err); }
  }
  const data = accountUsageState.value;
  if (!data) {
    target.replaceChildren(
      el("span", { class: accountUsageState.error ? "err" : "muted", text: accountUsageState.error || m("ui.loading") })
    );
    return;
  }
  const rows = (data.data || []).map((r) => {
    const total = Number(r.total);
    const remaining = Number(r.remaining);
    const unlimited = total === -1;
    const pct = unlimited ? 100 : total > 0 ? Math.max(0, Math.min(100, (remaining / total) * 100)) : 0;
    const level = pct >= 70 ? "high" : pct >= 30 ? "medium" : "low";
    const fmt = (v) => (r.unit === "USD" ? "$" + Number(v).toFixed(2) : r.unit === "%" ? Math.round(v) + "%" : int(v));
    return el(
      "div",
      { class: "quota-window quota-single" + (r.kind === "upstream" ? " usage-upstream" : "") },
      el(
        "div",
        { class: "quota-metric" },
        el(
          "div",
          { class: "quota-heading" },
          el("b", { text: r.planName || "—", "data-tip": r.planName || "" }),
          el("span", {
            class: "mono" + (r.isValid === false ? " blocked" : ""),
            text: unlimited ? m("ui.unlimited") : r.unit === "%" ? fmt(remaining) : fmt(remaining) + DISPLAY_SLASH + fmt(total)
          }),
          el("span", { class: "muted", text: r.isValid === false ? r.invalidMessage || r.extra || "" : r.extra || "" })
        ),
        unlimited
          ? null
          : el(
              "div",
              { class: "bar", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(Math.round(pct)) },
              el("i", { class: "quota-fill " + level, style: "width:" + pct + "%" })
            )
      )
    );
  });
  target.replaceChildren(...rows);
  setText($("account-usage-note"), data.shared_quota ? m("ui.my_usage_note_shared") : m("ui.my_usage_note_own"));
}

function ccSwitchUsageScript(origin) {
  return [
    "({",
    "  request: {",
    '    url: "' + origin + ACCOUNT_BASE + '/usage",',
    '    method: "GET",',
    '    headers: { "Authorization": "Bearer {{apiKey}}" }',
    "  },",
    "  extractor: function (r) {",
    "    return r && r.data && r.data.length ? r.data : { isValid: false, invalidMessage: (r && r.error && r.error.message) || " +
      JSON.stringify(m("ui.ccswitch_query_failed")) +
      " };",
    "  }",
    "})"
  ].join("\n");
}

function renderAccountModels() {
  const data = resources.account;
  const target = $("account-models");
  if (renderDataPending(target, [data.models, data.catalog, data.routing], m("ui.loading_model_prices"))) return;
  const access = data.routing.value;
  const directory = data.models.value;
  const modelScope = new Set(access.models.map((model) => model.toLowerCase()));
  const deniedModels = new Set((access.denied_models || []).map((model) => model.toLowerCase()));
  const models = (
    access.routing_valid === false
      ? []
      : !modelScope.size ? directory : directory.length ? directory.filter((model) => modelScope.has(model.toLowerCase())) : access.models
  )
    .filter((model) => !deniedModels.has(model.toLowerCase()))
    .slice()
    .sort(compareModelId);
  renderCollection(target, models, { attrs: { class: "display-contents" }, empty: m("ui.no_model_prices"), render: renderAccountModel });
}

function renderAccountRouting() {
  const data = resources.account.routing;
  const target = $("account-access");
  if (renderDataPending(target, [data], m("ui.loading_routing_permissions"))) return;
  target.replaceChildren(renderAccountAccess(data.value));
}

function renderAccountAccess(access) {
  const routingValid = access.routing_valid !== false;
  const accountCredentialLabel = (item, state) => {
    const name = window.billingI18n.serverMessage(item.name_message, item.name);
    return {
      text: item.provider_wide
        ? credentialProviderLabel(item)
        : item.status === "missing"
          ? name ? maskedLabelText(name) : m("ui.the_selected_upstream_credential_is_currently_unavailable")
          : maskedTailText(item.provider, name || m("ui.credential_unavailable")),
      tag: item.provider_wide ? m("ui.entire_category") : "",
      iconName: credentialIcon(item),
      state: item.denied ? "deny" : state
    };
  };
  const deniedModels = new Set((access.denied_models || []).map((model) => model.toLowerCase()));
  const modelItems = access.models.map((text) => ({
    text,
    state: deniedModels.has(text.toLowerCase()) ? "deny" : "allow",
    iconName: "astroid"
  }));
  if (!modelItems.length) modelItems.push({ text: routingValid ? m("ui.all_models") : m("ui.unknown"), iconName: "astroid" });
  for (const text of access.denied_models || [])
    if (!access.models.some((model) => model.toLowerCase() === text.toLowerCase()))
      modelItems.push({ text, state: "deny", iconName: "astroid" });
  const credentialItems = access.credentials.map((item) => accountCredentialLabel(item, "allow"));
  if (!credentialItems.length) credentialItems.push({ text: routingValid ? m("ui.all_credentials") : m("ui.unknown") });
  const credentialKey = (item) => JSON.stringify([item.source, item.provider, !!item.provider_wide, item.name]);
  const deniedCredentials = new Set(access.credentials.filter((item) => item.denied).map(credentialKey));
  for (const item of access.denied_credentials || []) {
    const key = credentialKey(item);
    if (deniedCredentials.has(key)) continue;
    deniedCredentials.add(key);
    credentialItems.push(accountCredentialLabel(item, "deny"));
  }
  const nodes = [
    el(
      "div",
      { class: "account-access-summary" },
      el(
        "div",
        { class: "inset-card" },
        el("div", { class: "account-access-scope-label", text: m("ui.model_access") }),
        settingsChipList(modelItems)
      ),
      el(
        "div",
        { class: "inset-card" },
        el("div", { class: "account-access-scope-label", text: m("ui.credential_access") }),
        settingsChipList(credentialItems)
      )
    )
  ];
  if (access.warnings?.length)
    nodes.push(
      el(
        "div",
        { class: "account-access-warnings" },
        access.warnings.map((warning, index) =>
          el("span", { class: "tag warn", text: window.billingI18n.serverMessage(access.warning_messages?.[index], warning) })
        )
      )
    );
  return el("div", { class: "display-contents" }, nodes);
}

function renderAccountModel(model) {
  const candidate = resources.account.catalog.value.prices.find((price) => price.model_id.toLowerCase() === model.toLowerCase());
  const price = candidate?.source === "none" ? null : candidate;
  const fields = [[m("ui.input"), "input_per_1m"], [m("ui.output"), "output_per_1m"]];
  const longContext = price?.long_context;
  for (const [label, field] of [[m("ui.cache_read"), "cache_read_per_1m"], [m("ui.cache_write"), "cache_write_per_1m"]]) {
    if (price?.[field] != null || longContext?.[field] != null) fields.push([label, field]);
  }
  const threshold = longContext ? ">" + tokenThreshold(longContext.threshold_input_tokens) : "";
  const priceRows = price
    ? fields.map(([label, field]) => ({
        label,
        base: price[field] == null ? null : Number(price[field]),
        long: longContext?.[field] == null ? null : Number(longContext[field])
      }))
    : [];
  const priceRow = (item) =>
    el(
      "div",
      { class: "account-model-price-row" + (longContext ? " tiered" : "") },
      el("span", { class: "account-model-price-label" }, item.label),
      el("strong", { class: "mono account-model-base-price" }, item.base == null ? "—" : usd(item.base)),
      longContext
        ? el(
            "span",
            { class: "account-model-long-price" },
            item.long != null
              ? [el("span", { class: "account-model-threshold" }, threshold), el("strong", { class: "mono" }, usd(item.long))]
              : el("span", { class: "muted" }, "—")
          )
        : null
    );
  return el(
    "div",
    { class: "account-model-card" },
    el("b", { class: "mono account-model-name", "data-tip": model, "data-tip-overflow": "" }, model),
    price
      ? el("div", { class: "account-model-price-list" }, priceRows.map(priceRow))
      : el("span", { class: "muted small", text: m("ui.no_price_configured_currently_unavailable") })
  );
}

$("account-usage-copy").onclick = async () => {
  try {
    await copyText(ccSwitchUsageScript(location.origin));
    notify(m("ui.ccswitch_script_copied"), "ok");
  } catch (err) { notify(err.message || String(err), "err"); }
};
