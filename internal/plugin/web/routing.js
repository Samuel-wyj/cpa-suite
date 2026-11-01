let keyRouteSelection = { scope: "" };

function editKeyRoutes(view) {
  closeHoverTooltip();
  openEditor("route-picker", "route", (data) => {
    const key = data.keys.find((entry) => entry.scope === view.scope);
    if (!key || key.deleted_at) throw new UIError(m("ui.this_api_key_no_longer_exists_refresh_the_list"));
    keyRouteSelection = {
      scope: view.scope,
      routeIDs: new Set(key.route_bindings.route_ids),
      ...routingEditorState(key.route_bindings, "route-picker-enabled-only")
    };
    $("route-picker-search").value = "";
    setText($("route-picker-identity"), apiKeyIdentity(key).text);
    renderKeyRouteList();
  });
}

function routeBindingChoice(selection, id, label, rerender) {
  return el(
    "label",
    { class: "routing-option", "data-routing-binding": id },
    checkboxControl({
      input: el("input", {
        type: "checkbox",
        value: id,
        "data-routing-focus": "route:" + id,
        checked: selection.routeIDs.has(id),
        onchange: (event) => {
          if (event.target.checked) selection.routeIDs.add(id); else selection.routeIDs.delete(id);
          rerender();
        }
      })
    }),
    actionIcon("route"),
    el("span", {}, displayText(label))
  );
}

function renderKeyRouteList() {
  renderRoutingList(
    $("route-picker-list"),
    keyRouteSelection,
    editors.get($("route-picker")).data,
    $("route-picker-search").value,
    renderKeyRouteList
  );
}

function renderRouteRuleList() {
  renderRoutingList(
    $("route-rule-list"),
    routeRuleSelection,
    editors.get($("route-dialog")).data,
    $("route-rule-search").value,
    renderRouteRuleList
  );
  renderRouteOrder();
  scheduleRoutePreview();
}

function routingChoiceGroups(selection, data, term) {
  const groups = [];
  if (selection.routeIDs) {
    const routes = data.routes.map((route) => ({ id: route.id, name: route.name || route.id }));
    for (const id of selection.routeIDs) {
      if (!routes.some((route) => route.id === id)) routes.push({ id, name: m("ui.routing_rule_no_longer_exists_value", { v0: id }) });
    }
    const choices = routes.filter((route) => matchesTerm(route.name, term))
      .map((route) => ({ kind: "route", value: route.id, label: route.name }));
    groups.push(["routes", m("ui.routing_rules"), choices]);
  }
  groups.push(
    ["credentials", m("ui.upstream_credentials"), routeCredentialChoices(selection, data.credentials, term)],
    ["models", m("ui.model"), routeModelChoices(selection, data.models, term)]
  );
  return groups;
}

function renderRoutingList(list, selection, data, search, rerender) {
  const term = search.trim().toLowerCase();
  const scrollTop = list.scrollTop;
  const focusKey = list.contains(document.activeElement) ? document.activeElement.dataset.routingFocus : null;
  if (term && !selection.beforeSearch) {
    selection.beforeSearch = selection.expanded;
    selection.expanded = { routes: true, credentials: true, models: true };
  } else if (!term && selection.beforeSearch) {
    selection.expanded = selection.beforeSearch;
    selection.beforeSearch = null;
  }
  const sections = routingChoiceGroups(selection, data, term).map(([kind, title, choices]) => {
    const options = choices.map((item) =>
      item.kind === "route" ? routeBindingChoice(selection, item.value, item.label, rerender) : routeChoiceRow(selection, item, rerender)
    );
    return routingSection(selection, kind, title, options, rerender);
  });
  list.replaceChildren(...sections);
  list.scrollTop = scrollTop;
  if (focusKey)
    [...list.querySelectorAll("[data-routing-focus]")].find((control) => control.dataset.routingFocus === focusKey)
      ?.focus({ preventScroll: true });
}

function routingSection(selection, kind, title, options, rerender) {
  const selected =
    kind === "routes"
      ? selection.routeIDs.size
      : kind === "credentials" ? selection.credential.size + selection.provider.size : selection.model.size;
  const expanded = !!selection.expanded[kind];
  return el(
    "div",
    { class: "routing-section", "data-routing-section": kind },
    el(
      "div",
      {
        class: "routing-section-header",
        onclick: (event) => {
          if (event.target.closest(".compact-toggle")) return;
          event.currentTarget.querySelector(".routing-section-toggle").focus({ preventScroll: true });
          selection.expanded[kind] = !expanded;
          rerender();
        }
      },
      el(
        "button",
        {
          type: "button",
          class: "routing-section-toggle",
          "aria-label": m(expanded ? "ui.collapse_section" : "ui.expand_section", { section: title }),
          "aria-expanded": String(expanded),
          "data-routing-focus": "section:" + kind
        },
        el("span", { class: "routing-chevron", "aria-hidden": "true" }, actionIcon("chevron")),
        el("span", { text: title })
      ),
      kind === "credentials"
        ? compactEnabledToggle(
            selection.enabledOnly,
            (checked) => {
              selection.enabledOnly = checked;
              saveViewPreference("admin", selection.preference, checked);
              rerender();
            },
            m("ui.show_only_enabled_upstream_credentials")
          )
        : null,
      el("span", { class: "routing-section-count", text: selected + "/" + options.length })
    ),
    expanded
      ? el(
          "div",
          {},
          ...(options.length ? options : [el("div", { class: "routing-empty", text: m("ui.no_value_available", { v0: title }) })])
        )
      : null
  );
}

