"use strict";

const state = {
  config: null,
  inputs: [],
  outputs: [],
  selectedRefs: [], // ordered list of input filenames
  ratio: "", // a tile of RATIOS; "" is Auto
  runMode: "oneshot",
  interactiveLoaded: false,
  currentJobId: null,
  // The take showing in the preview, and whether a live render may take the
  // preview over. Picking a take while a render is running stops it following
  // — otherwise the next step frame, a second away, would undo the click.
  selected: null,
  followPreview: true,
  // The render iris is working on, whichever session it belongs to. A preview
  // frame does not say whose it is, so this is what says.
  runningJob: null,
  queueHTML: null, // what the queue panel last drew
  queueClearedAt: 0, // finished-job entries at/before this unix time are hidden from the queue panel
  // What the stage strip draws, in the order it is looked at: the render in
  // hand, one the terminal asked for, and the one that last ended here, which
  // stays until the form says what comes next.
  stripJob: null,
  terminalRun: null,
  lastRun: null,
  es: null,
};

const $ = (id) => document.getElementById(id);

// Whether a job belongs to the session on screen. The queue is global; what a
// render produces is shown only in its own session.
const isMine = (job) => job?.params?.session_name === state.config.session;

// Whether iris is working on a job. One being cancelled still is, until it ends.
const isLive = (job) => job.state === "running" || job.state === "cancelling";

// A render's size as far as its params know. One that follows a ratio or a
// reference has none until its take exists, and then the server fills them in.
const sizeLabel = (p) => (p.width && p.height ? `${p.width}×${p.height}` : "auto");

// The session goes in the URL so a file means the same file whichever session
// the server has active. It is the page's and not the sidecar's: everything the
// page lists is this session's, and a duplicated take's sidecar names the
// session it was copied from.
const mediaURL = (route, name) =>
  `${route}/${encodeURIComponent(name)}?session=${encodeURIComponent(state.config.session)}`;

const els = {
  sessionSelect: $("sessionSelect"),
  modelDisplay: $("modelDisplay"),
  modelLabel: $("modelLabel"),
  modelBadge: $("modelBadge"),
  promptInput: $("promptInput"),
  widthInput: $("widthInput"),
  heightInput: $("heightInput"),
  ratioSrc: $("ratioSrc"),
  ratios: $("ratios"),
  planDims: $("planDims"),
  planMP: $("planMP"),
  planGrid: $("planGrid"),
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
  stageOverlay: $("stageOverlay"),
  stages: $("stages"),
  stageRow: $("stageRow"),
  stageLabel: $("stageLabel"),
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

// The tiles, in order. What size each stands for is the server's to say.
const RATIOS = ["", "1:1", "4:3", "3:4", "3:2", "2:3", "16:9", "9:16"];

// The strip's columns: the server's word for each stage, and the page's.
const STAGES = [["Loading", "Load"], ["Encoding", "Encode"], ["Denoising", "Denoise"], ["Decoding", "Decode"], ["Saving", "Save"]];

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
  buildRatioTiles();
  buildStages();
  wireEvents();
  await loadConfig();
  await openSession(state.config.session);
  // Elapsed time moves whether or not iris has anything to say.
  setInterval(() => { if (stripView() && isLive(stripView())) renderStrip(); }, 1000);
}

function buildRatioTiles() {
  for (const key of RATIOS) {
    const [w, h] = key ? key.split(":") : [1, 1];
    const scale = 22 / Math.max(w, h);
    const btn = document.createElement("button");
    btn.className = key ? "ratio" : "ratio auto";
    btn.dataset.value = key;
    btn.innerHTML = `<i style="width:${Math.round(w * scale)}px;height:${Math.round(h * scale)}px"></i>${key || "Auto"}`;
    btn.onclick = () => { setRatio(key); schedulePlan(); scheduleSave(); };
    els.ratios.appendChild(btn);
  }
}

