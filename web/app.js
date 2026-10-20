// Uncharted Spectra — app.js
// Client logic. Server API base is same origin (port 8484).

"use strict";

const $ = (sel) => document.querySelector(sel);

const els = {
  meta:        $("#metaLine"),
  drop:        $("#dropZone"),
  fileInput:   $("#fileInput"),
  browse:      $("#browseBtn"),
  play:        $("#playBtn"),
  undo:        $("#btnUndo"),
  revert:      $("#btnRevert"),
  exportWav:   $("#btnExportWav"),
  exportBmp:   $("#btnExportBmp"),
  presetSave:  $("#btnPresetSave"),
  presetLoad:  $("#btnPresetLoad"),
  presetFile:  $("#presetFile"),
  scan:        $("#btnScan"),
  scanKeep:    $("#btnScanKeep"),
  scanStop:    $("#btnScanStop"),
  scanMeter:   $("#scanMeter"),
  scanScore:   $("#scanScore"),
  scanStats:   $("#scanStats"),
  scanSource:  $("#scanSource"),
  scanSourceFile: $("#scanSourceFile"),
  scanSourceAdd:  $("#btnScanSourceAdd"),
  wave:        $("#waveCanvas"),
  waveCtx:     $("#waveCanvas")?.getContext("2d") || {},
  waveInfo:    $("#waveInfo"),
  waveEmpty:   $("#waveEmpty"),
  image:       $("#imageView"),
  imageInfo:   $("#imageInfo"),
  imageEmpty:  $("#imageEmpty"),
  tabs:        $("#moduleTabs"),
  moduleList:  $("#moduleList"),
  modal:       $("#modalBackdrop"),
  modalTitle:  $("#modalTitle"),
  modalBody:   $("#modalBody"),
  modalClose:  $("#modalClose"),
  modalCancel: $("#modalCancel"),
  modalApply:  $("#modalApply"),
  commitRotary: $("#btnCommitRotary"),
  status:      $("#statusLine"),
};

let audio = null;
let state = null;          // last /api/state response
let uploadedBMPBlobURL = null;
let playheadFrac = -1;     // waveform playhead, 0..1 (-1 = hidden)

// ------------------------------------------------------------- utilities --

function setStatus(msg, dim) {
  const el = els.status;
  if (!el) return;
  el.textContent = msg;
  el.classList.toggle("dim", !!dim);
}

function fmtBytes(n) {
  if (n >= 1024 * 1024) return (n / 1048576).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(1) + " kB";
  return n + " B";
}

function fmtDur(sec) {
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  const ms = Math.floor((sec % 1) * 100);
  return `${m}:${String(s).padStart(2, "0")}.${String(ms).padStart(2, "0")}`;
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(body.error || res.statusText), { status: res.status });
  return body;
}

// ---------------------------------------------------------------- upload --

if (els.drop) {
  els.drop.addEventListener("click", () => els.fileInput.click());
  els.browse.addEventListener("click", (e) => { e.stopPropagation(); els.fileInput.click(); });

  ["dragenter", "dragover"].forEach(ev =>
    els.drop.addEventListener(ev, (e) => { e.preventDefault(); els.drop.classList.add("dragover"); }));
  ["dragleave", "drop"].forEach(ev =>
    els.drop.addEventListener(ev, (e) => { e.preventDefault(); els.drop.classList.remove("dragover"); }));

  els.drop.addEventListener("drop", (e) => {
    const f = e.dataTransfer.files[0];
    if (f) uploadFile(f);
  });

  els.fileInput.addEventListener("change", () => {
    const f = els.fileInput.files[0];
    if (f) uploadFile(f);
    els.fileInput.value = "";
  });
}