function compactEnabledToggle(checked, onChange, title) {
  return el(
    "label",
    { class: "toggle-filter compact-toggle", "data-tip": title },
    switchControl({
      input: el("input", {
        type: "checkbox",
        role: "switch",
        "data-routing-focus": "enabled-only",
        checked,
        onchange: (event) => onChange(event.target.checked)
      })
    }),
    el("span", { text: m("ui.enabled_only") })
  );
}

function routingDimensionChips(rule, kind, labels) {
  const items = [];
  for (const denied of [false, true]) {
    const prefix = denied ? "denied_" : "";
    const state = denied ? "deny" : "allow";
    if (kind === "model") items.push(...(rule[prefix + "models"] || []).map((text) => ({ text, state, iconName: "astroid" })));
    else {
      items.push(
        ...(rule[prefix + "credential_providers"] || []).map((item) => ({
          text: credentialProviderLabel(item),
          state,
          tag: m("ui.entire_category"),
          iconName: credentialIcon(item)
        }))
      );
      items.push(
        ...(rule[prefix + "credential_ids"] || []).map((ref) => {
          const credential = (resources.admin.credentials.value || []).find((item) => item.ref === ref);
          return {
            text: credential
              ? maskedTailText(credential.provider, credentialName(credential))
              : labels?.[ref]
                ? maskedLabelText(labels[ref])
                : maskedTailText(m("ui.selected_upstream_credentialvaluevalue", { v0: "", v1: "" }), ref.slice(-8)),
            state,
            iconName: credential ? credentialIcon(credential) : "key"
          };
        })
      );
    }
  }
  return items;
}

function renderRoutes() {
  if (renderDataPending($("routes-body"), [resources.admin.routes, resources.admin.keys], m("ui.loading"), "empty section-loading")) return;
  const routes = resources.admin.routes.value.map((route) => ({
    route,
    boundKeys: resources.admin.keys.value.filter((key) => key.route_bindings.route_ids.includes(route.id))
  }));
  renderCollection($("routes-body"), routes, {
    attrs: { class: "event-list settings-card-list", role: "list" },
    empty: m("ui.no_routing_rules"),
    render: ({ route, boundKeys }) => {
      const fields = [
        ["model", m("ui.model_access"), m("ui.all_models"), "astroid"],
        ["credential", m("ui.credential_access"), m("ui.all_credentials"), "key"]
      ].map(([kind, label, fallback, iconName]) => {
        const items = routingDimensionChips(route.rule, kind, route.credential_labels);
        if (items.length && !items.some((item) => item.state === "allow")) items.unshift({ text: fallback, iconName });
        return settingsEntryField(label, settingsChipList(items, fallback));
      });
      const name = route.name || route.id;
      const order = route.rule.credential_order;
      const orderItems = [{ text: order?.mode === "custom" ? m("ui.order_mode_custom") : m("ui.order_mode_cpa"), iconName: "route" }];
      if (order?.mode === "custom")
        orderItems.push({ text: order.same_tier === "round-robin" ? m("ui.same_tier_round_robin") : m("ui.same_tier_fill_first") });
      if (order?.rule_priority) orderItems.push({ text: m("ui.rule_priority_value", { v0: String(order.rule_priority) }) });
      fields.push(settingsEntryField(m("ui.credential_usage_order"), settingsChipList(orderItems, "—")));
      return settingsCard(
        name,
        [...fields, boundKeyField(boundKeys)],
        () => editRoute(route),
        () =>
          openActionDialog(
            m("ui.delete_routing_rule_value", { v0: name }),
            boundKeys.length
              ? m("ui.remove_this_rule_from_value_of_these_value_non_deleted_keys_will_have_unrestricted_mo", {
                  v0: bindingSummary(boundKeys),
                  v1: route.fully_unrestricted_keys || 0
                })
              : m("ui.this_action_cannot_be_undone"),
            undefined,
            async () => {
              await mutateAdmin("DELETE", "/routes?id=" + encodeURIComponent(route.id));
              notify(m("ui.routing_rule_deleted"));
            }
          )
      );
    }
  });
}

let routeRuleSelection;

