const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");
const { strings } = require("./strings.cjs");
const { reach, pointerClick } = require("./reader.cjs");
async function click(page, locator) {
  const closed = locator.locator('xpath=ancestor::details[not(@open)]');
  for (const detail of await closed.all()) await detail.locator(':scope > summary').click();
  await reach(page, locator); await pointerClick(page, locator);
}
// WebKit can acknowledge a viewport change before delivering resize/layout.
// Measure responsive geometry after that event and its animation frame, not
// while the preceding width's flex layout is still being replaced.
async function resizeWAN(page, width) {
  const same = await page.evaluate(w => innerWidth === w && innerHeight === 900, width);
  if (same) return;
  await page.evaluate(() => {
    window.wanResizeSeen = false;
    addEventListener("resize", () => { wanResizeSeen = true; }, { once: true });
  });
  await page.setViewportSize({ width, height: 900 });
  await page.waitForFunction(w => wanResizeSeen && innerWidth === w && innerHeight === 900, width);
  if (width <= 600) {
    const small = await page.locator("#view-wan button:visible").evaluateAll(es => es.map(e => ({ text: e.textContent.trim(), height: e.getBoundingClientRect().height })).filter(e => e.height < 44));
    assert.deepEqual(small, [], "narrow WAN buttons have at least 44px touch targets");
  }
}
const { I18N } = strings();
const engines = process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"];
const endpoint = "https://wan-fixture.example.invalid";
const assignment = "Use separate WAN keys for different clients. Names are labels, not device binding.";

function wanFixture(lang, events) {
  const ordinary = fixture(lang, "light", events);
  const state = { state: "stopped", endpoint: "", error: "", runtimeAvailable: false, controllable: true, keys: [],
    auth: { state: "signed_out", error: "", userCode: "", verificationURI: "", expiresAt: "0001-01-01T00:00:00Z", attemptID: 0, storage: "memory", storageAvailable: true } };
  let serial = 0, attempt = 0, failure = "", modelFailure = false, modelState;
  const secrets = new Map();
  return {
    state,
    ready() { state.runtimeAvailable = true; this.signedIn(); },
    code() { Object.assign(state.auth, { state: "awaiting_authorization", userCode: "ABCD-EFGH", verificationURI: "https://github.com/login/device", expiresAt: new Date(Date.now() + 900000).toISOString() }); },
    signedIn() { Object.assign(state.auth, { state: "signed_in", userCode: "", verificationURI: "", expiresAt: new Date(Date.now() + 28800000).toISOString(), renewal: { state: "scheduled", nextAttemptAt: new Date(Date.now() + 28500000).toISOString() } }); },
    fail(message) { failure = message; },
    models(value) { modelState = value; modelFailure = false; },
    failModels() { modelFailure = true; },
    route: async (route) => {
      const request = route.request(), path = new URL(request.url()).pathname;
      if (path === "/api/providers" && modelFailure) return route.fulfill({ status: 500, json: { error: "fixture model read failed" } });
      if (path === "/api/providers" && modelState !== undefined) return route.fulfill({ json: modelState });
      if (path === "/api/open") { events.push({ action: "open", body: request.postDataJSON() }); return route.fulfill({ json: {} }); }
      if (!path.startsWith("/api/wan")) return ordinary(route);
      const action = path.slice("/api/wan/".length), body = request.method() === "POST" ? request.postDataJSON() : undefined;
      events.push({ action: "wan:" + (request.method() === "GET" ? "read" : action), body });
      if (failure) { const error = failure; failure = ""; return route.fulfill({ status: 400, json: { error } }); }
      if (request.method() === "GET") return route.fulfill({ json: state });
      if (!state.controllable) return route.fulfill({ status: 409, json: { error: "This window does not own the gateway. WAN controls are unavailable." } });
      let secret = "";
      if (action === "start") { state.state = "starting"; state.error = ""; }
      else if (action === "stop") state.state = "stopping";
      else if (action === "usage/refresh") {}
      else if (action === "tunnel/new") state.tunnel.new = true;
      else if (action === "runtime/install") state.runtimeAvailable = true;
      else if (action === "auth/start") {
        assert.deepEqual(Object.keys(body), body.provider ? ["remember", "provider"] : ["remember"], "only the selected provider and storage choice reach the backend");
        state.auth.provider = body.provider || "github";
        Object.assign(state.auth, { state: "requesting_code", error: "", userCode: "", verificationURI: "", attemptID: ++attempt, storage: body.remember ? "system" : "memory" });
      } else if (action === "auth/cancel") {
        if (state.auth.attemptID === body.attemptID) Object.assign(state.auth, { state: "signed_out", userCode: "", verificationURI: "", storage: "memory" });
      } else if (action === "auth/logout") {
        state.state = "stopped"; state.endpoint = "";
        Object.assign(state.auth, { state: "signed_out", error: "", userCode: "", verificationURI: "", attemptID: 0, storage: "memory" });
      } else if (action.startsWith("keys/")) {
        const key = state.keys.find((k) => k.id === body.key);
        switch (action.slice(5)) {
          case "add-key": {
            if (!body.name.trim()) return route.fulfill({ status: 400, json: { error: "Use a name between 1 and 120 characters" } });
            const id = "wan-" + ++serial;
            secret = "sk-magpie-wan-fixture-" + serial;
            secrets.set(id, secret);
            state.keys.push({ id, scope: "wan", name: body.name, masked: "sk-magpie-wan-â€¦" + serial });
            break;
          }
          case "rename-key": key.name = body.name; break;
          case "off-key": key.off = true; break;
          case "on-key": key.off = false; break;
          case "copy-key": secret = secrets.get(key.id); break;
          case "rotate-key": secret = "sk-magpie-wan-rotated-" + ++serial; secrets.set(key.id, secret); key.masked = "sk-magpie-wan-â€¦rotated-" + serial; break;
          case "remove-key": state.keys = state.keys.filter((k) => k !== key); break;
          default: throw new Error("Unexpected WAN key action " + action);
        }
      } else throw new Error("Unexpected WAN action " + action);
      return route.fulfill({ json: { ...state, secret } });
    },
  };
}

async function open(t, engine, lang, ready = true) {
  const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
  t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 }, reducedMotion: "reduce" });
  page.setDefaultTimeout(10000);
  const events = [], errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const f = wanFixture(lang, events);
  if (ready) f.ready();
  await page.route("**/*", f.route);
  if (process.env.WAN_BREAK_STALE) await page.route("**/plugins/gateway/wan.js*", async (route) => {
    const fs = require("node:fs/promises"), path = require("node:path");
    const source = await fs.readFile(path.resolve(__dirname, "../assets/plugins/gateway/wan.js"), "utf8");
    const broken = source.replace("if (mine !== generation || !visible()) return;", "if (!visible()) return;");
    assert.notEqual(broken, source, "mutation must remove the stale-read barrier");
    await route.fulfill({ contentType: "text/javascript", body: broken });
  });
  await page.addInitScript(() => {
    // Drive the poll's state, never a wall-clock delay. Other app timers run
    // normally. Also keep a record of focus calls for the no-scroll rule.
    window.wanTimers = new Map(); let timerID = -1;
    const later = window.setTimeout, clear = window.clearTimeout;
    window.setTimeout = (fn, ms, ...args) => {
      if (ms === 3000 && fn.name === "refresh") { const id = timerID--; wanTimers.set(id, fn); return id; }
      return later(fn, ms, ...args);
    };
    window.clearTimeout = (id) => { wanTimers.delete(id); return clear(id); };
    window.pollWAN = () => { const pending = [...wanTimers.values()]; wanTimers.clear(); return Promise.all(pending.map((fn) => fn())); };
    window.wanFocus = [];
    const focus = HTMLElement.prototype.focus;
    HTMLElement.prototype.focus = function (opts) {
      if (this.closest("#wanKeys, #modal")) wanFocus.push(opts?.preventScroll === true);
      return focus.call(this, opts);
    };
  });
  const word = (key) => lang === "en" ? key : I18N[lang][key];
  await page.goto("http://magpie.test/?view=wan");
  await page.waitForFunction(() => document.querySelector("#wanStatus")?.dataset.state === "stopped");
  return { page, f, word, events, errors };
}

