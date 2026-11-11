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
})();