function routeProviderChoices(credentials, selected) {
  const providers = [];
  for (const item of credentials) {
    const existing = providers.find((provider) => sameCredentialProvider(provider, item));
    if (existing) { existing.available ||= !item.disabled && !item.unavailable; } else {
      providers.push({ source: item.source, provider: item.provider, available: !item.disabled && !item.unavailable });
    }
  }
  for (const item of selected) {
    if (providers.some((provider) => sameCredentialProvider(provider, item))) continue;
    providers.push({ ...item, available: false });
  }
  return providers.sort(compareCredentialProviders);
}

const ROUTING_FIELDS = { model: "models", credential: "credential_ids", provider: "credential_providers" };

function routingSelection(rule = {}) {
  const selection = Object.fromEntries(Object.keys(ROUTING_FIELDS).map((kind) => [kind, new Map()]));
  for (const [kind, field] of Object.entries(ROUTING_FIELDS)) {
    for (const [prefix, state] of [["", "allow"], ["denied_", "deny"]]) {
      for (const value of rule[prefix + field] || []) setRouteChoice(selection, kind, value, state);
    }
  }
  return selection;
}

function routingPayload(selection) {
  const rule = {};
  for (const [kind, field] of Object.entries(ROUTING_FIELDS)) {
    rule[field] = [];
    rule["denied_" + field] = [];
    for (const { value, state } of selection[kind].values()) { rule[(state === "deny" ? "denied_" : "") + field].push(value); }
  }
  const order = orderPayload(selection.order);
  if (order) rule.credential_order = order;
  return rule;
}

function routingEditorState(rule, preference) {
  return {
    ...routingSelection(rule),
    expanded: { routes: true, credentials: true, models: true },
    enabledOnly: viewPreference("admin", preference, true),
    preference,
    beforeSearch: null
  };
}

function routeChoiceKey(kind, value) { return (kind === "provider" ? value.source + ":" + value.provider : value).toLowerCase(); }

function selectedRouteValues(selection, kind) { return [...selection[kind].values()].map((item) => item.value); }

function routeChoiceState(selection, kind, value) { return selection[kind].get(routeChoiceKey(kind, value))?.state || "none"; }

function setRouteChoice(selection, kind, value, state) {
  const key = routeChoiceKey(kind, value);
  if (state === "none") selection[kind].delete(key);
  else selection[kind].set(key, { value: kind === "provider" ? { source: value.source, provider: value.provider } : value, state });
}

function routeChoiceRow(selection, item, rerender) {
  const { kind, value, label, text, iconName, badge, note } = item;
  const state = routeChoiceState(selection, kind, value);
  const next = { none: "allow", allow: "deny", deny: "none" }[state];
  const names = { none: m("ui.not_selected"), allow: m("ui.allowlist"), deny: m("ui.denylist") };
  const key = kind + ":" + (kind === "provider" ? value.source + ":" + value.provider : value);
  const title = m("ui.valuevalue_value_click_to_switch_to_value", {
    v0: kind === "credential" && iconName ? m("ui.join", { v0: credentialSourceLabel(item.source), v1: DISPLAY_SEPARATOR, v2: "" }) : "",
    v1: label,
    v2: names[state],
    v3: names[next]
  });
  return el(
    "button",
    {
      type: "button",
      class: "routing-option route-choice",
      "data-state": state,
      "data-routing-choice": key,
      "data-routing-focus": key,
      "data-tip": title,
      "aria-label": title,
      onclick: () => {
        setRouteChoice(selection, kind, value, next);
        rerender();
      }
    },
    checkboxControl({ state }),
    iconName ? actionIcon(iconName) : null,
    el("span", { class: "route-choice-name" + (kind === "model" ? " mono" : "") }, text || displayText(label)),
    kind === "provider" ? el("span", { class: "tag warn", text: m("ui.entire_category") }) : null,
    badge ? el("span", { class: "tag plain", text: badge }) : null,
    note ? el("span", { class: "route-choice-note", text: note }) : null
  );
}

function routeCredentialChoices(selection, credentials, term) {
  const selectedProviders = selectedRouteValues(selection, "provider");
  const options = routeProviderChoices(credentials, selectedProviders).filter(
    (item) =>
      (!selection.enabledOnly || item.available || routeChoiceState(selection, "provider", item) !== "none") &&
      matchesTerm(m("ui.value_entire_category", { v0: credentialProviderLabel(item) }), term)
  )
    .map((item) => ({
      kind: "provider",
      value: item,
      label: credentialProviderLabel(item),
      iconName: credentialIcon(item),
      badge: item.available ? "" : m("ui.none_available")
    }));
  for (const item of credentials.filter(
    (item) =>
      (!selection.enabledOnly || !item.disabled || routeChoiceState(selection, "credential", item.ref) !== "none") &&
      matchesTerm(credentialLabel(item), term)
  )
    .sort(compareCredentials)) {
    options.push({
      kind: "credential",
      value: item.ref,
      label: credentialLabel(item),
      text: maskedTailText(item.provider, credentialName(item)),
      source: item.source,
      iconName: credentialIcon(item),
      badge: item.disabled ? m("ui.disabled") : item.unavailable ? m("ui.none_available") : ""
    });
  }
  for (const ref of selectedRouteValues(selection, "credential")) {
    if (!credentials.some((item) => item.ref === ref) && matchesTerm(ref, term))
      options.push({
        kind: "credential",
        value: ref,
        label: m("ui.credential_no_longer_exists"),
        iconName: "key",
        badge: m("ui.none_available")
      });
  }
  return options;
}

