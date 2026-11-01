// -test browser checks against a real CPA host:
//  1. plain-http origin: crypto.subtle absent, note shown, no "not saved" toast,
//     session survives reload, a new tab needs a login unless "remember" was ticked;
//  2. secure origin (127.0.0.1): upstream vault still remembers the key;
//  3. embedded in a same-origin parent with CPAMP's enc::v2 store: auto sign-in;
//  4. clients tab + key column + rule editor, then a real request through CPA
//     is refused with 403 client_not_allowed while a codex UA passes the guard.
//   node ui_suite3.mjs <http-base> <secure-base> <plugin-id> <mgmt-key> <api-key>
import puppeteer from "puppeteer-core";
const [httpBase, secureBase, id, mgmt, apiKey] = process.argv.slice(2);
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || "/usr/bin/google-chrome", headless: true, args: ["--no-sandbox"] });
const failures = [], ok = [];
const check = (cond, msg) => (cond ? ok : failures).push(msg);
const ui = (base) => `${base}/v0/resource/plugins/${id}/ui`;
const idle = (page) => page.waitForNetworkIdle({ idleTime: 700 }).catch(() => {});
const signedIn = (page) => page.evaluate(() => !document.getElementById("app").classList.contains("hidden") && document.getElementById("gate").classList.contains("hidden"));
const toasts = (page) => page.evaluate(() => [...document.querySelectorAll(".notice.err.show, #notice.err")].map((n) => n.textContent.trim()).filter(Boolean));

async function login(page, key, remember) {
  await page.waitForSelector("#gate-key", { visible: true });
  await page.type("#gate-key", key);
  if (remember !== (await page.$eval("#gate-remember", (x) => x.checked))) await page.$eval("#gate-remember", (x) => x.click());
  await Promise.all([idle(page), page.click("#gate-submit")]);
  await new Promise((r) => setTimeout(r, 400));
}

