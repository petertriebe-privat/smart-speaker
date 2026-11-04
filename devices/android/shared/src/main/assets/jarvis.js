// Jarvis screen renderer: render(card) draws one card from Jarvis (MQTT jarvis/screen/<room>/show)
// in the overlay (small, above any app) or the full screen. A card is a head (kind, title,
// subtitle) and blocks; the first format ({title, text, items, image}) still works. Text from a
// card only ever goes in as textContent. See devices/android/docs/design.md.
"use strict";

var FULL = location.search.indexOf("mode=fullscreen") >= 0;
// TALK: the phone app (devices/android/phone): Jarvis' button at the bottom of the HUD.
var TALK = location.search.indexOf("talk=1") >= 0;
// CAR: pictures for Android Auto (the phone app's car/CardRenderer.java): the card or the hologram
// alone, without the HUD's bars and avatar; the app cuts the picture to carRect().
var CAR = location.search.indexOf("car=1") >= 0;
// A full-screen card stays until back, the next card or a clear (no running time shown; the app
// closes a forgotten one after 30 min, FullscreenActivity); the corner's card 20 s.
var DEFAULT_SECONDS = FULL ? 0 : 20;

// --- Icons: 24 px line drawings in currentColor (static strings, never card text) -------------
var I = {
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v6M12 7.5v.5"/>',
  list: '<path d="M10 6h10M10 12h10M10 18h10M3.5 6l1.2 1.2L7 5M3.5 12l1.2 1.2L7 11M3.5 18l1.2 1.2L7 17"/>',
  recipe: '<path d="M4 10h16v4.5A5.5 5.5 0 0 1 14.5 20h-5A5.5 5.5 0 0 1 4 14.5zM2 10h20M9 7c0-1.3 1.2-1.3 1.2-2.6M14 7c0-1.3 1.2-1.3 1.2-2.6"/>',
  timer: '<circle cx="12" cy="13" r="8"/><path d="M12 13V9M10 2h4M18.5 6.5L20 5"/>',
  weather: '<circle cx="9" cy="8.5" r="3.2"/><path d="M9 2.5v1.3M3 8.5h1.3M4.8 4.3l.9.9M13.2 4.3l-.9.9"/><path d="M8 19h9a3.4 3.4 0 0 0 .3-6.8A5 5 0 0 0 8 13.6 2.7 2.7 0 0 0 8 19z"/>',
  calendar: '<rect x="3.5" y="5" width="17" height="15" rx="1"/><path d="M3.5 10h17M8 3v4M16 3v4M7.5 14h3M13.5 14h3M7.5 17h3"/>',
  message: '<path d="M4 5h16v11H9l-5 4z"/><path d="M8 9.5h8M8 12.5h5"/>',
  alert: '<path d="M12 3l10 18H2z"/><path d="M12 10v5M12 18v.5"/>',
  camera: '<rect x="3" y="7" width="18" height="13" rx="1"/><circle cx="12" cy="13.5" r="3.5"/><path d="M8 7l1.5-3h5L16 7"/>',
  music: '<path d="M9 18V5l11-2v13"/><circle cx="6.5" cy="18" r="2.5"/><circle cx="17.5" cy="16" r="2.5"/>',
  home: '<path d="M3 11l9-7 9 7M5 9.5V20h14V9.5M10 20v-6h4v6"/>',
  energy: '<path d="M13 2L5 14h6l-1 8 8-12h-6z"/>',
  route: '<path d="M12 21s7-6.5 7-12a7 7 0 0 0-14 0c0 5.5 7 12 7 12z"/><circle cx="12" cy="9" r="2.5"/>',
  package: '<path d="M3 7.5L12 3l9 4.5v9L12 21l-9-4.5zM3 7.5l9 4.5 9-4.5M12 12v9M7.5 5.2l9 4.6"/>',
  call: '<path d="M5 4h4l2 5-2.5 1.5a11 11 0 0 0 5 5L15 13l5 2v4a2 2 0 0 1-2 2A16 16 0 0 1 3 6a2 2 0 0 1 2-2z"/>',
  search: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="M15.5 15.5L21 21"/>',
  choice: '<path d="M4 6h3M4 12h3M4 18h3M10 6h10M10 12h10M10 18h10"/>',
  web: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.6 2.6 3.8 5.6 3.8 9s-1.2 6.4-3.8 9c-2.6-2.6-3.8-5.6-3.8-9S9.4 5.6 12 3z"/>',
  video: '<rect x="3" y="5" width="18" height="14" rx="1"/><path d="M10 9l5 3-5 3z"/>'
};
var CLOUD = "M7 18h10a4 4 0 0 0 .5-8A6 6 0 0 0 6 11.5 3.3 3.3 0 0 0 7 18z";
var CLOUD_UP = "M7 14h10a4 4 0 0 0 .5-8A6 6 0 0 0 6 7.5 3.3 3.3 0 0 0 7 14z";
// Weather by Home Assistant's conditions.
var WX = {
  "sunny": '<circle cx="12" cy="12" r="4.5"/><path d="M12 2v2.5M12 19.5V22M2 12h2.5M19.5 12H22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8"/>',
  "clear-night": '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
  "partlycloudy": I.weather,
  "cloudy": '<path d="' + CLOUD + '"/>',
  "rainy": '<path d="' + CLOUD_UP + '"/><path d="M8.5 17l-1 3M12.5 17l-1 3M16.5 17l-1 3"/>',
  "pouring": '<path d="' + CLOUD_UP + '"/><path d="M7.5 16.5l-1.5 4.5M11 16.5l-1.5 4.5M14.5 16.5L13 21M18 16.5L16.5 21"/>',
  "snowy": '<path d="' + CLOUD_UP + '"/><path d="M8 18h.01M12 19h.01M16 18h.01M10 21.5h.01M14 21.5h.01" stroke-width="2.6" stroke-linecap="round"/>',
  "lightning": '<path d="' + CLOUD_UP + '"/><path d="M12.5 14l-2.5 4h4l-2.5 4"/>',
  "fog": '<path d="M4 9h16M3 13h18M5 17h14"/>',
  "windy": '<path d="M3 9h11a3 3 0 1 0-3-3M3 14h15a3 3 0 1 1-3 3"/>'
};
WX["lightning-rainy"] = WX.lightning; WX["snowy-rainy"] = WX.snowy; WX["windy-variant"] = WX.windy;
WX["exceptional"] = I.alert;

function icon(paths) {
  return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round">' + paths + "</svg>";
}

// Kinds: label in the eyebrow and the icon.
var KINDS = {
  info: ["Info", "info"], list: ["Liste", "list"], recipe: ["Rezept", "recipe"], timer: ["Timer", "timer"],
  weather: ["Wetter", "weather"], calendar: ["Kalender", "calendar"], message: ["Nachricht", "message"],
  alert: ["Warnung", "alert"], camera: ["Kamera", "camera"], music: ["Musik", "music"], home: ["Zuhause", "home"],
  energy: ["Energie", "energy"], route: ["Route", "route"], package: ["Sendung", "package"], call: ["Anruf", "call"],
  search: ["Recherche", "search"], choice: ["Auswahl", "choice"], web: ["Web", "web"], video: ["Video", "video"]
};