function routeModelChoices(selection, available, term) {
  // Keep retired selections so a temporary provider outage cannot rewrite access.
  const known = new Set(available.map((model) => model.toLowerCase()));
  const retired = selectedRouteValues(selection, "model").filter((model) => !known.has(model.toLowerCase())).sort(compareModelId);
  return available.concat(retired).filter((model) => matchesTerm(model, term)).map((model) => ({
    kind: "model",
    value: model,
    label: model,
    iconName: "astroid",
    note: retired.includes(model) ? m("ui.not_in_the_cpa_model_list") : ""
  }));
}

function renderRouteKeyList(keys, routeID) {
  const list = $("route-key-list");
  keys = keys.filter((key) => !key.deleted_at || (routeID && key.route_bindings.route_ids.includes(routeID)));
  if (!keys.length) {
    list.replaceChildren(el("div", { class: "empty", text: m("ui.no_api_keys") }));
    return;
  }
  list.replaceChildren(
    ...keys.map((key) => {
      const checked = !!routeID && key.route_bindings.route_ids.includes(routeID);
      return el(
        "label",
        {},
        checkboxControl({ input: el("input", { type: "checkbox", value: key.scope, checked }) }),
        renderAPIKeyIdentity(key),
        key.deleted_at ? el("span", { class: "tag plain", text: m("ui.deleted"), "data-tip": m("ui.uncheck_to_remove_the_binding") }) : null
      );
    })
  );
}

function editRoute(route) {
  const id = route?.id;
  openEditor("route-dialog", "route", (data) => {
    const current = id ? data.routes.find((entry) => entry.id === id) : null;
    if (id && !current) throw new UIError(m("ui.this_routing_rule_no_longer_exists_close_the_dialog_and_refresh_the_list"));
    setText($("route-dialog-title"), current ? m("ui.edit_rule_value", { v0: current.name || id }) : m("ui.create_rule"));
    $("route-id").value = id || "";
    setText($("route-save"), current ? m("ui.save") : m("ui.create"));
    $("route-name").value = current?.name || "";
    $("route-rule-search").value = "";
    routeRuleSelection = routingEditorState(current?.rule, "route-credential-enabled-only");
    routeRuleSelection.order = orderFromRule(current?.rule);
    renderRouteRuleList();
    renderRouteKeyList(data.keys, id || "");
    return { route: structuredClone(current), scopes: selectedKeyScopes("route-key-list") };
  });
}

$("route-picker-search").addEventListener("input", renderKeyRouteList);
$("route-picker-all").onclick = () => setVisibleRouteChoices("route-picker", keyRouteSelection, "allow", renderKeyRouteList);
$("route-picker-deny-all").onclick = () => setVisibleRouteChoices("route-picker", keyRouteSelection, "deny", renderKeyRouteList);
$("route-picker-save").onclick = () =>
  submitEditor("route-picker", async () => {
    await mutateAdmin("PUT", "/keys/routes", {
      scope: keyRouteSelection.scope,
      bindings: { route_ids: [...keyRouteSelection.routeIDs], ...routingPayload(keyRouteSelection) }
    });
    notify(m("ui.api_key_routing_rules_saved"));
  });
