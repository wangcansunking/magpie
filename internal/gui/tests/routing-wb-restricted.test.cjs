// Run with Node's test runner and Playwright on the module path; see README.md.
// WorkBuddy's 403 11140 "request illegal" for an account it restricted
// (#1849): the Routing page tells the try as the account restricted, not
// "failed", says it rests two hours because only the vendor lifting the
// restriction brings it back, after an appeal; gives magpie's hint apart
// from the vendor's words; and the resting account's row lifts its rest by
// hand once the appeal went through — in every language the page speaks.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { reach, wheelTo } = require("./reader.cjs");

const assets = path.resolve(__dirname, "../assets");
// Run the actual countdown between native down/up, not a DOM replacement
// invented by the test. Date alone advances; action timeouts keep real time.
async function watchCountdown(page) {
  await page.addInitScript(() => {
    const interval = setInterval, now = Date.now;
    window.restClock = 0;
    Date.now = () => now() + restClock;
    window.setInterval = function (fn, ms, ...args) {
      if (ms === 1000 && String(fn).includes("renderActs(listed())")) window.restCountdown = fn;
      return interval.call(this, fn, ms, ...args);
    };
  });
}
const now = new Date();
const day = [now.getFullYear(), now.getMonth() + 1, now.getDate()].map((n) => String(n).padStart(2, "0")).join("-");
const start = new Date(now.getTime() - 60e3).toISOString();
const hint = "WorkBuddy restricted this account for unusual activity; it stays out until WorkBuddy lifts that, which an appeal (Submit feedback in WorkBuddy) asks for";
const vendor = "WorkBuddy AI: request illegal";
const a = { id: "workbuddy-ai#a", provider: "workbuddy-ai", name: "WorkBuddy AI", kind: "account", who: "a@x", model: "deepseek-v4.1-flash" };
const b = { id: "workbuddy-ai#b", provider: "workbuddy-ai", name: "WorkBuddy AI", kind: "account", who: "b@x", model: "deepseek-v4.1-flash" };
const rest = { why: "restricted", by: "restricted", status: 403, key: "workbuddy-ai@a", until: new Date(now.getTime() + 2 * 3600e3 - 60e3).toISOString() };
const routes = [{
  id: 100, seq: 100, time: start, agent: "codex", model: "workbuddy-ai/deepseek-v4.1-flash", provider: "workbuddy-ai",
  order: [a, b],
  tries: [
    { id: a.id, model: a.model, start, done: true, status: 403, ms: 900, fail: "restricted", rest, error: vendor + " — " + hint },
    { id: b.id, model: b.model, start, done: true, status: 200, ms: 1200 },
  ],
  done: true, status: 200, ms: 2100,
}];

function serve(lang, lifted) {
  const state = { agents: [{ id: "codex", name: "Codex", path: "/test/config.toml", fields: [] }], profiles: [], settings: { lang, theme: "light" } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/gateway/trace") {
      if (url.searchParams.get("wait")) return; // the context closes the held poll with the test
      return json({ mine: true, now: now.toISOString(), seq: 1, totals: { requests: 0, rerouted: 0, errors: 0 }, routes: [] });
    }
    if (url.pathname === "/api/gateway/history") {
      const d = url.searchParams.get("day");
      return json({ cut: false, days: [{ day, requests: routes.length }], routes: d ? routes : [] });
    }
    if (url.pathname === "/api/gateway/unrest") {
      lifted.push(JSON.parse(route.request().postData() || "{}"));
      return json({ lifted: true });
    }
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], excluded: [], gateway: { running: true, window: true } });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

