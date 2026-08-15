// Comma Sync GUI — drives the `comma-sync` core via Tauri commands and renders
// its --json progress stream. Uses the global Tauri API (withGlobalTauri).
const { invoke } = window.__TAURI__.core;
const { listen } = window.__TAURI__.event;

const $ = (id) => document.getElementById(id);
const store = {
  get out() { return localStorage.getItem("out") || ""; },
  set out(v) { localStorage.setItem("out", v); },
  get chunks() { return localStorage.getItem("chunks") || ""; },
  set chunks(v) { localStorage.setItem("chunks", v); },
};

let drives = [];           // last indexed drive list
let running = false;
let jobQueue = [];         // pending arg-arrays to run sequentially
const rowByRoute = {};     // route -> DOM row (for live status)
// How far through the run we are. Sync New is handed no list of drives — the core
// discovers them and reports the count — so this is the only source for "drive 3 of 7".
const runState = { index: 0, total: 0, phase: "", transferred: new Set(), done: new Set() };

function resetBatch() {
  runState.index = 0; runState.total = 0; runState.phase = "";
  runState.transferred = new Set(); runState.done = new Set();
  $("batchLine").textContent = "";
}

function renderBatchLine() {
  if (!runState.total) { $("batchLine").textContent = ""; return; }
  // During the transfer-everything-first pass nothing is stitched yet, so a "done"
  // tally would sit at zero the whole time and read as stuck. Count what's moving.
  const tally = runState.phase === "download"
    ? `${runState.transferred.size} transferred`
    : `${runState.done.size} done`;
  const pos = runState.index > 0 ? `Drive ${runState.index}/${runState.total} · ` : "";
  $("batchLine").textContent = pos + tally;
}

function opts() {
  return {
    output: store.out,
    chunks: store.chunks,
    audio: $("audio").checked,
    clean: $("clean").checked,
    use_usb: false,
    combined: $("combined").checked,
    primary: $("primaryCam").value,
    secondary: $("secondaryCam").value,
    tertiary: $("tertiaryCam").value,
    vr360: $("with360").checked,
    vertical: $("withVertical").checked,
    vertical_pos: $("verticalPos").value,
    all_first: $("allFirst").checked,
  };
}

function refreshPaths() {
  $("outPath").textContent = store.out || "Not set";
  $("chunkPath").textContent = store.chunks || "Not set";
  $("syncBtn").disabled = !store.out;
}

function logLine(s) {
  const el = $("log");
  if (el.textContent === "Output will appear here…") el.textContent = "";
  el.textContent += s + "\n";
  el.scrollTop = el.scrollHeight;
}

function setRunning(on) {
  running = on;
  $("syncBtn").classList.toggle("hidden", on);
  $("stopBtn").classList.toggle("hidden", !on);
  $("progress").classList.toggle("hidden", !on);
  if (!on) {
    $("progFill").style.width = "0%";
    $("progPct").textContent = "0%";
    $("progStatus").textContent = "";
  }
}

// ---- sequential job queue ---------------------------------------------------
function runQueue(jobs) {
  if (running || !jobs.length) return;
  jobQueue = jobs.slice();
  $("log").textContent = "";
  resetBatch();
  setRunning(true);
  runNext();
}
function runNext() {
  const args = jobQueue.shift();
  if (args === undefined) {
    setRunning(false);
    logLine("— all done —");
    return;
  }
  invoke("start_job", { opts: opts(), args }).catch((e) => {
    logLine("!! " + e);
    runNext();
  });
}