function setRatio(key) {
  state.ratio = key;
  for (const btn of els.ratios.children) {
    btn.setAttribute("aria-pressed", String(btn.dataset.value === key));
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

/**
 * openSession puts a session on screen, and is the one way that happens.
 *
 * The GET in loadSessionSetting is what activates it on the server. Connecting
 * again comes after that: the hello it gets back carries the takes, references,
 * queue and terminal of the session now active, and there is no other route to
 * a session's terminal log.
 */
async function openSession(name) {
  clearTimeout(saveTimer);
  await loadSessionSetting(name);
  // What was showing belongs to the session just left.
  state.selected = null;
  state.lastRun = null;
  els.previewImg.classList.add("hidden");
  els.previewEmpty.classList.remove("hidden");
  els.validationErrors.classList.add("hidden");
  window.showHelmRenderLog?.(null);
  await loadSessions();
  connectSSE();
}

async function loadSessionSetting(name) {
  const data = await getJSON(`/api/session/${encodeURIComponent(name)}`);
  state.config.session = data.session_name || name;
  els.promptInput.value = data.prompt || "";
  setRatio(data.ratio || "");
  els.widthInput.value = data.width || "";
  els.heightInput.value = data.height || "";
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

// ---------------------------------------------------------------------------
// Form <-> params
// ---------------------------------------------------------------------------

function collectParams() {
  return {
    session_name: state.config.session,
    label: "",
    prompt: els.promptInput.value.trim(),
    ratio: state.ratio,
    width: parseInt(els.widthInput.value, 10) || 0,
    height: parseInt(els.heightInput.value, 10) || 0,
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

// Saves the form a moment after it stops changing. The session travels in the
// params, so a save that lands late still lands in the session it came from;
// openSession drops one still waiting, whose form is about to be replaced.
let saveTimer = null;
function scheduleSave() {
  clearTimeout(saveTimer);
  // Every change to the form lands here, so this is where the strip turns from
  // the last render to the next.
  state.lastRun = null;
  renderStrip();
  saveTimer = setTimeout(() => {
    postJSON("/api/session/save", collectParams()).catch((err) => showErrors([err.message]));
  }, 600);
}

// Asks the server what size the form comes to, a moment after it stops changing.
// A change makes any answer still on its way out of date, whichever lands first.
let planTimer = null;
let planSeq = 0;
function schedulePlan() {
  clearTimeout(planTimer);
  const seq = ++planSeq;
  planTimer = setTimeout(async () => {
    const plan = await postJSON("/api/plan", collectParams()).catch(() => ({ source: "unknown" }));
    if (seq === planSeq) showPlan(plan);
  }, 150);
}

function showPlan(plan) {
  const known = plan.width && plan.height;
  const followed = !state.ratio && plan.source !== "width × height";
  els.ratioSrc.textContent = followed ? `auto · ${plan.source}` : plan.source;
  els.planDims.textContent = known ? `${plan.width} × ${plan.height}` : "—";
  els.planMP.textContent = known ? ((plan.width * plan.height) / 1e6).toFixed(2) : "—";
  // Only a typed side can be off the grid: a lone reference's own size is
  // cropped back on purpose.
  const { width, height } = collectParams();
  const ok = width % state.config.dim_step === 0 && height % state.config.dim_step === 0;
  els.planGrid.textContent = ok ? "ok" : "not a multiple";
  els.planGrid.classList.toggle("warn", !ok);
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

async function cancelJob(id) {
  try {
    await postJSON("/api/cancel", { id });
  } catch (err) {
    showErrors([err.message]);
  }
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
  scheduleSave();
}

async function deleteInput(name) {
  // The server refuses a reference a waiting render uses, so the selection
  // only changes once the file has gone.
  try {
    await postJSON("/api/delete", { name, kind: "image" });
  } catch (err) {
    showErrors([err.message]);
    return;
  }
  const idx = state.selectedRefs.indexOf(name);
  if (idx < 0) return;
  state.selectedRefs.splice(idx, 1);
  renderRefGallery();
  scheduleSave();
}

function renderRefGallery() {
  schedulePlan(); // the first reference can decide the size, and a new session lands here too
  els.refGallery.innerHTML = "";
  els.refCount.textContent = `${state.selectedRefs.length}/${state.config.max_refs || 16}`;
  for (const item of state.inputs) {
    const div = document.createElement("div");
    div.className = "ref-item";
    const order = state.selectedRefs.indexOf(item.name);
    if (order >= 0) div.classList.add("selected");
    div.innerHTML = `
      <img src="${mediaURL("/media/input", item.name)}" loading="lazy">
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
      <img src="${mediaURL("/media/output", item.name)}" loading="lazy">
      <div class="take-actions">
        <button class="use-ref" title="Use as reference">↩</button>
        <button class="download" title="Download">${DOWNLOAD_ICON}</button>
        <button class="del" title="Delete">×</button>
      </div>
      <div class="take-meta">seed ${seed} · ${sizeLabel(params)}</div>
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
      window.open(mediaURL("/download", item.name), "_blank");
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

function queueRowHTML(job) {
  const p = job.params || {};
  const seed = job.seed ?? p.seed;
  // The reason comes first, to clear the ellipsis. It is here because the error
  // panel is only for the session on screen, and this row is for all of them.
  const text = `${job.error ? job.error + " · " : ""}${p.prompt || ""} · ${sizeLabel(p)} · ${seed >= 0 ? seed : "random"}`;
  const status = isMine(job) ? job.state : `${job.state} · ${p.session_name}`;
  const waiting = job.state === "queued" || job.state === "running";
  return `<div class="queue-item">
    <div class="qi-top"><span>${escapeHTML(status)}</span>${waiting ? `<button class="link-btn" data-cancel="${escapeHTML(job.id)}">cancel</button>` : ""}</div>
    <div class="qi-text" title="${escapeHTML(text)}">${escapeHTML(text)}</div>
  </div>`;
}

function renderQueue(queue, history) {
  state.runningJob = queue.find(isLive) || null;
  const mine = queue.filter(isMine);
  const active = mine.find(isLive) || mine[0];
  const previous = state.currentJobId;
  state.currentJobId = active ? active.id : null;
  // A different render from the one the preview was parked against follows
  // again: the choice not to watch was about that render, not every later one.
  if (state.currentJobId && state.currentJobId !== previous) state.followPreview = true;
  updateFollowButton();

  // A render cancelled before it started never finished, so no finish time is
  // not a reason to survive clear.
  const visibleHistory = history.filter((j) => !state.queueClearedAt || j.finished > state.queueClearedAt);
  const rows = [...queue, ...visibleHistory.slice(0, 8)];
  const html = rows.map(queueRowHTML).join("") || '<div class="queue-empty">Nothing yet</div>';
  // This runs for every line a render prints, and a row rebuilt between press
  // and release loses the click on its cancel.
  if (html !== state.queueHTML) {
    state.queueHTML = html;
    els.queueList.innerHTML = html;
  }
  applyJobToPreview(active);
}

function applyJobToPreview(job) {
  state.stripJob = job || null;
  renderStrip();
}

function buildStages() {
  for (const [, name] of STAGES) {
    const col = document.createElement("div");
    col.className = "stage";
    col.innerHTML = `<div class="bar"><i></i></div>${name}<small></small>`;
    els.stages.appendChild(col);
  }
}

const stripView = () => state.stripJob || state.terminalRun || state.lastRun;

// Seconds as they read in the strip's one line of text.
function clock(seconds) {
  const s = Math.max(0, Math.round(seconds));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
}

// What each column says when it has no time to show: what this render uses, or
// with none in hand, what the next one will.
function stagePlan(p, total) {
  const refs = p.refs?.length || 0;
  const steps = total || p.steps;
  return {
    Loading: p.run_mode === "interactive" && state.interactiveLoaded ? "in memory" : p.mmap === false ? "no mmap" : "mmap",
    Encoding: refs ? `text + ${refs} ref${refs > 1 ? "s" : ""}` : "text",
    Denoising: steps ? `${steps} steps` : "auto",
    Decoding: "VAE",
    Saving: "PNG",
  };
}

// The strip's line of text. An estimate wants two finished steps: the first
// carries the warm-up, and on one-shot FLUX.3 its line arrives as it starts.
function stageText(view, now) {
  if (view.state === "queued") return "Queued";
  const [cur, total] = view.progress || [0, 0];
  if (!isLive(view)) {
    const name = view.state[0].toUpperCase() + view.state.slice(1);
    return view.started && view.finished ? `${name} · ${clock(view.finished - view.started)}` : name;
  }
  const parts = [view.state === "cancelling" ? "Cancelling" : view.phase || "Starting", clock(now - view.started)];
  const denoise = view.stages?.Denoising;
  if (view.phase === "Denoising" && denoise?.state === "now" && cur >= 2 && total > cur) {
    parts.push(`ETA ${clock(((now - denoise.started) / cur) * (total - cur))}`);
  }
  return parts.join(" · ");
}

/**
 * renderStrip draws the stage strip, and everything it needs is on the job's
 * summary, so a page opened half way through a render draws the same strip.
 * With nothing to show it stays, quiet, and says what the next render will use.
 */
function renderStrip() {
  const view = stripView();
  const [cur, total] = view?.progress || [0, 0];
  const plan = stagePlan(view?.params || collectParams(), total);
  STAGES.forEach(([phase], i) => {
    const mark = view?.stages?.[phase];
    const col = els.stages.children[i];
    col.className = `stage ${mark?.state || ""}`;
    // Steps say how far, where any other stage can only say it is under way.
    const filling = phase === "Denoising" && mark && mark.state !== "done";
    col.querySelector("i").style.width = filling && total > 0 ? `${(cur / total) * 100}%` : filling ? "0" : "";
    const timed = mark && mark.state !== "now" && mark.state !== "skip";
    col.querySelector("small").textContent = mark?.state === "skip" ? "skipped" : timed ? `${mark.seconds.toFixed(1)}s` : plan[phase];
  });
  const live = !!view && isLive(view);
  els.stageRow.classList.toggle("hidden", !view);
  if (view) els.stageLabel.textContent = stageText(view, Date.now() / 1000);
  // A render the terminal asked for has no job to cancel, and one being
  // cancelled is past asking.
  els.cancelBtn.hidden = !view?.id || !(view.state === "running" || view.state === "queued");
  const counting = live && view.phase === "Denoising" && total > 0 && state.followPreview;
  els.stageOverlay.classList.toggle("hidden", !counting);
  if (counting) els.stageOverlay.textContent = `step ${cur}/${total}`;
}

function showTakeInViewer(name) {
  els.previewImg.src = `${mediaURL("/media/output", name)}&t=${Date.now()}`;
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
 * the click before the eye got there. The way back is the button the stage
 * row grows while that is true, and the next render follows again.
 */
function selectTake(name) {
  if (state.currentJobId) state.followPreview = false;
  showTakeInViewer(name);
  updateFollowButton();
  renderStrip();
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

// A generation the terminal asked for has no job, so nothing else moves the
// strip: without this the page looks idle for the minute iris takes. A job comes
// first in stripView whenever there is one — it knows more, and it can be
// cancelled, which this cannot.
function onInteractiveActivity(activity) {
  // The REPL can go with nobody here asking: a one-shot render unloads it, and
  // it can die.
  if (activity && activity.loaded === false) {
    state.interactiveLoaded = false;
    els.interactiveState.textContent = "";
  }
  // A take the terminal asked for is shown the same way a finished render is.
  // This comes before the guard below: the take exists whatever else is going
  // on, and not showing it is the thing that looked broken.
  if (activity && activity.output && state.followPreview) showTakeInViewer(activity.output);
  state.terminalRun = activity?.running ? { ...activity, state: "running", params: { run_mode: "interactive" } } : null;
  renderStrip();
}

function onJobUpdate(job) {
  state.runningJob = isLive(job) ? job : null;
  refreshQueueFromServer();
  if (!isMine(job)) return;
  const waiting = isLive(job) || job.state === "queued";
  if (!waiting) state.lastRun = job;
  applyJobToPreview(waiting ? job : null);
  // Point the Render log tab at this render. helmstudio.js defines this once
  // it has connected; with no helmstudio there is no job log to follow, the
  // tab never appears, and this call does nothing.
  window.showHelmRenderLog?.(job);
  if (job.state === "done" && job.output && state.followPreview) showTakeInViewer(job.output);
  if (job.state === "failed" && job.error) {
    showErrors([job.error]);
  }
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
  // With no job running the frame is the terminal's, and that is this session's.
  if (state.runningJob && !isMine(state.runningJob)) return;
  if (state.followPreview) {
    els.previewImg.src = payload.url;
    els.previewImg.classList.remove("hidden");
    els.previewEmpty.classList.add("hidden");
    // A live frame is the render's, not a take: nothing in the rail is
    // showing, so nothing in the rail is marked.
    state.selected = null;
    markSelectedTake();
  }
}

// ---------------------------------------------------------------------------
// Terminal
// ---------------------------------------------------------------------------

const TERMINAL_LINES = 2000;

// One text node a line, so a new line costs a node and the oldest goes with it,
// rather than all of it being written out again.
function appendTerminal(line) {
  els.terminalLog.append(line + "\n");
  if (els.terminalLog.childNodes.length > TERMINAL_LINES) els.terminalLog.firstChild.remove();
  els.terminalLog.scrollTop = els.terminalLog.scrollHeight;
}

function setTerminal(text) {
  const lines = text.split("\n");
  lines.pop(); // what follows the last newline, which is nothing
  els.terminalLog.replaceChildren(...lines.slice(-TERMINAL_LINES).map((line) => line + "\n"));
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
    renderStrip();
  } catch (err) {
    showErrors([err.message]);
  }
}

async function stopInteractive() {
  await postJSON("/api/interactive/stop", {});
  state.interactiveLoaded = false;
  els.interactiveState.textContent = "";
  renderStrip();
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
      state.terminalRun = null; // an ending missed while away would never come
      renderQueue(payload.queue, payload.history);
      state.outputs = payload.outputs || [];
      renderGallery();
      state.inputs = payload.inputs || [];
      renderRefGallery();
      setTerminal(payload.terminal_log || "");
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
      // A line naming no session is the terminal's own, which is this one's.
      if (payload.line && (!payload.session || payload.session === state.config.session)) {
        appendTerminal(payload.line);
      }
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
  els.cancelBtn.onclick = () => cancelJob(state.currentJobId);
  // Back to the render. The next frame it draws puts it in the preview; there
  // is nothing to show until then, so nothing is drawn here.
  els.followBtn.onclick = () => {
    state.followPreview = true;
    state.selected = null;
    markSelectedTake();
    updateFollowButton();
    renderStrip();
  };

  els.scheduleSelect.onchange = updatePowerAlphaVisibility;
  // Every field in the panel fires this. What is not a field saves for itself:
  // ratio tiles, run mode and the reference selection.
  document.querySelector(".params-panel").addEventListener("input", scheduleSave);
  els.widthInput.oninput = els.heightInput.oninput = schedulePlan;

  for (const btn of els.runModeSeg.querySelectorAll("button")) {
    btn.onclick = () => { setRunMode(btn.dataset.value); scheduleSave(); };
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

  els.queueList.onclick = (e) => {
    const btn = e.target.closest("[data-cancel]");
    if (btn) cancelJob(btn.dataset.cancel);
  };
  els.queueClearBtn.onclick = async () => {
    state.queueClearedAt = Date.now() / 1000;
    try {
      const d = await getJSON("/api/queue");
      renderQueue(d.queue, d.history);
    } catch (_) { /* ignore */ }
  };

  els.sessionSelect.onchange = () => openSession(els.sessionSelect.value);
  els.sessionNewBtn.onclick = () => openSessionNameModal({
    title: "New session",
    label: "Session name",
    placeholder: "session-2",
    submit: async (name) => {
      if (!name) return;
      const res = await postJSON("/api/session/activate", { name });
      await openSession(res.name);
    },
  });
  els.sessionDupBtn.onclick = () => openSessionNameModal({
    title: "Duplicate session",
    label: "Duplicate as",
    value: state.config.session + "-copy",
    submit: async (name) => {
      const res = await postJSON("/api/session/duplicate", { name: state.config.session, as: name });
      await openSession(res.name);
    },
  });
  els.sessionDelBtn.onclick = async () => {
    const ok = await openConfirmModal({
      title: "Delete session?",
      message: `Delete session "${state.config.session}"? This removes its inputs and outputs.`,
      okLabel: "Delete",
    });
    if (!ok) return;
    clearTimeout(saveTimer); // or it fires into a directory that is gone
    try {
      const res = await postJSON("/api/session/delete", { name: state.config.session });
      await openSession(res.name);
    } catch (err) {
      showErrors([err.message]);
    }
  };

  wirePaths();
}

// ---------------------------------------------------------------------------
// Generic name-prompt / confirm modals (native prompt()/confirm() can be
// blocked or silently no-op depending on how the page is embedded, so every
// user-input dialog in this app is a real in-page modal instead).
// ---------------------------------------------------------------------------

// submit does the work with the name. The modal stays open on what it throws
// and shows it, so a name already taken can be changed where it was typed.
function openSessionNameModal({ title, label, placeholder, value, submit }) {
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
  els.sessionNameModalSave.onclick = async () => {
    try {
      await submit(els.sessionNameInput.value.trim());
      cleanup();
    } catch (err) {
      els.sessionNameModalError.textContent = err.message;
      els.sessionNameModalError.classList.remove("hidden");
    }
  };
  els.sessionNameModalCancel.onclick = cleanup;
  els.sessionNameInput.onkeydown = (e) => { if (e.key === "Enter") els.sessionNameModalSave.click(); };
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
      schedulePlan(); // a family sizes a reference its own way
      els.modelModal.classList.add("hidden");
    } catch (err) {
      els.modelModalError.textContent = err.message;
      els.modelModalError.classList.remove("hidden");
    }
  };
  els.modelPathInput.onkeydown = (e) => { if (e.key === "Enter") els.modelModalSave.click(); };
}

init();
