const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const kind of ["protocol", "session"]) test(`${engine}: Escape from ${kind} menu restores focus without revealing a moved anchor`, async t => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 1100, height: 800 } });
    const errors = [];
    page.on("pageerror", e => errors.push(e.message));
    await page.route("**/*", fixture("en", "light", [], { lan: true }));
    await page.goto("http://magpie.test/?view=gateway");
    await page.locator("#gatewayKeys .acc[data-key]").first().waitFor();
    await page.evaluate(kind => {
      const scroller = document.createElement("div");
      scroller.id = "fixtureMenuScroll";
      Object.assign(scroller.style, { position: "fixed", top: "80px", left: "260px", width: "300px", height: "160px", overflow: "auto", overflowAnchor: "none", background: "white", zIndex: "5" });
      const before = document.createElement("div"), anchor = document.createElement("button"), after = document.createElement("div");
      before.id = "fixtureMenuGrowth"; anchor.id = "fixtureMenuAnchor"; anchor.textContent = "Fixture menu";
      after.style.height = "1200px";
      anchor.onclick = () => {
        const options = [{ v: "one", name: "One", note: "First" }, { v: "two", name: "Two", note: "Second" }];
        if (kind === "protocol") openProtoMenu(anchor, options, "one", () => {}, "Name");
        else openSessCombo(anchor, "All", options, "one", () => {}, "Name");
      };
      scroller.append(before, anchor, after); document.body.append(scroller);
    }, kind);
    await page.locator("#fixtureMenuAnchor").click();
    await page.locator(".proto-menu").waitFor();
    // Hold the open menu while content moves its anchor below the viewport.
    // Returning focus must not turn that unrelated layout change into a scroll.
    await page.evaluate(() => {
      document.querySelector("#fixtureMenuGrowth").style.height = "900px";
      document.querySelector("#fixtureMenuScroll").scrollTop = 0;
    });
    await page.keyboard.press("Escape");
    const state = await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => resolve({
      top: document.querySelector("#fixtureMenuScroll").scrollTop,
      focused: document.activeElement === document.querySelector("#fixtureMenuAnchor"),
      closed: !document.querySelector(".proto-menu"),
    }))));
    assert.deepEqual(state, { top: 0, focused: true, closed: true });
    assert.deepEqual(errors, []);
  });
}