// ---- core event stream ------------------------------------------------------
listen("core-stderr", (e) => logLine(e.payload));
listen("core-done", () => { if (running) runNext(); });
listen("core-event", (e) => {
  let ev;
  try { ev = JSON.parse(e.payload); } catch { logLine(e.payload); return; }
  switch (ev.type) {
    case "progress": {
      const pct = Math.round(ev.percent || 0);
      const rate = ev.rateMBps > 0 ? " · " + ev.rateMBps.toFixed(1) + " MB/s" : "";
      $("progFill").style.width = pct + "%";
      $("progPct").textContent = pct + "%" + rate;
      $("progStatus").textContent =
        (ev.phase === "render" ? "Rendering " + (ev.message || "video")
          : ev.phase === "analyze" ? (ev.message || "Analyzing")
          : (ev.phase || "")) +
        (ev.route ? " · " + ev.route : "");
      if (ev.phase === "stitch") updateRow(ev.route, "Stitching…", null);
      else if (ev.phase === "render" || ev.phase === "analyze") updateRow(ev.route, pct + "%", pct);
      else updateRow(ev.route, pct + "%" + rate, pct);
      break;
    }
    case "plan":
      // The core has settled how many drives this run covers, before any transfer starts.
      runState.total = ev.total || 0;
      runState.index = 0;
      renderBatchLine();
      if (ev.message) logLine(ev.message);
      break;
    case "drive":
      if (ev.index > 0) runState.index = ev.index;
      if (ev.total > 0) runState.total = ev.total;
      runState.phase = ev.phase || "";
      renderBatchLine();
      logLine("==> " + ev.message);
      break;
    case "log": logLine(ev.message); break;
    case "routedone":
      // A finished transfer is not a finished drive — it only counts as done once it
      // has been stitched.
      if (ev.phase === "download") {
        runState.transferred.add(ev.route);
        updateRow(ev.route, "Transferred", null);
      } else {
        runState.done.add(ev.route);
        markDone(ev.route);
      }
      renderBatchLine();
      break;
    case "done": logLine(ev.message); markDone(ev.route); break;
    case "error": logLine("!! " + ev.message); break;
  }
});

// ---- folder pickers + buttons ----------------------------------------------
document.querySelectorAll("[data-pick]").forEach((b) => {
  b.addEventListener("click", async () => {
    const dir = await invoke("pick_folder");
    if (!dir) return;
    if (b.dataset.pick === "out") store.out = dir; else store.chunks = dir;
    refreshPaths();
  });
});
$("syncBtn").addEventListener("click", () => {                           // sync new (in the sheet)
  $("sheet").classList.add("hidden");
  runQueue([[]]);
});
$("stopBtn").addEventListener("click", () => { jobQueue = []; invoke("cancel_job"); });
$("indexBtn").addEventListener("click", openSheet);
$("closeSheet").addEventListener("click", () => $("sheet").classList.add("hidden"));
$("selAll").addEventListener("change", (e) => {
  document.querySelectorAll(".dcheck").forEach((c) => (c.checked = e.target.checked));
  updateSelCount();
});
$("dlAll").addEventListener("click", () => batch(drives.map((d) => d.route)));
$("dlSelected").addEventListener("click", () => batch(selectedRoutes()));
$("restitchSelected").addEventListener("click", () => batch(selectedRoutes()));

function batch(routes) {
  if (!routes.length) return;
  $("sheet").classList.add("hidden");
  // One core process handles the whole batch so IT controls the ordering (with
  // "download all drives first" on, every transfer finishes before any stitching).
  runQueue([["batch", ...routes]]);
}

// ---- indexing sheet ---------------------------------------------------------
async function openSheet() {
  $("sheet").classList.remove("hidden");
  $("sheetSub").textContent = "Indexing this computer and your comma…";
  $("driveList").innerHTML = "<p class='sub' style='text-align:center'>Scanning…</p>";
  try {
    drives = await invoke("list_drives", { opts: opts() });
  } catch (err) {
    $("driveList").innerHTML = "<p class='sub'>Couldn't index: " + err + "</p>";
    return;
  }
  renderDrives();
}

function renderDrives() {
  const total = drives.reduce((a, d) => a + (d.sizeKB || 0), 0);
  $("sheetSub").textContent = `${drives.length} drives · ${fmtSize(total)} total — on this computer and still on the comma`;
  const list = $("driveList");
  list.innerHTML = "";
  for (const d of drives) {
    const onComma = d.location === "device";
    const where = onComma ? "on comma" : d.location === "stitched" ? "videos only" : "on this computer";
    // A drive can be on the comma AND already stitched here. Showing only where it
    // lives made finished work look like work still to do. "videos only" already
    // means it was stitched, so it doesn't get the tag twice.
    const syncedTag = d.synced && d.location !== "stitched"
      ? '<span class="badge done" title="Already stitched — the videos are in your output folder">synced</span>'
      : "";
    const audio = d.hasAudio === true ? " · audio" : d.hasAudio === false ? " · no audio" : "";
    const row = document.createElement("div");
    row.className = "drive";
    row.innerHTML = `
      <input type="checkbox" class="dcheck" data-route="${d.route}" />
      <div class="dmeta">
        <div class="dname">${d.stamp}</div>
        <div class="dsub">${(d.cameras || []).join(", ")}${audio} · ${fmtSize(d.sizeKB)} · ${d.segments} min</div>
      </div>
      <span class="badges"><span class="badge">${where}</span>${syncedTag}</span>
      <span class="status"></span>`;
    list.appendChild(row);
    rowByRoute[d.route] = row;
  }
  list.querySelectorAll(".dcheck").forEach((c) => c.addEventListener("change", updateSelCount));
  updateSelCount();
}