for (const engine of engines) for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
  test(`${engine} ${lang}: WAN state, setup, endpoint and independent named key controls`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, lang);
    for (const key of ["WAN Gateway", "Start", "Stop", "WAN stopped", "WAN running", "WAN keys", assignment]) assert.equal(typeof word(key), "string", key);
    assert.equal(await page.locator("#pageTitle").textContent(), word("WAN Gateway"));
    assert.equal(await page.locator(".wan-assignment").textContent(), word(assignment));
    assert.equal(await page.locator("#wanAuthState").textContent(), word("Signed in to GitHub for WAN"));
    assert.equal(await page.locator("#wanRuntimeState").textContent(), word("WAN runtime installed"));
    assert.equal(await page.locator("#wanRuntimeDetails").isVisible(), false, "installed runtime leaves no permanent dashboard card");
    assert.equal(await page.locator("#wanStart").isEnabled(), true, "explicit sign-in permits Start to check the runtime locally");
    assert.equal(await page.locator("#wanStop").isDisabled(), true);
    assert.match(await page.locator("#wanClaudeExample").textContent(), /YOUR_WAN_ENDPOINT/);
    assert.match(await page.locator("#wanClaudeExample").textContent(), /YOUR_WAN_KEY/);
    assert.equal(await page.locator("#wanEndpoint .copy").count(), 0);
    assert.equal(events.some((e) => e.action === "wan:start"), false, "opening never publishes a tunnel");
    f.fail("WAN SDK operation failed; start WAN again explicitly");
    await click(page, page.locator("#wanStart"));
    await page.waitForFunction((message) => document.querySelector("#wanProblem").textContent === message, word("WAN SDK operation failed; start WAN again explicitly"));
    assert.equal(await page.locator("#wanStart").isEnabled(), true);
    await click(page, page.locator("#wanStart"));
    await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "starting");
    assert.equal(await page.locator("#wanStart").isDisabled(), true);
    assert.equal(await page.locator("#wanStop").isEnabled(), true, "starting can be canceled");
    await page.waitForFunction(() => document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    f.state.state = "running"; f.state.endpoint = endpoint;
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanState").textContent(), word("WAN running"));
    assert((await page.locator("#wanClaudeExample").textContent()).includes(endpoint), "Claude example follows the live endpoint");
    await click(page, page.locator("#wanEndpoint .copy"));
    await page.waitForFunction(() => document.querySelector("#wanEndpoint .copy.done"));
    assert(events.some((e) => e.action === "clipboard" && e.body.text === endpoint));
    await click(page, page.locator("#wanStop"));
    await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "stopping");
    assert.equal(await page.locator("#wanStop").isDisabled(), true);
    assert.equal(f.state.auth.state, "signed_in", "Stop does not sign out");
    assert.equal(events.some((e) => e.action === "wan:auth/logout"), false);
    assert.equal(await page.locator("#wanEndpoint .copy").count(), 0, "stale endpoint isn't offered as live");
    await page.waitForFunction(() => document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    f.state.state = "failed"; f.state.error = "WAN SDK operation failed; start WAN again explicitly";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanState").textContent(), word("WAN failed"));
    assert.equal(await page.locator("#wanProblem").isVisible(), true);
    assert.equal(await page.locator("#wanStart").isEnabled(), true);

    await click(page, page.locator("#addWANKey"));
    await click(page, page.locator("#wanKeys .adding .primary"));
    await page.waitForFunction(() => document.querySelector("#wanKeys .adding .primary")?.disabled === false);
    await page.locator("#wanKeys .adding input").fill("Laptop");
    await page.locator("#wanKeys .adding input").press("Enter");
    let row = page.locator('#wanKeys [data-key="wan-1"]');
    await row.waitFor();
    assert.equal(await page.locator("#wanKeys .adding").count(), 0);
    await click(page, row.locator(".copy"));
    await page.waitForFunction(() => document.querySelector("#wanKeys .copy.done"));
    assert(events.some((e) => e.action === "clipboard" && e.body.text === "sk-magpie-wan-fixture-1"));
    assert.equal(await page.locator("#view-wan").textContent().then((s) => s.includes("sk-magpie-wan-fixture-1")), false);
    await click(page, row.locator(".rename"));
    await page.locator("#wanKeys .rename-in").fill("Work laptop");
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanKeys .rename-in").inputValue(), "Work laptop", "poll preserves the name being typed");
    await page.locator("#wanKeys .rename-in").press("Enter");
    await row.getByRole("button", { name: "Work laptop", exact: true }).waitFor();
    f.fail("Failed to save key");
    await click(page, row.locator(".tick"));
    await page.waitForFunction(() => document.querySelector("#wanProblem").textContent === "Failed to save key");
    assert.equal(await row.getAttribute("class").then((s) => s.includes("in-use")), true, "a failed off leaves the key on");
    await click(page, row.locator(".tick"));
    await page.waitForFunction(() => document.querySelector('#wanKeys [data-key="wan-1"]').classList.contains("off"));
    await click(page, row.locator(".tick"));
    await page.waitForFunction(() => !document.querySelector('#wanKeys [data-key="wan-1"]').classList.contains("off"));
    await click(page, page.locator("#addWANKey"));
    await page.locator("#wanKeys .adding input").fill("Phone");
    await page.locator("#wanKeys .adding input").press("Enter");
    await page.locator('#wanKeys [data-key="wan-2"]').waitFor();
    const rotate = word("Rotate key"), remove = word("Remove");
    await click(page, row.locator(".wan-key-more"));
    await click(page, page.locator(".wan-key-menu").getByRole("menuitemradio", { name: rotate, exact: true }));
    await page.keyboard.press("Escape");
    await page.locator("#modal").waitFor({ state: "hidden" });
    assert.equal(events.some((e) => e.action === "wan:keys/rotate-key"), false);
    await click(page, row.locator(".wan-key-more"));
    await click(page, page.locator(".wan-key-menu").getByRole("menuitemradio", { name: rotate, exact: true }));
    await click(page, page.locator("#modal").getByRole("button", { name: rotate, exact: true }));
    await page.locator("#modal").waitFor({ state: "hidden" });
    assert.match(await row.locator(".plan").textContent(), /rotated/);
    assert.equal(await page.locator('#wanKeys [data-key="wan-2"] .plan').textContent(), "sk-magpie-wan-â€¦2");
    await click(page, row.locator(".wan-key-more"));
    await click(page, page.locator(".wan-key-menu").getByRole("menuitemradio", { name: remove, exact: true }));
    await click(page, page.locator("#modal").getByRole("button", { name: word("Cancel"), exact: true }));
    await page.locator("#modal").waitFor({ state: "hidden" });
    assert.equal(events.some((e) => e.action === "wan:keys/remove-key"), false);
    await click(page, row.locator(".wan-key-more"));
    await click(page, page.locator(".wan-key-menu").getByRole("menuitemradio", { name: remove, exact: true }));
    await click(page, page.locator("#modal").getByRole("button", { name: remove, exact: true }));
    await page.locator("#modal").waitFor({ state: "hidden" });
    assert.equal(await row.count(), 0);
    assert.equal(await page.locator("#wanKeys [data-key]").count(), 1);
    assert.equal(events.some((e) => /^wan:keys\//.test(e.action) && ("secret" in e.body || "device" in e.body || "ip" in e.body)), false);

    f.state.controllable = false; f.state.state = "unknown"; f.state.error = "";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanOwnerNote").isVisible(), true);
    assert.equal(await page.locator("#wanStart").isDisabled(), true);
    assert.equal(await page.locator("#addWANKey").isDisabled(), true);
    assert.equal(await page.locator("#wanKeys button:enabled").count(), 0);
    for (const width of [1100, 420, 360]) {
      await resizeWAN(page, width);
      const size = await page.locator("#view-wan").evaluate((e) => ({ own: e.scrollWidth - e.clientWidth, doc: document.documentElement.scrollWidth - innerWidth }));
      assert(size.own <= 1 && size.doc <= 1, JSON.stringify({ lang, width, ...size }));
    }
    assert.equal(await page.locator("#view-wan select").count(), 0);
    assert.equal(await page.evaluate(() => wanFocus.every(Boolean)), true, "all explicit focus prevents scrolling");
    assert.deepEqual(errors, []);
  });
}

