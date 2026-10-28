// A reply barrier joins body consumption and its caller, never just headers.
const assert = require("node:assert/strict");
const http = require("node:http");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { watchReplies, repliesConsumed } = require("./lifecycle.cjs");

for (const engine of ["chromium", "webkit"]) {
  test(`${engine}: reply barriers join JSON and text consumers`, async (t) => {
    const held = Promise.withResolvers();
    let browser;
    const server = http.createServer(async (req, res) => {
      if (req.url === "/") { res.end("<!doctype html><title>Reply fixture</title>"); return; }
      res.setHeader("Content-Type", "application/json");
      if (req.url === "/json") {
        res.flushHeaders();
        // WebKit delivers response metadata after some body bytes arrive.
        // Keep the JSON incomplete so neither engine can finish consuming it.
        res.write(" ".repeat(2048) + '{"value":');
        await held.promise;
        res.end("7}");
      } else if (req.url === "/text") res.end('{"value":8}');
      else if (req.url === "/bad") res.end("{");
      else { res.statusCode = 404; res.end("{}"); }
    });
    await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
    t.after(async () => {
      held.resolve();
      try { await browser?.close(); }
      finally {
        server.closeAllConnections();
        await new Promise(resolve => server.close(resolve));
      }
    });
    browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    const page = await browser.newPage();
    page.setDefaultTimeout(5000);
    await watchReplies(page, ["/json", "/text", "/bad"]);
    await page.goto(`http://127.0.0.1:${server.address().port}/`);
    const headers = page.waitForResponse(r => new URL(r.url()).pathname === "/json");
    await page.evaluate(() => {
      window.reading = fetch("/json").then(r => r.json()).then(body => { window.applied = body.value; });
    });
    await headers;
    assert.equal(await page.evaluate(() => __apiReplies[0].done), false, "headers cannot complete a held JSON read");
    held.resolve();
    await repliesConsumed(page, ["/json"]);
    assert.equal(await page.evaluate(() => window.applied), 7, "the JSON caller has applied its reply");
    await page.evaluate(() => {
      window.reading = fetch("/text").then(r => r.text()).then(body => { window.applied = JSON.parse(body).value; });
    });
    await repliesConsumed(page, ["/text"]);
    assert.equal(await page.evaluate(() => window.applied), 8, "text callers retain the same barrier");
    await page.evaluate(() => {
      window.reading = fetch("/bad").then(r => r.json()).catch(() => { window.rejected = true; });
    });
    await page.waitForFunction(() => window.rejected && __apiReplies.find(r => r.path === "/bad")?.done);
    assert.equal(await page.evaluate(() => __apiReplies.find(r => r.path === "/bad").error), "/bad: invalid JSON");
    await assert.rejects(() => repliesConsumed(page, ["/bad"]), /observed API operations succeeded/);
  });
}