// --- The orb: segmented rings around a glowing core (Jarvis himself) -------------------------
function orbSVG() {
  var ticks = "";
  for (var i = 0; i < 72; i++) {
    var a = i * 5 * Math.PI / 180, long = i % 6 === 0, r1 = long ? 90 : 94;
    ticks += '<line x1="' + (100 + r1 * Math.cos(a)).toFixed(1) + '" y1="' + (100 + r1 * Math.sin(a)).toFixed(1) +
      '" x2="' + (100 + 98 * Math.cos(a)).toFixed(1) + '" y2="' + (100 + 98 * Math.sin(a)).toFixed(1) + '"/>';
  }
  return '<svg viewBox="0 0 200 200" fill="none" stroke="currentColor">' +
    '<defs><radialGradient id="coreFill"><stop offset="0" stop-color="#fff4dc"/><stop offset="0.35" stop-color="#ffc46b"/>' +
    '<stop offset="0.75" stop-color="#ff9a1f" stop-opacity="0.55"/><stop offset="1" stop-color="#ff9a1f" stop-opacity="0"/></radialGradient></defs>' +
    '<g class="r-ticks" stroke-width="1.4" opacity="0.7">' + ticks + "</g>" +
    '<circle class="r-seg" cx="100" cy="100" r="82" stroke-width="5" stroke-dasharray="46 10 12 10 80 18 30 10 22 26"/>' +
    '<circle class="r-dash" cx="100" cy="100" r="68" stroke-width="7" stroke-dasharray="2 5" opacity="0.6"/>' +
    '<circle cx="100" cy="100" r="58" stroke-width="1" opacity="0.5"/>' +
    '<circle class="wave" cx="100" cy="100" r="48" stroke-width="2"/><circle class="wave w2" cx="100" cy="100" r="48" stroke-width="2"/>' +
    '<g class="core"><circle cx="100" cy="100" r="46" fill="url(#coreFill)" stroke="none"/>' +
    '<circle cx="100" cy="100" r="26" stroke="#ffe2b0" stroke-width="2"/></g></svg>';
}

function orb(state) {
  var el = h("div", "orb " + (state || "idle"));
  el.innerHTML = orbSVG();
  return el;
}

// holo: Jarvis as the 3D hologram (jarvis3d.js) with a glow behind it; the 2D orb without WebGL.
function holo(state) {
  var el = h("div", "holo " + (state || "idle"));
  add(el, h("div", "glow"));
  var canvas = add(el, h("canvas"));
  // started once the element is on the page (the canvas needs its size)
  setTimeout(function () {
    if (!canvas.isConnected) return;
    el.sphere = window.Sphere3D && Sphere3D.start(canvas, state);
    if (!el.sphere) {
      el.textContent = "";
      el.appendChild(orb(state));
    }
  }, 0);
  return el;
}

// setLevel: the loudness of the voice Jarvis hears or speaks (0..1), from the app ~20 times a
// second: the hologram and its glow pulse with it as the Echo's ring does.
function setLevel(v) {
  if (window.Sphere3D) Sphere3D.setLevel(v);
  document.documentElement.style.setProperty("--lv", v.toFixed(2));
}

