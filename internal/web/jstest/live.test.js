// Drives static/live.js with a fake DOM, fake timers and a scripted fetch. Run: node live.test.js (make test-js)
const fs = require('fs');
const code = fs.readFileSync(require('path').join(__dirname, '..', 'static', 'live.js'), 'utf8');

function scenario(name, instance, responses, steps) {
  let now = 0, timers = [], reloads = 0, dialogOpen = false, statusText = '';
  const listeners = {};
  const status = { set textContent(v) { statusText = v; }, get textContent() { return statusText; } };
  const dlg = {
    get open() { return dialogOpen; },
    showModal() { dialogOpen = true; }, setAttribute() { dialogOpen = true; },
    addEventListener() {}, querySelector: () => status,
  };
  let i = 0, fetches = 0;
  global.window = { addEventListener(e, f) { listeners['w:' + e] = f; }, fetch: true, AbortController: true };
  global.AbortController = class { constructor() { this.signal = {}; } abort() {} };
  global.fetch = () => {
    fetches++;
    const r = responses[Math.min(i++, responses.length - 1)];
    return r === 'net' ? Promise.reject(new Error('net')) : Promise.resolve({ ok: r.ok, headers: { get: () => r.id || null } });
  };
  global.document = {
    hidden: false,
    querySelector: () => ({ content: instance }),
    getElementById: () => dlg,
    addEventListener(e, f) { listeners['d:' + e] = f; },
  };
  global.location = { reload() { reloads++; } };
  global.setTimeout = (f, ms) => { timers.push({ f, at: now + ms, id: timers.length + 1 }); return timers.length; };
  global.clearTimeout = id => { timers = timers.filter(t => t.id !== id); };
  eval(code);
  const tick = async (ms) => { // advance fake time, running due timers and flushing promises
    const end = now + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      const t = timers[0];
      if (!t || t.at > end) break;
      timers.shift(); now = Math.max(now, t.at); t.f();
      for (let k = 0; k < 5; k++) await Promise.resolve();
    }
    now = end;
  };
  return steps({ tick, state: () => ({ reloads, open: dialogOpen, fetches, text: statusText }), listeners }).then(() => console.log('ok  ', name));
}
const eq = (a, b, m) => { if (a !== b) { console.log('FAIL', m, 'got', a, 'want', b); process.exitCode = 1; } };
process.on('unhandledRejection', e => { console.log('FAIL', e); process.exitCode = 1; });

(async () => {
  const up = { ok: true, id: 'A' };
  await scenario('healthy backend: no modal, no reload', 'A', [up], async ({ tick, state }) => {
    await tick(30000); eq(state().open, false, 'modal'); eq(state().reloads, 0, 'reloads'); eq(state().fetches >= 9, true, 'polls');
  });
  await scenario('one failed probe is tolerated', 'A', [up, 'net', up], async ({ tick, state }) => {
    await tick(10000); eq(state().open, false, 'modal after single failure'); eq(state().reloads, 0, 'reloads');
  });
  await scenario('offline: modal, then reload when back', 'A', [up, 'net', 'net', 'net', up], async ({ tick, state }) => {
    await tick(4000); eq(state().open, false, 'not yet'); await tick(12000);
    eq(state().open, true, 'modal shown'); eq(state().reloads, 1, 'reloaded when back'); eq(state().text, 'Server is back, reloading…', 'text');
  });
  await scenario('proxy error page (no X-Instance) counts as offline', 'A', [{ ok: false }, { ok: true /* no id */ }, { ok: true /* no id */ }, up], async ({ tick, state }) => {
    await tick(9000); eq(state().open, true, 'modal'); await tick(10000); eq(state().reloads, 1, 'reloaded');
  });
  await scenario('new deployment (instance changed) reloads without ever showing the modal', 'A', [up, { ok: true, id: 'B' }], async ({ tick, state }) => {
    await tick(10000); eq(state().reloads, 1, 'reloads'); eq(state().open, false, 'modal');
  });
  await scenario('browser offline event shows modal at once', 'A', [up], async ({ tick, state, listeners }) => {
    await tick(1000); listeners['w:offline'](); eq(state().open, true, 'modal');
  });
  await scenario('htmx send error triggers a quick probe', 'A', ['net', 'net', 'net'], async ({ tick, state, listeners }) => {
    await tick(1000); listeners['d:htmx:sendError'](); await tick(100); await tick(100);
    eq(state().open, true, 'modal after hint + failed probes'); 
  });
  await scenario('htmx requests are blocked while offline', 'A', [up, 'net', 'net', 'net'], async ({ tick, state, listeners }) => {
    await tick(12000); let prevented = false;
    listeners['d:htmx:beforeRequest']({ preventDefault() { prevented = true; } }); eq(prevented, true, 'request blocked');
  });
})();