async function uploadFile(f) {
  setStatus(`uploading ${f.name} …`);
  try {
    const fd = new FormData();
    fd.append("file", f);
    const res = await api("/api/upload", { method: "POST", body: fd });

    // Right box: for BMP uploads show the original file; for WAV uploads
    // the server-generated BMP will be fetched during refresh().
    if (res.kind === "bmp") {
      if (uploadedBMPBlobURL) URL.revokeObjectURL(uploadedBMPBlobURL);
      uploadedBMPBlobURL = URL.createObjectURL(f);
    } else if (uploadedBMPBlobURL) {
      URL.revokeObjectURL(uploadedBMPBlobURL);
      uploadedBMPBlobURL = null;
    }

    stopAudio();
    playheadFrac = -1;
    stateMutatedSinceUpload = false;
    ensureAudio().src = "/audio/current.wav?t=" + Date.now();
    await refresh(false);
    await loadScanSources();
    setStatus(`loaded ${f.name}`);
  } catch (err) {
    setStatus(`upload failed: ${err.message}`);
  }
}

// --------------------------------------------------------------- refresh --

async function refresh(afterMutation) {
  state = await api("/api/state");
  renderState();
  if (afterMutation) reloadAudio();
}

function renderState() {
  if (!state || !state.hasState) return;

  els.meta.textContent =
    `${state.fileName} · ${state.sampleRate} Hz · ${state.bitsPerSample}-bit · ${state.channels}ch · ${fmtDur(state.durationSec)} · ${fmtBytes(state.bytes)}`;

  if (els.waveInfo) els.waveInfo.textContent = `${fmtDur(state.durationSec)} · ${state.imageWidth}×${state.imageHeight}px`;
  if (els.imageInfo) els.imageInfo.textContent = `PCM ${fmtBytes(state.bytes)} · ${state.imageWidth}×${state.imageHeight}px`;

  drawWaveform();

  // Image box
  if (els.imageEmpty) els.imageEmpty.hidden = true;
  if (els.image) {
    els.image.classList.remove("hidden-view");
    if (uploadedBMPBlobURL && !stateMutatedSinceUpload) {
      els.image.src = uploadedBMPBlobURL;
    } else {
      els.image.src = "/image/current.bmp?t=" + Date.now();
    }
  }

  if (els.play) els.play.disabled = false;
  if (els.undo) els.undo.disabled = !state.canUndo;
  if (els.revert) els.revert.disabled = stateMutatedSinceUpload === false && !state.canUndo;
  if (els.exportWav) els.exportWav.disabled = false;
  if (els.exportBmp) els.exportBmp.disabled = false;
  if (els.presetSave) els.presetSave.disabled = !state.chainLength;
  if (els.presetLoad) els.presetLoad.disabled = false;
  if (els.commitRotary) els.commitRotary.disabled = false;

  if (els.waveEmpty) els.waveEmpty.hidden = true;
}

// -------------------------------------------------------------- waveform --

function drawWaveform() {
  const ctx = els.waveCtx;
  if (!ctx || typeof ctx.fillRect !== "function") return;
  const W = els.wave.width, H = els.wave.height;
  ctx.fillStyle = "#0e1116";
  ctx.fillRect(0, 0, W, H);

  if (!state || !state.hasState) return;

  // center line
  ctx.strokeStyle = "#2a3341";
  ctx.beginPath();
  ctx.moveTo(0, H / 2); ctx.lineTo(W, H / 2);
  ctx.stroke();

  const n = state.peakMin?.length || 0;
  if (!n) return;

  ctx.strokeStyle = "#6d4aff";
  ctx.lineWidth = 1;
  ctx.beginPath();
  const xStep = W / n;
  for (let i = 0; i < n; i++) {
    const x = i * xStep;
    const yMin = (H / 2) * (1 - state.peakMax[i]);
    const yMax = (H / 2) * (1 - state.peakMin[i]);
    ctx.moveTo(x, yMin);
    ctx.lineTo(x, yMax);
  }
  ctx.stroke();

  // playhead position (green line), drawn on top
  if (playheadFrac >= 0) {
    ctx.strokeStyle = "#3ddc97";
    ctx.lineWidth = 2;
    ctx.beginPath();
    const px = playheadFrac * W;
    ctx.moveTo(px, 0);
    ctx.lineTo(px, H);
    ctx.stroke();
  }
}

// -------------------------------------------------------------- playback --