// --- DOM helpers --------------------------------------------------------------------------
function h(tag, cls, text) {
  var el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text !== undefined && text !== null && text !== "") el.textContent = String(text);
  return el;
}
function add(parent, child) { if (child) parent.appendChild(child); return child; }
function str(item) { return typeof item === "string" || typeof item === "number" ? { text: String(item) } : (item || {}); }
function url(value) { return /^https?:\/\//.test(value || "") ? value : ""; }
function two(n) { return (n < 10 ? "0" : "") + n; }

// --- Blocks ------------------------------------------------------------------------------
var BLOCKS = {
  text: function (b) { return h("div", "text", b.text); },

  list: function (b) {
    var ul = h("ul", "list");
    var items = b.items || [];
    if (items.length > 8) ul.classList.add("columns");
    items.forEach(function (raw) {
      var it = str(raw), li = add(ul, h("li", it.done ? "done" : ""));
      add(li, h("span", "mark"));
      add(li, h("span", "", it.text));
      if (it.note) add(li, h("span", "note", it.note));
    });
    return ul;
  },

  steps: function (b) {
    var ol = h("ol", "steps"), now = typeof b.current === "number" ? b.current : -1;
    (b.items || []).forEach(function (raw, i) {
      add(ol, h("li", i === now ? "now" : (now >= 0 && i < now ? "past" : ""), str(raw).text));
    });
    return ol;
  },

  stats: function (b) {
    var box = h("div", "stats");
    (b.items || []).forEach(function (it) {
      var s = add(box, h("div", "stat"));
      add(s, h("div", "label", it.label));
      var v = add(s, h("div", "value", it.value));
      if (it.unit) add(v, h("span", "unit", it.unit));
      if (it.trend) add(v, h("span", "trend", it.trend));
    });
    return box;
  },

  kv: function (b) {
    var dl = h("dl", "kv");
    (b.items || []).forEach(function (it) { add(dl, h("dt", "", it.label)); add(dl, h("dd", "", it.value)); });
    return dl;
  },

  table: function (b) {
    var t = h("table"), cols = b.columns || [], rows = b.rows || [];
    var num = cols.map(function (_, c) {
      return rows.length > 0 && rows.every(function (r) { return /^[-+]?[\d.,]+\s*(%|€|°|°C|kWh|W|km|m|min|h|dB|GB|ms)?$/.test(String(r[c] || "").trim()); });
    });
    var tr = add(add(t, h("thead")), h("tr"));
    cols.forEach(function (c, i) { add(tr, h("th", num[i] ? "num" : "", c)); });
    var body = add(t, h("tbody"));
    rows.forEach(function (r, i) {
      var row = add(body, h("tr", i === b.highlight ? "hl" : ""));
      r.forEach(function (cell, c) { add(row, h("td", num[c] ? "num" : "", cell)); });
    });
    return t;
  },

  chart: function (b) { return chart(b); },

  image: function (b) {
    var src = url(b.url);
    if (!src) return null;
    var box = h("div", "image");
    if (b.live) add(box, h("div", "live", "Live"));
    var img = add(box, h("img"));
    img.onload = report;
    img.onerror = function () { console.warn("image failed: " + src); box.classList.add("hidden"); report(); };
    img.src = src;
    if (b.caption) add(box, h("div", "caption", b.caption));
    return box;
  },

  // Several pictures as a grid ("zeig mir Bilder davon"): items [{url, caption}] or links.
  gallery: function (b) {
    var grid = h("div", "gallery");
    (b.items || []).slice(0, 9).forEach(function (raw) {
      var it = typeof raw === "string" ? { url: raw } : raw || {}, src = url(it.url);
      if (!src) return;
      var cell = add(grid, h("figure"));
      var img = add(cell, h("img"));
      img.onload = report;
      img.onerror = function () { console.warn("image failed: " + src); cell.classList.add("hidden"); report(); };
      img.src = src;
      if (it.caption) add(cell, h("figcaption", "", it.caption));
    });
    grid.classList.add("n" + Math.min(3, grid.childElementCount || 1));
    return grid.childElementCount ? grid : null;
  },

  timer: function (b) {
    var box = h("div", "timer");
    var total = b.total || b.seconds || 60;
    var end = b.ends_at ? Date.parse(b.ends_at) : Date.now() + 1000 * (b.seconds || 0);
    var ring = add(box, h("div", "ring"));
    var ticks = "";
    for (var i = 0; i < 60; i++) {
      var a = i * 6 * Math.PI / 180;
      ticks += '<line x1="' + (60 + 57 * Math.cos(a)).toFixed(1) + '" y1="' + (60 + 57 * Math.sin(a)).toFixed(1) +
        '" x2="' + (60 + 59.5 * Math.cos(a)).toFixed(1) + '" y2="' + (60 + 59.5 * Math.sin(a)).toFixed(1) + '"/>';
    }
    ring.innerHTML = '<svg viewBox="0 0 120 120" fill="none"><g class="ticks" stroke-width="1">' + ticks + "</g>" +
      '<circle class="track" cx="60" cy="60" r="50" stroke-width="7"/>' +
      '<circle class="left" cx="60" cy="60" r="50" stroke-width="7" stroke-dasharray="314.16" stroke-dashoffset="0"/></svg>';
    add(ring, h("div", "time"));
    var info = add(box, h("div"));
    add(info, h("div", "what", b.label || "Timer"));
    add(info, h("div", "when", "Endet " + new Date(end).toLocaleTimeString("de-DE", { hour: "2-digit", minute: "2-digit" }) + " Uhr"));
    box.dataset.end = end;
    box.dataset.total = total;
    tickTimer(box);
    return box;
  },

  progress: function (b) {
    var box = h("div", "progress"), row = add(box, h("div", "row"));
    add(row, h("span", "", b.label));
    add(row, h("span", "", b.text || Math.round(100 * (b.value || 0)) + " %"));
    var fill = add(add(box, h("div", "bar")), h("div", "fill"));
    fill.style.width = Math.max(0, Math.min(100, 100 * (b.value || 0))) + "%";
    return box;
  },

  // Temperatures as numbers: the backend may send "12 °C"; the card adds the degree itself.
  forecast: function (b) {
    function deg(v) { return String(v === undefined || v === null ? "" : v).replace(/\s*°\s*C?\s*$/, ""); }
    var box = h("div", "forecast");
    if (b.now) {
      var now = add(box, h("div", "now"));
      var wx = add(now, h("div", "wx"));
      wx.innerHTML = icon(WX[b.now.condition] || WX.cloudy);
      var t = add(now, h("div"));
      var temp = add(t, h("div", "temp", deg(b.now.temp)));
      add(temp, h("small", "", "°"));
      var desc = [b.now.text, b.now.high !== undefined ? "↑ " + deg(b.now.high) + "°  ↓ " + deg(b.now.low) + "°" : ""].filter(Boolean).join("  ·  ");
      add(t, h("div", "desc", desc));
    }
    if (b.items && b.items.length) {
      var hours = add(box, h("div", "hours"));
      b.items.slice(0, 8).forEach(function (it) {
        var c = add(hours, h("div", "hour"));
        add(c, h("div", "t", it.time));
        add(c, h("div", "wx")).innerHTML = icon(WX[it.condition] || WX.cloudy);
        add(c, h("div", "v", deg(it.temp) + "°"));
        if (it.low !== undefined && it.low !== "") add(c, h("div", "lo", deg(it.low) + "°")); // a day: high and low
        add(c, h("div", "r", it.rain ? it.rain : ""));
      });
    }
    return box;
  },

  timeline: function (b) {
    var ul = h("ul", "timeline");
    (b.items || []).forEach(function (it) {
      var li = add(ul, h("li", it.state || ""));
      add(li, h("div", "time", it.time));
      add(li, h("div", "what", it.title));
      if (it.note) add(li, h("div", "note", it.note));
    });
    return ul;
  },

  choices: function (b) {
    var ol = h("ol", "choices");
    (b.items || []).forEach(function (raw, i) {
      var it = str(raw), li = add(ol, h("li"));
      add(li, h("span", "n", i + 1));
      add(li, h("span", "", it.text));
      if (it.note) add(li, h("span", "note", it.note));
    });
    return ol;
  },

  // A video file or stream (mp4, webm, HLS) in the card, with sound; YouTube goes to its app.
  video: function (b) {
    var src = url(b.url);
    if (!src) return null;
    var box = h("div", "image video");
    // Full screen: the app plays it in a native player laid over this slot (Amazon's WebView draws
    // video behind the page, where the HUD covers it).
    if (FULL && window.Card && Card.video) {
      var slot = add(box, h("div", "slot"));
      if (b.caption) add(box, h("div", "caption", b.caption));
      setTimeout(function () { placeVideo(slot, src, !!b.loop); }, 700); // after the entry animation
      return box;
    }
    var v = add(box, h("video"));
    v.autoplay = true;
    v.controls = false;
    v.loop = !!b.loop;
    v.playsInline = true;
    v.onloadedmetadata = report;
    v.onerror = function () { console.warn("video failed: " + src); box.classList.add("hidden"); report(); };
    v.src = src;
    if (b.caption) add(box, h("div", "caption", b.caption));
    return box;
  },

  // A web page in a frame of the HUD (full screen only): the app lays a browser over the slot.
  web: function (b) {
    var src = url(b.url);
    if (!src || !FULL || !window.Card || !Card.web) return null;
    var slot = h("div", "webslot");
    setTimeout(function () {
      if (!slot.isConnected) return;
      var r = slot.getBoundingClientRect(), d = devicePixels();
      Card.web(src, Math.round(r.left * d), Math.round(r.top * d), Math.round(r.width * d), Math.round(r.height * d));
    }, 700);
    return slot;
  },

  media: function (b) {
    var box = h("div", "media");
    var src = url(b.cover);
    if (src) { var img = add(box, h("img", "cover")); img.onload = report; img.src = src; }
    var t = add(box, h("div"));
    add(t, h("div", "t", b.title));
    add(t, h("div", "a", b.artist));
    if (b.playing !== false) {
      var eq = add(t, h("div", "eq"));
      for (var i = 0; i < 12; i++) { var bar = add(eq, h("i")); bar.style.animationDelay = (-i * 0.17 % 0.9) + "s"; }
    }
    return box;
  }
};

// devicePixels: the app's pixels per CSS pixel. Not devicePixelRatio: on the 4K TVs (1920 x 1080
// for apps) the WebView reported twice the app's ratio and the page landed at twice its place, down
// in the corner (29.10.2026).
var pixelsPerCss = 0; // cached: the hologram asks every frame, Card.width is a call into the app
addEventListener("resize", function () { pixelsPerCss = 0; });
function devicePixels() {
  if (!pixelsPerCss) {
    var w = window.Card && Card.width ? Card.width() : 0;
    pixelsPerCss = w > 0 ? w / (document.documentElement.clientWidth || innerWidth) : window.devicePixelRatio || 1;
  }
  return pixelsPerCss;
}

// placeVideo hands the slot's place on the screen (device pixels) to the native player.
function placeVideo(slot, src, loop) {
  if (!slot.isConnected) return;
  var r = slot.getBoundingClientRect(), d = devicePixels();
  Card.video(src, Math.round(r.left * d), Math.round(r.top * d), Math.round(r.width * d), Math.round(r.height * d), loop);
}

// --- Charts: line (one or two series) or bars, in SVG ----------------------------------------
function chart(b) {
  var series = (b.series || []).slice(0, 2), labels = b.labels || [];
  if (!series.length) return null;
  var W = 640, H = 240, L = 44, B = 26, T = 10, R = 8;
  var all = [].concat.apply([], series.map(function (s) { return s.values || []; }));
  var lo = Math.min.apply(null, all.concat(b.kind === "bar" ? [0] : [])), hi = Math.max.apply(null, all);
  if (hi === lo) hi = lo + 1;
  var pad = (hi - lo) * 0.08;
  if (b.kind !== "bar") lo = lo >= 0 && lo - pad < 0 ? 0 : lo - pad; // no axis below 0 for counts
  hi += pad;
  var n = Math.max.apply(null, series.map(function (s) { return (s.values || []).length; }));
  function x(i) { return L + (b.kind === "bar" ? (i + 0.5) * (W - L - R) / n : i * (W - L - R) / Math.max(1, n - 1)); }
  function y(v) { return T + (H - T - B) * (1 - (v - lo) / (hi - lo)); }
  var svg = '<svg viewBox="0 0 ' + W + " " + H + '"><defs>' +
    '<linearGradient id="areaFill" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ff9a1f" stop-opacity="0.35"/><stop offset="1" stop-color="#ff9a1f" stop-opacity="0"/></linearGradient>' +
    '<linearGradient id="barFill" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffc46b"/><stop offset="1" stop-color="#e8590c" stop-opacity="0.6"/></linearGradient></defs>';
  for (var g = 0; g <= 4; g++) {
    var v = lo + (hi - lo) * g / 4, yy = y(v).toFixed(1);
    svg += '<line class="grid" x1="' + L + '" x2="' + (W - R) + '" y1="' + yy + '" y2="' + yy + '"/>';
    svg += '<text class="axis" x="' + (L - 8) + '" y="' + (+yy + 4) + '" text-anchor="end">' + fmt(v) + "</text>";
  }
  var every = Math.max(1, Math.ceil(labels.length / 8));
  labels.forEach(function (lab, i) {
    if (i % every === 0) svg += '<text class="axis" x="' + x(i).toFixed(1) + '" y="' + (H - 6) + '" text-anchor="middle">' + esc(lab) + "</text>";
  });
  if (b.kind === "bar") {
    var vals = series[0].values || [], bw = (W - L - R) / n * 0.62, peak = vals.indexOf(Math.max.apply(null, vals));
    vals.forEach(function (v, i) {
      svg += '<rect class="bar' + (i === peak ? " peak" : "") + '" x="' + (x(i) - bw / 2).toFixed(1) + '" y="' + y(v).toFixed(1) +
        '" width="' + bw.toFixed(1) + '" height="' + (y(lo) - y(v)).toFixed(1) + '"/>';
    });
  } else {
    series.forEach(function (s, k) {
      var pts = (s.values || []).map(function (v, i) { return x(i).toFixed(1) + "," + y(v).toFixed(1); });
      if (k === 0) svg += '<polygon fill="url(#areaFill)" points="' + x(0) + "," + y(lo) + " " + pts.join(" ") + " " + x(pts.length - 1) + "," + y(lo) + '"/>';
      svg += '<polyline class="line s' + k + '" points="' + pts.join(" ") + '"/>';
      var last = pts[pts.length - 1].split(",");
      svg += '<circle cx="' + last[0] + '" cy="' + last[1] + '" r="4.5" fill="#ffc46b"/>';
    });
  }
  svg += "</svg>";
  var box = h("div", "chart");
  box.innerHTML = svg;
  if (series.length > 1 || b.unit) {
    var lg = add(box, h("div", "legend"));
    series.forEach(function (s, k) { var it = add(lg, h("span", "", s.name)); it.insertBefore(h("i", "s" + k), it.firstChild); });
    if (b.unit) add(lg, h("span", "", "Einheit: " + b.unit));
  }
  return box;
}
function fmt(v) { return Math.abs(v) >= 100 ? Math.round(v) : Math.round(v * 10) / 10; }
function esc(s) { var d = document.createElement("div"); d.textContent = String(s); return d.innerHTML; }

// --- Timers tick every second ---------------------------------------------------------------
function tickTimer(box) {
  var left = Math.max(0, Math.round((+box.dataset.end - Date.now()) / 1000)), total = +box.dataset.total;
  var m = Math.floor(left / 60), s = left % 60, hrs = Math.floor(m / 60);
  box.querySelector(".time").textContent = hrs ? hrs + ":" + two(m % 60) + ":" + two(s) : two(m) + ":" + two(s);
  box.querySelector(".left").setAttribute("stroke-dashoffset", (314.16 * (1 - left / Math.max(1, total))).toFixed(1));
  box.classList.toggle("done", left === 0);
}
setInterval(function () { document.querySelectorAll(".timer").forEach(tickTimer); }, 1000);

// --- The page: overlay = the card; full screen = the HUD around it ----------------------------
var root, main, clock, date, label, leftBar, avatar, shownAt = 0, shownFor = 0, consoleMode = false;

function buildPage() {
  document.body.className = FULL ? "fullscreen" : "overlay";
  if (!FULL) { root = main = document.body; return; }
  var hud = add(document.body, h("div", "hud"));
  // Jarvis' avatar on the left: the cards are cast from it (hidden where a card has its own)
  avatar = add(hud, holo("idle"));
  avatar.classList.add("avatar");
  var top = add(hud, h("div", "bar"));
  var brand = add(top, h("div", "brand"));
  add(brand, orb("idle"));
  add(brand, h("span", "", "Jarvis"));
  add(top, h("div", "line"));
  label = add(top, h("div", "", ""));
  add(top, h("div", "line"));
  date = add(top, h("div", "date"));
  clock = add(top, h("div", "clock"));
  main = add(hud, h("main"));
  if (TALK) talkButton(hud);
  var bottom = add(hud, h("div", "bar bottom"));
  add(bottom, h("span", "", TALK ? "Tippen · Halten" : "Zurück · Schließen"));
  leftBar = add(add(bottom, h("div", "left")), h("i"));
  add(bottom, h("span", "", "Sys · Online"));
  root = hud;
  if (CAR) document.body.classList.add("car");
  if (TALK) {
    document.body.classList.add("talk");
    orbPlay();
  }
  // The HUD is laid out on a 1920 x 1080 stage and scaled to the screen: the stick's WebView has
  // 960 x 540 CSS pixels (density 2), another TV other ones. A phone upright gets a stage 1080
  // wide and as tall as its screen (jarvis.css body.portrait).
  fit();
  addEventListener("resize", fit);
  tickClock();
  setInterval(tickClock, 1000);
}

// fit scales the 1920 x 1080 HUD to the window; again on every card (the WebView may not know its
// size yet when the page loads).
var fitted = "", carBox = null;
function fit() {
  var w = document.documentElement.clientWidth || innerWidth, hgt = document.documentElement.clientHeight || innerHeight;
  var x = 0, y = 0;
  if (carBox) { x = carBox[0]; y = carBox[1]; w = carBox[2]; hgt = carBox[3]; }
  if (!FULL || !root || !w || !hgt) return;
  // upright, or in the car's free part beside its panel (often near square) the phone's layout
  var portrait = hgt > w || (CAR && w < hgt * 1.5);
  document.body.classList.toggle("portrait", portrait);
  root.style.height = portrait ? Math.round(1080 * hgt / w) + "px" : "";
  var s = portrait ? w / 1080 : Math.min(w / 1920, hgt / 1080);
  if (carBox && !portrait) { x += (w - 1920 * s) / 2; y += (hgt - 1080 * s) / 2; } // centred in the car's free part
  var scale = (x || y ? "translate(" + x.toFixed(1) + "px, " + y.toFixed(1) + "px) " : "") + "scale(" + s.toFixed(4) + ")";
  if (scale !== fitted) console.log("hud " + w + "x" + hgt + " " + scale);
  fitted = scale;
  root.style.transform = scale;
}

// carArea: the part of the car's screen beside Android Auto's panel, in CSS pixels (the phone app's
// car/CarSurface.java); the HUD fits itself into it.
function carArea(x, y, w, h) {
  carBox = [x, y, w, h];
  fit();
}

// carRect: where the car's picture is on the screen, in CSS pixels: the card, else the hologram.
function carRect() {
  var el = document.querySelector("main .frame:not(.stage)") || document.querySelector("main .status .holo");
  if (!el) return null;
  var r = el.getBoundingClientRect();
  return [r.left, r.top, r.width, r.height];
}

// --- Playing with the orb (the phone) --------------------------------------------------------
// Fingers near the hologram: as in a plasma ball, one filament of violet plasma reaches from its
// core to each finger, a soft line that wavers and follows it, glowing where it touches the glass
// (the owner's wish: the plasma ball's violet, not lightning). While fingers stay on the glass the
// sphere charges, faster with more fingers: it fills with plasma (filaments wander from the core
// to its shell, more and more), glows, grows and spins up, and the phone's crackle thickens. Full,
// it explodes: a flash, shockwaves, a shower of sparks, a rumble; the sphere swells, collapses and
// builds itself anew. Let go, it discharges slowly. Dragged across the sphere it turns and keeps
// spinning (tilted, it springs back). A double tap sends a shockwave and one strong pulse; five
// quick taps spin it wild through every colour. Cards and Jarvis' button keep their own touches.
var PLASMA_GLOW = "150,70,255", PLASMA_BODY = "225,80,255", PLASMA_CORE = "255,170,240";
var CHARGE_PER_FINGER = 0.11; // per second: one finger fills it in ~9 s, three in ~3 s
var DISCHARGE = 0.07;         // per second, let go
function orbPlay() {
  var canvas = null, ctx = null, dpr = window.devicePixelRatio || 1;
  var fingers = {}, count = 0, dragId = null, buzzAt = 0, flick = 1;
  var last = null, downAt = 0, moved = 0, taps = [], waves = [], raf = 0, prev = 0;
  var charge = 0, coolUntil = 0, sparks = [], flashAt = 0, fades = [];
  var SEEDS = [];
  for (var s = 0; s < 10; s++) SEEDS.push({ a: Math.random() * 6.283, v: (Math.random() - 0.5) * 1.6, k: 0.75 + Math.random() * 0.2 });

  // the hologram to play with: the console's large one, else the avatar while it shows
  function holoNow() {
    var big = document.querySelector("#card.is-status .holo");
    if (big) return big;
    return avatar && !root.classList.contains("solo") ? avatar : null;
  }
  function sphereOf(el) { return el && el.sphere ? el.sphere : null; }
  function geometry(el) {
    var r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2, r: r.width / 2 };
  }
  function ensureCanvas() {
    if (canvas) return;
    canvas = add(document.body, h("canvas", "plasma"));
    ctx = canvas.getContext("2d");
  }
  function resize() {
    var w = Math.round(innerWidth * dpr), hh = Math.round(innerHeight * dpr);
    if (canvas.width !== w || canvas.height !== hh) { canvas.width = w; canvas.height = hh; }
  }

  // filament: the points of one plasma line from a to b: a soft curve whose waves travel along
  // it, calm at both ends, livelier in the middle; continuous in time, so it flows.
  var POINTS = 40;
  function filament(ax, ay, bx, by, t, phase) {
    var dx = bx - ax, dy = by - ay, len = Math.hypot(dx, dy) || 1, nx = -dy / len, ny = dx / len;
    var amp = Math.min(70, len * 0.14), pts = [];
    for (var i = 0; i <= POINTS; i++) {
      var q = i / POINTS, env = Math.sin(Math.PI * q);
      var off = amp * env * (0.55 * Math.sin(q * 6.5 - t * 4.2 + phase) + 0.3 * Math.sin(q * 13 + t * 7.1 + phase) +
        0.15 * Math.sin(q * 23 - t * 11.3));
      pts.push((ax + dx * q + nx * off) * dpr, (ay + dy * q + ny * off) * dpr);
    }
    return pts;
  }
  function trace(pts, width, style) {
    ctx.lineWidth = width;
    ctx.strokeStyle = style;
    ctx.beginPath();
    ctx.moveTo(pts[0], pts[1]);
    for (var i = 2; i < pts.length; i += 2) ctx.lineTo(pts[i], pts[i + 1]);
    ctx.stroke();
  }
  function plasma(pts, a, thick) {
    trace(pts, 26 * thick * dpr, "rgba(" + PLASMA_GLOW + "," + 0.10 * a + ")"); // the violet haze
    trace(pts, 12 * thick * dpr, "rgba(" + PLASMA_GLOW + "," + 0.22 * a + ")");
    trace(pts, 5 * thick * dpr, "rgba(" + PLASMA_BODY + "," + 0.65 * a + ")");   // the filament
    trace(pts, 1.8 * thick * dpr, "rgba(" + PLASMA_CORE + "," + 0.95 * a + ")"); // its bright heart
  }
  function glow(x, y, r, inner, outer, a) {
    var grad = ctx.createRadialGradient(x * dpr, y * dpr, 0, x * dpr, y * dpr, r * dpr);
    grad.addColorStop(0, "rgba(" + inner + "," + a + ")");
    grad.addColorStop(0.35, "rgba(" + outer + "," + 0.45 * a + ")");
    grad.addColorStop(1, "rgba(" + outer + ",0)");
    ctx.fillStyle = grad;
    ctx.fillRect((x - r) * dpr, (y - r) * dpr, 2 * r * dpr, 2 * r * dpr);
  }

  // explode: full of plasma, the sphere bursts
  function explode(g, now, el) {
    flashAt = now;
    waves.push(now, now + 100, now + 220);
    for (var i = 0; i < 90; i++) {
      var a = Math.random() * 6.283, v = 300 + Math.random() * 900;
      sparks.push({ x: g.x, y: g.y, vx: Math.cos(a) * v, vy: Math.sin(a) * v, born: now, life: 700 + Math.random() * 900 });
    }
    charge = 0;
    coolUntil = now + 2500;
    var sp = sphereOf(el);
    if (sp && sp.explode) sp.explode();
    if (navigator.vibrate) navigator.vibrate([120, 40, 200, 60, 90]);
  }

  function draw(now) {
    raf = 0;
    var el = holoNow();
    if (!canvas || !el) return;
    var dt = prev ? Math.min(0.1, (now - prev) / 1000) : 0;
    prev = now;
    resize();
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.globalCompositeOperation = "lighter";
    ctx.lineCap = "round";
    var g = geometry(el), t = now / 1000, sp = sphereOf(el);
    flick += ((0.8 + 0.2 * Math.random()) - flick) * 0.3; // the plasma's gentle flicker

    // the charge: fingers fill it, let go it drains
    if (count > 0 && now > coolUntil) charge = Math.min(1, charge + dt * CHARGE_PER_FINGER * count);
    else charge = Math.max(0, charge - dt * DISCHARGE);
    if (sp && sp.charge) sp.charge(charge);
    if (charge >= 1) explode(g, now, el);

    // the plasma inside: filaments from the core to the shell, more with the charge
    var inner = Math.floor(charge * SEEDS.length + 0.5);
    for (var s = 0; s < inner; s++) {
      var sd = SEEDS[s], ang = sd.a + t * sd.v, rr = g.r * sd.k;
      plasma(filament(g.x, g.y, g.x + Math.cos(ang) * rr, g.y + Math.sin(ang) * rr, t, s * 1.7), 0.35 + 0.5 * charge, 0.55);
    }
    if (charge > 0.02) glow(g.x, g.y, g.r * (0.45 + 0.5 * charge), PLASMA_CORE, PLASMA_GLOW, 0.25 + 0.5 * charge);

    // a filament to each finger, and those just let go fading out
    var ids = Object.keys(fingers);
    for (var i = 0; i < ids.length; i++) {
      var f = fingers[ids[i]];
      plasma(filament(g.x, g.y, f.x, f.y, t, i * 2.3), flick, 1 + 0.4 * charge);
      glow(f.x, f.y, 52 + 30 * charge, "255,235,255", PLASMA_BODY, 0.8 * flick); // where it touches the glass
    }
    for (var j = fades.length - 1; j >= 0; j--) {
      var fd = fades[j];
      fd.a *= 0.82;
      if (fd.a < 0.02) { fades.splice(j, 1); continue; }
      plasma(filament(g.x, g.y, fd.x, fd.y, t, j), fd.a * flick, 1);
    }
    if (ids.length) glow(g.x, g.y, g.r * 0.35, PLASMA_CORE, PLASMA_BODY, 0.5 * flick); // the core they leave
    if (ids.length && navigator.vibrate && now > buzzAt) { // the crackle in the hand, denser with the charge
      navigator.vibrate(4 + Math.floor(Math.random() * 8 + 20 * charge));
      buzzAt = now + (90 + Math.random() * 260) * (1 - 0.75 * charge);
    }

    // the explosion: a flash, sparks flying out
    var since = now - flashAt;
    if (flashAt && since < 450) {
      ctx.fillStyle = "rgba(235,200,255," + 0.75 * (1 - since / 450) + ")";
      ctx.fillRect(0, 0, canvas.width, canvas.height);
    }
    for (var k = sparks.length - 1; k >= 0; k--) {
      var p = sparks[k], age = (now - p.born) / p.life;
      if (age >= 1) { sparks.splice(k, 1); continue; }
      var x0 = p.x, y0 = p.y;
      p.x += p.vx * dt;
      p.y += p.vy * dt;
      p.vx *= 0.97;
      p.vy = p.vy * 0.97 + 400 * dt; // they fall a little
      ctx.lineWidth = (3.5 - 2.5 * age) * dpr;
      ctx.strokeStyle = "rgba(" + (age < 0.3 ? "255,230,255" : PLASMA_BODY) + "," + (1 - age) + ")";
      ctx.beginPath();
      ctx.moveTo(x0 * dpr, y0 * dpr);
      ctx.lineTo((p.x + (p.x - x0)) * dpr, (p.y + (p.y - y0)) * dpr);
      ctx.stroke();
    }
    for (var w = waves.length - 1; w >= 0; w--) { // rings out of the core
      var wa = (now - waves[w]) / 700;
      if (wa < 0) continue;
      if (wa >= 1) { waves.splice(w, 1); continue; }
      ctx.lineWidth = (6 - 5 * wa) * dpr;
      ctx.strokeStyle = "rgba(" + PLASMA_GLOW + "," + (1 - wa) * 0.8 + ")";
      ctx.beginPath();
      ctx.arc(g.x * dpr, g.y * dpr, g.r * (0.3 + 1.6 * wa) * dpr, 0, 6.283);
      ctx.stroke();
    }
    if (ids.length || fades.length || waves.length || sparks.length || charge > 0 || now - flashAt < 450) {
      raf = requestAnimationFrame(draw);
    } else {
      ctx.clearRect(0, 0, canvas.width, canvas.height);
      prev = 0;
    }
  }
  function kickDraw() { if (!raf) raf = requestAnimationFrame(draw); }

  // a touch for the orb: not on Jarvis' button, not on a card (it scrolls), near the hologram
  function claims(e) {
    if (e.target.closest && (e.target.closest(".talk-button") || e.target.closest(".frame:not(.stage)"))) return null;
    var el = holoNow();
    if (!el) return null;
    var g = geometry(el), d = Math.hypot(e.clientX - g.x, e.clientY - g.y);
    var big = el.classList.contains("avatar") ? 1.5 : 2.2; // the console's free space is the orb's
    return d <= g.r * big ? { el: el, g: g, d: d } : null;
  }

  document.addEventListener("pointerdown", function (e) {
    var c = claims(e);
    if (!c) return;
    ensureCanvas();
    var now = performance.now();
    fingers[e.pointerId] = { x: e.clientX, y: e.clientY };
    count = Object.keys(fingers).length;
    if (count === 1) { // the first finger: taps and turning
      last = { x: e.clientX, y: e.clientY, t: now };
      downAt = now;
      moved = 0;
      dragId = c.d <= c.g.r * 0.9 ? e.pointerId : null;
    } else {
      moved = 999; // several fingers are no tap
    }
    var sp = sphereOf(c.el);
    if (sp && sp.excite) sp.excite(1);
    kickDraw();
  });
  document.addEventListener("pointermove", function (e) {
    var f = fingers[e.pointerId];
    if (!f) return;
    if (e.pointerId === dragId || count === 1) {
      var now = performance.now(), dx = e.clientX - last.x, dy = e.clientY - last.y;
      moved += Math.hypot(dx, dy);
      var el = holoNow(), sp = sphereOf(el);
      if (e.pointerId === dragId && sp && sp.drag) {
        var g = geometry(el);
        sp.drag(dx / g.r * 1.6, dy / g.r * 0.9, (now - last.t) / 1000);
      }
      last = { x: e.clientX, y: e.clientY, t: now };
    }
    f.x = e.clientX;
    f.y = e.clientY;
  });
  // the console must not scroll under fingers that play
  document.addEventListener("touchmove", function (e) { if (count) e.preventDefault(); }, { passive: false });
  function release(e) {
    var f = fingers[e.pointerId];
    if (!f) return;
    delete fingers[e.pointerId];
    count = Object.keys(fingers).length;
    fades.push({ x: f.x, y: f.y, a: 1 });
    if (e.pointerId === dragId) dragId = null;
    var now = performance.now(), el = holoNow(), sp = sphereOf(el);
    if (!count && sp && sp.excite) sp.excite(0);
    kickDraw();
    if (count || now - downAt > 250 || moved > 14) return;
    taps.push(now);
    taps = taps.filter(function (t) { return now - t < 1600; });
    if (taps.length >= 5) { // the easter egg
      taps = [];
      if (sp && sp.wild) sp.wild();
      if (navigator.vibrate) navigator.vibrate([20, 40, 20, 40, 60]);
    } else if (taps.length >= 2 && now - taps[taps.length - 2] < 320) {
      waves.push(now, now + 120);
      if (sp && sp.pulse) sp.pulse();
      if (navigator.vibrate) navigator.vibrate(30);
    }
  }
  document.addEventListener("pointerup", release);
  document.addEventListener("pointercancel", release);
}

