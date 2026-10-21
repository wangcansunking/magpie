const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");
const { openPart, watchSettingsReads, settingsDrawn } = require("./reader.cjs");

// Persistence is checked after startup, not while the first draw's second
// provider read is still pending. That read starts Update/What's new, and
// Windows' titlebar timer also runs after the first Connect row is visible.
// Watch these exact operations through consumed bodies; networkidle would
// also wait for unrelated page polls. The held-reply test below separately
// interrupts startup and checks the production pagehide guard.
async function watchConnectStartup(page) {
  await page.addInitScript(() => {
    const pending = window.__connectStartup = { asked: 0, drawn: 0, seen: [] };
    const fetch0 = window.fetch;
    window.fetch = function (input, init) {
      const p = fetch0.apply(this, arguments);
      const path = new URL(typeof input === "string" ? input : input.url, location.href).pathname;
      if (!["/api/update", "/api/whatsnew", "/api/window/titlebar"].includes(path)) return p;
      pending.asked++;
      const drawn = () => setTimeout(() => { pending.drawn++; pending.seen.push(path); });
      return p.then((res) => {
        const text = res.text;
        res.text = function () { return text.call(this).finally(drawn); };
        return res;
      });
    };
  });
}
const connectStarted = (page) => page.waitForFunction(() => {
  const p = __connectStartup;
  return load.done && p.asked === p.drawn && p.seen.includes("/api/update") && p.seen.includes("/api/whatsnew") &&
    (!document.documentElement.classList.contains("win") || p.seen.includes("/api/window/titlebar"));
});

