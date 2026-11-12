// Models & pools (group host): the model library (find .gguf files in a
// folder such as an external disk, register them) and the pools (one model
// spread over this computer and chosen workers' GPUs, with its layer split,
// KV cache and request slots), including where llama.cpp put each part.
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var CACHE = ["f16", "q8_0", "q4_0", "q4_1", "q5_0", "q5_1", "iq4_nl", "bf16", "f32"];
  var state = { pools: [], sharing: [], models: [], editing: null };

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function short(name) { return name && name.indexOf("worker-") === 0 ? name.slice(0, 15) + "…" : (name || ""); }
  function who(node) { return node === "local" ? "this computer" : short(node); }
  function size(n) {
    if (typeof n !== "number") return "—";
    var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i ? n.toFixed(1) : n) + " " + u[i];
  }
  function mib(v) { return v ? (v >= 1024 ? (v / 1024).toFixed(2) + " GiB" : v.toFixed(1) + " MiB") : "—"; }
  async function api(method, url, body) {
    var res = await fetch(url, { method: method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined, cache: "no-store" });
    var out = res.status === 204 ? {} : await res.json().catch(function () { return {}; });
    if (!res.ok) throw new Error(out.error || ("HTTP " + res.status));
    return out;
  }

  // ---- model library ----
  async function loadModels() {
    try {
      var lib = await api("GET", "/api/models");
      state.models = lib.models || [];
      var rows = state.models.map(function (m) {
        return "<tr><td><b>" + esc(m.name) + "</b>" + (m.missing ? " <span class=\"missing\">file missing (disk unplugged?)</span>" : "") + "</td><td class=\"num\">" + size(m.size_bytes) + "</td><td class=\"num\" style=\"white-space:normal;word-break:break-all\">" + esc(m.path) + "</td><td><button type=\"button\" class=\"secondary\" data-remove=\"" + esc(m.name) + "\" style=\"padding:.15rem .5rem;font-size:.72rem\">Forget</button></td></tr>";
      });
      $("modelRows").innerHTML = rows.length ? rows.join("") : "<tr><td colspan=\"4\" class=\"empty\">No model registered. Point the folder box at where your .gguf files are and press Find models.</td></tr>";
      var dl = $("scanDirs");
      dl.innerHTML = (lib.dirs || []).map(function (d) { return "<option value=\"" + esc(d) + "\">"; }).join("");
      if (!$("scanDir").value && lib.dirs && lib.dirs.length) $("scanDir").value = lib.dirs[0];
      var jobs = (lib.jobs || []).filter(function (j) { return j.state !== "done" || Date.now() - new Date(j.started) < 60000; });
      $("jobOut").innerHTML = jobs.map(function (j) {
        return esc(j.name) + ": " + (j.state === "hashing" ? "checking the file (large models on a slow disk take minutes)…" : j.state === "done" ? "registered" : "<span class=\"missing\">failed: " + esc(j.error) + "</span>");
      }).join("<br>");
      fillModels();
      if (jobs.some(function (j) { return j.state === "hashing"; })) setTimeout(loadModels, 1500);
    } catch (err) {
      $("modelRows").innerHTML = "<tr><td colspan=\"4\" class=\"empty\">" + esc(err.message) + "</td></tr>";
    }
  }

  async function scan() {
    var dir = $("scanDir").value.trim();
    if (!dir) { $("scanMsg").textContent = "type a folder"; return; }
    $("scanMsg").textContent = "looking…";
    try {
      var out = await api("POST", "/api/models/scan", { dir: dir, remember: $("scanRemember").checked });
      var files = out.files || [];
      $("scanMsg").textContent = files.length + (files.length === 1 ? " model file" : " model files");
      $("scanOut").innerHTML = files.length ? "<div class=\"table-wrap\"><table><thead><tr><th>File</th><th>Size</th><th>Register as</th></tr></thead><tbody>" + files.map(function (f, i) {
        var name = f.path.split(/[\\/]/).pop() + (f.parts > 1 ? " (" + f.parts + " parts)" : "");
        var action = f.registered ? "<span class=\"badge\">registered as " + esc(f.registered) + "</span>" :
          "<span class=\"scan-row pool-row\"><input data-name=\"" + i + "\" value=\"" + esc(f.suggested) + "\"><button type=\"button\" data-add=\"" + i + "\">Register</button></span>";
        return "<tr><td style=\"white-space:normal;word-break:break-all\" title=\"" + esc(f.path) + "\">" + esc(name) + "</td><td class=\"num\">" + size(f.size_bytes) + "</td><td>" + action + "</td></tr>";
      }).join("") + "</tbody></table></div>" : "";
      $("scanOut").dataset.files = JSON.stringify(files);
      loadModels();
    } catch (err) {
      $("scanMsg").textContent = err.message;
    }
  }

  // ---- pools ----
  async function loadPools() {
    try {
      var st = await api("GET", "/api/pools");
      state.pools = st.pools || [];
      state.sharing = st.sharing || [];
      renderPools();
    } catch (err) {
      $("poolState").textContent = err.message;
    }
  }

  function renderPools() {
    var ready = state.pools.filter(function (p) { return p.state === "ready"; }).length;
    $("poolState").textContent = state.pools.length ? ready + " of " + state.pools.length + " pool(s) ready · " + state.sharing.length + " worker(s) lending" : state.sharing.length + " worker(s) lending a GPU";
    var box = $("poolList");
    if (!state.pools.length) {
      box.innerHTML = "<div class=\"hint\">No pool yet. Register a model, then press New pool.</div>";
      return;
    }
    box.innerHTML = state.pools.map(function (p, i) {
      var s = p.settings || {};
      var bits = [];
      bits.push(p.context + " context" + (p.parallel > 1 ? " · " + p.parallel + " slots (" + Math.floor(p.context / p.parallel) + " each)" : ""));
      bits.push("KV " + (s.cache_type_k || "f16") + "/" + (s.cache_type_v || "f16") + (s.kv_on_host ? " in host RAM" : ""));
      if (s.flash_attn) bits.push("flash attention " + s.flash_attn);
      bits.push(s.no_host_gpu ? "host GPU not used" : "host GPU used");
      bits.push(s.split && Object.keys(s.split).length ? "manual split" : "automatic split");
      bits.push(s.workers && s.workers.length ? "workers: " + s.workers.map(short).join(", ") : "workers: automatic");
      var rows = (p.devices || []).map(function (d) {
        return "<tr><td>" + esc(who(d.node)) + "</td><td class=\"num\">" + esc(d.device) + "</td><td class=\"num\">" + mib(d.weights_mib) + "</td><td class=\"num\">" + mib(d.kv_mib) + "</td><td class=\"num\">" + mib(d.compute_mib) + "</td></tr>";
      });
      var color = p.state === "ready" ? "var(--accent)" : p.state === "failed" ? "var(--bad)" : "var(--warn)";
      return "<div class=\"poolcard " + esc(p.state) + "\" data-pool=\"" + esc(p.name) + "\">" +
        "<div class=\"poolcard-head\"><b>" + esc(p.name) + "</b><span class=\"badge\">" + esc(p.model) + "</span><span style=\"color:" + color + "\">" + esc(p.enabled ? p.state : "stopped") + "</span>" +
        (p.in_flight ? "<span class=\"status-text\">" + p.in_flight + " answering</span>" : "") +
        "<span style=\"flex:1\"></span>" +
        "<button type=\"button\" class=\"secondary\" data-edit=\"" + i + "\">Edit</button>" +
        "<button type=\"button\" class=\"secondary\" data-toggle=\"" + i + "\">" + (p.enabled ? "Stop" : "Start") + "</button>" +
        "<button type=\"button\" class=\"secondary\" data-drop=\"" + i + "\">Remove</button></div>" +
        "<div class=\"hint\">" + esc(bits.join(" · ")) + "</div>" +
        (p.detail ? "<div class=\"hint\">" + esc(p.detail) + "</div>" : "") +
        (p.enabled ? "<div class=\"hint\">GPUs in use: " + esc(["this computer"].concat((p.workers || []).map(short)).join(", ")) + (p.offloaded ? " · " + esc(p.offloaded) + " layers offloaded" : "") + "</div>" : "") +
        (rows.length ? "<div class=\"table-wrap\"><table><thead><tr><th>Computer</th><th>Device</th><th>Weights</th><th>KV cache</th><th>Compute</th></tr></thead><tbody>" + rows.join("") + "</tbody></table></div>" : "") +
        (p.state === "failed" && p.log && p.log.length ? "<pre class=\"pool-log\">" + esc(p.log.slice(-6).join("\n")) + "</pre>" : "") +
        "</div>";
    }).join("");
  }

  // ---- editor ----
  function fillModels() {
    var sel = $("edModel"), keep = sel.value;
    sel.innerHTML = state.models.filter(function (m) { return !m.missing; }).map(function (m) { return "<option>" + esc(m.name) + "</option>"; }).join("") || "<option value=\"\">register a model first</option>";
    if (keep) sel.value = keep;
  }
  function claimedElsewhere(name) {
    var taken = {};
    state.pools.forEach(function (p) {
      if (p.name === name) return;
      ((p.settings || {}).workers || []).forEach(function (w) { taken[w] = p.name; });
    });
    return taken;
  }
  function renderWorkerChoices(selected) {
    var taken = claimedElsewhere($("edName").value.trim());
    var all = state.sharing.slice();
    selected.forEach(function (w) { if (all.indexOf(w) < 0) all.push(w); });
    $("edWorkers").innerHTML = all.length ? all.map(function (w) {
      var off = !!taken[w];
      return "<label title=\"" + esc(w) + "\"><input type=\"checkbox\" value=\"" + esc(w) + "\"" + (selected.indexOf(w) >= 0 ? " checked" : "") + (off ? " disabled" : "") + "> " + esc(short(w)) + (off ? " (in pool " + esc(taken[w]) + ")" : state.sharing.indexOf(w) < 0 ? " (not lending now)" : "") + "</label>";
    }).join("") : "<span class=\"hint\">No worker is lending a GPU yet (they need llama.cpp: ./linux.sh gpu or win.bat option 5).</span>";
    renderSplit();
  }
  function chosenWorkers() {
    return Array.prototype.filter.call($("edWorkers").querySelectorAll("input:checked"), function (i) { return !i.disabled; }).map(function (i) { return i.value; });
  }
  function renderSplit() {
    var manual = $("edSplitMode").value === "manual";
    $("edSplit").hidden = !manual;
    if (!manual) return;
    var keep = {};
    Array.prototype.forEach.call($("edSplit").querySelectorAll("input"), function (i) { keep[i.dataset.node] = i.value; });
    var split = (state.editing && state.editing.split) || {};
    var nodes = [];
    if ($("edHostGPU").checked) nodes.push("local");
    var picked = chosenWorkers();
    (picked.length ? picked : state.sharing.filter(function (w) { return !claimedElsewhere($("edName").value.trim())[w]; })).forEach(function (w) { nodes.push(w); });
    $("edSplit").innerHTML = nodes.map(function (n) {
      var v = keep[n] != null ? keep[n] : (split[n] != null ? split[n] : 1);
      return "<label>" + esc(who(n)) + " <input type=\"number\" min=\"0\" max=\"1000\" step=\"0.5\" value=\"" + esc(v) + "\" data-node=\"" + esc(n) + "\" style=\"width:5rem\"></label>";
    }).join("") + "<span class=\"hint\">relative weights: 1 and 3 put a quarter of the layers on the first; 0 puts none there</span>";
  }
  function openEditor(p) {
    var s = p ? p.settings : { name: state.pools.length ? "" : "main", context: 4096, parallel: 1 };
    state.editing = s;
    $("poolEditor").hidden = false;
    $("edTitle").textContent = p ? "Edit pool " + s.name : "New pool";
    $("edName").value = s.name || "";
    $("edName").disabled = !!p;
    fillModels();
    if (s.model) $("edModel").value = s.model;
    $("edContext").value = s.context || 4096;
    $("edParallel").value = s.parallel || 1;
    $("edHostGPU").checked = !s.no_host_gpu;
    $("edSplitMode").value = s.split && Object.keys(s.split).length ? "manual" : "auto";
    ["edCtk", "edCtv"].forEach(function (id, k) {
      $(id).innerHTML = CACHE.map(function (c) { return "<option>" + c + "</option>"; }).join("");
      $(id).value = (k ? s.cache_type_v : s.cache_type_k) || "f16";
    });
    $("edKVHost").checked = !!s.kv_on_host;
    $("edFA").value = s.flash_attn || "";
    $("edMsg").textContent = "";
    renderWorkerChoices(s.workers || []);
    hint();
    $("poolEditor").scrollIntoView({ behavior: "smooth", block: "nearest" });
  }
  function hint() {
    var notes = [];
    var v = $("edCtv").value;
    if (v !== "f16" && v !== "f32" && v !== "bf16" && $("edFA").value === "off") notes.push("A quantized V cache needs flash attention; choose auto or on.");
    var ctx = Number($("edContext").value) || 0, par = Number($("edParallel").value) || 1;
    if (par > 1) notes.push("Each of the " + par + " slots gets " + Math.floor(ctx / par) + " tokens of context.");
    if ($("edKVHost").checked) notes.push("KV cache in host RAM frees VRAM on every GPU but slows generation.");
    $("edHint").textContent = notes.join(" ");
  }
  async function save() {
    var cfg = {
      name: $("edName").value.trim(), model: $("edModel").value, enabled: true,
      context: Number($("edContext").value) || 0, parallel: Number($("edParallel").value) || 1,
      workers: chosenWorkers(), no_host_gpu: !$("edHostGPU").checked,
      cache_type_k: $("edCtk").value === "f16" ? "" : $("edCtk").value,
      cache_type_v: $("edCtv").value === "f16" ? "" : $("edCtv").value,
      kv_on_host: $("edKVHost").checked, flash_attn: $("edFA").value
    };
    if ($("edSplitMode").value === "manual") {
      cfg.split = {};
      Array.prototype.forEach.call($("edSplit").querySelectorAll("input"), function (i) { cfg.split[i.dataset.node] = Number(i.value) || 0; });
    }
    if (cfg.parallel === 1) cfg.parallel = 0;
    $("edMsg").textContent = "saving…";
    try {
      var st = await api("POST", "/api/pools", { action: "set", pool: cfg });
      state.pools = st.pools || [];
      state.sharing = st.sharing || state.sharing;
      $("poolEditor").hidden = true;
      renderPools();
    } catch (err) {
      $("edMsg").textContent = err.message;
    }
  }

  async function onPoolClick(e) {
    var b = e.target.closest("button");
    if (!b) return;
    var p = state.pools[Number(b.dataset.edit || b.dataset.toggle || b.dataset.drop)];
    if (!p) return;
    try {
      if (b.dataset.edit != null) openEditor(p);
      if (b.dataset.toggle != null) {
        var cfg = JSON.parse(JSON.stringify(p.settings));
        cfg.enabled = !p.enabled;
        await api("POST", "/api/pools", { action: "set", pool: cfg });
      }
      if (b.dataset.drop != null) await api("POST", "/api/pools", { action: "remove", name: p.name });
      loadPools();
    } catch (err) {
      $("poolState").textContent = err.message;
    }
  }

  async function init() {
    var info;
    try { info = await api("GET", "/api/info"); } catch (_) { return; }
    if (info.role !== "host") return;
    $("poolPanel").hidden = false;
    $("scanBtn").addEventListener("click", scan);
    $("scanDir").addEventListener("keydown", function (e) { if (e.key === "Enter") scan(); });
    $("scanOut").addEventListener("click", async function (e) {
      var b = e.target.closest("button[data-add]");
      if (!b) return;
      var files = JSON.parse($("scanOut").dataset.files || "[]");
      var f = files[Number(b.dataset.add)];
      var name = $("scanOut").querySelector("input[data-name=\"" + b.dataset.add + "\"]").value.trim();
      b.disabled = true;
      try { await api("POST", "/api/models/add", { name: name, path: f.path }); b.textContent = "Registering…"; }
      catch (err) { b.disabled = false; $("scanMsg").textContent = err.message; }
      loadModels();
    });
    $("modelRows").addEventListener("click", async function (e) {
      var b = e.target.closest("button[data-remove]");
      if (!b) return;
      try { await api("POST", "/api/models/remove", { name: b.dataset.remove }); } catch (err) { $("scanMsg").textContent = err.message; }
      loadModels();
    });
    $("poolNew").addEventListener("click", function () { openEditor(null); });
    $("poolList").addEventListener("click", onPoolClick);
    $("edSave").addEventListener("click", save);
    $("edCancel").addEventListener("click", function () { $("poolEditor").hidden = true; });
    $("edWorkers").addEventListener("change", renderSplit);
    $("edHostGPU").addEventListener("change", renderSplit);
    $("edSplitMode").addEventListener("change", renderSplit);
    $("edName").addEventListener("input", function () { renderWorkerChoices(chosenWorkers()); });
    ["edCtv", "edFA", "edContext", "edParallel", "edKVHost"].forEach(function (id) { $(id).addEventListener("input", hint); $(id).addEventListener("change", hint); });
    loadModels();
    loadPools();
    setInterval(function () { if ($("poolEditor").hidden || !document.activeElement || !$("poolEditor").contains(document.activeElement)) loadPools(); }, 3000);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