// talkButton: the phone's Jarvis button. A tap starts a conversation (Jarvis hears when the user
// is done) or ends it; held, it is push to talk. The app decides which on release
// (AudioBridge.press, release). A small arc reactor in the hologram's colours: it breathes amber
// at rest, sends turquoise waves while the microphone is open (its glow follows the voice),
// spins purple while Jarvis thinks and pulses with his voice while he speaks.
var talkState = "idle";
function talkButton(hud) {
  var b = add(hud, h("div", "talk-button idle"));
  add(b, h("span", "ripple"));
  add(b, h("span", "ripple r2"));
  add(b, orb("idle"));
  var down = false;
  function press(e) {
    e.preventDefault();
    if (down) return;
    down = true;
    b.classList.add("down");
    if (window.Card) Card.press();
  }
  function release(e) {
    e.preventDefault();
    if (!down) return;
    down = false;
    b.classList.remove("down");
    if (window.Card) Card.release();
  }
  b.addEventListener("pointerdown", press);
  b.addEventListener("pointerup", release);
  b.addEventListener("pointercancel", release);
  b.addEventListener("pointerleave", release);
  var shown = "";
  setInterval(function () {
    var mic = !!(window.Card && Card.listening());
    var state = mic ? "listening" : talkState === "listening" ? "idle" : talkState;
    // Push to talk: Jarvis waits for the next press with the microphone closed. "Ich höre" made
    // the user speak without holding, and nothing happened (01.11.2026).
    var label = document.querySelector("#card.is-status .state");
    if (label) label.textContent = talkState === "listening" && !mic ? "Halten zum Sprechen" : STATES[talkState];
    if (state === shown) return; // a class set again would restart the orb's animations
    shown = state;
    b.className = "talk-button " + state + (down ? " down" : "");
    b.querySelector(".orb").setAttribute("class", "orb " + state);
  }, 150);
}

