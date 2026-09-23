"use strict";

const state = {
  config: null,
  inputs: [],
  outputs: [],
  selectedRefs: [], // ordered list of input filenames
  runMode: "oneshot",
  interactiveLoaded: false,
  currentJobId: null,
  // The take showing in the preview, and whether a live render may take the
  // preview over. Picking a take while a render is running stops it following
  // — otherwise the next step frame, a second away, would undo the click.
  selected: null,
  followPreview: true,
  queueClearedAt: 0, // finished-job entries at/before this unix time are hidden from the queue panel
  es: null,
};

const $ = (id) => document.getElementById(id);

const els = {
  sessionSelect: $("sessionSelect"),
  modelDisplay: $("modelDisplay"),
  modelLabel: $("modelLabel"),
  modelBadge: $("modelBadge"),
  promptInput: $("promptInput"),
  widthInput: $("widthInput"),
  heightInput: $("heightInput"),
  sizePresets: $("sizePresets"),
  stepsInput: $("stepsInput"),
  guidanceInput: $("guidanceInput"),
  seedInput: $("seedInput"),
  zoomInput: $("zoomInput"),
  scheduleSelect: $("scheduleSelect"),
  powerAlphaWrap: $("powerAlphaWrap"),
  powerAlphaInput: $("powerAlphaInput"),
  baseModeInput: $("baseModeInput"),
  mmapInput: $("mmapInput"),
  showStepsInput: $("showStepsInput"),
  runModeSeg: $("runModeSeg"),
  interactiveControls: $("interactiveControls"),
  interactiveLoadBtn: $("interactiveLoadBtn"),
  interactiveStopBtn: $("interactiveStopBtn"),
  interactiveState: $("interactiveState"),
  generateBtn: $("generateBtn"),
  validationErrors: $("validationErrors"),
  previewImg: $("previewImg"),
  previewFrame: $("previewFrame"),
  previewEmpty: $("previewEmpty"),
  followBtn: $("followBtn"),
  progressRow: $("progressRow"),
  progressFill: $("progressFill"),
  progressLabel: $("progressLabel"),
  cancelBtn: $("cancelBtn"),
  gallery: $("gallery"),
  takeCount: $("takeCount"),
  dropZone: $("dropZone"),
  fileInput: $("fileInput"),
  refGallery: $("refGallery"),
  refCount: $("refCount"),
  queueList: $("queueList"),
  queueClearBtn: $("queueClearBtn"),
  terminalLog: $("terminalLog"),
  terminalInput: $("terminalInput"),
  terminalSendBtn: $("terminalSendBtn"),
  termClearBtn: $("termClearBtn"),
  connStatus: $("connStatus"),
  connLabel: $("connLabel"),
  sessionNewBtn: $("sessionNewBtn"),
  sessionDupBtn: $("sessionDupBtn"),
  sessionDelBtn: $("sessionDelBtn"),
  sessionNameModal: $("sessionNameModal"),
  sessionNameModalTitle: $("sessionNameModalTitle"),
  sessionNameModalLabel: $("sessionNameModalLabel"),
  sessionNameInput: $("sessionNameInput"),
  sessionNameModalError: $("sessionNameModalError"),
  sessionNameModalCancel: $("sessionNameModalCancel"),
  sessionNameModalSave: $("sessionNameModalSave"),
  confirmModal: $("confirmModal"),
  confirmModalTitle: $("confirmModalTitle"),
  confirmModalMessage: $("confirmModalMessage"),
  confirmModalCancel: $("confirmModalCancel"),
  confirmModalOk: $("confirmModalOk"),
  pathsBtn: $("pathsBtn"),
  pathPanel: $("pathPanel"),
  irisPathText: $("irisPathText"),
  modelPathText: $("modelPathText"),
  changeIrisBtn: $("changeIrisBtn"),
  changeModelBtn: $("changeModelBtn"),
  irisModal: $("irisModal"),
  irisPathInput: $("irisPathInput"),
  irisModalError: $("irisModalError"),
  irisModalCancel: $("irisModalCancel"),
  irisModalSave: $("irisModalSave"),
  modelModal: $("modelModal"),
  modelPathInput: $("modelPathInput"),
  modelModalError: $("modelModalError"),
  modelModalCancel: $("modelModalCancel"),
  modelModalSave: $("modelModalSave"),
};

