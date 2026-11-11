// Drives real headless Chrome through the DevTools protocol against a live
// MeshCompute host and worker dashboard. Exits non-zero on the first failed
// check. Usage: node ui-test.mjs HOST_URL WORKER_URL OUT_DIR
import { spawn } from "node:child_process";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [hostURL, workerURL, outDir] = process.argv.slice(2);
const port = 9333 + Math.floor(Math.random() * 500);
const profile = mkdtempSync(join(outDir, "chrome-"));
const chromeBin = process.env.CHROME || "google-chrome";
const chrome = spawn(chromeBin, ["--headless=new", "--disable-gpu", "--no-sandbox", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`, "--window-size=1400,1800", "about:blank"], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
function check(ok, what, detail) {
  console.log((ok ? "PASS " : "FAIL ") + what + (ok || detail === undefined ? "" : "  -> " + JSON.stringify(detail).slice(0, 600)));
  if (!ok) failures++;
}

async function connect() {
  for (let i = 0; i < 50; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
      const page = list.find((t) => t.type === "page");
      if (page) return page.webSocketDebuggerUrl;
    } catch (_) {}
    await sleep(200);
  }
  throw new Error("chrome did not start");
}
const ws = new WebSocket(await connect());
await new Promise((r) => ws.addEventListener("open", r));
let seq = 0;
const pending = new Map();
const consoleErrors = [];
ws.addEventListener("message", (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
  if (msg.method === "Runtime.exceptionThrown") consoleErrors.push(msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text);
  if (msg.method === "Runtime.consoleAPICalled" && msg.params.type === "error") consoleErrors.push(msg.params.args.map((a) => a.value || a.description).join(" "));
});
function send(method, params = {}) {
  const id = ++seq;
  ws.send(JSON.stringify({ id, method, params }));
  return new Promise((r) => pending.set(id, (msg) => r(msg.result || msg)));
}
async function evaluate(expr) {
  const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error("page error: " + JSON.stringify(r.exceptionDetails).slice(0, 400));
  return r.result.value;
}
async function waitFor(what, expr, ms = 15000) {
  const end = Date.now() + ms;
  let v;
  while (Date.now() < end) {
    v = await evaluate(expr);
    if (v) { console.log("PASS " + what); return v; }
    await sleep(250);
  }
  check(false, what, v);
  return v;
}
async function shot(name) {
  const r = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
  writeFileSync(join(outDir, name), Buffer.from(r.data, "base64"));
}
async function typeText(text) {
  for (const ch of text) {
    if (ch === "\r") {
      await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" });
      await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
    } else {
      await send("Input.insertText", { text: ch });
    }
  }
}
const paneText = (i) => `(document.querySelectorAll('#grid .pane')[${i}]?.querySelector('.xterm-rows')?.innerText || '')`;
const focusPane = async (i) => {
  await evaluate(`document.querySelectorAll('#grid .pane')[${i}].querySelector('textarea').focus(), true`);
  await sleep(150);
};

try {
  await send("Runtime.enable");
  await send("Page.enable");
  // Start from a clean slate: close shells left by earlier runs.
  await send("Page.navigate", { url: hostURL });
  await waitFor("host page loaded (cleanup)", "document.readyState === 'complete'");
  await evaluate(`(async () => {
    const list = (await (await fetch('/api/term/list')).json()).sessions;
    for (const s of list) await fetch('/api/term/close', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: s.id }) });
    localStorage.clear();
    return list.length;
  })()`);
  await send("Page.navigate", { url: hostURL });
  await waitFor("host page loaded", "document.readyState === 'complete' && !!document.getElementById('fleet')");

  // ---- fleet ----
  const cards = await waitFor("fleet shows 3 computers", "document.querySelectorAll('#fleet .twin').length === 3 && document.querySelectorAll('#fleet .twin').length");
  check(cards === 3, "three fleet cards", cards);
  const rtts = await waitFor("both workers show a round trip", "Array.from(document.querySelectorAll('#fleet .rtt')).map(e => e.textContent).length >= 2 && Array.from(document.querySelectorAll('#fleet .rtt')).map(e => e.textContent)");
  check(Array.isArray(rtts) && rtts.every((t) => / ms$/.test(t)), "round-trip badges in ms", rtts);
  const meters = await evaluate("document.querySelector('#fleet .twin').innerText");
  check(/GPU/.test(meters) && /VRAM/.test(meters) && /Temp/.test(meters) && /Power/.test(meters) && /CPU/.test(meters) && /RAM/.test(meters), "host card shows GPU, VRAM, Temp, Power, CPU, RAM", meters);
  if (process.env.HAS_NVIDIA === "1") check(/°C/.test(meters) && !/no NVIDIA GPU/.test(meters), "real GPU name and temperature on the host card", meters);
  await waitFor("sparklines draw after a few samples", "document.querySelectorAll('#fleet svg.spark path').length >= 3", 12000);

  // ---- terminal grid ----
  await waitFor("grid panel visible with two worker panes", "!document.getElementById('gridPanel').hidden && document.querySelectorAll('#grid .pane').length === 2");
  // xterm.js only draws terminals that are on screen, like a real user scrolling there.
  await evaluate("document.getElementById('gridPanel').scrollIntoView({block: 'start'}), true");
  await evaluate("document.getElementById('gridConnect').click(), true");
  await waitFor("both panes connected", "Array.from(document.querySelectorAll('#grid .pane .status-text')).every(e => e.textContent === 'connected')");
  await waitFor("both panes show a shell prompt", `${paneText(0)}.trim().length > 0 && ${paneText(1)}.trim().length > 0`);
  await focusPane(1);
  await typeText("echo only-pane-$((40+2))\r");
  await waitFor("pane 2 runs the command typed into it", `${paneText(1)}.includes('only-pane-42')`);
  await sleep(800);
  check(!(await evaluate(`${paneText(0)}.includes('only-pane')`)), "pane 1 did not receive pane 2's input", await evaluate(paneText(0)));
  await evaluate("document.getElementById('gridBroadcast').click(), true");
  await focusPane(0);
  await typeText("echo bcast-$((7*6))\r");
  await waitFor("type-into-all reaches pane 1", `${paneText(0)}.includes('bcast-42')`);
  await waitFor("type-into-all reaches pane 2", `${paneText(1)}.includes('bcast-42')`);
  await evaluate("document.getElementById('gridBroadcast').click(), true");
  // Which computer is each pane on? Each shell reports the parent process's state directory.
  await focusPane(0);
  await typeText("ps -o args= -p $PPID | grep -o -- '--home [^ ]*' | sed 's#.*/#home-is-#'\r");
  await focusPane(1);
  await typeText("ps -o args= -p $PPID | grep -o -- '--home [^ ]*' | sed 's#.*/#home-is-#'\r");
  const p0 = await waitFor("pane 1 names its worker", `(${paneText(0)}.match(/home-is-(W2?)\\b/) || [])[1]`);
  const p1 = await waitFor("pane 2 names its worker", `(${paneText(1)}.match(/home-is-(W2?)\\b/) || [])[1]`);
  check(p0 && p1 && p0 !== p1, "the two panes run on two different workers", [p0, p1]);
  // Each worker card's terminal count must match the server's session list.
  const terms = await waitFor("fleet cards count the open terminals", `(async () => {
    const list = (await (await fetch('/api/term/list')).json()).sessions;
    const want = {};
    list.forEach(s => { want[s.node] = (want[s.node] || 0) + 1; });
    const cards = Array.from(document.querySelectorAll('#fleet .twin')).filter(c => c.id !== 'twin-local');
    return cards.length === 2 && cards.every(c => {
      const n = want[c.id.slice(5)] || 0;
      const items = c.querySelector('.twin-foot span').innerText.split(' · ');
      return n >= 1 && items.includes(n + (n === 1 ? ' terminal open' : ' terminals open'));
    });
  })()`);
  check(!!terms, "terminal counts on both worker cards match the server");
  await shot("host-grid.png");

  // ---- reload: panes re-attach with their scrollback ----
  await send("Page.reload");
  await sleep(1500);
  await waitFor("grid panel visible after reload", "!document.getElementById('gridPanel').hidden && document.querySelectorAll('#grid .pane').length === 2");
  await evaluate("document.getElementById('gridPanel').scrollIntoView({block: 'start'}), true");
  await waitFor("after reload both panes are connected again", "document.querySelectorAll('#grid .pane').length === 2 && Array.from(document.querySelectorAll('#grid .pane .status-text')).every(e => e.textContent === 'connected')");
  await waitFor("after reload pane 2 replays its earlier output", `${paneText(1)}.includes('only-pane-42')`);

  // ---- task from a fleet card's button ----
  const target = await evaluate("Array.from(document.querySelectorAll('#fleet button[data-task]')).map(b => b.dataset.task)");
  check(Array.isArray(target) && target.length === 2, "both worker cards offer Send task", target);
  const auto = await evaluate("(() => { const n = (window.__meshData.nodes || []).find(n => n.status && n.status.tasks === 'auto'); return n && n.name; })()");
  await evaluate(`document.querySelector('#fleet button[data-task="${auto}"]').click(), true`);
  await waitFor("Send task ticks that worker in the composer", `document.querySelector('#taskNodes input[value="${auto}"]').checked`);
  await evaluate("document.getElementById('taskCmd').value = 'echo task-from-ui-$((1+1)); uname -s', true");
  await evaluate("document.getElementById('taskSend').click(), true");
  await waitFor("task succeeds on the auto worker", "Array.from(document.querySelectorAll('#taskList .task')).some(t => /succeeded/.test(t.innerText) && /task-from-ui-2/.test(t.innerText) && /Linux/.test(t.innerText))", 20000);
  await waitFor("its card counts the finished tasks", `(async () => {
    const tasks = (await (await fetch('/api/tasks')).json()).tasks.filter(t => t.node === '${auto}' && ['succeeded','failed','denied','cancelled','expired'].includes(t.state));
    const card = document.getElementById('twin-${auto}');
    return tasks.length >= 1 && card.querySelector('.twin-foot span').innerText.split(' · ').includes(tasks.length + ' finished');
  })()`, 8000);
  await shot("host-full.png");

  // ---- worker dashboard: approve with a click ----
  const ask = await evaluate("(() => { const n = (window.__meshData.nodes || []).find(n => n.status && n.status.tasks === 'ask'); return n && n.name; })()");
  await evaluate(`(() => { document.querySelectorAll('#taskNodes input').forEach(i => { i.checked = i.value === '${ask}'; i.dispatchEvent(new Event('change')); }); document.getElementById('taskCmd').value = 'echo approved-by-click'; document.getElementById('taskSend').click(); return true; })()`);
  await waitFor("host shows it waiting for approval", "Array.from(document.querySelectorAll('#taskList .task')).some(t => /waiting for approval/.test(t.innerText) && /approved-by-click/.test(t.innerText))");
  await send("Page.navigate", { url: workerURL });
  await waitFor("worker page loaded", "document.readyState === 'complete' && !document.getElementById('workerPanel').hidden");
  await waitFor("worker fleet shows only this computer", "document.querySelectorAll('#fleet .twin').length === 1 && /this computer/.test(document.querySelector('#fleet .twin').innerText)");
  check(await evaluate("document.getElementById('gridPanel').hidden"), "no terminal grid on a worker");
  await waitFor("Approve button appears", "Array.from(document.querySelectorAll('#taskList .task.approval button')).some(b => b.textContent === 'Approve')");
  await shot("worker-approval.png");
  await evaluate("Array.from(document.querySelectorAll('#taskList .task.approval button')).find(b => b.textContent === 'Approve').click(), true");
  await waitFor("worker shows it succeeded", "Array.from(document.querySelectorAll('#taskList .task')).some(t => /succeeded/.test(t.innerText) && /approved-by-click/.test(t.innerText))");
  await send("Page.navigate", { url: hostURL });
  await waitFor("host sees the approved task succeed", "Array.from(document.querySelectorAll('#taskList .task')).some(t => /succeeded/.test(t.innerText) && /approved-by-click/.test(t.innerText))");

  check(consoleErrors.length === 0, "no JavaScript errors", consoleErrors);
} catch (err) {
  check(false, "script error", String(err && err.stack || err));
} finally {
  ws.close();
  chrome.kill();
}
console.log(failures ? `${failures} FAILED` : "ALL PASSED");
process.exit(failures ? 1 : 0);