function tickClock() {
  var now = new Date();
  if (clock) clock.textContent = two(now.getHours()) + ":" + two(now.getMinutes());
  if (date) date.textContent = now.toLocaleDateString("de-DE", { weekday: "short", day: "2-digit", month: "2-digit" });
  document.querySelectorAll(".ambient .big").forEach(function (el) { el.textContent = two(now.getHours()) + ":" + two(now.getMinutes()); });
  document.querySelectorAll(".ambient .day").forEach(function (el) {
    el.textContent = now.toLocaleDateString("de-DE", { weekday: "long" }) + "\n" +
      now.toLocaleDateString("de-DE", { day: "numeric", month: "long" });
  });
  if (leftBar) leftBar.style.width = shownFor ? Math.max(0, 100 * (1 - (Date.now() - shownAt) / shownFor)) + "%" : "0";
}

// The blocks of a card; the first format becomes blocks too.
// blocksOf: the card's text, picture and list first, then its blocks. Text and items were drawn
// only without blocks: a list with an empty gallery beside it showed the heading alone, and a
// recipe lost its ingredients (29.10.2026). Blocks with an empty list are left out.
function blocksOf(card) {
  var blocks = [];
  if (card.text) blocks.push({ type: "text", text: card.text });
  if (card.image) blocks.push({ type: "image", url: card.image });
  if (Array.isArray(card.items) && card.items.length) blocks.push({ type: "list", items: card.items });
  (Array.isArray(card.blocks) ? card.blocks : []).forEach(function (b) {
    if (b && !(Array.isArray(b.items) && b.items.length === 0 && !b.now)) blocks.push(b);
  });
  return blocks;
}

