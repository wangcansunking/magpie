// Observe only the API operations a test is about to leave behind. Unlike
// networkidle, a held stream or an unrelated poll is not part of this barrier.
// Each document gets its own ledger, with a separate entry per request. Install
// before navigation; closing/navigating the page discards its wrapper and tasks.
const assert = require("node:assert/strict");

async function watchReplies(page, paths) {
  await page.addInitScript((paths) => {
    const selected = new Set(paths), fetch0 = window.fetch;
    const replies = window.__apiReplies = [];
    window.fetch = function (input, init) {
      const path = new URL(typeof input === "string" ? input : input.url, location.href).pathname;
      const promise = fetch0.apply(this, arguments);
      if (!selected.has(path)) return promise;
      const entry = { id: replies.length, path, method: init?.method || "GET", done: false, error: "" };
      replies.push(entry);
      // api()'s parse and its caller's synchronous redraw run in the following
      // microtasks. A task after consumption observes those, not just headers.
      const consumed = () => setTimeout(() => { entry.done = true; });
      return promise.then((res) => {
        if (!res.ok) entry.error = `${path}: HTTP ${res.status}`;
        if (res.status === 204) consumed();
        const text = res.text;
        res.text = function () {
          return text.call(this).then((body) => {
            try { if (body) JSON.parse(body); } catch { entry.error = `${path}: invalid JSON`; }
            consumed(); return body;
          }, (e) => {
            entry.error = `${path}: ${e.message}`; consumed(); throw e;
          });
        };
        return res;
      }, (e) => {
        entry.error = `${path}: ${e.message}`; consumed(); throw e;
      });
    };
  }, paths);
}

async function repliesConsumed(page, seen = []) {
  // A failed request must fail this check, not look like completed startup or
  // sit out a timeout. Repeated requests are all joined, not collapsed by URL.
  await page.waitForFunction((seen) => {
    const all = window.__apiReplies;
    return all && (all.some((r) => r.error) ||
      seen.every((path) => all.some((r) => r.path === path && r.done)) && all.every((r) => r.done));
  }, seen);
  assert.deepEqual(await page.evaluate(() => __apiReplies.filter((r) => r.error).map((r) => r.error)), [], "observed API operations succeeded");
}

module.exports = { watchReplies, repliesConsumed };