for (const engine of engines) for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
  test(`${engine} ${lang}: WAN setup uses explicit install and service-app device authorization`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, lang, false);
    assert.equal(await page.locator("#wanStart").isDisabled(), true);
    assert.equal(await page.locator("#wanAuthState").textContent(), word("Signed out of GitHub for WAN"));
    assert.equal(await page.locator("#wanRuntimeDetails").isVisible(), true);
    assert.equal(await page.locator("#wanRuntimeDetails").getAttribute("role"), "alert");
    assert(await page.locator("#wanRuntimeDetails").evaluate(e => !!(e.compareDocumentPosition(document.querySelector("#wanStatus")) & Node.DOCUMENT_POSITION_FOLLOWING)), "required runtime warning precedes connection controls");
    assert.equal(await page.locator("#wanRemember").isChecked(), false, "memory-only is the default");
    assert.equal(await page.locator(".wan-key-head").isVisible(), false, "no empty table header before the first key");
    assert.equal(await page.locator("#wanMemory").getAttribute("tabindex"), "0");
    assert.equal(await page.locator('#view-wan input[type="text"]').count(), 0, "no client-ID or OAuth-app setting");
    assert.equal(events.some((e) => e.action !== "wan:read" && e.action.startsWith("wan:")), false, "GET status has no setup or publication side effect");
    assert.equal(events.some((e) => e.action === "open"), false);
    await click(page, page.locator("#wanInstall"));
    await page.waitForFunction(() => document.querySelector("#wanInstall").hidden && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    assert.equal(await page.locator("#wanStart").isDisabled(), true, "install does not sign in or start");
    assert.equal(await page.locator("#wanRuntimeDetails").isVisible(), false, "successful install removes the prerequisite warning");
    assert.equal(await page.locator("#wanRuntimeHelp").isVisible(), false);
    await page.locator("#wanMemory").focus();
    await page.keyboard.press("ArrowRight");
    assert.equal(await page.locator("#wanRemember").evaluate(e => e === document.activeElement), true);
    await page.keyboard.press("Home");
    assert.equal(await page.locator("#wanMemory").isChecked(), true);
    await click(page, page.locator("#wanRemember"));
    assert.equal(await page.locator("#wanStorage").textContent(), word("Restore sign-in after restarting magpie using this computer’s secure storage."));
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRemember").isChecked(), true, "poll preserves the explicit preference");
    await click(page, page.locator("#wanLogin"));
    await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "requesting_code" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    assert.deepEqual(events.find((e) => e.action === "wan:auth/start").body, { remember: true });
    assert.equal(await page.locator("#wanCancelAuth").isEnabled(), true);
    f.code();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanUserCode").textContent(), "ABCD-EFGH");
    assert.equal(await page.locator("#wanVerificationURI").textContent(), "https://github.com/login/device");
    await Promise.all([
      page.waitForResponse(r => new URL(r.url()).pathname === "/api/copy"),
      click(page, page.locator("#wanCopyCode")),
    ]);
    assert(events.some(e => e.action === "clipboard" && e.body.text === "ABCD-EFGH"));
    assert.equal(await page.locator("#wanDeviceValidity").isVisible(), true);
    assert.equal(await page.locator("#wanCredentialDetails").isVisible(), false, "pending authorization does not show signed-in management");
    assert.equal(events.some((e) => e.action === "open"), false, "neither starting nor polling opens a browser");
    // A poll does not replace the code, cancel button or focused key form.
    const code = await page.locator("#wanUserCode").elementHandle();
    const cancel = await page.locator("#wanCancelAuth").elementHandle();
    await page.evaluate(() => pollWAN());
    assert.equal(await code.evaluate((e) => e === document.querySelector("#wanUserCode")), true);
    assert.equal(await cancel.evaluate((e) => e === document.querySelector("#wanCancelAuth")), true);
    await click(page, page.locator("#wanOpenGitHub"));
    await page.waitForFunction(() => document.querySelector("#wanOpenGitHub")?.disabled === false);
    assert.deepEqual(events.filter((e) => e.action === "open").map((e) => e.body), [{ url: "https://github.com/login/device" }]);
    f.signedIn();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanAuthState").textContent(), word("Signed in to GitHub for WAN"));
    assert.equal(await page.locator("#wanUserCode").textContent(), "");
    assert.equal(await page.locator("#wanCopyCode").isDisabled(), true, "completed codes cannot be copied again");
    assert.equal(await page.locator("#wanCancelAuth").count(), 0);
    assert.equal(await page.locator("#wanStart").isEnabled(), true);
    assert.equal(events.some((e) => e.action === "wan:start"), false, "authorization never hosts a tunnel");
    assert.equal(await page.locator("#wanStorage").textContent(), word("WAN sign-in uses this computer's system secure storage."));
    f.state.auth.state = "expired"; f.state.auth.error = "WAN sign-in expired; sign in again";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanAuthState").textContent(), word("WAN sign-in expired"));
    await click(page, page.locator("#wanMemory"));
    assert.equal(await page.locator("#wanStorage").textContent(), word("Sign-in clears when magpie exits. Nothing is saved in the browser."), "reauthorization help describes the new selection, not the old saved grant");
    assert.equal(await page.locator("#wanStart").isDisabled(), true);
    assert.equal(await page.locator("#wanLogin").isEnabled(), true);
    await click(page, page.locator("#wanLogout"));
    await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_out" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    f.state.auth.storageAvailable = false;
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRemember").isDisabled(), true);
    assert.equal(await page.locator("#wanRemember").isChecked(), false);
    assert.equal(await page.locator("#wanStorage").textContent(), word("System secure storage is unavailable. Sign-in stays in memory until magpie exits."));
    await click(page, page.locator("#wanLogin"));
    await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "requesting_code" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    assert.deepEqual(events.filter((e) => e.action === "wan:auth/start").at(-1).body, { remember: false });
    const attemptID = f.state.auth.attemptID;
    await click(page, page.locator("#wanCancelAuth"));
    await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_out" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    assert.deepEqual(events.find((e) => e.action === "wan:auth/cancel").body, { attemptID });
    f.state.auth.state = "error"; f.state.auth.expiresAt = "0001-01-01T00:00:00Z";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanCredentialDetails").isVisible(), false, "no empty credential disclosure when validity is unknown");
    assert.equal(await page.locator("#wanLogout").isVisible(), true, "an errored session can still be cleared");
    assert.equal(events.some((e) => e.action === "wan:start"), false);
    for (const width of [1100, 420, 360]) {
      await resizeWAN(page, width);
      const size = await page.locator("#view-wan").evaluate((e) => ({ own: e.scrollWidth - e.clientWidth, doc: document.documentElement.scrollWidth - innerWidth }));
      assert(size.own <= 1 && size.doc <= 1, JSON.stringify({ lang, width, ...size }));
    }
    assert.deepEqual(errors, []);
  });
}

for (const engine of engines) for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
  test(`${engine} ${lang}: WAN credential validity stays visible and follows renewal`, async (t) => {
    const { page, f, word, errors } = await open(t, engine, lang);
    const now = Date.parse("2026-10-22T10:00:00Z");
    await page.evaluate((now) => { Date.now = () => now; }, now);
    f.signedIn();
    f.state.auth.expiresAt = new Date(now + 125 * 60000).toISOString();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("Automatic WAN renewal active"));
    assert.equal(await page.locator("#wanAuthExpiresAt").isVisible(), true, "the current credential deadline stays visible without opening a disclosure");
    const format = (key, values) => word(key).replace(/\{(\w+)\}/g, (_, k) => values[k]);
    assert.equal(await page.locator("#wanAuthRemaining").textContent(), format("Expires in {hours}h {minutes}m", { hours: 2, minutes: 5 }));
    assert.equal(await page.locator("#wanAuthExpiresAt").getAttribute("datetime"), f.state.auth.expiresAt);
    assert.match(await page.locator("#wanAuthExpiresAt").textContent(), /2026/);
    assert.equal(await page.locator("#wanStorageMode").textContent(), word("Until magpie exits"));
    assert.equal(await page.locator("#wanStorage").isVisible(), false, "signed-in card avoids the generic Remember instructions");
    f.state.auth.storage = "system";
    f.state.auth.expiresAt = new Date(now + 7 * 60000).toISOString();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanAuthRemaining").textContent(), format("Expires in {minutes}m", { minutes: 7 }));
    assert.equal(await page.locator("#wanStorageMode").textContent(), word("Saved on this computer"));
    // A renewed deadline replaces both the countdown and absolute date.
    f.state.auth.expiresAt = new Date(now + 8 * 3600000).toISOString();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanAuthRemaining").textContent(), format("Expires in {hours}h {minutes}m", { hours: 8, minutes: 0 }));
    assert.equal(await page.locator("#wanAuthExpiresAt").getAttribute("datetime"), f.state.auth.expiresAt);
    for (const theme of ["light", "dark"]) for (const width of [1100, 420, 360]) {
      await page.evaluate(theme => document.documentElement.dataset.theme = theme, theme);
      await resizeWAN(page, width);
      const card = page.locator(".wan-setup-card").filter({ has: page.locator("#wanAuth") });
      const bounds = await card.evaluate(e => {
        const box = e.getBoundingClientRect(), parts = ["wanAuthRemaining", "wanAuthExpiresAt", "wanLogout"].map(id => document.getElementById(id).getBoundingClientRect());
        return { overflow: e.scrollWidth - e.clientWidth, inside: parts.every(p => p.left >= box.left && p.right <= box.right), size: parseFloat(getComputedStyle(document.getElementById("wanAuthRemaining")).fontSize) };
      });
      assert(bounds.overflow <= 1 && bounds.inside && bounds.size <= 14, JSON.stringify({ theme, width, ...bounds }));
    }
    for (const value of ["0001-01-01T00:00:00Z", "invalid"]) {
      f.state.auth.expiresAt = value;
      await page.evaluate(() => pollWAN());
      assert.equal(await page.locator("#wanAuthRemaining").textContent(), word("Expiry not reported"));
      assert.equal(await page.locator("#wanAuthExpiresAt").isVisible(), false);
    }
    f.state.auth.expiresAt = new Date(now - 1000).toISOString();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanAuthRemaining").textContent(), word("WAN credential expired"));
    assert.deepEqual(errors, []);
  });
}