function ensureAudio() {
  if (audio) return audio;
  audio = new Audio();
  audio.preload = "auto";
  audio.addEventListener("playing", () => setPlayButton(true));
  audio.addEventListener("pause",  () => setPlayButton(false));
  audio.addEventListener("ended",  () => {
    setPlayButton(false);
    playheadFrac = 0;
    drawWaveform();
  });
  audio.addEventListener("timeupdate", () => {
    if (audio && audio.duration > 0) {
      playheadFrac = audio.currentTime / audio.duration;
      drawWaveform();
    }
  });
  return audio;
}

function setPlayButton(playing) {
  if (!els.play) return;
  els.play.textContent = playing ? "■ Stop" : "▶ Play";
  els.play.classList.toggle("primary", !playing);
}

function reloadAudio() {
  if (!state || !state.hasState) return;
  const wasPlaying = audio && !audio.paused;
  const t = audio ? audio.currentTime : 0;
  if (audio) audio.pause();
  ensureAudio().src = "/audio/current.wav?t=" + Date.now();
  if (wasPlaying) {
    audio.play().catch(() => {});
  } else if (t) {
    // preserve seek position on mutation while paused
    audio.addEventListener("loadedmetadata", () => {
      audio.currentTime = Math.min(t, audio.duration || t);
      playheadFrac = audio.currentTime / audio.duration;
      drawWaveform();
    }, { once: true });
  }
}

function stopAudio() {
  if (audio) { audio.pause(); audio.currentTime = 0; }
  setPlayButton(false);
  playheadFrac = 0;
  if (state && state.hasState) drawWaveform();
}

if (els.play) {
  els.play.addEventListener("click", () => {
    ensureAudio();
    if (audio.paused) {
      if (!audio.src) audio.src = "/audio/current.wav";
      audio.play().catch((e) => setStatus("playback error: " + e.message));
    } else {
      stopAudio(); // STOP = pause AND rewind to the very beginning
    }
  });
}

// ---------------------------------------------------- waveform scrubbing --

if (els.wave) {
  let scrubbing = false;
  let wasPlayingBeforeScrub = false;

  const fracFromEvent = (e) => {
    const rect = els.wave.getBoundingClientRect();
    const cx = Math.min(Math.max(e.clientX - rect.left, 0), rect.width);
    return cx / rect.width;
  };

  const seekTo = (frac) => {
    if (!audio || !audio.src || !state || !state.hasState) return;
    const dur = audio.duration > 0 ? audio.duration : state.durationSec;
    if (!(dur > 0)) return;
    audio.currentTime = frac * dur;
    playheadFrac = frac;
    drawWaveform();
  };

  els.wave.addEventListener("pointerdown", (e) => {
    if (!state || !state.hasState) return;
    ensureAudio();
    if (!audio.src) return;
    scrubbing = true;
    wasPlayingBeforeScrub = !audio.paused;
    audio.pause(); // silence while scrubbing
    els.wave.setPointerCapture(e.pointerId);
    seekTo(fracFromEvent(e));
  });
  els.wave.addEventListener("pointermove", (e) => {
    if (scrubbing) seekTo(fracFromEvent(e));
  });
  const endScrub = () => {
    if (!scrubbing) return;
    scrubbing = false;
    if (wasPlayingBeforeScrub) audio.play().catch(() => {});
  };
  els.wave.addEventListener("pointerup", endScrub);
  els.wave.addEventListener("pointercancel", endScrub);
}

// ------------------------------------------------------------ undo/revert --

let stateMutatedSinceUpload = false;

async function mutate(path) {
  try {
    await api(path, { method: "POST" });
    await refresh(true);
    stateMutatedSinceUpload = state && state.canUndo ? true : false;
    setStatus(path.includes("undo") ? "undid last change" : "reverted to base");
  } catch (err) {
    setStatus(err.message);
  }
}

if (els.undo) els.undo.addEventListener("click", () => mutate("/api/undo"));
if (els.revert) els.revert.addEventListener("click", async () => {
  await mutate("/api/revert");
  stateMutatedSinceUpload = state && state.canUndo ? true : false;
  if (uploadedBMPBlobURL) {
    await refresh(false);
    if (els.image) els.image.src = uploadedBMPBlobURL;
  }
});

// --------------------------------------------------- export & presets (10.2) --

