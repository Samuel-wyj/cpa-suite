// -test browser checks against a real CPA host (new layout layer):
//  1. admin: 5 groups, every sub-tab opens its panel, "coming soon" entries are
//     disabled, no 404s / console errors / error banners, overview shows real data;
//  2. upstream actions still work through the new nav: create+edit plan (assign
//     key), create route, add+edit price, reset quota, add client rule;
//  3. API-key login: every account tab works;
//  4. CPAMP shell simulation (host attrs + variables + api-key-aliases mock):
//     alias is used as display name; light/dark/sim screenshots (SHOTS=dir).
//   node ui_suite4.mjs <base> <plugin-id> <mgmt-key> <api-key>
import puppeteer from "puppeteer-core";
const [base, id, mgmt, apiKey] = process.argv.slice(2);
const SHOTS = process.env.SHOTS;
const SIM_HTML = (await import("node:fs")).readFileSync(new URL("./cpamp-sim.html", import.meta.url), "utf8");
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || "/usr/bin/google-chrome", headless: true, args: ["--no-sandbox"] });
const failures = [], ok = [];
const check = (cond, msg) => (cond ? ok : failures).push(msg);
const ui = `${base}/v0/resource/plugins/${id}/ui`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const idle = (page) => (page.waitForNetworkIdle ? page.waitForNetworkIdle({ idleTime: 600 }) : sleep(900)).catch(() => {});
const wire = (page, tag, bag) => {
  page.on("console", (msg) => { if (msg.type() === "error" && !/Failed to load resource/.test(msg.text())) bag.push(`${tag} console: ${msg.text()}`); });
  page.on("pageerror", (e) => bag.push(`${tag} pageerror: ${e.message}`));
  page.on("response", (r) => { if (r.status() === 404 && !/favicon/.test(r.url())) bag.push(`${tag} 404 ${r.url()}`); });
};
async function login(page, key, account) {
  await page.waitForSelector("#gate-key", { visible: true });
  if (account) await click(page, "#gate-mode-account");
  await page.type("#gate-key", key);
  await Promise.all([idle(page), click(page, "#gate-submit")]);
  await sleep(500);
}
const click = (p, sel) => p.$eval(sel, (x) => x.click());
const banners = (page) => page.$$eval(".notice.err.show", (n) => n.map((x) => x.textContent.trim()).filter(Boolean));
async function sub(page, group, s) {
  await click(page, `[data-suite-group="${group}"]`); await idle(page);
  if (s) { await click(page, `[data-suite-sub="${s}"]`); await idle(page); }
  await sleep(300);
}
const visibleSections = (page) => page.$$eval("#app section[id^=tab-]", (s) => s.filter((x) => x.getClientRects().length).map((x) => x.id));
const call = (key, ua = "codex_cli_rs/0.40.0") => fetch(`${base}/v1/chat/completions`, { method: "POST", headers: { Authorization: "Bearer " + key, "Content-Type": "application/json", "User-Agent": ua },
  body: JSON.stringify({ model: "gpt-5.5", messages: [{ role: "user", content: "hi" }] }) }).then((r) => r.status);
const mgmtGet = (p) => fetch(`${base}/v0/management/plugins/${id}${p}`, { headers: { Authorization: "Bearer " + mgmt } }).then((r) => r.json());

for (const ev of ["uncaughtException", "unhandledRejection"]) process.on(ev, (e) => { console.log(ok.map((x) => "ok   " + x).join("\n")); console.error(failures.map((x) => "FAIL " + x).join("\n")); console.error(e); process.exit(1); });
for (let i = 0; i < 3; i++) await call(apiKey); // real usage for the overview
await sleep(1500);