// guessType: a block the model wrote without its type, told by its content.
function guessType(b) {
  var first = Array.isArray(b.items) && b.items.length ? b.items[0] : null;
  if (b.now || first && first.condition !== undefined) return "forecast";
  if (b.columns || b.rows) return "table";
  if (b.series) return "chart";
  if (b.url && /\.(mp4|webm|m3u8)(\?|$)/i.test(b.url)) return "video";
  if (b.url) return "image";
  if (b.seconds !== undefined || b.ends_at) return "timer";
  if (b.value !== undefined && b.label !== undefined && !b.items) return "progress";
  if (first && first.time !== undefined) return "timeline";
  if (first && first.label !== undefined && first.value !== undefined) return "stats";
  if (b.items) return b.current !== undefined ? "steps" : "list";
  return "text";
}

function panel(cls) {
  var frame = h("div", "frame"), edge = add(frame, h("div", "edge " + (cls || "")));
  return { frame: frame, inner: add(edge, h("div", "panel")) };
}

function head(card, kind) {
  var hd = h("div", "head");
  var ic = add(hd, h("div", "icon"));
  ic.innerHTML = icon(I[kind[1]] || I.info);
  var t = add(hd, h("div"));
  var eb = add(t, h("div", "eyebrow", kind[0]));
  if (card.eyebrow) { add(eb, h("span", "dot")); eb.appendChild(document.createTextNode(card.eyebrow)); }
  add(t, h("h1", "", card.title || "Jarvis"));
  if (card.subtitle) add(t, h("div", "sub", card.subtitle));
  return hd;
}