for (const engine of engines) for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
  test(`${engine} ${lang}: WAN health reflects scheduled, retrying and refused renewal`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, lang);
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("Automatic WAN renewal active"));
    assert.equal(await page.locator("#wanRenewalNextRow").isVisible(), true);
    assert.equal(await page.locator("#wanRenewalNext").getAttribute("datetime"), f.state.auth.renewal.nextAttemptAt);
    f.state.auth.renewal = { state: "retrying", nextAttemptAt: new Date(Date.now() + 30000).toISOString() };
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("Retrying WAN renewal"));
    assert.equal(await page.locator("#wanRenewalNextLabel").textContent(), word("Next WAN retry"));
    assert.equal(await page.locator("#wanRenewalHelp").textContent(), word("Renewal will retry automatically. The current credential is still valid."));
    f.state.auth.state = "loading"; f.state.auth.expiresAt = new Date(Date.now() - 1000).toISOString();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalHelp").textContent(), word("Access is unavailable until renewal succeeds. Start WAN again after recovery."));
    assert.equal(await page.locator("#wanStart").isDisabled(), true);
    f.state.auth.renewal = { state: "refreshing" };
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("Renewing WAN credentials…"));
    assert.equal(await page.locator("#wanRenewalNextRow").isVisible(), false);
    f.state.auth.state = "error"; f.state.auth.renewal = { state: "required" };
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("Sign in to WAN again"));
    assert.equal(await page.locator("#wanLogin").isEnabled(), true);
    f.signedIn(); delete f.state.auth.renewal;
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("Automatic WAN renewal unavailable"));
    f.state.auth.expiresAt = "0001-01-01T00:00:00Z";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewalState").textContent(), word("No fixed WAN credential expiry"));
    f.fail("offline fixture status"); await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanRenewal").isVisible(), false, "unknown status cannot keep a healthy renewal badge");
    assert.equal(events.some(e => e.action === "wan:start" || e.action === "wan:auth/start"), false);
    assert.deepEqual(errors, []);
  });

  test(`${engine} ${lang}: WAN client examples choose model, API and shell without reading a secret`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, lang);
    await page.locator("#wanPickModel").waitFor({ state: "visible" });
    await page.waitForFunction(() => !document.querySelector("#wanPickModel").disabled);
    await click(page, page.locator("#wanPickModel"));
    await click(page, page.locator(".wan-model-menu .pm-item").filter({ hasText: /relay\/m$/ }));
    assert.equal(await page.locator("#wanCopyExample").isDisabled(), true, "no live endpoint while WAN is stopped");
    f.state.state = "running"; f.state.endpoint = endpoint;
    await page.evaluate(() => pollWAN());
    await click(page, page.locator("#wanPowerShell"));
    assert.match(await page.locator("#wanClaudeExample").textContent(), /\$env:ANTHROPIC_BASE_URL=/);
    assert.match(await page.locator("#wanClaudeExample").textContent(), /claude --model 'relay\/m'/);
    await Promise.all([page.waitForResponse(r => new URL(r.url()).pathname === "/api/copy"), click(page, page.locator("#wanCopyExample"))]);
    assert(events.some(e => e.action === "clipboard" && e.body.text.includes("YOUR_WAN_KEY") && e.body.text.includes("relay/m")));
    await click(page, page.locator("#wanBash"));
    assert.match(await page.locator("#wanClaudeExample").textContent(), /^export ANTHROPIC_BASE_URL=/);
    await click(page, page.locator("#wanClientOpenAI"));
    assert.equal(await page.locator("#wanEndpoint code").textContent(), endpoint + "/v1");
    assert((await page.locator("#wanClaudeExample").textContent()).includes(endpoint + "/v1/chat/completions"));
    assert.match(await page.locator("#wanClaudeExample").textContent(), /"model":"relay\/m"/);
    await Promise.all([page.waitForResponse(r => new URL(r.url()).pathname === "/api/copy"), click(page, page.locator("#wanEndpoint .copy"))]);
    assert(events.some(e => e.action === "clipboard" && e.body.text === endpoint + "/v1"));
    await click(page, page.locator("#wanPowerShell"));
    assert.match(await page.locator("#wanClaudeExample").textContent(), /^Invoke-RestMethod /);
    assert.equal(events.some(e => e.action === "wan:keys/copy-key"), false, "snippets never fetch a real key implicitly");
    f.models({ gateway: { groups: [] }, providers: [{ id: "relay", name: "Relay", off: true, models: [{ id: "m", name: "Model", on: true }] }] });
    await page.evaluate(() => pollWAN());
    await page.waitForFunction(() => document.querySelector("#wanPickModel").disabled);
    assert.equal(await page.locator("#wanCopyExample").isDisabled(), true, "disabled provider cannot remain selected");
    f.failModels(); await page.evaluate(() => pollWAN());
    await page.waitForFunction(message => document.querySelector("#wanModelHelp").textContent === message, word("Unable to load WAN models. Other WAN controls remain available."));
    assert.equal(await page.locator("#wanStop").isEnabled(), true);
    assert.deepEqual(errors, []);
  });
}

