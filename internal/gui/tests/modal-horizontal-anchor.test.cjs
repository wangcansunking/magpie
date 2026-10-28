// A horizontal scroll caused by content reflow must not discard the modal's
// vertical anchor. Vertical scrolling still rebases it in either direction.
const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");
const { pointerClick } = require("./reader.cjs");

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: horizontal modal scroll keeps its vertical anchor while vertical scroll rebases it`, async t => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 900, height: 900 }, reducedMotion: "reduce" });
    page.setDefaultTimeout(5000);
    await page.route("**/*", fixture("en", "light", [], { lan: true }));
    await page.goto("http://magpie.test/?view=gateway");
    await page.locator("#gatewayKeys .acc[data-key]").first().waitFor();
    await page.evaluate(() => {
      const ed = document.createElement("div"); ed.className = "editor framed";
      ed.innerHTML = '<div class="ehead"><b>Scroll fixture</b></div><div class="ebody" id="fixtureScroll" style="height:200px;flex:none;overflow:auto;overflow-anchor:none"><div style="width:1400px"><div id="fixtureGrowth" style="height:80px"></div><button id="fixtureAnchor" style="height:30px;width:160px">Anchor</button><div style="height:600px"></div></div></div><div class="bar">Scroll fixture</div>';
      openModal(ed);
    });
    await page.evaluate(async () => {
      await Promise.all(document.getAnimations().map(a => a.finished.catch(() => {})));
      await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
    });
    await pointerClick(page, page.locator("#fixtureAnchor"));
    const state = () => page.evaluate(() => {
      const scroller = document.getElementById("fixtureScroll");
      return { anchor: document.getElementById("fixtureAnchor").getBoundingClientRect().top,
        dialog: document.querySelector("#modal .dialog").getBoundingClientRect().top,
        top: scroller.scrollTop, left: scroller.scrollLeft };
    });
    const start = await state();
    for (const top of [0, 25, 0]) {
      await page.evaluate(top => {
        const scroller = document.getElementById("fixtureScroll");
        scroller.scrollTop = top;
        scroller.dispatchEvent(new Event("scroll"));
        holdModal();
      }, top);
      const before = await state();
      assert.equal(before.top, top);
      assert(Math.abs(before.anchor - (start.anchor - top)) < 0.5, "vertical input moves the anchor rather than the dialog, including back to zero");
      await page.evaluate(() => {
        const scroller = document.getElementById("fixtureScroll"), growth = document.getElementById("fixtureGrowth");
        growth.style.height = parseFloat(growth.style.height) + 17.25 + "px";
        scroller.scrollLeft += 80;
        // Deliver the scroll before the resize observer's fit, as WebKit
        // does when reflow clamps horizontal overflow on a pressed control.
        scroller.dispatchEvent(new Event("scroll"));
        holdModal();
      });
      const after = await state();
      assert.equal(after.top, top, "horizontal reflow leaves vertical scroll alone");
      assert.equal(after.left, before.left + 80);
      assert(Math.abs(after.anchor - before.anchor) < 0.5, `horizontal scroll lost the vertical anchor: ${before.anchor} to ${after.anchor}`);
    }
  });
}