function cardView(card) {
  var kindName = KINDS[card.kind] ? card.kind : "info";
  var p = panel("kind-" + kindName);
  p.frame.classList.add("kind-" + kindName);
  if (FULL && !card.side) p.frame.classList.add("cast"); // unfolds from the avatar's side
  var c = add(p.inner, h("div", "card"));
  add(c, head(card, KINDS[kindName]));
  var blocks = add(c, h("div", "blocks"));
  if (card.from) {
    var from = add(blocks, h("div", "from block wide"));
    add(from, h("div", "avatar", (card.from.name || "?").trim().charAt(0).toUpperCase()));
    var who = add(from, h("div"));
    add(who, h("div", "who", card.from.name));
    add(who, h("div", "via", card.from.via));
  }
  blocksOf(card).forEach(function (b, i) {
    if (!b.type) b.type = guessType(b);
    var make = BLOCKS[b.type];
    if (!make) return;
    var el = make(b);
    if (!el) return;
    var wrap = add(blocks, h("div", "block"));
    if (b.wide || b.type === "image" && FULL && blocksOf(card).length <= 2) wrap.classList.add("wide");
    wrap.style.animationDelay = (0.08 * i + 0.1) + "s";
    if (b.label) add(wrap, h("div", "block-label", b.label));
    wrap.appendChild(el);
  });
  return p.frame;
}

// Jarvis' state with live captions: {kind: "status", state: idle|listening|thinking|speaking, user,
// jarvis}. A new status while one is shown only updates it: the hologram changes its motion
// smoothly instead of starting again.
var STATES = { idle: "Bereit", listening: "Ich höre", thinking: "Einen Moment", speaking: "Jarvis" };
function statusView(card) {
  // full screen: the hologram floats free with the words below it; the overlay has a panel
  var p = FULL ? { frame: h("div", "stage") } : panel();
  p.inner = p.inner || p.frame;
  var s = add(p.inner, h("div", "status"));
  add(s, holo(card.state));
  var t = add(s, h("div", "words"));
  add(t, h("div", "state"));
  if (card.console) {
    p.frame.classList.add("console");
    add(t, h("div", "history"));
  }
  add(t, h("div", "caption"));
  updateStatus(p.frame, card);
  return p.frame;
}