for (const engine of engines) test(`${engine}: held model reads do not block Stop and late hidden reads cannot update the picker`, async t => {
  const { page, f, events, errors } = await open(t, engine, "en");
  await page.waitForFunction(() => !document.querySelector("#wanPickModel").disabled);
  let held, signal;
  const entered = new Promise(resolve => { signal = resolve; });
  await page.route("**/api/providers", route => { if (!held) { held = route; signal(); } else return route.fallback(); });
  f.state.state = "running"; f.state.endpoint = endpoint;
  await page.evaluate(() => pollWAN()); await entered;
  await click(page, page.locator("#wanStop"));
  await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "stopping");
  assert(events.some(e => e.action === "wan:stop"), "model loading cannot delay Stop");
  await page.evaluate(() => { document.querySelector("#view-wan").hidden = true; });
  await page.waitForFunction(() => wanTimers.size === 0);
  await held.fulfill({ json: { gateway: { groups: [] }, providers: [{ id: "stale", models: [{ id: "late", name: "Late", on: true }] }] } }).catch(() => {});
  await page.evaluate(() => { document.querySelector("#view-wan").hidden = false; });
  await page.waitForFunction(() => !document.querySelector("#wanPickModel").disabled);
  await click(page, page.locator("#wanPickModel"));
  assert.equal(await page.locator(".wan-model-menu").textContent().then(s => s.includes("stale/late")), false);
  await page.keyboard.press("Escape");
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: WAN key menus close on unknown keys and Escape preserves focus without scrolling`, async t => {
  const { page, f, events, errors } = await open(t, engine, "en");
  f.state.keys = [{ id: "wan-menu", name: "Work laptop", masked: "sk-magpie-wan-••••menu" }];
  await page.evaluate(() => pollWAN());
  const more = page.locator("#wanKeys .wan-key-more");
  await click(page, more);
  await page.keyboard.press("Escape");
  assert.equal(await page.locator(".wan-key-menu").count(), 0);
  assert.equal(await more.evaluate(e => e === document.activeElement), true);
  assert.equal(await page.evaluate(() => wanFocus.every(Boolean)), true);
  await click(page, more);
  f.state.keysError = "fixture key read failed";
  await page.evaluate(() => pollWAN());
  assert.equal(await page.locator(".wan-key-menu").count(), 0);
  assert.equal(await more.isDisabled(), true);
  assert.equal(events.some(e => e.action === "wan:keys/remove-key" || e.action === "wan:keys/rotate-key"), false);
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: cached unavailable runtime does not prevent a signed-in user's explicit Start`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en", false);
  f.signedIn();
  await page.evaluate(() => pollWAN());
  assert.equal(f.state.runtimeAvailable, false);
  assert.equal(await page.locator("#wanInstall").isVisible(), true, "explicit installation remains offered for cached absence");
  assert.equal(await page.locator("#wanInstall").isEnabled(), true);
  assert.equal(await page.locator("#wanStart").isEnabled(), true, "the backend may discover a runtime installed externally since ownership began");
  f.fail("WAN runtime is unavailable; install the runtime before starting");
  await click(page, page.locator("#wanStart"));
  await page.waitForFunction(() => document.querySelector("#wanProblem").textContent === "WAN runtime is unavailable; install the runtime before starting" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
  assert.equal(await page.locator("#wanStart").isEnabled(), true, "a failed lookup leaves an explicit retry available");
  assert.equal(events.some((e) => e.action === "wan:runtime/install"), false, "Start never requests a runtime download");
  await click(page, page.locator("#wanStart"));
  await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "starting" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
  assert.equal(events.filter((e) => e.action === "wan:start").length, 2);
  assert.equal(events.some((e) => e.action === "wan:runtime/install" || e.action === "wan:auth/start"), false);
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: WAN model read errors stay unknown and a completed key copy respects returned locks`, async t => {
  const { page, f, errors } = await open(t, engine, "en");
  await page.waitForFunction(() => !document.querySelector("#wanPickModel").disabled);
  f.state.state = "running"; f.state.endpoint = endpoint;
  f.state.keys = [{ id: "wan-lock", name: "Laptop", masked: "sk-magpie-wan-••••lock" }];
  f.models({ gateway: { groups: [] }, providers: [{ id: "bad", models: null }] });
  await page.evaluate(() => pollWAN());
  await page.waitForFunction(() => document.querySelector("#wanModelHelp").textContent === "Unable to load WAN models. Other WAN controls remain available.");
  assert.equal(await page.locator("#wanPickModel").isDisabled(), true);
  assert.equal(await page.locator("#wanStop").isEnabled(), true);
  await page.route("**/api/wan/keys/copy-key", route => {
    f.state.keysError = "fixture unknown keys";
    return route.fulfill({ json: { ...f.state, secret: "sk-fixture-copied-explicitly" } });
  });
  await page.evaluate(() => {
    const button = document.querySelector("#wanKeys .copy"), handler = button.onclick;
    window.wanCopyFinished = false;
    button.onclick = async event => { try { await handler(event); } finally { wanCopyFinished = true; } };
  });
  await click(page, page.locator("#wanKeys .copy"));
  await page.waitForFunction(() => wanCopyFinished);
  assert.equal(await page.locator("#wanKeys button:enabled").count(), 0, "copy completion cannot re-enable an unknown key store");
  assert.equal((await page.locator("#view-wan").textContent()).includes("sk-fixture-copied-explicitly"), false);
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: a held WAN model response cannot render into the kernel error boundary`, async t => {
  const { page, errors } = await open(t, engine, "en");
  await page.waitForFunction(() => !document.querySelector("#wanPickModel").disabled);
  await page.evaluate(() => {
    const fetch0 = window.fetch;
    window.modelHeld = false; window.modelParsed = false;
    window.fetch = async function (...args) {
      const response = await fetch0.apply(this, args);
      if (String(args[0]) !== "/api/providers" || modelHeld) return response;
      const body = await response.text(); modelHeld = true;
      await new Promise(resolve => { window.releaseModel = resolve; });
      const held = new Response(body, { status: response.status, headers: response.headers });
      const text = held.text.bind(held);
      held.text = async () => { const value = await text(); modelParsed = true; return value; };
      return held;
    };
  });
  t.after(() => page.evaluate(() => window.releaseModel?.()).catch(() => {}));
  await page.evaluate(() => pollWAN());
  await page.waitForFunction(() => modelHeld);
  await page.evaluate(() => {
    Object.defineProperty(document.querySelector("#wanState"), "textContent", {
      configurable: true, set() { throw Error("fixture WAN boundary"); },
    });
    magpie.call("wan", "relang");
  });
  const broken = page.locator("#view-wan .slot-error");
  await broken.waitFor({ state: "attached" });
  await page.evaluate(() => releaseModel());
  await page.waitForFunction(() => modelParsed);
  assert.deepEqual(errors, [], "late model parsing must leave the error boundary intact");
  await click(page, broken.locator(".slot-again"));
  await page.waitForFunction(() => document.querySelector("#wanStatus")?.dataset.state === "stopped");
  await page.evaluate(() => pollWAN());
  await page.waitForFunction(() => !document.querySelector("#wanPickModel").disabled);
  assert.deepEqual(errors, []);
});

for (const engine of engines) for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
  test(`${engine} ${lang}: unreadable WAN keys lock key writes without blocking Stop or sign out`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, lang);
    const keysError = "WAN keys could not be read; key changes are unavailable";
    // Keep the user's actual input node and draft through the failed read,
    // including a keyboard submission delivered to the now-locked form.
    await click(page, page.locator("#addWANKey"));
    await page.locator("#wanKeys .adding input").fill("Keep this device name");
    const name = await page.locator("#wanKeys .adding input").elementHandle();
    f.state.keys = null; f.state.keysError = keysError;
    f.state.state = "running"; f.state.endpoint = endpoint;
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "running", "a key read error is not a failed runtime status read");
    assert.equal(await page.locator("#wanKeysProblem").textContent(), word(keysError));
    assert.equal(await page.locator("#wanKeysProblem").isVisible(), true);
    assert.equal(await name.evaluate((e) => e === document.querySelector("#wanKeys .adding input")), true);
    assert.equal(await page.locator("#wanKeys .adding input").inputValue(), "Keep this device name");
    assert.equal(await page.locator("#wanKeys .adding input").isDisabled(), true);
    assert.equal(await page.locator("#wanKeys button:enabled").count(), 0);
    assert.equal(await page.locator("#addWANKey").isDisabled(), true);
    assert.equal(await page.locator("#wanKeys .none").count(), 0, "unknown keys never claim an empty store");
    await name.evaluate((e) => e.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true })));
    assert.equal(events.some((e) => e.action.startsWith("wan:keys/")), false, "the action guard also rejects a queued keyboard submission");
    assert.equal(await page.locator("#wanStop").isEnabled(), true);
    assert.equal(await page.locator("#wanLogout").isEnabled(), true);
    await click(page, page.locator("#wanStop"));
    await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "stopping" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    assert.equal(events.filter((e) => e.action === "wan:stop").length, 1);
    assert.equal(await page.locator("#wanLogout").isEnabled(), true, "the key error persists without disabling sign out");
    await click(page, page.locator("#wanLogout"));
    await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_out" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
    assert.equal(events.filter((e) => e.action === "wan:auth/logout").length, 1);
    f.signedIn();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanStart").isDisabled(), true, "unknown key state cannot start publication");
    delete f.state.keysError; f.state.keys = [];
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanKeysProblem").isHidden(), true);
    assert.equal(await name.evaluate((e) => e === document.querySelector("#wanKeys .adding input")), true);
    assert.equal(await page.locator("#wanKeys .adding input").inputValue(), "Keep this device name");
    assert.equal(await page.locator("#wanKeys .adding input").isEnabled(), true, "a successful read unlocks the existing form");
    assert.equal(await page.locator("#wanStart").isEnabled(), true);
    await page.locator("#wanKeys .adding input").press("Enter");
    await page.locator('#wanKeys [data-key="wan-1"]').waitFor();
    assert.equal(events.filter((e) => e.action === "wan:keys/add-key").length, 1);
    // A failed read also must not discard a known row or replace it with the
    // No WAN keys message. The next successful empty read is authoritative.
    const row = await page.locator('#wanKeys [data-key="wan-1"]').elementHandle();
    f.state.keys = null; f.state.keysError = keysError;
    await page.evaluate(() => pollWAN());
    assert.equal(await row.evaluate((e) => e.isConnected), true);
    assert.equal(await page.locator("#wanKeys button:enabled").count(), 0);
    assert.equal(await page.locator("#wanKeys .none").count(), 0);
    delete f.state.keysError; f.state.keys = [];
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanKeys [data-key]").count(), 0);
    assert.equal(await page.locator("#wanKeys .none").textContent(), word("No WAN keys yet"));
    f.state.keys = null; f.state.keysError = keysError;
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanKeys .none").count(), 0, "a formerly empty read is not evidence of the unknown store");
    assert.equal(await page.locator("#wanKeysProblem").textContent(), word(keysError));
    assert.deepEqual(errors, []);
  });
}

