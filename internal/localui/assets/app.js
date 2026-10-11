"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  let csrf = "";
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
  let qrURL = "";
  const remoteAdmin = document.documentElement.dataset.remoteAdmin === "true";
  const capabilityKey = "wa_archive_owner_capability";

  const messages = {
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
      throw new Error(messages[value.code] || "This action did not finish. Try again or restart the app.");
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
    authExpired = true;
    authEpoch++;
    stopPolling();
    const expiredCSRF = csrf;
    csrf = "";
    if (!remoteAdmin) {
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
    $("auth-message").textContent = messages.unauthorized;
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
    $("workspace").hidden = true;
    $("auth-screen").hidden = false;
    $("auth-message").textContent = message;
    $("admin-login-form").hidden = false;
    $("admin-password").value = "";
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
    if (!Number.isFinite(seconds) || seconds < 0 || seconds === 0 && !allowEpoch) return "No messages yet";
    const date = new Date(seconds * 1000);
    return Number.isNaN(date.getTime()) ? "Not observed" : new Intl.DateTimeFormat(undefined, options).format(date);
  }

  function statsUnavailable() {
    return Boolean(status?.ReconciliationRequired || status?.Connection === "reconciliation_required");
  }

  function clearStatsData() {
    statsSnapshot = null;
    statsRenderKey = "";
    for (const id of ["stats-message-count", "stats-chat-count", "stats-sent-count", "stats-received-count", "stats-transcribed", "stats-latest-message"]) $(id).textContent = "—";
    $("stats-range-description").textContent = "Only readable messages in your saved archive are counted.";
    $("stats-activity-total").textContent = "";
    $("stats-chats-total").textContent = "";
    $("stats-chart").replaceChildren();
    $("stats-top-chats").replaceChildren();
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
    $("stats-chat").replaceChildren(new Option("All archived chats", ""));
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
    activeView = view;
    $("stats-view").hidden = view !== "stats";
    $("archive-view").hidden = view !== "archive";
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
    if (chosen && !select.value) invalidateStats(true);
  }

  function statsFilter() {
    const request = {};
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
    $("stats-custom-dates").hidden = $("stats-range").value !== "custom";
    invalidateStats(true);
    $("stats-updated").textContent = "Updating view";
    refreshStats();
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
      if (![snapshot.messages, snapshot.chats, snapshot.sent, snapshot.received].every(value => Number.isSafeInteger(value) && value >= 0) || !Array.isArray(snapshot.daily) || !Array.isArray(snapshot.top_chats) || !Array.isArray(snapshot.kinds)) throw new Error("Stats could not be read. Use Refresh stats to try again.");
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

  function renderStatsChart(snapshot) {
    const target = $("stats-chart");
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
    const height = width < 480 ? 205 : 238;
    const left = 43, right = 9, top = 14, bottom = 34;
    const plotWidth = width - left - right;
    const plotHeight = height - top - bottom;
    const maximum = Math.max(1, ...rows.map(row => statsCount(row.messages)));
    const magnitude = 10 ** Math.floor(Math.log10(maximum));
    const axisMaximum = Math.max(4, Math.ceil(maximum / magnitude) * magnitude);
    const chart = svgNode("svg", {viewBox:"0 0 " + width + " " + height, role:"img", "aria-label":"Message activity: " + statsNumber(snapshot.messages) + " messages across " + rows.length + " UTC periods. An accessible table follows."});
    for (let tick = 0; tick <= 4; tick++) {
      const y = top + plotHeight * tick / 4;
      chart.append(svgNode("line", {x1:left, y1:y, x2:width - right, y2:y, class:"chart-grid"}));
      const value = Math.round(axisMaximum * (4 - tick) / 4);
      chart.append(svgNode("text", {x:left - 8, y:y + 4, "text-anchor":"end", class:"chart-label"}, new Intl.NumberFormat(undefined, {notation:axisMaximum >= 10000 ? "compact" : "standard", maximumFractionDigits:1}).format(value)));
    }
    const stride = plotWidth / rows.length;
    const gap = Math.max(1, Math.min(5, stride * .24));
    const labelCount = width < 480 ? 3 : 5;
    const labelIndices = new Set([0, rows.length - 1]);
    for (let index = 1; index < labelCount - 1; index++) labelIndices.add(Math.round(index * (rows.length - 1) / (labelCount - 1)));
    rows.forEach((row, index) => {
      const count = statsCount(row.messages);
      const barHeight = count / axisMaximum * plotHeight;
      const x = left + index * stride + gap / 2;
      const rect = svgNode("rect", {x, y:top + plotHeight - barHeight, width:Math.max(1, stride - gap), height:barHeight, rx:Math.min(2, stride / 4), class:"chart-bar"});
      rect.append(svgNode("title", {}, String(row.date) + ": " + statsNumber(count) + " messages"));
      chart.append(rect);
      if (labelIndices.has(index)) {
        const date = new Date(String(row.date) + "T00:00:00Z");
        const options = snapshot.activity_interval === "year" ? {year:"numeric", timeZone:"UTC"} : snapshot.activity_interval === "month" ? {month:"short", year:"2-digit", timeZone:"UTC"} : {month:"short", day:"numeric", timeZone:"UTC"};
        const label = Number.isNaN(date.getTime()) ? "" : new Intl.DateTimeFormat(undefined, options).format(date);
        const anchor = index === 0 ? "start" : index === rows.length - 1 ? "end" : "middle";
        const textX = index === 0 ? left : index === rows.length - 1 ? width - right : left + (index + .5) * stride;
        chart.append(svgNode("text", {x:textX, y:height - 9, "text-anchor":anchor, class:"chart-label"}, label));
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
  }

  function renderStats(snapshot) {
    statsSnapshot = snapshot;
    const key = JSON.stringify({...snapshot, sampled_at:0});
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
    const top = $("stats-top-chats");
    top.replaceChildren();
    const topChats = (snapshot.top_chats || []).slice(0, 8);
    const maxChat = Math.max(1, ...topChats.map(chat => statsCount(chat.messages)));
    for (const chat of topChats) {
      const item = document.createElement("li");
      const row = document.createElement("div");
      row.className = "stats-chat-row";
      row.append(bdi(chat.name || "Unnamed chat", "stats-chat-name"));
      const count = document.createElement("strong");
      count.textContent = statsNumber(chat.messages);
      row.append(count);
      const track = document.createElement("div");
      track.className = "stats-chat-track";
      track.setAttribute("aria-hidden", "true");
      const fill = document.createElement("span");
      fill.style.width = statsCount(chat.messages) / maxChat * 100 + "%";
      track.append(fill);
      item.append(row, track);
      top.append(item);
    }
    if (!topChats.length) {
      const empty = document.createElement("li");
      empty.className = "stats-empty";
      empty.textContent = "No chats match these filters.";
      top.append(empty);
    }
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
    const select = $("search-chat"), chosen = select.value;
    select.replaceChildren(new Option("All included chats", ""));
    for (const chat of chats) {
      if (setup?.Policy.ChatMode === "all" || chat.Selected) select.append(new Option(chat.Name || "Unnamed chat", chat.ID));
    }
    select.value = chosen;
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
  }

  function renderConnections(snapshot) {
    clientSnapshot=snapshot;
    const stateNames={ready:"Available", not_configured:"Not configured", unavailable:"Unavailable"};
    $("clients-availability").textContent="Local: "+(stateNames[snapshot.local_state]||"Unavailable")+" · Remote: "+(stateNames[snapshot.remote_state]||"Unavailable");
    if (!status?.ReconciliationRequired && status?.Connection !== "reconciliation_required" && (status?.Codes || []).includes("app_state_sync_error")) $("clients-availability").textContent+=". WhatsApp state sync is incomplete. AI clients can read stored messages within their permissions, with coverage warnings; the archive may differ from your phone.";
    for(const mode of ["local","remote"])if(connectionRecovery(snapshot,mode)!=="ready")$("clients-availability").textContent+=". "+(mode==="local"?"Local":"Remote")+" recovery needs attention; access for that mode remains blocked.";
    updateConnectionFields();
    const key=JSON.stringify(snapshot); if(key===clientRenderKey)return;
    clientRenderKey=key;
    const focused=document.activeElement;
    const hint=focused===document.body||focused===$("clients-dialog")?clientFocusHint:null;
    const focusID=focused?.dataset?.connectionId||hint?.id, focusAction=focused?.dataset?.action||hint?.action;
    clientFocusHint=null;
    const list=$("client-list"); list.replaceChildren();
    for (const item of snapshot.items||[]) {
      const card=document.createElement("article"); card.className="model-card"; card.dataset.connectionId=item.id;
      const title=document.createElement("h3"); title.append(bdi(item.label)); title.tabIndex=-1; title.dataset.connectionId=item.id; title.dataset.action="heading"; card.append(title);
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
        if(item.mode==="remote" && item.revision>0)action("reveal","Reveal remote token");
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
        clientSnapshot=null; clientRenderKey=""; $("client-list").replaceChildren(); clearClientGuide(); clearRemoteSecret(); updateConnectionFields();
        feedback("clients-feedback",error.message,true);
      }
    }
  }

  function renderClientGuide(guide,item) {
    clearClientGuide(); clientGuideID=item.id;
    clientGuideBinding={id:item.id,revision:item.revision,client:item.client,mode:item.mode};
    if(item.mode==="remote"&&guide.mode==="remote"&&guide.client===item.client&&guide.status!=="unsupported"&&typeof guide.endpoint==="string") {
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
    if(kind==="reveal") { await revealRemoteCredential(button,item); return; }
    if(clientBusy||authExpired)return;
    const epoch=authEpoch;
    clientBusy=true; clientVersion++; button.disabled=true; updateConnectionFields();
    clientFocusHint={id:item.id,action:kind};
    const version=clientVersion;
    feedback("clients-feedback",kind==="revoke"?"Revoking client access…":"Preparing inert setup instructions…");
    if(kind==="revoke") { clearClientGuide(); clearRemoteSecret(); }
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
    clientBusy=true; clientVersion++; updateConnectionFields(); clearClientGuide(); clearRemoteSecret();
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
  $("original-budget-size").addEventListener("input",()=>{editBudget();$("original-budget-preset").value="custom";});
  $("original-budget-unit").addEventListener("change",()=>{editBudget();$("original-budget-preset").value="custom";updateBudgetUnits();});
  $("original-budget-preset").addEventListener("change",()=>{editBudget();if($("original-budget-preset").value!=="custom"){$("original-budget-unit").value="mib";$("original-budget-size").value=$("original-budget-preset").value;updateBudgetUnits();}});
  $("clients-open").addEventListener("click",()=>{if(!authExpired){clearClientGuide();feedback("clients-feedback","");$("clients-dialog").showModal();refreshConnections();renderConnectionChats();}});
  $("clients-close").addEventListener("click",()=>$("clients-dialog").close());
  $("clients-dialog").addEventListener("close",()=>{clientVersion++;clientFocusHint=null;clearClientGuide();clearRemoteSecret();});
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
      selectWorkspaceView("stats");
      await refreshStatus();
      if (epoch !== authEpoch || authExpired) return;
      await refreshPairing();
      if (epoch !== authEpoch || authExpired) return;
      if (!setup.configured) await openSettings();
      else {
        await loadChats();
        if (epoch !== authEpoch || authExpired) return;
        $("stats-title").focus({preventScroll:true});
      }
      if (epoch === authEpoch && !authExpired) startPolling();
    } catch (error) {
      if (epoch !== authEpoch || authExpired) return;
      $("workspace").hidden = true;
      $("auth-screen").hidden = false;
      $("auth-message").textContent = error.message;
    }
  }

  async function start() {
    // Remove any fragment capability before requests, including on the remote
    // login shell. Remote access never consumes a local owner bootstrap.
    const fragment = new URLSearchParams(location.hash.slice(1));
    const token = remoteAdmin ? "" : fragment.get("bootstrap");
    history.replaceState(null, "", location.pathname);
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