const EXPECT = {
  overview: { overview: ["tab-overview"] },
  billing: { keys: ["tab-keys"], plans: ["tab-plans"], routes: ["tab-routes"], prices: ["tab-prices"], analysis: ["tab-analysis"] },
  upstream: { "auth-files": ["tab-auth-files"], forecast: ["tab-forecast"] },
  monitor: { clients: ["tab-clients"], "request-events": ["tab-request-events"], errors: ["tab-errors"], ranking: ["tab-ranking"] },
  settings: { modules: ["tab-modules"], "plugin-logs": ["tab-plugin-logs"] }
};
const SOON = []; // shipped alerts and the monitor; nothing is "coming soon" any more
// 1 + 2: admin
{
  const bag = [];
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });
  wire(page, "admin", bag);
  await page.goto(ui, { waitUntil: "networkidle0" });
  await login(page, mgmt);
  const groups = await page.$$eval("[data-suite-group]", (b) => b.map((x) => x.dataset.suiteGroup));
  check(JSON.stringify(groups) === JSON.stringify(Object.keys(EXPECT)), "5 nav groups " + groups.join(","));
  check(await page.$eval("#app .page-tab-switcher", (x) => !x.getClientRects().length), "upstream tab strip hidden");
  for (const [group, subs] of Object.entries(EXPECT)) {
    for (const [s, sections] of Object.entries(subs)) {
      await sub(page, group, s);
      const vis = (await visibleSections(page)).filter((x) => x !== "tab-plugin-logs" || s === "plugin-logs");
      const want = sections.filter((x) => !vis.includes(x));
      const extra = vis.filter((x) => !sections.includes(x));
      check(!want.length && !extra.length, `${group}/${s} shows exactly ${sections} (visible: ${vis})`);
      check(await page.$eval(`[data-suite-sub="${s}"]`, (x) => x.classList.contains("active")), `${group}/${s} pill active`);
      const b = await banners(page);
      check(!b.length, `${group}/${s} no error banner ${JSON.stringify(b)}`);
    }
  }
  await sub(page, "upstream");
  check(!(await page.$('[data-suite-sub="gemini"]')) && !(await page.evaluate(() => /Gemini/.test(document.getElementById("suite-subnav").innerText))), "no Gemini entry");
  for (const s of SOON) {
    const st = await page.evaluate((s) => { const g = { alerts: "monitor", grok: "monitor" }[s];
      document.querySelector(`[data-suite-group="${g}"]`).click(); const b = document.querySelector(`[data-suite-sub="${s}"]`); return b ? { disabled: b.disabled, text: b.textContent } : null; }, s);
    await idle(page);
    check(st && st.disabled && /Coming soon|即将推出/.test(st.text), `"${s}" is a disabled coming-soon entry`);
  }
  // overview: real numbers only
  await sub(page, "overview");
  await page.waitForFunction(() => document.querySelectorAll("#suite-overview .suite-stat").length === 4, { timeout: 10000 }).catch(() => {});
  const ov = await page.$eval("#suite-overview", (x) => x.innerText);
  const real = await mgmtGet("/keys");
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const d0 = new Date(); d0.setHours(0, 0, 0, 0);
  const an = await mgmtGet(`/analysis?from=${d0.toISOString()}&to=${new Date(Date.now() + 60000).toISOString()}&timezone=${tz}`);
  const reqs = an.summary?.requests;
  check(reqs > 0 && ov.includes(String(reqs)), `overview: today's requests match the analysis API (${reqs})`);
  check(ov.includes("gpt-5.5"), "overview: top models lists the real model");
  check(!/mock|demo|示例/i.test(ov), "overview: no mock data");
  check(Array.isArray(real.keys || real) , "keys API reachable");
  if (SHOTS) await page.screenshot({ path: SHOTS + "/overview-light.png", fullPage: true });

  // create plan, assign the key
  await sub(page, "billing", "plans");
  await click(page, "#plan-new"); await page.waitForSelector("#plan-dialog[open]");
  await page.type("#plan-name", "E2E plan");
  if (!(await page.$("#plan-windows .plan-window"))) await click(page, "#plan-window-add");
  const planInputs = await page.evaluate(() => {
    const set = (sel, v) => { for (const x of document.querySelectorAll("#plan-windows " + sel)) { x.value = v; x.dispatchEvent(new Event("input", { bubbles: true })); x.dispatchEvent(new Event("change", { bubbles: true })); } };
    set('[name="window_name"]', "5h"); set('[name="window_period"]', "5"); set('input[name*="request"]', "100");
    return [...document.querySelectorAll("#plan-windows input")].map((x) => x.name);
  });
  await page.$$eval("#plan-key-list input[type=checkbox]", (c) => c.forEach((x) => { if (!x.checked) x.click(); }));
  await Promise.all([idle(page), click(page, "#plan-save")]); await sleep(400);
  const planOpen = await page.$("#plan-dialog[open]");
  if (planOpen) failures.push("plan inputs " + planInputs + " / plan dialog error: " + (await page.$eval("#plan-dialog", (x) => x.innerText.slice(-300))));
  let plans = await mgmtGet("/plans");
  const plan = (plans.plans || plans).find?.((p) => p.name === "E2E plan");
  check(!planOpen && plan, "create plan via Key与计费/套餐");
  // edit plan
  await page.$$eval("#plans-body button", (b) => b.find((x) => /Edit|编辑/.test(x.textContent))?.click());
  await page.waitForSelector("#plan-dialog[open]", { timeout: 3000 }).catch(() => {});
  if (await page.$("#plan-dialog[open]")) {
    await page.$eval("#plan-name", (x) => { x.value = ""; }); await page.type("#plan-name", "E2E plan 2");
    await Promise.all([idle(page), click(page, "#plan-save")]); await sleep(400);
  }
  plans = await mgmtGet("/plans");
  check((plans.plans || plans).some?.((p) => p.name === "E2E plan 2"), "edit plan saved");
  // route
  await sub(page, "billing", "routes");
  await click(page, "#route-new"); await page.waitForSelector("#route-dialog[open]");
  await page.type("#route-name", "E2E route");
  await click(page, "#route-rule-all").catch(() => {});
  await Promise.all([idle(page), click(page, "#route-save")]); await sleep(400);
  const routeOpen = await page.$("#route-dialog[open]");
  if (routeOpen) failures.push("route dialog: " + (await page.$eval("#route-dialog", (x) => x.innerText.slice(-300))));
  const routes = await mgmtGet("/routes");
  check(!routeOpen && JSON.stringify(routes).includes("E2E route"), "create route via Key与计费/路由");
  // price add + edit
  await sub(page, "billing", "prices");
  await click(page, "#price-new"); await page.waitForSelector("#price-dialog[open]");
  await page.type("#price-model", "e2e-model");
  await page.$eval("#price-in", (x) => { x.value = "1.5"; }); await page.$eval("#price-out", (x) => { x.value = "6"; });
  await Promise.all([idle(page), click(page, "#price-save")]); await sleep(400);
  let prices = JSON.stringify(await mgmtGet("/prices"));
  check(prices.includes("e2e-model"), "add price via Key与计费/模型价格");
  await page.$eval("#price-filter", (x) => { x.value = "e2e-model"; x.dispatchEvent(new Event("input", { bubbles: true })); }); await sleep(300);
  await page.$$eval("#prices-body button", (b) => b.find((x) => /Edit|编辑/.test(x.textContent))?.click());
  if (await page.waitForSelector("#price-dialog[open]", { timeout: 3000 }).catch(() => null)) {
    await page.$eval("#price-out", (x) => { x.value = "7.25"; });
    await Promise.all([idle(page), click(page, "#price-save")]); await sleep(400);
  }
  prices = JSON.stringify(await mgmtGet("/prices"));
  check(/7\.25/.test(prices), "edit price saved");
  // reset quota on the key row
  await sub(page, "billing", "keys"); await sleep(500);
  if (SHOTS) await page.screenshot({ path: SHOTS + "/keys-light.png", fullPage: true });
  const reset = await page.$$eval("#keys-body button", (b) => { const x = b.find((x) => /^(Reset|重置)$/.test(x.textContent.trim())); x?.click(); return !!x; });
  if (reset) { await page.waitForSelector("#action-dialog[open]", { timeout: 3000 }).catch(() => {}); await Promise.all([idle(page), click(page, "#action-confirm")]); await sleep(400); }
  const toast = await page.$eval("#notice", (x) => x.textContent).catch(() => "");
  check(reset && /reset|重置/i.test(toast), "reset quota from the key row: " + toast);
  // client rule
  await click(page, ".suite-client-edit");
  await page.waitForSelector("#action-dialog[open] .suite-rule-editor textarea", { timeout: 5000 });
  await page.select("#action-dialog[open] .suite-rule-editor select", "enforce");
  await page.$eval("#action-dialog[open] .suite-rule-editor textarea", (x) => { x.value = "re:^codex[_-]"; });
  await Promise.all([idle(page), click(page, "#action-confirm")]); await sleep(500);
  check((await call(apiKey, "curl/8")) === 403, "client rule added from the key row blocks curl (403)");
  check((await call(apiKey)) === 200, "codex UA still served");
  await sub(page, "monitor", "clients"); await sleep(800);
  check(/curl\/8/.test(await page.$eval("#suite-clients", (x) => x.innerText)), "clients page lists the blocked request");
  // regression: the old preset pattern blocks Codex Desktop; the page flags it and updates it on request.
  const desktopUA = "Codex Desktop/0.162.0-alpha.17.2 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.1007.21434)";
  check((await call(apiKey, desktopUA)) === 403, "old rule re:^codex[_-] blocks Codex Desktop (the reported bug)");
  check(!!(await page.$("[data-suite-outdated]")), "clients page warns about the outdated preset pattern");
  await click(page, "[data-suite-outdated-update]");
  await page.waitForSelector("#action-dialog[open]", { timeout: 3000 });
  await Promise.all([idle(page), click(page, "#action-confirm")]); await sleep(800);
  check(!(await page.$("[data-suite-outdated]")), "warning gone after one-click update");
  check((await call(apiKey, desktopUA)) === 200 && (await call(apiKey, "curl/8")) === 403, "after update: Codex Desktop 200, curl still 403");
  if (SHOTS) {
    await page.screenshot({ path: SHOTS + "/clients-light.png", fullPage: true });
    await sub(page, "settings", "modules"); await page.screenshot({ path: SHOTS + "/modules-light.png", fullPage: true });
    await page.emulateMediaFeatures([{ name: "prefers-color-scheme", value: "dark" }]);
    await page.evaluate(() => document.querySelector('[data-page-action="theme"]')?.click()); await sleep(300);
    if (await page.evaluate(() => document.documentElement.dataset.theme !== "dark")) await page.evaluate(() => document.querySelector('[data-page-action="theme"]')?.click());
    await sleep(300);
    await sub(page, "overview"); await sleep(500); await page.screenshot({ path: SHOTS + "/overview-dark.png", fullPage: true });
    await sub(page, "billing", "keys"); await page.screenshot({ path: SHOTS + "/keys-dark.png", fullPage: true });
  }
  // reload keeps the route (group + sub-part)
  await sub(page, "billing", "prices");
  await page.reload({ waitUntil: "networkidle0" }); await sleep(600);
  check(await page.$eval('[data-suite-sub="prices"]', (x) => x.classList.contains("active")).catch(() => false), "reload keeps Key与计费/模型价格");
  check(!bag.length, "admin: no console errors / 404s " + JSON.stringify(bag));
  await page.close();
}
// 3: account
{
  const bag = [];
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });
  wire(page, "account", bag);
  await page.goto(ui, { waitUntil: "networkidle0" });
  await login(page, apiKey, true);
  const tabs = await page.$$eval("#account-app [data-account-tab]", (b) => b.filter((x) => x.offsetParent).map((x) => x.dataset.accountTab));
  check(tabs.length >= 3, "account tabs " + tabs);
  for (const t of tabs) {
    await Promise.all([idle(page), click(page, `#account-app [data-account-tab="${t}"]`)]); await sleep(300);
    const b = await banners(page);
    check(!b.length, `account/${t} no error banner ${JSON.stringify(b)}`);
  }
  if (SHOTS) await page.screenshot({ path: SHOTS + "/account-light.png", fullPage: true });
  check(!bag.length, "account: no console errors / 404s " + JSON.stringify(bag));
  await page.close();
}
// 4: CPAMP shell simulation at the CPA origin
for (const dark of [false, true]) {
  const bag = [];
  const page = await browser.newPage();
  await page.setViewport({ width: 1340, height: 1000 });
  const enc = new TextEncoder();
  const hash = [...new Uint8Array(await crypto.subtle.digest("SHA-256", enc.encode(apiKey)))].map((b) => b.toString(16).padStart(2, "0")).join("");
  await page.setRequestInterception(true);
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.pathname === "/cpamp-sim") return r.respond({ contentType: "text/html", body: SIM_HTML.replace("__SRC__", ui) });
    if (u.pathname === "/v0/management/api-key-aliases") return r.respond({ contentType: "application/json", body: JSON.stringify({ items: [{ apiKeyHash: hash, alias: "Alice laptop" }] }) });
    r.continue();
  });
  wire(page, dark ? "sim-dark" : "sim", bag);
  await page.goto(`${base}/cpamp-sim${dark ? "?dark" : ""}`, { waitUntil: "networkidle0" });
  const frame = await (await page.waitForSelector("iframe")).contentFrame();
  await frame.waitForSelector("#gate-key", { visible: true, timeout: 10000 }).catch(() => {});
  if (await frame.$("#gate-key:not([hidden])").then((x) => x && x.isVisible())) await login(frame, mgmt);
  await sleep(1200);
  const st = await frame.evaluate(() => ({ host: document.documentElement.dataset.cpampPluginHost, theme: document.documentElement.dataset.theme,
    header: !!document.querySelector("#app .app-header")?.getClientRects().length, nav: document.querySelectorAll("[data-suite-group]").length,
    keys: document.getElementById("keys-body")?.innerText || "" }));
  check(st.host === "true" && st.nav === 5 && !st.header, `sim${dark ? "-dark" : ""}: hosted layout (header hidden, 5 groups)`);
  check(st.theme === (dark ? "dark" : "white"), `sim follows host theme ${st.theme}`);
  check(st.keys.includes("Alice laptop"), "sim: CPAMP alias shown as key name");
  if (SHOTS) {
    await page.screenshot({ path: SHOTS + `/cpamp-sim-keys${dark ? "-dark" : ""}.png` });
    await click(frame, '[data-suite-group="overview"]'); await sleep(1200);
    await page.screenshot({ path: SHOTS + `/cpamp-sim-overview${dark ? "-dark" : ""}.png` });
  }
  check(!bag.length, `sim${dark ? "-dark" : ""}: no console errors / 404s ` + JSON.stringify(bag));
  await page.close();
}
await browser.close();
console.log(ok.map((x) => "ok   " + x).join("\n"));
if (failures.length) { console.error(failures.map((x) => "FAIL " + x).join("\n")); process.exit(1); }
console.log(`SUITE4 UI E2E OK: ${ok.length} checks`);