function selectedRoutes() {
  return [...document.querySelectorAll(".dcheck:checked")].map((c) => c.dataset.route);
}
function updateSelCount() {
  $("selCount").textContent = `${selectedRoutes().length} selected`;
}
function updateRow(route, text, pct) {
  const row = rowByRoute[route];
  if (!row) return;
  const st = row.querySelector(".status");
  if (pct != null && pct < 100) {
    st.innerHTML = `<span class="minibar"><i style="width:${pct}%"></i></span> `;
    st.append(text); // e.g. "42% · 5.8 MB/s" — append as text, never HTML
  } else {
    st.textContent = text;
  }
}
function markDone(route) {
  const row = rowByRoute[route];
  if (!row) return;
  const st = row.querySelector(".status");
  st.textContent = "Synced ✓";
  st.classList.add("done");
}

function fmtSize(kb) {
  const mb = (kb || 0) / 1024;
  return mb >= 1024 ? (mb / 1024).toFixed(1) + " GB" : Math.round(mb) + " MB";
}

refreshPaths();

// ---- combined multi-angle video controls ------------------------------------
function syncCombinedUI() { $("combinedRoles").classList.toggle("hidden", !$("combined").checked); }
$("combined").checked = localStorage.getItem("combined") === "1";
$("primaryCam").value = localStorage.getItem("primaryCam") || "road";
$("secondaryCam").value = localStorage.getItem("secondaryCam") || "wide";
$("tertiaryCam").value = localStorage.getItem("tertiaryCam") || "driver";
syncCombinedUI();
$("combined").addEventListener("change", (e) => {
  localStorage.setItem("combined", e.target.checked ? "1" : "0");
  syncCombinedUI();
});
for (const id of ["primaryCam", "secondaryCam", "tertiaryCam"]) {
  $(id).addEventListener("change", (e) => localStorage.setItem(id, e.target.value));
}

// ---- 360 VR video ----------------------------------------------------------
$("with360").checked = localStorage.getItem("with360") === "1";
$("with360").addEventListener("change", (e) => localStorage.setItem("with360", e.target.checked ? "1" : "0"));

// ---- vertical phone video ---------------------------------------------------
function syncVerticalUI() { $("verticalRoles").classList.toggle("hidden", !$("withVertical").checked); }
$("withVertical").checked = localStorage.getItem("withVertical") === "1";
$("verticalPos").value = localStorage.getItem("verticalPos") || "bottom";
syncVerticalUI();
$("withVertical").addEventListener("change", (e) => {
  localStorage.setItem("withVertical", e.target.checked ? "1" : "0");
  syncVerticalUI();
});
$("verticalPos").addEventListener("change", (e) => localStorage.setItem("verticalPos", e.target.value));

// ---- sync order (default: download everything first) ------------------------
$("allFirst").checked = localStorage.getItem("allFirst") !== "0";
$("allFirst").addEventListener("change", (e) => localStorage.setItem("allFirst", e.target.checked ? "1" : "0"));

// ---- update check -----------------------------------------------------------
// On by default; the core reads only GitHub's public releases list (no data sent).
function autoUpdateOn() { return localStorage.getItem("autoUpdate") !== "0"; }

async function checkUpdate() {
  if (!autoUpdateOn()) { $("updateBanner").classList.add("hidden"); return; }
  try {
    const r = await invoke("check_update");
    if (r && r.updateAvailable) {
      $("updateSub").textContent = `${r.tag} is out — you're on ${r.current}.`;
      $("updateGet").onclick = () => invoke("open_url", { url: r.url });
      $("updateBanner").classList.remove("hidden");
    }
  } catch (_) { /* offline or rate-limited — ignore */ }
}

$("autoUpdate").checked = autoUpdateOn();
$("autoUpdate").addEventListener("change", (e) => {
  localStorage.setItem("autoUpdate", e.target.checked ? "1" : "0");
  if (e.target.checked) checkUpdate(); else $("updateBanner").classList.add("hidden");
});
$("updateClose").addEventListener("click", () => $("updateBanner").classList.add("hidden"));

checkUpdate();
setInterval(checkUpdate, 12 * 60 * 60 * 1000);   // re-check every 12h while left open