function triggerDownload(url) {
  const a = document.createElement("a");
  a.href = url;
  a.style.display = "none";
  document.body.appendChild(a);
  a.click();
  a.remove();
}

if (els.exportWav) {
  els.exportWav.addEventListener("click", () => {
    if (!state || !state.hasState) return;
    triggerDownload("/api/export/wav?t=" + Date.now());
    setStatus("exporting current audio as WAV …");
  });
}

if (els.exportBmp) {
  els.exportBmp.addEventListener("click", () => {
    if (!state || !state.hasState) return;
    triggerDownload("/api/export/bmp?t=" + Date.now());
    setStatus("exporting current image as BMP …");
  });
}

if (els.presetSave) {
  els.presetSave.addEventListener("click", () => {
    if (!state || !state.chainLength) return;
    triggerDownload("/api/preset?download=1&t=" + Date.now());
    setStatus(`exported preset (${state.chainLength} step${state.chainLength === 1 ? "" : "s"})`);
  });
}

if (els.presetLoad && els.presetFile) {
  els.presetLoad.addEventListener("click", () => els.presetFile.click());
  els.presetFile.addEventListener("change", async () => {
    const f = els.presetFile.files[0];
    els.presetFile.value = "";
    if (!f) return;
    setStatus(`loading preset ${f.name} …`);
    try {
      const text = await f.text();
      const body = JSON.parse(text);
      const r = await api("/api/preset/apply", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      stateMutatedSinceUpload = true;
      await refresh(true);
      setStatus(`applied preset ${f.name} (${r.applied} step${r.applied === 1 ? "" : "s"})`);
    } catch (err) {
      setStatus("preset apply failed: " + err.message);
    }
  });
}

// ---------------------------------------------------------- rotary knobs --

/* Angle mapping: -135deg .. +135deg across value range.
   Value ranges:  shift_x: -300..300 px, shift_y: -300..300 px,
                  randomize: 0..100 %.
   Interaction: drag vertically, mouse wheel, keyboard arrows, or type
   an exact value into the number input under each knob. */

const ROTARY_SPECS = {
  shift_x:   { min: -300, max: 300, unit: " px", label: "px shift" },
  shift_y:   { min: -300, max: 300, unit: " px", label: "px shift" },
  randomize: { min: 0,   max: 100, unit: " %",  label: "% replaced" },
};

const knobs = {}; // moduleName -> { el, value }

document.querySelectorAll(".rotary").forEach(rot => {
  const name = rot.dataset.module;
  const spec = ROTARY_SPECS[name];
  if (!name || !spec) return; // skip malformed
  const knobEl = rot.querySelector(".knob");
  const valEl = rot.querySelector(".rotary-value input");
  if (!knobEl || !valEl) return; // skip missing children
  const st = { el: knobEl, value: 0 };
  knobs[name] = st;

  function valueToAngle(v) {
    return -135 + (v - spec.min) / (spec.max - spec.min) * 270;
  }
  function setValue(v, silent) {
    v = Math.round(Math.max(spec.min, Math.min(spec.max, v)));
    st.value = v;
    knobEl.style.setProperty("--angle", valueToAngle(v) + "deg");
    knobEl.setAttribute("aria-valuenow", Math.round((v - spec.min) / (spec.max - spec.min) * 100));
    valEl.value = v;
    if (!silent) schedulePreview();
  }

  // manual numeric entry
  valEl.addEventListener("change", () => {
    const v = parseFloat(valEl.value);
    if (isNaN(v)) { valEl.value = st.value; return; }
    setValue(v);
  });

  // mouse wheel over the knob
  knobEl.addEventListener("wheel", (e) => {
    e.preventDefault();
    const step = (spec.max - spec.min) / 50;
    setValue(st.value - Math.sign(e.deltaY) * step);
  }, { passive: false });

  // pointer drag: vertical motion adjusts value
  let dragging = false, startY = 0, startVal = 0;
  knobEl.addEventListener("pointerdown", (e) => {
    dragging = true; startY = e.clientY; startVal = st.value;
    knobEl.setPointerCapture(e.pointerId);
  });
  knobEl.addEventListener("pointermove", (e) => {
    if (!dragging) return;
    const dy = startY - e.clientY;            // up = increase
    const perPx = (spec.max - spec.min) / 200; // 200px travel full scale
    setValue(startVal + dy * perPx);
  });
  knobEl.addEventListener("pointerup",   () => { dragging = false; });
  knobEl.addEventListener("pointercancel", () => { dragging = false; });

  // keyboard
  knobEl.addEventListener("keydown", (e) => {
    const step = (spec.max - spec.min) / 50;
    if (e.key === "ArrowLeft" || e.key === "ArrowDown") { setValue(st.value - step); e.preventDefault(); }
    if (e.key === "ArrowRight" || e.key === "ArrowUp")  { setValue(st.value + step); e.preventDefault(); }
    if (e.key === "Home") setValue(spec.min);
    if (e.key === "End")  setValue(spec.max);
  });

  knobEl._setValue = setValue;
  setValue(0, true);
});

// Safe random byte generation avoiding browser 65,536-byte QuotaExceededError
function fillRandomBytes(u8) {
  const maxChunk = 65536;
  for (let offset = 0; offset < u8.length; offset += maxChunk) {
    const chunk = u8.subarray(offset, Math.min(offset + maxChunk, u8.length));
    crypto.getRandomValues(chunk);
  }
}

// --- live preview: manipulate the current BMP bytes locally -----------------

let previewBMPBuf = null;   // ArrayBuffer of BMP currently displayed
let previewTimer = null;
let previewURL = null;

async function loadCurrentBMP() {
  const res = await fetch("/image/current.bmp?t=" + Date.now());
  previewBMPBuf = await res.arrayBuffer();
}

function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(runPreview, 120);
}