// The console keeps the last turns above the current one.
var turn = { user: "", jarvis: "" };
function keepTurn(view, card) {
  var history = view.querySelector(".history");
  if (!history) return;
  var newTurn = card.user && turn.user && card.user.indexOf(turn.user.slice(0, 12)) !== 0;
  if (newTurn && (turn.user || turn.jarvis)) {
    var line = add(history, h("div", "turn"));
    if (turn.user) add(line, h("div", "you", "„" + turn.user + "“"));
    if (turn.jarvis) add(line, h("div", "", turn.jarvis));
    while (history.children.length > 3) history.removeChild(history.firstChild);
  }
  if (card.user !== undefined) turn.user = card.user || turn.user;
  if (newTurn || card.jarvis) turn.jarvis = card.jarvis || "";
}

function updateStatus(view, card) {
  keepTurn(view, card);
  var state = STATES[card.state] ? card.state : "idle";
  talkState = state;
  var hl = view.querySelector(".holo");
  if (hl) {
    hl.className = "holo " + state;
    if (hl.sphere) hl.sphere.setState(state);
  }
  view.querySelector(".state").textContent = STATES[state];
  var cap = view.querySelector(".caption");
  cap.textContent = "";
  if (card.user) add(cap, h("span", "you", "„" + card.user + "“ "));
  if (card.jarvis) add(cap, h("span", "", card.jarvis));
}

// The idle screen: a dial with the time, weather on the left, the day on the right.
function ambientView(card) {
  var box = h("div", "ambient");
  var left = add(box, h("div", "side"));
  if (card.weather) add(left, cardView({ side: true, kind: "weather", title: card.weather.title || "Draußen", blocks: [{ type: "forecast", now: card.weather.now, items: (card.weather.items || []).slice(0, 4) }] }));
  var dial = add(box, h("div", "dial"));
  add(dial, holo("idle"));
  var center = add(dial, h("div", "center"));
  add(center, h("div", "big"));
  add(center, h("div", "day"));
  var right = add(box, h("div", "side"));
  if (card.agenda) add(right, cardView({ side: true, kind: "calendar", title: card.agenda.title || "Heute", blocks: [{ type: "timeline", items: card.agenda.items }] }));
  return box;
}

// A web page ({"mode": "web", "url", "title"?}) becomes a card with the page in a frame.
function webCard(card) {
  var host = (card.url || "").replace(/^https?:\/\/(www\.|m\.)?/, "").split("/")[0];
  return { kind: "web", title: card.title || host, subtitle: card.subtitle || card.url, eyebrow: host,
    seconds: card.seconds, blocks: [{ type: "web", url: card.url, wide: true }] };
}

// setPageTitle: the framed page's title and address once it has loaded (called by the app).
function setPageTitle(title, address) {
  var h1 = document.querySelector("#card h1"), sub = document.querySelector("#card .sub");
  if (h1 && title) h1.textContent = title;
  if (sub && address) sub.textContent = address;
}

// render: one card replaces the last one.
function render(card) {
  card = card || {};
  if (card.mode === "web") card = webCard(card);
  if (!root) buildPage();
  fit();
  var shown = document.getElementById("card");
  if (card.kind === "status" && shown && shown.classList.contains("is-status")) {
    updateStatus(shown, card);
    shownAt = Date.now();
    shownFor = card.seconds > 0 ? 1000 * card.seconds : consoleMode ? 0 : 1000 * DEFAULT_SECONDS;
    report();
    return;
  }
  // Jarvis' state while a card is shown: the card stays; the avatar takes the state, the words
  // run in the caption line (the overlay keeps its card as it is).
  if (card.kind === "status" && shown) {
    if (FULL) {
      setAvatar(card.state);
      captions(card);
    }
    return;
  }
  main.textContent = "";
  var view;
  if (card.kind === "status") { view = statusView(card); view.classList.add("is-status"); }
  else if (card.kind === "ambient" && FULL) view = ambientView(card);
  else view = cardView(card);
  view.id = "card";
  view.classList.add("enter");
  main.appendChild(view);
  if (FULL) {
    var solo = card.kind === "status" || card.kind === "ambient";
    root.classList.toggle("solo", solo);
    var hidden = avatar.querySelector("canvas"); // none with the 2D orb
    if (hidden) hidden.paused = solo; // hidden then: it draws nothing (jarvis3d.js)
    root.classList.toggle("wide", card.kind === "web" || card.kind === "video");
    if (card.state) setAvatar(card.state); // else the avatar keeps what Jarvis does now
    if (card.kind === "status") captions({}); // the console shows the words itself
  }
  if (label) label.textContent = (KINDS[card.kind] || ["System"])[0];
  shownAt = Date.now();
  if (card.console) consoleMode = true; // cards stay in the console: no running time
  shownFor = card.seconds > 0 ? 1000 * card.seconds : consoleMode ? 0 : 1000 * DEFAULT_SECONDS;
  tickClock();
  report();
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(report);
  setTimeout(report, 600);
}

// setAvatar: the hologram on the left listens, thinks, speaks or rests.
function setAvatar(state) {
  if (!avatar) return;
  state = STATES[state] ? state : "idle";
  talkState = state;
  avatar.className = "holo avatar " + state;
  if (avatar.sphere) avatar.sphere.setState(state);
}

// captions: the conversation's words under a card (the user's in amber, Jarvis' in white); the
// line fades out when Jarvis rests. Long answers covered the card (30.10.2026): Jarvis' newest
// words only, about three lines, the user's beginning only.
var captionLine;
var CAPTION_JARVIS = 210, CAPTION_USER = 110;
function captionTail(text, max) {
  if (text.length <= max) return text;
  var cut = text.slice(text.length - max), space = cut.indexOf(" ");
  return "… " + (space >= 0 && space < 30 ? cut.slice(space + 1) : cut);
}
function captionHead(text, max) {
  if (text.length <= max) return text;
  var cut = text.slice(0, max), space = cut.lastIndexOf(" ");
  return (space > max - 30 ? cut.slice(0, space) : cut) + " …";
}
function captions(card) {
  if (!root) return;
  if (!captionLine) captionLine = add(root, h("div", "captionline"));
  captionLine.textContent = "";
  var state = STATES[card.state] ? card.state : "idle";
  if (card.user) add(captionLine, h("div", "you", "„" + captionHead(card.user, CAPTION_USER) + "“"));
  if (card.jarvis) add(captionLine, h("div", "", captionTail(card.jarvis, CAPTION_JARVIS)));
  else if (state !== "idle") add(captionLine, h("div", "hint", STATES[state] + " …"));
  captionLine.classList.toggle("off", state === "idle" && !card.jarvis || !card.state);
}

// Tells the overlay window its height (no bridge in full screen).
function report() {
  var card = document.getElementById("card");
  if (window.Card && card) Card.resize(Math.ceil(card.getBoundingClientRect().height) + 12);
}

buildPage();