// 1. plain http
{
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.goto(ui(httpBase), { waitUntil: "networkidle0" });
  const env = await page.evaluate(() => ({ secure: window.isSecureContext, subtle: !!window.crypto?.subtle, note: !!document.getElementById("suite-login-note") }));
  check(!env.secure && !env.subtle, `http origin has no WebCrypto (secure=${env.secure}, subtle=${env.subtle})`);
  check(env.note, "http login note shown under remember checkbox");
  await login(page, mgmt, false);
  check(await signedIn(page), "http: signed in");
  const t = await toasts(page);
  check(!t.some((x) => /not saved|未保存/.test(x)), "http: no 'session not saved' toast " + JSON.stringify(t));
  await page.reload({ waitUntil: "networkidle0" }); await new Promise((r) => setTimeout(r, 500));
  check(await signedIn(page), "http: session survives reload (session-only default)");
  const tab2 = await ctx.newPage();
  await tab2.goto(ui(httpBase), { waitUntil: "networkidle0" });
  check(!(await signedIn(tab2)), "http: without remember a new tab asks to sign in");
  const stored = await tab2.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith("cpa-suite.credential.v1.")));
  check(stored.length === 0, "http: nothing remembered in localStorage by default");
  await login(tab2, mgmt, true);
  const raw = await tab2.evaluate(() => localStorage.getItem("cpa-suite.credential.v1.admin") || "");
  check(raw.startsWith("cpa-suite-obf:v1:") && !raw.includes(mgmt), "http: remembered key stored obfuscated, not plain");
  const tab3 = await ctx.newPage();
  await tab3.goto(ui(httpBase), { waitUntil: "networkidle0" }); await new Promise((r) => setTimeout(r, 500));
  check(await signedIn(tab3), "http: with remember a new tab signs in automatically");
  await ctx.close();
}
// 2. secure origin keeps the upstream vault
{
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.goto(ui(secureBase), { waitUntil: "networkidle0" });
  check(await page.evaluate(() => !!window.crypto?.subtle && !document.getElementById("suite-login-note")), "secure origin: WebCrypto path, no http note");
  await login(page, mgmt, true);
  const tab2 = await ctx.newPage();
  await tab2.goto(ui(secureBase), { waitUntil: "networkidle0" }); await new Promise((r) => setTimeout(r, 500));
  check(await signedIn(tab2), "secure origin: upstream encrypted vault remembers the key");
  check(await tab2.evaluate(() => !Object.keys(localStorage).some((k) => k.startsWith("cpa-suite.credential"))), "secure origin: fallback store unused");
  await ctx.close();
}
// 3. embedded, CPAMP enc::v2
{
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.goto(`${httpBase}/`, { waitUntil: "networkidle0" }); // any same-origin document can be the parent
  await page.evaluate((key, src) => {
    const secret = new TextEncoder().encode("cli-proxy-api-webui::secure-storage|v2|" + location.host);
    const data = new TextEncoder().encode(JSON.stringify({ state: { apiBase: location.origin, managementKey: key, rememberPassword: true }, version: 0 }));
    let bin = ""; for (let i = 0; i < data.length; i++) bin += String.fromCharCode(data[i] ^ secret[i % secret.length]);
    localStorage.setItem("cli-proxy-auth", "enc::v2::" + btoa(bin));
    document.documentElement.innerHTML = "<body></body>";
    const f = document.createElement("iframe"); f.id = "panel"; f.style.width = "1200px"; f.style.height = "800px";
    f.src = src; document.body.append(f);
  }, mgmt, ui(httpBase));
  const handle = await page.waitForSelector("#panel");
  const frame = await handle.contentFrame();
  await frame.waitForSelector("#app", { timeout: 10000 });
  await frame.waitForFunction(() => !document.getElementById("app").classList.contains("hidden"), { timeout: 10000 }).catch(() => {});
  check(await frame.evaluate(() => window.parent !== window), "embedded: running inside a parent frame");
  check(await signedIn(frame), "embedded: signed in from CPAMP enc::v2 store without a login form");
  await ctx.close();
}
// 4. clients tab, key column, editor, real 403
{
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  const bad = [];
  page.on("response", (r) => { if (r.url().includes("/plugins/") && r.status() >= 400) bad.push(r.status() + " " + r.url()); });
  await page.goto(ui(secureBase), { waitUntil: "networkidle0" });
  await login(page, mgmt, false);
  await page.waitForSelector(".suite-client-summary", { timeout: 10000 }).catch(() => {});
  await new Promise((r) => setTimeout(r, 800));
  const header = await page.$$eval("#keys-body th", (t) => t.map((x) => x.textContent.trim()));
  check(header.some((h) => /Allowed clients|允许客户端/.test(h)), "keys table has the allowed-clients column " + JSON.stringify(header));
  const summary = await page.$eval(".suite-client-summary", (x) => x.textContent.trim()).catch(() => "");
  check(summary && !/Loading|加载/.test(summary), "key row shows a client rule summary: " + summary);
  await page.click(".suite-client-edit");
  await page.waitForSelector("#action-dialog[open] .suite-rule-editor textarea", { timeout: 5000 });
  await page.select("#action-dialog[open] .suite-rule-editor select", "enforce");
  await page.$eval("#action-dialog[open] .suite-rule-editor textarea", (x) => { x.value = "re:^codex[_-]"; });
  await Promise.all([idle(page), page.click("#action-confirm")]);
  await new Promise((r) => setTimeout(r, 500));
  check(!(await page.$("#action-dialog[open]")), "rule editor saved and closed");
  const after = await page.$eval(".suite-client-summary", (x) => x.textContent.trim());
  check(/Enforce|拦截/.test(after) && after.includes("codex"), "key row summary updated: " + after);
  // real requests through CPA
  const call = (ua) => fetch(`${secureBase}/v1/chat/completions`, { method: "POST", headers: { Authorization: "Bearer " + apiKey, "Content-Type": "application/json", "User-Agent": ua },
    body: JSON.stringify({ model: "gpt-5.5", messages: [{ role: "user", content: "hi" }] }) }).then(async (r) => ({ status: r.status, body: await r.text() }));
  const events = () => fetch(`${secureBase}/v0/management/plugins/${id}/events`, { headers: { Authorization: "Bearer " + mgmt } }).then((r) => r.json()).then((v) => v.total);
  const keys = () => fetch(`${secureBase}/v0/management/plugins/${id}/keys`, { headers: { Authorization: "Bearer " + mgmt } }).then((r) => r.json());
  const before = await events();
  const blocked = await call("curl/8.4.0");
  await new Promise((r) => setTimeout(r, 800));
  check((await events()) === before, `blocked request recorded no billing event (${before})`);
  const k = JSON.stringify(await keys());
  check(!/"current_concurrency":[1-9]/.test(k), "blocked request holds no concurrency slot");
  check(blocked.status === 403 && blocked.body.includes("client_not_allowed"), `CPA refuses a curl UA with 403 client_not_allowed (${blocked.status} ${blocked.body.slice(0, 120)})`);
  const passed = await call("codex_cli_rs/0.40.0 (Mac OS 15.0; arm64)");
  check(passed.status === 200 && !passed.body.includes("client_not_allowed"), `codex UA passes the client check and is served (${passed.status})`);
  await new Promise((r) => setTimeout(r, 1500));
  check((await events()) === before + 1, "allowed request is billed once");
  await Promise.all([idle(page), page.evaluate(() => { document.querySelector('[data-suite-group="monitor"]').click(); document.querySelector('[data-suite-sub="clients"]').click(); })]);
  await new Promise((r) => setTimeout(r, 1200));
  const text = await page.$eval("#suite-clients", (x) => x.innerText);
  check(/client-guard 0\.1\.2/.test(text), "clients page shows module status");
  check(text.includes("curl/8.4.0") && /Blocked|已拦截/.test(text), "clients page lists the blocked request");
  check(/Codex/.test(text), "clients page lists presets");
  if (process.env.SHOTS) {
    await page.setViewport({ width: 1400, height: 1000 });
    await page.screenshot({ path: process.env.SHOTS + "/clients.png", fullPage: true });
    await Promise.all([idle(page), page.evaluate(() => document.querySelector('[data-suite-group="billing"]').click())]);
    await page.screenshot({ path: process.env.SHOTS + "/keys.png" });
    await page.click(".suite-client-edit"); await new Promise((r) => setTimeout(r, 800));
    await page.screenshot({ path: process.env.SHOTS + "/editor.png" });
    await page.keyboard.press("Escape");
    await Promise.all([idle(page), page.evaluate(() => { document.querySelector('[data-suite-group="monitor"]').click(); document.querySelector('[data-suite-sub="clients"]').click(); })]);
  }
  const visible = await page.$eval("#tab-clients", (x) => !x.classList.contains("hidden"));
  check(visible, "clients section visible");
  check(!bad.length, "no failed plugin calls: " + bad.join(", "));
  await ctx.close();
}
await browser.close();
console.log(ok.map((x) => "ok   " + x).join("\n"));
if (failures.length) { console.error(failures.map((x) => "FAIL " + x).join("\n")); process.exit(1); }
console.log(`SUITE3 UI E2E OK: ${ok.length} checks`);