$("route-picker-clear").onclick = () => {
  keyRouteSelection.routeIDs.clear();
  Object.assign(keyRouteSelection, routingSelection());
  renderKeyRouteList();
};
$("route-picker").addEventListener("close", () => { if (!$("route-picker").open) keyRouteSelection = { scope: "" }; });
$("route-rule-search").addEventListener("input", renderRouteRuleList);
function setVisibleRouteChoices(id, selection, state, rerender) {
  const dialog = $(id);
  const data = editors.get(dialog).data;
  const term = dialog.querySelector('input[type="search"]').value.trim().toLowerCase();
  for (const [, , choices] of routingChoiceGroups(selection, data, term)) {
    for (const item of choices) {
      if (item.kind === "route") selection.routeIDs.add(item.value); else setRouteChoice(selection, item.kind, item.value, state);
    }
  }
  rerender();
}
$("route-rule-all").onclick = () => setVisibleRouteChoices("route-dialog", routeRuleSelection, "allow", renderRouteRuleList);
$("route-rule-deny-all").onclick = () => setVisibleRouteChoices("route-dialog", routeRuleSelection, "deny", renderRouteRuleList);
$("route-rule-none").onclick = () => {
  Object.assign(routeRuleSelection, routingSelection());
  renderRouteRuleList();
};
$("route-new").onclick = () => editRoute();
$("route-name").addEventListener("input", scheduleRoutePreview);
$("route-save").onclick = () =>
  submitEditor("route-dialog", async (editor) => {
    const name = $("route-name").value.trim();
    if (!name) throw new UIError(m("ui.enter_a_rule_name"));
    const id = $("route-id").value;
    const rule = routingPayload(routeRuleSelection);
    const scopes = selectedKeyScopes("route-key-list");
    const data = { name, rule, scopes };
    if (id) {
      const route = editor.initial.route;
      if (
        route?.bound_key_count &&
        !(await openActionDialog(
          m("ui.save_routing_rule"),
          m("ui.update_the_selected_keys_bindings_and_rules_value", {
            v0: route.deleted_key_count ? m("ui.deleted_keys_will_also_retain_or_lose_bindings_according_to_this_selection_retained_b") : ""
          })
        ))
      )
        return false;
      if (!editorCurrent(editor)) return false;
      if (sameScopes(scopes, editor.initial.scopes)) delete data.scopes;
      if (name === route.name) delete data.name;
      const saved = { ...routingSelection(route.rule), order: { ...orderFromRule(route.rule), touched: true } };
      if (!route.rule?.credential_order && !rule.credential_order) delete saved.order;
      if (JSON.stringify(rule) === JSON.stringify(routingPayload(saved))) delete data.rule;
      if (!Object.keys(data).length) return;
      await mutateAdmin("PATCH", "/routes", { id, ...data });
    } else await mutateAdmin("POST", "/routes", data);
    notify(id ? m("ui.routing_rule_saved") : m("ui.routing_rule_created"));
  });

// per-rule credential order, rule priority and the
// effective-order preview. Legacy rules (no credential_order) keep CPA's
// priority until the user touches the order controls.
const ORDER_LIMIT = 256;

function orderFromRule(rule) {
  const order = rule?.credential_order;
  if (!order) {
    // A new rule starts in custom order; an existing one keeps CPA priority.
    return {
      legacy: !!rule,
      mode: rule ? "cpa" : "custom",
      rule_priority: 0,
      same_tier: "fill-first",
      fallback_return: "new-session",
      items: []
    };
  }
  const items = (order.items || []).map((item, index, list) => ({
    ref: item.ref,
    tie: index > 0 && item.priority === list[index - 1].priority
  }));
  return {
    legacy: false,
    mode: order.mode === "custom" ? "custom" : "cpa",
    rule_priority: Number(order.rule_priority) || 0,
    same_tier: order.same_tier === "round-robin" ? "round-robin" : "fill-first",
    fallback_return: order.fallback_return === "immediate" ? "immediate" : "new-session",
    items
  };
}

function orderPayload(order) {
  if (!order || (order.legacy && !order.touched)) return undefined;
  const levels = order.items.reduce((count, item, index) => count + (index === 0 || !item.tie ? 1 : 0), 0);
  let level = 0;
  const items =
    order.mode === "custom"
      ? order.items.map((item, index) => {
          if (index > 0 && !item.tie) level++;
          return { ref: item.ref, priority: (levels - level) * 10 };
        })
      : [];
  const payload = { mode: order.mode, rule_priority: order.rule_priority };
  if (items.length) payload.items = items;
  payload.same_tier = order.same_tier;
  payload.fallback_return = order.fallback_return;
  return payload;
}

// The credentials this rule lets through, best CPA priority first; null when
// the rule has no explicit credential allowlist.
function orderCandidates(selection, credentials) {
  const allowRefs = new Set(), denyRefs = new Set(), allowProviders = [], denyProviders = [];
  for (const { value, state } of selection.credential.values()) (state === "deny" ? denyRefs : allowRefs).add(value);
  for (const { value, state } of selection.provider.values()) (state === "deny" ? denyProviders : allowProviders).push(value);
  const denied = (item) => denyRefs.has(item.ref) || denyProviders.some((provider) => sameCredentialProvider(provider, item));
  const explicit = allowRefs.size > 0 || allowProviders.length > 0;
  const allowed = (item) =>
    !denied(item) && (!explicit || allowRefs.has(item.ref) || allowProviders.some((provider) => sameCredentialProvider(provider, item)));
  const known = new Map(credentials.map((item) => [item.ref, item]));
  const list = credentials.filter(allowed);
  for (const ref of allowRefs) if (!known.has(ref) && !denyRefs.has(ref)) list.push({ ref, missing: true });
  list.sort((a, b) => (Number(b.priority) || 0) - (Number(a.priority) || 0) || (a.missing ? 1 : b.missing ? -1 : compareCredentials(a, b)));
  return { explicit, list, known, denied: (ref) => denyRefs.has(ref) || (known.has(ref) && denied(known.get(ref))) };
}

