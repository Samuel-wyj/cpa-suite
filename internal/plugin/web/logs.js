const expandedPluginLogIDs = new Set();

const pluginLogLevels = {
  debug: { label: m("ui.debug"), class: "tag plain" },
  error: { label: m("ui.error"), class: "tag bad" },
  info: { label: m("ui.info"), class: "tag info" }
};

function renderPluginLogs() {
  const list = pagedList("logs");
  const note = $("plugin-log-note");
  if (list.renderEmpty()) {
    setText(note, "");
    return;
  }
  const pluginLogs = list.data.entries;
  const filter = $("plugin-log-filter").value.trim().toLowerCase();
  const currentIDs = new Set(pluginLogs.map((entry) => String(entry.id)));
  for (const id of expandedPluginLogIDs) if (!currentIDs.has(id)) expandedPluginLogIDs.delete(id);
  const searched = pluginLogs.filter((pluginLog) => {
    if (!filter) return true;
    return matchesTerm(pluginLog.message || "", filter);
  });
  const counts = { ...list.data.level_counts, all: Object.values(list.data.level_counts || {}).reduce((sum, count) => sum + count, 0) };
  const levelSelect = $("plugin-log-level");
  for (const option of levelSelect.options) {
    const label = option.value === "all" ? m("ui.all_levels") : pluginLogLevels[option.value].label;
    const text = label + DISPLAY_SEPARATOR + int(counts[option.value]);
    if (option.textContent !== text) setText(option, text);
  }
  const total = counts[levelSelect.value] || 0;
  setText(note, loadedCountText(pluginLogs.length, total, list.complete));
  list.renderItems(searched, {
    attrs: { class: "event-list", role: "list", "aria-label": m("ui.plugin_log_list") },
    empty: pluginLogs.length ? m("ui.no_matching_entries_in_loaded_logs") : m("ui.no_plugin_logs"),
    render: (pluginLog) => {
      const level = pluginLogLevels[pluginLog.level] || pluginLogLevels.info;
      return el(
        "article",
        { class: "event-entry", role: "listitem" },
        el(
          "div",
          { class: "event-entry-head" },
          el("time", { class: "event-time", datetime: pluginLog.at || null }, when(pluginLog.at)),
          el("span", { class: level.class, text: level.label })
        ),
        renderPluginLogMessage(pluginLog)
      );
    }
  });
}

function routeLogDetails(row) {
  const details = document.createDocumentFragment();
  JSON.stringify(row, null, 2).split("\n").forEach((line, index) => {
    if (index) details.append("\n");
    const skipped = line.match(/^    "#\d+ /);
    if (skipped) {
      // "#N provider · account: reason" lines of the skip list.
      const separator = line.indexOf("·");
      const reason = separator < 0 ? -1 : line.indexOf(": ", separator);
      if (separator >= 0 && reason > separator) {
        details.append(
          line.slice(0, separator + 1),
          el("span", { class: "mask-blur", text: line.slice(separator + 1, reason) }),
          line.slice(reason)
        );
        return;
      }
    }
    const field = line.match(/^  "(key|selected_credential)": "/);
    if (!field) {
      details.append(line);
      return;
    }
    const separator = field[1] === "selected_credential" ? line.indexOf("·", field[0].length) : -1;
    const start = separator < 0 ? field[0].length : separator + 1;
    const end = line.lastIndexOf('"');
    // Preserve JSON whitespace and escapes; displayText would normalize middle-dot separators.
    details.append(line.slice(0, start), el("span", { class: "mask-blur", text: line.slice(start, end) }), line.slice(end));
  });
  return details;
}

function upstreamCredentialText(value) {
  const label = (name) => m("ui.upstream_credential_value", { v0: name });
  if (!value) return label(m("ui.unknown_2"));
  const [provider, account] = splitCredentialLabel(value);
  return provider ? maskedTailText(label(provider), account) : [label(""), maskedText(account)];
}

function renderPluginLogMessage(pluginLog) {
  const message = pluginLog.message || "";
  if (!message.startsWith("route ")) return el("div", { class: "plugin-log-message" }, message);
  try {
    const row = JSON.parse(message.slice(6));
    const model =
      row.model_result === "deny"
        ? m("ui.denied")
        : row.model_result === "configuration_error" ? m("ui.configuration_error") : m("ui.allowed");
    const credential =
      row.credential_result === "selected"
        ? upstreamCredentialText(row.selected_credential)
        : row.credential_result === "no_match"
          ? m("ui.upstream_credential_no_available_match")
          : row.credential_result === "not_reached"
            ? m("ui.upstream_credential_selection_not_reached")
            : row.credential_result === "not_observed"
              ? m("ui.upstream_credential_selection_not_observed")
              : row.credential_result === "configuration_error"
                ? m("ui.upstream_credential_configuration_error")
                : m("ui.upstream_credential_unrestricted");
    const outcome =
      { succeeded: m("ui.succeeded"), failed: m("ui.failed"), rejected: m("ui.rejected"), canceled: m("ui.canceled") }[row.outcome] ||
      row.outcome ||
      m("ui.unknown_2");
    const id = String(pluginLog.id);
    return el(
      "details",
      {
        class: "plugin-log-message",
        open: expandedPluginLogIDs.has(id),
        ontoggle: (event) => {
          if (!event.currentTarget.isConnected) return;
          if (event.currentTarget.open) expandedPluginLogIDs.add(id); else expandedPluginLogIDs.delete(id);
        }
      },
      el(
        "summary",
        { class: "details-summary" },
        el("span", { class: "details-summary-icon", "aria-hidden": "true" }, actionIcon("chevron")),
        el(
          "span",
          { class: "row" },
          el("span", {}, m("ui.api_key_value", { v0: "" }), row.key ? maskedText(row.key) : m("ui.unknown_2")),
          el("span", { text: m("ui.model_value_value", { v0: row.model || m("ui.unknown_2"), v1: model }) }),
          el("span", {}, credential),
          row.credential_rank
            ? el("span", {
                class: "tag plain",
                text: row.session_sticky
                  ? m("ui.credential_rank_sticky_value", { v0: String(row.credential_rank) })
                  : m("ui.credential_rank_value", { v0: String(row.credential_rank) })
              })
            : null,
          row.skipped?.length
            ? el("span", { class: "tag warn", text: m("ui.skipped_credentials_value", { v0: String(row.skipped.length) }) })
            : null,
          el("span", { text: m("ui.request_result_value", { v0: outcome }) })
        )
      ),
      el("pre", {}, routeLogDetails(row))
    );
  } catch (_) { return el("div", { class: "plugin-log-message" }, message); }
}

$("plugin-log-filter").addEventListener("input", () => {
  renderPluginLogs();
  const list = pagedList("logs");
  if (list.data) list.renderStatus();
});
$("plugin-log-level").addEventListener("change", () => {
  saveChoice("admin", "plugin-log-level");
  guard(() => pagedList("logs").load(true));
});
$("plugin-logs-clear").onclick = () =>
  openActionDialog(
    m("ui.clear_all_plugin_logs"),
    m("ui.this_permanently_clears_the_plugin_s_operational_logs_billing_data_is_unaffected"),
    undefined,
    async () => {
      const result = await mutateAdmin("DELETE", "/plugin-logs");
      notify(m("ui.cleared_value_plugin_log_entries", { v0: result.cleared }));
    }
  );
