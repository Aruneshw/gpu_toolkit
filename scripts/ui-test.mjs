// Drives real headless Chrome through the DevTools protocol against a live
// MeshCompute host and worker dashboard. Exits non-zero on the first failed
// check. Usage: node ui-test.mjs HOST_URL WORKER_URL OUT_DIR
import { spawn } from "node:child_process";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [hostURL, workerURL, outDir] = process.argv.slice(2);
// How many workers the host has (scripts/ui-test.sh starts them).
const N = Number(process.env.EXPECT_WORKERS || 2);
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
  const cards = await waitFor(`fleet shows ${N + 1} computers`, `document.querySelectorAll('#fleet .twin').length === ${N + 1} && document.querySelectorAll('#fleet .twin').length`, 30000);
  check(cards === N + 1, "one fleet card per computer", cards);
  const rtts = await waitFor("every worker shows a round trip", `Array.from(document.querySelectorAll('#fleet .rtt')).length >= ${N} && Array.from(document.querySelectorAll('#fleet .rtt')).map(e => e.textContent)`, 20000);
  check(Array.isArray(rtts) && rtts.every((t) => / ms$/.test(t)), "round-trip badges in ms", rtts);
  await waitFor("every worker's round trip is measured through the tunnel", `(() => {
    const badges = Array.from(document.querySelectorAll('#fleet .twin:not(#twin-local) [data-rtt]'));
    return badges.length === ${N} && badges.every(b => /tunnel/.test(b.title));
  })()`, 20000);
  check(await evaluate(`Array.from(document.querySelectorAll('#fleet .twin:not(#twin-local) [data-path]')).filter(b => b.dataset.path === 'local').length === ${N}`), "every worker card names its path (same computer here)");
  const meters = await evaluate("document.querySelector('#fleet .twin').innerText");
  check(/GPU/.test(meters) && /VRAM/.test(meters) && /Temp/.test(meters) && /Power/.test(meters) && /CPU/.test(meters) && /RAM/.test(meters), "host card shows GPU, VRAM, Temp, Power, CPU, RAM", meters);
  if (process.env.HAS_NVIDIA === "1") check(/°C/.test(meters) && !/no NVIDIA GPU/.test(meters), "real GPU name and temperature on the host card", meters);
  await waitFor("sparklines draw after a few samples", "document.querySelectorAll('#fleet svg.spark path').length >= 3", 12000);

  // ---- terminal grid ----
  await waitFor(`grid panel visible with ${N} worker panes`, `!document.getElementById('gridPanel').hidden && document.querySelectorAll('#grid .pane').length === ${N}`);
  // xterm.js only draws terminals that are on screen, like a real user scrolling there.
  await evaluate("document.getElementById('gridPanel').scrollIntoView({block: 'start'}), true");
  await evaluate("document.getElementById('gridConnect').click(), true");
  await waitFor("every pane connected", "Array.from(document.querySelectorAll('#grid .pane .status-text')).every(e => e.textContent === 'connected')");
  await waitFor("every pane shows a shell prompt", `Array.from(document.querySelectorAll('#grid .pane .xterm-rows')).filter(r => r.innerText.trim().length > 0).length === ${N}`);
  await focusPane(1);
  await typeText("echo only-pane-$((40+2))\r");
  await waitFor("pane 2 runs the command typed into it", `${paneText(1)}.includes('only-pane-42')`);
  await sleep(800);
  check(!(await evaluate(`${paneText(0)}.includes('only-pane')`)), "pane 1 did not receive pane 2's input", await evaluate(paneText(0)));
  await evaluate("document.getElementById('gridBroadcast').click(), true");
  await focusPane(0);
  await typeText("echo bcast-$((7*6))\r");
  await waitFor(`type-into-all reaches all ${N} panes`, `Array.from(document.querySelectorAll('#grid .pane .xterm-rows')).filter(r => r.innerText.includes('bcast-42')).length === ${N}`);
  await evaluate("document.getElementById('gridBroadcast').click(), true");
  // Which computer is each pane on? Each shell reports the parent process's state directory.
  const homes = [];
  for (let i = 0; i < N; i++) {
    await focusPane(i);
    await typeText("ps -o args= -p $PPID | grep -o -- '--home [^ ]*' | sed 's#.*/#home-is-#'\r");
    homes.push(await waitFor(`pane ${i + 1} names its worker`, `(${paneText(i)}.match(/home-is-(W[0-9]*)\\b/) || [])[1]`));
  }
  check(new Set(homes).size === N && homes.every(Boolean), `the ${N} panes run on ${N} different workers`, homes);
  // Each worker card's terminal count must match the server's session list.
  const terms = await waitFor("fleet cards count the open terminals", `(async () => {
    const list = (await (await fetch('/api/term/list')).json()).sessions;
    const want = {};
    list.forEach(s => { want[s.node] = (want[s.node] || 0) + 1; });
    const cards = Array.from(document.querySelectorAll('#fleet .twin')).filter(c => c.id !== 'twin-local');
    return cards.length === ${N} && cards.every(c => {
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
  await waitFor("grid panel visible after reload", `!document.getElementById('gridPanel').hidden && document.querySelectorAll('#grid .pane').length === ${N}`);
  await evaluate("document.getElementById('gridPanel').scrollIntoView({block: 'start'}), true");
  await waitFor("after reload every pane is connected again", `document.querySelectorAll('#grid .pane').length === ${N} && ` + "Array.from(document.querySelectorAll('#grid .pane .status-text')).every(e => e.textContent === 'connected')");
  await waitFor("after reload pane 2 replays its earlier output", `${paneText(1)}.includes('only-pane-42')`);

  // ---- task from a fleet card's button ----
  const target = await evaluate("Array.from(document.querySelectorAll('#fleet button[data-task]')).map(b => b.dataset.task)");
  check(Array.isArray(target) && target.length === N, "every worker card offers Send task", target);
  const auto = await evaluate("(() => { const n = (window.__meshData.nodes || []).find(n => n.status && n.status.tasks === 'auto'); return n && n.name; })()");
  await evaluate(`document.querySelector('#fleet button[data-task="${auto}"]').click(), true`);
  await waitFor("Send task ticks that worker in the composer", `!!document.querySelector('#taskNodes input[value="${auto}"]')?.checked`);
  await evaluate("document.getElementById('taskCmd').value = 'echo task-from-ui-$((1+1)); uname -s', true");
  await evaluate("document.getElementById('taskSend').click(), true");
  await waitFor("task succeeds on the auto worker", "Array.from(document.querySelectorAll('#taskList .task')).some(t => /succeeded/.test(t.innerText) && /task-from-ui-2/.test(t.innerText) && /Linux/.test(t.innerText))", 20000);
  await waitFor("its card counts the finished tasks", `(async () => {
    const tasks = (await (await fetch('/api/tasks')).json()).tasks.filter(t => t.node === '${auto}' && ['succeeded','failed','denied','cancelled','expired'].includes(t.state));
    const card = document.getElementById('twin-${auto}');
    return tasks.length >= 1 && card.querySelector('.twin-foot span').innerText.split(' · ').includes(tasks.length + ' finished');
  })()`, 8000);
  await shot("host-full.png");

  // ---- models & pools (only with real llama.cpp) ----
  if (process.env.POOL_MODEL) {
    const modelDir = process.env.POOL_MODEL.replace(/[\\/][^\\/]*$/, "");
    await waitFor("models & pools panel visible", "!document.getElementById('poolPanel').hidden");
    await evaluate("document.getElementById('poolPanel').scrollIntoView(), true");
    await evaluate(`(() => { const d = document.getElementById('scanDir'); d.value = ${JSON.stringify(modelDir)}; document.getElementById('scanBtn').click(); return true; })()`);
    await waitFor("the folder scan lists the model", "document.querySelectorAll('#scanOut button[data-add]').length >= 1");
    await evaluate("(() => { const i = document.querySelector('#scanOut input[data-name]'); i.value = 'uimodel'; document.querySelector('#scanOut button[data-add]').click(); return true; })()");
    await waitFor("the model is registered from the page", "Array.from(document.querySelectorAll('#modelRows b')).some(b => b.textContent === 'uimodel')", 60000);
    await waitFor("workers lend their GPUs", `document.getElementById('poolState').textContent.includes('${N} worker(s) lending')`, 30000);
    await evaluate("document.getElementById('poolNew').click(), true");
    await waitFor("pool editor opens", "!document.getElementById('poolEditor').hidden && document.querySelectorAll('#edWorkers input').length === " + N);
    await evaluate(`(() => {
      const $ = id => document.getElementById(id);
      $('edName').value = 'ui'; $('edName').dispatchEvent(new Event('input'));
      $('edModel').value = 'uimodel';
      $('edContext').value = 2048; $('edParallel').value = 2;
      const boxes = document.querySelectorAll('#edWorkers input');
      boxes[0].checked = true; boxes[1].checked = true; boxes[0].dispatchEvent(new Event('change', { bubbles: true }));
      $('edSplitMode').value = 'manual'; $('edSplitMode').dispatchEvent(new Event('change'));
      const w = document.querySelectorAll('#edSplit input');
      w[0].value = 1; w[1].value = 1; w[2].value = 3;
      $('edCtk').value = 'q8_0'; $('edCtv').value = 'q8_0'; $('edFA').value = 'on';
      window.__heavy = boxes[1].value; window.__light = boxes[0].value;
      $('edSave').click();
      return w.length;
    })()`);
    await waitFor("the pool is ready", "(() => { const c = document.querySelector('.poolcard[data-pool=\"ui\"]'); return c && /ready/.test(c.querySelector('.poolcard-head').innerText); })()", 120000);
    const table = await waitFor("the memory table shows this computer and two workers", "(() => { const rows = Array.from(document.querySelectorAll('.poolcard[data-pool=\"ui\"] tbody tr')).map(r => Array.from(r.cells).map(c => c.innerText)); return rows.length === 3 && rows; })()", 30000);
    if (Array.isArray(table)) {
      const mibOf = (r) => parseFloat(r[2]) * (/GiB/.test(r[2]) ? 1024 : 1);
      // llama.cpp splits whole layers and puts the output layer last, so
      // weights 1:3 give roughly, not exactly, 3x.
      const heavy = (await evaluate("window.__heavy")).slice(0, 15), light = (await evaluate("window.__light")).slice(0, 15);
      const row = (id) => table.find(r => r[0].startsWith(id));
      const ratio = row(heavy) && row(light) ? mibOf(row(heavy)) / mibOf(row(light)) : 0;
      check(ratio > 2.5 && ratio < 4.5, "the worker with weight 3 holds about 3x the weights of the one with weight 1", { ratio, table });
      check(table.every(r => r[2] !== "—"), "every computer holds weights", table);
    }
    check(/q8_0\/q8_0/.test(await evaluate("document.querySelector('.poolcard[data-pool=\"ui\"]').innerText")), "the card shows the q8_0 KV cache");
    await waitFor("two worker cards carry the pool badge", "Array.from(document.querySelectorAll('#fleet .twin')).filter(c => /pool ui/.test(c.innerText) && c.id !== 'twin-local').length === 2", 15000);
    // Prompt it from the Run panel.
    await waitFor("the run panel offers the pooled model", "Array.from(document.getElementById('model').options).some(o => o.value === 'uimodel')", 15000);
    await evaluate("(() => { const $ = id => document.getElementById(id); $('node').value = 'local'; $('node').dispatchEvent(new Event('change')); $('model').value = 'uimodel'; $('model').dispatchEvent(new Event('change')); return true; })()");
    await waitFor("the run panel offers pool ui", "!document.getElementById('poolPick').hidden && Array.from(document.getElementById('pool').options).some(o => o.value === 'ui')", 15000);
    // Remember what the page sends, then run on the chosen pool.
    await evaluate(`(() => { const f = window.fetch; window.__runBodies = []; window.fetch = (u, o) => { if (u === '/api/run') window.__runBodies.push(JSON.parse(o.body)); return f(u, o); }; return true; })()`);
    await evaluate("(() => { const $ = id => document.getElementById(id); $('pool').value = 'ui'; $('context').value = 1024; $('maxTokens').value = 12; $('prompt').value = 'The capital of France is'; $('run').click(); return true; })()");
    await waitFor("the pooled model answers in the page", "document.getElementById('output').textContent.trim().length > 3", 60000);
    check(await evaluate("window.__runBodies.length === 1 && window.__runBodies[0].pool === 'ui'"), "the run went to the chosen pool", await evaluate("window.__runBodies"));
    const rpc = await waitFor("the two pooled workers report RPC traffic", `(async () => {
      const f = await (await fetch('/api/fleet')).json();
      const moved = f.nodes.filter(n => n.link && n.link.rpc_up_total > 0 && n.link.rpc_down_total > 0);
      return moved.length === 2 && moved.map(n => n.link.rpc_up_total);
    })()`, 15000);
    check(Array.isArray(rpc), "RPC byte counters on the pooled workers", rpc);
    await waitFor("run panel idle again", "!document.getElementById('run').disabled", 30000);
    const unknown = await evaluate(`fetch('/api/run', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ node: 'local', model: 'uimodel', pool: 'nope', prompt: 'hi', max_tokens: 4, context: 512 }) }).then(r => r.text())`);
    check(/no pool named nope/.test(unknown), "an unknown pool is refused with its name", unknown);
    // Stop it.
    await evaluate("Array.from(document.querySelectorAll('.poolcard[data-pool=\"ui\"] button')).find(b => b.textContent === 'Stop').click(), true");
    await waitFor("the pool stops", "(() => { const c = document.querySelector('.poolcard[data-pool=\"ui\"]'); return c && /stopped/.test(c.querySelector('.poolcard-head').innerText); })()", 20000);
    await shot("host-pools.png");
  }

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