async function runPreview() {
  if (!previewBMPBuf) { try { await loadCurrentBMP(); } catch { return; } }
  try {
    const view = new DataView(previewBMPBuf);
    const width = view.getUint32(18, true);
    const height = view.getUint32(22, true);
    const rowSize = width * 3;
    const pixOff = 54;

    // Work on a copy so repeated previews compose from the committed state.
    const buf = previewBMPBuf.slice(0);
    const u8 = new Uint8Array(buf);

    const sx = knobs.shift_x ? knobs.shift_x.value : 0;
    const sy = knobs.shift_y ? knobs.shift_y.value : 0;
    const rndPct = knobs.randomize ? knobs.randomize.value : 0;

    function getPixel(y, x) {
      const rowTop = (height - 1 - y);              // bottom-up storage
      return pixOff + rowTop * rowSize + x * 3;
    }

    // shift_x: positive = image moves right, fresh column enters on left.
    // Every row gets its OWN random bytes (no shared strip → no stripes).
    if (sx !== 0) {
      const s = Math.sign(sx) * Math.min(Math.abs(sx), width);
      const n = Math.abs(s);
      const rnd = new Uint8Array(height * n * 3);
      fillRandomBytes(rnd);
      for (let y = 0; y < height; y++) {
        const rowRnd = rnd.subarray(y * n * 3, (y + 1) * n * 3);
        const base = getPixel(y, 0);
        const line = new Uint8Array(u8.subarray(base, base + rowSize));
        for (let x = 0; x < width; x++) {
          const from = x - s;
          if (from < 0 || from >= width) {
            const k = s > 0 ? x : x - (width - n);
            line.set(rowRnd.subarray(k * 3, k * 3 + 3), x * 3);
          }
        }
        u8.set(line, base);
      }
    }

    // shift_y: positive = image moves down, fresh rows enter on top.
    // Safe iteration direction + per-vacated-row entropy.
    if (sy !== 0) {
      const s = Math.sign(sy) * Math.min(Math.abs(sy), height);
      const n = Math.abs(s);
      const rnd = new Uint8Array(n * rowSize);
      fillRandomBytes(rnd);
      const start = s > 0 ? height - 1 : 0;
      const end   = s > 0 ? -1 : height;
      const step  = s > 0 ? -1 : 1;
      let fresh = 0;
      for (let y = start; y !== end; y += step) {
        const srcY = y - s;
        if (srcY < 0 || srcY >= height) {
          u8.set(rnd.subarray(fresh * rowSize, (fresh + 1) * rowSize), getPixel(y, 0));
          fresh++;
        } else {
          const to = getPixel(y, 0), from = getPixel(srcY, 0);
          u8.copyWithin(to, from, from + rowSize);
        }
      }
    }

    // randomize: replace that fraction of pixels entirely
    if (rndPct > 0) {
      const count = Math.floor(width * height * rndPct / 100);
      const rnd = new Uint8Array(count * 3);
      fillRandomBytes(rnd);
      for (let i = 0; i < count; i++) {
        // pick pseudo-random pixel (good enough for preview)
        const idx = i * 214013 % (width * height); // LCG-ish scatter
        const y = Math.floor(idx / width), x = idx % width;
        u8.set(rnd.subarray(i * 3, i * 3 + 3), getPixel(y, x));
      }
    }

    if (previewURL) URL.revokeObjectURL(previewURL);
    previewURL = URL.createObjectURL(new Blob([buf], { type: "image/bmp" }));
    els.image.src = previewURL;
  } catch (err) {
    console.warn("preview failed", err);
  }
}