const want = {
  en: { label: "account restricted by the vendor", why: "only the vendor lifting the restriction brings it back. Appeal with the vendor", hint, lift: "The restriction is lifted — try it again" },
  zh: { label: "账号被服务商限制", why: "只有服务商解除限制才能让它恢复。请向服务商申诉", hint: "WorkBuddy 因异常行为限制了这个账号；在 WorkBuddy 解除之前它不会再被使用，可在 WorkBuddy 中提交反馈申诉", lift: "限制已解除，重新使用" },
  "zh-TW": { label: "帳號被服務商限制", why: "只有服務商解除限制才能讓它恢復。請向服務商申訴", hint: "WorkBuddy 因異常行為限制了這個帳號；在 WorkBuddy 解除之前它不會再被使用，可在 WorkBuddy 中提交回饋申訴", lift: "限制已解除，重新使用" },
  ja: { label: "ベンダーがアカウントを制限", why: "制限を解除できるのはベンダーだけなので", hint: "WorkBuddy が不審なアクティビティを理由にこのアカウントを制限しました。WorkBuddy が解除するまで使われません。解除は WorkBuddy のフィードバック送信から申し立てられます", lift: "制限が解除された — もう一度試す" },
  de: { label: "Konto vom Anbieter gesperrt", why: "denn nur die Aufhebung der Sperre durch den Anbieter bringt es zurück. Legen Sie beim Anbieter Einspruch ein", hint: "WorkBuddy hat dieses Konto wegen ungewöhnlicher Aktivität gesperrt; es bleibt außen vor, bis WorkBuddy die Sperre aufhebt, was ein Einspruch (Feedback senden in WorkBuddy) beantragt", lift: "Die Sperre ist aufgehoben – erneut versuchen" },
};