const SIZE_PRESETS = [
  [256, 256], [512, 512], [768, 768], [1024, 1024],
  [512, 768], [768, 512], [1024, 768], [768, 1024],
  [1792, 1792],
];

// Download-tray icon (arrow into a tray) — an inline SVG instead of a unicode
// glyph so it renders consistently regardless of the system font's coverage.
const DOWNLOAD_ICON = `<svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M8 1.5v8.5M4.5 6.5 8 10l3.5-3.5"/><path d="M2 11v2a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1v-2"/></svg>`;

// ---------------------------------------------------------------------------
// API helpers
// ---------------------------------------------------------------------------

async function api(path, opts) {
  const res = await fetch(path, opts);
  let body = null;
  try { body = await res.json(); } catch (_) { /* no body */ }
  if (!res.ok) {
    const msg = (body && (body.error || (body.errors && body.errors.join(" ")))) || res.statusText;
    throw new Error(msg);
  }
  return body;
}

const getJSON = (path) => api(path);
const postJSON = (path, data) => api(path, {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(data || {}),
});

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

async function init() {
  buildSizePresets();
  wireEvents();
  await loadConfig();
  await Promise.all([loadSessions(), loadInputs(), loadOutputs()]);
  await loadSessionSetting(state.config.session);
  connectSSE();
}

function buildSizePresets() {
  els.sizePresets.innerHTML = "";
  for (const [w, h] of SIZE_PRESETS) {
    const btn = document.createElement("button");
    btn.textContent = `${w}×${h}`;
    btn.onclick = () => { els.widthInput.value = w; els.heightInput.value = h; };
    els.sizePresets.appendChild(btn);
  }
}

async function loadConfig() {
  state.config = await getJSON("/api/config");
  els.widthInput.min = state.config.min_dimension;
  els.widthInput.max = state.config.max_dimension;
  els.widthInput.step = state.config.dim_step;
  els.heightInput.min = state.config.min_dimension;
  els.heightInput.max = state.config.max_dimension;
  els.heightInput.step = state.config.dim_step;
  renderPaths();
}

function renderPaths() {
  const c = state.config;
  els.irisPathText.textContent = c.iris;
  els.modelPathText.textContent = c.model;
  els.modelLabel.textContent = c.model_label || c.model;
  els.modelBadge.classList.toggle("hidden", !!c.model_valid);
}

async function loadSessions() {
  const sessions = await getJSON("/api/sessions");
  els.sessionSelect.innerHTML = "";
  const names = sessions.map((s) => s.name);
  if (!names.includes(state.config.session)) names.unshift(state.config.session);
  for (const name of names) {
    const opt = document.createElement("option");
    opt.value = name;
    opt.textContent = name;
    if (name === state.config.session) opt.selected = true;
    els.sessionSelect.appendChild(opt);
  }
}

async function loadSessionSetting(name) {
  const data = await getJSON(`/api/session/${encodeURIComponent(name)}`);
  state.config.session = data.session_name || name;
  els.promptInput.value = data.prompt || "";
  els.widthInput.value = data.width || 1792;
  els.heightInput.value = data.height || 1792;
  els.stepsInput.value = data.steps ?? 0;
  els.guidanceInput.value = data.guidance ?? 0;
  els.seedInput.value = data.seed ?? -1;
  els.zoomInput.value = data.zoom || 2;
  els.scheduleSelect.value = data.schedule || "default";
  els.powerAlphaInput.value = data.power_alpha || 2.0;
  els.baseModeInput.checked = !!data.base_mode;
  els.mmapInput.checked = data.mmap !== false;
  els.showStepsInput.checked = !!data.show_steps;
  setRunMode(data.run_mode || "oneshot");
  state.selectedRefs = (data.refs || []).map((r) => r.name).filter(Boolean);
  updatePowerAlphaVisibility();
  renderRefGallery();
}

async function loadInputs() {
  state.inputs = await getJSON("/api/inputs");
  renderRefGallery();
}

async function loadOutputs() {
  state.outputs = await getJSON("/api/outputs");
  renderGallery();
}

// ---------------------------------------------------------------------------
// Form <-> params
// ---------------------------------------------------------------------------