// Refresh the preview base whenever the server state changes.
const _origRender = renderState;
renderState = async function () {
  await _origRender();
  previewBMPBuf = null; // force reload on next preview
};

// Commit: send the three knob values as one module application.
if (els.commitRotary) {
  els.commitRotary.addEventListener("click", async () => {
    try {
      const params = {
        shift_x_px: knobs.shift_x.value,
        shift_y_px: knobs.shift_y.value,
        randomize_pct: knobs.randomize.value,
      };
      const nonZero = Object.entries(params).filter(([, v]) => v !== 0);
      if (!nonZero.length) { setStatus("knobs at zero — nothing to commit"); return; }
      await api("/api/module/apply", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          module: "terrain_apply",
          params: params,
        }),
      });
      stateMutatedSinceUpload = true;
      // reset knobs after commit
      for (const n of Object.keys(knobs)) knobs[n].el._setValue(0, true);
      await refresh(true);
      setStatus("terrain shift committed");
    } catch (err) {
      setStatus("commit failed: " + err.message);
    }
  });
}

// ---------------------------------------------------------------- modules --

let moduleDefs = [];
let activeTab = "sound";

if (els.tabs) {
  els.tabs.addEventListener("click", (e) => {
    const tab = e.target.closest(".tab");
    if (!tab) return;
    activeTab = tab.dataset.cat;
    document.querySelectorAll(".tab").forEach(t => t.classList.toggle("active", t === tab));
    renderModuleList();
  });
}

function renderModuleList() {
  const list = moduleDefs.filter(m => m.category === activeTab);
  els.moduleList.innerHTML = "";
  if (!list.length) {
    els.moduleList.innerHTML = '<div class="empty-note small">no modules in this category yet</div>';
    return;
  }
  for (const m of list) {
    const card = document.createElement("div");
    card.className = "module-card";
    const h = document.createElement("h4"); h.textContent = m.label || m.name;
    const p = document.createElement("p");
    p.textContent = (m.params || []).length + " parameter" + ((m.params || []).length === 1 ? "" : "s");
    card.append(h, p);
    card.addEventListener("click", () => openModal(m));
    els.moduleList.appendChild(card);
  }
}

let modalModule = null;
let modalValues = {};