function syncOrder(selection, data) {
  const order = selection.order;
  if (!order) return null;
  const candidates = orderCandidates(selection, data.credentials);
  if (order.mode !== "custom") return candidates;
  const allowedRefs = new Set(candidates.list.map((item) => item.ref));
  // With an explicit allowlist the list mirrors it; otherwise keep what the
  // rule already orders and drop only what is now denied.
  order.items = order.items.filter((item) => (candidates.explicit ? allowedRefs.has(item.ref) : !candidates.denied(item.ref)));
  if (order.items.length) order.items[0].tie = false;
  if (candidates.explicit) {
    for (const item of candidates.list) {
      if (order.items.length >= ORDER_LIMIT) break;
      if (!order.items.some((entry) => entry.ref === item.ref)) order.items.push({ ref: item.ref, tie: false });
    }
  }
  return candidates;
}

function orderLabel(item, ref) {
  if (!item || item.missing) return [m("ui.credential_no_longer_exists")];
  return maskedTailText(item.provider, credentialName(item));
}

function orderSegment(options, value, onChange, label) {
  return el(
    "div",
    { class: "route-seg", role: "group", "aria-label": label },
    options.map(([key, text, tip]) =>
      el("button", {
        type: "button",
        class: key === value ? "active" : "",
        "aria-pressed": String(key === value),
        "data-order-option": key,
        "data-tip": tip || null,
        text,
        onclick: () => { if (key !== value) onChange(key); }
      })
    )
  );
}

let orderDragIndex = -1;

function moveOrderItem(order, from, to) {
  if (to < 0 || to >= order.items.length || from === to) return;
  const [item] = order.items.splice(from, 1);
  item.tie = false;
  order.items.splice(to, 0, item);
  order.items[0].tie = false;
}

function renderRouteOrder() {
  const host = $("route-order");
  const editor = editors.get($("route-dialog"));
  const selection = routeRuleSelection;
  if (!editor || !selection?.order) return;
  const order = selection.order;
  const candidates = syncOrder(selection, editor.data);
  const rerender = () => {
    order.touched = true;
    renderRouteOrder();
    scheduleRoutePreview();
  };
  const header = el(
    "div",
    { class: "route-order-header" },
    el(
      "div",
      { class: "route-order-title" },
      el("strong", { text: m("ui.credential_usage_order") }),
      el("span", { class: "small muted", text: m("ui.credential_usage_order_help") })
    ),
    orderSegment(
      [["cpa", m("ui.order_mode_cpa"), m("ui.order_mode_cpa_tip")], ["custom", m("ui.order_mode_custom"), m("ui.order_mode_custom_tip")]],
      order.mode,
      (mode) => {
        order.mode = mode;
        if (mode === "custom" && !order.items.length)
          order.items = candidates.list.slice(0, ORDER_LIMIT).map((item) => ({ ref: item.ref, tie: false }));
        rerender();
      },
      m("ui.credential_usage_order")
    ),
    el(
      "label",
      { class: "route-order-priority", "data-tip": m("ui.rule_priority_tip") },
      el("span", { text: m("ui.rule_priority") }),
      el("input", {
        id: "route-rule-priority",
        type: "number",
        step: "1",
        min: "-1000000",
        max: "1000000",
        value: String(order.rule_priority),
        oninput: (event) => {
          const value = Math.trunc(Number(event.target.value));
          order.rule_priority = Number.isFinite(value) ? Math.max(-1000000, Math.min(1000000, value)) : 0;
          order.touched = true;
          scheduleRoutePreview();
        }
      })
    )
  );
  const body = [];
  if (order.mode === "cpa") {
    body.push(el("p", { class: "small muted route-order-note", text: m("ui.order_mode_cpa_note") }));
    if (candidates.list.length) body.push(orderRows(order, candidates, true, rerender));
  } else if (!candidates.explicit && !order.items.length) {
    body.push(el("p", { class: "small muted route-order-note", text: m("ui.order_needs_allowlist") }));
  } else {
    body.push(orderRows(order, candidates, false, rerender));
    body.push(
      el(
        "div",
        { class: "route-order-settings" },
        el("span", { class: "small muted", text: m("ui.same_level_credentials") }),
        orderSegment(
          [
            ["fill-first", m("ui.same_tier_fill_first"), m("ui.same_tier_fill_first_tip")],
            ["round-robin", m("ui.same_tier_round_robin"), m("ui.same_tier_round_robin_tip")]
          ],
          order.same_tier,
          (value) => {
            order.same_tier = value;
            rerender();
          },
          m("ui.same_level_credentials")
        ),
        el("span", { class: "small muted", text: m("ui.after_recovery") }),
        orderSegment(
          [
            ["new-session", m("ui.fallback_new_session"), m("ui.fallback_new_session_tip")],
            ["immediate", m("ui.fallback_immediate"), m("ui.fallback_immediate_tip")]
          ],
          order.fallback_return,
          (value) => {
            order.fallback_return = value;
            rerender();
          },
          m("ui.after_recovery")
        )
      )
    );
  }
  host.replaceChildren(header, ...body);
}

