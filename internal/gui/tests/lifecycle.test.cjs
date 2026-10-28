// The persistence barrier watches consumed replies, not headers, and never
// turns a failed operation or an earlier reply into successful completion.
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { watchReplies, repliesConsumed } = require("./lifecycle.cjs");

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: explicit reply barrier joins bodies, repeated calls and document lifetimes`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    let page;
    // Release our gate, then close even if a broken page cannot run the bounded
    // release. Never await the pending fetch/body operation during teardown.
    t.after(async () => {
      try {
        if (page && !page.isClosed()) await page.waitForFunction(() => { window.releaseBody?.(); return true; }, null, { timeout: 5000 });
      } finally { await browser.close(); }
    });
    page = await browser.newPage();
    page.setDefaultTimeout(5000);
    await page.route("**/*", (route) => {
      const url = new URL(route.request().url());
      if (url.pathname === "/") return route.fulfill({ contentType: "text/html", body: "<!doctype html><title>reply fixture</title>" });
      if (url.searchParams.has("fetch-error")) return route.abort("failed");
      if (url.searchParams.has("empty")) return route.fulfill({ status: 204 });
      if (url.searchParams.has("error")) return route.fulfill({ status: 503, json: { error: "fixture unavailable" } });
      if (url.searchParams.has("invalid")) return route.fulfill({ contentType: "text/plain", body: "not JSON" });
      return route.fulfill({ json: { value: url.searchParams.get("id") || "ok" } });
    });
    await watchReplies(page, ["/api/tracked"]);
    await page.goto("http://magpie.test/");
    await page.evaluate(() => {
      const text = Response.prototype.text;
      Response.prototype.text = async function () {
        const body = await text.call(this);
        if (this.url.endsWith("id=held")) {
          window.bodyHeld = true;
          await new Promise((resolve) => { window.releaseBody = resolve; });
        }
        return body;
      };
      window.continuations = [];
      window.pendingReplies = Promise.all(["fast", "held"].map((id) =>
        fetch("/api/tracked?id=" + id).then((r) => r.text()).then((body) => continuations.push(JSON.parse(body).value))));
    });
    await page.waitForFunction(() => window.bodyHeld && __apiReplies[0]?.done);
    assert.deepEqual(await page.evaluate(() => __apiReplies.map((r) => ({ id: r.id, done: r.done }))), [
      { id: 0, done: true }, { id: 1, done: false },
    ], "the earlier reply cannot finish a repeated request whose body is held");
    assert.deepEqual(await page.evaluate(() => continuations), ["fast"]);
    // Navigate with that old request still held. Its later completion must
    // belong to the departing document, never the new document's ledger.
    await page.evaluate(() => addEventListener("pagehide", () => {
      sessionStorage.setItem("old-reply-released", "yes");
      releaseBody();
    }, { once: true }));
    await page.reload();
    assert.equal(await page.evaluate(() => sessionStorage.getItem("old-reply-released")), "yes");
    assert.deepEqual(await page.evaluate(() => __apiReplies), [], "a late old-document reply cannot complete a new request");
    await page.evaluate(() => {
      const text = Response.prototype.text;
      Response.prototype.text = async function () {
        const body = await text.call(this);
        if (this.url.endsWith("id=held")) {
          window.bodyHeld = true;
          await new Promise((resolve) => { window.releaseBody = resolve; });
        }
        return body;
      };
      window.continuations = [];
      window.pendingReplies = Promise.all(["fast", "held"].map((id) =>
        fetch("/api/tracked?id=" + id).then((r) => r.text()).then((body) => continuations.push(JSON.parse(body).value))));
    });
    await page.waitForFunction(() => window.bodyHeld && __apiReplies[0]?.done);
    assert.equal(await page.evaluate(() => __apiReplies[1].done), false);
    // Observe the real barrier predicate's first evaluation while the body is
    // held. This is a state handshake, not a pause hoping it hasn't resolved.
    const wait = page.waitForFunction.bind(page);
    page.waitForFunction = (predicate, arg, options) => wait(({ source, arg }) => {
      const ready = (0, eval)("(" + source + ")")(arg);
      (window.barrierChecks ||= []).push(ready);
      return ready;
    }, { source: predicate.toString(), arg }, options);
    const barrier = repliesConsumed(page, ["/api/tracked"]).then(() => null, (error) => error);
    page.waitForFunction = wait;
    try {
      await wait(() => window.barrierChecks?.length > 0);
      assert.equal(await page.evaluate(() => barrierChecks[0]), false, "one finished reply cannot release the barrier while another body is held");
    } finally {
      await page.evaluate(() => releaseBody());
    }
    assert.equal(await barrier, null, "the barrier finishes after the held body is released");
    assert.deepEqual(await page.evaluate(() => continuations), ["fast", "held"], "the barrier includes the consumer continuation");
    await page.evaluate(() => pendingReplies);

    await page.evaluate(() => fetch("/api/unrelated").then((r) => r.text()));
    assert.equal(await page.evaluate(() => __apiReplies.length), 2, "unselected operations stay outside the barrier");
    await page.evaluate(() => fetch("/api/tracked?error=1").then((r) => r.text()));
    await assert.rejects(repliesConsumed(page, ["/api/tracked"]), /observed API operations succeeded/, "HTTP failure is not completion");
    await page.waitForFunction(() => __apiReplies.every((r) => r.done));

    await page.reload();
    assert.deepEqual(await page.evaluate(() => __apiReplies), [], "a new document has no previous request identities");
    await page.evaluate(() => fetch("/api/tracked?invalid=1").then((r) => r.text()));
    await assert.rejects(repliesConsumed(page, ["/api/tracked"]), /observed API operations succeeded/, "invalid JSON is not completion");
    await page.waitForFunction(() => __apiReplies.every((r) => r.done));

    await page.reload();
    await page.evaluate(() => fetch("/api/tracked?empty=1"));
    await repliesConsumed(page, ["/api/tracked"]);
    await page.reload();
    const failure = page.waitForEvent("pageerror");
    await page.evaluate(() => { fetch("/api/tracked?fetch-error=1").then((r) => r.text()); });
    assert.match((await failure).message, /fetch|Load failed|NetworkError/i, "the watcher does not swallow a real fetch rejection");
    await assert.rejects(repliesConsumed(page, ["/api/tracked"]), /observed API operations succeeded/);
    await page.waitForFunction(() => __apiReplies.every((r) => r.done));

    await page.reload();
    const textFailure = page.waitForEvent("pageerror");
    await page.evaluate(() => {
      Response.prototype.text = function () { return Promise.reject(new Error("fixture text read failed")); };
      fetch("/api/tracked").then((r) => r.text());
    });
    assert.match((await textFailure).message, /fixture text read failed/, "the watcher does not swallow a real body rejection");
    await assert.rejects(repliesConsumed(page, ["/api/tracked"]), /observed API operations succeeded/);
    await page.waitForFunction(() => __apiReplies.every((r) => r.done));
  });
}