for (const engine of engines) test(`${engine}: WAN cancel targets its exact attempt and never follows a successor`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en", false);
  await click(page, page.locator("#wanLogin"));
  await page.waitForFunction(() => document.querySelector("#wanCancelAuth") && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
  const previous = await page.locator("#wanCancelAuth").elementHandle();
  const oldID = f.state.auth.attemptID;
  f.state.auth.attemptID++;
  await page.evaluate(() => pollWAN());
  assert.equal(await previous.evaluate((e) => e.isConnected), false, "an old pointer press has no successor target");
  await previous.evaluate((e) => e.click()); // a stale detached handler must still fail closed
  assert.equal(events.some((e) => e.action === "wan:auth/cancel"), false);
  await click(page, page.locator("#wanCancelAuth"));
  await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_out");
  assert.deepEqual(events.find((e) => e.action === "wan:auth/cancel").body, { attemptID: oldID + 1 });
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: held WAN auth start keeps polling and logout rejects its late reply`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en", false);
  await page.evaluate(() => {
    const fetch0 = window.fetch;
    window.authHeld = false;
    window.fetch = async (...args) => {
      const response = await fetch0(...args);
      if (String(args[0]) === "/api/wan/auth/start") {
        const body = await response.text();
        authHeld = true;
        await new Promise((resolve) => { window.releaseAuth = resolve; });
        return new Response(body, { status: response.status, headers: response.headers });
      }
      return response;
    };
    const button = document.querySelector("#wanLogin"), start = button.onclick;
    button.onclick = function (...args) { return window.authFinished = start.apply(this, args); };
  });
  t.after(() => page.evaluate(() => window.releaseAuth?.()).catch(() => {}));
  await click(page, page.locator("#wanLogin"));
  await page.waitForFunction(() => authHeld && wanTimers.size === 1);
  await page.evaluate(() => pollWAN());
  assert.equal(await page.locator("#wanCancelAuth").isEnabled(), true, "poll obtains the attempt ID even while the initial POST reply is held");
  assert.equal(await page.locator("#wanLogout").isVisible(), false, "pending authorization offers Cancel instead of session management");
  f.code(); await page.evaluate(() => pollWAN());
  assert.equal(await page.locator("#wanUserCode").textContent(), "ABCD-EFGH");
  // Authorization may complete through polling before auth/start answers.
  // Sign out the completed session while that older reply remains held.
  f.signedIn(); await page.evaluate(() => pollWAN());
  await click(page, page.locator("#wanLogout"));
  await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_out" && document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
  await page.evaluate(() => { releaseAuth(); return authFinished; });
  assert.equal(await page.locator("#wanAuth").getAttribute("data-state"), "signed_out");
  assert.equal(await page.locator("#wanUserCode").textContent(), "");
  assert.equal(events.filter((e) => e.action === "wan:auth/logout").length, 1);
  assert.equal(events.some((e) => e.action === "wan:start" || e.action === "open"), false);
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: hidden WAN authorization continues in backend but late UI results are invalidated`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en", false);
  await page.evaluate(() => {
    const fetch0 = window.fetch;
    window.authHeld = false;
    window.fetch = async (...args) => {
      const response = await fetch0(...args);
      if (String(args[0]) === "/api/wan/auth/start") {
        const body = await response.text(); authHeld = true;
        await new Promise((resolve) => { window.releaseAuth = resolve; });
        return new Response(body, { status: response.status, headers: response.headers });
      }
      return response;
    };
    const button = document.querySelector("#wanLogin"), start = button.onclick;
    button.onclick = function (...args) { return window.authFinished = start.apply(this, args); };
  });
  t.after(() => page.evaluate(() => window.releaseAuth?.()).catch(() => {}));
  await click(page, page.locator("#wanLogin"));
  await page.waitForFunction(() => authHeld && wanTimers.size === 1);
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true })));
  assert.equal(await page.evaluate(() => wanTimers.size), 0);
  assert.equal(events.some((e) => e.action === "wan:auth/cancel" || e.action === "wan:auth/logout"), false);
  f.signedIn();
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
  await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_in" && wanTimers.size === 1);
  await page.evaluate(() => { releaseAuth(); return authFinished; });
  assert.equal(await page.locator("#wanAuth").getAttribute("data-state"), "signed_in");
  assert.equal(await page.locator("#wanUserCode").textContent(), "");
  assert.equal(events.some((e) => e.action === "wan:start" || e.action === "open"), false);
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: persisted page restoration resumes only visible WAN polling`, async (t) => {
  const { page, events, errors } = await open(t, engine, "en");
  await page.waitForFunction(() => wanTimers.size === 1);
  // Synthetic persisted lifecycle events exercise the event contract. This
  // is not a claim that Playwright's routed page entered native bfcache.
  await page.evaluate(() => {
    const fetch0 = window.fetch;
    window.restoreReads = 0;
    window.fetch = function (...args) {
      if (String(args[0]) === "/api/wan") restoreReads++;
      return fetch0.apply(this, args);
    };
  });
  // Calling mount on an already-mounted kernel page does nothing. Break a
  // render instead, then use the actual boundary's Try again path: it restores
  // the markup and mounts again. Each retry must dispose of the old hooks.
  const retry = async (visible = true) => {
    await page.evaluate(() => {
      Object.defineProperty(document.querySelector("#wanState"), "textContent", {
        configurable: true,
        set() { throw new Error("fixture WAN render failure"); },
      });
      magpie.call("wan", "relang");
    });
    const broken = page.locator("#view-wan .slot-error");
    await broken.waitFor({ state: "attached" });
    assert.match(await broken.textContent(), /fixture WAN render failure/);
    if (visible) await click(page, broken.locator(".slot-again"));
    // Exercise the same retry handler while hidden without opening the page.
    else await broken.locator(".slot-again").evaluate((b) => b.click());
    await broken.waitFor({ state: "detached" });
  };
  for (let i = 0; i < 2; i++) {
    const reads = await page.evaluate(() => restoreReads);
    await retry();
    await page.waitForFunction(() => document.querySelector("#wanStatus")?.dataset.state === "stopped");
    assert.equal(await page.locator("#wanStart").isEnabled(), true, "retry mounts working controls");
    assert.equal(await page.evaluate(() => wanTimers.size), 1, "retry itself resumes visible polling");
    assert.equal(await page.evaluate(() => restoreReads), reads, "remount schedules without an extra immediate read");
    await page.evaluate(() => pollWAN()); // advance only the timer retry installed
    assert.equal(await page.evaluate(() => restoreReads), reads + 1, "the resumed timer actually reads status");
    assert.equal(await page.evaluate(() => wanTimers.size), 1, "the resumed poll schedules its successor");
  }
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true })));
  assert.equal(await page.evaluate(() => wanTimers.size), 0);
  const before = await page.evaluate(() => restoreReads);
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
  assert.equal(await page.evaluate(() => restoreReads), before + 1, "one restore read, no duplicate remount hooks");
  await page.waitForFunction(() => wanTimers.size === 1);
  const restored = await page.evaluate(() => restoreReads);
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pageshow", { persisted: false })));
  assert.equal(await page.evaluate(() => restoreReads), restored, "initial pageshow adds no duplicate read");
  await page.evaluate(() => {
    dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true }));
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));
  });
  assert.equal(await page.evaluate(() => wanTimers.size), 0);
  assert.equal(await page.evaluate(() => restoreReads), restored, "a hidden restoration does not read");
  await retry(false);
  assert.equal(await page.evaluate(() => wanTimers.size), 0, "retry in a hidden document does not schedule polling");
  assert.equal(await page.evaluate(() => restoreReads), restored, "retry in a hidden document does not read");
  await page.evaluate(() => { delete document.hidden; document.dispatchEvent(new Event("visibilitychange")); });
  await page.waitForFunction(() => wanTimers.size === 1);
  await click(page, page.locator('#nav [data-view="providers"]'));
  await page.waitForFunction(() => document.querySelector("#view-wan").hidden && wanTimers.size === 0);
  const offPage = await page.evaluate(() => restoreReads);
  await retry(false);
  assert.equal(await page.evaluate(() => wanTimers.size), 0, "retry off-page does not schedule polling");
  assert.equal(await page.evaluate(() => restoreReads), offPage, "retry off-page does not read");
  await page.evaluate(() => {
    dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true }));
    dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));
  });
  assert.equal(await page.evaluate(() => wanTimers.size), 0);
  assert.equal(await page.evaluate(() => restoreReads), offPage);
  assert.equal(events.some((e) => e.action === "wan:start" || e.action === "wan:stop"), false, "page lifecycle never starts or stops the server");
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: a pre-hide WAN action cannot replace restored status`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en");
  await page.evaluate(() => {
    const fetch0 = window.fetch;
    window.actionHeld = false;
    window.fetch = async (...args) => {
      const response = await fetch0(...args);
      if (String(args[0]) === "/api/wan/start") {
        const body = await response.text();
        window.actionHeld = true;
        await new Promise((r) => { window.releaseAction = r; });
        return new Response(body, { status: response.status, headers: response.headers });
      }
      return response;
    };
    window.wanPublished = [];
    new MutationObserver(() => wanPublished.push(document.querySelector("#wanStatus").dataset.state))
      .observe(document.querySelector("#wanStatus"), { attributes: true, attributeFilter: ["data-state"] });
  });
  t.after(() => page.evaluate(() => window.releaseAction?.()).catch(() => {}));
  await click(page, page.locator("#wanStart")); // fake backend only
  await page.waitForFunction(() => actionHeld);
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true })));
  assert.equal(await page.evaluate(() => wanTimers.size), 0);
  f.state.state = "stopped";
  await page.evaluate(() => dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
  await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "stopped" && wanTimers.size === 1);
  await page.evaluate(() => { wanPublished.length = 0; releaseAction(); });
  await page.waitForFunction(() => document.querySelector("#view-wan").getAttribute("aria-busy") === "false" && wanTimers.size === 1);
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "stopped");
  assert.equal(await page.evaluate(() => wanPublished.includes("starting")), false, "even a transient stale action publication is forbidden");
  assert.equal(events.filter((e) => e.action === "wan:start").length, 1);
  assert.equal(events.some((e) => e.action === "wan:stop"), false, "restoring never stops the server");
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: Stop cancels a pending WAN start and ignores its late success`, async (t) => {
  const { page, f, errors } = await open(t, engine, "en");
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let started;
  const arrived = new Promise((resolve) => { started = resolve; });
  await page.route("**/api/wan/start", async (route) => {
    f.state.state = "starting";
    started();
    await held;
    await route.fulfill({ json: { ...f.state, state: "running", endpoint } });
  });
  t.after(() => release());
  await page.evaluate(() => {
    const button = document.querySelector("#wanStart"), start = button.onclick;
    button.onclick = function (...args) {
      // Await the real action, not just fixture fulfillment or a frame: this
      // resolves after api() consumes the late body and action() publishes
      // or rejects it and completes its finally block.
      return window.wanStartFinished = start.apply(this, args);
    };
  });
  await click(page, page.locator("#wanStart"));
  await arrived;
  assert.equal(await page.evaluate(() => typeof wanStartFinished?.then), "function", "the click exposes its actual action completion");
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "starting");
  assert.equal(await page.locator("#wanStop").isEnabled(), true, "Stop stays usable while Start awaits readiness");
  assert.equal(await page.locator("#addWANKey").isDisabled(), true);
  await page.evaluate(() => pollWAN());
  assert.equal(await page.locator("#wanStop").isEnabled(), true, "polling during Start does not lock Stop");
  await click(page, page.locator("#wanStop"));
  await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "stopping");
  await page.waitForFunction(() => document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
  f.state.state = "stopped";
  await page.evaluate(() => pollWAN());
  release();
  await page.evaluate(() => wanStartFinished);
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "stopped", "late start cannot revive the stopped page");
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: logout interrupts a pending WAN host start without accepting its late success`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en");
  let release, started;
  const held = new Promise((resolve) => { release = resolve; });
  const arrived = new Promise((resolve) => { started = resolve; });
  await page.route("**/api/wan/start", async (route) => {
    f.state.state = "starting";
    const late = structuredClone({ ...f.state, state: "running", endpoint });
    started(); await held;
    await route.fulfill({ json: late });
  });
  t.after(() => release());
  await page.evaluate(() => {
    const button = document.querySelector("#wanStart"), start = button.onclick;
    button.onclick = function (...args) { return window.wanStartFinished = start.apply(this, args); };
  });
  await click(page, page.locator("#wanStart")); await arrived;
  assert.equal(await page.locator("#wanLogout").isEnabled(), true);
  await click(page, page.locator("#wanLogout"));
  await page.waitForFunction(() => document.querySelector("#wanAuth").dataset.state === "signed_out" && document.querySelector("#wanStatus").dataset.state === "stopped");
  release(); await page.evaluate(() => wanStartFinished);
  assert.equal(await page.locator("#wanAuth").getAttribute("data-state"), "signed_out");
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "stopped");
  assert.equal(await page.locator("#wanEndpoint .copy").count(), 0);
  assert.equal(events.filter((e) => e.action === "wan:auth/logout").length, 1);
  assert.deepEqual(errors, []);
});