function collectParams() {
  return {
    session_name: state.config.session,
    label: "",
    prompt: els.promptInput.value.trim(),
    width: parseInt(els.widthInput.value, 10) || 512,
    height: parseInt(els.heightInput.value, 10) || 512,
    steps: parseInt(els.stepsInput.value, 10) || 0,
    seed: parseInt(els.seedInput.value, 10),
    guidance: parseFloat(els.guidanceInput.value) || 0,
    schedule: els.scheduleSelect.value,
    power_alpha: parseFloat(els.powerAlphaInput.value) || 2.0,
    base_mode: els.baseModeInput.checked,
    mmap: els.mmapInput.checked,
    run_mode: state.runMode,
    show_steps: els.showStepsInput.checked,
    zoom: parseInt(els.zoomInput.value, 10) || 2,
    refs: state.selectedRefs.map((name) => ({ name, kind: "image" })),
  };
}

function updatePowerAlphaVisibility() {
  els.powerAlphaWrap.classList.toggle("hidden", els.scheduleSelect.value !== "power");
}

function setRunMode(mode) {
  state.runMode = mode;
  for (const btn of els.runModeSeg.querySelectorAll("button")) {
    btn.classList.toggle("active", btn.dataset.value === mode);
  }
  els.interactiveControls.classList.toggle("hidden", mode !== "interactive");
}

// ---------------------------------------------------------------------------
// Generation
// ---------------------------------------------------------------------------

async function generate() {
  els.validationErrors.classList.add("hidden");
  const params = collectParams();
  els.generateBtn.disabled = true;
  try {
    await postJSON("/api/render", params);
    // A render you just asked for is one you want to watch, whatever you
    // were looking at before it.
    state.followPreview = true;
    await loadSessions();
  } catch (err) {
    showErrors([err.message]);
  } finally {
    els.generateBtn.disabled = false;
  }
}

function showErrors(list) {
  els.validationErrors.innerHTML = "<ul>" + list.map((e) => `<li>${escapeHTML(e)}</li>`).join("") + "</ul>";
  els.validationErrors.classList.remove("hidden");
}

async function cancelCurrent() {
  if (!state.currentJobId) return;
  await postJSON("/api/cancel", { id: state.currentJobId });
}

// ---------------------------------------------------------------------------
// References
// ---------------------------------------------------------------------------

async function uploadFiles(files) {
  for (const file of files) {
    const buf = await file.arrayBuffer();
    await fetch("/api/upload", {
      method: "POST",
      headers: { "X-Filename": encodeURIComponent(file.name) },
      body: buf,
    });
  }
  await loadInputs();
}

function toggleRef(name) {
  const idx = state.selectedRefs.indexOf(name);
  if (idx >= 0) {
    state.selectedRefs.splice(idx, 1);
  } else {
    if (state.selectedRefs.length >= (state.config.max_refs || 16)) return;
    state.selectedRefs.push(name);
  }
  renderRefGallery();
}

async function deleteInput(name) {
  const idx = state.selectedRefs.indexOf(name);
  if (idx >= 0) state.selectedRefs.splice(idx, 1);
  await postJSON("/api/delete", { name, kind: "image" });
}

function renderRefGallery() {
  els.refGallery.innerHTML = "";
  els.refCount.textContent = `${state.selectedRefs.length}/${state.config.max_refs || 16}`;
  for (const item of state.inputs) {
    const div = document.createElement("div");
    div.className = "ref-item";
    const order = state.selectedRefs.indexOf(item.name);
    if (order >= 0) div.classList.add("selected");
    div.innerHTML = `
      <img src="/media/input/${encodeURIComponent(item.name)}" loading="lazy">
      ${order >= 0 ? `<span class="ref-order">$${order}</span>` : ""}
      <button class="ref-del" title="Delete">×</button>
    `;
    div.querySelector("img").onclick = () => toggleRef(item.name);
    div.querySelector(".ref-del").onclick = (e) => { e.stopPropagation(); deleteInput(item.name); };
    els.refGallery.appendChild(div);
  }
}

// ---------------------------------------------------------------------------
// Gallery (takes)
// ---------------------------------------------------------------------------

