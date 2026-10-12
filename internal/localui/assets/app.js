"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  const refreshSelects = (...ids) => {
    if (!window.MyWhatsAppSelects) return;
    for (const id of ids) window.MyWhatsAppSelects.refresh($(id));
  };
  const refreshAllSelects = () => {
    if (!window.MyWhatsAppSelects) return;
    for (const select of document.querySelectorAll("select")) window.MyWhatsAppSelects.refresh(select);
  };
  let csrf = "";
  let remoteMode = false;
  let wizardSnapshot = null;
  let wizardVersion = 0;
  let wizardBusy = false;
  let wizardTimer = null;
  let wizardConfirmation = "";
  let setup = null;
  let status = null;
  let pairingSnapshot = null;
  let chats = [];
  let nextCursor = "";
  let activeQuery = "";
  let searchRequest = null;
  let visibleMessages = [];
  let selectedMessage = "";
  let searching = false;
  let searchVersion = 0;
  let contextVersion = 0;
  let pairingVersion = 0;
  let qrVersion = 0;
  let chatDiscoveryBusy = false;
  let statusVersion = 0;
  let modelVersion = 0;
  let modelRenderKey = "";
  let modelBusy = false;
  let localAIVersion = 0;
  let localAISnapshot = null;
  let localAIBusy = false;
  let localAIDirty = false;
  let localAIDraftRevision = 0;
  let clientVersion = 0;
  let clientRenderKey = "";
  let clientBusy = false;
  let clientSnapshot = null;
  let clientGuideID = "";
  let clientGuideBinding = null;
  let clientFocusHint = null;
  let sendPermissionBinding = null;
  let sendPermissionMode = "none";
  let sendPermissionSelection = new Set();
  let sendPermissionVersion = 0;
  let remoteSecretVersion = 0;
  let remoteSecretID = "";
  let remoteSecretFocusID = "";
  let remoteSecretRevision = 0;
  let remoteSecretExpiry = 0;
  let budgetVersion = 0;
  let budgetSnapshot = null;
  let budgetBusy = false;
  let budgetDirty = false;
  let budgetDraftRevision = 0;
  let voiceRetryBusy = false;
  let authExpired = false;
  let authEpoch = 0;
  let authBusy = false;
  let pollingTimer = null;
  let activeView = "stats";
  let statsSnapshot = null;
  let statsVersion = 0;
  let statsController = null;
  let statsReloadQueued = false;
  let statsRenderKey = "";
  let statsDimension = "chats";
  let statsPage = 1;
  let statsPageSize = 10;
  let statsSort = "messages";
  let statsChartMode = "bars";
  let statsChartSelection = null;
  let statsRhythmSelection = null;
  let statsPageFocus = null;
  let qrURL = "";
  const remoteAdmin = document.documentElement.dataset.remoteAdmin === "true";
  const capabilityKey = "wa_archive_owner_capability";

  const messages = {
    remote_operation_busy: "Another remote action is finishing. Wait a moment, then retry.",
    remote_identity_changed: "This hostname or provider belongs to the saved identity. Choose Change domain or provider account to migrate explicitly.",
    remote_manual_mode: "Remote MCP is managed by your launch settings. Restart without the manual MCP flags before using this wizard.",
    owner_auth_busy: "Password setup is busy. Wait a moment, then try again.",
    owner_auth_unavailable: "Password setup is unavailable. Try again from this device.",
    remote_password_required: "Save a separate remote owner password before enabling browser access.",
    remote_invalid_intent: "Check the saved HTTPS hostname, device name, and provider token.",
    remote_state_unavailable: "Remote setup is unavailable. Refresh its status or restart the app. Local archive search remains available.",

    unauthorized: "Your owner session has ended. Open the app again to request a fresh private owner window.",
    csrf_denied: "This page has no active owner session. Open the app again to request a fresh private owner window.",
    origin_denied: "Open the owner interface directly to make this change.",
    invalid_policy: "Choose an import date and a chat policy before saving.",
    policy_changed: "Your archive settings changed. Reopen settings and try again.",
    invalid_query: "Use a shorter search or adjust the date filters.",
    invalid_stats_filter: "Choose a valid date range, chat, and message type.",
    cursor_stale: "New messages or settings changed the archive. Run your search again.",
    source_unavailable: "This message is no longer available. Run your search again.",
    reconciliation_required: "Import has stopped because the app could not verify the archive. Search is temporarily unavailable. Your local files are retained.",
    setup_required: "Save your import choices before linking WhatsApp.",
    archive_unavailable: "The archive is unavailable. Try again or restart the app.",
    fts5_unavailable: "Keyword search is unavailable in this build.",
    request_interrupted: "The request stopped. Try again.",
    connection_unavailable: "WhatsApp could not connect. Try showing a new linking code.",
    model_management_unavailable: "Model management is unavailable in this app. Keyword search remains available.",
    model_operation_busy: "A model task is already running. Wait for it to finish or cancel it.",
    model_profile_unknown: "This model profile is unavailable. Reopen Models and media.",
    model_not_installed: "This model is not installed. Download the pinned profile first.",
    model_hash_mismatch: "Model verification failed. Remove and download the pinned profile again.",
    model_size_mismatch: "The model is incomplete. Retry its download or remove it.",
    model_platform_unavailable: "This model does not support this computer.",
    model_offline_required: "This profile requires a verified offline import. A download is unavailable.",
    model_active: "This model is in use. Pause its work before removing it.",
    model_operation_failed: "The model task did not finish. Check its state and try again.",
    model_download_failed: "The download stopped. Check the connection and retry the pinned download.",
    model_space_required: "There is not enough free disk space. Free some space and retry.",
    model_operation_interrupted: "This task stopped before completion. Verify or retry the resource.",
    model_operation_cancelled: "Task cancelled. Verify the resource or retry when ready.",
    client_connections_unavailable: "AI connections are unavailable. Keep the app running and refresh their state.",
    client_recovery_pending: "Client access is paused while private connection records need recovery. Restart the app and refresh; access remains blocked until recovery finishes.",
    client_connection_changed: "This connection changed. Refresh the list before trying again.",
    client_connection_unsupported: "This client and connection mode are unavailable. Choose a supported combination.",
    client_connection_not_found: "This connection is no longer available. Refresh the list.",
    client_connection_invalid: "Choose a client, explicit chat scope, valid dates, and a future expiry.",
    send_permission_changed: "Send permissions changed in another window. Review the current permissions before saving again.",
    send_forbidden: "These chats are outside this connection's current permissions. Refresh connections and review the available chats.",
    original_budget_unavailable: "Original media storage controls are unavailable. Refresh to check their current state.",
    original_budget_changed: "The storage limit changed in another owner window. Current state has been reloaded; review it before saving again.",
    voice_retry_unavailable: "Voice downloads could not be retried. Check the archive connection and import status, then try again.",
    local_ai_unavailable: "Local AI is unavailable. Keyword search remains usable.",
    local_ai_conflict: "Local AI settings changed. Current state has been reloaded; review it before applying again.",
    local_ai_invalid: "Choose eligible local AI components or turn local AI off.",
    local_ai_bundle_unavailable: "The trusted local AI bundle is unavailable. Keyword search remains usable.",
    local_ai_model_missing: "A selected model is unavailable. Install and verify its pinned resource, then explicitly retry this choice.",
    local_ai_profile_unqualified: "This component is not eligible for activation. Installation alone does not qualify it.",
    local_ai_operation_failed: "Local AI could not apply this choice. Check its current state before retrying.",
    app_state_sync_incomplete: "WhatsApp state sync is incomplete. Readable messages continue importing. Some deletions, chat changes, or expiry settings may be missing; saved results may differ from your phone.",
    app_state_sync_incomplete_inactive: "WhatsApp state sync is incomplete. Readable messages can still be imported and searched locally. Some deletions, chat changes, or expiry settings may be missing; saved results may differ from your phone.",
  };
  if (remoteAdmin) {
    messages.unauthorized = "Your sign-in has ended. Sign in again to access this private archive.";
    messages.csrf_denied = messages.unauthorized;
  }

  async function api(path, method = "GET", body, options = {}) {
    if (authExpired) throw new Error(messages.unauthorized);
    const epoch = authEpoch;
    const headers = {};
    if (csrf) headers["X-CSRF-Token"] = csrf;
    if (method !== "GET") {
      headers["Content-Type"] = "application/json";
    }
    let response;
    try {
      response = await fetch(path, {method, headers, credentials: "same-origin", cache: "no-store", signal: options.signal, body: method === "GET" ? undefined : JSON.stringify(body ?? {})});
    } catch (error) {
      if (error.name === "AbortError") throw error;
      throw new Error("The app is not responding. Check that it is running, then try again.");
    }
    let value = {};
    try { value = await response.json(); }
    catch (error) {
      if (error.name === "AbortError") throw error;
      /* fixed fallback below */
    }
    if (authExpired || epoch !== authEpoch) throw new Error(messages.unauthorized);
    if (!response.ok) {
      if (path !== "/api/bootstrap" && (response.status === 401 || response.status === 403 && value.code === "csrf_denied")) expireAuth();
      if (path === "/api/pairing/start" && ["source_unavailable", "connection_unavailable"].includes(value.code)) throw new Error("WhatsApp did not complete the connection. Try again from Connection details; a saved linked session reconnects without a new code.");
      const error = new Error(messages[value.code] || "This action did not finish. Try again or restart the app.");
      error.code = value.code;
      error.status = response.status;
      throw error;
    }
    return value;
  }

  function assertOwner(epoch = authEpoch) { if (authExpired || epoch !== authEpoch) throw new Error(messages.unauthorized); }

  function feedback(id, text, error = false) {
    const node = $(id);
    node.textContent = text;
    node.classList.toggle("error", error);
  }

  function expireAuth() {
    window.MyWhatsAppSelects?.closeAll();
    authExpired = true;
    authEpoch++;
    stopPolling();
    const expiredCSRF = csrf;
    csrf = "";
    if (!remoteAdmin && !remoteMode) {
      try {
        if (localStorage.getItem(capabilityKey) === expiredCSRF) localStorage.removeItem(capabilityKey);
      } catch { /* session already revoked */ }
    }
    clearResults();
    resetStats();
    activeQuery = "";
    searchRequest = null;
    searching = false;
    modelBusy = false;
    localAIBusy = false;
    clientBusy = false;
    budgetBusy = false;
    voiceRetryBusy = false;
    chatDiscoveryBusy = false;
    $("search-form").reset();
    $("search-submit").disabled = false;
    $("search-more").disabled = false;
    $("policy-form").reset();
    $("delete-form").reset();
    for (const id of ["save-policy", "pairing-start", "disconnect", "pause-toggle"]) $(id).disabled = false;
    $("delete-confirm").disabled = true;
    chats = [];
    setup = null;
    status = null;
    statusVersion++;
    pairingSnapshot = null;
    modelVersion++;
    modelRenderKey = "";
    $("model-list").replaceChildren();
    clearLocalAI();
    clientVersion++;
    clientSnapshot = null;
    clientRenderKey = "";
    clientFocusHint=null;
    $("client-list").replaceChildren();
    $("client-chat-choices").replaceChildren();
    $("client-form").reset();
    $("client-create").disabled = true;
    clearClientGuide();
    clearRemoteSecret();
    clearSendPermissions();
    clearRemoteWizard();
    budgetVersion++;
    budgetSnapshot=null;
    budgetDirty=false;
    budgetDraftRevision=0;
    $("original-budget-form").reset();
    $("original-budget-size").value="";
    $("original-budget-save").disabled=true;
    $("original-budget-summary").textContent="";
    $("original-budget-pressure").hidden=true;
    updateBudgetControls();
    updateVoiceRetryControls();
    feedback("voice-retry-feedback", "");
    $("chat-choices").replaceChildren();
    $("search-chat").replaceChildren(new Option("All included chats", ""));
    clearQR();
    for (const dialog of document.querySelectorAll("dialog[open]")) dialog.close();
    $("workspace").hidden = true;
    $("auth-screen").hidden = false;
    refreshAllSelects();
    $("auth-message").textContent = messages.unauthorized;
    if (remoteMode) {
      $("auth-message").textContent = "Unlock your private archive";
      $("remote-login-form").hidden = false;
      $("remote-password").value = "";
      $("remote-password").focus();
    }
    if (remoteAdmin) {
      showRemoteLogin(messages.unauthorized);
      if (!authBusy) $("admin-password").focus();
    }
  }

  function stopPolling() {
    if (pollingTimer !== null) window.clearInterval(pollingTimer);
    pollingTimer = null;
  }

  function startPolling() {
    if (pollingTimer !== null || authExpired) return;
    pollingTimer = window.setInterval(() => {
      if (remoteSecretExpiry && remoteSecretExpiry <= Date.now()) clearRemoteSecret();
      if (!document.hidden && !authExpired) {
        refreshStatus(); refreshPairing(); refreshModels(); refreshLocalAI(); refreshConnections(); refreshBudget();
        if (activeView === "stats") refreshStats();
      }
    }, 5000);
  }

  function showRemoteLogin(message = "Sign in to your private WhatsApp archive.") {
    window.MyWhatsAppSelects?.closeAll();
    $("workspace").hidden = true;
    $("auth-screen").hidden = false;
    $("auth-message").textContent = message;
    $("admin-login-form").hidden = false;
    $("admin-password").value = "";
    refreshAllSelects();
  }

  function updateAuthControls() {
    $("admin-password").disabled = authBusy;
    $("admin-login-submit").disabled = authBusy;
    $("admin-login-submit").textContent = authBusy ? "Signing in…" : "Sign in";
    $("admin-logout").disabled = authBusy;
  }

  async function remoteAuthRequest(path, body, token = "") {
    const epoch = authEpoch;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 15000);
    try {
      const headers = {"Content-Type":"application/json"};
      if (token) headers["X-CSRF-Token"] = token;
      const response = await fetch(path, {method:"POST", headers, credentials:"same-origin", cache:"no-store", signal:controller.signal, body:JSON.stringify(body)});
      let value = {};
      try { value = await response.json(); } catch { /* fixed errors only */ }
      if (epoch !== authEpoch) throw new Error(messages.unauthorized);
      if (!response.ok) {
        const code = typeof value.code === "string" ? value.code : "auth_unavailable";
        const error = new Error(code === "login_throttled" ? "Too many sign-in attempts. Please wait before trying again." : code === "unauthorized" ? "The password was not accepted. Try again." : "Sign-in is unavailable. Check that the app is running, then try again.");
        error.code = code;
        throw error;
      }
      return value;
    } catch (error) {
      if (epoch !== authEpoch || error.code) throw error;
      throw new Error("The app is not responding. Check that it is running, then try again.");
    } finally { window.clearTimeout(timeout); }
  }

  function acceptRemoteSession(session) {
    if (typeof session.csrf_token !== "string" || !/^[A-Za-z0-9_-]{43}$/u.test(session.csrf_token)) throw new Error("Sign-in did not finish. Try again.");
    authEpoch++;
    csrf = session.csrf_token;
    authExpired = false;
    feedback("admin-login-feedback", "");
  }

  async function loginRemote(event) {
    event.preventDefault();
    if (!remoteAdmin || authBusy) return;
    const epoch = authEpoch;
    const payload = {password:$("admin-password").value};
    if (!payload.password) return;
    authBusy = true;
    updateAuthControls();
    feedback("admin-login-feedback", "Checking your password…");
    try {
      const request = remoteAuthRequest("/api/auth/login", payload);
      payload.password = "";
      $("admin-password").value = "";
      const session = await request;
      if (epoch !== authEpoch) return;
      acceptRemoteSession(session);
      await initializeWorkspace();
    } catch (error) {
      if (epoch === authEpoch) feedback("admin-login-feedback", error.message, true);
    } finally {
      payload.password = "";
      $("admin-password").value = "";
      authBusy = false;
      updateAuthControls();
      if (authExpired) $("admin-password").focus();
    }
  }

  async function logoutRemote() {
    if (!remoteAdmin || authBusy || authExpired) return;
    const epoch = authEpoch;
    authBusy = true;
    updateAuthControls();
    clearRemoteSecret();
    clearQR();
    try {
      await remoteAuthRequest("/api/auth/logout", {}, csrf);
      if (epoch !== authEpoch) return;
      expireAuth();
      showRemoteLogin("You are signed out.");
      feedback("admin-login-feedback", "");
    } catch (error) {
      if (epoch !== authEpoch) return;
      if (["unauthorized", "csrf_denied"].includes(error.code)) expireAuth();
      else {
        $("status-notice").textContent = "Sign out did not finish. Check that the app is running, then try again.";
        $("status-notice").hidden = false;
      }
    } finally {
      authBusy = false;
      updateAuthControls();
      if (authExpired) $("admin-password").focus();
    }
  }

  function bdi(text, className) {
    const node = document.createElement("bdi");
    node.dir = "auto";
    node.textContent = String(text ?? "");
    if (className) node.className = className;
    return node;
  }

  const finalLetters = {"ך":"כ", "ם":"מ", "ן":"נ", "ף":"פ", "ץ":"צ"};
  function normalized(text) {
    let value = "", offset = 0;
    const starts = [], ends = [];
    for (const char of text) {
      const unit = char.normalize("NFKD").replace(/\p{M}/gu, "").toLocaleLowerCase();
      for (const part of unit) {
        const normalizedPart = finalLetters[part] || part;
        value += normalizedPart;
        for (let j = 0; j < normalizedPart.length; j++) { starts.push(offset); ends.push(offset + char.length); }
      }
      offset += char.length;
    }
    return {value, starts, ends};
  }

  // All source text remains in its original logical order. Highlight and isolate
  // nodes carry only text; message content is never parsed as HTML or links.
  function appendIsolated(parent, text) {
    const pattern = /(?:https?:\/\/[^\s<>]+|www\.[^\s<>]+|[+]?\d[\d.,:/%+\-]*\d|\d)/gu;
    let offset = 0;
    for (const match of text.matchAll(pattern)) {
      parent.append(document.createTextNode(text.slice(offset, match.index)));
      parent.append(bdi(match[0]));
      offset = match.index + match[0].length;
    }
    parent.append(document.createTextNode(text.slice(offset)));
  }

  function renderText(node, original, query = "") {
    const text = String(original ?? "");
    node.replaceChildren();
    node.dir = "auto";
    const norm = normalized(text);
    const ranges = [];
    for (const term of query.split(/\s+/u).filter(Boolean).slice(0, 32)) {
      const needle = normalized(term).value;
      if (!needle) continue;
      let from = 0;
      while (from < norm.value.length && ranges.length < 256) {
        const index = norm.value.indexOf(needle, from);
        if (index < 0) break;
        ranges.push([norm.starts[index], norm.ends[index + needle.length - 1]]);
        from = index + needle.length;
      }
    }
    ranges.sort((a, b) => a[0] - b[0]);
    const merged = [];
    for (const range of ranges) {
      const last = merged[merged.length - 1];
      if (last && range[0] <= last[1]) last[1] = Math.max(last[1], range[1]);
      else merged.push(range);
    }
    let offset = 0;
    for (const [start, end] of merged) {
      appendIsolated(node, text.slice(offset, start));
      const mark = document.createElement("mark");
      appendIsolated(mark, text.slice(start, end));
      node.append(mark);
      offset = end;
    }
    appendIsolated(node, text.slice(offset));
  }

  function dateText(value) {
    if (!value || value.startsWith("0001-")) return "Not observed";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "Not observed";
    return new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "short"}).format(date);
  }

  function dayStart(value) {
    const date = new Date(value + "T00:00:00");
    if (Number.isNaN(date.getTime())) throw new Error("Choose a valid date.");
    return date;
  }

  function codeText(code) {
    const labels = {
      connected:"Connected", disconnected:"Disconnected", pairing:"Waiting for phone", setup_required:"Setup needed", paused:"Paused",
      unknown:"Unknown coverage", partial:"Partial coverage", observed:"Observed history only", not_configured:"Not configured",
      ready:"Ready", complete:"Indexed", indexing:"Indexing", pending:"Waiting", failed:"Stopped", experimental:"Experimental",
      unavailable:"Unavailable", reconciliation_required:"Import stopped", importing:"Receiving messages", discovery:"Discovering chats",
      idle:"Idle", running:"Processing", queued:"Waiting", source_unavailable:"Original unavailable", policy_skipped:"Skipped by policy",
    };
    return labels[code] || "Status not available";
  }

  function progressText(progress) {
    if (progress?.Code === "transcription_busy") return "Transcribing another recording";
    if (!progress) return "Not configured";
    return codeText(progress.State || "not_configured");
  }

  function stateSyncGapText(snapshot) {
    return !snapshot?.Paused && ["connected", "discovery", "importing"].includes(snapshot?.Connection) ? messages.app_state_sync_incomplete : messages.app_state_sync_incomplete_inactive;
  }

  function importFailureText(codes = []) {
    if (codes.includes("app_state_sync_error")) return "WhatsApp could not sync this linked device's state. Import and search are paused. Your local files are retained.";
    if (codes.includes("undecryptable_message")) return "A WhatsApp message was unavailable or could not be decrypted. Import and search are paused. Your local files are retained.";
    if (codes.includes("ambiguous_message_structure")) return "This version could not safely decode the structure of a WhatsApp message. Import and search are paused. Your local files are retained.";
    if (codes.includes("ambiguous_edit_structure")) return "This version could not safely decode the structure of a WhatsApp message edit. Import and search are paused. Your local files are retained.";
    if (codes.includes("unsafe_mutation_projection")) return "A WhatsApp message or change could not be decoded safely. Import and search are paused. Your local files are retained.";
    if (codes.includes("history_limit_exceeded")) return "WhatsApp sent a history batch larger than this version can process. This archive is paused and your local files are retained. Start a new import with the updated app.";
    if (codes.includes("history_corrupt")) return "A history batch from WhatsApp could not be read safely. This archive is paused and your local files are retained. Keep this data folder while the import problem is resolved.";
    if (codes.includes("queue_limit_exceeded")) return "WhatsApp sent more data than the import queue could hold. This archive is paused and your local files are retained. Keep this data folder while the import problem is resolved.";
    return messages.reconciliation_required;
  }

  function importPresentation(snapshot, policy, configured, discovered = 0, pairing = null) {
    const count = Number(snapshot?.Messages || 0);
    const unsafe = Boolean(snapshot?.ReconciliationRequired || snapshot?.Connection === "reconciliation_required");
    const selected = policy?.ChatMode === "all" || Boolean(policy?.IncludedChats?.length);
    const connected = ["connected", "discovery", "importing"].includes(snapshot?.Connection);
    const base = {phase:"checking", tone:"normal", title:"Checking your archive", description:"Checking your connection and saved messages…", action:"", actionText:"Checking status", linked:connected, selected, receiving:count > 0, compact:false};
    if (unsafe) return {...base,phase:"stopped",tone:"warning",title:"Import stopped",description:importFailureText(snapshot.Codes),action:"check",actionText:"Check import status",linked:false,receiving:false};
    if (!configured) return {...base,phase:"setup",title:"Bring your conversations into reach",description:"Link WhatsApp with your phone, choose the chats to keep, and search them here. You can start with the last 30 days.",action:"settings",actionText:"Set up your archive"};
    if (snapshot?.Paused) return {...base,phase:"paused",title:"Import is paused",description:count ? "Your saved messages are still searchable. Resume when you want to receive new messages." : "Resume import to continue receiving chats and messages from WhatsApp.",action:"resume",actionText:"Resume import",compact:count > 0};
    const connection = connectionPresentation(pairing || {state:snapshot?.Connection}, snapshot);
    if (connection.busy) return {...base,phase:"connecting",title:connection.title,description:connection.description,action:"connection",actionText:"View connection",compact:count > 0};
    if (connection.ended) return {...base,phase:"session_ended",tone:"warning",title:connection.title,description:connection.feedback,action:"connection",actionText:"View connection details",compact:count > 0};
    if (snapshot?.Connection === "pairing" && pairing?.has_session !== true) return {...base,phase:"pairing",title:"Link WhatsApp on your phone",description:"Open the linking code, then scan it in WhatsApp under Linked devices. This screen will update after your phone connects.",action:"connection",actionText:"Open linking code"};
    if (connected && !selected) return {...base,phase:"choose",title:discovered ? "WhatsApp is linked. Choose your chats." : "WhatsApp linked. Receiving your chat list…",description:discovered ? discovered.toLocaleString() + " chats discovered. Choose which conversations to archive and the dates to keep." : "Leave this app running while WhatsApp sends your chat list. No message bodies are saved until you select chats.",action:discovered ? "settings" : "",actionText:discovered ? "Choose chats" : "Waiting for chat list"};
    if (connected && !count) return {...base,phase:"receiving",title:discovered ? "Receiving your WhatsApp history" : "Waiting for your WhatsApp history",description:"Messages from your selected chats will appear as WhatsApp delivers them. Leave this app running; the amount of available history depends on WhatsApp.",action:"settings",actionText:"Review chats and dates"};
    if (count) return {...base,phase:connected ? "ready" : "offline",title:connected ? "Your archive is ready" : "Your saved archive is ready",description:connected ? count.toLocaleString() + " saved messages are searchable. New messages are received while WhatsApp is connected." : "WhatsApp is disconnected. Your saved messages are still searchable; reconnect to receive new ones.",action:"connection",actionText:"Reconnect WhatsApp",compact:true};
    if (pairing?.has_session === true) return {...base,phase:"reconnect",title:"Reconnect your linked WhatsApp",description:connection.feedback || "Your linked device session is saved. Reconnect to finish signing in and receive your chats; you do not need a new QR code.",action:"connection",actionText:"Reconnect WhatsApp"};
    return {...base,phase:"disconnected",title:"Link WhatsApp to start importing",description:"Your chat and date choices are saved. Scan a linking code with your phone to receive your WhatsApp history.",action:"connection",actionText:"Link WhatsApp"};
  }

  function renderImportState() {
    const policy = setup?.Policy || setup?.policy;
    const view = importPresentation(status, policy, setup?.configured, chats.length, pairingSnapshot);
    const panel = $("import-panel");
    panel.hidden = view.phase === "ready";
    panel.dataset.tone = view.tone;
    panel.dataset.compact = String(view.compact);
    $("import-title").textContent = view.title;
    $("import-description").textContent = view.description;
    $("import-symbol").textContent = view.phase === "stopped" ? "!" : view.phase === "choose" || view.phase === "ready" ? "✓" : "↓";
    $("import-action").textContent = view.actionText;
    $("import-action").dataset.action = view.action;
    $("import-action").disabled = !view.action;
    $("import-check").hidden = !["receiving", "choose", "pairing"].includes(view.phase);
    $("import-progress").hidden = ["stopped", "paused", "offline", "connecting", "session_ended", "reconnect"].includes(view.phase);
    const steps = [["progress-link",view.linked, ["pairing","disconnected"].includes(view.phase)],["progress-choose",view.selected,view.phase === "choose"],["progress-import",view.receiving,view.phase === "receiving"]];
    for (const [id, complete, current] of steps) {
      const node = $(id);
      node.dataset.state = complete ? "complete" : current ? "current" : "waiting";
      node.querySelector(".step-marker").textContent = complete ? "✓" : id === "progress-link" ? "1" : id === "progress-choose" ? "2" : "3";
      if (current) node.setAttribute("aria-current", "step"); else node.removeAttribute("aria-current");
    }
    $("progress-link-detail").textContent = view.linked ? "Phone linked" : "Scan a code with your phone";
    $("progress-choose-detail").textContent = policy?.ChatMode === "all" ? "All chats selected" : policy?.IncludedChats?.length ? policy.IncludedChats.length.toLocaleString() + " chats selected" : "Select after your phone links";
    $("progress-import-detail").textContent = view.receiving ? Number(status.Messages).toLocaleString() + " messages saved" : view.phase === "receiving" ? "Waiting for messages from WhatsApp" : "Search as messages arrive";
    $("search-content").hidden = Boolean(status?.ReconciliationRequired || status?.Connection === "reconciliation_required" || !Number(status?.Messages || 0));
  }

  async function refreshStatus() {
    if (authExpired) return;
    const epoch = authEpoch;
    const version = ++statusVersion;
    try {
      const next = await api("/api/status");
      if (version !== statusVersion || authExpired) return;
      const previous = status;
      const changed = previous && (previous.Snapshot?.PolicyGeneration !== next.Snapshot?.PolicyGeneration || previous.Snapshot?.ArchiveRevision !== next.Snapshot?.ArchiveRevision);
      if (changed || next.ReconciliationRequired || next.Connection === "reconciliation_required") {
        window.MyWhatsAppSelects?.closeAll();
        clearResults();
        invalidateStats(true);
        feedback("search-feedback", next.ReconciliationRequired ? "" : "The archive changed. Run your search again to see current messages.");
      }
      status = next;
      if (status.ConnectionDiagnostic) pairingSnapshot = {...pairingSnapshot,state:status.Connection,connection_diagnostic:status.ConnectionDiagnostic};
      const connectionView = connectionPresentation({...pairingSnapshot,state:status.Connection}, status);
      $("connection-status").textContent = connectionView.busy ? connectionView.title : connectionView.ended ? "Session ended" : codeText(status.Connection || "setup_required");
      $("connection-dot").dataset.state = connectionView.busy ? "pairing" : status.Connection || "setup_required";
      const unsafe = Boolean(status.ReconciliationRequired || status.Connection === "reconciliation_required");
      const count = Number(status.Messages || 0);
      const hasStateSyncGap = !unsafe && (status.Codes || []).includes("app_state_sync_error");
      const hasMessageGap = !unsafe && (status.Codes || []).some(code => ["undecryptable_message", "source_revision_conflict"].includes(code));
      const hasUnresolvedDeletion = !unsafe && (status.Codes || []).includes("unresolved_deletion");
      const hasRestoredSnapshot = !unsafe && status.Snapshot?.RecoveryCode === "owner_restored_snapshot";
      $("messages-count").textContent = unsafe ? "—" : count.toLocaleString();
      $("coverage-status").textContent = unsafe ? "Archive checks required" : hasStateSyncGap || hasMessageGap || hasUnresolvedDeletion || hasRestoredSnapshot ? "Incomplete history" : count ? status.Coverage === "partial" ? "Some history received" : "Received history" : "Waiting for history";
      $("history-detail").textContent = unsafe ? "Message counts are unavailable while this archive is paused." : count ? "Only the history delivered by WhatsApp is included." : "Messages appear here after WhatsApp sends them.";
      $("keyword-status").textContent = unsafe ? "Unavailable" : !count ? "Waiting" : progressText(status.Keyword);
      $("keyword-detail").textContent = unsafe ? "Search is paused until the archive is safe to read." : !count ? "Ready when messages arrive." : status.Keyword?.Pending ? Number(status.Keyword.Pending).toLocaleString() + " waiting to index" : "Search your saved message text.";
      $("semantic-status").textContent = "Semantic search: " + (!status.Semantic || status.Semantic.State === "not_configured" ? "off" : progressText(status.Semantic));
      $("media-status").textContent = "Media extraction: " + (!status.Media || status.Media.State === "not_configured" ? "off" : progressText(status.Media));
      $("models-semantic").textContent = "Semantic search: " + progressText(status.Semantic);
      $("models-media").textContent = "Media extraction: " + progressText(status.Media);
      $("pause-toggle").textContent = status.Paused ? "Resume import" : "Pause import";
      $("pause-toggle").hidden = unsafe || !setup?.configured;
      $("connection-open").textContent = unsafe ? "View import problem" : connectionView.busy || connectionView.ended || ["connected","pairing","discovery","importing"].includes(status.Connection) ? "Connection details" : pairingSnapshot?.has_session === true ? "Reconnect WhatsApp" : "Link WhatsApp";
      renderImportState();
      const notices = [];
      if (hasRestoredSnapshot) notices.push("You restored this stored archive after an interrupted import. Messages, edits, or deletions during the interruption may be missing. Current WhatsApp state is unverified; known unavailable sources remain withheld.");
      if (hasStateSyncGap) notices.push(stateSyncGapText(status));
      if (hasMessageGap) notices.push("Some WhatsApp messages were unavailable, could not be decrypted, or had conflicting updates. Affected messages are withheld; other readable messages can still be imported and searched. Received history is incomplete." + ((status.Codes || []).includes("undecryptable_message") ? " WhatsApp may retry delivery." : ""));
      if (hasUnresolvedDeletion) notices.push("Some deleted messages could not be matched completely. Possible matches are withheld; other messages remain searchable.");
      for (const code of unsafe ? [] : status.Codes || []) {
        if (code === "history_limit_exceeded") notices.push("Some history exceeded the import limit. Coverage remains partial.");
        else if (code === "history_corrupt") notices.push("Some received history could not be read. Coverage remains partial.");
        else if (code === "queue_limit_exceeded") notices.push("Import reached its queue limit. Some messages may be missing.");
      }
      $("status-notice").textContent = notices.join(" ");
      $("status-notice").hidden = !notices.length;
      $("status-notice").classList.toggle("warning", hasStateSyncGap || hasMessageGap || hasUnresolvedDeletion || hasRestoredSnapshot);
      renderStatsStatus();
      if (changed && activeView === "stats" && !unsafe) refreshStats();
      if ($("connection-dialog").open) renderConnectionPresentation({...pairingSnapshot,state:status.Connection});
      const policy = setup?.Policy || setup?.policy;
      if (!unsafe && ["connected","discovery","importing"].includes(status.Connection) && policy?.ChatMode === "selective" && !(policy.IncludedChats || []).length && !chats.length && !$("connection-dialog").open && !chatDiscoveryBusy) {
        chatDiscoveryBusy = true;
        try { await loadChats(); } finally { if (epoch === authEpoch) chatDiscoveryBusy = false; }
      }
    } catch (error) { if (version === statusVersion && !authExpired) { $("status-notice").textContent = error.message; $("status-notice").hidden = false; } }
  }

  function statsCount(value) {
    const count = Number(value);
    return Number.isSafeInteger(count) && count >= 0 ? count : 0;
  }

  function statsNumber(value) {
    return statsCount(value).toLocaleString();
  }

  function statsDate(value, options = {dateStyle:"medium", timeStyle:"short"}, allowEpoch = false) {
    const seconds = Number(value);
    if (!Number.isSafeInteger(seconds) || seconds < -62135596800 || seconds > 253402300799 || seconds === 0 && !allowEpoch) return "No messages yet";
    const date = new Date(seconds * 1000);
    return Number.isNaN(date.getTime()) ? "Not observed" : new Intl.DateTimeFormat(undefined, options).format(date);
  }

  function statsUnavailable() {
    return Boolean(status?.ReconciliationRequired || status?.Connection === "reconciliation_required");
  }

  function clearStatsData() {
    statsSnapshot = null;
    statsRenderKey = "";
    for (const id of ["stats-message-count", "stats-chat-count", "stats-sent-count", "stats-received-count", "stats-transcribed", "stats-latest-message", "stats-active-days", "stats-streak", "stats-busiest-day", "stats-daily-average"]) $(id).textContent = "—";
    $("stats-range-description").textContent = "Only readable messages in your saved archive are counted.";
    $("stats-activity-total").textContent = "";
    $("stats-chats-total").textContent = "";
    $("stats-chart").replaceChildren();
    $("stats-aggregate-rows").replaceChildren();
    $("stats-aggregate-empty").hidden = true;
    $("stats-page-description").textContent = "Waiting for archive stats.";
    $("stats-page-prev").disabled = true;
    $("stats-page-next").disabled = true;
    $("stats-heatmap").replaceChildren();
    $("stats-rhythm-rows").replaceChildren();
    $("stats-rhythm-table").hidden = true;
    $("stats-chart-detail").textContent = "Point to a period, or use the arrow keys to explore.";
    $("stats-rhythm-detail").textContent = "Choose an hour to see its message count.";
    $("stats-busiest-detail").textContent = "Most messages on one UTC date";
    statsChartSelection = null;
    statsRhythmSelection = null;
    $("stats-kinds").replaceChildren();
    $("stats-kind-bar").replaceChildren();
    $("stats-activity-rows").replaceChildren();
    $("stats-voice-progress").value = 0;
    $("stats-voice-progress").max = 1;
    $("stats-activity-table").hidden = true;
    $("stats-voice-description").textContent = "Saved transcripts and original recordings appear here.";
  }

  function invalidateStats(clear = false) {
    statsVersion++;
    statsController?.abort();
    statsReloadQueued = false;
    if (clear) clearStatsData();
  }

  function resetStats() {
    invalidateStats(true);
    $("stats-filters").reset();
    statsDimension = "chats";
    statsPage = 1;
    statsPageSize = 10;
    statsSort = "messages";
    statsChartMode = "bars";
    statsPageFocus = null;
    $("stats-breakdown-sort").value = statsSort;
    $("stats-breakdown-size").value = String(statsPageSize);
    renderStatsControls();
    $("stats-chat").replaceChildren(new Option("All archived chats", ""));
    refreshSelects("stats-range", "stats-chat", "stats-kind");
    $("stats-custom-dates").hidden = true;
    $("stats-data").setAttribute("aria-busy", "false");
    $("stats-updated").textContent = "Checking archive";
    $("stats-notice").textContent = "";
    $("stats-notice").hidden = true;
    feedback("stats-feedback", "");
    for (const kind of ["keyword", "media", "semantic"]) {
      $("stats-" + kind + "-state").textContent = "Checking";
      $("stats-" + kind + "-detail").textContent = "";
      $("stats-" + kind + "-progress").value = 0;
      $("stats-" + kind + "-progress").max = 1;
    }
    $("stats-connection-state").textContent = "Checking connection";
    $("stats-connection-description").textContent = "Checking WhatsApp and import status…";
    activeView = "stats";
  }

  function selectWorkspaceView(view, focus = false) {
    if (authExpired || !["stats", "archive"].includes(view)) return;
    window.MyWhatsAppSelects?.closeAll();
    activeView = view;
    $("stats-view").hidden = view !== "stats";
    $("archive-view").hidden = view !== "archive";
    refreshAllSelects();
    for (const name of ["stats", "archive"]) {
      if (name === view) $("nav-" + name).setAttribute("aria-current", "page");
      else $("nav-" + name).removeAttribute("aria-current");
    }
    document.querySelector(".skip-link").setAttribute("href", view === "stats" ? "#stats-title" : "#search-query");
    if (view === "stats") {
      renderStatsStatus();
      refreshStats();
      if (focus) $("stats-title").focus({preventScroll:true});
    } else {
      invalidateStats();
      if (focus) ($("search-content").hidden ? $("import-action") : $("search-query")).focus({preventScroll:true});
    }
  }

  function renderStatsChatOptions() {
    const select = $("stats-chat");
    const chosen = select.value;
    select.replaceChildren(new Option("All archived chats", ""));
    for (const chat of chats) {
      if (setup?.Policy?.ChatMode === "all" || chat.Selected) select.append(new Option(chat.Name || "Unnamed chat", chat.ID));
    }
    select.value = chosen;
    refreshSelects("stats-chat");
    if (chosen && !select.value) invalidateStats(true);
  }

  function statsFilter() {
    const request = {breakdown:statsDimension, page:statsPage, page_size:statsPageSize, sort:statsSort};
    const range = $("stats-range").value;
    if (range === "custom") {
      if (!$("stats-from").value && !$("stats-until").value) throw new Error("Choose at least one custom date.");
      if ($("stats-from").value) request.from = Math.floor(dayStart($("stats-from").value).getTime() / 1000);
      if ($("stats-until").value) {
        const end = dayStart($("stats-until").value);
        end.setDate(end.getDate() + 1);
        request.until = Math.floor(end.getTime() / 1000);
      }
      if (request.from !== undefined && request.until !== undefined && request.from >= request.until) throw new Error("The end date must be on or after the start date.");
    } else if (range !== "all") {
      const end = new Date();
      end.setHours(0, 0, 0, 0);
      end.setDate(end.getDate() + 1);
      const start = new Date(end);
      start.setDate(start.getDate() - Number(range));
      request.from = Math.floor(start.getTime() / 1000);
      request.until = Math.floor(end.getTime() / 1000);
    }
    if ($("stats-chat").value) request.chat_id = $("stats-chat").value;
    if ($("stats-kind").value !== "all") request.kind = $("stats-kind").value;
    return request;
  }

  function changeStatsFilters() {
    statsPage = 1;
    $("stats-custom-dates").hidden = $("stats-range").value !== "custom";
    invalidateStats(true);
    $("stats-updated").textContent = "Updating view";
    refreshStats();
  }

  function renderStatsControls() {
    refreshSelects("stats-breakdown-sort", "stats-breakdown-size");
    for (const dimension of ["people", "chats"]) $("stats-breakdown-" + dimension).setAttribute("aria-pressed", String(statsDimension === dimension));
    for (const mode of ["bars", "line"]) $("stats-chart-" + mode).setAttribute("aria-pressed", String(statsChartMode === mode));
    $("stats-aggregate-name-heading").textContent = statsDimension === "people" ? "Person" : "Chat";
    $("stats-aggregate-chats-heading").hidden = statsDimension !== "people";
    $("stats-breakdown-description").textContent = statsDimension === "people" ? "People who sent messages in your saved history, including you. This is not your phone's contact list." : "Message counts across your archived conversations.";
  }

  function changeStatsBreakdown(options = {}) {
    statsPageFocus = document.activeElement === $("stats-page-prev") ? "stats-page-prev" : document.activeElement === $("stats-page-next") ? "stats-page-next" : null;
    if (options.dimension !== undefined && ["people", "chats"].includes(options.dimension)) statsDimension = options.dimension;
    if (options.sort !== undefined && ["messages", "name", "recent"].includes(options.sort)) statsSort = options.sort;
    if (options.pageSize !== undefined && [10, 25, 50].includes(Number(options.pageSize))) statsPageSize = Number(options.pageSize);
    statsPage = Number.isSafeInteger(options.page) && options.page > 0 ? options.page : 1;
    $("stats-breakdown-sort").value = statsSort;
    $("stats-breakdown-size").value = String(statsPageSize);
    renderStatsControls();
    invalidateStats();
    // The filters still match. Keep the chart in place while clearing the table
    // so an old page cannot appear under its new page or dimension label.
    $("stats-aggregate-rows").replaceChildren();
    $("stats-aggregate-empty").hidden = true;
    $("stats-page-description").textContent = "Loading message counts…";
    $("stats-chats-total").textContent = "";
    $("stats-page-prev").disabled = true;
    $("stats-page-next").disabled = true;
    statsRenderKey = "";
    refreshStats();
  }

  function setStatsChartMode(mode) {
    if (!["bars", "line"].includes(mode) || mode === statsChartMode) return;
    statsChartMode = mode;
    renderStatsControls();
    if (statsSnapshot) renderStatsChart(statsSnapshot);
  }

  function restoreStatsPageFocus() {
    if (!statsPageFocus) return;
    const previous = $(statsPageFocus);
    const active = document.activeElement;
    if (!active || active === document.body || active === previous) {
      const alternate = $(statsPageFocus === "stats-page-next" ? "stats-page-prev" : "stats-page-next");
      (!previous.disabled ? previous : !alternate.disabled ? alternate : $("stats-breakdown-size")).focus({preventScroll:true});
    }
    statsPageFocus = null;
  }

  function validStatsSnapshot(snapshot) {
    const count = value => Number.isSafeInteger(value) && value >= 0;
    const timestamp = value => Number.isSafeInteger(value) && value >= -62135596800 && value <= 253402300799;
    if (!snapshot || ![snapshot.messages, snapshot.chats, snapshot.sent, snapshot.received].every(count) || snapshot.sent + snapshot.received !== snapshot.messages || !Array.isArray(snapshot.daily) || snapshot.daily.length > 90 || !snapshot.daily.every(row => count(row.messages) && /^\d{4}-\d{2}-\d{2}$/.test(row.date)) || !Array.isArray(snapshot.top_chats) || !Array.isArray(snapshot.kinds)) return false;
    if (snapshot.insights && (![snapshot.insights.active_days, snapshot.insights.longest_streak_days, snapshot.insights.busiest_date_messages].every(count) || snapshot.insights.active_days > snapshot.messages || snapshot.insights.longest_streak_days > snapshot.insights.active_days || snapshot.insights.busiest_date_messages > snapshot.messages || snapshot.insights.busiest_date && !/^\d{4}-\d{2}-\d{2}$/.test(snapshot.insights.busiest_date))) return false;
    if (snapshot.rhythm && (!Array.isArray(snapshot.rhythm) || snapshot.rhythm.length > 168 || !snapshot.rhythm.every(row => Number.isInteger(row.weekday) && row.weekday >= 0 && row.weekday <= 6 && Number.isInteger(row.hour) && row.hour >= 0 && row.hour <= 23 && count(row.messages) && row.messages <= snapshot.messages))) return false;
    if (Array.isArray(snapshot.rhythm) && new Set(snapshot.rhythm.map(row => row.weekday * 24 + row.hour)).size !== snapshot.rhythm.length) return false;
    const breakdown = snapshot.breakdown;
    return !breakdown || breakdown.dimension === statsDimension && breakdown.sort === statsSort && breakdown.page_size === statsPageSize && Number.isSafeInteger(breakdown.page) && breakdown.page > 0 && breakdown.page <= statsPage && count(breakdown.total) && Array.isArray(breakdown.rows) && breakdown.rows.length <= breakdown.page_size && breakdown.rows.every(row => typeof row.id === "string" && typeof row.name === "string" && [row.messages, row.sent, row.received, row.chats].every(count) && timestamp(row.last_message_at) && row.sent + row.received === row.messages);
  }

  async function refreshStats() {
    if (authExpired || activeView !== "stats" || document.hidden) return;
    if (statsUnavailable()) {
      invalidateStats(true);
      feedback("stats-feedback", "Stats are unavailable while the archive requires checks.", true);
      return;
    }
    if (statsController) {
      statsReloadQueued = true;
      return;
    }
    let filter;
    try { filter = statsFilter(); }
    catch (error) {
      $("stats-data").setAttribute("aria-busy", "false");
      feedback("stats-feedback", error.message, true);
      return;
    }
    const version = ++statsVersion;
    const epoch = authEpoch;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 15000);
    statsController = controller;
    $("stats-data").setAttribute("aria-busy", "true");
    $("stats-refresh").disabled = true;
    try {
      const snapshot = await api("/api/stats", "POST", filter, {signal:controller.signal});
      if (version !== statsVersion || epoch !== authEpoch || authExpired || activeView !== "stats" || document.hidden || statsUnavailable()) return;
      if (!validStatsSnapshot(snapshot)) throw new Error("Stats could not be read. Use Refresh stats to try again.");
      renderStats(snapshot);
      $("stats-updated").textContent = "Updated " + statsDate(snapshot.sampled_at, {hour:"numeric", minute:"2-digit", second:"2-digit"});
      document.querySelector(".stats-update").dataset.state = "ready";
      feedback("stats-feedback", "");
    } catch (error) {
      if (version !== statsVersion || epoch !== authEpoch || authExpired || activeView !== "stats" || document.hidden) return;
      clearStatsData();
      $("stats-updated").textContent = "Update unavailable";
      document.querySelector(".stats-update").dataset.state = "error";
      feedback("stats-feedback", error.name === "AbortError" ? "Stats took too long to load. Use Refresh stats to try again." : error.message, true);
    } finally {
      window.clearTimeout(timeout);
      if (statsController === controller) {
        statsController = null;
        if (epoch === authEpoch) {
          $("stats-data").setAttribute("aria-busy", "false");
          $("stats-refresh").disabled = false;
          if (version === statsVersion) restoreStatsPageFocus();
        }
      }
      if (statsReloadQueued && !authExpired && activeView === "stats" && !document.hidden) {
        statsReloadQueued = false;
        refreshStats();
      }
    }
  }

  function renderStatsProgress(kind, progress) {
    const state = $("stats-" + kind + "-state");
    const detail = $("stats-" + kind + "-detail");
    const bar = $("stats-" + kind + "-progress");
    const complete = statsCount(progress?.Complete);
    const pending = statsCount(progress?.Pending);
    const failed = statsCount(progress?.Failed);
    const total = complete + pending + failed;
    const disabled = !progress || progress.State === "not_configured";
    state.textContent = statsUnavailable() ? "Unavailable" : disabled ? "Not configured" : progressText(progress);
    state.parentElement.parentElement.dataset.state = failed ? "failed" : progress?.State || "not_configured";
    bar.max = total || 1;
    bar.value = statsUnavailable() ? 0 : complete;
    bar.hidden = disabled || statsUnavailable() || !total;
    detail.textContent = statsUnavailable() ? "Archive checks are required before processing can continue." : disabled ? kind === "keyword" ? "Available when messages arrive." : "Optional. Configure in Models and media." : total ? statsNumber(complete) + " completed · " + statsNumber(pending) + " waiting" + (failed ? " · " + statsNumber(failed) + " failed" : "") : "No queued jobs.";
  }

  function renderStatsStatus() {
    if (!status) return;
    const policy = setup?.Policy || setup?.policy;
    const view = importPresentation(status, policy, setup?.configured, chats.length, pairingSnapshot);
    const connected = ["connected", "discovery", "importing"].includes(status.Connection);
    $("stats-connection-state").textContent = status.Paused ? "Import paused" : $("connection-status").textContent;
    $("stats-connection-dot").dataset.state = status.Connection || "setup_required";
    $("stats-connection-description").textContent = statsUnavailable() ? "Import and stats are paused while the archive requires checks. Your local files are retained." : connected && !status.Paused && Number(status.Messages) ? "New messages arrive while WhatsApp is connected. History coverage may be incomplete." : view.description;
    $("stats-connection-action").textContent = !setup?.configured ? "Set up your archive" : status.Paused && !statsUnavailable() ? "Resume import" : !connected ? "Link or reconnect WhatsApp" : "Connection details";
    const notice = $("status-notice");
    $("stats-notice").textContent = notice.textContent;
    $("stats-notice").hidden = notice.hidden;
    for (const [kind, progress] of [["keyword", status.Keyword], ["media", status.Media], ["semantic", status.Semantic]]) renderStatsProgress(kind, progress);
  }

  function statsUTCDate(value, options = {month:"short", day:"numeric", year:"numeric"}) {
    const date = new Date(String(value) + "T00:00:00Z");
    return Number.isNaN(date.getTime()) ? "Unknown date" : new Intl.DateTimeFormat(undefined, {...options, timeZone:"UTC"}).format(date);
  }

  function renderStatsChart(snapshot) {
    const target = $("stats-chart");
    const focusedIndex = document.activeElement?.dataset?.periodIndex;
    target.replaceChildren();
    const rows = (snapshot.daily || []).slice(0, 90);
    const table = $("stats-activity-rows");
    table.replaceChildren();
    if (!rows.length) {
      const empty = document.createElement("p");
      empty.className = "stats-empty";
      empty.textContent = setup?.configured ? "No readable messages match these filters. Try a wider date range or another chat." : "Set up your archive to see messages and activity here.";
      target.append(empty);
      $("stats-activity-table").hidden = true;
      $("stats-chart-detail").textContent = "No activity in this view.";
      return;
    }
    $("stats-activity-table").hidden = false;
    const ns = "http://www.w3.org/2000/svg";
    function svgNode(name, attributes, text) {
      const node = document.createElementNS(ns, name);
      for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, String(value));
      if (text !== undefined) node.textContent = text;
      return node;
    }
    const width = Math.max(260, target.clientWidth);
    const height = width < 480 ? 240 : 284;
    const left = 43, right = 9, top = 15, bottom = 36;
    const plotWidth = width - left - right;
    const plotHeight = height - top - bottom;
    const maximum = Math.max(1, ...rows.map(row => statsCount(row.messages)));
    const magnitude = 10 ** Math.floor(Math.log10(maximum));
    const axisMaximum = Math.max(4, Math.ceil(maximum / magnitude) * magnitude);
    const chart = svgNode("svg", {viewBox:"0 0 " + width + " " + height, role:"group", "aria-label":"Message activity: " + statsNumber(snapshot.messages) + " messages across " + rows.length + " UTC periods. Use left and right arrow keys to explore; an accessible table follows."});
    for (let tick = 0; tick <= 4; tick++) {
      const y = top + plotHeight * tick / 4;
      chart.append(svgNode("line", {x1:left, y1:y, x2:width - right, y2:y, class:"chart-grid"}));
      const value = Math.round(axisMaximum * (4 - tick) / 4);
      chart.append(svgNode("text", {x:left - 8, y:y + 4, "text-anchor":"end", class:"chart-label"}, new Intl.NumberFormat(undefined, {notation:axisMaximum >= 10000 ? "compact" : "standard", maximumFractionDigits:1}).format(value)));
    }
    const stride = plotWidth / rows.length;
    const points = rows.map((row,index) => ({x:left + (index + .5) * stride, y:top + plotHeight - statsCount(row.messages) / axisMaximum * plotHeight}));
    if (statsChartMode === "line") {
      const line = points.map((point,index) => (index ? "L" : "M") + point.x + " " + point.y).join(" ");
      chart.append(svgNode("path", {d:line + " L" + points[points.length - 1].x + " " + (top + plotHeight) + " L" + points[0].x + " " + (top + plotHeight) + " Z", class:"chart-area", "aria-hidden":"true"}));
      chart.append(svgNode("path", {d:line, class:"chart-line", "aria-hidden":"true"}));
    }
    const gap = Math.max(1, Math.min(7, stride * .28));
    const labelCount = width < 480 ? 3 : 5;
    const labelIndices = new Set([0, rows.length - 1]);
    for (let index = 1; index < labelCount - 1; index++) labelIndices.add(Math.round(index * (rows.length - 1) / (labelCount - 1)));
    const periods = [];
    const selected = Math.max(0, Math.min(rows.length - 1, statsChartSelection ?? 0));
    function periodDescription(row) {
      const interval = ["day", "week", "month", "year"].includes(snapshot.activity_interval) ? snapshot.activity_interval : "day";
      const step = statsCount(snapshot.activity_step) || 1;
      const grouped = interval !== "day" || step > 1;
      return (grouped ? "Period starting " : "") + statsUTCDate(row.date) + " UTC" + (grouped ? " (" + step + " " + interval + (step === 1 ? "" : "s") + ")" : "") + ": " + statsNumber(row.messages) + " messages";
    }
    function inspect(index, select = true) {
      if (select) {
        statsChartSelection = index;
        periods.forEach((period,offset) => period.setAttribute("tabindex", offset === index ? "0" : "-1"));
      }
      $("stats-chart-detail").textContent = periodDescription(rows[index]);
    }
    rows.forEach((row, index) => {
      const count = statsCount(row.messages);
      const point = points[index];
      const period = svgNode("g", {class:"chart-period", role:"button", tabindex:index === selected ? "0" : "-1", "aria-label":periodDescription(row)});
      period.dataset.periodIndex = String(index);
      if (statsChartMode === "bars") {
        const barHeight = count / axisMaximum * plotHeight;
        const barWidth = Math.max(1, Math.min(58, stride - gap));
        period.append(svgNode("rect", {x:point.x - barWidth / 2, y:top + plotHeight - barHeight, width:barWidth, height:barHeight, rx:Math.min(3, stride / 4), class:"chart-bar", "aria-hidden":"true"}));
      } else period.append(svgNode("circle", {cx:point.x, cy:point.y, r:rows.length < 35 ? 3.5 : 2, class:"chart-point", "aria-hidden":"true"}));
      period.append(svgNode("rect", {x:left + index * stride, y:top, width:stride, height:plotHeight, rx:3, class:"chart-hit", "aria-hidden":"true"}));
      period.addEventListener("pointerenter", () => inspect(index, false));
      period.addEventListener("focus", () => inspect(index));
      period.addEventListener("click", () => inspect(index));
      period.addEventListener("keydown", event => {
        const next = event.key === "ArrowRight" ? Math.min(rows.length - 1, index + 1) : event.key === "ArrowLeft" ? Math.max(0, index - 1) : event.key === "Home" ? 0 : event.key === "End" ? rows.length - 1 : null;
        if (next !== null) { event.preventDefault(); inspect(next); periods[next].focus({preventScroll:true}); }
        else if (["Enter", " "].includes(event.key)) { event.preventDefault(); inspect(index); }
      });
      periods.push(period);
      chart.append(period);
      if (labelIndices.has(index)) {
        const options = snapshot.activity_interval === "year" ? {year:"numeric"} : snapshot.activity_interval === "month" ? {month:"short", year:"2-digit"} : {month:"short", day:"numeric"};
        const anchor = rows.length === 1 ? "middle" : index === 0 ? "start" : index === rows.length - 1 ? "end" : "middle";
        const textX = rows.length === 1 ? point.x : index === 0 ? left : index === rows.length - 1 ? width - right : point.x;
        chart.append(svgNode("text", {x:textX, y:height - 9, "text-anchor":anchor, class:"chart-label"}, statsUTCDate(row.date, options)));
      }
      const tableRow = document.createElement("tr");
      for (const text of [row.date, statsNumber(count)]) {
        const cell = document.createElement("td");
        cell.textContent = String(text);
        tableRow.append(cell);
      }
      table.append(tableRow);
    });
    target.append(chart);
    if (statsChartSelection !== null) inspect(selected);
    if (focusedIndex !== undefined && periods[Number(focusedIndex)]) periods[Number(focusedIndex)].focus({preventScroll:true});
  }

  function renderStatsRhythm(snapshot) {
    const target = $("stats-heatmap");
    const focused = document.activeElement;
    const restoreFocus = focused?.dataset?.weekday !== undefined && focused?.dataset?.hour !== undefined;
    const focusKey = restoreFocus ? Number(focused.dataset.weekday) * 24 + Number(focused.dataset.hour) : null;
    target.replaceChildren();
    $("stats-rhythm-rows").replaceChildren();
    $("stats-rhythm-table").hidden = !Array.isArray(snapshot.rhythm);
    if (!Array.isArray(snapshot.rhythm)) {
      const empty = document.createElement("p");
      empty.className = "stats-empty";
      empty.textContent = "Messaging rhythm is not available in this archive view.";
      target.append(empty);
      $("stats-rhythm-detail").textContent = "Hourly message counts are unavailable.";
      return;
    }
    const counts = new Map(snapshot.rhythm.map(row => [row.weekday * 24 + row.hour, statsCount(row.messages)]));
    const maximum = Math.max(1, ...counts.values());
    const weekdays = Array.from({length:7},(_,index) => new Intl.DateTimeFormat(undefined,{weekday:"long",timeZone:"UTC"}).format(new Date(Date.UTC(2026,9,11 + index))));
    const blank = document.createElement("span");
    blank.setAttribute("aria-hidden", "true");
    target.append(blank);
    for (let hour = 0; hour < 24; hour++) {
      const label = document.createElement("span");
      label.className = "stats-heatmap-hour";
      label.textContent = hour % 3 === 0 ? String(hour).padStart(2,"0") : "";
      label.setAttribute("aria-hidden", "true");
      target.append(label);
    }
    const cells = [];
    const selected = statsRhythmSelection ?? 0;
    function inspect(index, select = true) {
      if (select) {
        statsRhythmSelection = index;
        cells.forEach((cell,offset) => cell.setAttribute("tabindex", offset === index ? "0" : "-1"));
      }
      const day = Math.floor(index / 24), hour = index % 24;
      $("stats-rhythm-detail").textContent = weekdays[day] + ", " + String(hour).padStart(2,"0") + ":00–" + String(hour).padStart(2,"0") + ":59 UTC: " + statsNumber(counts.get(index) || 0) + " messages";
    }
    for (let day = 0; day < 7; day++) {
      const label = document.createElement("span");
      label.className = "stats-heatmap-day";
      label.textContent = new Intl.DateTimeFormat(undefined,{weekday:"short",timeZone:"UTC"}).format(new Date(Date.UTC(2026,9,11 + day)));
      label.setAttribute("aria-hidden", "true");
      target.append(label);
      let total = 0;
      for (let hour = 0; hour < 24; hour++) {
        const index = day * 24 + hour, count = counts.get(index) || 0;
        total += count;
        const cell = document.createElement("button");
        cell.type = "button";
        cell.className = "stats-heatmap-cell";
        cell.dataset.weekday = String(day);
        cell.dataset.hour = String(hour);
        cell.dataset.level = count ? String(Math.max(1, Math.ceil(count / maximum * 4))) : "0";
        const description = weekdays[day] + ", " + String(hour).padStart(2,"0") + ":00–" + String(hour).padStart(2,"0") + ":59 UTC: " + statsNumber(count) + " messages";
        cell.setAttribute("aria-label", description);
        cell.setAttribute("title", description);
        cell.setAttribute("tabindex", index === selected ? "0" : "-1");
        cell.addEventListener("pointerenter", () => inspect(index, false));
        cell.addEventListener("focus", () => inspect(index));
        cell.addEventListener("click", () => inspect(index));
        cell.addEventListener("keydown", event => {
          const next = event.key === "ArrowRight" ? day * 24 + Math.min(23,hour + 1) : event.key === "ArrowLeft" ? day * 24 + Math.max(0,hour - 1) : event.key === "ArrowDown" ? Math.min(6,day + 1) * 24 + hour : event.key === "ArrowUp" ? Math.max(0,day - 1) * 24 + hour : event.key === "Home" ? day * 24 : event.key === "End" ? day * 24 + 23 : null;
          if (next !== null) { event.preventDefault(); inspect(next); cells[next].focus({preventScroll:true}); cells[next].scrollIntoView?.({block:"nearest",inline:"nearest"}); }
        });
        cells.push(cell);
        target.append(cell);
      }
      const row = document.createElement("tr");
      const name = document.createElement("th"); name.scope = "row"; name.textContent = weekdays[day];
      const count = document.createElement("td"); count.textContent = statsNumber(total);
      row.append(name,count);
      $("stats-rhythm-rows").append(row);
    }
    if (statsRhythmSelection !== null) inspect(selected);
    if (restoreFocus && cells[focusKey]) cells[focusKey].focus({preventScroll:true});
  }

  function renderStatsInsights(snapshot) {
    const insights = snapshot.insights;
    if (!insights) {
      for (const id of ["stats-active-days", "stats-streak", "stats-busiest-day", "stats-daily-average"]) $(id).textContent = "—";
      $("stats-busiest-detail").textContent = "Daily highlights are unavailable.";
      return;
    }
    const activeDays = statsCount(insights.active_days);
    const streak = statsCount(insights.longest_streak_days);
    $("stats-active-days").textContent = statsNumber(activeDays);
    $("stats-streak").textContent = statsNumber(streak) + (streak === 1 ? " day" : " days");
    $("stats-busiest-day").textContent = insights.busiest_date ? statsUTCDate(insights.busiest_date) : "No active days";
    $("stats-busiest-detail").textContent = insights.busiest_date ? statsNumber(insights.busiest_date_messages) + " messages on this date" : "No saved messages in this view";
    $("stats-daily-average").textContent = new Intl.NumberFormat(undefined,{maximumFractionDigits:1}).format(activeDays ? statsCount(snapshot.messages) / activeDays : 0);
  }

  function renderStatsBreakdown(snapshot) {
    const table = $("stats-aggregate-rows");
    table.replaceChildren();
    const breakdown = snapshot.breakdown;
    $("stats-aggregate-empty").hidden = true;
    if (!breakdown) {
      $("stats-chats-total").textContent = "";
      $("stats-page-description").textContent = "Message breakdown is not available.";
      $("stats-page-prev").disabled = true;
      $("stats-page-next").disabled = true;
      return;
    }
    statsPage = breakdown.page;
    renderStatsControls();
    const rows = breakdown.rows;
    for (const source of rows) {
      const row = document.createElement("tr");
      row.setAttribute("role", "row");
      const name = document.createElement("td");
      name.setAttribute("role", "cell");
      name.dataset.label = statsDimension === "people" ? "Person" : "Chat";
      name.append(bdi(source.name || (statsDimension === "people" ? "Unnamed sender" : "Unnamed chat")));
      row.append(name);
      for (const [label,value] of [["Messages",source.messages],["Sent",source.sent],["Received",source.received],...(statsDimension === "people" ? [["Chats",source.chats]] : []),["Last active",statsDate(source.last_message_at,undefined,source.messages > 0)]]) {
        const cell = document.createElement("td");
        cell.setAttribute("role", "cell");
        cell.dataset.label = label;
        cell.textContent = typeof value === "number" ? statsNumber(value) : value;
        row.append(cell);
      }
      table.append(row);
    }
    const total = statsCount(breakdown.total);
    $("stats-chats-total").textContent = statsNumber(total) + (statsDimension === "people" ? " people" : " chats");
    const start = rows.length ? (statsPage - 1) * statsPageSize + 1 : 0;
    const end = rows.length ? start + rows.length - 1 : 0;
    $("stats-page-description").textContent = "Showing " + statsNumber(start) + "–" + statsNumber(end) + " of " + statsNumber(total);
    $("stats-page-prev").disabled = statsPage <= 1;
    $("stats-page-next").disabled = statsPage * statsPageSize >= total;
    $("stats-aggregate-empty").hidden = rows.length > 0;
    $("stats-aggregate-empty").textContent = statsDimension === "people" ? "No people have readable messages matching these filters." : "No conversations match these filters.";
  }

  function renderStats(snapshot) {
    statsSnapshot = snapshot;
    const key = JSON.stringify({...snapshot, sampled_at:0, archive_revision:0});
    if (key === statsRenderKey) return;
    statsRenderKey = key;
    $("stats-message-count").textContent = statsNumber(snapshot.messages);
    $("stats-chat-count").textContent = statsNumber(snapshot.chats);
    $("stats-sent-count").textContent = statsNumber(snapshot.sent);
    $("stats-received-count").textContent = statsNumber(snapshot.received);
    $("stats-latest-message").textContent = statsDate(snapshot.last_message_at, undefined, statsCount(snapshot.messages) > 0);
    const range = $("stats-range");
    const rangeName = range.options[range.selectedIndex]?.text || "Selected history";
    $("stats-range-description").textContent = rangeName + ". Only readable, saved messages are included.";
    const intervals = {day:"day", week:"week", month:"month", year:"year"};
    const interval = intervals[snapshot.activity_interval] || "day";
    const step = statsCount(snapshot.activity_step) || 1;
    $("stats-activity-description").textContent = "Messages by date sent, grouped by " + (step > 1 ? step + " " + interval + "s" : interval) + " in UTC.";
    $("stats-activity-total").textContent = statsNumber(snapshot.messages) + " messages";
    $("stats-chats-total").textContent = statsNumber(snapshot.chats) + " chats";
    renderStatsChart(snapshot);
    renderStatsInsights(snapshot);
    renderStatsRhythm(snapshot);
    renderStatsBreakdown(snapshot);
    const kindLabels = {text:"Text", image:"Images", voice:"Voice notes", other:"Other"};
    const kindCounts = new Map((snapshot.kinds || []).map(kind => [kind.kind, statsCount(kind.messages)]));
    const kinds = $("stats-kinds"), composition = $("stats-kind-bar");
    kinds.replaceChildren();
    composition.replaceChildren();
    for (const [kind, label] of Object.entries(kindLabels)) {
      const count = kindCounts.get(kind) || 0;
      const item = document.createElement("li");
      const color = document.createElement("span");
      color.className = "stats-kind-color";
      color.dataset.kind = kind;
      color.setAttribute("aria-hidden", "true");
      const name = document.createElement("span");
      name.textContent = label;
      const number = document.createElement("strong");
      number.textContent = statsNumber(count);
      item.append(color, name, number);
      kinds.append(item);
      if (count) {
        const segment = document.createElement("span");
        segment.dataset.kind = kind;
        segment.style.flexGrow = String(count);
        segment.style.flexBasis = "0";
        composition.append(segment);
      }
    }
    const voice = statsCount(snapshot.media?.voice_messages);
    const transcribed = Math.min(voice, statsCount(snapshot.media?.transcribed_voice));
    const retained = statsCount(snapshot.media?.retained_voice);
    $("stats-transcribed").textContent = statsNumber(transcribed) + " / " + statsNumber(voice);
    $("stats-voice-progress").max = voice || 1;
    $("stats-voice-progress").value = transcribed;
    $("stats-voice-description").textContent = voice ? statsNumber(retained) + " original recordings retained. A voice note needs a saved recording before it can be transcribed." : "No voice notes in this view.";
  }

  function bytesText(bytes) {
    const value = Number(bytes);
    if (!Number.isFinite(value) || value <= 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB"];
    const index = Math.min(3, Math.floor(Math.log(value) / Math.log(1024)));
    return new Intl.NumberFormat(undefined, {maximumFractionDigits:1}).format(value / 1024 ** index) + " " + units[index];
  }

  function modelOperationText(operation) {
    const states = {queued:"Waiting to start", running:"Working", completed:"Task finished", failed:"Task failed", cancelled:"Task cancelled", interrupted:"Task interrupted"};
    const phases = {preparing:"Preparing", downloading:"Downloading", download:"Downloading", verifying:"Verifying", verify:"Verifying", removing:"Removing", remove:"Removing", reusing:"Checking existing resource", installed:"Resource stored"};
    return (["queued","running"].includes(operation.state) ? phases[operation.phase] : "") || states[operation.state] || "State unavailable";
  }

  function modelParagraph(card, text, className = "") {
    const node = document.createElement("p");
    node.textContent = text;
    if (className) node.className = className;
    card.append(node);
    return node;
  }

  function renderModels(profiles, snapshot) {
    const key = JSON.stringify([profiles, snapshot]);
    if (key === modelRenderKey) return;
    modelRenderKey = key;
    const focused = document.activeElement;
    const focusProfile = focused?.dataset?.profileId;
    const focusAction = focused?.dataset?.action;
    const states = new Map((snapshot.items || []).map(item => [item.profile_id, item]));
    const active = (snapshot.items || []).some(item => ["queued", "running"].includes(item.operation?.state));
    const list = $("model-list");
    list.replaceChildren();
    for (const profile of profiles) {
      const state = states.get(profile.id) || {installation_state:"unknown"};
      const operation = state.operation;
      const running = ["queued", "running"].includes(operation?.state);
      const card = document.createElement("article"); card.className = "model-card";
      const title = document.createElement("h3"); title.append(bdi(profile.id)); title.tabIndex = -1; card.append(title);
      const qualification = profile.selection === "accepted" ? "Qualified profile" : profile.selection === "failed" ? "Qualification failed" : "Candidate: qualification pending";
      modelParagraph(card, qualification + " · " + profile.platform + " · " + profile.license, profile.selection === "failed" ? "model-warning" : "");
      const gates = {memory_gate_failed:"The memory limit was exceeded. This profile is not qualified for release.", numerical_reference_failed:"The numerical accuracy check failed. This profile is not qualified for release.", NOT_RUN:"Required qualification checks have not run. This profile is not qualified for release."};
      if (gates[profile.gate_status]) modelParagraph(card, gates[profile.gate_status]);
      const installations = {unknown:"Installation not checked", absent:"Not installed", present:"Stored; verification needed", verified:"Verified on this computer", unavailable:"Installation unavailable"};
      modelParagraph(card, installations[state.installation_state] || installations.unknown, "model-state");
      modelParagraph(card, bytesText(profile.bytes) + " pinned resource" + (state.verified_at ? " · Verified " + dateText(state.verified_at) : ""));
      if (state.code && messages[state.code]) modelParagraph(card, messages[state.code], "model-warning");
      if (operation) {
        modelParagraph(card, modelOperationText(operation));
        if (operation.code && messages[operation.code] && operation.code !== state.code) modelParagraph(card, messages[operation.code], "model-warning");
        if (operation.additional_bytes > 0) modelParagraph(card, "Free at least " + bytesText(operation.additional_bytes) + " more disk space before retrying.", "model-warning");
        if (running) {
          const progress = document.createElement("progress"); progress.className = "model-progress"; progress.setAttribute("aria-label", "Model task progress");
          if (operation.total_bytes > 0) { progress.max = operation.total_bytes; progress.value = Math.min(operation.total_bytes, operation.completed_bytes); }
          else progress.removeAttribute("value");
          card.append(progress);
          if (operation.total_bytes > 0) modelParagraph(card, bytesText(operation.completed_bytes) + " of " + bytesText(operation.total_bytes));
        }
      }
      const actions = document.createElement("div"); actions.className = "model-actions";
      function action(kind, label, disabled = false) {
        const button = document.createElement("button"); button.type = "button"; button.className = "secondary"; button.textContent = label;
        button.dataset.profileId = profile.id; button.dataset.action = kind; button.disabled = disabled || modelBusy;
        button.addEventListener("click", () => modelAction(button, profile.id, kind, operation?.id));
        actions.append(button);
      }
      if (running) action("cancel", "Cancel task", !operation.id);
      else {
        if (profile.download_available) action("install", "Download pinned resource", active);
        else modelParagraph(card, "Download unavailable. This profile requires a verified offline import.");
        action("verify", "Verify local resource", active || state.installation_state === "absent");
        if (state.installation_state !== "absent") action("remove", "Remove local resource", active);
      }
      card.append(actions); list.append(card);
    }
    if (!profiles.length) modelParagraph(list, "No pinned model profiles are available. Keyword search remains available.");
    if (focusProfile && focusAction) {
      const replacement = Array.from(list.querySelectorAll("button")).find(button => button.dataset.profileId === focusProfile && button.dataset.action === focusAction);
      if (replacement && !replacement.disabled) replacement.focus({preventScroll:true});
      else Array.from(list.querySelectorAll("article")).find(card => card.querySelector("button")?.dataset.profileId === focusProfile)?.querySelector("h3").focus({preventScroll:true});
    }
  }

  async function refreshModels() {
    if (authExpired || !$("models-dialog").open || modelBusy) return;
    const version = ++modelVersion;
    try {
      const [catalog, snapshot] = await Promise.all([api("/api/models"), api("/api/models/state")]);
      if (version !== modelVersion || authExpired || !$("models-dialog").open) return;
      renderModels(catalog.profiles || [], snapshot);
      feedback("models-feedback", "State reflects tasks observed by this app. Use Verify to check existing files.");
    } catch (error) {
      if (version === modelVersion && !authExpired) {
        $("model-list").replaceChildren(); modelRenderKey = "";
        feedback("models-feedback", error.message, true);
      }
    }
  }

  function localAIFormChoice() {
    const mode=$("local-ai-mode").value;
    if(mode==="off")return {mode:"off",caption:false};
    return {mode,encoder_profile:$("local-ai-encoder").value,ocr_profile:$("local-ai-ocr").value,caption:$("local-ai-caption").checked,speech_language:$("local-ai-speech").value};
  }

  function localAIEligible(kind,id,mode) {
    return (localAISnapshot?.options||[]).some(option=>option.kind===kind&&option.state===mode&&(id===undefined||option.profile_id===id));
  }

  function updateLocalAIControls() {
    const choice=localAIFormChoice(), off=choice.mode==="off";
    const disabled=localAIBusy||!localAISnapshot||authExpired;
    $("local-ai-mode").disabled=disabled;
    for(const kind of ["encoder","ocr","speech"])$("local-ai-"+kind).disabled=disabled||off||!localAIEligible(kind,undefined,choice.mode);
    $("local-ai-caption").disabled=disabled||off||!localAIEligible("caption",undefined,choice.mode);
    const selected=choice.encoder_profile||choice.ocr_profile||choice.caption||choice.speech_language;
    const eligible=off||selected&&(!choice.encoder_profile||localAIEligible("encoder",choice.encoder_profile,choice.mode))&&(!choice.ocr_profile||localAIEligible("ocr",choice.ocr_profile,choice.mode))&&(!choice.caption||localAIEligible("caption",undefined,choice.mode))&&(!choice.speech_language||localAIEligible("speech",choice.speech_language,choice.mode));
    $("local-ai-save").disabled=disabled||!eligible;
    refreshSelects("local-ai-mode", "local-ai-encoder", "local-ai-ocr", "local-ai-speech");
  }

  function clearLocalAI() {
    localAIVersion++; localAISnapshot=null; localAIDirty=false; localAIDraftRevision=0;
    $("local-ai-form").reset();
    for(const id of ["local-ai-encoder","local-ai-ocr","local-ai-speech"])$(id).replaceChildren(new Option("Off",""));
    for(const option of $("local-ai-mode").options)option.disabled=option.value!=="off";
    for(const id of ["local-ai-status","local-ai-choice","local-ai-options"])$(id).textContent="";
    feedback("local-ai-feedback",""); updateLocalAIControls();
  }

  function renderLocalAI(snapshot) {
    const choice=localAIDirty?localAIFormChoice():snapshot.choice;
    localAISnapshot=snapshot;
    const states={off:"Off",applying:"Applying saved choice; keyword search remains available",active:"Active; indexing and media coverage are reported separately",unavailable:"Saved choice unavailable; keyword search remains available",cleanup_pending:"Waiting for safe cleanup; replacement AI work stays blocked"};
    $("local-ai-status").textContent=states[snapshot.state]||states.unavailable;
    if(messages[snapshot.code])$("local-ai-status").textContent+=". "+messages[snapshot.code];
    const saved=snapshot.choice;
    $("local-ai-choice").textContent="Saved choice: "+(saved.mode==="off"?"Off":saved.mode==="preview"?"Preview · experimental":"Qualified")+(saved.encoder_profile?" · Semantic: "+saved.encoder_profile:"")+(saved.ocr_profile?" · Image text: "+saved.ocr_profile:"")+(saved.caption?" · Image descriptions":"")+(saved.speech_language?" · Voice: "+({he:"Hebrew",en:"English",mixed:"Hebrew + English"}[saved.speech_language]||"unavailable"):"");
    $("local-ai-mode").value=choice.mode;
    for(const option of $("local-ai-mode").options) {
      const enabled=option.value==="off"||(snapshot.options||[]).some(item=>item.state===option.value);
      option.disabled=!enabled;
      if(option.value==="accepted")option.textContent=enabled?"Qualified":"Qualified · unavailable";
    }
    for(const kind of ["encoder","ocr","speech"]) {
      const id=kind==="speech"?choice.speech_language:choice[kind+"_profile"];
      const select=$("local-ai-"+kind); select.replaceChildren(new Option("Off",""));
      for(const option of snapshot.options||[])if(option.kind===kind&&option.state===choice.mode) {
        const label=kind==="speech"?({he:"Hebrew",en:"English",mixed:"Hebrew + English"}[option.profile_id]||"Unavailable"):option.profile_id;
        select.append(new Option(label,option.profile_id));
      }
      if(id&&!Array.from(select.options).some(option=>option.value===id)) {
        const unavailable=new Option("Saved choice unavailable · "+id,id); unavailable.disabled=true; select.append(unavailable);
      }
      select.value=id||"";
    }
    $("local-ai-caption").checked=Boolean(choice.caption);
    const eligible=(snapshot.options||[]).filter(option=>option.state==="preview"||option.state==="accepted");
    $("local-ai-options").textContent=eligible.length?"Only the components offered for this mode can be selected. Other installed resources remain inactive.":"No eligible AI components are available. Installing or verifying models does not enable them.";
    updateLocalAIControls();
  }

  async function refreshLocalAI() {
    if(authExpired||!$("models-dialog").open||localAIBusy)return;
    const version=++localAIVersion;
    try {
      const snapshot=await api("/api/local-ai");
      if(version!==localAIVersion||authExpired||!$("models-dialog").open)return;
      if(!Number.isSafeInteger(snapshot.revision)||snapshot.revision<1||!snapshot.choice)throw new Error(messages.local_ai_unavailable);
      renderLocalAI(snapshot);
    } catch(error) {
      if(version===localAIVersion&&!authExpired&&$("models-dialog").open) {
        localAISnapshot=null; updateLocalAIControls();
        $("local-ai-status").textContent=messages.local_ai_unavailable;
        $("local-ai-choice").textContent=localAIDirty?"Your unsaved choice is retained. Refresh to check current state before applying.":"The current saved choice could not be checked.";
        $("local-ai-options").textContent="Current eligibility is unavailable. Apply stays disabled until state reloads.";
        feedback("local-ai-feedback",error.message,true);
      }
    }
  }

  function editLocalAI() {
    if(localAIBusy||!localAISnapshot)return;
    if(!localAIDirty)localAIDraftRevision=localAISnapshot.revision;
    localAIDirty=true; updateLocalAIControls();
  }

  async function saveLocalAI(event) {
    event.preventDefault(); if(authExpired||localAIBusy||!localAISnapshot||$("local-ai-save").disabled)return;
    const epoch=authEpoch;
    const input={choice:localAIFormChoice(),expected_revision:localAIDirty?localAIDraftRevision:localAISnapshot.revision};
    localAIBusy=true; const version=++localAIVersion; updateLocalAIControls();
    feedback("local-ai-feedback","Saving this choice and checking local AI state…");
    let failure="";
    try {
      const snapshot=await api("/api/local-ai/select","POST",input);
      if(version!==localAIVersion||authExpired||!$("models-dialog").open)return;
      localAIDirty=false; localAIDraftRevision=0; renderLocalAI(snapshot);
      feedback("local-ai-feedback","Choice saved. The current state above shows whether it has applied.");
    } catch(error) {
      if(version===localAIVersion&&!authExpired&&$("models-dialog").open) { failure=error.message; if(error.message===messages.local_ai_conflict) { localAIDirty=false;localAIDraftRevision=0; } }
    } finally { if(epoch===authEpoch){localAIBusy=false; updateLocalAIControls();} }
    if(epoch!==authEpoch||authExpired)return;
    await refreshLocalAI();
    if(failure&&!authExpired&&$("models-dialog").open)feedback("local-ai-feedback",failure,true);
  }

  function updateBudgetUnits() {
    const gib=$("original-budget-unit").value==="gib";
    $("original-budget-size").min=gib?"1":"64";
    $("original-budget-size").max=gib?"1024":"1048576";
    refreshSelects("original-budget-preset", "original-budget-unit");
  }

  function updateVoiceRetryControls() {
    $("voice-retry-button").disabled = voiceRetryBusy || authExpired;
  }

  async function retryVoiceDownloads() {
    if (authExpired || voiceRetryBusy) return;
    const epoch = authEpoch;
    voiceRetryBusy = true;
    updateVoiceRetryControls();
    feedback("voice-retry-feedback", "Checking voice downloads and missing references…");
    try {
      const value = await api("/api/voice/retry", "POST", {});
      if (epoch !== authEpoch || authExpired || !$("models-dialog").open) return;
      if (!Number.isSafeInteger(value.queued) || !Number.isSafeInteger(value.skipped) || !Number.isSafeInteger(value.requested) || value.queued < 0 || value.skipped < 0 || value.requested < 0 || value.requested > 8 || value.queued + value.skipped > 32) throw new Error(messages.voice_retry_unavailable);
      const text = value.queued
        ? value.queued + " voice download" + (value.queued === 1 ? "" : "s") + " queued. Downloads need a connected WhatsApp session; transcription needs its configured service."
        : "No voice downloads could be queued in this batch.";
      const requested = value.requested ? " Asked your phone for " + value.requested + " missing voice note reference" + (value.requested === 1 ? "." : "s.") + " Keep WhatsApp open on your phone, wait for its response, then click Retry voice downloads again to queue returned recordings." : "";
      feedback("voice-retry-feedback", text + requested + (value.skipped ? " " + value.skipped + " unusable download reference" + (value.skipped === 1 ? " was" : "s were") + " skipped. Retry again to check the next batch." : ""));
    } catch (error) {
      if (epoch === authEpoch && !authExpired && $("models-dialog").open) feedback("voice-retry-feedback", error.message, true);
    } finally {
      if (epoch === authEpoch) {
        voiceRetryBusy = false;
        updateVoiceRetryControls();
      }
    }
  }

  function updateBudgetControls() {
    const disabled=budgetBusy||!budgetSnapshot;
    for(const id of ["original-budget-preset","original-budget-unit","original-budget-size","original-budget-save"])$(id).disabled=disabled;
    refreshSelects("original-budget-preset", "original-budget-unit");
  }

  function editBudget() {
    if(!budgetDirty)budgetDraftRevision=budgetSnapshot?.revision||0;
    budgetDirty=true;
  }

  async function refreshBudget() {
    if(authExpired||!$("models-dialog").open||budgetBusy)return;
    updateBudgetControls();
    const version=++budgetVersion;
    try {
      const value=await api("/api/original-budget");
      if(version!==budgetVersion||authExpired||!$("models-dialog").open)return;
      if(!Number.isSafeInteger(value.maximum_bytes)||!Number.isSafeInteger(value.charged_bytes)||!Number.isSafeInteger(value.revision))throw new Error(messages.original_budget_unavailable);
      budgetSnapshot=value;
      $("original-budget-summary").textContent=bytesText(value.charged_bytes)+" stored or reserved of "+bytesText(value.maximum_bytes)+" ("+value.maximum_bytes.toLocaleString()+" bytes) allowed for original media.";
      $("original-budget-pressure").textContent="Storage is at or above its limit. New originals wait for safe cleanup to make room. Saved message text remains searchable.";
      $("original-budget-pressure").hidden=!value.pressure;
      if(!budgetDirty) {
        budgetDraftRevision=value.revision;
        const mib=value.maximum_bytes/1048576;
        $("original-budget-unit").value="mib";
        $("original-budget-size").value=Number.isInteger(mib)?String(mib):"";
        $("original-budget-preset").value=["64","512","2048","8192"].includes(String(mib))?String(mib):"custom";
        updateBudgetUnits();
      }
      updateBudgetControls();
    } catch(error) {
      if(version===budgetVersion&&!authExpired&&$("models-dialog").open) {
        budgetSnapshot=null; updateBudgetControls(); $("original-budget-summary").textContent="Current media storage state unavailable."; $("original-budget-pressure").hidden=true;
        feedback("original-budget-feedback",error.message,true);
      }
    }
  }

  async function saveBudget(event) {
    event.preventDefault(); if(authExpired||budgetBusy||!budgetSnapshot)return;
    const focused=document.activeElement, epoch=authEpoch;
    const restoreTarget=["original-budget-preset","original-budget-unit","original-budget-size","original-budget-save"].includes(focused?.id)?focused:null;
    budgetBusy=true; budgetVersion++; updateBudgetControls();
    const version=budgetVersion;
    let failure="";
    try {
      const raw=$("original-budget-size").value;
      if(!/^\d{1,12}$/u.test(raw))throw new Error("Enter a whole number of MiB or GiB.");
      const bytes=BigInt(raw)*($("original-budget-unit").value==="gib"?1073741824n:1048576n);
      if(bytes<67108864n||bytes>1099511627776n)throw new Error("Choose a limit from 64 MiB to 1 TiB.");
      await api("/api/original-budget","PUT",{maximum_bytes:Number(bytes),expected_revision:budgetDirty?budgetDraftRevision:budgetSnapshot.revision});
      assertOwner(epoch);
      if(version!==budgetVersion||!$("models-dialog").open)return;
      budgetDirty=false;
      feedback("original-budget-feedback","Media storage limit saved. Existing files and reservations remain charged until safe cleanup.");
    } catch(error) { if(epoch===authEpoch){failure=error.message; if(error.message===messages.original_budget_changed)budgetDirty=false;} }
    finally {
      if(epoch===authEpoch){budgetBusy=false; updateBudgetControls();}
      const dialog=$("models-dialog"), active=document.activeElement;
      if(restoreTarget&&epoch===authEpoch&&version===budgetVersion&&!authExpired&&dialog.open&&restoreTarget.isConnected&&!restoreTarget.disabled&&(active===document.body||active===dialog))restoreTarget.focus({preventScroll:true});
    }
    if(epoch!==authEpoch||authExpired)return;
    await refreshBudget();
    if(failure&&!authExpired&&$("models-dialog").open)feedback("original-budget-feedback",failure,true);
  }

  async function modelAction(button, profileID, kind, operationID) {
    if (modelBusy || authExpired) return;
    const epoch = authEpoch;
    modelBusy = true; button.disabled = true; modelVersion++;
    feedback("models-feedback", kind === "cancel" ? "Requesting cancellation…" : "Requesting model task…");
    let failure = "";
    try {
      if (kind === "cancel") await api("/api/models/operations/cancel", "POST", {operation_id:operationID});
      else await api("/api/models/operations", "POST", {profile_id:profileID, kind});
      assertOwner(epoch);
      modelRenderKey = "";
    } catch (error) { failure = error.message; }
    finally { if(epoch===authEpoch){modelBusy = false; button.disabled = false;} }
    if(epoch!==authEpoch||authExpired)return;
    await refreshModels();
    if (failure && !authExpired) feedback("models-feedback", failure, true);
  }

  async function loadChats() {
    const epoch = authEpoch;
    const list = [];
    let cursor = "";
    for (let page = 0; page < 10; page++) {
      const result = await api("/api/chats?limit=100" + (cursor ? "&cursor=" + encodeURIComponent(cursor) : ""));
      assertOwner(epoch);
      list.push(...(result.Chats || []));
      cursor = result.NextCursor || "";
      if (!cursor) break;
    }
    assertOwner(epoch);
    chats = list;
    renderChatChoices();
    renderConnectionChats();
    renderSendPermissionChats();
    const select = $("search-chat"), chosen = select.value;
    select.replaceChildren(new Option("All included chats", ""));
    for (const chat of chats) {
      if (setup?.Policy.ChatMode === "all" || chat.Selected) select.append(new Option(chat.Name || "Unnamed chat", chat.ID));
    }
    select.value = chosen;
    refreshSelects("search-chat");
    $("chat-discovery-note").textContent = cursor ? "Showing the first discovered chats. More chats are available; narrow your archive selection before importing them." : chats.length ? "Select chats to save their messages. Coverage below describes received history only." : "No chats discovered yet. Link WhatsApp, then refresh. With no selection, no message bodies are saved.";
    renderStatsChatOptions();
    renderImportState();
  }

  function renderChatChoices() {
    const target = $("chat-choices");
    const previous = new Map(Array.from(target.querySelectorAll("input"), (input) => [input.value,input.checked]));
    const included = new Set(setup?.Policy.IncludedChats || []);
    target.replaceChildren();
    for (const chat of chats) {
      const label = document.createElement("label"); label.className = "chat-choice";
      const input = document.createElement("input"); input.type = "checkbox"; input.value = chat.ID;
      input.checked = previous.has(chat.ID) ? previous.get(chat.ID) : included.has(chat.ID);
      const details = document.createElement("span");
      details.append(bdi(chat.Name || "Unnamed chat"));
      const coverage = document.createElement("small");
      coverage.append(document.createTextNode(chat.Coverage === "partial" ? "Partial coverage · " : "Coverage unknown · "), bdi(dateText(chat.ObservedOldest)), document.createTextNode(" to "), bdi(dateText(chat.ObservedLatest)));
      details.append(coverage); label.append(input, details); target.append(label);
    }
  }

  const clientNames = {codex_cli:"Codex CLI", codex_desktop:"Codex desktop", claude_code:"Claude Code", claude_connectors:"Claude connectors", composio:"Composio"};

  function clearClientGuide() {
    clientGuideID = "";
    clientGuideBinding = null;
    $("client-guide").hidden = true;
    $("client-guide-remote").hidden = true;
    $("client-guide-url").value = "";
    $("client-copy-url").disabled = true;
    $("client-guide-reveal").disabled = true;
    for (const id of ["client-guide-config", "client-guide-command"]) $(id).value = "";
    for (const id of ["client-guide-status", "client-guide-explanation", "client-guide-placement", "client-guide-test", "client-guide-revoke", "client-guide-version"]) $(id).textContent = "";
    $("client-guide-warnings").replaceChildren();
  }

  function currentGuideRemoteConnection() {
    if(authExpired||!$("clients-dialog").open||$("client-guide-remote").hidden||!clientGuideBinding||clientGuideBinding.mode!=="remote"||!$("client-guide-url").value||clientSnapshot?.remote_state!=="ready"||connectionRecovery(clientSnapshot,"remote")!=="ready")return null;
    return (clientSnapshot.items||[]).find(item=>item.id===clientGuideBinding.id&&item.revision===clientGuideBinding.revision&&item.client===clientGuideBinding.client&&item.mode==="remote"&&item.state==="ready"&&item.revision>0)||null;
  }

  function updateClientGuideControls() {
    const disabled=clientBusy||!currentGuideRemoteConnection();
    $("client-copy-url").disabled=disabled;
    $("client-guide-reveal").disabled=disabled;
  }

  function connectionRecovery(snapshot,mode) {
    return snapshot?.[mode+"_recovery_state"] || snapshot?.recovery_state || "recovery_pending";
  }

  function restoreConnectionFocus(id,action) {
    if(authExpired||!$("clients-dialog").open||$("client-secret-dialog").open)return;
    const list=$("client-list");
    const replacement=Array.from(list.querySelectorAll("button")).find(button=>button.dataset.connectionId===id&&button.dataset.action===action&&!button.disabled);
    const heading=Array.from(list.querySelectorAll("article")).find(card=>card.dataset.connectionId===id)?.querySelector("h3");
    (replacement||heading||$("clients-refresh")).focus({preventScroll:true});
  }

  function clearRemoteSecret(close=true) {
    const dialog=$("client-secret-dialog"), focusID=remoteSecretFocusID;
    remoteSecretVersion++;
    remoteSecretID=""; remoteSecretRevision=0; remoteSecretExpiry=0;
    $("client-secret-token").value="";
    $("client-secret-url").textContent="";
    $("client-secret-expiry").textContent="";
    $("client-secret-copy").disabled=true;
    feedback("client-secret-feedback","");
    if(close&&dialog.open)dialog.close();
    if(!dialog.open) {
      remoteSecretFocusID="";
      if(focusID)restoreConnectionFocus(focusID,"reveal");
    }
  }

  function renderConnectionChats() {
    const target = $("client-chat-choices");
    const selected = new Set(Array.from(target.querySelectorAll("input:checked"), input=>input.value));
    target.replaceChildren();
    for (const chat of chats) {
      if (setup?.Policy.ChatMode !== "all" && !chat.Selected) continue;
      const label = document.createElement("label"); label.className="chat-choice";
      const input = document.createElement("input"); input.type="checkbox"; input.value=chat.ID; input.checked=selected.has(chat.ID);
      label.append(input, bdi(chat.Name || "Unnamed chat")); target.append(label);
    }
    $("client-chat-note").textContent = target.children.length ? "Choose 1 to 256 archived chats. Unselected chats are excluded. This list shows the chats currently loaded by the archive." : "No archived chats are available to select. Save your archive settings and import chats first.";
  }

  function connectionSendPermission(item) {
    const permission = item?.send_permission;
    if (!permission || !["none", "selected", "all"].includes(permission.mode) || !Number.isSafeInteger(permission.revision) || permission.revision < 0 || !Array.isArray(permission.chats)) return null;
    if (permission.chats.some(id => typeof id !== "string" || !id) || new Set(permission.chats).size !== permission.chats.length) return null;
    if (permission.mode === "selected" ? permission.chats.length < 1 || permission.chats.length > 100 : permission.chats.length !== 0) return null;
    return permission;
  }

  function currentSendPermissionConnection() {
    if (!sendPermissionBinding || authExpired || !$("clients-dialog").open || sendPermissionBinding.epoch !== authEpoch) return null;
    const item = (clientSnapshot?.items || []).find(value => value.id === sendPermissionBinding.id && value.revision === sendPermissionBinding.revision);
    const permission = connectionSendPermission(item);
    if (!sendPermissionEditable(item) || !permission || permission.revision !== sendPermissionBinding.sendRevision || JSON.stringify(permission) !== sendPermissionBinding.permissionKey) return null;
    return item;
  }

  function sendPermissionEditable(item) {
    return !!item && item.state === "ready" && Number.isSafeInteger(item.revision) && item.revision > 0 && new Date(item.expires_at).getTime() > Date.now() && !!connectionSendPermission(item) && (item.scope?.all_chats === true || Array.isArray(item.scope?.chats)) && connectionRecovery(clientSnapshot, item.mode) === "ready" && clientSnapshot?.[item.mode + "_state"] === "ready";
  }

  function clearSendPermissions(restoreFocus = false) {
    const id = sendPermissionBinding?.id;
    sendPermissionVersion++;
    sendPermissionBinding = null;
    sendPermissionMode = "none";
    sendPermissionSelection.clear();
    $("client-send-editor").hidden = true;
    $("client-send-name").textContent = "";
    $("client-send-search").value = "";
    $("client-send-chats").replaceChildren();
    $("client-send-selection-note").textContent = "";
    $("client-send-save").disabled = true;
    feedback("client-send-feedback", "");
    if (restoreFocus && id) restoreConnectionFocus(id, "permissions");
  }

  function openSendPermissions(item) {
    if (clientBusy || !sendPermissionEditable(item) || authExpired || !$("clients-dialog").open) return;
    const current = (clientSnapshot.items || []).find(value => value.id === item.id && value.revision === item.revision);
    if (!current || JSON.stringify(connectionSendPermission(current)) !== JSON.stringify(connectionSendPermission(item))) return;
    clearSendPermissions();
    const permission = connectionSendPermission(current);
    sendPermissionBinding = {id:current.id,revision:current.revision,sendRevision:permission.revision,permissionKey:JSON.stringify(permission),epoch:authEpoch};
    sendPermissionMode = permission.mode;
    sendPermissionSelection = new Set(permission.chats);
    $("client-send-name").textContent = current.label || "AI connection";
    for (const input of document.querySelectorAll("input[name=client-send-mode]")) input.checked = input.value === sendPermissionMode;
    $("client-send-editor").hidden = false;
    renderSendPermissionChats();
    updateSendPermissionControls();
    $("client-send-title").focus({preventScroll:false});
  }

  function eligibleSendPermissionChats(item) {
    const scope = new Set(item.scope?.chats || []);
    return chats.filter(chat => (setup?.Policy.ChatMode === "all" || chat.Selected) && (item.scope?.all_chats || scope.has(chat.ID)));
  }

  function sendPermissionNameKey(value) {
    return String(value).toLocaleLowerCase().normalize("NFD").replace(/[\u0591-\u05BD\u05BF-\u05C7]/gu, "").replace(/[^\p{L}\p{N}]/gu, "");
  }

  function renderSendPermissionChats() {
    const item = currentSendPermissionConnection();
    if (!item) return;
    const target = $("client-send-chats");
    const query = sendPermissionNameKey($("client-send-search").value);
    const visible = eligibleSendPermissionChats(item).filter(chat => !query || sendPermissionNameKey(chat.Name || "Unnamed chat").includes(query));
    target.replaceChildren();
    for (const chat of visible) {
      const label = document.createElement("label"); label.className = "chat-choice";
      const input = document.createElement("input"); input.type = "checkbox"; input.value = chat.ID; input.checked = sendPermissionSelection.has(chat.ID);
      input.addEventListener("change", () => {
        if (clientBusy || !currentSendPermissionConnection()) { input.checked = sendPermissionSelection.has(chat.ID); return; }
        if (input.checked && !sendPermissionSelection.has(chat.ID) && sendPermissionSelection.size >= 100) {
          input.checked = false;
          feedback("client-send-feedback", "Choose up to 100 chats, or use All permitted chats.", true);
        } else {
          if (input.checked) sendPermissionSelection.add(chat.ID); else sendPermissionSelection.delete(chat.ID);
          feedback("client-send-feedback", "");
        }
        updateSendPermissionControls();
      });
      label.append(input, bdi(chat.Name || "Unnamed chat")); target.append(label);
    }
    if (!visible.length) modelParagraph(target, query ? "No loaded chats match this name." : "No loaded chats are available in this connection's scope.", "field-help");
    updateSendPermissionControls();
  }

  function updateSendPermissionControls() {
    const item = currentSendPermissionConnection();
    const editable = !!item && !clientBusy;
    $("client-send-chat-panel").hidden = sendPermissionMode !== "selected";
    $("client-send-warning").hidden = sendPermissionMode === "none";
    $("client-send-search").disabled = !editable;
    $("client-send-cancel").disabled = clientBusy;
    $("client-send-clear").disabled = !editable || !sendPermissionSelection.size;
    for (const input of document.querySelectorAll("input[name=client-send-mode]")) input.disabled = !editable;
    for (const input of $("client-send-chats").querySelectorAll("input")) input.disabled = !editable || !input.checked && sendPermissionSelection.size >= 100;
    $("client-send-save").disabled = !editable || sendPermissionMode === "selected" && (sendPermissionSelection.size < 1 || sendPermissionSelection.size > 100);
    const visible = new Set(Array.from($("client-send-chats").querySelectorAll("input"), input => input.value));
    const hidden = Array.from(sendPermissionSelection).filter(id => !visible.has(id)).length;
    $("client-send-selection-note").textContent = sendPermissionSelection.size + " of 100 chats selected." + (hidden ? " " + hidden + " selected chat" + (hidden === 1 ? " is" : "s are") + " not shown in this list. Clear the selection to remove them." : "");
  }

  async function saveSendPermissions(event) {
    event.preventDefault();
    updateSendPermissionControls();
    const item = currentSendPermissionConnection();
    if (clientBusy || !item || $("client-send-save").disabled) return;
    const epoch = authEpoch, permissionVersion = sendPermissionVersion;
    const input = {id:item.id,revision:item.revision,mode:sendPermissionMode,chats:sendPermissionMode === "selected" ? Array.from(sendPermissionSelection) : [],expected_send_revision:sendPermissionBinding.sendRevision};
    clientBusy = true;
    const version = ++clientVersion;
    updateConnectionFields();
    feedback("client-send-feedback", "Saving send permissions…");
    let failure = "";
    try {
      await api("/api/connections/send-permission", "POST", input);
      assertOwner(epoch);
      if (version !== clientVersion || permissionVersion !== sendPermissionVersion || !$("clients-dialog").open) return;
      clearSendPermissions(true);
      clientFocusHint = {id:item.id,action:"permissions"};
      feedback("clients-feedback", "Send permissions saved. Your existing token is unchanged.");
    } catch (error) {
      if (epoch === authEpoch && !authExpired && version === clientVersion && permissionVersion === sendPermissionVersion && $("clients-dialog").open) {
        if (error.status === 409) {
          clearSendPermissions(true);
          failure = "Send permissions changed. Reopen Permissions and review the current state before saving again.";
        } else {
          feedback("client-send-feedback", error.message, true);
        }
      }
    } finally {
      if (epoch === authEpoch) { clientBusy = false; updateConnectionFields(); }
    }
    if (epoch !== authEpoch || authExpired || version !== clientVersion || !$("clients-dialog").open) return;
    clientRenderKey = "";
    await refreshConnections();
    if (failure && epoch === authEpoch && !authExpired && $("clients-dialog").open) feedback("clients-feedback", failure, true);
  }

  function updateConnectionFields() {
    const all = document.querySelector("input[name=client-scope]:checked").value === "all";
    $("client-all-warning").hidden=!all; $("client-chat-panel").hidden=all;
    const requiresRemote=["claude_connectors","composio"].includes($("client-choice").value);
    const modeSelect=$("client-mode");
    if(requiresRemote&&modeSelect.value!=="remote")modeSelect.value="remote";
    for(const option of modeSelect.options)option.disabled=requiresRemote&&option.value==="local"||!clientSnapshot||clientSnapshot[option.value+"_state"]!=="ready"||connectionRecovery(clientSnapshot,option.value)!=="ready";
    const mode=modeSelect.value;
    $("client-remote-notice").hidden=mode!=="remote";
    let help=requiresRemote?"This hosted client uses Remote. Its setup still needs a successful test with the selected client.":"The exact client connection still needs a successful test after setup.";
    if(mode==="remote"&&!clientSnapshot)help="Checking whether remote access is available…";
    else if(mode==="remote"&&clientSnapshot.remote_state==="not_configured")help="Remote access is not configured. Configure a secure remote endpoint for this app, then refresh connections.";
    else if(mode==="remote"&&(clientSnapshot.remote_state!=="ready"||connectionRecovery(clientSnapshot,"remote")!=="ready"))help="Remote access is unavailable. Check the app's remote configuration, then refresh connections.";
    $("client-choice-help").textContent=help;
    $("client-create").disabled=clientBusy || !clientSnapshot || connectionRecovery(clientSnapshot,mode)!=="ready" || clientSnapshot[mode+"_state"]!=="ready";
    updateClientGuideControls();
    updateSendPermissionControls();
    refreshSelects("client-choice", "client-mode");
  }

  function renderConnections(snapshot) {
    const focused=document.activeElement;
    let invalidatedPermissionFocusID="";
    clientSnapshot=snapshot;
    if (sendPermissionBinding && !currentSendPermissionConnection()) {
      if ($("client-send-editor").contains(focused)) invalidatedPermissionFocusID=sendPermissionBinding.id;
      clearSendPermissions();
      feedback("clients-feedback", "This connection's permissions changed or access ended. Reopen Permissions to review its current state.", true);
    }
    const stateNames={ready:"Available", not_configured:"Not configured", unavailable:"Unavailable"};
    $("clients-availability").textContent="Local: "+(stateNames[snapshot.local_state]||"Unavailable")+" · Remote: "+(stateNames[snapshot.remote_state]||"Unavailable");
    if (!status?.ReconciliationRequired && status?.Connection !== "reconciliation_required" && (status?.Codes || []).includes("app_state_sync_error")) $("clients-availability").textContent+=". WhatsApp state sync is incomplete. AI clients can read stored messages within their permissions, with coverage warnings; the archive may differ from your phone.";
    for(const mode of ["local","remote"])if(connectionRecovery(snapshot,mode)!=="ready")$("clients-availability").textContent+=". "+(mode==="local"?"Local":"Remote")+" recovery needs attention; access for that mode remains blocked.";
    updateConnectionFields();
    const key=JSON.stringify(snapshot);
    if(key===clientRenderKey) {
      if(invalidatedPermissionFocusID)restoreConnectionFocus(invalidatedPermissionFocusID,"permissions");
      return;
    }
    clientRenderKey=key;
    const hint=invalidatedPermissionFocusID?{id:invalidatedPermissionFocusID,action:"permissions"}:focused===document.body||focused===$("clients-dialog")?clientFocusHint:null;
    const focusID=focused?.dataset?.connectionId||hint?.id, focusAction=focused?.dataset?.action||hint?.action;
    clientFocusHint=null;
    const list=$("client-list"); list.replaceChildren();
    for (const item of snapshot.items||[]) {
      const card=document.createElement("article"); card.className="model-card"; card.dataset.connectionId=item.id;
      const heading=document.createElement("div"); heading.className="client-card-heading";
      const title=document.createElement("h3"); title.append(bdi(item.label)); title.tabIndex=-1; title.dataset.connectionId=item.id; title.dataset.action="heading";
      const permission=connectionSendPermission(item);
      const badge=document.createElement("span"); badge.className="client-permission-badge"; badge.dataset.mode=permission?.mode||"none";
      badge.textContent=permission?.mode==="all"?"Send to all permitted chats":permission?.mode==="selected"?"Send to "+permission.chats.length+" chat"+(permission.chats.length===1?"":"s"):"Read-only";
      heading.append(title,badge); card.append(heading);
      const states={ready:"Grant ready; client call not yet verified", revoked:"Access revoked", expired:"Access expired", recovery_pending:"Recovery needed; access blocked"};
      modelParagraph(card,(clientNames[item.client]||"AI client")+" · "+(item.mode==="local"?"Local":item.mode==="remote"?"Remote":"Mode unavailable"));
      modelParagraph(card,states[item.state]||states.recovery_pending,"model-state");
      modelParagraph(card,item.scope?.all_chats?"All archived chats, current and future":Number(item.scope?.chat_count||0).toLocaleString()+" selected chats");
      modelParagraph(card,"From "+(item.scope?.start_date?dateText(item.scope.start_date):"all kept dates")+" · Before "+(item.scope?.end_date?dateText(item.scope.end_date):"no additional end date")+" · Expires "+dateText(item.expires_at));
      const actions=document.createElement("div"); actions.className="model-actions";
      function action(kind,text) {
        const button=document.createElement("button"); button.type="button"; button.className="secondary"; button.textContent=text;
        button.dataset.connectionId=item.id; button.dataset.action=kind; button.disabled=clientBusy;
        button.addEventListener("click",()=>connectionAction(button,item,kind)); actions.append(button);
      }
      if (item.state==="ready" && clientNames[item.client] && item.mode && connectionRecovery(snapshot,item.mode)==="ready" && snapshot[item.mode+"_state"]==="ready") {
        action("guide","Show setup instructions");
        if(sendPermissionEditable(item))action("permissions","Permissions");
        if(!remoteMode && item.mode==="remote" && item.revision>0)action("reveal","Reveal remote token");
      }
      if (item.state!=="revoked" && item.revision>0) action("revoke","Revoke access");
      card.append(actions); list.append(card);
    }
    if (!(snapshot.items||[]).length) modelParagraph(list,"No AI clients have access. Create a connection when you are ready to choose its scope.");
    if (clientGuideID && !(snapshot.items||[]).some(item=>item.id===clientGuideID && item.revision===clientGuideBinding?.revision && item.client===clientGuideBinding?.client && item.mode===clientGuideBinding?.mode && item.state==="ready" && connectionRecovery(snapshot,item.mode)==="ready" && snapshot[item.mode+"_state"]==="ready")) clearClientGuide();
    if(remoteSecretID&&!(snapshot.items||[]).some(item=>item.id===remoteSecretID&&item.mode==="remote"&&item.state==="ready"&&item.revision===remoteSecretRevision&&connectionRecovery(snapshot,"remote")==="ready"&&snapshot.remote_state==="ready"))clearRemoteSecret();
    if(focusID&&focusAction)restoreConnectionFocus(focusID,focusAction);
  }

  async function refreshConnections() {
    if(authExpired||!$("clients-dialog").open||clientBusy)return;
    const version=++clientVersion;
    try {
      const snapshot=await api("/api/connections");
      if(version!==clientVersion||authExpired||!$("clients-dialog").open)return;
      renderConnections(snapshot);
    } catch(error) {
      if(version===clientVersion&&!authExpired&&$("clients-dialog").open) {
        clientSnapshot=null; clientRenderKey=""; $("client-list").replaceChildren(); clearClientGuide(); clearRemoteSecret(); clearSendPermissions(); updateConnectionFields();
        feedback("clients-feedback",error.message,true);
      }
    }
  }

  function renderClientGuide(guide,item) {
    clearClientGuide(); clientGuideID=item.id;
    clientGuideBinding={id:item.id,revision:item.revision,client:item.client,mode:item.mode};
    if(!remoteMode&&item.mode==="remote"&&guide.mode==="remote"&&guide.client===item.client&&guide.status!=="unsupported"&&typeof guide.endpoint==="string") {
      try {
        const endpoint=new URL(guide.endpoint);
        if(endpoint.protocol==="https:"&&!endpoint.username&&!endpoint.password&&!endpoint.search&&!endpoint.hash&&endpoint.pathname==="/mcp"&&guide.endpoint===endpoint.origin+"/mcp") {
          $("client-guide-url").value=guide.endpoint;
          $("client-guide-remote").hidden=false;
        }
      } catch { /* no URL or token actions for an invalid endpoint */ }
    }
    $("client-guide-status").textContent=guide.status==="unsupported"?"Unsupported combination":"Not yet verified with this client";
    $("client-guide-explanation").textContent=guide.explanation||"";
    for(const warning of guide.warnings||[])modelParagraph($("client-guide-warnings"),warning,"model-warning");
    $("client-guide-placement").textContent=guide.credential_placement||"";
    for(const [field,key,button] of [["client-guide-config","configuration","client-copy-config"],["client-guide-command","command","client-copy-command"]]) {
      $(field).value=guide[key]||""; $(field).parentElement.hidden=!guide[key]; $(button).hidden=!guide[key];
    }
    $("client-guide-test").textContent=guide.test_action||"";
    $("client-guide-revoke").textContent=guide.revoke_action||"";
    $("client-guide-version").textContent=(guide.observed_version||"Version not observed")+(guide.documentation_date?" · Documentation checked "+guide.documentation_date:"");
    $("client-guide").hidden=false;
    updateClientGuideControls();
  }

  async function connectionAction(button,item,kind) {
    if(kind==="permissions") { openSendPermissions(item); return; }
    if(kind==="reveal") { await revealRemoteCredential(button,item); return; }
    if(clientBusy||authExpired)return;
    const epoch=authEpoch;
    clientBusy=true; clientVersion++; button.disabled=true; updateConnectionFields();
    clientFocusHint={id:item.id,action:kind};
    const version=clientVersion;
    feedback("clients-feedback",kind==="revoke"?"Revoking client access…":"Preparing inert setup instructions…");
    if(kind==="revoke") { clearClientGuide(); clearRemoteSecret(); clearSendPermissions(); }
    try {
      if(kind==="revoke")await api("/api/connections/revoke","POST",{id:item.id,revision:item.revision});
      else {
        const guide=await api("/api/connections/guide","POST",{id:item.id,client:item.client,mode:item.mode});
        if(version!==clientVersion||authExpired||!$("clients-dialog").open)return;
        renderClientGuide(guide,item);
      }
      assertOwner(epoch);
      if(version===clientVersion&&$("clients-dialog").open)feedback("clients-feedback",kind==="revoke"?"Client access revoked. Remove its registration from the AI client as well.":"Instructions are ready to review and copy. They do not run automatically.");
    } catch(error) { if(version===clientVersion&&!authExpired&&$("clients-dialog").open)feedback("clients-feedback",error.message,true); }
    finally { if(epoch===authEpoch){clientBusy=false; button.disabled=false; updateConnectionFields();} }
    if(epoch!==authEpoch||authExpired)return;
    clientRenderKey=""; await refreshConnections();
  }

  async function createConnection(event) {
    event.preventDefault(); if(clientBusy||authExpired||$("client-create").disabled)return;
    const epoch=authEpoch;
    clientBusy=true; clientVersion++; updateConnectionFields(); clearClientGuide(); clearRemoteSecret(); clearSendPermissions();
    const version=clientVersion;
    feedback("clients-feedback","Creating the private read-only connection…");
    try {
      const all=document.querySelector("input[name=client-scope]:checked").value==="all";
      const selected=Array.from($("client-chat-choices").querySelectorAll("input:checked"),input=>input.value);
      if(!all&&(selected.length<1||selected.length>256))throw new Error("Choose 1 to 256 chats for this client.");
      const expiry=new Date($("client-expiry").value); if(Number.isNaN(expiry.getTime())||expiry<=new Date())throw new Error("Choose a future expiry for client access.");
      const input={client:$("client-choice").value,label:$("client-label").value.trim(),mode:$("client-mode").value,all_chats:all,chats:all?[]:selected,expires_at:expiry.toISOString()};
      if($("client-from").value)input.start_date=dayStart($("client-from").value).toISOString();
      if($("client-through").value) { const end=dayStart($("client-through").value); end.setDate(end.getDate()+1); input.end_date=end.toISOString(); }
      if(input.start_date&&input.end_date&&input.start_date>=input.end_date)throw new Error("The through date must be on or after the start date.");
      const item=await api("/api/connections/create","POST",input);
      assertOwner(epoch);
      if(version!==clientVersion||!$("clients-dialog").open)return;
      feedback("clients-feedback",item.mode==="remote"?"Remote connection created. Choose Show setup instructions to copy its MCP server URL and reveal its private token.":"Read-only connection created. Choose Show setup instructions to configure and test the client.");
      $("client-create-panel").open=false;
      $("client-form").reset(); renderConnectionChats();
      clientFocusHint={id:item.id,action:"guide"};
      clientRenderKey="";
    } catch(error) { if(version===clientVersion&&!authExpired&&$("clients-dialog").open)feedback("clients-feedback",error.message,true); }
    finally { if(epoch===authEpoch){clientBusy=false; updateConnectionFields();} }
    if(epoch!==authEpoch||authExpired)return;
    await refreshConnections();
  }

  async function revealRemoteCredential(button,item) {
    if (remoteMode) return;
    if(clientBusy||authExpired||item.mode!=="remote"||item.state!=="ready"||connectionRecovery(clientSnapshot,"remote")!=="ready"||clientSnapshot?.remote_state!=="ready")return;
    const epoch=authEpoch;
    clearRemoteSecret();
    remoteSecretID=item.id; remoteSecretFocusID=item.id; remoteSecretRevision=item.revision;
    const version=remoteSecretVersion;
    clientBusy=true; clientVersion++; button.disabled=true; updateConnectionFields();
    $("client-secret-dialog").showModal();
    feedback("client-secret-feedback","Checking this connection before revealing its token…");
    let secret=null;
    try {
      secret=await api("/api/connections/reveal","POST",{id:item.id,revision:item.revision});
      if(version!==remoteSecretVersion||authExpired||!$("client-secret-dialog").open||!$("clients-dialog").open)return;
      const expiry=new Date(secret.expires_at).getTime();
      if(secret.id!==item.id||secret.revision!==item.revision||typeof secret.token!=="string"||!/^[A-Za-z0-9_-]{43}$/u.test(secret.token)||!Number.isFinite(expiry)||expiry<=Date.now())throw new Error(messages.client_connections_unavailable);
      remoteSecretExpiry=expiry;
      $("client-secret-token").value=secret.token;
      $("client-secret-url").textContent=secret.public_url;
      $("client-secret-expiry").textContent="Access expires "+dateText(secret.expires_at)+".";
      $("client-secret-copy").disabled=false;
      feedback("client-secret-feedback","Copy this token once, then close this private interaction.");
      $("client-secret-token").focus();
    } catch(error) {
      if(version===remoteSecretVersion&&!authExpired&&$("client-secret-dialog").open) { clearRemoteSecret(false); feedback("client-secret-feedback",error.message,true); }
    } finally {
      if(secret)secret.token="";
      if(epoch===authEpoch){clientBusy=false; button.disabled=false; updateConnectionFields();}
    }
  }

  function updatePolicyFields() {
    const custom = document.querySelector("input[name=date-mode]:checked").value === "custom";
    $("custom-date-label").hidden = !custom; $("import-start").required = custom;
    $("selective-panel").hidden = document.querySelector("input[name=chat-mode]:checked").value !== "selective";
  }

  async function openSettings(expectedPairingVersion = null) {
    const epoch = authEpoch;
    try {
      const nextSetup = await api("/api/setup");
      assertOwner(epoch);
      setup = nextSetup;
      if (typeof expectedPairingVersion === "number" && (expectedPairingVersion !== pairingVersion || !$("connection-dialog").open)) return false;
      if (typeof expectedPairingVersion === "number" && (status?.ReconciliationRequired || status?.Connection === "reconciliation_required")) return false;
      const policy = setup.Policy || setup.policy;
      // Setup itself uses snake-case fields; archive policy is the frozen archive DTO.
      setup.Policy = policy;
      const mode = policy.DateMode === "all_available" ? "all" : setup.configured ? "custom" : "30";
      document.querySelector("input[name=date-mode][value='" + mode + "']").checked = true;
      if (policy.ImportStart && !policy.ImportStart.startsWith("0001-")) {
        const d = new Date(policy.ImportStart);
        $("import-start").value = d.getFullYear() + "-" + String(d.getMonth()+1).padStart(2,"0") + "-" + String(d.getDate()).padStart(2,"0");
      }
      const discoveryFirst = !setup.configured && !setup.demo;
      document.querySelector("input[name=chat-mode][value='" + (discoveryFirst || policy.ChatMode === "selective" ? "selective" : "all") + "']").checked = true;
      $("settings-intro").textContent = discoveryFirst ? "Link WhatsApp to discover your chats, then choose which messages to keep. No chat message bodies are saved until you select chats." : "Choose the import range and chats to keep. These choices also apply after linking WhatsApp.";
      $("settings-title").textContent = discoveryFirst ? "Set up your archive" : "Chats and dates";
      $("chat-policy-fieldset").hidden = discoveryFirst;
      $("save-policy").textContent = discoveryFirst ? "Link WhatsApp to discover chats" : "Save choices";
      $("retention-days").value = Math.round((policy.RollingRetention || 0) / 86400000000000);
      $("settings-close").hidden = !setup.configured;
      $("policy-change-notice").hidden = !setup.configured;
      updatePolicyFields(); feedback("policy-feedback", "");
      $("chat-choices").replaceChildren();
      if (!$("settings-dialog").open) $("settings-dialog").showModal();
      await loadChats();
      assertOwner(epoch);
      if (typeof expectedPairingVersion === "number" && (status?.ReconciliationRequired || status?.Connection === "reconciliation_required")) { $("settings-dialog").close(); return false; }
      if (typeof expectedPairingVersion === "number" && (expectedPairingVersion !== pairingVersion || !$("connection-dialog").open || authExpired)) return false;
      return true;
    } catch (error) {
      if (epoch !== authEpoch || authExpired) return false;
      if (typeof expectedPairingVersion === "number" && (status?.ReconciliationRequired || status?.Connection === "reconciliation_required")) $("settings-dialog").close();
      feedback("policy-feedback", error.message, true); return false;
    }
  }

  async function savePolicy(event) {
    event.preventDefault();
    if (authExpired) return;
    const epoch = authEpoch;
    const button = $("save-policy"); button.disabled = true;
    try {
      const mode = document.querySelector("input[name=date-mode]:checked").value;
      const chatMode = document.querySelector("input[name=chat-mode]:checked").value;
      const request = {expected_generation: setup.Policy.Generation || 0, date_mode: mode === "all" ? "all_available" : "since", chat_mode: chatMode, included_chats: [], rolling_retention_days: Number($("retention-days").value)};
      if (mode !== "all") {
        const start = mode === "custom" ? dayStart($("import-start").value) : new Date();
        if (mode !== "custom") { start.setHours(0,0,0,0); start.setDate(start.getDate() - Number(mode)); }
        request.import_start = start.toISOString();
      }
      if (chatMode === "selective") {
        const included = new Set(setup.Policy.IncludedChats || []);
        for (const node of $("chat-choices").querySelectorAll("input")) {
          if (node.checked) included.add(node.value); else included.delete(node.value);
        }
        request.included_chats = Array.from(included);
      }
      const policy = await api("/api/policy", "PUT", request);
      assertOwner(epoch);
      const initial = !setup.configured;
      setup = {...setup, configured:true, policy, Policy:policy};
      $("settings-dialog").close();
      // Policy changes invalidate visible results rather than leaving stale bodies on screen.
      clearResults();
      invalidateStats(true);
      await refreshStatus();
      assertOwner(epoch);
      await loadChats();
      assertOwner(epoch);
      if (initial) { openConnection(); if (!setup.demo) await startPairing(); }
      else feedback("search-feedback", "Archive choices saved. Run a search to see messages in this range.");
    } catch (error) { if(epoch===authEpoch&&!authExpired)feedback("policy-feedback", error.message, true); }
    finally { if(epoch===authEpoch)button.disabled = false; }
  }

  function clearResults() {
    searchVersion++; contextVersion++;
    searching = false; $("search-submit").disabled = false; $("search-more").disabled = false;
    nextCursor = ""; selectedMessage = ""; visibleMessages = [];
    $("search-results").replaceChildren(); $("conversation-messages").replaceChildren();
    $("results-empty").hidden = false; $("search-more").hidden = true; $("context-copy").hidden = true;
    $("result-count").textContent = "";
    $("context-title").textContent = "Conversation";
    feedback("context-feedback", "Choose a result to read the messages around it.");
  }

  async function runSearch(more = false) {
    if (searching || authExpired) return;
    const epoch = authEpoch;
    searching = true;
    if (!more) clearResults();
    const version = ++searchVersion;
    $("search-submit").disabled = true; $("search-more").disabled = true;
    feedback("search-feedback", "Searching saved messages…");
    try {
      if (!more) {
        activeQuery = $("search-query").value;
        searchRequest = {query:activeQuery, limit:30, chats:$("search-chat").value ? [$("search-chat").value] : []};
        if ($("search-from").value) searchRequest.from = dayStart($("search-from").value).toISOString();
        if ($("search-until").value) { const until = dayStart($("search-until").value); until.setDate(until.getDate()+1); searchRequest.until = until.toISOString(); }
      }
      const result = await api("/api/search", "POST", {...searchRequest, cursor:more ? nextCursor : ""});
      if (version !== searchVersion) return;
      for (const hit of result.Hits || []) addResult(hit);
      nextCursor = result.NextCursor || "";
      const count = $("search-results").children.length;
      $("result-count").textContent = count + " shown";
      $("results-empty").hidden = count > 0;
      $("search-more").hidden = !nextCursor;
      feedback("search-feedback", count ? count + " saved messages shown. Choose one to read its conversation." : "No saved messages match. Try fewer words, another date, or a different chat. History that was not delivered cannot appear here.");
    } catch (error) { if (version === searchVersion) feedback("search-feedback", error.message, true); }
    finally { if(epoch===authEpoch){searching = false; $("search-submit").disabled = false; $("search-more").disabled = false;} }
  }

  function addResult(hit) {
    const item = document.createElement("li");
    const button = document.createElement("button"); button.type="button"; button.className="result-button"; button.setAttribute("aria-pressed", "false"); button.dataset.messageId=hit.MessageID;
    const meta = document.createElement("div"); meta.className="result-meta";
    meta.append(bdi(hit.ChatName || "Unnamed chat", "result-chat"), bdi(dateText(hit.SentAt)));
    const sender = document.createElement("div"); sender.className="result-sender"; sender.append(bdi(hit.SenderName || "Unknown sender"));
    const snippet = document.createElement("div"); snippet.className="message-text result-snippet";
    renderText(snippet, hit.Snippet, activeQuery);
    button.append(meta, sender);
    const extracted = (hit.Derived || []).find(excerpt => excerpt.DocumentID === hit.DocumentID);
    if (extracted) { const label=document.createElement("div"); label.className="derived-label"; label.append(bdi(extractionLabel(extracted))); button.append(label); }
    button.append(snippet);
    if (hit.Kind && hit.Kind !== "text") { const kind = document.createElement("div"); kind.className="result-kind"; kind.textContent=hit.Kind === "voice" ? "Voice note" : hit.Kind === "image" ? "Image message" : "Media message"; button.append(kind); }
    button.addEventListener("click", () => showContext(hit, button)); item.append(button); $("search-results").append(item);
  }

  async function showContext(hit, button) {
    const version = ++contextVersion;
    selectedMessage = hit.MessageID;
    for (const node of $("search-results").querySelectorAll("button")) node.setAttribute("aria-pressed", String(node === button));
    $("conversation-messages").replaceChildren(); visibleMessages=[]; $("context-copy").hidden = true;
    $("context-title").replaceChildren(bdi(hit.ChatName || "Conversation"));
    feedback("context-feedback", "Loading permitted surrounding messages…");
    try {
      const result = await api("/api/context", "POST", {message_id:hit.MessageID, before:10, after:10});
      if (version !== contextVersion) return;
      visibleMessages = result.Messages || [];
      for (const message of visibleMessages) addMessage(message);
      feedback("context-feedback", visibleMessages.length ? "Only messages allowed by your current archive settings are shown." : "This message is no longer available. Run your search again.");
      $("context-copy").hidden = !visibleMessages.length;
      const selected = $("conversation-messages").querySelector(".selected");
      if (selected) selected.scrollIntoView({block:"nearest", behavior:"auto"});
      if (matchMedia("(max-width:850px)").matches) $("context-title").closest("section").focus({preventScroll:false});
    } catch (error) { if (version === contextVersion) feedback("context-feedback", error.message, true); }
  }

  function addMessage(message) {
    const article = document.createElement("article"); article.className="conversation-message"; article.classList.toggle("selected", message.ID === selectedMessage);
    const meta = document.createElement("div"); meta.className="message-meta"; meta.append(bdi(message.SenderName || "Unknown sender"), bdi(dateText(message.SentAt) + (message.Edited ? " · Edited" : "")));
    article.append(meta);
    if (message.ReplyQuote) { const quote=document.createElement("div"); quote.className="reply-quote message-text"; renderText(quote,message.ReplyQuote); article.append(quote); }
    const body = document.createElement("div"); body.className="message-text"; renderText(body,message.Body,activeQuery); article.append(body);
    if (message.Caption) { const caption=document.createElement("div"); caption.className="message-text message-caption"; renderText(caption,message.Caption,activeQuery); article.append(caption); }
    appendDerived(article, message.Derived || []);
    if (message.Kind !== "text") { const note=document.createElement("p"); note.className="media-note"; const name=message.Kind === "voice" ? "Voice note" : message.Kind === "image" ? "Image" : "Media"; note.textContent=name + ": " + (message.MediaAvailability === "retained" ? "original available" : "original unavailable") + ((message.Derived || []).length ? "; extracted text is shown separately" : "; no extracted text available"); article.append(note); }
    $("conversation-messages").append(article);
  }

  function extractionLabel(excerpt) {
    const provenance = excerpt.Provenance || {};
    const names = {ocr:"Text read from image (OCR)", caption:"Image description (generated)", transcript:"Voice transcript"};
    let label = names[provenance.Kind] || "Extracted text";
    if (provenance.Kind === "transcript" && ["slice", "word"].includes(provenance.OffsetKind) && Number.isSafeInteger(provenance.StartMS) && Number.isSafeInteger(provenance.EndMS) && provenance.StartMS >= 0 && provenance.EndMS > provenance.StartMS) {
      const time = ms => Math.floor(ms / 60000) + ":" + String(Math.floor(ms / 1000) % 60).padStart(2,"0");
      label += " · " + time(provenance.StartMS) + "–" + time(provenance.EndMS) + (provenance.OffsetKind === "slice" ? " (clip timing)" : " (word timing)");
    }
    return label;
  }

  function appendDerived(parent, excerpts) {
    for (const excerpt of excerpts) {
      const section = document.createElement("div"); section.className = "derived-excerpt";
      const label = document.createElement("div"); label.className = "derived-label"; label.append(bdi(extractionLabel(excerpt)));
      const text = document.createElement("div"); text.className = "message-text"; renderText(text,excerpt.Text,activeQuery);
      section.append(label,text); parent.append(section);
    }
  }

  function connectionPresentation(state, snapshot) {
    const snapshotLinked = ["connected","discovery","importing"].includes(snapshot?.Connection);
    const current = snapshotLinked && state?.state === "pairing" ? snapshot.Connection : state?.state || snapshot?.Connection || "disconnected";
    const unsafe = Boolean(snapshot?.ReconciliationRequired || snapshot?.Connection === "reconciliation_required" || current === "reconciliation_required");
    if (unsafe) return {state:"reconciliation_required",title:"Import stopped",description:"WhatsApp import cannot continue in this archive.",feedback:importFailureText(snapshot?.Codes || []),error:true,canPair:false,canDisconnect:false,canChoose:false,canCheck:true,showProgress:false};
    const hasSession = state?.has_session === true;
    const diagnostic = state?.connection_diagnostic || snapshot?.ConnectionDiagnostic || {};
    const actionText = hasSession ? "Reconnect WhatsApp" : current === "pairing" ? "Refresh linking code" : "Show linking code";
    if (["connected","discovery","importing"].includes(current)) return {state:current,title:"WhatsApp is linked",description:"Your phone is connected to this computer.",feedback:(snapshot?.Codes || []).includes("app_state_sync_error") ? stateSyncGapText(snapshot) : "",error:false,canPair:false,canDisconnect:true,canChoose:true,canCheck:false,showProgress:true};
    if (current === "paused") return {state:current,title:"Import is paused",description:"Your archive choices and saved messages are kept.",feedback:"Resume import from your archive to receive new messages.",error:false,canPair:false,canDisconnect:true,canChoose:true,canCheck:false,showProgress:false};
    if (diagnostic.ErrorClass === "device_deleted" || diagnostic.Phase === "session_deleted" || diagnostic.Event === "logged_out") return {state:current,title:"WhatsApp device session ended",description:diagnostic.Event === "logged_out" && diagnostic.OnConnect === true ? "WhatsApp rejected this linked device during sign-in." : "This saved linked device can no longer connect to WhatsApp.",feedback:"Your saved archive is kept. Restart the app to create a fresh linked-device session, then scan its new linking code.",error:true,ended:true,canPair:false,canDisconnect:true,canChoose:false,canCheck:true,showProgress:false};
    const failedEvents = ["disconnected","logged_out","manual_login_reconnect","connect_failure","client_outdated","temporary_ban","stream_error","stream_replaced"];
    const failed = Boolean(diagnostic.ErrorClass && diagnostic.ErrorClass !== "none" || failedEvents.includes(diagnostic.Event));
    const connecting = ["start","optional_join","prior_stop","policy","observation","qr_channel","connect","post_pair_stop","post_pair_start","awaiting_authentication"].includes(diagnostic.Phase);
    if (!failed && diagnostic.ErrorClass === "none" && connecting && (hasSession || current !== "pairing")) {
      const signingIn = diagnostic.Phase === "awaiting_authentication";
      return {state:signingIn ? "logging_in" : "connecting",title:signingIn ? "Signing in to WhatsApp" : "Connecting to WhatsApp",description:signingIn ? "Waiting for WhatsApp to finish device sign-in. Chats have not arrived from this connection yet." : hasSession ? "Opening the connection with your saved linked-device session. A new QR code is not needed." : "Opening the connection to WhatsApp.",feedback:"",error:false,busy:true,canPair:false,canDisconnect:true,canChoose:false,canCheck:false,showProgress:true};
    }
    if (hasSession && failed) return {state:current,title:"WhatsApp connection stopped",description:"Your linked-device session is saved on this computer.",feedback:connectionFailureText(diagnostic),error:true,actionText,canPair:!["logged_out","client_outdated","temporary_ban"].includes(diagnostic.Event),canDisconnect:true,canChoose:false,canCheck:true,showProgress:false};
    if (hasSession) return {state:current,title:"Reconnect WhatsApp",description:"Your linked-device session is saved. Reconnect to finish signing in and receive your chats.",feedback:"Reconnect uses this saved session. You do not need to scan a new QR code.",error:false,actionText,canPair:true,canDisconnect:true,canChoose:false,canCheck:false,showProgress:false};
    if (current === "pairing") return {state:current,title:"Link WhatsApp",description:"Scan the code with WhatsApp on your phone.",feedback:state?.qr_available ? "" : "Connecting to WhatsApp. The linking code will appear here…",error:false,canPair:true,canDisconnect:true,canChoose:false,canCheck:false,showProgress:false};
    return {state:current,title:"Link WhatsApp",description:"Your archive choices are saved. Link your phone to start importing.",feedback:null,error:false,canPair:true,canDisconnect:false,canChoose:false,canCheck:false,showProgress:false};
  }

  function connectionFailureText(diagnostic) {
    const events = {
      logged_out:"WhatsApp logged out this linked device. The saved archive is kept; a fresh linked-device session is needed to import again.",
      manual_login_reconnect:"WhatsApp requested another connection to finish device sign-in. Reconnect WhatsApp using the saved session.",
      client_outdated:"WhatsApp rejected this client's version. Update the app before reconnecting; your saved files are kept.",
      temporary_ban:"WhatsApp temporarily blocked this linked device. Wait for the restriction to end before reconnecting.",
      stream_replaced:"Another connection replaced this WhatsApp session. Close the duplicate app connection, then reconnect here.",
    };
    if (Object.hasOwn(events, diagnostic.Event)) return events[diagnostic.Event];
    const errors = {
      dial:"The app could not reach WhatsApp. Check this computer's internet connection, then reconnect.",
      network_timeout:"WhatsApp did not respond in time. Check the internet connection, then reconnect.",
      deadline:"WhatsApp sign-in did not finish in time. Reconnect using the saved session.",
      canceled:"The connection attempt was stopped. Reconnect when you are ready to continue.",
      http_status:"WhatsApp refused the connection. Your saved session is kept; try reconnecting.",
      source_unavailable:"WhatsApp did not complete device sign-in. Reconnect using the saved session.",
      already_connected:"A connection is already active. Check the current status before starting another connection.",
    };
    return Object.hasOwn(errors, diagnostic.ErrorClass) ? errors[diagnostic.ErrorClass] : "WhatsApp stopped the connection before chats arrived. Reconnect using the saved session.";
  }

  function renderConnectionPresentation(state) {
    const view = connectionPresentation(state, status);
    const dialog = $("connection-dialog");
    dialog.dataset.state = view.state;
    $("connection-title").textContent = setup?.demo ? "Synthetic archive" : view.title;
    $("pairing-status").textContent = setup?.demo ? "Synthetic demo: no WhatsApp account is linked." : view.description;
    $("pairing-start").hidden = !setup?.demo && !view.canPair;
    $("pairing-start").textContent = setup?.demo ? "Load synthetic messages" : view.actionText || (view.state === "pairing" ? "Refresh linking code" : "Show linking code");
    $("disconnect").hidden = Boolean(setup?.demo) || !view.canDisconnect;
    $("connection-settings").hidden = Boolean(setup?.demo) || !view.canChoose;
    $("connection-check").hidden = Boolean(setup?.demo) || !view.canCheck;
    $("connection-check").disabled = false;
    $("connection-progress").hidden = Boolean(setup?.demo) || !view.showProgress;
    const policy = setup?.Policy || setup?.policy;
    const selected = policy?.IncludedChats?.length || 0;
    const choosing = policy?.ChatMode === "selective" && !selected;
    $("connection-settings").disabled = choosing && !chats.length;
    $("connection-settings").textContent = choosing ? "Choose chats" : "Review chats and dates";
    $("connection-progress-text").textContent = view.busy ? view.description : choosing ? chats.length ? chats.length.toLocaleString() + " chats discovered. Choose which conversations to keep." : "Receiving your chat list from WhatsApp. Choose chats will become available as they arrive." : Number(status?.Messages || 0) ? Number(status.Messages).toLocaleString() + " messages saved. New messages arrive while connected." : "Waiting for WhatsApp to send messages from your selected chats.";
    $("connection-help").hidden = !setup?.demo && (!view.canPair || state?.has_session === true);
    $("connection-help").textContent = setup?.demo ? "Explore sample Hebrew and English conversations. This demo never links a WhatsApp account." : "On your phone, open WhatsApp → Linked devices → Link a device. Your linking code stays private to this owner window.";
    $("connection-scope").hidden = Boolean(setup?.demo);
    $("connection-scope").textContent = policy?.ChatMode === "all" ? "Archive choices: all chats in your selected date range." : selected ? "Archive choices: " + selected.toLocaleString() + " selected chats in your chosen date range." : "No chats selected yet. Choose after linking; only chat metadata is kept until then.";
    if (!setup?.demo && view.feedback !== null) feedback("pairing-feedback", view.feedback, view.error);
    else if (!setup?.demo && !$("pairing-feedback").classList.contains("error") && /waiting for a linking code|connecting to whatsapp/iu.test($("pairing-feedback").textContent)) feedback("pairing-feedback", "WhatsApp is disconnected. Show a new linking code to try again.");
    if (view.state !== "pairing" || !view.canPair || state?.has_session === true) {
      qrVersion++;
      $("pairing-code").hidden = true;
      $("pairing-qr").removeAttribute("src");
      if (qrURL) { URL.revokeObjectURL(qrURL); qrURL = ""; }
    }
    return view;
  }

  function clearQR() { pairingVersion++; qrVersion++; $("pairing-code").hidden=true; $("pairing-qr").removeAttribute("src"); if(qrURL){URL.revokeObjectURL(qrURL);qrURL="";} }
  function openConnection() {
    if (authExpired) return;
    clearQR(); feedback("pairing-feedback", "");
    renderConnectionPresentation({...pairingSnapshot,state:status?.Connection || "disconnected"});
    $("connection-dialog").showModal();
    refreshPairing();
  }

  async function refreshPairing() {
    if (authExpired) return;
    const epoch = authEpoch;
    const opened = $("connection-dialog").open;
    const version = ++pairingVersion;
    let qrRequest = null;
    try {
      const state = await api("/api/pairing");
      if (version !== pairingVersion || authExpired || opened && !$("connection-dialog").open) return;
      pairingSnapshot = state;
      renderImportState();
      if (!$("connection-dialog").open) return;
      const view = renderConnectionPresentation(state);
      if (state.qr_available && state.state === "pairing" && state.has_session !== true && view.canPair && !setup?.demo) {
        qrRequest = ++qrVersion;
        const response = await fetch("/api/pairing/qr", {headers:{"X-CSRF-Token":csrf},credentials:"same-origin",cache:"no-store"});
        if (epoch !== authEpoch || authExpired) return;
        if (response.status === 401 || response.status === 403) { expireAuth(); return; }
        if (!response.ok) throw new Error("The linking code is unavailable. Show a new code.");
        const blob = await response.blob();
        if (epoch !== authEpoch || authExpired || version !== pairingVersion || qrRequest !== qrVersion || pairingSnapshot?.has_session === true || !$("connection-dialog").open || connectionPresentation(state, status).state !== "pairing" || !connectionPresentation(state, status).canPair) return;
        if(qrURL)URL.revokeObjectURL(qrURL);
        qrURL = URL.createObjectURL(blob);
        $("pairing-qr").src = qrURL;
        $("pairing-code").hidden=false;
        $("pairing-expiry").textContent = state.expires_at && !state.expires_at.startsWith("0001-") ? "Expires " + dateText(state.expires_at) + ". Show a new code if it expires." : "Link promptly. Show a new code if it expires.";
      } else { $("pairing-code").hidden=true; $("pairing-qr").removeAttribute("src"); if(qrURL){URL.revokeObjectURL(qrURL);qrURL="";} }
      if (state.code && view.state !== "reconciliation_required" && !state.has_session) feedback("pairing-feedback", messages[state.code] || "The linking attempt stopped. Show a new linking code to try again.", true);
      if (state.state === "connected" && view.state !== "reconciliation_required") {
        if(!setup?.demo)$("pairing-start").hidden=true;
        await loadChats();
        assertOwner(epoch);
        await refreshStatus();
        if (version !== pairingVersion || !$("connection-dialog").open || authExpired) return;
        if (connectionPresentation(state, status).state === "reconciliation_required") return;
        const policy = setup?.Policy || setup?.policy;
        if (!authExpired && !setup?.demo && chats.length && policy?.ChatMode === "selective" && !(policy.IncludedChats || []).length) {
          if (await openSettings(version)) {
            $("connection-dialog").close();
            if (!authExpired) feedback("policy-feedback", "WhatsApp linked. Select the chats whose messages you want to keep. Available history depends on what WhatsApp delivers.");
          } else if (version === pairingVersion && $("connection-dialog").open && !authExpired) {
            if (status?.ReconciliationRequired || status?.Connection === "reconciliation_required") renderConnectionPresentation({state:"reconciliation_required"});
            else feedback("pairing-feedback", "Could not open archive settings. Use Chats and dates to try again.", true);
          }
        }
      }
    } catch (error) {
      if (version !== pairingVersion || authExpired || !$("connection-dialog").open) return;
      if (status?.ReconciliationRequired || status?.Connection === "reconciliation_required") { renderConnectionPresentation({state:"reconciliation_required"}); return; }
      if (qrRequest !== null && qrRequest !== qrVersion) return;
      feedback("pairing-feedback", error.message, true);
    }
  }

  async function command(button, path, target, success) {
    if (authExpired) return false;
    const epoch = authEpoch;
    button.disabled=true;
    try { await api(path,"POST",{}); assertOwner(epoch); feedback(target,success); await refreshStatus(); return epoch===authEpoch&&!authExpired; }
    catch(error) { if(epoch===authEpoch&&!authExpired)feedback(target,error.message,true); return false; }
    finally { if(epoch===authEpoch)button.disabled=false; }
  }

  async function startPairing() {
    if (authExpired) return;
    const epoch = authEpoch;
    if (!setup?.demo && (status?.ReconciliationRequired || status?.Connection === "reconciliation_required")) {
      renderConnectionPresentation({state:"reconciliation_required"});
      return;
    }
    clearQR();
    const success = await command($("pairing-start"),"/api/pairing/start","pairing-feedback",setup?.demo?"Synthetic messages loaded. Close this window to search the sample archive.":pairingSnapshot?.has_session === true ? "Reconnecting with your saved WhatsApp session…" : "Connecting to WhatsApp. The linking code will appear here…");
    if(epoch!==authEpoch||authExpired)return;
    if (success || pairingSnapshot?.has_session === true) await refreshPairing();
  }

  $("nav-stats").addEventListener("click", () => selectWorkspaceView("stats", true));
  $("nav-archive").addEventListener("click", () => selectWorkspaceView("archive", true));
  $("stats-filters").addEventListener("submit", event => event.preventDefault());
  $("stats-filters").addEventListener("change", changeStatsFilters);
  $("stats-refresh").addEventListener("click", refreshStats);
  for (const dimension of ["people", "chats"]) $("stats-breakdown-" + dimension).addEventListener("click", () => changeStatsBreakdown({dimension}));
  $("stats-breakdown-sort").addEventListener("change", () => changeStatsBreakdown({sort:$("stats-breakdown-sort").value}));
  $("stats-breakdown-size").addEventListener("change", () => changeStatsBreakdown({pageSize:Number($("stats-breakdown-size").value)}));
  $("stats-page-prev").addEventListener("click", () => { if (!$("stats-page-prev").disabled) changeStatsBreakdown({page:statsPage - 1}); });
  $("stats-page-next").addEventListener("click", () => { if (!$("stats-page-next").disabled) changeStatsBreakdown({page:statsPage + 1}); });
  for (const mode of ["bars", "line"]) $("stats-chart-" + mode).addEventListener("click", () => setStatsChartMode(mode));
  $("stats-models-open").addEventListener("click", () => $("models-open").click());
  $("stats-connection-action").addEventListener("click", async () => {
    if (authExpired) return;
    if (!setup?.configured) await openSettings();
    else if (status?.Paused && !statsUnavailable()) await command($("stats-connection-action"), "/api/resume", "stats-feedback", "Import resumed.");
    else $("connection-open").click();
  });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) invalidateStats();
    else if (!authExpired && activeView === "stats") {
      refreshStatus();
      refreshStats();
    }
  });
  let statsResizeTimer = null;
  window.addEventListener("resize", () => {
    if (statsResizeTimer !== null) window.clearTimeout(statsResizeTimer);
    statsResizeTimer = window.setTimeout(() => {
      statsResizeTimer = null;
      if (!authExpired && activeView === "stats" && statsSnapshot && !statsUnavailable()) renderStatsChart(statsSnapshot);
    }, 100);
  });
  $("policy-form").addEventListener("change",updatePolicyFields);
  $("policy-form").addEventListener("submit",savePolicy);
  $("settings-open").addEventListener("click",openSettings);
  $("models-open").addEventListener("click",()=>{if(!authExpired){feedback("models-feedback","Checking model state…");updateVoiceRetryControls();$("models-dialog").showModal();refreshModels();refreshLocalAI();refreshBudget();}});
  $("models-close").addEventListener("click",()=>$("models-dialog").close());
  $("models-dialog").addEventListener("close",()=>{modelVersion++;localAIVersion++;budgetVersion++;});
  $("models-refresh").addEventListener("click",()=>{refreshModels();refreshLocalAI();refreshBudget();});
  $("local-ai-form").addEventListener("submit",saveLocalAI);
  $("local-ai-mode").addEventListener("change",()=>{editLocalAI();for(const id of ["local-ai-encoder","local-ai-ocr","local-ai-speech"])$(id).value="";$("local-ai-caption").checked=false;if(localAISnapshot)renderLocalAI(localAISnapshot);});
  for(const id of ["local-ai-encoder","local-ai-ocr","local-ai-caption","local-ai-speech"])$(id).addEventListener("change",editLocalAI);
  $("original-budget-form").addEventListener("submit",saveBudget);
  $("voice-retry-button").addEventListener("click",retryVoiceDownloads);
  $("original-budget-size").addEventListener("input",()=>{editBudget();$("original-budget-preset").value="custom";refreshSelects("original-budget-preset");});
  $("original-budget-unit").addEventListener("change",()=>{editBudget();$("original-budget-preset").value="custom";updateBudgetUnits();});
  $("original-budget-preset").addEventListener("change",()=>{editBudget();if($("original-budget-preset").value!=="custom"){$("original-budget-unit").value="mib";$("original-budget-size").value=$("original-budget-preset").value;updateBudgetUnits();}});
  $("clients-open").addEventListener("click",()=>{if(!authExpired){clearClientGuide();clearSendPermissions();feedback("clients-feedback","");$("clients-dialog").showModal();refreshConnections();renderConnectionChats();}});
  $("clients-close").addEventListener("click",()=>$("clients-dialog").close());
  $("clients-dialog").addEventListener("close",()=>{clientVersion++;clientFocusHint=null;clearClientGuide();clearRemoteSecret();clearSendPermissions();});
  $("client-send-form").addEventListener("submit",saveSendPermissions);
  $("client-send-cancel").addEventListener("click",()=>{if(!clientBusy)clearSendPermissions(true);});
  $("client-send-search").addEventListener("input",renderSendPermissionChats);
  $("client-send-clear").addEventListener("click",()=>{if(clientBusy||!currentSendPermissionConnection())return;sendPermissionSelection.clear();renderSendPermissionChats();});
  for(const input of document.querySelectorAll("input[name=client-send-mode]")) input.addEventListener("change",()=>{if(clientBusy||!currentSendPermissionConnection())return;sendPermissionMode=input.value;feedback("client-send-feedback","");updateSendPermissionControls();});
  $("clients-refresh").addEventListener("click",refreshConnections);
  $("client-form").addEventListener("change",updateConnectionFields);
  $("client-form").addEventListener("submit",createConnection);
  for(const [button,field] of [["client-copy-config","client-guide-config"],["client-copy-command","client-guide-command"]])$(button).addEventListener("click",async()=>{const epoch=authEpoch;try{assertOwner(epoch);await navigator.clipboard.writeText($(field).value);assertOwner(epoch);feedback("clients-feedback","Setup text copied. Review it in the selected client before running it.");}catch{if(epoch===authEpoch&&!authExpired)feedback("clients-feedback","Select the setup text and copy it with your keyboard.");}});
  $("client-copy-url").addEventListener("click",async()=>{
    const item=currentGuideRemoteConnection();
    if(clientBusy||!item)return;
    const epoch=authEpoch,url=$("client-guide-url").value;
    try {
      assertOwner(epoch);await navigator.clipboard.writeText(url);assertOwner(epoch);
      const current=currentGuideRemoteConnection();
      if(current?.id===item.id&&current.revision===item.revision)feedback("clients-feedback","MCP server URL copied. Use it with this connection's private token in your client.");
    } catch {
      const current=currentGuideRemoteConnection();
      if(epoch===authEpoch&&current?.id===item.id&&current.revision===item.revision)feedback("clients-feedback","Select the MCP server URL and copy it with your keyboard.");
    }
  });
  $("client-guide-reveal").addEventListener("click",async()=>{
    const item=currentGuideRemoteConnection();
    if(clientBusy||!item)return;
    await revealRemoteCredential($("client-guide-reveal"),item);
  });
  $("client-secret-close").addEventListener("click",()=>clearRemoteSecret());
  $("client-secret-dialog").addEventListener("cancel",()=>clearRemoteSecret(false));
  $("client-secret-dialog").addEventListener("close",()=>clearRemoteSecret(false));
  $("client-secret-copy").addEventListener("click",async()=>{
    const version=remoteSecretVersion;
    if(authExpired||!$("client-secret-dialog").open||!$("client-secret-token").value)return;
    if(remoteSecretExpiry<=Date.now()) { clearRemoteSecret(false); feedback("client-secret-feedback","This access expired. Refresh the connection list.",true); return; }
    try {
      assertOwner();await navigator.clipboard.writeText($("client-secret-token").value);
      if(version===remoteSecretVersion&&!authExpired) { clearRemoteSecret();feedback("clients-feedback","Token copied and removed from this window. Paste it only into this client's private credential setting."); }
    } catch { if(version===remoteSecretVersion&&!authExpired)feedback("client-secret-feedback","Select the private token and copy it with your keyboard, then close this window."); }
  });
  window.addEventListener("pagehide",()=>clearRemoteSecret());
  $("settings-close").addEventListener("click",()=>$("settings-dialog").close());
  $("settings-dialog").addEventListener("cancel",event=>{if(!setup?.configured)event.preventDefault();});
  $("refresh-chats").addEventListener("click",async()=>{const epoch=authEpoch;try{await loadChats();}catch(error){if(epoch===authEpoch&&!authExpired)feedback("policy-feedback",error.message,true);}});
  $("connection-open").addEventListener("click",()=>setup?.configured?openConnection():openSettings());
  $("connection-close").addEventListener("click",()=>$("connection-dialog").close());
  $("connection-settings").addEventListener("click",()=>{$("connection-dialog").close();openSettings();});
  $("connection-check").addEventListener("click",async()=>{const epoch=authEpoch;await refreshStatus();if(epoch===authEpoch&&!authExpired)await refreshPairing();});
  $("import-check").addEventListener("click",refreshStatus);
  $("import-action").addEventListener("click",async()=>{
    if(authExpired)return;
    const action=$("import-action").dataset.action;
    if(action==="settings")await openSettings();
    else if(action==="connection")openConnection();
    else if(action==="check")await refreshStatus();
    else if(action==="resume")await command($("import-action"),"/api/resume","import-feedback","Import resumed.");
  });
  $("connection-dialog").addEventListener("close",clearQR);
  $("pairing-start").addEventListener("click",startPairing);
  $("disconnect").addEventListener("click",async()=>{const epoch=authEpoch;clearQR();await command($("disconnect"),"/api/disconnect","pairing-feedback","WhatsApp disconnected. Saved messages remain searchable.");if(epoch===authEpoch&&!authExpired)await refreshPairing();});
  $("pause-toggle").addEventListener("click",()=>command($("pause-toggle"),status?.Paused?"/api/resume":"/api/pause",$("search-content").hidden?"import-feedback":"search-feedback",status?.Paused?"Import resumed.":"Import paused. Saved messages remain searchable."));
  $("filters-toggle").addEventListener("click",()=>{const hidden=!$("search-filters").hidden;$("search-filters").hidden=hidden;$("filters-toggle").setAttribute("aria-expanded",String(!hidden));});
  $("search-form").addEventListener("submit",event=>{event.preventDefault();runSearch();});
  $("search-more").addEventListener("click",()=>runSearch(true));
  $("context-copy").addEventListener("click",async()=>{const epoch=authEpoch;try{assertOwner(epoch);await navigator.clipboard.writeText(visibleMessages.map(message=>(message.SenderName||"Unknown sender")+" · "+dateText(message.SentAt)+"\n"+(message.Body||"")+(message.Caption?"\n"+message.Caption:"")+(message.Derived||[]).map(excerpt=>"\n"+extractionLabel(excerpt)+"\n"+excerpt.Text).join("")).join("\n\n"));assertOwner(epoch);feedback("context-feedback","Visible messages copied in their original text order.");}catch{if(epoch===authEpoch&&!authExpired)feedback("context-feedback","Select the message text and copy it with your keyboard.");}});
  $("delete-open").addEventListener("click",()=>{$("delete-confirmation").value="";$("delete-confirm").disabled=true;feedback("delete-feedback","");$("delete-dialog").showModal();});
  $("delete-cancel").addEventListener("click",()=>$("delete-dialog").close());
  $("delete-confirmation").addEventListener("input",()=>{$("delete-confirm").disabled=$("delete-confirmation").value!=="DELETE LOCAL DATA";});
  $("delete-form").addEventListener("submit",async event=>{event.preventDefault();if(authExpired||$("delete-confirmation").value!=="DELETE LOCAL DATA")return;const epoch=authEpoch;$("delete-confirm").disabled=true;try{await api("/api/data/delete","POST",{confirmation:$("delete-confirmation").value});assertOwner(epoch);expireAuth();$("auth-message").textContent="Local data deleted. Restart the app to create a new archive.";}catch(error){if(epoch===authEpoch&&!authExpired){feedback("delete-feedback",error.message,true);$("delete-confirm").disabled=false;}}});
  $("admin-login-form").addEventListener("submit", loginRemote);
  $("admin-logout").addEventListener("click", logoutRemote);

  $("remote-login-form").addEventListener("submit", async event => {
    event.preventDefault();
    if (!remoteMode || $("remote-login-submit").disabled) return;
    const epoch = authEpoch;
    $("remote-login-submit").disabled = true;
    feedback("remote-login-feedback", "Unlocking…");
    let password = $("remote-password").value;
    $("remote-password").value = "";
    try {
      const challengeResponse = await fetch("/api/remote/login", {credentials:"same-origin",cache:"no-store",signal:AbortSignal.timeout(15000)});
      if (!challengeResponse.ok) throw new Error("Please wait briefly, then try again.");
      const challenge = await challengeResponse.json();
      if (epoch !== authEpoch) return;
      const response = await fetch("/api/remote/login", {method:"POST",headers:{"Content-Type":"application/json"},credentials:"same-origin",cache:"no-store",body:JSON.stringify({password,challenge:challenge.challenge}),signal:AbortSignal.timeout(15000)});
      password = "";
      if (epoch !== authEpoch) return;
      if (!response.ok) throw new Error(response.status === 429 ? "Too many attempts. Wait 30 seconds, then try again." : "Could not unlock. Check your password and try again.");
      const session = await response.json();
      if (epoch !== authEpoch) return;
      if (typeof session.csrf_token !== "string" || !/^[A-Za-z0-9_-]{43}$/u.test(session.csrf_token)) throw new Error("Sign-in did not finish. Try again.");
      authEpoch++; authExpired=false; csrf=session.csrf_token;
      feedback("remote-login-feedback", "");
      await initializeWorkspace();
    } catch (error) {
      if (epoch === authEpoch || !authExpired) feedback("remote-login-feedback", error.message || "Could not unlock. Try again.", true);
    } finally {
      password = "";
      $("remote-login-submit").disabled = false;
      if (authExpired) $("remote-password").focus();
    }
  });
  $("remote-logout").addEventListener("click", async () => {
    // Capture logout material for this one request, then fence every pending
    // archive response immediately, including QR blob and connection requests.
    const token = csrf;
    expireAuth();
    $("remote-login-submit").disabled = true;
    try { await fetch("/api/remote/logout", {method:"POST",headers:{"Content-Type":"application/json","X-CSRF-Token":token},credentials:"same-origin",cache:"no-store",body:"{}",signal:AbortSignal.timeout(5000)}); } catch { /* local private state is already cleared */ } finally { $("remote-login-submit").disabled = false; if (authExpired) $("remote-password").focus(); }
  });

  const remoteProviderInfo = {
    tailscale_private: ["Private device access. Sign in to Tailscale on this computer and your phone. Hosted AI services cannot reach a private Tailscale address.", "tailscale-serve"],
    tailscale_funnel: ["Public HTTPS for an AI client. Tailscale Funnel permissions must be enabled for this saved device. Sign in after saving.", "tailscale-funnel"],
    ngrok: ["Use your assigned ngrok HTTPS domain and authtoken. No second API key is required.", "ngrok"],
    cloudflare: ["Requires an existing Cloudflare domain and configured tunnel. Route this exact hostname to http://127.0.0.1:8124 on the archive computer. Route verification remains separate from connecting.", "cloudflare"]
  };
  const remoteStates = {
    disabled: ["Remote access is off", "Local archiving and search work without remote access."],
    needs_setup: ["Needs setup", "Review the provider settings and save a persistent hostname."],
    connecting: ["Connecting", "Keep the app open. Complete provider sign-in if requested."],
    online: ["Online", "Provider transport is connected. Configure a scoped AI connection and test your client separately."],
    reconnecting: ["Reconnecting", "The saved hostname is preserved. Check connectivity or update the provider token; local search remains available."],
    permission_required: ["Permission required", "Review your provider's access or Funnel permissions, then retry the saved setup."],
    credentials_rejected: ["Credentials rejected", "Enter a valid provider token or complete provider sign-in, then retry."],
    service_limit: ["Service limit reached", "Check your provider's account limits, then retry the saved setup."],
    service_limit_reached: ["Service limit reached", "Check your provider's account limits, then retry the saved setup."],
    route_unverified: ["Helper running; connection and hostname unverified", "The helper process is running, but its Cloudflare connection and saved hostname are unverified. Check connectivity, provider credentials and the dashboard route; test the public endpoint and AI client separately."],
    manual: ["Manual MCP configuration", "Your launch settings own remote MCP. Restart without manual MCP flags to use managed setup here."]
  };
  function clearRemoteWizard() {
    wizardVersion++; wizardSnapshot=null; wizardBusy=false; wizardConfirmation="";
    if(wizardTimer!==null){clearInterval(wizardTimer);wizardTimer=null;}
    $("remote-access-form").reset(); $("remote-password-form").reset();
    $("remote-provider-secret").value=""; $("remote-new-password").value="";
    $("remote-signin-link").removeAttribute("href"); $("remote-signin-link").hidden=true;
    $("remote-endpoint").value=""; $("remote-endpoint-panel").hidden=true;
    for(const id of ["remote-access-form","remote-password-form","remote-confirm-panel"])$(id).hidden=true;
    for(const id of ["remote-access-state","remote-access-action","remote-access-mcp","remote-access-feedback"])$(id).textContent="";
    for(const el of $("remote-access-dialog").querySelectorAll("button,input,select"))el.disabled=false;
  }
  function renderRemoteStatus(v) {
    wizardSnapshot=v;
    const text=remoteStates[v.state]||remoteStates.needs_setup;
    $("remote-access-state").textContent=text[0];$("remote-access-action").textContent=text[1];
    $("remote-access-mcp").textContent=v.manual?"This wizard does not supervise the manual tunnel.":!v.saved_origin?"MCP endpoint: no hostname saved.":v.authorization_ready?"MCP account authorization ready. Each AI client still needs its own scoped, expiring credential.":"MCP waiting for account or credential recovery. Provider connectivity does not grant archive access.";
    $("remote-endpoint-panel").hidden=!v.saved_origin;$("remote-endpoint").value=v.saved_origin?v.saved_origin+"/mcp":"";
    $("remote-configure-open").hidden=v.manual;$("remote-configure-open").textContent=v.provider?"Review or reconnect":"Set up remote access";
    $("remote-saved-actions").hidden=!v.provider||v.manual;
    $("remote-disable").hidden=v.state==="disabled";
    $("remote-signin").hidden=!String(v.provider).startsWith("tailscale_")||v.state==="disabled";
    $("remote-password-open").textContent=v.password_configured?"Change remote owner password":"Set a remote owner password first";
    if(v.manual)$("remote-access-form").hidden=true;
  }
  async function refreshRemoteStatus() {
    if(remoteMode||remoteAdmin||wizardBusy||!$("remote-access-dialog").open)return;
    const version=wizardVersion;
    try{const v=await api("/api/remote/status");if(version===wizardVersion&&!wizardBusy)renderRemoteStatus(v);}catch(error){if(version===wizardVersion)feedback("remote-access-feedback",error.message,true);}
  }
  function remoteProviderFields() {
    const provider=$("remote-provider").value, tailscale=provider.startsWith("tailscale_");
    const info=remoteProviderInfo[provider];$("remote-provider-help").textContent=info[0];
    $("remote-provider-guide").href="https://devshiftteam.github.io/my-whatsapp-mcp/remote/"+info[1]+".html";
    $("remote-node-field").hidden=!tailscale;$("remote-origin-field").hidden=tailscale;$("remote-provider-secret-field").hidden=tailscale;
    $("remote-node").required=tailscale;$("remote-origin").required=!tailscale;$("remote-secret-note").hidden=tailscale||!wizardSnapshot?.provider;
    $("remote-provider-secret").value="";
    refreshSelects("remote-goal","remote-provider");
  }
  function openRemoteSetup() {
    const v=wizardSnapshot||{};
    $("remote-access-form").reset();$("remote-access-form").hidden=false;$("remote-confirm-panel").hidden=true;
    $("remote-provider").value=v.provider||"tailscale_private";
    $("remote-goal").value=v.provider==="cloudflare"?"domain":v.provider&&v.provider!=="tailscale_private"?"public":"private";
    $("remote-node").value=v.node_name||"wa-archive";$("remote-origin").value=v.saved_origin||"";$("remote-browser-enable").checked=Boolean(v.browser_enabled);
    $("remote-goal").disabled=$("remote-provider").disabled=Boolean(v.provider);
    $("remote-node").readOnly=$("remote-origin").readOnly=Boolean(v.provider);
    remoteProviderFields();refreshSelects("remote-goal","remote-provider");$(v.provider?"remote-browser-enable":"remote-goal").focus();
  }
  async function remoteWizardAction(action,success) {
    if(wizardBusy)return;
    wizardBusy=true;const version=++wizardVersion;let accepted=false;
    for(const el of $("remote-access-dialog").querySelectorAll("button,input,select"))el.disabled=true;refreshAllSelects();
    try{await action();if(version!==wizardVersion)return;success?.();accepted=true;feedback("remote-access-feedback","Saved. Check the provider and MCP states below.");}
    catch(error){if(version===wizardVersion)feedback("remote-access-feedback",error.message,true);}
    finally{if(version===wizardVersion){wizardBusy=false;for(const el of $("remote-access-dialog").querySelectorAll("button,input,select"))el.disabled=false;$("remote-goal").disabled=$("remote-provider").disabled=Boolean(wizardSnapshot?.provider);refreshAllSelects();await refreshRemoteStatus();if(accepted&&$("remote-access-form").hidden){$("remote-access-state").focus({preventScroll:true});$("remote-access-dialog").scrollTop=0;}}}
  }
  $("remote-access-open").addEventListener("click",async()=>{if(remoteMode||remoteAdmin)return;clearRemoteWizard();$("remote-access-dialog").showModal();refreshAllSelects();$("remote-access-dialog").scrollTop=0;await refreshRemoteStatus();if($("remote-access-dialog").open)wizardTimer=setInterval(refreshRemoteStatus,3000);});
  $("remote-access-close").addEventListener("click",()=>{if(!wizardBusy){$("remote-access-dialog").close();clearRemoteWizard();}});
  $("remote-access-dialog").addEventListener("cancel",event=>{if(wizardBusy)event.preventDefault();else clearRemoteWizard();});
  $("remote-access-dialog").addEventListener("close",clearRemoteWizard);
  $("remote-access-refresh").addEventListener("click",refreshRemoteStatus);
  $("remote-configure-open").addEventListener("click",openRemoteSetup);
  $("remote-goal").addEventListener("change",()=>{$("remote-provider").value={private:"tailscale_private",public:"tailscale_funnel",domain:"cloudflare"}[$("remote-goal").value];remoteProviderFields();});
  $("remote-provider").addEventListener("change",remoteProviderFields);
  $("remote-form-cancel").addEventListener("click",()=>{$("remote-access-form").reset();$("remote-access-form").hidden=true;$("remote-password-form").reset();$("remote-password-form").hidden=true;});
  $("remote-password-open").addEventListener("click",()=>{$("remote-password-form").hidden=false;$("remote-new-password").focus();});
  $("remote-password-cancel").addEventListener("click",()=>{$("remote-password-form").reset();$("remote-password-form").hidden=true;});
  $("remote-browser-enable").addEventListener("change",()=>{if($("remote-browser-enable").checked&&!wizardSnapshot?.password_configured){$("remote-browser-enable").checked=false;$("remote-password-form").hidden=false;feedback("remote-access-feedback",messages.remote_password_required);$("remote-new-password").focus();}});
  $("remote-password-form").addEventListener("submit",event=>{event.preventDefault();let password=$("remote-new-password").value;$("remote-new-password").value="";const bytes=new TextEncoder().encode(password);const valid=bytes.length>=12&&bytes.length<=128&&!/\p{Cc}/u.test(password);bytes.fill(0);if(!valid){password="";feedback("remote-access-feedback","Use 12–128 UTF-8 bytes without control characters for the owner password.",true);return;}remoteWizardAction(async()=>{try{await api("/api/remote/password","PUT",{password});}finally{password="";}},()=>{$("remote-password-form").hidden=true;});});
  $("remote-access-form").addEventListener("submit",event=>{event.preventDefault();const provider=$("remote-provider").value;const input={provider,origin:provider.startsWith("tailscale_")?wizardSnapshot?.saved_origin||"":$("remote-origin").value,node_name:provider.startsWith("tailscale_")?$("remote-node").value:"",secret:$("remote-provider-secret").value,expose_browser:$("remote-browser-enable").checked};$("remote-provider-secret").value="";remoteWizardAction(async()=>{try{await api("/api/remote/configure","POST",input);}finally{input.secret="";}},()=>{$("remote-access-form").hidden=true;$("remote-password-form").hidden=true;$("remote-new-password").value="";});});
  $("remote-endpoint-copy").addEventListener("click",async()=>{try{await navigator.clipboard.writeText($("remote-endpoint").value);assertOwner();feedback("remote-access-feedback","Endpoint copied. No token is included.");}catch{feedback("remote-access-feedback","Select the endpoint and copy it manually.",true);}});
  $("remote-signin").addEventListener("click",async()=>{const version=wizardVersion;try{const result=await api("/api/remote/sign-in","POST",{});if(version!==wizardVersion)return;$("remote-signin-link").hidden=!result.url;if(result.url)$("remote-signin-link").href=result.url;else feedback("remote-access-feedback","No sign-in action is pending. Refresh the provider status.");}catch(error){if(version===wizardVersion)feedback("remote-access-feedback",error.message,true);}});
  for(const kind of ["disable","remove","migrate"])$("remote-"+kind).addEventListener("click",()=>{wizardConfirmation=kind;$("remote-confirm-title").textContent={disable:"Disable remote access?",remove:"Remove this saved provider?",migrate:"Change the saved identity?"}[kind];$("remote-confirm-text").textContent={disable:"Connections stop, but the saved hostname and provider credentials remain for a later reconnect.",remove:"Connections stop and the saved hostname and provider credentials are removed. Your archive and separate owner password remain.",migrate:"Remove the current provider identity first, then configure the new domain or account. You must reconfigure AI clients for a changed endpoint. The current hostname will never be silently replaced."}[kind];$("remote-confirm-panel").hidden=false;$("remote-confirm-yes").focus();});
  $("remote-confirm-no").addEventListener("click",()=>{$("remote-confirm-panel").hidden=true;wizardConfirmation="";});
  $("remote-confirm-yes").addEventListener("click",()=>{const kind=wizardConfirmation;if(!kind)return;remoteWizardAction(()=>api("/api/remote/"+(kind==="disable"?"disable":"remove"),"POST",{confirm:kind==="disable"?"disable":"remove"}),()=>{$("remote-confirm-panel").hidden=true;$("remote-access-form").hidden=true;$("remote-password-form").hidden=true;$("remote-new-password").value="";$("remote-provider-secret").value="";$("remote-signin-link").hidden=true;$("remote-signin-link").removeAttribute("href");wizardConfirmation="";});});


  async function initializeWorkspace() {
    const epoch = authEpoch;
    try {
      const nextSetup = await api("/api/setup");
      if (epoch !== authEpoch || authExpired) return;
      setup = nextSetup; setup.Policy = setup.policy;
      $("demo-label").hidden = !setup.demo;
      $("admin-logout").hidden = !remoteAdmin;
      $("auth-screen").hidden = true;
      $("workspace").hidden = false;
      refreshAllSelects();
      selectWorkspaceView("stats");
      await refreshStatus();
      if (epoch !== authEpoch || authExpired) return;
      await refreshPairing();
      if (epoch !== authEpoch || authExpired) return;
      if (!setup.configured) await openSettings();
      else {
        await loadChats();
        if (epoch !== authEpoch || authExpired) return;
      }
      if (epoch === authEpoch && !authExpired) startPolling();
    } catch (error) {
      if (epoch !== authEpoch || authExpired) return;
      $("workspace").hidden = true;
      $("auth-screen").hidden = false;
      window.MyWhatsAppSelects?.closeAll();
      refreshAllSelects();
      $("auth-message").textContent = error.message;
    }
  }

  async function start() {
    // Remove any fragment capability before requests, including on the remote
    // login shell. Remote access never consumes a local owner bootstrap.
    let initialFragment = location.hash;
    history.replaceState(null, "", location.pathname);
    if (!remoteAdmin) {
      try {
        const response = await fetch("/api/interface", {credentials:"same-origin", cache:"no-store"});
        if (!response.ok) throw new Error("The archive interface is unavailable.");
        const mode = await response.json();
        if (mode.realm !== "local" && mode.realm !== "remote") throw new Error("The archive interface is unavailable.");
        remoteMode = mode.realm === "remote";
      } catch (error) { initialFragment = ""; $("auth-message").textContent = error.message; return; }
    }
    for (const node of document.querySelectorAll("[data-native-only]")) node.hidden = remoteAdmin || remoteMode;
    if (remoteMode) {
      initialFragment = "";
      $("admin-login-form").hidden = true;
      for (const node of document.querySelectorAll("[data-managed-excluded]")) node.hidden = true;
      $("remote-logout").hidden = false;
      expireAuth();
      return;
    }
    const token = remoteAdmin ? "" : new URLSearchParams(initialFragment.slice(1)).get("bootstrap");
    initialFragment = "";
    if (remoteAdmin) {
      authExpired = true;
      authBusy = true;
      showRemoteLogin("Checking your sign-in…");
      updateAuthControls();
      const epoch = authEpoch;
      try {
        const session = await remoteAuthRequest("/api/auth/session", {});
        if (epoch !== authEpoch) return;
        acceptRemoteSession(session);
        await initializeWorkspace();
      } catch (error) {
        if (epoch === authEpoch) {
          showRemoteLogin();
          if (error.code !== "unauthorized") feedback("admin-login-feedback", error.message, true);
        }
      } finally {
        authBusy = false;
        updateAuthControls();
        if (authExpired) $("admin-password").focus();
      }
      return;
    }
    try {
      // localStorage shares only this capability across tabs of the exact
      // origin, including its port. The HttpOnly cookie is still required.
      if (!token) { try { csrf=localStorage.getItem(capabilityKey)||""; } catch { /* fail closed at API */ } }
      const session = token ? await api("/api/bootstrap","POST",{token}) : await api("/api/session");
      csrf=session.csrf_token;
      try { localStorage.setItem(capabilityKey,csrf); } catch { /* memory session works; reload requires a fresh owner link */ }
      await initializeWorkspace();
    }catch(error){if(!authExpired)$("auth-message").textContent=error.message;}
  }
  start();
})();