function openModal(m) {
  modalModule = m;
  modalValues = {};
  els.modalTitle.textContent = m.label || m.name;

  els.modalBody.innerHTML = "";
  for (const prm of m.params || []) {
    const row = document.createElement("div");
    row.className = "param-row";
    const lbl = document.createElement("label");
    const nameSpan = document.createElement("span");
    nameSpan.textContent = prm.label || prm.name;
    const valSpan = document.createElement("span");
    valSpan.className = "param-val";
    lbl.append(nameSpan, valSpan);
    row.appendChild(lbl);

    if (prm.kind === "toggle") {
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = prm.def > 0.5;
      cb.addEventListener("change", () => { modalValues[prm.name] = cb.checked ? 1 : 0; });
      modalValues[prm.name] = cb.checked ? 1 : 0;
      row.appendChild(cb);
    } else if (prm.kind === "select") {
      const sel = document.createElement("select");
      for (let v = prm.min; v <= prm.max; v += Math.max(prm.step, 1)) {
        const o = document.createElement("option");
        o.value = v; o.textContent = v;
        sel.appendChild(o);
      }
      sel.value = prm.def;
      sel.addEventListener("change", () => { modalValues[prm.name] = Number(sel.value); });
      modalValues[prm.name] = prm.def;
      row.appendChild(sel);
    } else {
      const inp = document.createElement("input");
      inp.type = "range";
      inp.min = prm.min; inp.max = prm.max; inp.step = prm.step || 0.01;
      inp.value = prm.def;
      valSpan.textContent = prm.def;
      inp.addEventListener("input", () => {
        modalValues[prm.name] = Number(inp.value);
        valSpan.textContent = Number(inp.value).toFixed(String(prm.step || 0.01).split(".")[1]?.length || 0);
      });
      modalValues[prm.name] = prm.def;
      row.appendChild(inp);
    }
    els.modalBody.appendChild(row);
  }

  els.modal.hidden = false;
  if (m.params && m.params.length) {
    const first = els.modalBody.querySelector("input, select");
    if (first) first.focus();
  }
}

function closeModal() { els.modal.hidden = true; modalModule = null; }

if (els.modalClose) els.modalClose.addEventListener("click", closeModal);
if (els.modalCancel) els.modalCancel.addEventListener("click", closeModal);
if (els.modal) els.modal.addEventListener("click", (e) => { if (e.target === els.modal) closeModal(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && els.modal && !els.modal.hidden) closeModal(); });

if (els.modalApply) {
  els.modalApply.addEventListener("click", async () => {
    if (!modalModule) return;
    const mod = modalModule;
    closeModal();
    try {
      await api("/api/module/apply", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ module: mod.name, params: modalValues }),
      });
      stateMutatedSinceUpload = true;
      await refresh(true);
      setStatus(`applied ${mod.label || mod.name}`);
    } catch (err) {
      setStatus("apply failed: " + err.message);
    }
  });
}

// ------------------------------------------------------------------- scan --
// Phase 9.1 — scanner wiring.
// Scan starts the server-side entropy hunt; the meter polls
// /api/scan/status and shows the best structure index seen so far.
// Keep finding applies the best candidate as one undoable mutation.
// Phase 10.0 — pluggable scan sources: picker + upload + hot-swap select.

const SCAN_POLL_MS = 250;
let scanPollBusy = false;

function updateScanUI(st) {
  // st: {running, segments, best_score, has_finding, elapsed_ms,
  //      segment_samples, source, source_name}
  const score = st.best_score || 0;

  if (els.scanMeter) els.scanMeter.value = Math.min(1, score / 100);
  if (els.scanScore) {
    els.scanScore.textContent = score.toFixed(2);
    // green highlight when a finding is held but scanning has stopped
    els.scanScore.classList.toggle("has-finding", !!st.has_finding && !st.running);
  }

  if (els.scanStats) {
    if (!st || st.segments === undefined || st.segments === 0) {
      els.scanStats.textContent = "scanned 0 B · best @ —";
    } else {
      const bytes = st.segments * (st.segment_samples || 16384) * 2;
      const bestTxt = st.has_finding ? score.toFixed(2) : "—";
      els.scanStats.textContent = `scanned ${fmtBytes(bytes)} · best @ ${bestTxt}`;
    }
  }

  // Button lifecycle:
  //   scanning     → Scan disabled, Stop enabled
  //   stopped      → Scan enabled (resume), Stop disabled
  //   Keep enabled whenever a finding is held (running or paused)
  if (els.scan)     els.scan.disabled = !!st.running;
  if (els.scanStop) els.scanStop.disabled = !st.running;
  if (els.scanKeep) els.scanKeep.disabled = !st.has_finding || (!state || !state.hasState);

  if (els.scan) els.scan.classList.toggle("scanning", !!st.running);

  // Phase 10.0: keep the source picker in sync with the server's active
  // source (covers hot-swaps triggered from elsewhere, e.g. uploads).
  if (els.scanSource && st.source && els.scanSource.value !== st.source &&
      [...els.scanSource.options].some(o => o.value === st.source)) {
    els.scanSource.value = st.source;
  }
}