function orderRows(order, candidates, readOnly, rerender) {
  const rows = readOnly ? candidates.list.map((item) => ({ ref: item.ref, tie: false })) : order.items;
  let rank = 0;
  return el(
    "ol",
    { class: "route-order-list" + (readOnly ? " read-only" : ""), "aria-label": m("ui.credential_usage_order") },
    rows.map((entry, index) => {
      const item = candidates.known.get(entry.ref);
      if (readOnly) {
        if (index === 0 || (Number(item?.priority) || 0) !== (Number(candidates.known.get(rows[index - 1].ref)?.priority) || 0)) rank++;
      } else if (index === 0 || !entry.tie) rank++;
      const state = !item ? "missing" : item.disabled ? "disabled" : item.unavailable ? "unavailable" : "ok";
      const move = (from, to) => {
        if (readOnly) {
          order.mode = "custom";
          order.items = rows.map((row) => ({ ...row }));
        }
        moveOrderItem(order, from, to);
        rerender();
      };
      return el(
        "li",
        {
          class: "route-order-row" + (entry.tie && !readOnly ? " tied" : ""),
          draggable: "true",
          "data-order-ref": entry.ref,
          "data-state": state,
          ondragstart: (event) => {
            orderDragIndex = index;
            event.dataTransfer.effectAllowed = "move";
            event.dataTransfer.setData("text/plain", String(index));
            event.currentTarget.classList.add("dragging");
          },
          ondragend: (event) => event.currentTarget.classList.remove("dragging"),
          ondragover: (event) => {
            event.preventDefault();
            event.currentTarget.classList.add("drop-target");
          },
          ondragleave: (event) => event.currentTarget.classList.remove("drop-target"),
          ondrop: (event) => {
            event.preventDefault();
            event.currentTarget.classList.remove("drop-target");
            if (orderDragIndex >= 0) move(orderDragIndex, index);
            orderDragIndex = -1;
          }
        },
        el("span", { class: "route-order-grip", "aria-hidden": "true", "data-tip": m("ui.drag_to_reorder") }, actionIcon("grip-vertical")),
        el("span", { class: "route-order-rank", text: String(rank) }),
        item ? actionIcon(credentialIcon(item)) : actionIcon("key"),
        el("span", { class: "route-order-name" }, orderLabel(item, entry.ref)),
        el("span", {
          class: "tag plain",
          "data-tip": m("ui.cpa_priority_tip"),
          text: m("ui.cpa_priority_value", { v0: String(Number(item?.priority) || 0) })
        }),
        state === "ok"
          ? null
          : el("span", {
              class: "tag " + (state === "unavailable" ? "warn" : "bad"),
              text: state === "disabled" ? m("ui.disabled") : state === "missing" ? m("ui.deleted") : m("ui.cooling_or_quota")
            }),
        readOnly
          ? null
          : el(
              "label",
              { class: "route-order-tie", "data-tip": m("ui.same_level_as_previous_tip") },
              el("input", {
                type: "checkbox",
                checked: !!entry.tie,
                disabled: index === 0,
                onchange: (event) => {
                  entry.tie = event.target.checked;
                  rerender();
                }
              }),
              el("span", { text: m("ui.same_level_as_previous") })
            ),
        el(
          "span",
          { class: "route-order-moves" },
          el(
            "button",
            {
              type: "button",
              class: "icon-button",
              disabled: index === 0,
              "aria-label": m("ui.move_up"),
              "data-tip": m("ui.move_up"),
              onclick: () => move(index, index - 1)
            },
            actionIcon("arrow-up")
          ),
          el(
            "button",
            {
              type: "button",
              class: "icon-button",
              disabled: index === rows.length - 1,
              "aria-label": m("ui.move_down"),
              "data-tip": m("ui.move_down"),
              onclick: () => move(index, index + 1)
            },
            actionIcon("arrow-down")
          )
        )
      );
    })
  );
}

let routePreviewTimer = 0, routePreviewSeq = 0;

function scheduleRoutePreview() {
  clearTimeout(routePreviewTimer);
  routePreviewTimer = setTimeout(runRoutePreview, 250);
}

async function runRoutePreview() {
  const dialog = $("route-dialog");
  const editor = editors.get(dialog);
  if (!dialog.open || !editor || !routeRuleSelection) return;
  const seq = ++routePreviewSeq;
  const body = {
    id: $("route-id").value,
    name: $("route-name").value.trim(),
    rule: routingPayload(routeRuleSelection),
    scopes: selectedKeyScopes("route-key-list")
  };
  let result;
  try { result = await plugin("POST", "/routes/preview", body); } catch (error) {
    if (error instanceof AuthError || error instanceof StaleRequestError) return;
    if (seq === routePreviewSeq)
      $("route-preview").replaceChildren(el("div", { class: "notice err show", text: error.message || String(error) }));
    return;
  }
  if (seq !== routePreviewSeq || !dialog.open) return;
  renderRoutePreview(result, editor.data);
}

