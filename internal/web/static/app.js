// Tiny enhancements; the UI works without them.
(function () {
  // highlight the current page in the nav
  document.querySelectorAll('.topbar nav a[href]').forEach(function (a) {
    var p = a.pathname, here = location.pathname;
    if (p === here || (p !== '/' && here.indexOf(p) === 0)) a.setAttribute('aria-current', 'page');
  });
  // live filter for file listings
  var f = document.getElementById('filter');
  if (f) {
    f.addEventListener('input', function () {
      var q = f.value.toLowerCase();
      document.querySelectorAll('#listing tbody tr').forEach(function (tr) {
        tr.hidden = q !== '' && tr.textContent.toLowerCase().indexOf(q) < 0;
      });
    });
  }

  // theme picker: mode (auto/light/dark) and one of Pico's 20 accent colors
  var T = window.u64Theme, box = document.getElementById('theme-panel');
  if (T && box) {
    var modes = [['auto', 'Auto'], ['light', 'Light'], ['dark', 'Dark']];
    var seg = box.querySelector('[data-modes]'), sw = box.querySelector('[data-swatches]');
    var paint = function () {
      seg.querySelectorAll('button').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.v === T.mode())); });
      sw.querySelectorAll('button').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.v === T.accent())); });
    };
    modes.forEach(function (m) {
      var b = document.createElement('button');
      b.type = 'button'; b.className = 'seg'; b.dataset.v = m[0]; b.textContent = m[1];
      b.onclick = function () { T.setMode(m[0]); paint(); };
      seg.appendChild(b);
    });
    T.accents.forEach(function (a) {
      var b = document.createElement('button');
      var label = a[0].charAt(0).toUpperCase() + a[0].slice(1);
      b.type = 'button'; b.className = 'swatch'; b.dataset.v = a[0];
      b.style.setProperty('--c', a[1]);
      b.title = label; b.setAttribute('aria-label', label);
      b.onclick = function () { T.setAccent(a[0]); paint(); };
      sw.appendChild(b);
    });
    paint();
    // close the panel when clicking elsewhere or pressing Escape
    var d = box.closest('details');
    document.addEventListener('click', function (e) { if (d.open && !d.contains(e.target)) d.open = false; });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') d.open = false; });
  }
})();