async function pollScan() {
  if (scanPollBusy) return;
  scanPollBusy = true;
  try {
    const res = await fetch("/api/scan/status");
    if (res.ok) updateScanUI(await res.json());
    else if (res.status === 404) {
      // scanner not present (older server build) — stop polling quietly
      clearInterval(scanTimer);
    }
  } catch { /* transient network hiccup — keep polling */ }
  finally { scanPollBusy = false; }
}

const scanTimer = setInterval(pollScan, SCAN_POLL_MS);

if (els.scan) {
  els.scan.addEventListener("click", async () => {
    try {
      await api("/api/scan/start", { method: "POST" });
      setStatus("scanning entropy …");
    } catch (err) {
      setStatus("scan failed to start: " + err.message);
    }
    pollScan();
  });
}

if (els.scanStop) {
  els.scanStop.addEventListener("click", async () => {
    try {
      await api("/api/scan/stop", { method: "POST" });
      setStatus("scan stopped — finding held");
    } catch (err) {
      setStatus("scan stop failed: " + err.message);
    }
    pollScan();
  });
}

if (els.scanKeep) {
  els.scanKeep.addEventListener("click", async () => {
    if (els.scanKeep.disabled) return;
    els.scanKeep.disabled = true; // prevent double-fire while in flight
    try {
      await api("/api/scan/keep", { method: "POST" });
      stateMutatedSinceUpload = true;
      await refresh(true);
      setStatus("kept scanner finding — one undo to recover");
    } catch (err) {
      if (err.status === 409) {
        setStatus("no finding in the scanner — scan again first");
      } else {
        setStatus("keep failed: " + err.message);
      }
    }
    pollScan();
  });
}

// ------------------------------------------------- scan sources (10.0) --

async function loadScanSources(preferred) {
  try {
    const r = await api("/api/scan/sources");
    if (!els.scanSource) return;
    els.scanSource.innerHTML = "";
    for (const s of r.sources || []) {
      const o = document.createElement("option");
      o.value = s.id;
      o.textContent = s.name + (s.bytes ? " · " + fmtBytes(s.bytes) : "");
      els.scanSource.appendChild(o);
    }
    const st = await api("/api/scan/status");
    if (preferred && [...els.scanSource.options].some(o => o.value === preferred)) {
      els.scanSource.value = preferred;
    } else if (st.source && [...els.scanSource.options].some(o => o.value === st.source)) {
      els.scanSource.value = st.source;
    }
  } catch { /* server unreachable — select stays empty */ }
}

if (els.scanSource) {
  els.scanSource.addEventListener("change", async () => {
    try {
      await api("/api/scan/source/select", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source: els.scanSource.value }),
      });
      setStatus("scan source → " + els.scanSource.value);
    } catch (err) {
      setStatus("source select failed: " + err.message);
      loadScanSources(); // resync on error (e.g. "state" with nothing loaded)
    }
    pollScan();
  });
}

if (els.scanSourceAdd && els.scanSourceFile) {
  els.scanSourceAdd.addEventListener("click", () => els.scanSourceFile.click());
  els.scanSourceFile.addEventListener("change", async () => {
    const f = els.scanSourceFile.files[0];
    els.scanSourceFile.value = "";
    if (!f) return;
    setStatus(`adding scan source ${f.name} …`);
    try {
      const fd = new FormData();
      fd.append("file", f);
      const r = await api("/api/scan/sources", { method: "POST", body: fd });
      await loadScanSources(r.id); // select it (server also made it active)
      setStatus(`scan source added: ${r.name}`);
    } catch (err) {
      setStatus("source upload failed: " + err.message);
    }
    pollScan();
  });
}

// ------------------------------------------------------------------- init --

(async function init() {
  try {
    moduleDefs = (await api("/api/modules")).modules || [];
    renderModuleList();
  } catch {
    if (els.moduleList) els.moduleList.innerHTML = '<div class="empty-note small">server unreachable</div>';
  }
  await refresh(false);
  loadScanSources(); // Phase 10.0: populate the source picker
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  }
  setStatus("ready");
})();
