// Tasks: on a group host, send commands to workers and watch them run; on a
// worker's own dashboard, approve or refuse what the host sends and change
// what the host may do here.
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var role = "local";
  var views = {};      // task id -> {el, out, offset, decoder, open, done}
  var checked = {};    // worker name -> ticked in the composer
  var FINAL = { succeeded: 1, failed: 1, denied: 1, cancelled: 1, expired: 1 };
  var STATE_TEXT = {
    sending: "sending", approval: "waiting for approval", running: "running",
    succeeded: "succeeded", failed: "failed", denied: "denied", cancelled: "cancelled", expired: "expired"
  };

  function post(url, body) {
    return fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  }
  async function errorOf(res) {
    var body = await res.json().catch(function () { return {}; });
    return body.error || ("HTTP " + res.status);
  }
  function short(name) { return name && name.indexOf("worker-") === 0 ? name.slice(0, 15) + "…" : (name || ""); }
  function when(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    return isNaN(d) ? "" : d.toLocaleTimeString();
  }
  function elapsed(info) {
    if (!info.started) return "";
    var end = info.ended ? new Date(info.ended) : new Date();
    var s = Math.max(0, Math.round((end - new Date(info.started)) / 1000));
    return s < 60 ? s + "s" : Math.floor(s / 60) + "m " + (s % 60) + "s";
  }
  // Terminal colour and cursor codes are noise in a plain text view.
  function stripAnsi(text) {
    return text.replace(/\x1b\[[0-9;?]*[ -\/]*[@-~]/g, "").replace(/\x1b\][^\x07]*(\x07|\x1b\\)/g, "").replace(/\r(?!\n)/g, "\n");
  }
  function b64bytes(b64) {
    var bin = atob(b64);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }
  function button(text, cls, onClick) {
    var b = document.createElement("button");
    b.type = "button";
    b.textContent = text;
    if (cls) b.className = cls;
    b.addEventListener("click", onClick);
    return b;
  }

  // ---- composer (host) ----
  function renderNodes() {
    var data = window.__meshData;
    if (!data) return;
    var workers = (data.nodes || []).filter(function (n) { return n.name.indexOf("worker-") === 0; });
    var box = $("taskNodes");
    if (!workers.length) { box.innerHTML = "<span class=\"hint\">No workers connected. Invite a friend first.</span>"; return; }
    var key = workers.map(function (n) { return n.name + (n.healthy ? 1 : 0) + (n.status && n.status.tasks); }).join("|");
    if (box.dataset.key === key && !window.__meshTickTask) return;
    box.dataset.key = key;
    box.textContent = "";
    workers.forEach(function (n) {
      var label = document.createElement("label");
      var input = document.createElement("input");
      input.type = "checkbox";
      input.value = n.name;
      var policy = n.status && n.status.tasks;
      input.disabled = !n.healthy || policy === "off";
      if (window.__meshTickTask === n.name && !input.disabled) { checked[n.name] = true; window.__meshTickTask = null; }
      input.checked = !input.disabled && !!checked[n.name];
      input.addEventListener("change", function () { checked[n.name] = input.checked; });
      label.appendChild(input);
      var text = short(n.name);
      if (!n.healthy) text += " (offline)";
      else if (policy === "off") text += " (tasks off)";
      else if (policy === "auto") text += " (runs automatically)";
      else if (policy === "ask") text += " (asks the owner)";
      label.appendChild(document.createTextNode(text));
      label.title = n.name;
      box.appendChild(label);
    });
  }

  async function send() {
    var nodes = Array.prototype.filter.call($("taskNodes").querySelectorAll("input:checked"), function (i) { return !i.disabled; }).map(function (i) { return i.value; });
    var command = $("taskCmd").value;
    if (!nodes.length) { $("taskMsg").textContent = "tick at least one worker"; return; }
    if (!command.trim()) { $("taskMsg").textContent = "type a command"; return; }
    $("taskSend").disabled = true;
    $("taskMsg").textContent = "sending…";
    try {
      var res = await post("/api/tasks", { nodes: nodes, command: command, shell: $("taskShell").value, dir: $("taskDir").value });
      if (!res.ok) throw new Error(await errorOf(res));
      var body = await res.json();
      (body.tasks || []).forEach(function (t) { ensure(t).open = true; });
      $("taskMsg").textContent = "sent to " + nodes.length + (nodes.length === 1 ? " worker" : " workers");
      refresh();
    } catch (err) {
      $("taskMsg").textContent = "failed: " + err.message;
    } finally {
      $("taskSend").disabled = false;
    }
  }

  // ---- task list (both) ----
  function ensure(info) {
    var v = views[info.id];
    if (v) return v;
    v = { offset: 0, decoder: new TextDecoder(), open: role === "worker" && !FINAL[info.state], done: false, polling: false };
    v.el = document.createElement("div");
    v.el.className = "task";
    v.head = document.createElement("div");
    v.head.className = "task-head";
    v.detail = document.createElement("div");
    v.detail.className = "task-detail";
    v.cmd = document.createElement("pre");
    v.cmd.className = "task-cmd";
    v.out = document.createElement("pre");
    v.out.className = "task-out";
    v.el.appendChild(v.head);
    v.el.appendChild(v.detail);
    v.el.appendChild(v.cmd);
    v.el.appendChild(v.out);
    views[info.id] = v;
    return v;
  }

  function renderTask(info) {
    var v = ensure(info);
    v.info = info;
    var times = when(info.created);
    if (elapsed(info)) times += " · " + elapsed(info);
    if (info.exit_code != null) times += " · exit " + info.exit_code;
    var key = [info.state, info.detail, info.exit_code, v.open].join("|");
    if (v.key === key) { v.meta.textContent = times; return v; }
    v.key = key;
    v.el.className = "task " + info.state;
    v.head.textContent = "";
    var badge = document.createElement("span");
    badge.className = "task-state " + info.state;
    badge.textContent = STATE_TEXT[info.state] || info.state;
    if (role === "host") {
      var who = document.createElement("b");
      who.textContent = short(info.node);
      who.title = info.node;
      v.head.appendChild(who);
    }
    v.head.appendChild(badge);
    var code = document.createElement("code");
    code.textContent = info.command.split("\n")[0] + (info.command.indexOf("\n") >= 0 ? " …" : "");
    code.title = info.command;
    v.head.appendChild(code);
    v.meta = document.createElement("span");
    v.meta.className = "status-text";
    v.meta.textContent = times;
    v.head.appendChild(v.meta);
    if (role === "worker" && info.state === "approval") {
      v.head.appendChild(button("Approve", "", function () { decide(info.id, true, false); }));
      v.head.appendChild(button("Always allow", "secondary", function () { decide(info.id, true, true); }));
      v.head.appendChild(button("Deny", "secondary", function () { decide(info.id, false, false); }));
    }
    v.head.appendChild(button(v.open ? "Hide output" : "Output", "secondary", function () { v.open = !v.open; v.key = ""; renderTask(v.info); if (v.open) poll(v); }));
    if (!FINAL[info.state]) v.head.appendChild(button(role === "worker" ? "Stop" : "Cancel", "secondary", function () { cancel(info.id); }));
    var detail = info.detail || "";
    if (role === "worker" && info.state === "approval") detail = "The group host wants to run this as your user account" + (info.dir ? " in " + info.dir : " in your home folder") + " (shell: " + info.shell + "). It runs only if you approve.";
    v.detail.textContent = detail;
    v.detail.hidden = !detail;
    v.cmd.textContent = info.command;
    v.cmd.hidden = !(role === "worker" && info.state === "approval") && !(v.open && info.command.indexOf("\n") >= 0);
    v.out.hidden = !v.open || info.state === "approval" || info.state === "sending";
    return v;
  }

  async function poll(v) {
    if (v.polling || !v.open || v.done) return;
    v.polling = true;
    try {
      var res = await fetch("/api/tasks/output?id=" + encodeURIComponent(v.info.id) + "&offset=" + v.offset, { cache: "no-store" });
      if (!res.ok) return;
      var body = await res.json();
      if (body.data) {
        if (body.offset > v.offset && v.offset > 0) v.out.textContent += "\n[… earlier output no longer kept …]\n";
        var text = v.decoder.decode(b64bytes(body.data), { stream: true });
        var atBottom = v.out.scrollTop + v.out.clientHeight >= v.out.scrollHeight - 8;
        v.out.textContent += stripAnsi(text);
        if (v.out.textContent.length > 400000) v.out.textContent = v.out.textContent.slice(-300000);
        if (atBottom) v.out.scrollTop = v.out.scrollHeight;
      }
      v.offset = body.next;
      if (FINAL[body.info.state] && body.next >= body.info.output) v.done = true;
      if (!v.out.textContent && v.done) v.out.textContent = "(no output)";
    } catch (_) {
    } finally {
      v.polling = false;
    }
  }

  async function decide(id, approve, always) {
    var res = await post("/api/tasks/decide", { id: id, approve: approve, always: always });
    if (!res.ok) alertLine(await errorOf(res));
    if (always) loadWorker();
    refresh();
  }
  async function cancel(id) {
    var res = await post("/api/tasks/cancel", { id: id });
    if (!res.ok) alertLine(await errorOf(res));
    refresh();
  }
  function alertLine(text) { $("taskNote").textContent = text; }

  async function refresh() {
    if (role === "host") renderNodes();
    try {
      var res = await fetch("/api/tasks", { cache: "no-store" });
      if (!res.ok) return;
      var body = await res.json();
      var list = $("taskList");
      var tasks = body.tasks || [];
      if (!tasks.length) {
        list.innerHTML = "<div class=\"hint\">" + (role === "worker" ? "The group host has not sent any task yet." : "No tasks yet.") + "</div>";
        $("taskNote").textContent = "";
        return;
      }
      var hint = list.querySelector(".hint");
      if (hint) hint.remove();
      var waiting = 0, running = 0;
      tasks.forEach(function (info, i) {
        if (info.state === "approval") waiting++;
        if (info.state === "running") running++;
        var v = renderTask(info);
        if (list.children[i] !== v.el) list.insertBefore(v.el, list.children[i] || null);
        if (v.open) poll(v);
      });
      while (list.children.length > tasks.length) list.removeChild(list.lastChild);
      var note = running + " running";
      if (waiting) note += " · " + waiting + (role === "worker" ? " waiting for YOUR approval" : " waiting for approval");
      $("taskNote").textContent = note;
      $("taskNote").style.color = waiting && role === "worker" ? "var(--warn)" : "";
      if (role === "worker") document.title = (waiting ? "(" + waiting + ") " : "") + "MeshCompute worker";
    } catch (_) {}
  }

  // ---- worker settings ----
  var settingsBusy = false;
  async function loadWorker() {
    try {
      var res = await fetch("/api/worker", { cache: "no-store" });
      if (!res.ok) return;
      var body = await res.json();
      var link = body.link || {};
      var state = $("linkState");
      state.textContent = link.connected ? "connected to " + link.endpoint + (link.since ? " since " + when(link.since) : "") : "not connected" + (link.endpoint ? " (retrying " + link.endpoint + ")" : "");
      state.style.color = link.connected ? "var(--accent)" : "var(--bad)";
      if (!settingsBusy) {
        $("setTerminal").checked = !!body.settings.allow_terminal;
        $("setShare").checked = !!body.settings.share_gpu;
        $("setHop").checked = !!body.settings.allow_hop;
        $("setTasks").value = body.settings.task_approval;
      }
      $("closeShells").textContent = "Close the host's terminals (" + body.terminals + " open)";
      $("closeShells").disabled = !body.terminals;
    } catch (_) {}
  }
  async function saveWorker() {
    settingsBusy = true;
    $("workerMsg").textContent = "saving…";
    try {
      var res = await post("/api/worker", { allow_terminal: $("setTerminal").checked, share_gpu: $("setShare").checked, allow_hop: $("setHop").checked, task_approval: $("setTasks").value });
      $("workerMsg").textContent = res.ok ? "saved" : "failed: " + await errorOf(res);
    } finally {
      settingsBusy = false;
      loadWorker();
    }
  }

  async function init() {
    var info;
    try {
      var res = await fetch("/api/info", { cache: "no-store" });
      if (!res.ok) return;
      info = await res.json();
    } catch (_) { return; }
    role = info.role || "local";
    if (!info.tasks) return;
    $("taskPanel").hidden = false;
    if (role === "host") {
      $("taskCompose").hidden = false;
      $("taskSend").addEventListener("click", send);
      $("taskCmd").addEventListener("keydown", function (e) { if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) send(); });
    }
    if (role === "worker") {
      $("taskTitle").textContent = "Tasks from the group host";
      $("workerPanel").hidden = false;
      $("promptPanel").hidden = true;
      document.querySelector("header p").textContent = "This computer, as a worker of a group";
      var foot = document.querySelector(".footer");
      if (foot) foot.textContent = "Only the group host you joined can send tasks or open terminals here, and only in the ways you allow above. Every task shows its exact command before it runs.";
      new MutationObserver(function () {
        var scope = $("scope");
        var want = "This is your own view. The group host sees this computer's GPU and can use it as you allow below.";
        if (scope.textContent !== want) scope.textContent = want;
      }).observe($("scope"), { childList: true, characterData: true, subtree: true });
      $("scope").textContent = "";
      ["setTerminal", "setShare", "setHop", "setTasks"].forEach(function (id) { $(id).addEventListener("change", saveWorker); });
      $("closeShells").addEventListener("click", async function () { await post("/api/worker/terminals/close", {}); loadWorker(); });
      loadWorker();
      setInterval(loadWorker, 3000);
    }
    refresh();
    setInterval(refresh, 1500);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
