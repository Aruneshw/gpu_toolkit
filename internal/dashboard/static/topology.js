// Network map (group host only): how each worker reaches this host (path and
// round trip through the tunnel), which workers forward for others (hops),
// and the measured connection time between workers that allow hops.
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function short(id) { return id && id.indexOf("worker-") === 0 ? id.slice(7, 13) : (id || ""); }
  function ms(v) {
    if (typeof v !== "number") return "—";
    return v < 0.05 ? "<1 ms" : v < 10 ? v.toFixed(1) + " ms" : Math.round(v) + " ms";
  }
  var pathLabel = { relay: "relay", mesh: "mesh VPN", direct: "direct", local: "same computer", hop: "hop" };

  function draw(t) {
    var nodes = t.nodes || [];
    var W = 640, H = 440, cx = W / 2, cy = H / 2, R = Math.min(W, H) / 2 - 58;
    var pos = {};
    nodes.forEach(function (n, i) {
      var a = -Math.PI / 2 + (2 * Math.PI * i) / Math.max(nodes.length, 1);
      pos[n.id] = { x: cx + R * Math.cos(a), y: cy + R * Math.sin(a) };
    });
    var lines = [], labels = [];
    function label(x, y, text, cls, anchor) {
      labels.push("<text class=\"tl " + (cls || "") + "\" x=\"" + x.toFixed(1) + "\" y=\"" + y.toFixed(1) + "\"" + (anchor ? " style=\"text-anchor:" + anchor + "\"" : "") + ">" + esc(text) + "</text>");
    }
    // Worker-to-worker measurements: one thin line per pair.
    var seen = {};
    (t.pairs || []).forEach(function (p) {
      var key = p.from < p.to ? p.from + p.to : p.to + p.from;
      if (seen[key] || !pos[p.from] || !pos[p.to]) return;
      var back = (t.pairs || []).find(function (q) { return q.from === p.to && q.to === p.from; });
      seen[key] = true;
      var a = pos[p.from], b = pos[p.to];
      var vals = [p.ms, back && back.ms].filter(function (v) { return typeof v === "number"; });
      lines.push("<line class=\"tp" + (vals.length ? "" : " none") + "\" data-pair x1=\"" + a.x + "\" y1=\"" + a.y + "\" x2=\"" + b.x + "\" y2=\"" + b.y + "\"/>");
      // Between the two workers, a little off the line towards the outside.
      var mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2, ox = mx - cx, oy = my - cy, od = Math.sqrt(ox * ox + oy * oy) || 1;
      label(mx + ox / od * 10, my + oy / od * 10 + 3, vals.length ? ms(Math.min.apply(null, vals)) : "unreachable", "pair");
    });
    // Each worker's own link: to the host, or to the worker it hops through.
    nodes.forEach(function (n) {
      var a = pos[n.id];
      var to = n.path === "hop" && pos[n.hop_via] ? pos[n.hop_via] : { x: cx, y: cy };
      lines.push("<line class=\"tw path-" + esc(n.path || "none") + "\" data-link=\"" + esc(n.id) + "\" x1=\"" + a.x + "\" y1=\"" + a.y + "\" x2=\"" + to.x + "\" y2=\"" + to.y + "\"/>");
      // Just outside the worker's dot, away from the centre, so labels do
      // not sit on the lines.
      var dx = a.x - cx, dy = a.y - cy, d = Math.sqrt(dx * dx + dy * dy) || 1;
      var text = (pathLabel[n.path] || "?") + " · " + ms(n.rtt_ms), cls = "link path-" + (n.path || "none");
      if (Math.abs(dx) > Math.abs(dy) * 1.5) label(a.x + (dx > 0 ? 30 : -30), a.y + 4, text, cls, dx > 0 ? "start" : "end"); // beside it
      else label(a.x + dx / d * 36, a.y + dy / d * 36 + (dy >= 0 ? 12 : -2), text, cls); // above or below it
    });
    var dots = "<circle class=\"th\" cx=\"" + cx + "\" cy=\"" + cy + "\" r=\"24\"/><text class=\"tn\" x=\"" + cx + "\" y=\"" + (cy + 4) + "\">host</text>";
    nodes.forEach(function (n) {
      var p = pos[n.id];
      var hop = n.hop ? " hopper" : "";
      dots += "<g data-node=\"" + esc(n.id) + "\"><title>" + esc(n.id) + (n.hop ? " · forwards for others at " + (n.hop.endpoints || []).join(", ") : "") + "</title>" +
        "<circle class=\"tw-dot" + hop + "\" cx=\"" + p.x.toFixed(1) + "\" cy=\"" + p.y.toFixed(1) + "\" r=\"23\"/>" +
        "<text class=\"tn\" x=\"" + p.x.toFixed(1) + "\" y=\"" + (p.y + 4).toFixed(1) + "\">" + esc(short(n.id)) + "</text>" +
        (n.hop ? "<text class=\"tl hopnote\" x=\"" + p.x.toFixed(1) + "\" y=\"" + (p.y < cy ? p.y + 37 : p.y - 29).toFixed(1) + "\">forwards · " + n.hop.active + " now</text>" : "") + "</g>";
    });
    return "<svg class=\"topo\" viewBox=\"0 0 " + W + " " + H + "\" role=\"img\" aria-label=\"Network map: the host in the middle, workers around it\">" +
      lines.join("") + dots + labels.join("") + "</svg>";
  }

  function matrix(t) {
    var hoppers = (t.nodes || []).filter(function (n) { return n.hop; });
    if (hoppers.length < 2) return "";
    var cell = {};
    (t.pairs || []).forEach(function (p) { cell[p.from + ">" + p.to] = p.ms; });
    var h = "<table class=\"topo-matrix\"><thead><tr><th>from \\ to</th>" + hoppers.map(function (n) { return "<th title=\"" + esc(n.id) + "\">" + esc(short(n.id)) + "</th>"; }).join("") + "</tr></thead><tbody>";
    hoppers.forEach(function (a) {
      h += "<tr><th title=\"" + esc(a.id) + "\">" + esc(short(a.id)) + "</th>";
      hoppers.forEach(function (b) {
        if (a.id === b.id) { h += "<td class=\"self\">·</td>"; return; }
        var k = a.id + ">" + b.id;
        var v = cell[k];
        h += "<td data-cell=\"" + esc(k) + "\">" + (k in cell ? (typeof v === "number" ? ms(v) : "unreachable") : "…") + "</td>";
      });
      h += "</tr>";
    });
    return h + "</tbody></table>";
  }

  async function poll() {
    if (document.hidden) return;
    var t;
    try {
      var res = await fetch("/api/topology", { cache: "no-store" });
      if (!res.ok) return;
      t = await res.json();
    } catch (_) { return; }
    var nodes = t.nodes || [];
    var hops = nodes.filter(function (n) { return n.hop; }).length;
    var via = nodes.filter(function (n) { return n.path === "hop"; }).length;
    $("topoNote").textContent = nodes.length + " worker(s) · " + hops + " forward for others · " + via + " reach the host through another worker" +
      (t.measured_at && t.measured_at.indexOf("0001") !== 0 ? " · measured " + new Date(t.measured_at).toLocaleTimeString() : "");
    $("topoMap").innerHTML = nodes.length ? draw(t) : "<div class=\"hint\">No worker is connected.</div>";
    $("topoMatrix").innerHTML = matrix(t) || "<div class=\"hint\">Worker-to-worker times appear once two workers allow hops: on each, <code>meshcompute join --allow-hop</code> or <code>meshcompute config set allow_hop true</code> (then reconnect). A worker that allows hops listens on its LAN and mesh addresses and forwards other workers' encrypted connections to this host.</div>";
  }

  async function init() {
    var info;
    try {
      var res = await fetch("/api/info", { cache: "no-store" });
      if (!res.ok) return;
      info = await res.json();
    } catch (_) { return; }
    if (info.role !== "host") return;
    $("topoPanel").hidden = false;
    poll();
    setInterval(poll, 5000);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