for (const engine of engines) test(`${engine}: WAN polling is canceled off-page and late reads cannot undo an action`, async (t) => {
  const { page, f, events, errors } = await open(t, engine, "en");
  // Hold a fully received response in fetch, so AbortController cannot take
  // away its bytes. This specifically tests generation rejection, not just
  // browser cancellation of a network request.
  await page.evaluate(() => {
    const fetch = window.fetch;
    window.holdWAN = false; window.heldWAN = false;
    window.fetch = async (...args) => {
      const response = await fetch(...args);
      if (holdWAN && String(args[0]) === "/api/wan") {
        const body = await response.text();
        holdWAN = false; heldWAN = true;
        await new Promise((resolve) => { window.releaseWAN = resolve; });
        return new Response(body, { status: response.status, headers: response.headers });
      }
      return response;
    };
    holdWAN = true;
    magpie.call("wan", "refresh");
  });
  await page.waitForFunction(() => heldWAN);
  await click(page, page.locator("#wanStart"));
  await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "starting");
  await page.waitForFunction(() => document.querySelector("#view-wan").getAttribute("aria-busy") === "false");
  await page.evaluate(() => releaseWAN());
  await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "starting");
  await click(page, page.locator('#nav [data-view="providers"]'));
  await page.waitForFunction(() => document.querySelector("#view-wan").hidden && wanTimers.size === 0);
  const requests = events.filter((e) => e.action === "wan:read").length;
  await page.evaluate(() => pollWAN());
  assert.equal(events.filter((e) => e.action === "wan:read").length, requests);
  assert.equal(events.some((e) => e.action === "wan:stop"), false, "leaving the page does not stop the backend");
  f.state.state = "running"; f.state.endpoint = endpoint;
  await click(page, page.locator('#nav [data-view="wan"]'));
  await page.waitForFunction(() => document.querySelector("#wanStatus").dataset.state === "running");
  assert.equal(await page.evaluate(() => wanTimers.size), 1);
  await page.evaluate(() => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    document.dispatchEvent(new Event("visibilitychange"));
  });
  assert.equal(await page.evaluate(() => wanTimers.size), 0, "background document has no poll");
  await page.evaluate(() => {
    delete document.hidden;
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await page.waitForFunction(() => wanTimers.size === 1);
  f.fail("WAN status could not be read");
  await page.evaluate(() => pollWAN());
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "unknown");
  assert.equal(await page.locator("#wanStart").isDisabled(), true);
  assert.equal(await page.locator("#wanStop").isDisabled(), true);
  assert.equal(await page.locator("#wanEndpoint .copy").count(), 0);
  await page.evaluate(() => pollWAN());
  assert.equal(await page.locator("#wanStatus").getAttribute("data-state"), "running");
  assert.deepEqual(errors, []);
});
for (const engine of engines) for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
  test(`${engine} ${lang}: WAN quota, account and tunnel diagnostics remain clear`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, lang);
    f.state.usage = { state: "ready", account: { id: "github:1", name: "fixture-user", provider: "github" }, used: 5371017692, limit: 5368709120, resetAt: new Date(Date.now() + 30 * 86400000).toISOString(), checkedAt: new Date().toISOString() };
    f.state.tunnel = { id: "fixture-tunnel", region: "usw2", port: 50188, accountID: "github:1", accountName: "fixture-user", new: false };
    f.state.state = "failed"; f.state.error = "WAN relay disconnected; start WAN again explicitly";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanState").textContent(), word("Dev Tunnels bandwidth exhausted"));
    assert.equal(await page.locator("#wanStart").isDisabled(), true);
    assert.match(await page.locator("#wanQuotaValue").textContent(), /5[.,]002 GiB \/ 5[.,]000 GiB/);
    assert.equal(await page.locator("#wanQuotaFill").evaluate(e=>e.style.width), "100%");
    assert.equal(await page.locator("#wanStatusReset").isVisible(), true);
    assert.notEqual(await page.locator("#wanQuotaReset").textContent(), word("Not reported"));
    assert.equal(await page.locator("#wanAccountName").textContent(), "fixture-user");
    assert.equal(await page.locator("#wanTunnelID").textContent(), "fixture-tunnel");
    assert.equal(await page.locator("#wanProblem").isVisible(), true);
    await resizeWAN(page, 360);
    const overflow = await page.locator("#view-wan").evaluate(e=>e.scrollWidth-e.clientWidth);
    assert(overflow <= 1, `narrow quota layout overflow ${overflow}`);
    f.state.usage.state = "stale"; f.state.usage.error = "usage check failed";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanQuotaState").textContent(), word("Last known usage · check failed"));
    assert.match(await page.locator("#wanQuotaValue").textContent(), /5[.,]002/);
    assert.equal(await page.locator("#wanStatusHelp").textContent(), word("WAN is offline. Review the error before starting again."));
    assert.equal(await page.locator("#wanStart").isDisabled(), false, "unknown current usage does not manufacture a current quota refusal");
    f.state.usage.state = "ready"; f.state.usage.error = "";
    f.state.usage.resetAt = new Date(Date.now() - 86400000).toISOString();
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanStart").isDisabled(), false, "a previous accounting period cannot block Start after its reset");
    f.state.usage = { state: "ready", account: { id: "github:2", name: "second-user" }, used: 0, limit: 5368709120, checkedAt: "2026-10-23T05:36:46Z" };
    f.state.tunnel = { accountID: "github:2", accountName: "second-user", new: true };
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanAccountName").textContent(), "second-user");
    assert.match(await page.locator("#wanQuotaValue").textContent(), /0[.,]000/);
    assert.equal(await page.locator("#wanTunnelID").textContent(), "—");
    assert.equal(await page.locator("#wanNewTunnel").isDisabled(), true);
    f.state.auth.state = "signed_out";
    await page.evaluate(() => pollWAN());
    assert.equal(await page.locator("#wanQuotaValue").textContent(), "—", "logout hides old account numbers");
    assert.equal(events.some(e=>e.action==="wan:start"), false);
    assert.deepEqual(errors, []);
  });
}
for (const engine of engines) {
  test(`${engine}: WAN Microsoft login uses its own device page and tunnel replacement is explicit`, async (t) => {
    const { page, f, word, events, errors } = await open(t, engine, "en", false);
    await click(page, page.locator("#wanMicrosoftLogin"));
    await page.waitForFunction(()=>document.querySelector("#view-wan").getAttribute("aria-busy")==="false" && document.querySelector("#wanAuth").dataset.state==="requesting_code");
    assert.deepEqual(events.find(e=>e.action==="wan:auth/start").body,{remember:false,provider:"microsoft"});
    f.code(); f.state.auth.provider="microsoft"; f.state.auth.verificationURI="https://login.microsoft.com/device";
    await page.evaluate(()=>pollWAN());
    assert.equal(await page.locator("#wanOpenGitHub").textContent(), "Open Microsoft");
    await Promise.all([page.waitForResponse(r => new URL(r.url()).pathname === "/api/open"), click(page,page.locator("#wanOpenGitHub"))]);
    assert.deepEqual(events.find(e=>e.action==="open").body,{url:"https://login.microsoft.com/device"});
    f.ready(); f.state.auth.provider="microsoft";
    f.state.tunnel={id:"microsoft-fixture",region:"usw2",port:50188,accountID:"microsoft:fixture",accountName:"ms-fixture",new:false};
    await page.evaluate(()=>pollWAN());
    await click(page,page.locator("#wanNewTunnel"));
    const dialog=page.getByRole("alertdialog");
    await click(page,dialog.getByRole("button",{name:"Cancel",exact:true}));
    assert.equal(events.some(e=>e.action==="wan:tunnel/new"),false);
    await click(page,page.locator("#wanNewTunnel"));
    await Promise.all([page.waitForResponse(r => new URL(r.url()).pathname === "/api/wan/tunnel/new"), click(page,dialog.getByRole("button",{name:"Use a new tunnel",exact:true}))]);
    await page.waitForFunction(()=>document.querySelector("#view-wan").getAttribute("aria-busy")==="false");
    assert.equal(events.filter(e=>e.action==="wan:tunnel/new").length,1);
    assert.equal(events.some(e=>e.action==="wan:start"),false,"replacement never publishes implicitly");
    assert.equal(await page.locator("#wanNewTunnel").isDisabled(),true);
    assert.deepEqual(errors,[]);
  });
}

