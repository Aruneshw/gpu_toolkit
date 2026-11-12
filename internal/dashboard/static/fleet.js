// Fleet: one live card per computer (a "digital twin": GPU, VRAM, heat,
// power, CPU, RAM, link and what the host is doing there) and, on a group
// host, a grid with one terminal per worker sharing a single event stream.
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var role = "local";
  var info = {};
  var fleetData = null;

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function short(name) { return name && name.indexOf("worker-") === 0 ? name.slice(0, 15) + "…" : (name || ""); }
  function gib(n) { return typeof n === "number" ? (n / 1073741824).toFixed(1) : "—"; }
  function post(url, body) {
    return fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  }
  function store(key, value) { try { if (value == null) localStorage.removeItem(key); else localStorage.setItem(key, value); } catch (_) {} }
  function load(key) { try { return localStorage.getItem(key); } catch (_) { return null; } }

  // ---- fleet cards ----
  function meter(label, pct, text, warnAt, critAt) {
    var w = typeof pct === "number" && isFinite(pct) ? Math.max(0, Math.min(100, pct)) : 0;
    var cls = pct >= critAt ? "crit" : (pct >= warnAt ? "hot" : "");
    return "<div class=\"meter\"><span>" + label + "</span><span class=\"bar\"><i class=\"" + cls + "\" style=\"width:" + w.toFixed(1) + "%\"></i></span><span>" + esc(text) + "</span></div>";
  }
  function spark(history, keys, colors, max) {
    var W = 280, H = 42, n = history.length;
    if (n < 2) return "<svg class=\"spark\" viewBox=\"0 0 " + W + " " + H + "\"></svg>";
    var paths = keys.map(function (key, k) {
      var top = max || 0;
      if (!max) history.forEach(function (s) { if (typeof s[key] === "number" && s[key] > top) top = s[key]; });
      if (!top) top = 1;
      var d = "", pen = false;
      history.forEach(function (s, i) {
        var v = s[key];
        if (typeof v !== "number") { pen = false; return; }
        var x = (i / (n - 1)) * W, y = H - 2 - (Math.min(v, top) / top) * (H - 4);
        d += (pen ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1);
        pen = true;
      });
      return d ? "<path d=\"" + d + "\" fill=\"none\" stroke=\"" + colors[k] + "\" stroke-width=\"1.6\" vector-effect=\"non-scaling-stroke\"/>" : "";
    });
    return "<svg class=\"spark\" viewBox=\"0 0 " + W + " " + H + "\" preserveAspectRatio=\"none\">" + paths.join("") + "</svg>";
  }
  function rttBadge(ms, link) {
    if (typeof ms !== "number") return "";
    var cls = ms < 30 ? "good" : (ms < 120 ? "mid" : "bad");
    var how = link && link.measured === "tunnel" ? "round trip through the encrypted tunnel the pool uses (median of the last 30 s)" : "time of one status request through the encrypted link, including a TLS handshake";
    return "<span class=\"rtt " + cls + "\" data-rtt title=\"" + how + "\">" + (ms < 0.05 ? "&lt;1" : ms < 10 ? ms.toFixed(1) : Math.round(ms)) + " ms</span>";
  }
  var pathNames = { relay: "via relay", mesh: "mesh VPN", direct: "direct", local: "same computer" };
  function pathBadge(link) {
    if (!link || !pathNames[link.path]) return "";
    var tip = { relay: "connected through the public TCP relay: works anywhere, but every round trip makes a detour", mesh: "connected over a mesh VPN (Tailscale, WireGuard, ...)", direct: "connected straight to the host's address", local: "a worker on this computer" }[link.path];
    return "<span class=\"badge path-" + esc(link.path) + "\" data-path=\"" + esc(link.path) + "\" title=\"" + tip + "\">" + pathNames[link.path] + "</span>";
  }
  function rate(b) {
    return b >= 1048576 ? (b / 1048576).toFixed(1) + " MB/s" : b >= 1024 ? Math.round(b / 1024) + " kB/s" : Math.round(b) + " B/s";
  }
  // The pool's traffic to this worker, and how much of the pool's time
  // waits on its link.
  function rpcLine(link) {
    if (!link || !(link.rpc_up_bps > 0 || link.rpc_down_bps > 0)) return "";
    var bits = ["RPC ↑ " + rate(link.rpc_up_bps) + " ↓ " + rate(link.rpc_down_bps), Math.round(link.rpc_round_trips_per_s) + " round trips/s"];
    var cls = "";
    if (typeof link.wait_share === "number") {
      bits.push("≈" + Math.round(link.wait_share * 100) + "% of the time waiting on this link");
      if (link.wait_share >= 0.4) cls = " warn";
    }
    return "<div class=\"hint rpc-line" + cls + "\" data-rpc>" + esc(bits.join(" · ")) + "</div>";
  }
  // Names of the ready pools a computer takes part in.
  function poolsOf(name) {
    var host = (fleetData && fleetData.nodes || []).find(function (n) { return n.name === "local"; });
    var pools = (host && host.status && host.status.pools) || [];
    return pools.filter(function (p) {
      return p.state === "ready" && (name === "local" ? !(p.settings && p.settings.no_host_gpu) : (p.workers || []).indexOf(name) >= 0);
    }).map(function (p) { return p.name; });
  }

  function card(n) {
    var st = n.status || {};
    var res = st.resources || {};
    var gpus = res.gpus || [];
    var last = n.history && n.history.length ? n.history[n.history.length - 1] : {};
    var vt = 0, vf = 0, temp = null, power = 0, limit = 0, hasPower = false;
    gpus.forEach(function (g) {
      if (typeof g.total_bytes === "number" && typeof g.free_bytes === "number") { vt += g.total_bytes; vf += g.free_bytes; }
      if (typeof g.temperature_c === "number" && (temp == null || g.temperature_c > temp)) temp = g.temperature_c;
      if (typeof g.power_watts === "number") { power += g.power_watts; hasPower = true; }
      if (typeof g.power_limit_watts === "number") limit += g.power_limit_watts;
    });
    var name = n.name === "local" ? (role === "worker" ? "this computer" : "host (this computer)") : short(n.name);
    var h = "<div class=\"twin-head\"><span class=\"dot" + (n.healthy ? " ok" : "") + "\"></span><b title=\"" + esc(n.name) + "\">" + esc(name) + "</b>";
    h += rttBadge(n.rtt_ms, n.link) + pathBadge(n.link);
    if (st.rpc_ready) h += "<span class=\"badge\">lending GPU</span>";
    poolsOf(n.name).forEach(function (p) { h += "<span class=\"badge\">pool " + esc(p) + "</span>"; });
    h += "</div>";
    h += rpcLine(n.link);
    h += "<div class=\"gpu-name\">" + esc(gpus.length ? gpus.map(function (g) { return g.name; }).join(" + ") : "no NVIDIA GPU reported") + (res.os ? " · " + esc(res.os) : "") + "</div>";
    if (!n.healthy) {
      var seen = n.last_seen && n.last_seen.indexOf("0001") !== 0 ? new Date(n.last_seen).toLocaleTimeString() : "never";
      h += "<div class=\"hint\">offline · last seen " + esc(seen) + (n.error ? "<br>" + esc(n.error.slice(0, 140)) : "") + "</div>";
    } else {
      h += meter("GPU", last.gpu, typeof last.gpu === "number" ? Math.round(last.gpu) + "%" : "—", 85, 97);
      h += meter("VRAM", last.vram, vt ? gib(vt - vf) + " / " + gib(vt) + " G" : "—", 85, 95);
      h += meter("Temp", temp != null ? temp / 0.95 : null, temp != null ? Math.round(temp) + " °C" : "—", 78, 87);
      h += meter("Power", limit ? power * 100 / limit : (hasPower ? Math.min(100, power) : null), hasPower ? power.toFixed(1) + (limit ? " / " + Math.round(limit) : "") + " W" : "—", 90, 98);
      h += meter("CPU", last.cpu, typeof last.cpu === "number" ? Math.round(last.cpu) + "%" : "—", 85, 97);
      h += meter("RAM", last.ram, typeof res.ram_total_bytes === "number" ? gib(res.ram_total_bytes - res.ram_available_bytes) + " / " + gib(res.ram_total_bytes) + " G" : "—", 85, 95);
    }
    var hist = n.history || [];
    h += spark(hist, ["gpu", "vram"], ["#7ee2a8", "#79c0ff"], 100);
    h += "<div class=\"spark-legend\"><span><i style=\"background:#7ee2a8\"></i>GPU %</span><span><i style=\"background:#79c0ff\"></i>VRAM %</span>";
    if (n.name !== "local") h += "<span><i style=\"background:#ffb454\"></i>link ms</span>";
    h += "<span style=\"margin-left:auto\">last " + Math.round(hist.length * 2 / 60 * 10) / 10 + " min</span></div>";
    if (n.name !== "local") h += spark(hist, ["rtt"], ["#ffb454"], 0);
    h += "<div class=\"twin-foot\">";
    var bits = [];
    if (n.tasks_running) bits.push(n.tasks_running + " task" + (n.tasks_running > 1 ? "s" : "") + " running");
    if (n.tasks_waiting) bits.push(n.tasks_waiting + " waiting");
    if (n.tasks_done) bits.push(n.tasks_done + " finished");
    if (n.terminals) bits.push(n.terminals + " terminal" + (n.terminals > 1 ? "s" : "") + " open");
    if (st.busy) bits.push("answering a prompt");
    if (role === "host" && n.name !== "local") {
      bits.push("tasks: " + (st.tasks || "?"));
      bits.push(st.terminal ? "terminal allowed" : "no terminal");
    }
    h += "<span>" + esc(bits.join(" · ") || "idle") + "</span>";
    if (role === "host" && n.healthy && n.name !== "local") {
      h += "<span style=\"flex:1\"></span>";
      if (st.terminal) h += "<button type=\"button\" class=\"secondary\" data-term=\"" + esc(n.name) + "\">Terminal</button>";
      if (st.tasks && st.tasks !== "off") h += "<button type=\"button\" class=\"secondary\" data-task=\"" + esc(n.name) + "\">Send task</button>";
    }
    h += "</div>";
    return h;
  }

  function renderFleet(data) {
    fleetData = data;
    var box = $("fleet");
    var nodes = data.nodes || [];
    var seen = {};
    nodes.forEach(function (n, i) {
      var id = "twin-" + n.name;
      seen[id] = true;
      var el = document.getElementById(id);
      if (!el) { el = document.createElement("div"); el.id = id; }
      if (box.children[i] !== el) box.insertBefore(el, box.children[i] || null);
      el.className = "twin" + (n.healthy ? "" : " offline");
      el.innerHTML = card(n);
    });
    Array.prototype.slice.call(box.children).forEach(function (el) { if (!seen[el.id]) el.remove(); });
    var healthy = nodes.filter(function (n) { return n.healthy; }).length;
    $("fleetNote").textContent = healthy + " / " + nodes.length + " online · updated " + new Date().toLocaleTimeString();
    if (role === "host") syncPanes(nodes);
  }

  async function pollFleet() {
    try {
      var res = await fetch("/api/fleet", { cache: "no-store" });
      if (res.ok) renderFleet(await res.json());
    } catch (_) {}
  }

  // ---- terminal grid (host) ----
  var panes = {};       // node -> pane
  var stream = null;    // AbortController of the shared event stream
  var streamKey = "";
  var owned = {};       // node -> session id this grid opened
  try { owned = JSON.parse(load("meshGrid") || "{}") || {}; } catch (_) { owned = {}; }
  function saveOwned() {
    var o = {};
    Object.keys(panes).forEach(function (k) { if (panes[k].id) o[k] = panes[k].id; });
    store("meshGrid", JSON.stringify(o));
  }

  function makePane(node) {
    var p = { node: node, id: null, offset: 0, term: null, fit: null, state: "closed" };
    p.el = document.createElement("div");
    p.el.className = "pane";
    p.el.dataset.node = node;
    p.head = document.createElement("div");
    p.head.className = "pane-head";
    p.body = document.createElement("div");
    p.body.className = "pane-term";
    p.el.appendChild(p.head);
    p.el.appendChild(p.body);
    panes[node] = p;
    renderHead(p);
    return p;
  }
  function renderHead(p) {
    var label = p.node === "local" ? "this computer" : short(p.node);
    p.head.innerHTML = "<input type=\"checkbox\" title=\"include in 'type into all'\"" + (p.cast === false ? "" : " checked") + "><span class=\"dot" + (p.state === "connected" ? " ok" : "") + "\"></span><b title=\"" + esc(p.node) + "\">" + esc(label) + "</b><span class=\"status-text\">" + esc(p.state) + "</span><span style=\"flex:1\"></span>";
    p.head.querySelector("input").addEventListener("change", function (e) { p.cast = e.target.checked; });
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "secondary";
    btn.textContent = p.id ? "Restart" : "Connect";
    btn.addEventListener("click", function () { if (p.id) closePane(p, true); openPane(p); });
    p.head.appendChild(btn);
    if (p.id) {
      var x = document.createElement("button");
      x.type = "button";
      x.className = "secondary";
      x.textContent = "Close";
      x.addEventListener("click", function () { closePane(p, true); });
      p.head.appendChild(x);
    }
  }
  function setState(p, state) { p.state = state; renderHead(p); updateNote(); }
  function updateNote() {
    var n = Object.keys(panes).filter(function (k) { return panes[k].state === "connected"; }).length;
    $("gridNote").textContent = n + " connected · " + Object.keys(panes).length + " available";
  }

  function ensureTerm(p) {
    if (p.term || !window.Terminal) return;
    p.term = new window.Terminal({
      cursorBlink: true, scrollback: 3000, fontSize: 12,
      fontFamily: 'ui-monospace, SFMono-Regular, "DejaVu Sans Mono", Menlo, Consolas, monospace',
      theme: { background: "#0b1016", foreground: "#d7e2ee", cursor: "#7ee2a8", selectionBackground: "#2b6346" }
    });
    p.fit = new window.FitAddon.FitAddon();
    p.term.loadAddon(p.fit);
    p.term.open(p.body);
    try { p.fit.fit(); } catch (_) {}
    p.term.onData(function (d) { typeInto(p, d); });
    p.term.onResize(function (size) { if (p.id) post("/api/term/resize", { id: p.id, cols: size.cols, rows: size.rows }); });
    p.term.textarea && p.term.textarea.addEventListener("focus", function () {
      Object.keys(panes).forEach(function (k) { panes[k].el.classList.toggle("focus", panes[k] === p); });
    });
    if (window.ResizeObserver) new ResizeObserver(function () { try { p.fit.fit(); } catch (_) {} }).observe(p.body);
  }

  // Keystrokes are queued per target set so fast typing stays in order.
  var inputQueue = [];
  var sending = false;
  function typeInto(p, data) {
    if (!p.id) return;
    var ids = [p.id];
    if ($("gridBroadcast").checked && p.cast !== false) {
      ids = Object.keys(panes).map(function (k) { return panes[k]; }).filter(function (q) { return q.id && q.cast !== false; }).map(function (q) { return q.id; });
    }
    var key = ids.join(",");
    var last = inputQueue[inputQueue.length - 1];
    if (last && last.key === key) last.data += data;
    else inputQueue.push({ key: key, ids: ids, data: data });
    flush();
  }
  async function flush() {
    if (sending) return;
    sending = true;
    try {
      while (inputQueue.length) {
        var item = inputQueue.shift();
        var body = item.ids.length === 1 ? { id: item.ids[0], data: item.data } : { ids: item.ids, data: item.data };
        await post("/api/term/input", body).catch(function () {});
      }
    } finally { sending = false; }
  }

  async function openPane(p) {
    ensureTerm(p);
    setState(p, "connecting…");
    var size = p.term ? { cols: p.term.cols, rows: p.term.rows } : { cols: 80, rows: 24 };
    try {
      var res = await post("/api/term/open", { node: p.node === "local" ? "" : p.node, cols: size.cols, rows: size.rows });
      var body = await res.json().catch(function () { return {}; });
      if (!res.ok) throw new Error(body.error || ("HTTP " + res.status));
      p.id = body.id;
      p.offset = 0;
      if (p.term) p.term.reset();
      setState(p, "connected");
      saveOwned();
      restartStream();
    } catch (err) {
      setState(p, "failed: " + err.message);
    }
  }
  function closePane(p, kill) {
    if (kill && p.id) post("/api/term/close", { id: p.id });
    p.id = null;
    setState(p, "closed");
    saveOwned();
    restartStream();
  }

  function restartStream() {
    var live = Object.keys(panes).map(function (k) { return panes[k]; }).filter(function (p) { return p.id; });
    var key = live.map(function (p) { return p.id; }).sort().join(",");
    if (key === streamKey && stream) return;
    if (stream) stream.abort();
    stream = null;
    streamKey = key;
    if (!live.length) return;
    var ctrl = new AbortController();
    stream = ctrl;
    var q = live.map(function (p) { return p.id + ":" + p.offset; }).join(",");
    (async function () {
      try {
        var res = await fetch("/api/term/multi?s=" + q, { signal: ctrl.signal, cache: "no-store" });
        if (!res.ok) throw new Error("HTTP " + res.status);
        var reader = res.body.getReader(), decoder = new TextDecoder(), buf = "";
        while (true) {
          var chunk = await reader.read();
          if (chunk.done) break;
          buf += decoder.decode(chunk.value, { stream: true });
          var i;
          while ((i = buf.indexOf("\n\n")) >= 0) {
            var block = buf.slice(0, i);
            buf = buf.slice(i + 2);
            handleEvent(block);
          }
        }
      } catch (_) {}
      if (stream === ctrl) {
        stream = null;
        streamKey = "";
        setTimeout(restartStream, 1500);
      }
    })();
  }
  function byID(id) {
    var found = null;
    Object.keys(panes).forEach(function (k) { if (panes[k].id === id) found = panes[k]; });
    return found;
  }
  function b64bytes(b64) {
    var bin = atob(b64), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }
  function handleEvent(block) {
    var event = "message", data = "";
    block.split("\n").forEach(function (line) {
      if (line.indexOf("event:") === 0) event = line.slice(6).trim();
      else if (line.indexOf("data:") === 0) data += line.slice(5).trim();
    });
    var v;
    try { v = JSON.parse(data); } catch (_) { return; }
    var p = v && v.id ? byID(v.id) : null;
    if (!p) return;
    if (event === "output") {
      var bytes = b64bytes(v.data);
      if (p.term) p.term.write(bytes);
      p.offset = v.offset;
    } else if (event === "exit") {
      if (p.term) p.term.write("\r\n\x1b[2m[" + (v.gone ? "session ended" : "shell exited with code " + v.code) + "]\x1b[0m\r\n");
      p.id = null;
      setState(p, v.gone ? "closed" : "exited");
      saveOwned();
      restartStream();
    }
  }

  function wantedNodes(nodes) {
    var out = [];
    if ($("gridLocal").checked && info.local_terminal) out.push("local");
    nodes.forEach(function (n) {
      if (n.name !== "local" && n.healthy && n.status && n.status.terminal) out.push(n.name);
    });
    return out;
  }
  function syncPanes(nodes) {
    if ($("gridPanel").hidden) return;
    var want = wantedNodes(nodes);
    var grid = $("grid");
    if (!want.length && !Object.keys(panes).length) return;
    var hint = grid.querySelector(".hint");
    if (hint && want.length) hint.remove();
    want.forEach(function (node, i) {
      var p = panes[node] || makePane(node);
      if (grid.children[i] !== p.el) grid.insertBefore(p.el, grid.children[i] || null);
      if (!p.id && owned[node] && !p.tried) {
        // Re-attach the shell this grid had open before the page reloaded.
        p.tried = true;
        p.id = owned[node];
        ensureTerm(p);
        setState(p, "connected");
        restartStream();
      } else if (!p.id && p.state === "closed" && load("meshGridAuto") === "1" && !p.autoTried) {
        p.autoTried = true;
        openPane(p);
      }
    });
    Object.keys(panes).forEach(function (node) {
      if (want.indexOf(node) < 0 && !panes[node].id) {
        panes[node].el.remove();
        if (panes[node].term) panes[node].term.dispose();
        delete panes[node];
      }
    });
    updateNote();
  }

  function focusPane(node) {
    var p = panes[node];
    if (!p) return;
    p.el.scrollIntoView({ behavior: "smooth", block: "center" });
    if (!p.id) openPane(p);
    setTimeout(function () { if (p.term) p.term.focus(); }, 300);
  }

  async function init() {
    try {
      var res = await fetch("/api/info", { cache: "no-store" });
      if (!res.ok) return;
      info = await res.json();
    } catch (_) { return; }
    role = info.role || "local";
    $("fleetPanel").hidden = false;
    if (role === "worker") $("fleetTitle").textContent = "This computer · live";
    if (role === "host" && info.terminal && info.remote_terminal) {
      $("gridPanel").hidden = false;
      $("gridLocal").checked = load("meshGridLocal") === "1";
      $("gridCols").value = load("meshGridCols") || "2";
      $("grid").style.gridTemplateColumns = "repeat(" + $("gridCols").value + ", minmax(0, 1fr))";
      $("gridConnect").addEventListener("click", function () {
        store("meshGridAuto", "1");
        Object.keys(panes).forEach(function (k) { if (!panes[k].id) openPane(panes[k]); });
      });
      $("gridLocal").addEventListener("change", function () { store("meshGridLocal", $("gridLocal").checked ? "1" : "0"); if (fleetData) syncPanes(fleetData.nodes || []); });
      $("gridCols").addEventListener("change", function () {
        store("meshGridCols", $("gridCols").value);
        $("grid").style.gridTemplateColumns = "repeat(" + $("gridCols").value + ", minmax(0, 1fr))";
      });
      // Drop remembered sessions the server no longer has.
      try {
        var list = await (await fetch("/api/term/list", { cache: "no-store" })).json();
        var alive = {};
        (list.sessions || []).forEach(function (s) { alive[s.id] = s.node; });
        Object.keys(owned).forEach(function (node) { if (alive[owned[node]] !== (node === "local" ? "local" : node)) delete owned[node]; });
      } catch (_) { owned = {}; }
    }
    $("fleet").addEventListener("click", function (e) {
      var t = e.target.closest("button");
      if (!t) return;
      if (t.dataset.term) focusPane(t.dataset.term);
      if (t.dataset.task) {
        var box = document.querySelector("#taskNodes input[value=\"" + t.dataset.task + "\"]");
        if (box && !box.disabled) { box.checked = true; box.dispatchEvent(new Event("change")); }
        // The composer may not list this worker yet; it ticks it when it does.
        else window.__meshTickTask = t.dataset.task;
        $("taskPanel").scrollIntoView({ behavior: "smooth", block: "start" });
        setTimeout(function () { $("taskCmd").focus(); }, 300);
      }
    });
    pollFleet();
    setInterval(pollFleet, 2000);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