// coeo91 on Discord: the LAN address picked as Gateway's Base URL went back
// to 127.0.0.1 after a restart or an update. The address and the gateway key
// picked in Connect, and the address picked under Share on local network,
// are kept across reloads; an address the machine doesn't have right now
// (the network not up yet after a start) shows this computer's, and the
// pick comes back with it.
for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: a late startup reply cannot fetch after the pagehide guard`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await (await browser.newContext({ viewport: { width: 420, height: 800 }, reducedMotion: "reduce" })).newPage();
    page.setDefaultTimeout(6000);
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await page.route("**/*", fixture("zh-TW", "light", [], { lan: true }));
    await page.addInitScript(() => {
      let reads = 0, hidden = false, release;
      const held = new Promise((r) => { release = r; }), fetch0 = window.fetch;
      window.__providerHeld = false;
      addEventListener("pagehide", () => { hidden = true; });
      window.__releaseProvider = () => { sessionStorage.setItem("provider-released", "yes"); release(); };
      window.fetch = async function (input, init) {
        const url = typeof input === "string" ? input : input.url;
        if (hidden) {
          const late = JSON.parse(sessionStorage.getItem("fetch-after-pagehide") || "[]");
          late.push(url);
          sessionStorage.setItem("fetch-after-pagehide", JSON.stringify(late));
        }
        const response = await fetch0.apply(this, arguments);
        if (new URL(url, location.href).pathname === "/api/providers" && ++reads === 2) {
          const text = response.text.bind(response);
          response.text = async () => {
            // Consume the body first: browser cancellation cannot hide the
            // continuation that starts Update and What's new after pagehide.
            const body = await text();
            window.__providerHeld = true;
            await held;
            return body;
          };
        }
        return response;
      };
    });
    await page.goto("http://magpie.test/?view=gateway");
    await page.locator("#connect pre.snip").waitFor();
    await page.waitForFunction(() => window.__providerHeld);
    // Register after app.js so its lifecycle listener runs before the held
    // continuation, as with a response completing after pagehide dispatch.
    await page.evaluate(() => {
      sessionStorage.removeItem("fetch-after-pagehide");
      sessionStorage.removeItem("provider-released");
      addEventListener("pagehide", window.__releaseProvider, { once: true });
    });
    await page.reload();
    await page.locator("#connect pre.snip").waitFor();
    assert.equal(await page.evaluate(() => sessionStorage.getItem("provider-released")), "yes", "the old document released its held reply at pagehide");
    assert.deepEqual(await page.evaluate(() => JSON.parse(sessionStorage.getItem("fetch-after-pagehide") || "[]")), [], "no update, release-note or titlebar fetch from the page being left");
    assert.deepEqual(errors, []);
  });

  test(`${engine}: pageshow restores API access without hiding active-page errors`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await (await browser.newContext({ reducedMotion: "reduce" })).newPage();
    page.setDefaultTimeout(6000);
    const errors = [], requests = [];
    page.on("pageerror", (e) => errors.push(e.message));
    let release, started;
    const held = new Promise((r) => { release = r; }), began = new Promise((r) => { started = r; });
    t.after(() => release());
    const serve = fixture("en", "light", [], { web: true });
    await page.route("**/*", async (route) => {
      const pathname = new URL(route.request().url()).pathname;
      if (!pathname.startsWith("/api/lifecycle-")) return serve(route);
      requests.push(pathname);
      if (pathname === "/api/lifecycle-held") { started(); await held; }
      if (pathname === "/api/lifecycle-http") return route.fulfill({ status: 503, json: { error: "fixture unavailable" } });
      if (pathname === "/api/lifecycle-parse") return route.fulfill({ contentType: "text/plain", body: "fixture not JSON" });
      return route.fulfill({ json: { alive: true } });
    });
    await page.goto("http://magpie.test/?view=gateway");
    await page.locator("#connect pre.snip").waitFor();
    await page.evaluate(() => { window.__inflight = api("lifecycle-held"); });
    await began;
    const rejected = await page.evaluate(async () => {
      window.__opens = [];
      window.open = (...args) => { window.__opens.push(args); return null; };
      dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true }));
      const out = [];
      for (const [path, body] of [["lifecycle-hidden"], ["update"], ["whatsnew"], ["window/titlebar", {}], ["open", { url: "https://fixture.test/" }]]) {
        try { await api(path, body); out.push("resolved"); } catch (e) { out.push(e.name); }
      }
      return { out, opens: window.__opens };
    });
    assert.deepEqual(rejected, { out: Array(5).fill("AbortError"), opens: [] });
    assert.deepEqual(requests, ["/api/lifecycle-held"], "suspended API calls never reach fetch");
    release();
    assert.deepEqual(await page.evaluate(() => window.__inflight), { alive: true }, "an already-started request is not canceled");
    const restored = await page.evaluate(async () => {
      dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));
      const value = await api("lifecycle-restored");
      await api("open", { url: "https://fixture.test/" });
      const failures = [];
      for (const path of ["lifecycle-http", "lifecycle-parse"]) {
        try { await api(path); failures.push({ name: "resolved", message: "" }); }
        catch (e) { failures.push({ name: e.name, message: e.message }); }
      }
      return { value, opens: window.__opens, failures };
    });
    assert.deepEqual(restored.value, { alive: true });
    assert.deepEqual(restored.opens, [["https://fixture.test/", "_blank", "noopener"]]);
    assert.deepEqual(restored.failures.map((e) => e.name), ["Error", "Error"]);
    assert.equal(restored.failures[0].message, "fixture unavailable");
    assert.match(restored.failures[1].message, /fixture not JSON/);
    assert.deepEqual(errors, []);
  });

  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    for (const width of [1100, 420]) {
      test(`${engine} ${lang} ${width}: the address and key picked in Connect are kept`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const page = await (await browser.newContext({ viewport: { width, height: 800 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(6000);
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        const lanURLs = ["http://192.168.1.10:3999", "http://10.0.0.10:3999"];
        await page.route("**/*", fixture(lang, "light", [], { lan: true, lanURLs }));
        await watchSettingsReads(page);
        await watchConnectStartup(page);
        const pick = async (button, text) => {
          await page.locator(button).click();
          await page.locator(".proto-menu .pm-item", { hasText: text }).click();
          await page.locator(".proto-menu").waitFor({ state: "detached" });
        };
        const shown = (sel) => page.locator(sel).textContent();
        const open = async () => {
          await page.goto("http://magpie.test/?view=gateway");
          await page.locator("#connect .val code").first().waitFor();
          await connectStarted(page);
        };

        await open();
        await pick("#connectAddress", "10.0.0.10");
        await page.waitForFunction(() => document.querySelector("#connectAddress")?.textContent.includes("10.0.0.10"));
        await page.locator("#connectKey").waitFor();
        await pick("#connectKey", "Server");
        await page.waitForFunction(() => document.querySelector("#connectKey")?.textContent.includes("Server"));
        // The label is optimistic; the copy-key reply supplies the usable snippet.
        await page.waitForFunction(() => document.querySelector("#connect pre.snip")?.textContent.includes("fixture-server"));

        // a restart or an update opens the page again
        await open();
        await page.waitForFunction(() => document.querySelector("#connectKey")?.textContent.includes("Server"));
        assert.match(await shown("#connectAddress"), /10\.0\.0\.10:3999\/v1/);
        assert.match(await shown("#connectKey"), /Server/);
        await page.waitForFunction(() => document.querySelector("#connect pre.snip")?.textContent.includes("fixture-server"));

        // the network isn't up yet: this computer's address is shown, and
        // nothing of the pick is lost
        lanURLs.length = 0;
        await open();
        assert.match(await page.locator("#connect").textContent(), /127\.0\.0\.1:3999\/v1/);
        assert.equal(await page.locator("#connectAddress").count(), 0);
        lanURLs.push("http://192.168.1.10:3999", "http://10.0.0.10:3999");
        await open();
        assert.match(await shown("#connectAddress"), /10\.0\.0\.10:3999\/v1/);
        await page.waitForFunction(() => document.querySelector("#connectKey")?.textContent.includes("Server"));

        // Share on local network's address is kept the same way
        await page.locator("#prefs").click();
        await openPart(page, "network");
        await settingsDrawn(page);
        await page.locator("#lanList .lan-address-row").waitFor();
        await pick("#lanList .lan-interface", "10.0.0.10");
        await page.reload();
        await connectStarted(page);
        await page.locator("#prefs").click();
        await openPart(page, "network");
        await settingsDrawn(page);
        await page.locator("#lanList .lan-address-row").waitFor();
        assert.match(await shown("#lanList .lan-interface"), /10\.0\.0\.10:3999/);

        // nothing on the page is wider than the window
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
        assert.deepEqual(errors, []);
      });
    }
  }
}
