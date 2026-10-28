// The no-scroll reader helper still sends trusted pointer input and refuses
// covered, disabled or offscreen targets. It must not weaken app assertions
// into forced clicks or synthetic DOM events.
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { pointerClick } = require("./reader.cjs");

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: reader pointer requires an enabled uncovered target and never scrolls it`, async t => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 800, height: 600 } });
    page.setDefaultTimeout(5000);
    await page.setContent(`<div id="view" style="position:fixed;left:100px;top:80px;width:300px;height:160px;overflow:auto;overflow-anchor:none">
      <div id="before"></div><button id="target" style="height:40px;width:160px"><span>Pointer target</span></button><div style="height:1200px"></div></div>
      <script>window.clicks=[]; document.querySelector('#target').onclick=e=>clicks.push({trusted:e.isTrusted,detail:e.detail});</script>`);
    const target = page.locator("#target");
    await pointerClick(page, target);
    assert.deepEqual(await page.evaluate(() => clicks), [{ trusted: true, detail: 1 }]);
    assert.equal(await page.locator("#view").evaluate(e => e.scrollTop), 0);

    await target.evaluate(e => { e.disabled = true; });
    await assert.rejects(pointerClick(page, target), /not enabled and uncovered/);
    await target.evaluate(e => { e.disabled = false; });
    await target.evaluate(e => { e.setAttribute("aria-disabled", "true"); });
    await assert.rejects(pointerClick(page, target), /not enabled and uncovered/);
    await target.evaluate(e => { e.removeAttribute("aria-disabled"); });
    await page.evaluate(() => {
      const cover = document.createElement("div"); cover.id = "cover";
      Object.assign(cover.style, { position: "fixed", left: "100px", top: "80px", width: "300px", height: "160px", zIndex: "10" });
      document.body.append(cover);
    });
    await assert.rejects(pointerClick(page, target), /not enabled and uncovered/);
    await page.locator("#cover").evaluate(e => e.remove());
    await page.locator("#before").evaluate(e => { e.style.height = "900px"; });
    await assert.rejects(pointerClick(page, target), /not enabled and uncovered/);
    assert.equal(await page.locator("#view").evaluate(e => e.scrollTop), 0, "offscreen input must not reveal itself");
    assert.deepEqual(await page.evaluate(() => clicks), [{ trusted: true, detail: 1 }], "refused targets received no click");
  });
}
