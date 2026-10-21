// The Gateway plugin's second page. Only the backend owns the tunnel;
// putting this page away cancels its reads, never starts or stops a server.
function registerWANPage(ctx) {
  let page, snapshot = null, fresh = false, busy = "", draft = null, suspended = false, remember = false;
  let generation = 0, actionID = 0, visibilityID = 0, statusVersion = 0, read = null, timer = null, cleanup = null, keysDrawn = "", problem = "";
  const deviceURI = "https://github.com/login/device";
  const visible = () => !suspended && page?.isConnected && !page.hidden && !document.hidden;
  const canChange = () => fresh && snapshot?.controllable && !busy;
  const canEditKeys = () => canChange() && !snapshot?.keysError;
  const pollingAction = () => ["start", "stop", "auth/start", "auth/cancel", "auth/logout", "runtime/install"].includes(busy);
  const authorizing = () => ["requesting_code", "awaiting_authorization"].includes(snapshot?.auth?.state);
  const canInterruptAuth = () => fresh && snapshot?.controllable && busy === "auth/start";
  const canLogout = () => canChange() || canInterruptAuth() || (fresh && snapshot?.controllable && busy === "start");
  const authStates = {
    loading: "Loading WAN sign-in…", signed_out: "Signed out of GitHub for WAN",
    requesting_code: "Requesting a GitHub device code…", awaiting_authorization: "Waiting for GitHub authorization…",
    signed_in: "Signed in to GitHub for WAN", expired: "WAN sign-in expired", error: "WAN sign-in failed",
    signing_out: "Signing out of GitHub for WAN…", unknown: "WAN sign-in status unavailable",
  };
  const states = {
    stopped: "WAN stopped", starting: "Starting WAN Gateway…", running: "WAN running",
    stopping: "Stopping WAN Gateway…", failed: "WAN failed", unknown: "WAN status unavailable",
  };
  function cancelRead() {
    generation++;
    read?.abort(); read = null;
    clearTimeout(timer); timer = null;
  }
  function schedule() {
    clearTimeout(timer);
    if (visible() && (!busy || pollingAction())) timer = setTimeout(refresh, 3000);
  }
  function publish(out) {
    // A copied credential is returned only to its explicit action, never
    // retained in the page's status snapshot or a browser store.
    const { secret, ...masked } = out;
    snapshot = masked; statusVersion++;
    fresh = true; problem = "";
  }
  async function refresh() {
    if (!visible() || (busy && !pollingAction())) return;
    cancelRead();
    const mine = generation, controller = new AbortController();
    read = controller;
    try {
      const out = await api("wan", undefined, undefined, controller.signal);
      if (mine !== generation || !visible()) return;
      publish(out); render();
    } catch (e) {
      if (mine !== generation || !visible() || e.name === "AbortError") return;
      fresh = false; problem = e.message; render();
    } finally {
      if (mine === generation) { read = null; schedule(); }
    }
  }
  async function action(path, body) {
    const interrupt = path === "stop" && busy === "start" && fresh && snapshot?.controllable;
    const interruptAuth = path === "auth/cancel" ? canInterruptAuth() : path === "auth/logout" && canLogout();
    if (!visible() || (!canChange() && !interrupt && !interruptAuth)) return null;
    if ((path.startsWith("keys/") || path === "start") && snapshot?.keysError) return null;
    if (path === "auth/cancel" && (!authorizing() || !Number.isSafeInteger(body.attemptID) || body.attemptID <= 0 || body.attemptID !== snapshot.auth.attemptID)) return null;
    cancelRead();
    const mine = ++actionID, opened = visibilityID, version = statusVersion, before = snapshot;
    busy = path; problem = "";
    if (path === "start" || path === "stop") {
      snapshot = { ...snapshot, state: path === "start" ? "starting" : "stopping", error: "" };
    }
    render(); schedule();
    try {
      const out = await api("wan/" + path, body);
      if (mine !== actionID || opened !== visibilityID || !visible()) return null;
      // A poll may have observed authorization completing while this action's
      // earlier reply was still in flight. Read after completion, never revive
      // its requesting-code (or starting) snapshot over the newer observation.
      if (version !== statusVersion) { await refresh(); return null; }
      cancelRead();
      publish(out);
      if (path === "keys/add-key") draft = null;
      if (path.startsWith("keys/") && path !== "keys/copy-key") renderKeys(true);
      return out;
    } catch (e) {
      if (mine === actionID && opened === visibilityID && visible()) {
        cancelRead();
        if (version === statusVersion && (path === "start" || path === "stop")) snapshot = before;
        problem = e.message; status(t(e.message), "err");
      }
      return null;
    } finally {
      if (mine === actionID) {
        busy = "";
        if (opened === visibilityID && visible()) { render(); schedule(); }
        else if (visible()) refresh();
      }
    }
  }
  const keyAction = (name, body) => action("keys/" + name, body);
  function controls() {
    if (!page) return;
    page.setAttribute("aria-busy", String(!!busy));
    for (const b of page.querySelectorAll("#wanKeys button, #wanKeys input, #addWANKey")) b.disabled = !canEditKeys();
    const state = snapshot?.state, auth = snapshot?.auth;
    // Availability is cached: an external runtime installation can make it
    // stale. Start rechecks locally without downloading; only Install downloads.
    $("#wanStart").disabled = !canChange() || !!snapshot?.keysError || auth?.state !== "signed_in" || !["stopped", "failed"].includes(state);
    $("#wanStop").disabled = !(canChange() || (fresh && busy === "start" && snapshot?.controllable)) || !["running", "starting"].includes(state);
    $("#wanInstall").disabled = !canChange() || snapshot.runtimeAvailable || !["stopped", "failed"].includes(state);
    $("#wanLogin").disabled = !canChange() || !["signed_out", "expired", "error"].includes(auth?.state) || !["stopped", "failed"].includes(state);
    $("#wanRemember").disabled = $("#wanLogin").disabled || !auth?.storageAvailable;
    $("#wanLogout").disabled = !canLogout() || !auth || auth.state === "signing_out";
    const cancel = $("#wanCancelAuth");
    if (cancel) cancel.disabled = !(canChange() || canInterruptAuth()) || !authorizing();
    $("#wanOpenGitHub").disabled = !(canChange() || canInterruptAuth()) || auth?.state !== "awaiting_authorization" || auth.verificationURI !== deviceURI;
  }
  function renderAuth() {
    const auth = snapshot?.auth, current = fresh ? auth?.state : "unknown";
    $("#wanAuth").dataset.state = current || "unknown";
    $("#wanAuthState").textContent = t(authStates[current] || authStates.unknown);
    $("#wanAuthProblem").textContent = t(auth?.error || "");
    $("#wanAuthProblem").hidden = !$("#wanAuthProblem").textContent;
    $("#wanRuntimeState").textContent = t(!fresh ? "WAN runtime status unavailable" : snapshot?.runtimeAvailable ? "WAN runtime installed" : "WAN runtime not installed");
    $("#wanInstall").hidden = !!snapshot?.runtimeAvailable;
    $("#wanInstallRow").hidden = !!snapshot?.runtimeAvailable;
    $("#wanRuntimeHelp").hidden = !!snapshot?.runtimeAvailable;
    $("#wanLoginHelp").hidden = current === "signed_in";
    $("#wanInstall").textContent = t(busy === "runtime/install" ? "Installing WAN runtime…" : "Install WAN runtime");
    const active = ["signed_in", "loading", "requesting_code", "awaiting_authorization", "signing_out"].includes(current);
    $("#wanLogin").hidden = active;
    $("#wanRememberRow").hidden = active || !auth?.storageAvailable;
    $("#wanAuthActions").hidden = active && !authorizing();
    $("#wanLogoutRow").hidden = ["signed_out", "unknown"].includes(current);
    if (!auth?.storageAvailable) remember = false;
    $("#wanRemember").checked = remember;
    const storage = !auth?.storageAvailable ? "System secure storage is unavailable. Sign-in stays in memory until magpie exits."
      : auth.storage === "system" && ["signed_in", "expired", "requesting_code", "awaiting_authorization"].includes(current)
        ? "WAN sign-in uses this computer's system secure storage."
        : "Unless Remember is selected, sign-in stays in memory until magpie exits. No token is saved in the browser.";
    $("#wanStorage").textContent = t(storage);
    const awaiting = current === "awaiting_authorization";
    $("#wanDevice").hidden = !awaiting;
    $("#wanUserCode").textContent = awaiting ? auth.userCode : "";
    // Never turn a returned URI into a navigable link. Only this fixed GitHub
    // device page may be opened, and only by the reader's explicit click.
    $("#wanVerificationURI").textContent = awaiting ? auth.verificationURI : "";
    const deadline = auth?.expiresAt && !auth.expiresAt.startsWith("0001-") ? new Date(auth.expiresAt) : null;
    const expiry = deadline && Number.isFinite(deadline.getTime()) && ["awaiting_authorization", "signed_in", "expired"].includes(current);
    $("#wanAuthExpiry").hidden = !expiry;
    $("#wanAuthExpiry").textContent = expiry ? t(awaiting ? "Device code expires at {time}" : "Sign-in expires at {time}", { time: deadline.toLocaleString(locale) }) : "";
    const attempt = fresh && authorizing() && Number.isSafeInteger(auth.attemptID) && auth.attemptID > 0 ? String(auth.attemptID) : "";
    const slot = $("#wanCancelSlot");
    if (slot.dataset.attempt !== attempt) {
      slot.dataset.attempt = attempt;
      slot.replaceChildren();
      if (attempt) {
        const cancel = el("button", "text", t("Cancel WAN sign-in"));
        cancel.id = "wanCancelAuth";
        cancel.onclick = () => action("auth/cancel", { attemptID: Number(attempt) });
        slot.append(cancel);
      }
    }
    if ($("#wanCancelAuth")) $("#wanCancelAuth").textContent = t("Cancel WAN sign-in");
  }
  function renderStatus() {
    const current = fresh ? snapshot?.state : "unknown";
    $("#wanState").textContent = t(states[current] || states.unknown);
    $("#wanStateDot").classList.toggle("on", current === "running");
    $("#wanStatus").dataset.state = current;
    $("#wanProblem").textContent = t(problem || snapshot?.error || "");
    $("#wanProblem").hidden = !$("#wanProblem").textContent;
    $("#wanOwnerNote").hidden = !snapshot || !!snapshot.controllable;
    const box = $("#wanEndpoint");
    const endpoint = fresh && snapshot?.state === "running" ? snapshot.endpoint : "";
    if (box.dataset.endpoint !== endpoint || box.dataset.locale !== locale) {
      box.dataset.endpoint = endpoint; box.dataset.locale = locale;
      box.replaceChildren(el("code", "", endpoint || t("Available after WAN Gateway starts")));
      if (endpoint) box.append(copyBtn(endpoint, t("Endpoint")));
    }
  }
  function keyForm() {
    const box = el("div", "acc adding"), fields = el("div", "kf"), bar = el("div", "kb");
    const name = input(draft, t("Key name, e.g. Laptop"));
    name.setAttribute("aria-label", t("Gateway key name"));
    name.oninput = () => { draft = name.value; };
    const add = el("button", "text primary", t("Create"));
    const go = async () => {
      const out = await keyAction("add-key", { name: name.value });
      if (out) status(t("WAN key created. Copy it to the client you named."), "ok");
    };
    add.onclick = go;
    const cancel = el("button", "text", t("Cancel"));
    cancel.onclick = () => { draft = null; renderKeys(true); };
    name.onkeydown = (e) => {
      e.stopPropagation();
      if (busy) return;
      if (e.key === "Enter") go();
      else if (e.key === "Escape") cancel.click();
    };
    fields.append(name); bar.append(el("span", "grow"), cancel, add); box.append(fields, bar);
    queueMicrotask(() => name.focus({ preventScroll: true }));
    return box;
  }
  function renderKeys(force = false) {
    const box = $("#wanKeys");
    $("#wanKeysProblem").textContent = t(snapshot?.keysError || "");
    $("#wanKeysProblem").hidden = !$("#wanKeysProblem").textContent;
    // Failed key reads are unknown, not an empty store. Retain the last rows
    // or form without replacing what the reader typed; every control is locked
    // separately, leaving Stop and Sign out available for the known WAN owner.
    if (snapshot?.keysError) {
      box.querySelector(".none")?.remove();
      keysDrawn = "";
      controls();
      return;
    }
    const signature = JSON.stringify([snapshot?.keys, draft === null, locale]);
    // A poll must not replace a name being typed, or the button under the
    // pointer, merely because the tunnel's state changed.
    if (!force && (box.querySelector("input") || keysDrawn === signature)) return;
    keysDrawn = signature;
    const list = el("div", "accts");
    for (const k of snapshot?.keys || []) {
      const row = el("div", "acc" + (k.off ? " off" : " in-use"));
      row.dataset.key = k.id;
      const tick = el("button", "dot tick");
      tick.title = t(k.off ? "Enable key" : "Disable key");
      tick.setAttribute("aria-label", tick.title);
      if (!k.off) tick.append(svg(CHECK, 10, 2.2));
      tick.onclick = () => keyAction(k.off ? "on-key" : "off-key", { key: k.id });
      const rotate = el("button", "text quiet", t("Rotate key"));
      rotate.onclick = () => askGatewayKey(k, true, keyAction, "WAN key rotated. Copy the new key to its clients.");
      const remove = el("button", "text quiet", t("Remove"));
      remove.onclick = () => askGatewayKey(k, false, keyAction);
      row.append(tick, gatewayRename(k, keyAction), el("span", "plan mono", k.masked), el("span", "grow"),
        copyCallerKeyBtn(k, () => keyAction("copy-key", { key: k.id }), "WAN key", "Copy WAN key"), rotate, remove);
      list.append(row);
    }
    if (draft !== null) list.append(keyForm());
    else if (fresh && snapshot && !snapshot.keys?.length) list.append(el("div", "none", t("No WAN keys yet")));
    box.replaceChildren(list);
    controls();
  }
  function renderClient() {
    const endpoint = fresh && snapshot?.state === "running" ? snapshot.endpoint : "https://YOUR_WAN_ENDPOINT";
    // Examples contain placeholders only; copying a key remains an explicit action.
    $("#wanClaudeExample").textContent = `export ANTHROPIC_BASE_URL="${endpoint}"\nexport ANTHROPIC_AUTH_TOKEN="YOUR_WAN_KEY"\nclaude --model "PROVIDER/MODEL"`;
  }
  function render() { renderStatus(); renderAuth(); renderClient(); renderKeys(); controls(); }
  ctx.slots.add("page", {
    key: "wan", title: "WAN Gateway",
    html: `
<p class="usage-note wan-intro" data-t>Connect your clients from anywhere.</p>
<section class="list gwcard wan-status-card">
  <div class="row gw" id="wanStatus" role="status"><span class="dot" id="wanStateDot" aria-hidden="true"></span><div class="who"><div class="name" id="wanState" data-t>WAN status unavailable</div></div><button class="text primary" id="wanStart" data-t disabled>Start</button><button class="text" id="wanStop" data-t disabled>Stop</button></div>
</section>
<p class="usage-note" id="wanOwnerNote" hidden data-t>This window does not own the gateway. WAN controls are unavailable.</p>
<p class="usage-note warn" id="wanProblem" role="alert" hidden></p>
<div class="wan-setup"><div class="wan-setup-card"><div class="row-head"><span class="label" data-t>WAN runtime</span></div>
<section class="list gwcard"><div class="row"><div class="who"><div class="name" id="wanRuntimeState"></div></div></div><div class="row" id="wanInstallRow"><button class="text" id="wanInstall" data-t disabled>Install WAN runtime</button></div></section>
<p class="usage-note" id="wanRuntimeHelp" data-t>Install downloads the WAN runtime. Installation and GitHub sign-in do not publish a tunnel; only Start does.</p>
</div><div class="wan-setup-card"><div class="row-head"><span class="label" data-t>GitHub sign-in for WAN</span></div>
<p class="usage-note" id="wanLoginHelp" data-t>Authorize Microsoft's Dev Tunnels GitHub App for WAN. No application registration is needed. Signing in never starts WAN.</p>
<section class="list gwcard" id="wanAuth" role="status">
  <div class="row"><div class="who"><div class="name" id="wanAuthState"></div></div></div>
  <div class="row" id="wanRememberRow"><label class="klf-check"><input type="checkbox" id="wanRemember"><span data-t>Remember WAN sign-in in system secure storage</span></label></div>
  <div class="row" id="wanAuthActions"><button class="text primary" id="wanLogin" data-t disabled>Sign in to GitHub for WAN</button><span id="wanCancelSlot"></span></div>
  <div id="wanDevice" hidden><div class="row"><div class="who"><div class="name" data-t>Enter this code on GitHub</div><code id="wanUserCode"></code></div></div><div class="row"><div class="who"><span class="usage-note" id="wanVerificationURI"></span></div></div><div class="row"><button class="text" id="wanOpenGitHub" data-t disabled>Open GitHub</button></div></div>
  <div class="row" id="wanLogoutRow"><button class="text" id="wanLogout" data-t disabled>Sign out of WAN</button></div>
</section>
<p class="usage-note" id="wanAuthExpiry" hidden></p>
<p class="usage-note" id="wanStorage"></p>
<p class="usage-note warn" id="wanAuthProblem" role="alert" hidden></p>
</div></div>
<details class="wan-details"><summary data-t>Connection details</summary>
<p class="usage-note" data-t>Stop leaves WAN signed in. Sign out stops WAN and removes its local sign-in; it does not revoke GitHub authorization.</p>
<p class="usage-note" data-t>Start publishes a key-protected HTTPS endpoint through Microsoft devtunnel. Microsoft handles TLS traffic. Dev tunnels is a public preview, not a production SLA. WAN stays off after restarting magpie.</p>
</details>
<div class="row-head"><span class="label" data-t>Client connection</span></div>
<section class="list connect" id="wanConnect"><label data-t>Endpoint</label><div><div class="val" id="wanEndpoint"></div><div class="hint" data-t>In an OpenAI-compatible client, use this Endpoint with /v1 as the base URL and a WAN key as the API key. No connector or device header is needed.</div></div></section>
<div class="wan-client-example"><span class="label">Claude Code · Linux / macOS</span><pre><code id="wanClaudeExample"></code></pre><p class="usage-note" data-t>Replace YOUR_WAN_KEY with a WAN key and PROVIDER/MODEL with a model from your provider.</p></div>
<p class="usage-note" data-t>The endpoint is available only while WAN is running. Its address may change if the tunnel expires or is removed.</p>
<div class="row-head"><span class="label" data-t>WAN keys</span><span class="grow"></span><button class="text" id="addWANKey" data-t disabled>Add WAN key</button></div>
<p class="usage-note wan-assignment" data-t>Give each device its own key. The name is an assignment label, not device enforcement: anyone with a copied Endpoint and key can share it across devices. No IP or hardware tracking is used.</p>
<p class="usage-note warn" id="wanKeysProblem" role="alert" hidden></p>
<section id="wanKeys"></section>
<p class="usage-note" data-t>Disabling, rotating or removing a key takes effect on the next request. Streams already started may finish. Stop ends WAN access and its active requests.</p>
`,
    mount() {
      cleanup?.();
      page = $("#view-wan"); keysDrawn = "";
      $("#wanStart").onclick = () => action("start", {});
      $("#wanStop").onclick = () => action("stop", {});
      $("#wanInstall").onclick = () => action("runtime/install", {});
      $("#wanRemember").onchange = (e) => { remember = e.target.checked; };
      $("#wanLogin").onclick = () => action("auth/start", { remember: remember && !!snapshot?.auth?.storageAvailable });
      $("#wanLogout").onclick = () => action("auth/logout", {});
      $("#wanOpenGitHub").onclick = async () => {
        if ($("#wanOpenGitHub").disabled || !visible()) return;
        const opened = visibilityID;
        try { await api("open", { url: deviceURI }); }
        catch (e) { if (opened === visibilityID && visible()) status(t(e.message), "err"); }
      };
      $("#addWANKey").onclick = () => { if (canEditKeys()) { draft = ""; renderKeys(true); } };
      const changed = () => { if (visible()) refresh(); else { visibilityID++; cancelRead(); fresh = false; } };
      const leaving = () => { suspended = true; changed(); };
      const restored = (e) => { if (e.persisted) { suspended = false; changed(); } };
      const observer = new MutationObserver(changed);
      observer.observe(page, { attributes: true, attributeFilter: ["hidden"] });
      document.addEventListener("visibilitychange", changed);
      window.addEventListener("pagehide", leaving);
      window.addEventListener("pageshow", restored);
      // Remounts (the kernel's retry) clean up explicitly; kernel onDispose
      // has no executor and must not own a polling timer.
      cleanup = () => {
        cancelRead(); observer.disconnect();
        document.removeEventListener("visibilitychange", changed);
        window.removeEventListener("pagehide", leaving);
        window.removeEventListener("pageshow", restored);
      };
      render();
      // Retry remounts a failed relang hook without reopening the page.
      // Resume its visible poll; the initial open still owns its first read.
      schedule();
    },
    open: refresh,
    refresh,
    relang() { keysDrawn = ""; render(); },
  });
}