function renderGallery() {
  els.gallery.innerHTML = "";
  els.takeCount.textContent = `(${state.outputs.length})`;
  for (const item of state.outputs) {
    const meta = item.meta || {};
    const div = document.createElement("div");
    div.className = "take" + (item.name === state.selected ? " on" : "");
    div.dataset.name = item.name;
    const seed = meta.seed !== undefined && meta.seed !== null ? meta.seed : "?";
    const params = meta.params || {};
    div.innerHTML = `
      <img src="/media/output/${encodeURIComponent(item.name)}" loading="lazy">
      <div class="take-actions">
        <button class="use-ref" title="Use as reference">↩</button>
        <button class="download" title="Download">${DOWNLOAD_ICON}</button>
        <button class="del" title="Delete">×</button>
      </div>
      <div class="take-meta">seed ${seed} · ${params.width || "?"}×${params.height || "?"}</div>
    `;
    // The whole tile, not only its image: the meta line under it is part of
    // the take. The action buttons stop the event, as they already did.
    div.onclick = () => selectTake(item.name);
    div.querySelector(".use-ref").onclick = async (e) => {
      e.stopPropagation();
      await postJSON("/api/use-ref", { name: item.name });
    };
    div.querySelector(".download").onclick = (e) => {
      e.stopPropagation();
      window.open(`/download/${encodeURIComponent(item.name)}`, "_blank");
    };
    div.querySelector(".del").onclick = async (e) => {
      e.stopPropagation();
      await postJSON("/api/delete", { name: item.name, kind: "output" });
    };
    els.gallery.appendChild(div);
  }
}

// ---------------------------------------------------------------------------
// Queue / job progress
// ---------------------------------------------------------------------------

function renderQueue(queue, history) {
  els.queueList.innerHTML = "";
  const active = queue.find((j) => j.state === "running") || queue.find((j) => j.state === "queued");
  const previous = state.currentJobId;
  state.currentJobId = active ? active.id : null;
  // A different render from the one the preview was parked against follows
  // again: the choice not to watch was about that render, not every later one.
  if (state.currentJobId && state.currentJobId !== previous) state.followPreview = true;
  updateFollowButton();

  const visibleHistory = history.filter((j) => !state.queueClearedAt || !j.finished || j.finished > state.queueClearedAt);
  const rows = [...queue, ...visibleHistory.slice(0, 8)];
  if (rows.length === 0) {
    els.queueList.innerHTML = '<div class="queue-empty">Nothing yet</div>';
  }
  for (const job of rows) {
    const div = document.createElement("div");
    div.className = "queue-item";
    const prompt = (job.params && job.params.prompt) || "";
    div.innerHTML = `<div class="qi-top"><span>${job.state}</span><span>${escapeHTML(prompt.slice(0, 28))}</span></div>`;
    els.queueList.appendChild(div);
  }
  applyJobToPreview(active);
}

function applyJobToPreview(job) {
  const running = !!job && (job.state === "running" || job.state === "queued");
  els.progressRow.style.display = running ? "flex" : "none";
  els.generateBtn.disabled = running;
  els.cancelBtn.style.display = "";
  if (!running) return;
  const [cur, total] = job.progress || [0, 0];
  const pct = total > 0 ? Math.round((cur / total) * 100) : 0;
  els.progressFill.style.width = pct + "%";
  els.progressLabel.textContent = job.phase ? `${job.phase} ${cur}/${total}` : "starting…";
}

// A generation the terminal asked for has no job, so nothing else moves the
// progress row: without this the page looks idle for the minute iris takes.
// A job owns the row whenever there is one — it knows more, and it can be
// cancelled, which this cannot.
function showTakeInViewer(name) {
  els.previewImg.src = `/media/output/${encodeURIComponent(name)}?t=${Date.now()}`;
  els.previewImg.classList.remove("hidden");
  els.previewEmpty.classList.add("hidden");
  state.selected = name;
  markSelectedTake();
}

/**
 * selectTake shows a take in the preview, because a click was asked for it.
 *
 * It used to open the file in a new tab, which left the page behind to look
 * at one image. The preview is where an image is looked at here.
 *
 * Choosing one while a render is live stops the preview following that
 * render: a step frame arrives about every second and would otherwise undo
 * the click before the eye got there. The way back is the button the progress
 * row grows while that is true, and the next render follows again.
 */
function selectTake(name) {
  if (state.currentJobId) state.followPreview = false;
  showTakeInViewer(name);
  updateFollowButton();
}

function markSelectedTake() {
  for (const tile of els.gallery.querySelectorAll(".take")) {
    tile.classList.toggle("on", tile.dataset.name === state.selected);
  }
}

// Shown only while a render is live and the preview is not following it, so
// a take chosen mid-render is never a dead end.
function updateFollowButton() {
  els.followBtn.hidden = !state.currentJobId || state.followPreview;
}

