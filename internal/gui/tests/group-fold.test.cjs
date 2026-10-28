// Run with Node's test runner and Playwright on the module path; see README.md.
// A routing group opened from its row folds back from the editor's heading
// (ARNO on Discord: 路由组详情现在可以单击展开，是否可以再加上单击收起):
// a click, or Enter on it, closes the editor while nothing was changed;
// with a change it stays open and says to Save or Cancel, so no edit is
// lost. Nothing is saved by folding. English and Chinese.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const models = [
  { id: "oa/gpt-6.1-sol", name: "gpt-6.1-sol", providerName: "OpenAI", icon: "generic", efforts: ["low", "medium", "high", "xhigh"], canFast: true },
  { id: "an/claude-opus-5-5", name: "claude-opus-5-5", providerName: "Anthropic", icon: "generic", canFast: true },
  { id: "rl/glm-5", name: "glm-5", providerName: "Relay", icon: "generic" },
];
const members = ["oa/gpt-6.1-sol:high", "an/claude-opus-5-5", "rl/glm-5"];
const saved = { firstToken: 0 };
const groups = () => ({
  models, pools: [],
  groups: [{ id: "sol", name: "Sol", members, routing: "order", ready: true, firstToken: saved.firstToken,
    memberInfo: members.map((id) => ({ id, ready: true })) }],
});

const words = {
  en: { fold: "Click to fold it", first: "Save or Cancel your changes first", cancel: "Cancel" },
  zh: { fold: "点击收起", first: "有未保存的修改，请先保存或取消", cancel: "取消" },
};

function serve(lang, posts) {
  const state = { agents: [{ id: "codex", name: "Codex", path: "/test/codex", fields: [] }], profiles: [], settings: { lang, theme: "light" } };
  return async (r) => {
    const url = new URL(r.request().url());
    const json = (data) => r.fulfill({ json: data });
    if (url.pathname === "/boot.js") return r.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return r.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/gateway/trace") {
      if (url.searchParams.get("wait")) return new Promise(() => {}); // nothing more comes
      return json({ mine: true, now: new Date().toISOString(), seq: 1, totals: { requests: 0, rerouted: 0, errors: 0 }, routes: [] });
    }
    if (url.pathname === "/api/gateway/history") return json({ cut: false, days: [], routes: [] });
    if (url.pathname === "/api/groups") return json(groups());
    if (url.pathname.startsWith("/api/groups/")) {
      const body = JSON.parse(r.request().postData() || "{}");
      posts.push({ path: url.pathname, body });
      saved.firstToken = body.firstToken || 0;
      return json(groups());
    }
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true, url: "http://127.0.0.1:3999" } });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await r.fulfill({ body: await fs.readFile(file), contentType });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh"]) {
    const w = words[lang];
    test(`${engine} ${lang}: an open group folds back from its heading`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      // tall enough for the whole editor, its Cancel clicked where it is
      // (under the names setting over the groups)
      // the window wider by the sidebar beside the page: the editor's foot in sight
      const page = await (await browser.newContext({ viewport: { width: 1360, height: 1700 }, reducedMotion: "reduce" })).newPage();
      t.after(() => browser.close());
      page.setDefaultTimeout(5000);
      const errors = [], posts = [], pending = new Set();
      page.on("pageerror", (e) => errors.push(e.message));
      page.on("request", (r) => pending.add(r));
      page.on("requestfinished", (r) => pending.delete(r));
      page.on("requestfailed", (r) => pending.delete(r));
      await page.route("**/*", serve(lang, posts));
      await page.goto("http://magpie.test/?view=routing");
      const card = page.locator(".rt-group", { hasText: "Sol" });
      const ed = page.locator(".rt-gedit"), head = ed.locator(".ehead");
      const closed = () => page.waitForFunction(() => !document.querySelector(".rt-gedit") && document.querySelector(".rt-group"));

      // Keep the original pointer action and its bound. If stability stalls,
      // record what the browser was waiting on instead of guessing its cause.
      // The browser's t.after cleanup also runs when this diagnostic rethrows.
      try {
        await card.locator(".main").click();
      } catch (error) {
        let timer;
        try {
          const snapshot = await Promise.race([page.evaluate(() => {
          const target = [...document.querySelectorAll(".rt-group")].find((e) => e.textContent.includes("Sol"))?.querySelector(".main");
          const parents = [];
          for (let e = target; e; e = e.parentElement) {
            const s = getComputedStyle(e);
            parents.push({ tag: e.tagName, class: e.className, connected: e.isConnected, rect: e.getBoundingClientRect().toJSON(), scrollTop: e.scrollTop,
              style: { display: s.display, visibility: s.visibility, transform: s.transform, overflow: s.overflow, pointerEvents: s.pointerEvents },
              animations: e.getAnimations().map((a) => ({ state: a.playState, time: a.currentTime, infinite: a.effect?.getComputedTiming().iterations === Infinity, timing: a.effect?.getComputedTiming(), frames: a.effect?.getKeyframes() })) });
          }
          return { parents, focus: document.activeElement?.outerHTML.slice(0, 500), visibility: document.visibilityState };
          }), new Promise((resolve) => { timer = setTimeout(() => resolve({ unavailable: "browser did not answer the diagnostic" }), 1000); })]);
          console.error("group-fold click diagnostic", JSON.stringify({ snapshot, pending: [...pending].map((r) => ({ method: r.method(), url: r.url() })) }));
        } finally {
          clearTimeout(timer);
          await browser.close();
        }
        throw error;
      }
      // opened from the row, folded from the heading
      await head.waitFor();
      assert.equal(await head.getAttribute("title"), w.fold);
      assert.equal(await head.getAttribute("aria-expanded"), "true");
      await head.click();
      await closed();

      // and from the keyboard
      await card.locator(".main").click();
      await head.focus();
      await page.keyboard.press("Enter");
      await closed();

      // a change keeps it open, and says so; Cancel still closes it
      await card.locator(".main").click();
      await ed.locator("input").first().fill("Sol 2");
      await head.click();
      await page.locator("#status").filter({ hasText: w.first }).waitFor();
      assert.equal(await ed.locator("input").first().inputValue(), "Sol 2", "the edit is kept");
      await ed.locator("button", { hasText: new RegExp("^" + w.cancel + "$") }).click();
      await page.locator("dialog.action-confirm[open] button").last().click();
      await closed();

      assert.deepEqual(posts, [], "folding saves nothing");
      assert.deepEqual(errors, []);
    });
  }
}
