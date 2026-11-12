// Applies the saved theme before first paint (loaded synchronously in <head>).
// mode: "auto" (follow the system), "light" or "dark" -> data-theme on <html>.
// accent: one of Pico's 20 color themes -> data-accent on <html>, styled by accents.css.
(function () {
  var ACCENTS = [
    ["amber", "#ffbf00"],
    ["azure", "#0172ad"],
    ["blue", "#2060df"],
    ["cyan", "#047878"],
    ["fuchsia", "#c1208b"],
    ["green", "#398712"],
    ["grey", "#ababab"],
    ["indigo", "#524ed2"],
    ["jade", "#007a50"],
    ["lime", "#a5d601"],
    ["orange", "#d24317"],
    ["pink", "#d92662"],
    ["pumpkin", "#ff9500"],
    ["purple", "#9236a4"],
    ["red", "#c52f21"],
    ["sand", "#ccc6b4"],
    ["slate", "#525f7a"],
    ["violet", "#7540bf"],
    ["yellow", "#f2df0d"],
    ["zinc", "#646b79"]
  ];
  var DEFAULT_ACCENT = "jade", DEFAULT_MODE = "auto";

  function load(key) { try { return localStorage.getItem(key); } catch (e) { return null; } }
  function save(key, v) { try { localStorage.setItem(key, v); } catch (e) {} }
  function known(a) { return ACCENTS.some(function (x) { return x[0] === a; }); }

  var accent = load("u64.accent");
  if (!known(accent)) accent = DEFAULT_ACCENT;
  var mode = load("u64.mode");
  if (mode !== "light" && mode !== "dark") mode = DEFAULT_MODE;

  function applyMode(m) {
    if (m === "light" || m === "dark") document.documentElement.setAttribute("data-theme", m);
    else document.documentElement.removeAttribute("data-theme");
  }
  applyMode(mode);
  document.documentElement.setAttribute("data-accent", accent);

  window.u64Theme = {
    accents: ACCENTS,
    mode: function () { return mode; },
    accent: function () { return accent; },
    setMode: function (m) { mode = m; save("u64.mode", m); applyMode(m); },
    setAccent: function (a) {
      if (!known(a)) return;
      accent = a; save("u64.accent", a);
      document.documentElement.setAttribute("data-accent", a);
    }
  };
})();