function onInteractiveActivity(activity) {
  // A take the terminal asked for is shown the same way a finished render is.
  // This comes before the guard below: the take exists whatever else is going
  // on, and not showing it is the thing that looked broken.
  if (activity && activity.output && state.followPreview) showTakeInViewer(activity.output);
  if (state.currentJobId) return;
  const running = !!activity && activity.running;
  els.progressRow.style.display = running ? "flex" : "none";
  els.generateBtn.disabled = running;
  els.cancelBtn.style.display = running ? "none" : "";
  if (!running) return;
  const [cur, total] = activity.progress || [0, 0];
  els.progressFill.style.width = total > 0 ? Math.round((cur / total) * 100) + "%" : "0%";
  els.progressLabel.textContent = total > 0
    ? `${activity.phase || "working"} ${cur}/${total}`
    : `${activity.phase || "working"}…`;
}

function onJobUpdate(job) {
  applyJobToPreview(job.state === "running" || job.state === "queued" ? job : null);
  // Point the Render log tab at this render. helmstudio.js defines this once
  // it has connected; with no helmstudio there is no job log to follow, the
  // tab never appears, and this call does nothing.
  window.showHelmRenderLog?.(job);
  if (job.state === "done" && job.output && state.followPreview) showTakeInViewer(job.output);
  if (job.state === "failed" && job.error) {
    showErrors([job.error]);
  }
  refreshQueueFromServer();
}

let queueRefreshTimer = null;
function refreshQueueFromServer() {
  if (queueRefreshTimer) return;
  queueRefreshTimer = setTimeout(async () => {
    queueRefreshTimer = null;
    try {
      const data = await getJSON("/api/queue");
      renderQueue(data.queue, data.history);
    } catch (_) { /* ignore */ }
  }, 150);
}

// ---------------------------------------------------------------------------
// Live preview frames
// ---------------------------------------------------------------------------

function onPreviewFrame(payload) {
  if (state.followPreview) {
    els.previewImg.src = payload.url;
    els.previewImg.classList.remove("hidden");
    els.previewEmpty.classList.add("hidden");
    // A live frame is the render's, not a take: nothing in the rail is
    // showing, so nothing in the rail is marked.
    state.selected = null;
    markSelectedTake();
  }
  if (payload.step >= 0) {
    els.progressRow.style.display = "flex";
    els.progressLabel.textContent = `Step ${payload.step}`;
  }
}

// ---------------------------------------------------------------------------
// Terminal
// ---------------------------------------------------------------------------

function appendTerminal(line) {
  els.terminalLog.textContent += line + "\n";
  els.terminalLog.scrollTop = els.terminalLog.scrollHeight;
}

// The line goes to interactive iris and nowhere else. There is no shell here:
// a line iris does not know is iris's error to report, not a command to run.
async function sendTerminalInput() {
  const text = els.terminalInput.value.trim();
  if (!text) return;
  els.terminalInput.value = "";
  try {
    await postJSON("/api/interactive/input", { line: text });
  } catch (err) {
    appendTerminal("error: " + err.message);
  }
}

// ---------------------------------------------------------------------------
// Interactive mode
// ---------------------------------------------------------------------------

async function loadInteractive() {
  const params = collectParams();
  try {
    await postJSON("/api/interactive/load", params);
    state.interactiveLoaded = true;
    els.interactiveState.textContent = "loaded";
  } catch (err) {
    showErrors([err.message]);
  }
}

