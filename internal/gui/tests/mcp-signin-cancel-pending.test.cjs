// Cancel owns an attempt, not a server name. Hold actual API reply delivery
// after the fake server commits it, then use the editor's real controls.
// No OAuth server, browser opening, install or live configuration is involved.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const base = "/api/library/";
const remote = (name) => ({ name, transport: "http", url: `https://${name}.example/mcp`, headers: { Authorization: "fixture-token" }, agents: ["claude"], signIn: { signedIn: false } });

// Observe each real async operation's completion, not a transient DOM state
// or a response-header event. No production branch or await is replaced.
function observe(source, name, list) {
  const marker = `async function ${name}(`;
  assert.equal(source.split(marker).length, 2, `one ${name} to observe`);
  return source.replace(marker, `function ${name}(...args) {
    const record = { settled: false };
    window.${list}.push(record);
    const promise = ${name}Observed(...args);
    promise.then(() => { record.settled = true; }, error => {
      Object.assign(record, { settled: true, error: error.message });
    });
    return promise;
  }
  async function ${name}Observed(`);
}

async function fixture(browser, t) {
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 800 }, reducedMotion: "reduce" });
  t.after(() => ctx.close());
  const page = await ctx.newPage();
  page.setDefaultTimeout(5000);
  const errors = [], network = [], calls = [], polls = [];
  const lib = {
    dir: "/fixture/library", backups: "/fixture/backups", home: "/fixture",
    agents: [{ id: "claude", name: "Claude Code", icon: "", skills: "/fixture/skills", mcp: "/fixture/mcp.json" }],
    instructions: { agents: [], sets: [] }, foundServers: [], projects: [], foundSkills: [], skills: [],
    servers: [remote("github"), remote("other")],
  };
  const state = { failStart: false, failSave: false, done: new Set(), starts: 0 };
  page.on("pageerror", e => errors.push(e.message));
  page.on("requestfailed", r => network.push(`${r.url()}: ${r.failure()?.errorText}`));
  await page.addInitScript(() => {
    localStorage.setItem("magpie.libTab", "mcp");
    window.__opened = [];
    window.open = (...args) => { __opened.push(args); return null; };
    window.__libraryReads = [];
    window.__mcpStarts = [];
    window.__mcpFollows = [];
    window.__mcpCancels = [];
    window.__delivery = {};
    const original = window.fetch;
    window.fetch = async function (input, init) {
      const pathname = new URL(String(input), location.href).pathname;
      const gate = __delivery[pathname];
      if (gate && !gate.claimed) gate.claimed = true;
      const held = gate?.claimed && !gate.request ? gate : null;
      if (held) held.request = true;
      const response = await original.apply(this, arguments);
      if (held) {
        held.waiting = true;
        await new Promise(resolve => { held.release = resolve; });
      }
      return response;
    };
  });
  await ctx.route("**/*", async route => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (url.origin !== "http://magpie.test") {
      network.push("unexpected destination " + url.href);
      await route.abort();
      return;
    }
    const json = (data, status = 200) => route.fulfill({ json: data, status });
    if (p === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: 'window.bootPrefs={lang:"en",theme:"light",web:true};' });
    if (p === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (p === "/api/state") return json({ agents: [], profiles: [], settings: { lang: "en", theme: "light" } });
    if (p === "/api/library") return json(lib);
    if (p === base + "mcp/check") return json({ servers: {} });
    if (p === base + "mcp-signin") {
      const id = "s" + ++state.starts, name = req.postDataJSON().name;
      calls.push(["start", id, name]);
      if (state.failStart) { state.failStart = false; return json({ error: "fixture start refused" }, 400); }
      return json({ id, server: name, state: "waiting", url: `https://signin.example/${id}` });
    }
    const cancel = p.match(/^\/api\/library\/mcp-signin\/(s\d+)\/cancel$/);
    if (cancel) { calls.push(["cancel", cancel[1]]); return json({}); }
    const poll = p.match(/^\/api\/library\/mcp-signin\/(s\d+)$/);
    if (poll) {
      polls.push(poll[1]);
      return json({ id: poll[1], state: state.done.has(poll[1]) ? "done" : "waiting" });
    }
    if (p === base + "servers/save") {
      const body = req.postDataJSON();
      calls.push(["save", body.server.name]);
      if (state.failSave) { state.failSave = false; return json({ error: "fixture save refused" }, 400); }
      const i = lib.servers.findIndex(s => s.name === body.old);
      lib.servers[i] = { ...lib.servers[i], ...body.server };
      return json({ ...lib, result: { changed: ["claude"] } });
    }
    if (p === "/api/open") { calls.push(["open", req.postDataJSON().url]); return json({}); }
    if (p === "/api/usage/quotas") return json([]);
    if (p === "/api/groups") return json({ groups: [] });
    if (p === "/api/plugins") return json({ plugins: [] });
    if (p === "/api/providers") return json({ providers: [], gateway: { running: true } });
    if (p.startsWith("/api/")) return json({});
    const file = path.join(assets, p === "/" ? "index.html" : p);
    let body = await fs.readFile(file);
    if (p === "/plugins/library/client.js") {
      body = observe(observe(observe(body.toString(), "start", "__mcpStarts"), "follow", "__mcpFollows"), "load", "__libraryReads");
      const invoke = "onclick(e, b);";
      assert.equal(body.split(invoke).length, 2, "one button action to observe");
      body = body.replace(invoke, `const cancel = b.textContent === "Cancel" && b.closest(".lib-signin");
        const promise = onclick(e, b);
        if (cancel) {
          const record = { settled: false };
          __mcpCancels.push(record);
          promise.then(() => { record.settled = true; }, error => { Object.assign(record, { settled: true, error: error.message }); });
        }`);
    }
    return route.fulfill({ body, contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)] });
  });
  await page.goto("http://magpie.test/?view=library");
  const ed = page.locator("#modal .lib-editor"), box = ed.locator(".lib-signin");
  const row = name => page.locator(".lib-server", { has: page.locator(`.lib-health[data-server="${name}"]`) });
  const open = async (name = "github") => { await row(name).locator(".name").click(); await ed.waitFor(); };
  const start = () => box.getByRole("button", { name: /^(Save and sign in|Sign in)$/ }).click();
  const cancel = async (wait = true) => {
    const at = await page.evaluate(() => __mcpCancels.length);
    await box.getByRole("button", { name: "Cancel", exact: true }).click();
    if (wait) await settled("__mcpCancels", at);
  };
  const close = async (discard = false) => {
    await ed.locator(":scope > .bar button").filter({ hasText: /^Cancel$/ }).click();
    if (discard) await page.locator("dialog.action-confirm button.primary").click();
    await ed.waitFor({ state: "detached" });
  };
  const arm = p => page.evaluate(p => { __delivery[p] = {}; }, p);
  const held = p => page.waitForFunction(p => __delivery[p]?.waiting, p);
  const release = p => page.evaluate(p => __delivery[p].release(), p);
  const settled = async (list, n) => {
    await page.waitForFunction(({ list, n }) => window[list][n]?.settled, { list, n });
    assert.equal(await page.evaluate(({ list, n }) => window[list][n].error, { list, n }), undefined, "observed operation completed without uncaught error");
  };
  await settled("__libraryReads", 0);
  const quiet = async () => {
    await page.waitForFunction(() => [__libraryReads, __mcpStarts, __mcpFollows, __mcpCancels].every(list => list.every(r => r.settled)));
    assert.deepEqual(errors, [], "no page errors");
    assert.deepEqual(network, [], "no failed network requests");
  };
  return { ctx, page, lib, state, calls, polls, ed, box, row, open, start, cancel, close, arm, held, release, settled, quiet };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: a canceled MCP attempt cannot return`, async t => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const run = (name, fn) => t.test(name, async child => {
      const f = await fixture(browser, child);
      await fn(f);
      await f.quiet(); // the registered child hook closes even on setup/assertion failure
    });

    await run("early Cancel cleans up a late ID without opening or polling", async f => {
      await f.open(); await f.arm(base + "mcp-signin"); await f.start(); await f.held(base + "mcp-signin");
      await f.cancel();
      await f.release(base + "mcp-signin"); await f.settled("__mcpStarts", 0);
      assert.deepEqual(f.calls, [["start", "s1", "github"], ["cancel", "s1"]]);
      assert.deepEqual(f.polls, []);
      assert.deepEqual(await f.page.evaluate(() => __opened), [], "canceled URL never opened");
      assert.equal(await f.box.getByRole("button", { name: "Sign in", exact: true }).count(), 1);
      assert.equal(await f.page.evaluate(() => __mcpFollows.length), 0, "late attempt started no follow");
    });

    for (const other of [false, true]) await run(`new ${other ? "different" : "same"}-name attempt survives a late start`, async f => {
      await f.open(); await f.arm(base + "mcp-signin"); await f.start(); await f.held(base + "mcp-signin");
      if (other) { await f.close(); await f.open("other"); } else await f.cancel();
      await f.start(); await f.settled("__mcpStarts", 1);
      await f.release(base + "mcp-signin"); await f.settled("__mcpStarts", 0);
      assert.equal(await f.box.getByText("Finish signing in in your browser…", { exact: true }).count(), 1);
      await f.cancel(); await f.settled("__mcpFollows", 0);
      assert.deepEqual(f.calls.filter(c => c[0] === "cancel"), [["cancel", "s1"], ["cancel", "s2"]]);
      assert.deepEqual(await f.page.evaluate(() => __opened), [["https://signin.example/s2", "_blank", "noopener"]]);
      assert(!f.polls.includes("s1"), "old ID never polled");
    });

    for (const other of [false, true]) for (const failure of [false, true]) await run(`Cancel while ready saves${failure ? " and fails late" : ""}, ${other ? "different" : "same"}-name replacement`, async f => {
      await f.open(); await f.ed.locator("input.mono").first().fill("https://github.example/changed");
      f.state.failSave = failure;
      await f.arm(base + "servers/save"); await f.start(); await f.held(base + "servers/save");
      await f.cancel();
      // Reopen from the saved row to give ready() an independent form.
      await f.close(true); // the held save has not reset this editor's draft
      await f.open(other ? "other" : "github"); await f.start(); await f.settled("__mcpStarts", 1);
      await f.release(base + "servers/save"); await f.settled("__mcpStarts", 0);
      assert.deepEqual(f.calls.filter(c => c[0] === "start"), [["start", "s1", other ? "other" : "github"]], "canceled ready never starts its sign-in");
      assert.equal(await f.box.getByText("Finish signing in in your browser…", { exact: true }).count(), 1);
      assert.equal(await f.ed.locator(".lib-signin-err").count(), 0, "late ready failure did not replace newer UI");
      if (!failure) assert.equal(f.lib.servers[0].url, "https://github.example/changed", "Cancel does not undo a committed save");
      await f.cancel(); await f.settled("__mcpFollows", 0);
    });

    await run("same-name ready reply cannot start while a replacement ready is pending", async f => {
      await f.open(); await f.ed.locator("input.mono").first().fill("https://github.example/first");
      await f.arm(base + "servers/save"); await f.start(); await f.held(base + "servers/save");
      await f.page.evaluate(() => { window.__firstSave = __delivery["/api/library/servers/save"]; });
      await f.cancel(); await f.ed.locator("input.mono").first().fill("https://github.example/second");
      await f.arm(base + "servers/save"); await f.start(); await f.held(base + "servers/save");
      await f.page.evaluate(() => __firstSave.release()); await f.settled("__mcpStarts", 0);
      assert.deepEqual(f.calls.filter(c => c[0] === "start"), [], "older same-name ready must not launch either server");
      await f.release(base + "servers/save"); await f.settled("__mcpStarts", 1);
      assert.deepEqual(f.calls.filter(c => c[0] === "start"), [["start", "s1", "github"]]);
      await f.cancel(); await f.settled("__mcpFollows", 0);
    });

    await run("late start failure cannot replace a newer same-name attempt", async f => {
      await f.open(); f.state.failStart = true;
      await f.arm(base + "mcp-signin"); await f.start(); await f.held(base + "mcp-signin"); await f.cancel();
      await f.start(); await f.settled("__mcpStarts", 1);
      await f.release(base + "mcp-signin"); await f.settled("__mcpStarts", 0);
      assert.equal(await f.ed.locator(".lib-signin-err").count(), 0);
      assert.equal(await f.box.getByText("Finish signing in in your browser…", { exact: true }).count(), 1);
      await f.cancel(); await f.settled("__mcpFollows", 0);
      assert.deepEqual(f.calls.filter(c => c[0] === "cancel"), [["cancel", "s2"]]);
    });

    await run("late cancel reply cannot clear a newer attempt", async f => {
      await f.open(); await f.start(); await f.settled("__mcpStarts", 0);
      await f.arm(base + "mcp-signin/s1/cancel"); await f.cancel(false); await f.held(base + "mcp-signin/s1/cancel");
      await f.start(); await f.settled("__mcpStarts", 1);
      await f.release(base + "mcp-signin/s1/cancel"); await f.settled("__mcpCancels", 0);
      // Join the old follow as well: it must notice invalidation after its timer.
      await f.settled("__mcpFollows", 0);
      assert.equal(await f.box.getByText("Finish signing in in your browser…", { exact: true }).count(), 1);
      await f.cancel(); await f.settled("__mcpFollows", 1);
      assert.deepEqual(f.calls.filter(c => c[0] === "cancel"), [["cancel", "s1"], ["cancel", "s2"]]);
    });

    await run("a held old poll cannot complete a newer attempt", async f => {
      await f.open(); await f.arm(base + "mcp-signin/s1");
      f.state.done.add("s1"); // the very first poll must carry done, even on a busy runner
      await f.start(); await f.settled("__mcpStarts", 0);
      await f.held(base + "mcp-signin/s1");
      await f.cancel(); await f.start(); await f.settled("__mcpStarts", 1);
      await f.release(base + "mcp-signin/s1"); await f.settled("__mcpFollows", 0);
      assert.equal(await f.box.getByText("Finish signing in in your browser…", { exact: true }).count(), 1);
      await f.cancel(); await f.settled("__mcpFollows", 1);
      assert.deepEqual(f.calls.filter(c => c[0] === "cancel"), [["cancel", "s1"], ["cancel", "s2"]]);
    });

    await run("closing only the editor still follows and updates the row", async f => {
      await f.open(); await f.arm(base + "mcp-signin"); await f.start(); await f.held(base + "mcp-signin"); await f.close();
      f.state.done.add("s1"); f.lib.servers[0].signIn.signedIn = true;
      await f.release(base + "mcp-signin"); await f.settled("__mcpStarts", 0); await f.settled("__mcpFollows", 0);
      assert.equal(await f.row("github").locator(".lib-signed").count(), 1);
      assert.deepEqual(f.calls.filter(c => c[0] === "cancel"), []);
      assert.deepEqual(f.polls, ["s1"]);
    });

    await run("late done-Library snapshot cannot replace a newer saved server", async f => {
      await f.open(); await f.start(); await f.settled("__mcpStarts", 0);
      await f.arm("/api/library"); f.state.done.add("s1"); await f.held("/api/library");
      await f.cancel(); await f.ed.locator("input.mono").first().fill("https://github.example/newer");
      await f.start(); await f.settled("__mcpStarts", 1);
      await f.release("/api/library"); await f.settled("__mcpFollows", 0);
      await f.cancel(); await f.settled("__mcpFollows", 1); await f.close(); await f.open();
      assert.equal(await f.ed.locator("input.mono").first().inputValue(), "https://github.example/newer", "stale done snapshot was not published");
    });
  });
}