// A live trace fixture also changes identities/targets without navigating:
// old pointer presses must never be redirected to a newly offered operation.
function liveRestFixture() {
  const f = { pending: null, nextPoll: Promise.withResolvers(), posts: [], opens: [], failure: "", held: null, hold: false, seq: 10 };
  const r = (key, extra = {}) => ({ why: "quota", until: new Date(Date.now() + 7200000).toISOString(), key, ...extra });
  f.seats = [
    { ...a, who: "same@x · One", id: "workbuddy-ai", rest: r("seat-one") },
    { ...a, who: "same@x · Two", id: "workbuddy-ai@two", rest: r("seat-two") },
  ];
  f.data = () => ({ mine: true, now: new Date().toISOString(), seq: f.seq, totals: {}, routes: [{ id: 1, seq: f.seq, time: new Date().toISOString(), agent: "codex", model: "group/g", provider: "workbuddy-ai", order: f.seats, tries: [], done: true, status: 200 }] });
  const base = serve("en", f.posts);
  f.route = async route => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/gateway/trace") {
      if (url.searchParams.has("wait")) { f.pending = route; f.nextPoll.resolve(); return; }
      return route.fulfill({ json: f.data() });
    }
    if (url.pathname === "/api/gateway/history") return route.fulfill({ json: { days: [], routes: [] } });
    if (url.pathname === "/api/gateway/unrest") {
      f.posts.push(route.request().postDataJSON());
      if (f.hold) { f.held = route; return; }
      return route.fulfill(f.failure ? { status: 500, json: { error: f.failure } } : { json: { lifted: true } });
    }
    if (url.pathname === "/api/open") { f.opens.push(route.request().postDataJSON()); return route.fulfill({ json: {} }); }
    return base(route);
  };
  const polled = async () => {
    let deadline;
    try {
      await Promise.race([f.nextPoll.promise, new Promise((_, reject) => {
        deadline = setTimeout(() => reject(new Error("the next trace poll was not requested")), 5000);
      })]);
    } finally { clearTimeout(deadline); }
  };
  f.publish = async page => {
    // Its next fulfilled route registration proves the prior body was
    // consumed, rather than merely seen by Playwright's request event.
    await polled();
    const pending = f.pending;
    f.pending = null;
    f.nextPoll = Promise.withResolvers();
    f.seq++;
    await pending.fulfill({ json: f.data() });
    await polled();
    await page.evaluate(() => restCountdown());
  };
  return f;
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: rest actions keep identities but never redirect an old press`, async t => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 1100, height: 1000 }, reducedMotion: "reduce" });
    page.setDefaultTimeout(5000);
    const f = liveRestFixture(), errors = [];
    page.on("pageerror", e => errors.push(e.message));
    // This fixture is magpie web: api("open") uses the browser's window,
    // not the desktop host's /api/open. Observe it without visiting the URL.
    await page.exposeFunction("fixtureOpen", url => { f.opens.push({ url }); });
    await page.addInitScript(() => {
      window.fixtureOpening = null;
      window.open = (url, target, features) => {
        window.fixtureOpenOptions = { target, features };
        window.fixtureOpening = window.fixtureOpen(url);
        return null;
      };
    });
    await watchCountdown(page);
    await page.route("**/*", f.route);
    await page.goto("http://magpie.test/?view=routing");
    const row = user => page.locator(".rt-act", { has: page.locator(".nm b", { hasText: user }) });
    const one = row("same@x · One"), two = row("same@x · Two");
    await one.locator(".fix button").waitFor();
    await two.locator(".fix button").waitFor();
    // Identical labels, different organizations; current-login id and list
    // position may change, but each button remains its own account's.
    const first = await one.locator(".fix button").elementHandle();
    const second = await two.locator(".fix button").elementHandle();
    f.seats = [{ ...f.seats[1], id: "workbuddy-ai" }, { ...f.seats[0], id: "workbuddy-ai@one" }];
    await f.publish(page);
    assert.equal(await first.evaluate(e => e.isConnected && e.closest(".rt-act").querySelector(".nm b").textContent === "same@x · One"), true);
    assert.equal(await second.evaluate(e => e.isConnected && e.closest(".rt-act").querySelector(".nm b").textContent === "same@x · Two"), true);
    assert.deepEqual(await page.locator(".rt-act .nm b").allTextContents(), ["same@x · Two", "same@x · One"]);

    const activate = async (button, expected) => {
      await reach(page, button);
      await button.evaluate(e => {
        window.fixtureActionDone = false;
        e.addEventListener("click", () => {
          // Capture the current handler in the click's own task: a countdown
          // before the click is allowed to refresh it without losing the join.
          const click = e.onclick;
          e.onclick = function (...args) {
            return window.fixtureAction = click.apply(this, args).finally(() => { window.fixtureActionDone = true; });
          };
        }, { once: true, capture: true });
      });
      const before = f.posts.length;
      await button.click();
      await page.waitForFunction(() => fixtureActionDone);
      await page.evaluate(() => fixtureAction);
      assert.deepEqual(f.posts.slice(before), expected);
    };
    f.failure = "Fixture lift denied";
    await activate(one.locator(".fix button"), [{ key: "seat-one" }]);
    assert.equal(await first.textContent(), f.failure, "errors land on the visible retained button");
    await page.evaluate(() => restCountdown());
    assert.equal(await first.textContent(), f.failure, "a countdown does not erase the failure");
    f.failure = "";
    await activate(one.locator(".fix button"), [{ key: "seat-one" }]);
    await one.locator(".fix").waitFor({ state: "detached" });

    // A model rest's target changes while the old native button is down.
    // Releasing it must not send either the old or the successor's request.
    const changeUnderPress = async (button, mutate) => {
      await reach(page, button);
      const old = await button.elementHandle(), at = await old.boundingBox();
      const before = [f.posts.length, f.opens.length];
      await page.mouse.move(at.x + at.width / 2, at.y + at.height / 2);
      await page.mouse.down();
      mutate();
      await f.publish(page);
      const connected = await old.evaluate(e => e.isConnected);
      await page.mouse.up();
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
      await page.evaluate(() => window.fixtureOpening);
      assert.deepEqual([f.posts.length, f.opens.length], before, "the old press activated no successor");
      assert.equal(connected, false, "a changed target cancels the old press");
      await old.dispose();
    };
    await changeUnderPress(two.locator(".fix button"), () => {
      f.seats[0].rest = { ...f.seats[0].rest, key: "seat-two/model-a", model: "model-a" };
    });
    assert.equal(await two.locator(".st.part").count(), 1);
    await changeUnderPress(two.locator(".fix button"), () => {
      f.seats[0].rest = { ...f.seats[0].rest, key: "seat-two/model-b", model: "model-b" };
      f.seats[0].model = "model-b";
    });
    delete f.seats[0].rest.model;
    await f.publish(page);
    assert.equal(await two.locator(".st.part").count(), 1, "legacy key suffix still classifies the model rest");
    await changeUnderPress(two.locator(".fix button"), () => {
      f.seats[0].rest = { why: "quota", key: "seat-two/model-b", until: rest.until };
      f.seats[0].model = "model-c";
    });
    assert.equal(await two.locator(".st.rest:not(.part)").count(), 1, "same legacy key now represents an account-wide rest");
    await changeUnderPress(two.locator(".fix button"), () => {
      f.seats[0].rest = { why: "verify", key: "seat-two-new", until: rest.until, link: "https://verify.invalid/one" };
    });
    await changeUnderPress(two.locator(".fix button").first(), () => { f.seats[0].rest.link = "https://verify.invalid/two"; });
    await two.locator(".fix button").first().click();
    await page.waitForFunction(() => window.fixtureOpening !== null);
    await page.evaluate(() => window.fixtureOpening);
    assert.deepEqual(f.opens, [{ url: "https://verify.invalid/two" }]);
    assert.deepEqual(await page.evaluate(() => window.fixtureOpenOptions), { target: "_blank", features: "noopener" });

    // A late failure belongs to the removed old action, not its successor.
    f.hold = true;
    const action = two.locator(".fix button").last();
    await reach(page, action);
    await action.evaluate(e => {
      window.lateLiftDone = false;
      e.addEventListener("click", () => {
        const click = e.onclick;
        e.onclick = function (...args) { return window.lateLift = click.apply(this, args).finally(() => { window.lateLiftDone = true; }); };
      }, { once: true, capture: true });
    });
    const sent = page.waitForRequest(r => new URL(r.url()).pathname === "/api/gateway/unrest");
    await action.click(); await sent;
    f.seats[0].rest = { ...f.seats[0].rest, key: "successor" };
    await f.publish(page);
    assert(f.held, "the old lift reply is held");
    await f.held.fulfill({ status: 500, json: { error: "Old target failed" } });
    await page.waitForFunction(() => lateLiftDone);
    await page.evaluate(() => lateLift);
    assert.equal(await two.locator(".fix button").last().textContent(), "It's verified — try it again");
    f.hold = false;
    await activate(two.locator(".fix button").last(), [{ key: "successor" }]);
    await two.locator(".fix").waitFor({ state: "detached" });

    // The exact same model-rest set may be ordered differently by its
    // deadlines. Keep the button, but activate the latest payload order.
    const until = Date.now() + 7200000;
    f.seats = ["model-a", "model-b"].map((model, i) => ({
      ...a, who: "models@x", id: "models@x", model,
      rest: { why: "quota", key: "models/" + model, model, until: new Date(until + i * 60000).toISOString() },
    }));
    await f.publish(page);
    const models = row("models@x"), modelsButton = await models.locator(".fix button").elementHandle();
    f.seats[0].rest.until = new Date(until + 120000).toISOString();
    await f.publish(page);
    assert.equal(await modelsButton.evaluate(e => e.isConnected), true, "same model target set keeps its button across ordering");
    await activate(models.locator(".fix button"), [{ key: "models/model-b" }, { key: "models/model-a" }]);
    await models.locator(".fix").waitFor({ state: "detached" });

    // Six account rows start folded. Folding or filtering out a row must
    // retire its cache entry: restoring that identity gets a fresh row.
    const modelRow = await models.elementHandle(), previous = f.seats;
    f.seats = [...previous, ...Array.from({ length: 5 }, (_, i) => ({ ...a, who: "fold-" + i + "@x", id: "fold-" + i }))];
    await f.publish(page);
    assert.equal(f.seats.some(w => w.who === "models@x"), true, "the folded identity is still in the aggregate");
    assert.equal(await modelRow.evaluate(e => e.isConnected), false);
    assert.equal(await page.locator(".rt-act:not(.rt-keys)").count(), 0, "folded accounts have no actionable cached row on the page");
    f.seats = previous;
    await f.publish(page);
    assert.equal(await modelRow.evaluate(e => e.isConnected), false, "a retired row is not resurrected");
    assert.equal(await models.count(), 1);
    const restored = await models.elementHandle();
    f.seats = [];
    await f.publish(page);
    assert.equal(await restored.evaluate(e => e.isConnected), false, "an absent identity is removed");
    await restored.dispose(); await modelRow.dispose(); await modelsButton.dispose();
    await first.dispose(); await second.dispose();
    assert.deepEqual(errors, []);
  });
  for (const lang of Object.keys(want)) {
    for (const width of [1100, 420]) {
      test(`${engine} ${lang} ${width}px: a restricted WorkBuddy account is told as one, with the way back`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        const context = await browser.newContext({ viewport: { width, height: 900 }, reducedMotion: "reduce" });
        const page = await context.newPage();
        page.setDefaultTimeout(5000);
        const errors = [], lifted = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", serve(lang, lifted));
        t.after(async () => {
          if (process.env.ARTIFACT_DIR) {
            await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
            await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-${width}-wb-restricted.png`), fullPage: true });
          }
          await browser.close();
        });
        await watchCountdown(page);
        await page.goto("http://magpie.test/?view=routing");
        await page.locator(".rt-days .rt-day").nth(1).click();
        await page.locator(".rt-req").first().waitFor();
        await page.locator(".rt-req").first().click();
        await page.locator(".rt-steps li").first().waitFor();
        await page.locator(".rt-steps").evaluate(async (e) => {
          const animations = [];
          for (let n = e; n; n = n.parentElement) animations.push(...n.getAnimations());
          await Promise.all(animations.filter(a => a.effect?.getComputedTiming().iterations !== Infinity).map(a => a.finished));
        });
        const w = want[lang];
        const steps = await page.locator(".rt-steps li").evaluateAll((ls) => ls.map((l) => [l.className, l.textContent]));
        // the try: the label, and why it rests as long as it does
        const told = steps.map(([, s]) => s).filter((s) => s.includes(w.label));
        assert.equal(told.length, 1, JSON.stringify(steps));
        assert(told[0].includes(w.why), told[0]);
        // the vendor's words as they were, and magpie's hint on its own line
        assert.equal(steps.filter(([c]) => c === "aside said").length, 1, JSON.stringify(steps));
        assert(steps.find(([c]) => c === "aside said")[1].endsWith(vendor), JSON.stringify(steps));
        assert.deepEqual(steps.filter(([, s]) => s === w.hint).map(([c]) => c), ["aside"]);
        if (lang !== "en") assert(!steps.some(([, s]) => s.includes("unusual activity")), JSON.stringify(steps));

        // the account's row: resting, restricted, and a way to lift it
        const row = page.locator(".rt-act").filter({ hasText: w.label }).first();
        await row.waitFor();
        assert((await row.locator(".st.rest").textContent()).includes(w.label));
        const go = row.locator(".fix button", { hasText: w.lift });
        await go.waitFor();
        // nothing of it runs past the page's width
        assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), "the page scrolls sideways");
        // scrolled to where a reader sees it, it is the button under the
        // pointer, not the footer
        // Wheel the view, not x=200 (the sidebar in a wide window), and
        // observe each delivered scroll before deciding whether to wheel again.
        // Keep the native press away from the footer's changing status
        // line; the countdown test concerns the retained action itself.
        await wheelTo(page, go, 450);
        const pressed = await go.elementHandle();
        // WebKit's protocol box can be offset after scrolling. The actual
        // native click below must activate this node across the countdown.
        const at = await pressed.evaluate(e => { const r = e.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; });
        await page.mouse.move(at.x, at.y);
        await page.mouse.down();
        const tick = await pressed.evaluate(e => {
          const row = e.closest(".rt-act"), fix = e.parentElement, before = row.querySelector(".st").textContent;
          window.restActivated = window.restDone = false;
          restClock += 60000;
          restCountdown();
          const connected = e.isConnected && row.contains(e) && fix === e.parentElement;
          e.addEventListener("click", () => {
            window.restActivated = true;
            const click = e.onclick;
            e.onclick = function (...args) {
              return window.restFinished = click.apply(this, args).finally(() => { window.restDone = true; });
            };
          }, { once: true, capture: true });
          return { connected, countdownChanged: before !== row.querySelector(".st").textContent };
        });
        await page.mouse.up();
        assert.equal(await page.evaluate(() => restActivated), true, "native down/up across the countdown activates the lift");
        await page.waitForFunction(() => restDone);
        await page.evaluate(() => restFinished);
        assert.deepEqual(lifted, [{ key: rest.key }]);
        assert.equal(tick.connected, true, "the actual countdown keeps the pressed row, fix and button connected");
        assert.equal(tick.countdownChanged, true, "the countdown really updated");
        await pressed.dispose();
        assert.deepEqual(errors, []);
      });
    }
  }
}