async function stopInteractive() {
  await postJSON("/api/interactive/stop", {});
  state.interactiveLoaded = false;
  els.interactiveState.textContent = "";
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

function connectSSE() {
  if (state.es) state.es.close();
  const es = new EventSource("/api/events");
  state.es = es;
  es.onopen = () => setConn(true);
  es.onerror = () => { setConn(false); setTimeout(connectSSE, 2000); es.close(); };
  es.onmessage = (evt) => {
    let msg;
    try { msg = JSON.parse(evt.data); } catch (_) { return; }
    handleEvent(msg.kind, msg.payload);
  };
}

function setConn(ok) {
  els.connStatus.classList.toggle("on", ok);
  els.connStatus.classList.toggle("off", !ok);
  els.connLabel.textContent = ok ? "live" : "reconnecting…";
}

function handleEvent(kind, payload) {
  switch (kind) {
    case "hello":
      renderQueue(payload.queue, payload.history);
      state.outputs = payload.outputs || [];
      renderGallery();
      state.inputs = payload.inputs || [];
      renderRefGallery();
      els.terminalLog.textContent = payload.terminal_log || "";
      els.terminalLog.scrollTop = els.terminalLog.scrollHeight;
      break;
    case "queue":
      getJSON("/api/queue").then((d) => renderQueue(d.queue, d.history)).catch(() => {});
      break;
    case "job":
      onJobUpdate(payload);
      break;
    case "outputs":
      state.outputs = payload;
      renderGallery();
      break;
    case "inputs":
      state.inputs = payload;
      renderRefGallery();
      break;
    case "preview":
      onPreviewFrame(payload);
      break;
    case "terminal":
      if (payload.line) appendTerminal(payload.line);
      break;
    case "interactive":
      onInteractiveActivity(payload);
      break;
    default:
      break;
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function wireEvents() {
  els.generateBtn.onclick = generate;
  els.cancelBtn.onclick = cancelCurrent;
  // Back to the render. The next frame it draws puts it in the preview; there
  // is nothing to show until then, so nothing is drawn here.
  els.followBtn.onclick = () => {
    state.followPreview = true;
    state.selected = null;
    markSelectedTake();
    updateFollowButton();
  };

  els.scheduleSelect.onchange = updatePowerAlphaVisibility;

  for (const btn of els.runModeSeg.querySelectorAll("button")) {
    btn.onclick = () => setRunMode(btn.dataset.value);
  }
  els.interactiveLoadBtn.onclick = loadInteractive;
  els.interactiveStopBtn.onclick = stopInteractive;
  // The frame takes the shown image's ratio, so a 1024×768 fills it instead of
  // sitting letterboxed in a square.
  els.previewImg.onload = () => {
    const { naturalWidth: w, naturalHeight: h } = els.previewImg;
    if (w && h) els.previewFrame.style.setProperty("--r", String(w / h));
  };

  els.dropZone.onclick = () => els.fileInput.click();
  els.fileInput.onchange = () => uploadFiles(els.fileInput.files);
  els.dropZone.ondragover = (e) => { e.preventDefault(); els.dropZone.classList.add("drag"); };
  els.dropZone.ondragleave = () => els.dropZone.classList.remove("drag");
  els.dropZone.ondrop = (e) => {
    e.preventDefault();
    els.dropZone.classList.remove("drag");
    uploadFiles(e.dataTransfer.files);
  };

  els.terminalSendBtn.onclick = sendTerminalInput;
  els.terminalInput.onkeydown = (e) => { if (e.key === "Enter") sendTerminalInput(); };
  els.termClearBtn.onclick = () => { els.terminalLog.textContent = ""; };

  els.queueClearBtn.onclick = async () => {
    state.queueClearedAt = Date.now() / 1000;
    try {
      const d = await getJSON("/api/queue");
      renderQueue(d.queue, d.history);
    } catch (_) { /* ignore */ }
  };

  els.sessionSelect.onchange = () => loadSessionSetting(els.sessionSelect.value);
  els.sessionNewBtn.onclick = async () => {
    const name = await openSessionNameModal({ title: "New session", label: "Session name", placeholder: "session-2" });
    if (!name) return;
    await postJSON("/api/session/activate", { name });
    await loadSessionSetting(name);
    await loadSessions();
    await loadInputs();
    await loadOutputs();
  };
  els.sessionDupBtn.onclick = async () => {
    const name = await openSessionNameModal({ title: "Duplicate session", label: "Duplicate as", value: state.config.session + "-copy" });
    if (name === null) return; // cancelled
    const res = await postJSON("/api/session/duplicate", { name: state.config.session, as: name });
    await loadSessionSetting(res.name);
    await loadSessions();
    await loadInputs();
    await loadOutputs();
  };
  els.sessionDelBtn.onclick = async () => {
    const ok = await openConfirmModal({
      title: "Delete session?",
      message: `Delete session "${state.config.session}"? This removes its inputs and outputs.`,
      okLabel: "Delete",
    });
    if (!ok) return;
    const res = await postJSON("/api/session/delete", { name: state.config.session });
    await loadSessionSetting(res.name);
    await loadSessions();
    await loadInputs();
    await loadOutputs();
  };

  wirePaths();
}

// ---------------------------------------------------------------------------
// Generic name-prompt / confirm modals (native prompt()/confirm() can be
// blocked or silently no-op depending on how the page is embedded, so every
// user-input dialog in this app is a real in-page modal instead).
// ---------------------------------------------------------------------------

function openSessionNameModal({ title, label, placeholder, value }) {
  return new Promise((resolve) => {
    els.sessionNameModalTitle.textContent = title;
    els.sessionNameModalLabel.textContent = label;
    els.sessionNameInput.placeholder = placeholder || "";
    els.sessionNameInput.value = value || "";
    els.sessionNameModalError.classList.add("hidden");
    els.sessionNameModal.classList.remove("hidden");
    els.sessionNameInput.focus();
    els.sessionNameInput.select();

    const cleanup = () => {
      els.sessionNameModal.classList.add("hidden");
      els.sessionNameModalSave.onclick = null;
      els.sessionNameModalCancel.onclick = null;
      els.sessionNameInput.onkeydown = null;
    };
    els.sessionNameModalSave.onclick = () => {
      const v = els.sessionNameInput.value.trim();
      cleanup();
      resolve(v);
    };
    els.sessionNameModalCancel.onclick = () => { cleanup(); resolve(null); };
    els.sessionNameInput.onkeydown = (e) => { if (e.key === "Enter") els.sessionNameModalSave.click(); };
  });
}

function openConfirmModal({ title, message, okLabel }) {
  return new Promise((resolve) => {
    els.confirmModalTitle.textContent = title;
    els.confirmModalMessage.textContent = message;
    els.confirmModalOk.textContent = okLabel || "Confirm";
    els.confirmModal.classList.remove("hidden");

    const cleanup = () => {
      els.confirmModal.classList.add("hidden");
      els.confirmModalOk.onclick = null;
      els.confirmModalCancel.onclick = null;
    };
    els.confirmModalOk.onclick = () => { cleanup(); resolve(true); };
    els.confirmModalCancel.onclick = () => { cleanup(); resolve(false); };
  });
}

// ---------------------------------------------------------------------------
// Paths popover + iris/model path modals (mirrors h3c-studio's settings UI)
// ---------------------------------------------------------------------------

function wirePaths() {
  els.pathsBtn.onclick = (e) => {
    e.stopPropagation();
    els.pathPanel.classList.toggle("hidden");
  };
  document.addEventListener("click", (e) => {
    if (!els.pathPanel.classList.contains("hidden") && !els.pathPanel.contains(e.target) && e.target !== els.pathsBtn) {
      els.pathPanel.classList.add("hidden");
    }
  });

  els.changeIrisBtn.onclick = () => {
    els.irisPathInput.value = state.config.iris;
    els.irisModalError.classList.add("hidden");
    els.irisModal.classList.remove("hidden");
    els.pathPanel.classList.add("hidden");
    els.irisPathInput.focus();
  };
  els.irisModalCancel.onclick = () => els.irisModal.classList.add("hidden");
  els.irisModalSave.onclick = async () => {
    const path = els.irisPathInput.value.trim();
    try {
      const data = await postJSON("/api/iris", { iris: path });
      state.config.iris = data.iris;
      renderPaths();
      els.irisModal.classList.add("hidden");
    } catch (err) {
      els.irisModalError.textContent = err.message;
      els.irisModalError.classList.remove("hidden");
    }
  };
  els.irisPathInput.onkeydown = (e) => { if (e.key === "Enter") els.irisModalSave.click(); };

  els.changeModelBtn.onclick = () => {
    els.modelPathInput.value = state.config.model;
    els.modelModalError.classList.add("hidden");
    els.modelModal.classList.remove("hidden");
    els.pathPanel.classList.add("hidden");
    els.modelPathInput.focus();
  };
  els.modelModalCancel.onclick = () => els.modelModal.classList.add("hidden");
  els.modelModalSave.onclick = async () => {
    const path = els.modelPathInput.value.trim();
    try {
      const data = await postJSON("/api/model", { model: path });
      state.config.model = data.model;
      state.config.model_label = data.model_label;
      state.config.model_valid = true;
      renderPaths();
      els.modelModal.classList.add("hidden");
    } catch (err) {
      els.modelModalError.textContent = err.message;
      els.modelModalError.classList.remove("hidden");
    }
  };
  els.modelPathInput.onkeydown = (e) => { if (e.key === "Enter") els.modelModalSave.click(); };
}

init();
