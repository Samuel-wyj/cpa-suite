// Real-browser regression: load the plugin UI from a real CPA host, sign in
// (management key and API key), open every tab, and fail on any plugin API
// call that 404s or targets a plugin ID other than the expected one.
//   node ui_e2e.mjs <base-url> <plugin-id> <management-key> <api-key>
import puppeteer from "puppeteer-core";
const [base, id, mgmt, apiKey] = process.argv.slice(2);
const chrome = process.env.CHROME || "/usr/bin/google-chrome";
const browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ["--no-sandbox"] });
const failures = [], seen = new Set();
async function session(mode, key) {
  const page = await browser.newPage();
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (!u.pathname.startsWith("/v0/")) return;
    const m = u.pathname.match(/^\/v0\/(management|resource)\/plugins\/([^/]+)/);
    seen.add(`${mode} ${r.request().method()} ${u.pathname} ${r.status()}`);
    if (m && m[2] !== id) failures.push(`${mode}: wrong plugin id ${u.pathname}`);
    if (r.status() === 404) failures.push(`${mode}: 404 ${u.pathname}`);
  });
  await page.goto(`${base}/v0/resource/plugins/${id}/ui`, { waitUntil: "networkidle0" });
  await page.waitForSelector("#gate-key", { visible: true });
  if (mode === "account") await page.click("#gate-mode-account");
  await page.type("#gate-key", key);
  await Promise.all([page.waitForNetworkIdle({ idleTime: 800 }), page.click("#gate-submit")]);
  for (const tab of await page.$$eval("[data-tab]", (b) => b.filter((x) => x.offsetParent).map((x) => x.dataset.tab))) {
    await Promise.all([page.waitForNetworkIdle({ idleTime: 800 }), page.click(`[data-tab="${tab}"]`)]);
  }
  for (const tab of await page.$$eval("[data-account-tab]", (b) => b.filter((x) => x.offsetParent).map((x) => x.dataset.accountTab)))
    await Promise.all([page.waitForNetworkIdle({ idleTime: 800 }), page.click(`[data-account-tab="${tab}"]`)]);
  // layout: admin views are reached through groups and sub-tab pills
  for (const group of await page.$$eval("[data-suite-group]", (b) => b.map((x) => x.dataset.suiteGroup))) {
    await Promise.all([page.waitForNetworkIdle({ idleTime: 800 }), page.click(`[data-suite-group="${group}"]`)]);
    for (const sub of await page.$$eval("[data-suite-sub]:not(:disabled)", (b) => b.map((x) => x.dataset.suiteSub)))
      await Promise.all([page.waitForNetworkIdle({ idleTime: 800 }), page.click(`[data-suite-sub="${sub}"]`)]);
  }
  const err = await page.$$eval(".notice.err", (n) => n.map((x) => x.textContent.trim()).filter(Boolean));
  if (err.length) failures.push(`${mode}: error banner: ${err.join(" | ")}`);
  await page.close();
}
await session("admin", mgmt);
await session("account", apiKey);
await browser.close();
const plugin = [...seen].filter((s) => s.includes(`/plugins/${id}`));
console.log([...seen].sort().join("\n"));
if (plugin.length < 8) failures.push(`only ${plugin.length} plugin API calls observed`);
if (failures.length) { console.error("UI E2E FAILED\n" + [...new Set(failures)].join("\n")); process.exit(1); }
console.log(`UI E2E OK: ${plugin.length} plugin API calls, none 404, all under /plugins/${id}`);
