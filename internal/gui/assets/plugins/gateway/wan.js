// The Gateway plugin's second page. Only the backend owns the tunnel;
// putting this page away cancels its reads, never starts or stops a server.
function registerWANPage(ctx) {
  let page, snapshot = null, fresh = false, busy = "", draft = null, suspended = false, remember = false;
  let generation = 0, actionID = 0, visibilityID = 0, statusVersion = 0, read = null, timer = null, cleanup = null, keysDrawn = "", problem = "";
  let client = "claude", platform = /Win/i.test(navigator.platform) ? "powershell" : "bash", model = "";
  let modelChoices = "";

  let modelCatalog = null, modelRead = null, modelGeneration = 0, modelsProblem = false;
  const githubURI = "https://github.com/login/device", microsoftURI = "https://microsoft.com/devicelogin";
  const deviceURI = () => snapshot?.auth?.provider === "microsoft" ? [microsoftURI, "https://login.microsoft.com/device"].includes(snapshot.auth.verificationURI) ? snapshot.auth.verificationURI : "https://login.microsoft.com/device" : githubURI;
  const mounted = () => page?.isConnected && !!page.querySelector("#wanClientType") && !!page.querySelector("#wanStatus");
  const visible = () => !suspended && mounted() && !page.hidden && !document.hidden;
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
      refreshModels();
    } catch (e) {
      if (mine !== generation || !visible() || e.name === "AbortError") return;
      fresh = false; problem = e.message; render();
    } finally {
      if (mine === generation) { read = null; schedule(); }
    }
  }
  function cancelModels() {
    modelGeneration++; modelRead?.abort(); modelRead = null; modelCatalog = null; modelsProblem = false;
    if (mounted()) { renderClient(); controls(); }
  }
  async function refreshModels() {
    if (!visible() || modelRead) return;
    const mine = ++modelGeneration, controller = new AbortController();
    modelRead = controller;
    try {
      const out = await api("providers", undefined, undefined, controller.signal);
      if (mine !== modelGeneration || !visible()) return;
      const invalid = p => !p || typeof p.id !== "string" || !Array.isArray(p.models) || p.models.some(m => !m || typeof m.id !== "string");
      if (!out?.gateway || !Array.isArray(out.providers) || out.providers.some(invalid)) throw new Error("WAN model list unavailable");
      const models = gatewayModels(out).filter(m => !decideEntry(m));
      if (models.some(m => typeof (m.name || m.id) !== "string")) throw new Error("WAN model list unavailable");
      // Retain only public model labels/ids, not the provider/account snapshot.
      modelCatalog = models.map(m => ({ id: m.id, name: m.name || m.id })); modelsProblem = false;
    } catch (e) {
      if (mine !== modelGeneration || !visible() || e.name === "AbortError") return;
      modelCatalog = null; modelsProblem = true;
    } finally {
      if (mine === modelGeneration && visible()) { modelRead = null; renderClient(); controls(); }
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
    if (!canEditKeys() && document.querySelector(".wan-key-menu")) closeProtoMenu();
    const state = snapshot?.state, auth = snapshot?.auth;
    // Availability is cached: an external runtime installation can make it
    // stale. Start rechecks locally without downloading; only Install downloads.
    $("#wanStart").disabled = !canChange() || quotaExceeded() || !!snapshot?.keysError || auth?.state !== "signed_in" || !["stopped", "failed"].includes(state);
    $("#wanStop").disabled = !(canChange() || (fresh && busy === "start" && snapshot?.controllable)) || !["running", "starting"].includes(state);
    $("#wanInstall").disabled = !canChange() || snapshot.runtimeAvailable || !["stopped", "failed"].includes(state);
    $("#wanLogin").disabled = !canChange() || !["signed_out", "expired", "error"].includes(auth?.state) || !["stopped", "failed"].includes(state);
    $("#wanRemember").disabled = $("#wanMemory").disabled = $("#wanLogin").disabled || !auth?.storageAvailable;
    $("#wanCopyCode").disabled = !fresh || auth?.state !== "awaiting_authorization" || !auth.userCode;
    $("#wanLogout").disabled = !canLogout() || !auth || auth.state === "signing_out";
    $("#wanRefreshUsage").disabled = !canChange() || auth?.state !== "signed_in" || Date.now() - new Date(snapshot?.usage?.checkedAt || 0).getTime() < 15000;
    $("#wanNewTunnel").disabled = !canChange() || auth?.state !== "signed_in" || !["stopped", "failed"].includes(state) || !snapshot?.tunnel?.id || !!snapshot.tunnel.new;
    $("#wanMicrosoftLogin").disabled = $("#wanLogin").disabled;
    const cancel = $("#wanCancelAuth");
    if (cancel) cancel.disabled = !(canChange() || canInterruptAuth()) || !authorizing();
    $("#wanOpenGitHub").disabled = !(canChange() || canInterruptAuth()) || auth?.state !== "awaiting_authorization" || auth.verificationURI !== deviceURI();
    $("#wanCopyExample").disabled = !fresh || state !== "running" || !wanModels().some(m => m.id === model);
    $("#wanPickModel").disabled = !wanModels().length;
  }
  function renderAuth() {
    const auth = snapshot?.auth, current = fresh ? auth?.state : "unknown";
    $("#wanAuth").dataset.state = current || "unknown";
    $("#wanAuthState").textContent = t(auth?.provider === "microsoft" && current === "requesting_code" ? "Requesting a Microsoft device code…" : auth?.provider === "microsoft" && current === "error" ? "Microsoft WAN sign-in failed" : auth?.provider === "microsoft" && current === "signed_in" ? "Signed in to Microsoft for WAN" : auth?.provider === "microsoft" && current === "awaiting_authorization" ? "Waiting for Microsoft authorization…" : authStates[current] || authStates.unknown);
    $("#wanAuthProblem").textContent = t(auth?.error || "");
    $("#wanAuthProblem").hidden = !$("#wanAuthProblem").textContent;
    $("#wanRuntimeState").textContent = t(!fresh ? "WAN runtime status unavailable" : snapshot?.runtimeAvailable ? "WAN runtime installed" : "WAN runtime not installed");
    $("#wanInstall").hidden = !!snapshot?.runtimeAvailable;
    $("#wanInstallRow").hidden = !!snapshot?.runtimeAvailable;
    $("#wanRuntimeHelp").hidden = !!snapshot?.runtimeAvailable;
    $("#wanRuntimeDetails").dataset.available = String(fresh && !!snapshot?.runtimeAvailable);
    $("#wanLoginHelp").hidden = current === "signed_in";
    $("#wanInstall").textContent = t(busy === "runtime/install" ? "Installing WAN runtime…" : "Install WAN runtime");
    const active = ["signed_in", "loading", "requesting_code", "awaiting_authorization", "signing_out"].includes(current);
    $("#wanLogin").hidden = active;
    $("#wanMicrosoftLogin").hidden = active;
    $("#wanOpenGitHub").textContent = t(auth?.provider === "microsoft" ? "Open Microsoft" : "Open GitHub");
    $("#wanDevicePrompt").textContent = t(auth?.provider === "microsoft" ? "Enter this code on Microsoft" : "Enter this code on GitHub");
    $("#wanAccountName").textContent = snapshot?.usage?.account?.name || t(auth?.provider === "microsoft" ? "Microsoft account" : "GitHub account");
    $("#wanAccountIdentity").hidden = current !== "signed_in";
    $("#wanAccountProvider").textContent = auth?.provider === "microsoft" ? t("Microsoft · Entra ID / Personal") : "GitHub";
    $("#wanRememberRow").hidden = active || !auth?.storageAvailable;
    $("#wanAuthActions").hidden = active && !authorizing();
    $("#wanLogoutRow").hidden = ["signed_out", "unknown", "requesting_code", "awaiting_authorization"].includes(current);
    if (!auth?.storageAvailable) remember = false;
    $("#wanRemember").setAttribute("aria-checked", String(remember));
    $("#wanMemory").setAttribute("aria-checked", String(!remember));
    $("#wanRemember").tabIndex = remember ? 0 : -1;
    $("#wanMemory").tabIndex = remember ? -1 : 0;
    const storage = !auth?.storageAvailable ? "System secure storage is unavailable. Sign-in stays in memory until magpie exits."
      : auth.storage === "system" && ["signed_in", "requesting_code", "awaiting_authorization"].includes(current)
        ? "WAN sign-in uses this computer's system secure storage."
        : remember ? "Restore sign-in after restarting magpie using this computer’s secure storage."
          : "Sign-in clears when magpie exits. Nothing is saved in the browser.";
    $("#wanStorage").textContent = t(storage);
    const awaiting = current === "awaiting_authorization";
    $("#wanDevice").hidden = !awaiting;
    $("#wanUserCode").textContent = awaiting ? auth.userCode : "";
    // Never turn a returned URI into a navigable link. Only this fixed GitHub
    // device page may be opened, and only by the reader's explicit click.
    $("#wanVerificationURI").textContent = awaiting ? auth.verificationURI : "";
    const deadline = typeof auth?.expiresAt === "string" && !auth.expiresAt.startsWith("0001-") ? new Date(auth.expiresAt) : null;
    const expiry = deadline && Number.isFinite(deadline.getTime()) && ["awaiting_authorization", "signed_in", "expired"].includes(current);
    $("#wanDeviceDeadline").textContent = awaiting && expiry ? deadline.toLocaleString(locale, { hour: "2-digit", minute: "2-digit", timeZoneName: "short" }) : "";
    $("#wanDeviceValidity").hidden = !awaiting || !expiry;
    $("#wanDeviceDeadline").dateTime = awaiting && expiry ? deadline.toISOString() : "";
    $("#wanAuthSummary").hidden = !(expiry || current === "signed_in");
    $("#wanAuthExpiry").hidden = !(expiry || current === "signed_in");
    $("#wanAuthExpiryLabel").textContent = t(awaiting ? "Device code validity" : "Current credential validity");
    const minutes = expiry ? Math.max(0, Math.ceil((deadline.getTime() - Date.now()) / 60000)) : 0;
    $("#wanAuthRemaining").textContent = !expiry ? t(current === "signed_in" && !auth.renewal && auth.expiresAt?.startsWith("0001-") ? "No fixed WAN credential expiry" : "Expiry not reported") : !minutes ? t("WAN credential expired")
      : minutes >= 60 ? t("Expires in {hours}h {minutes}m", { hours: Math.floor(minutes / 60), minutes: minutes % 60 })
        : t("Expires in {minutes}m", { minutes });
    const at = $("#wanAuthExpiresAt");
    at.hidden = !expiry;
    at.dateTime = expiry ? deadline.toISOString() : "";
    at.textContent = expiry ? deadline.toLocaleString(locale, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" }) : "";
    $("#wanStorageInfo").hidden = current !== "signed_in";
    $("#wanStorageMode").textContent = t(auth?.storage === "system" ? "Saved on this computer" : "Until magpie exits");
    $("#wanStorage").hidden = current === "signed_in";
    renderRenewal(auth, current, deadline);
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
  function dateText(value) {
    const date = typeof value === "string" && !value.startsWith("0001-") ? new Date(value) : null;
    return date && Number.isFinite(date.getTime()) ? date.toLocaleString(locale, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" }) : "";
  }
  function renderRenewal(auth, current, deadline) {
    const renewal = fresh && ["signed_in", "loading", "expired", "error"].includes(current) ? auth?.renewal : null;
    const state = renewal?.state;
    const supported = ["scheduled", "refreshing", "retrying", "required"].includes(state);
    const signedIn = current === "signed_in", restoring = current === "loading";
    $("#wanLoginHelp").hidden = signedIn || restoring && supported;
    const box = $("#wanRenewal");
    box.hidden = !signedIn && !restoring && !supported;
    box.dataset.state = supported ? state : "unknown";
    let title = "WAN renewal status unavailable", help = "";
    if (state === "required") {
      title = "Sign in to WAN again"; help = "WAN authorization was refused. Sign in again to restore access.";
    } else if (state === "retrying" && (signedIn || restoring)) {
      title = "Retrying WAN renewal";
      help = deadline && deadline.getTime() > Date.now()
        ? "Renewal will retry automatically. The current credential is still valid."
        : "Access is unavailable until renewal succeeds. Start WAN again after recovery.";
    } else if (state === "refreshing" && (signedIn || restoring)) {
      title = "Renewing WAN credentials…"; help = "The backend is renewing this sign-in. No browser authorization is needed.";
    } else if (state === "scheduled" && signedIn) {
      title = "Automatic WAN renewal active"; help = "No periodic sign-in is needed while renewal succeeds.";
    } else if (restoring) {
      title = "Restoring WAN sign-in…";
    } else if (signedIn && !renewal && auth.expiresAt?.startsWith("0001-")) {
      title = "No fixed WAN credential expiry"; help = "GitHub did not set an expiry for this access token.";
    } else if (signedIn && !renewal) {
      title = "Automatic WAN renewal unavailable"; help = "This credential cannot renew. Sign in again to acquire a refresh grant.";
    }
    $("#wanRenewalState").textContent = t(title);
    $("#wanRenewalHelp").textContent = t(help);
    $("#wanAuthHeader").hidden = signedIn || restoring && supported;
    const next = supported && ["scheduled", "retrying"].includes(state) ? dateText(renewal.nextAttemptAt) : "";
    $("#wanRenewalNextRow").hidden = !next;
    $("#wanSessionMeta").hidden = !next && current !== "signed_in";
    $("#wanRenewalNextLabel").textContent = t(state === "retrying" ? "Next WAN retry" : "Next planned WAN renewal");
    $("#wanRenewalNext").textContent = next;
    $("#wanRenewalNext").dateTime = next ? new Date(renewal.nextAttemptAt).toISOString() : "";
    $("#wanExpiryNote").hidden = !supported || state === "required";
    const detail = $("#wanCredentialDetails"), group = current === "awaiting_authorization" ? "code" : signedIn || restoring ? "session" : "other";
    detail.hidden = !signedIn && !(current === "expired" && deadline && Number.isFinite(deadline.getTime()));
    if (detail.dataset.group !== group) {
      detail.dataset.group = group;
      detail.open = true;
    }
  }

  function quotaExceeded() {
    const u = snapshot?.usage;
    return fresh && snapshot?.auth?.state === "signed_in" && u?.state === "ready" && Number.isSafeInteger(u.used) && Number.isSafeInteger(u.limit) && u.used >= u.limit && (!u.resetAt || new Date(u.resetAt).getTime() > Date.now());
  }
  function quotaSize(value) {
    if (!Number.isSafeInteger(value) || value < 0) return t("Not reported");
    return (value / 1073741824).toLocaleString(locale, { minimumFractionDigits: 3, maximumFractionDigits: 3 }) + " GiB";
  }
  function renderUsage() {
    const u = snapshot?.usage, signedIn = fresh && snapshot?.auth?.state === "signed_in";
    const known = signedIn && ["ready", "stale"].includes(u?.state) && Number.isSafeInteger(u.used) && u.used >= 0 && (u.limit === null || Number.isSafeInteger(u.limit) && u.limit >= 0);
    $("#wanQuotaValue").textContent = known ? quotaSize(u.used) + " / " + (u.limit === null ? t("No reported limit") : quotaSize(u.limit)) : "—";
    const ratio = known && u.limit !== null ? u.limit === 0 ? 100 : Math.min(100, u.used / u.limit * 100) : 0;
    const track = $("#wanQuotaTrack"); track.hidden = !known || u.limit === null;
    track.setAttribute("aria-label", t("Account bandwidth")); track.setAttribute("aria-valuemin", "0"); track.setAttribute("aria-valuemax", "100"); track.setAttribute("aria-valuenow", String(ratio));
    $("#wanQuotaFill").style.width = ratio + "%"; track.dataset.exhausted = String(quotaExceeded());
    $("#wanQuotaState").textContent = t(!signedIn ? "Sign in to see account usage" : !known ? u?.state === "loading" ? "Checking account usage…" : "Account usage unavailable" : u.state === "stale" ? "Last known usage · check failed" : quotaExceeded() ? "Bandwidth allowance exhausted" : "Monthly account allowance");
    $("#wanQuotaReset").textContent = known ? dateText(u.resetAt) || t("Not reported") : "—";
    $("#wanQuotaChecked").textContent = known ? dateText(u.checkedAt) || t("Not reported") : "—";
    $("#wanQuotaProblem").textContent = signedIn ? t(u?.error || "") : "";
    $("#wanQuotaProblem").hidden = !$("#wanQuotaProblem").textContent;
  }
  function renderTunnel() {
    const tunnel = fresh ? snapshot?.tunnel : null;
    $("#wanTunnelID").textContent = tunnel?.id || "—";
    $("#wanTunnelRegion").textContent = tunnel?.region || "—";
    $("#wanTunnelPort").textContent = tunnel?.port || "—";
    $("#wanTunnelAccount").textContent = tunnel?.accountName || "—";
    $("#wanTunnelState").textContent = t(tunnel?.new ? "The next Start creates a new tunnel for this account." : tunnel?.id ? "Saved tunnel · reused for this account" : "Start WAN to create this account’s tunnel.");
  }

  function renderStatus() {
    const current = fresh ? snapshot?.state : "unknown";
    $("#wanState").textContent = t(quotaExceeded() ? "Dev Tunnels bandwidth exhausted" : states[current] || states.unknown);
    $("#wanStateDot").classList.toggle("on", current === "running");
    $("#wanStatus").dataset.state = current;
    $("#wanStart").hidden = ["running", "starting", "stopping"].includes(current);
    $("#wanStop").hidden = !["running", "starting", "stopping"].includes(current);
    const note = quotaExceeded() ? "Public requests are blocked by Microsoft. Your provider sign-in is unaffected."
      : current === "failed" ? "WAN is offline. Review the error before starting again."
      : current === "starting" ? "Connecting to Microsoft Dev Tunnels…"
      : current === "stopping" ? "Closing the public connection…"
      : current === "running" ? "Public access is enabled. Requests require a WAN key."
      : !fresh ? "Read the WAN status before making changes."
        : snapshot?.auth?.state !== "signed_in" ? "Choose GitHub or Microsoft to authorize Dev Tunnels. Signing in never starts WAN."
          : "Ready to start. Starting WAN publishes a key-protected public endpoint.";
    $("#wanStatusHelp").textContent = t(snapshot?.keysError && ["stopped", "failed"].includes(current) ? "WAN keys could not be read. Resolve the key error before starting." : note);
    $("#wanProblem").textContent = t(problem || snapshot?.error || "");
    $("#wanProblem").hidden = !$("#wanProblem").textContent || !problem && quotaExceeded() && /429|bandwidth is exhausted/i.test(snapshot?.error || "");
    $("#wanStatusReset").hidden = !quotaExceeded();
    $("#wanStatusReset").textContent = quotaExceeded() ? t("Resets {time}", { time: dateText(snapshot.usage.resetAt) || t("Not reported") }) : "";
    $("#wanOwnerNote").hidden = !snapshot || !!snapshot.controllable;
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
    page.querySelector(".wan-key-head").hidden = !fresh || !!snapshot?.keysError || !snapshot?.keys?.length;
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
      const row = el("div", "acc wan-key-row" + (k.off ? " off" : " in-use"));
      row.dataset.key = k.id;
      const tick = el("button", "text wan-key-state tick", t(k.off ? "WAN key disabled" : "WAN key enabled"));
      tick.title = t(k.off ? "Enable key" : "Disable key");
      tick.setAttribute("aria-label", tick.title);
      tick.onclick = () => keyAction(k.off ? "on-key" : "off-key", { key: k.id });
      const more = el("button", "text wan-key-more", "⋯");
      more.setAttribute("aria-label", t("WAN key actions"));
      more.setAttribute("aria-haspopup", "menu");
      more.setAttribute("aria-expanded", "false");
      more.onclick = () => {
        if (!canEditKeys()) return;
        openProtoMenu(more, [{ v: "rotate", name: "Rotate key" }, { v: "remove", name: "Remove" }], "", value => {
          const current = snapshot?.keys?.find(key => key.id === k.id);
          if (!visible() || !canEditKeys() || !current) return;
          askGatewayKey(current, value === "rotate", keyAction, "WAN key rotated. Copy the new key to its clients.");
        }, "WAN key actions", "wan-key-menu", "right");
      };
      const actions = el("div", "wan-key-actions");
      const copyKey = copyCallerKeyBtn(k, () => keyAction("copy-key", { key: k.id }), "WAN key", "Copy WAN key");
      const copyAction = copyKey.onclick;
      copyKey.onclick = async () => {
        if (!canEditKeys()) return;
        try { await copyAction(); } finally { controls(); }
      };
      copyKey.append(el("span", "", t("Copy")));
      actions.append(copyKey, more);
      row.append(gatewayRename(k, keyAction), tick, el("span", "plan mono wan-key-mask", k.masked), actions);
      list.append(row);
    }
    if (draft !== null) list.append(keyForm());
    else if (fresh && snapshot && !snapshot.keys?.length) list.append(el("div", "none", t("No WAN keys yet")));
    if (document.querySelector(".wan-key-menu")) closeProtoMenu();
    box.replaceChildren(list);
    controls();
  }
  function wanModels() {
    return modelCatalog || [];
  }
  function shellQuote(value) { return "'" + String(value).replaceAll("'", platform === "powershell" ? "''" : "'\\''") + "'"; }
  function renderClient() {
    const choices = wanModels();
    const signature = JSON.stringify(choices.map(m => [m.id, m.name]));
    if (signature !== modelChoices && document.querySelector(".wan-model-menu")) closeProtoMenu();
    modelChoices = signature;
    if ((modelCatalog || modelsProblem) && !choices.some(m => m.id === model)) model = "";
    for (const [id, on] of [["wanClientClaude", client === "claude"], ["wanClientOpenAI", client === "openai"], ["wanPowerShell", platform === "powershell"], ["wanBash", platform === "bash"]]) $("#" + id).setAttribute("aria-pressed", String(on));
    $("#wanPickModel").textContent = model || t("Choose a WAN model");
    $("#wanClientType").setAttribute("aria-label", t("WAN client type"));
    $("#wanExamplePlatform").setAttribute("aria-label", t("WAN example platform"));
    $("#wanModelHelp").textContent = t(modelsProblem ? "Unable to load WAN models. Other WAN controls remain available."
      : !modelCatalog ? "Loading WAN models…" : choices.length ? "Use a WAN key below with a model from your signed-in providers." : "No WAN models are available. Add a provider or sign in to a subscription in Providers.");
    const endpoint = fresh && snapshot?.state === "running" ? snapshot.endpoint : "https://YOUR_WAN_ENDPOINT";
    const base = client === "openai" ? endpoint + "/v1" : endpoint;
    const box = $("#wanEndpoint");
    if (box.dataset.client !== client || box.dataset.base !== base || box.dataset.locale !== locale) {
      box.dataset.client = client; box.dataset.base = base; box.dataset.locale = locale;
      box.replaceChildren(el("code", "", fresh && snapshot?.state === "running" ? base : t("Available after WAN Gateway starts")));
      if (fresh && snapshot?.state === "running") {
        const copyAddress = copyBtn(base, t("WAN client address"));
        copyAddress.append(el("span", "", t("Copy WAN address")));
        box.append(copyAddress);
      }
    }
    const picked = model || "PROVIDER/MODEL";
    let example;
    if (client === "claude") {
      const prefix = platform === "powershell" ? "$env:" : "export ";
      example = `${prefix}ANTHROPIC_BASE_URL=${shellQuote(endpoint)}\n${prefix}ANTHROPIC_AUTH_TOKEN=${shellQuote("YOUR_WAN_KEY")}\nclaude --model ${shellQuote(picked)}`;
    } else {
      const body = JSON.stringify({ model: picked, messages: [{ role: "user", content: "Hello" }] });
      example = platform === "powershell"
        ? `Invoke-RestMethod -Uri ${shellQuote(base + "/chat/completions")} -Method Post -Headers @{Authorization=${shellQuote("Bearer YOUR_WAN_KEY")}} -ContentType 'application/json' -Body ${shellQuote(body)}`
        : `curl ${shellQuote(base + "/chat/completions")} \\\n  -H ${shellQuote("Authorization: Bearer YOUR_WAN_KEY")} \\\n  -H 'Content-Type: application/json' \\\n  -d ${shellQuote(body)}`;
    }
    // A copied example contains placeholders, never a key read from the backend.
    $("#wanClaudeExample").textContent = example;
  }
  function render() { renderStatus(); renderAuth(); renderUsage(); renderTunnel(); renderClient(); renderKeys(); controls(); }
  ctx.slots.add("page", {
    key: "wan", title: "WAN Gateway",
    html: `
<p class="usage-note wan-intro" data-t>Let remote clients use your models.</p>
<section class="list gwcard wan-status-card">
  <div class="row gw" id="wanStatus" role="status"><span class="dot" id="wanStateDot" aria-hidden="true"></span><div class="who"><div class="name" id="wanState" data-t>WAN status unavailable</div><div class="usage-note" id="wanStatusHelp"></div></div><span class="usage-note" id="wanStatusReset" hidden></span><button class="text primary" id="wanStart" data-t disabled>Start</button><button class="text" id="wanStop" data-t disabled>Stop</button></div>
</section>
<p class="usage-note" id="wanOwnerNote" hidden data-t>This window does not own the gateway. WAN controls are unavailable.</p>
<p class="usage-note warn" id="wanProblem" role="alert" hidden></p>
<div class="wan-overview">
<aside class="wan-setup-card wan-session-card"><div class="row-head"><span class="label" data-t>Dev Tunnels account</span></div>
<p class="usage-note" id="wanLoginHelp" data-t>Choose GitHub or Microsoft to authorize Dev Tunnels. Signing in never starts WAN.</p>
<section class="list gwcard" id="wanAuth" role="status"><div id="wanAccountIdentity" class="wan-account-identity" hidden><span class="wan-account-avatar" aria-hidden="true">◎</span><div><strong id="wanAccountName"></strong><div class="usage-note" id="wanAccountProvider"></div></div></div>
  <div class="wan-auth-header" id="wanAuthHeader"><div class="wan-auth-status"><span class="wan-auth-dot" aria-hidden="true"></span><div class="name" id="wanAuthState"></div></div></div>
  <div class="wan-renewal" id="wanRenewal" hidden><div class="wan-renewal-title"><span class="wan-renewal-mark" aria-hidden="true">↻</span><strong id="wanRenewalState"></strong></div><p class="usage-note" id="wanRenewalHelp"></p></div>
  <dl class="wan-session-meta" id="wanSessionMeta"><div id="wanRenewalNextRow" hidden><dt id="wanRenewalNextLabel"></dt><dd><time id="wanRenewalNext"></time></dd></div><div id="wanStorageInfo" hidden><dt data-t>Login storage</dt><dd id="wanStorageMode"></dd></div></dl>
  <section id="wanCredentialDetails" class="wan-credential-details"><dl class="wan-auth-summary" id="wanAuthSummary" hidden><div id="wanAuthExpiry" hidden><dt id="wanAuthExpiryLabel"></dt><dd><time id="wanAuthExpiresAt"></time><span id="wanAuthRemaining"></span></dd></div></dl><p class="usage-note" id="wanExpiryNote" data-t>Renewal updates this access-token deadline. It is not a fixed sign-in expiry.</p></section>
  <div id="wanLogoutRow"><button class="text" id="wanLogout" data-t disabled>Sign out of WAN</button><p class="usage-note" data-t>Sign out stops WAN and removes its saved login on this computer.</p></div>
  <div class="wan-storage-choice" id="wanRememberRow"><div class="name" id="wanRememberLabel" data-t>Login storage</div><div class="wan-client-tabs" role="radiogroup" aria-labelledby="wanRememberLabel"><button class="text" id="wanMemory" role="radio" aria-checked="true" data-t>Until magpie exits</button><button class="text" id="wanRemember" role="radio" aria-checked="false" data-t>Remember on this computer</button></div></div>
  <div class="row" id="wanAuthActions"><button class="text primary" id="wanLogin" data-t disabled>Sign in to GitHub for WAN</button><button class="text" id="wanMicrosoftLogin" data-t disabled>Sign in with Microsoft</button><span id="wanCancelSlot"></span></div>
  <div id="wanDevice" hidden><div class="row"><div class="who"><div class="name" id="wanDevicePrompt" data-t>Enter this code on GitHub</div><div class="wan-device-code"><code id="wanUserCode"></code><button class="text" id="wanCopyCode" aria-describedby="wanUserCode" data-t disabled>Copy</button></div></div></div><p class="usage-note" id="wanDeviceValidity"><span data-t>Device code validity</span> · <time id="wanDeviceDeadline"></time></p><div class="row"><div class="who"><span class="usage-note" id="wanVerificationURI"></span></div></div><div class="row"><button class="text primary" id="wanOpenGitHub" data-t disabled>Open GitHub</button></div></div>
</section>
<p class="usage-note" id="wanStorage"></p>
<p class="usage-note warn" id="wanAuthProblem" role="alert" hidden></p>
<section id="wanRuntimeDetails" class="wan-runtime-details"><div class="wan-runtime-status" role="status"><span class="wan-runtime-dot" aria-hidden="true"></span><span id="wanRuntimeState"></span></div><div class="row" id="wanInstallRow"><button class="text" id="wanInstall" data-t disabled>Install WAN runtime</button></div><p class="usage-note" id="wanRuntimeHelp" data-t>Install downloads the WAN runtime. Installation and GitHub sign-in do not publish a tunnel; only Start does.</p></section>
</aside>
<section class="wan-quota-card"><div class="row-head"><span class="label" data-t>Account bandwidth</span><span class="grow"></span><button class="text" id="wanRefreshUsage" data-t disabled>Refresh usage</button></div><div class="wan-quota-value" id="wanQuotaValue"></div><div class="wan-quota-track" id="wanQuotaTrack" role="meter"><span id="wanQuotaFill"></span></div><p class="usage-note" id="wanQuotaState"></p><dl class="wan-session-meta"><div><dt data-t>Reset time</dt><dd id="wanQuotaReset"></dd></div><div><dt data-t>Last checked</dt><dd id="wanQuotaChecked"></dd></div></dl><p class="usage-note warn" id="wanQuotaProblem" role="alert" hidden></p><p class="usage-note" data-t>Monthly allowance is shared across this account. Changing a model or WAN key does not reset it.</p></section>
</div>
<section class="wan-tunnel-card"><div class="row-head"><span class="label" data-t>Tunnel</span><span class="grow"></span><button class="text" id="wanNewTunnel" data-t disabled>Use a new tunnel</button></div><p class="usage-note" id="wanTunnelState"></p><dl class="wan-tunnel-grid"><div><dt data-t>Tunnel ID</dt><dd id="wanTunnelID"></dd></div><div><dt data-t>Region</dt><dd id="wanTunnelRegion"></dd></div><div><dt data-t>Port</dt><dd id="wanTunnelPort"></dd></div><div><dt data-t>Account</dt><dd id="wanTunnelAccount"></dd></div></dl><p class="usage-note" data-t>Each account keeps its own tunnel. A new tunnel changes the address and keeps WAN keys.</p></section>
<section class="wan-client-card">
<div class="row-head"><span class="label" data-t>Client connection</span></div>
<div class="wan-client-tabs" id="wanClientType" role="group"><button class="text" id="wanClientClaude" aria-pressed="true">Claude Code</button><button class="text" id="wanClientOpenAI" aria-pressed="false" data-t>OpenAI-compatible WAN client</button></div>
<section class="list connect" id="wanConnect"><label data-t>WAN client address</label><div><div class="val" id="wanEndpoint"></div></div></section>
<div class="wan-example-options"><div class="wan-client-tabs" id="wanExamplePlatform" role="group"><button class="text" id="wanPowerShell" aria-pressed="false">PowerShell</button><button class="text" id="wanBash" aria-pressed="false">Bash</button></div><div class="wan-model-picker"><span data-t>Model</span><button class="text" id="wanPickModel" aria-haspopup="menu" aria-expanded="false" disabled></button></div></div>
<p class="usage-note" id="wanModelHelp"></p>
<div class="wan-client-example"><div class="wan-example-toolbar"><span class="usage-note" data-t>WAN client example</span><button class="text" id="wanCopyExample" data-t disabled>Copy WAN example</button></div><pre><code id="wanClaudeExample"></code></pre></div>
<p class="usage-note" data-t>The endpoint is available only while WAN is running. Its address may change if the tunnel expires or is removed.</p>
</section>
<section class="wan-keys-card">
<div class="row-head"><span class="label" data-t>WAN keys</span><span class="grow"></span><button class="text" id="addWANKey" data-t disabled>Add WAN key</button></div>
<p class="usage-note wan-assignment" data-t>Use separate WAN keys for different clients. Names are labels, not device binding.</p>
<p class="usage-note warn" id="wanKeysProblem" role="alert" hidden></p>
<div class="wan-key-head"><span data-t>Name</span><span data-t>WAN key state</span><span data-t>Masked WAN key</span><span data-t>WAN key actions</span></div>
<section id="wanKeys"></section>
<p class="usage-note" data-t>Disabling, rotating or removing a key takes effect on the next request. Streams already started may finish. Stop ends WAN access and its active requests.</p>
</section>
<div class="wan-info-footer"><details class="wan-details"><summary data-t>Connection details</summary><p class="usage-note" data-t>Start publishes a key-protected HTTPS endpoint through Microsoft devtunnel. Microsoft handles TLS traffic. Dev tunnels is a public preview, not a production SLA. WAN stays off after restarting magpie.</p></details><p class="usage-note" data-t>Provider sign-in supplies models. Dev Tunnels sign-in only establishes the WAN tunnel.</p></div>
`,
    mount() {
      cleanup?.();
      page = $("#view-wan"); keysDrawn = "";
      $("#wanStart").onclick = () => action("start", {});
      $("#wanStop").onclick = () => action("stop", {});
      $("#wanInstall").onclick = () => action("runtime/install", {});
      for (const id of ["wanMemory", "wanRemember"]) $("#" + id).onclick = () => {
        if ($("#" + id).disabled) return;
        remember = id === "wanRemember";
        renderAuth();
      };
      for (const id of ["wanMemory", "wanRemember"]) $("#" + id).onkeydown = e => {
        if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"].includes(e.key)) return;
        e.preventDefault();
        const next = $("#" + (e.key === "Home" ? "wanMemory" : e.key === "End" ? "wanRemember" : remember ? "wanMemory" : "wanRemember"));
        if (next.disabled) return;
        next.click(); next.focus({ preventScroll: true });
      };
      $("#wanCopyCode").onclick = () => {
        if ($("#wanCopyCode").disabled || !visible()) return;
        copy(snapshot.auth.userCode, t("Code"), $("#wanCopyCode"));
      };
      $("#wanLogin").onclick = () => action("auth/start", { remember: remember && !!snapshot?.auth?.storageAvailable });
      $("#wanMicrosoftLogin").onclick = () => action("auth/start", { remember: remember && !!snapshot?.auth?.storageAvailable, provider: "microsoft" });
      $("#wanRefreshUsage").onclick = () => action("usage/refresh", {});
      $("#wanNewTunnel").onclick = async () => {
        if ($("#wanNewTunnel").disabled || !visible()) return;
        const selected = snapshot.tunnel.id, account = snapshot.usage?.account?.id, opened = visibilityID;
        if (await confirmAction(t("Use a new tunnel?"), t("The next Start creates a new address for this account. Update your clients afterward. WAN keys and other accounts’ tunnels are kept."), t("Use a new tunnel"))) {
          if (visible() && opened === visibilityID && selected === snapshot?.tunnel?.id && account === snapshot?.usage?.account?.id && !$("#wanNewTunnel").disabled) await action("tunnel/new", {});
        }
      };
      $("#wanLogout").onclick = () => action("auth/logout", {});
      $("#wanOpenGitHub").onclick = async () => {
        if ($("#wanOpenGitHub").disabled || !visible()) return;
        const opened = visibilityID;
        try { await api("open", { url: deviceURI() }); }
        catch (e) { if (opened === visibilityID && visible()) status(t(e.message), "err"); }
      };
      for (const [id, value] of [["wanClientClaude", "claude"], ["wanClientOpenAI", "openai"]]) $("#" + id).onclick = () => { client = value; renderClient(); controls(); };
      for (const [id, value] of [["wanPowerShell", "powershell"], ["wanBash", "bash"]]) $("#" + id).onclick = () => { platform = value; renderClient(); controls(); };
      $("#wanCopyExample").onclick = () => {
        const button = $("#wanCopyExample");
        if (!button.disabled) copy($("#wanClaudeExample").textContent, t("WAN client example"), button);
      };
      $("#wanPickModel").onclick = () => {
        const choices = wanModels();
        if (!choices.length) return;
        openProtoMenu($("#wanPickModel"), choices.map(m => ({ v: m.id, name: m.name || m.id, note: m.id, literalName: true })), model, value => {
          if (!visible() || !wanModels().some(m => m.id === value)) return;
          model = value; renderClient(); controls();
        }, "Choose a WAN model", "wan-model-menu", "right", false, "Search WAN models");
      };
      $("#addWANKey").onclick = () => { if (canEditKeys()) { draft = ""; renderKeys(true); } };
      const closeMenus = () => { if (document.querySelector(".wan-key-menu, .wan-model-menu")) closeProtoMenu(); };
      const changed = () => { if (visible()) refresh(); else { visibilityID++; cancelRead(); fresh = false; cancelModels(); closeMenus(); } };
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
        closeMenus();
        cancelModels();
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
