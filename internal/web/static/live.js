// Liveness: detects an unreachable backend, shows a modal, and reloads the page once the
// backend answers again, or answers as a different instance (a new deployment), so the
// browser never keeps state from an old version. Probes /healthz, which stays open even
// with basic auth.
(function () {
  var meta = document.querySelector('meta[name="u64-instance"]');
  var dlg = document.getElementById('offline-dialog');
  if (!meta || !dlg || !window.fetch || !window.AbortController) return;

  var instance = meta.content;
  var offline = false, reloading = false, fails = 0, attempts = 0, timer = null;
  var status = dlg.querySelector('[data-status]');

  // the dialog is not dismissable: there is nothing to do but wait
  dlg.addEventListener('cancel', function (e) { e.preventDefault(); });

  function showOffline() {
    offline = true;
    if (!dlg.open) { try { dlg.showModal(); } catch (e) { dlg.setAttribute('open', ''); } }
    status.textContent = 'Waiting for the server… (attempt ' + attempts + ')';
  }

  function reload() {
    if (reloading) return;
    reloading = true;
    clearTimeout(timer);
    status.textContent = 'Server is back, reloading…';
    location.reload();
  }

  function schedule(ms) {
    clearTimeout(timer);
    if (reloading) return;
    timer = setTimeout(check, ms !== undefined ? ms : offline ? Math.min(1500 + attempts * 250, 5000) : document.hidden ? 30000 : 3000);
  }

  function check() {
    if (reloading) return;
    var ctl = new AbortController();
    var abort = setTimeout(function () { ctl.abort(); }, 4000);
    fetch('/healthz', { cache: 'no-store', signal: ctl.signal })
      .then(function (r) {
        clearTimeout(abort);
        var id = r.headers.get('X-Instance'); // a proxy's error page has none
        if (!r.ok || !id) throw new Error('backend not answering');
        if (offline || id !== instance) { reload(); return; }
        fails = 0; attempts = 0;
        schedule();
      })
      .catch(function () {
        clearTimeout(abort);
        fails++; attempts++;
        if (fails >= 2 || offline) showOffline();
        schedule();
      });
  }

  // failed UI requests are an early hint: confirm quickly instead of waiting for the next probe
  function hint() { fails = Math.max(fails, 1); schedule(0); }
  document.addEventListener('htmx:sendError', hint);
  document.addEventListener('htmx:timeout', hint);
  document.addEventListener('htmx:responseError', function (e) {
    var s = e.detail && e.detail.xhr && e.detail.xhr.status;
    if (s === 502 || s === 503 || s === 504) hint();
  });
  // no point in firing UI requests at a server we know is gone
  document.addEventListener('htmx:beforeRequest', function (e) { if (offline) e.preventDefault(); });

  window.addEventListener('offline', function () { fails = 2; attempts++; showOffline(); });
  window.addEventListener('online', function () { schedule(0); });
  document.addEventListener('visibilitychange', function () { if (!document.hidden) schedule(0); });

  schedule();
})();