for (const engine of engines) for (const theme of ["light", "dark"]) {
  test(engine + " " + theme + ": WAN actions have clear contrast, focus and required runtime placement", async t => {
    const {page,f,errors}=await open(t,engine,"en",false);
    await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
    const enabled=page.locator("#wanInstall, #wanLogin, #wanMicrosoftLogin");
    const styles=await enabled.evaluateAll(es=>es.map(e=>{
      const c=getComputedStyle(e),r=e.getBoundingClientRect();
      const rgb=x=>x.match(/[\d.]+/g).slice(0,3).map(Number);
      const lum=x=>rgb(x).map(v=>v/255).map(v=>v<=.04045?v/12.92:((v+.055)/1.055)**2.4).reduce((a,v,i)=>a+v*[.2126,.7152,.0722][i],0);
      const l=[lum(c.color),lum(c.backgroundColor)].sort((a,b)=>b-a);
      return {id:e.id,border:parseFloat(c.borderTopWidth),contrast:(l[0]+.05)/(l[1]+.05),height:r.height,opacity:c.opacity};
    }));
    for(const s of styles){assert(s.border>=1,s.id+" has a visible button boundary");assert(s.contrast>=4.5,s.id+" text contrast "+s.contrast);assert(s.height>=36,s.id+" has a clear target");assert.equal(s.opacity,"1");}
    await page.locator("#wanInstall").focus();
    assert(await page.locator("#wanInstall").evaluate(e=>parseFloat(getComputedStyle(e).outlineWidth)>=2),"keyboard focus has a visible outline");
    await resizeWAN(page,360);
    assert(await page.locator("#wanRuntimeDetails").evaluate(e=>e.getBoundingClientRect().bottom<=document.querySelector("#wanStatus").getBoundingClientRect().top),"runtime prerequisite is above status on mobile");
    f.ready();await page.evaluate(()=>pollWAN());
    assert.equal(await page.locator("#wanRuntimeDetails").isVisible(),false);
    await resizeWAN(page,1400);
    const geometry=await page.locator("#view-wan").evaluate(e=>{const a=e.querySelector(".wan-session-card").getBoundingClientRect(),q=e.querySelector(".wan-quota-card").getBoundingClientRect();return {account:a.x,quota:q.x,delta:Math.abs(a.y-q.y),overflow:e.scrollWidth-e.clientWidth};});
    assert(geometry.account<geometry.quota && geometry.delta<1,"desktop account and quota follow the approved order");
    assert(geometry.overflow<=1,"WAN dashboard fits its container");
    f.state.state="running";f.state.endpoint=endpoint;await page.evaluate(()=>pollWAN());
    assert(await page.locator("#wanEndpoint .copy").evaluate(e=>parseFloat(getComputedStyle(e).borderTopWidth)>=1),"address copy has an explicit button boundary");
    await resizeWAN(page,360);
    assert(await page.locator("#wanEndpoint .copy").evaluate(e=>e.getBoundingClientRect().height>=44),"address copy has a mobile touch target");
    assert.deepEqual(errors,[]);
  });
}