function previewCredentialText(entry) {
  if (entry.state === "missing") return [m("ui.credential_no_longer_exists")];
  return maskedTailText(entry.provider, credentialName(entry));
}

function previewHintNodes(hints) {
  const nodes = [];
  const hint = (kind, text, extra) => nodes.push(el("div", { class: "route-preview-hint " + kind }, el("span", { text }), extra || null));
  if (hints.host_top_tier_only) hint("warn", m("ui.hint_host_top_tier_only"));
  if (hints.websockets_mismatch) {
    const names = (list) =>
      el("span", { class: "route-preview-names" }, list.map((entry, index) => [index ? ", " : "", previewCredentialText(entry)]));
    hint(
      "warn",
      m("ui.hint_websockets_mismatch"),
      el(
        "div",
        { class: "small" },
        el("div", {}, m("ui.websockets_on_value", { v0: "" }), names(hints.websockets_on || [])),
        el("div", {}, m("ui.websockets_off_value", { v0: "" }), names(hints.websockets_off || []))
      )
    );
  }
  if (hints.order_unusable?.length) hint("warn", m("ui.hint_order_unusable_value", { v0: String(hints.order_unusable.length) }));
  if (hints.inventory_incomplete) hint("muted", m("ui.hint_inventory_incomplete"));
  return nodes;
}

function renderRoutePreview(result, data) {
  const host = $("route-preview");
  const keys = requireArray(result?.keys, m("ui.effective_order_preview"));
  const hints = result?.hints || {};
  const sections = [
    el(
      "div",
      { class: "route-preview-title" },
      el("strong", { text: m("ui.effective_order_preview") }),
      el("span", {
        class: "small muted",
        text: m("ui.effective_order_preview_help_value", { v0: String(hints.session_ttl_minutes ?? 120) })
      })
    )
  ];
  sections.push(...previewHintNodes(hints));
  if (!keys.length) sections.push(el("p", { class: "small muted", text: m("ui.preview_select_keys") }));
  for (const key of keys) {
    const view = data.keys.find((entry) => entry.scope === key.scope);
    const identity = view ? renderAPIKeyIdentity(view) : el("span", { class: "muted", text: m("ui.unknown_2") });
    const meta = [];
    if (key.rule_names?.length > 1) meta.push(m("ui.preview_rules_value", { v0: key.rule_names.join(" > ") }));
    if (key.custom) meta.push(key.same_tier === "round-robin" ? m("ui.same_tier_round_robin") : m("ui.same_tier_fill_first"));
    else if (key.unrestricted) meta.push(m("ui.preview_unrestricted")); else meta.push(m("ui.order_mode_cpa"));
    const conflicts = (key.conflicts || []).map((conflict) =>
      el(
        "div",
        { class: "route-preview-hint muted" },
        m("ui.preview_conflict_value", { v0: conflict.rules.join(", "), v1: conflict.winner })
      )
    );
    sections.push(
      el(
        "div",
        { class: "route-preview-key", "data-preview-scope": key.scope },
        el("div", { class: "route-preview-key-head" }, identity, el("span", { class: "small muted", text: meta.join(" · ") })),
        key.error ? el("div", { class: "notice err show", text: key.error }) : null,
        key.unrestricted && !key.order?.length
          ? el("p", { class: "small muted", text: m("ui.preview_cpa_decides") })
          : el(
              "ol",
              { class: "route-preview-order" },
              (key.order || []).map((entry) =>
                el(
                  "li",
                  { "data-state": entry.state, class: entry.current_first ? "current" : "" },
                  el("span", { class: "route-order-rank", text: String(entry.rank) }),
                  el("span", { class: "route-order-name" }, previewCredentialText(entry)),
                  el("span", { class: "tag plain", text: m("ui.cpa_priority_value", { v0: String(entry.cpa_priority || 0) }) }),
                  entry.current_first ? el("span", { class: "tag ok", text: m("ui.current_first_choice") }) : null,
                  entry.state === "ok"
                    ? null
                    : el("span", {
                        class: "tag " + (entry.state === "unavailable" ? "warn" : "bad"),
                        text:
                          entry.state === "disabled"
                            ? m("ui.disabled")
                            : entry.state === "missing" ? m("ui.deleted") : m("ui.cooling_or_quota")
                      }),
                  !entry.listed && key.custom ? el("span", { class: "tag plain", text: m("ui.not_in_order") }) : null
                )
              )
            ),
        conflicts
      )
    );
  }
  host.replaceChildren(...sections);
  refreshHoverTooltip();
}

$("route-key-list").addEventListener("change", scheduleRoutePreview);
$("route-dialog").addEventListener("close", () => {
  clearTimeout(routePreviewTimer);
  routePreviewSeq++;
});
