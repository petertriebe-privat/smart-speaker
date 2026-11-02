// The assistant as a hologram: a sphere drawn in glowing amber lines, after the film's holographic
// butler (our own geometry, no film assets): banded rings around it, an eye-like ring core with
// spokes running out, circuit traces with nodes on the shells, arc dashes and small panels, soft
// out-of-focus lights (bokeh) and sparks. Each shell turns at its own speed and tilt, the far side
// dims. Lines are screen-space quads (WebGL lines are 1 px), with a bright core and a glow.
// Sphere3D.start(canvas, state) runs until the canvas leaves the page; null without WebGL.
"use strict";

var Sphere3D = (function () {
  // Shared by both programs: rotate per shell, breathe with the pulse, project.
  var COMMON =
    "uniform mat3 uRot[6]; uniform float uTime, uPulse, uFlicker, uPx, uScale;\n" +
    "vec3 place(vec3 p, float shell, float seed) {\n" +
    "  int s = int(shell + 0.5);\n" +
    "  p *= uScale * (1.0 + uPulse * 0.03 * sin(uTime * 2.8 + seed * 9.0 + shell));\n" +
    "  vec3 q = uRot[s] * p;\n" +
    "  float persp = 3.4 / (3.4 - q.z);\n" +
    "  return vec3(q.xy * persp * 0.8, q.z);\n" +
    "}\n" +
    "float shade(float z, float seed, float bright) {\n" +
    "  float depth = clamp((z + 1.1) / 2.2, 0.0, 1.0);\n" +
    "  float tw = 0.75 + 0.25 * sin(uTime * (0.4 + seed * 1.3) * uFlicker + seed * 61.0);\n" +
    "  return bright * mix(0.12, 1.0, depth * depth) * tw;\n" +
    "}\n";
  var LINE_VS =
    "attribute vec3 aA, aB; attribute vec4 aInfo; attribute vec2 aCorner;\n" + // info: shell, seed, width, bright
    COMMON +
    "varying float vSide, vAlpha, vHot;\n" +
    "void main() {\n" +
    "  vec3 a = place(aA, aInfo.x, aInfo.y), b = place(aB, aInfo.x, aInfo.y);\n" +
    "  vec2 dir = b.xy - a.xy;\n" +
    "  dir = length(dir) > 1e-5 ? normalize(dir) : vec2(1.0, 0.0);\n" +
    "  vec3 p = mix(a, b, aCorner.x);\n" +
    "  float halfw = aInfo.z * uPx;\n" + // in clip units
    "  gl_Position = vec4(p.xy + vec2(-dir.y, dir.x) * aCorner.y * halfw, 0.0, 1.0);\n" +
    "  vSide = aCorner.y;\n" +
    "  vAlpha = shade(p.z, aInfo.y, aInfo.w);\n" +
    "  vHot = aInfo.w;\n" +
    "}\n";
  var LINE_FS =
    "precision mediump float;\n" +
    "uniform vec3 uTint; uniform float uTintMix;\n" +
    "varying float vSide, vAlpha, vHot;\n" +
    "void main() {\n" +
    "  float d = abs(vSide);\n" +
    "  float core = 1.0 - smoothstep(0.0, 0.32, d);\n" +
    "  float glow = (1.0 - d) * (1.0 - d) * 0.55;\n" +
    "  float k = (core + glow) * vAlpha;\n" +
    "  vec3 col = mix(vec3(1.0, 0.5, 0.08), vec3(1.0, 0.9, 0.62), core * vHot);\n" +
    "  col = mix(col, mix(uTint, vec3(1.0), core * vHot * 0.5), uTintMix);\n" +
    "  gl_FragColor = vec4(col * k * 1.25, k);\n" +
    "}\n";
  var POINT_VS =
    "attribute vec3 aPos; attribute vec4 aInfo;\n" + // info: shell, seed, size (px at 420), bright
    COMMON +
    "uniform float uPtPx;\n" +
    "varying float vAlpha, vSoft;\n" +
    "void main() {\n" +
    "  vec3 p = place(aPos, aInfo.x, aInfo.y);\n" +
    "  gl_Position = vec4(p.xy, 0.0, 1.0);\n" +
    "  vAlpha = shade(p.z, aInfo.y, aInfo.w);\n" +
    "  vSoft = aInfo.z > 6.0 ? 1.0 : 0.0;\n" + // large points are bokeh: soft all the way
    "  gl_PointSize = aInfo.z * uPtPx;\n" +
    "}\n";
  var POINT_FS =
    "precision mediump float;\n" +
    "uniform vec3 uTint; uniform float uTintMix;\n" +
    "varying float vAlpha, vSoft;\n" +
    "void main() {\n" +
    "  float d = length(gl_PointCoord - 0.5) * 2.0;\n" +
    "  float a = vSoft > 0.5 ? (1.0 - d) * (1.0 - d) : 1.0 - smoothstep(0.2, 1.0, d);\n" +
    "  float k = max(a, 0.0) * vAlpha;\n" +
    "  vec3 col = mix(vec3(1.0, 0.72, 0.3), mix(uTint, vec3(1.0), 0.35), uTintMix);\n" +
    "  gl_FragColor = vec4(col * k * 1.3, k);\n" +
    "}\n";

  function rng(seed) {
    return function () { seed = (seed * 16807) % 2147483647; return (seed - 1) / 2147483646; };
  }

  // Geometry in sphere units; shells 0..4 turn separately, 5 is the core and the main rings.
  function geometry() {
    var r = rng(7), A = [], B = [], LI = [], C = [], P = [], PI = [];
    function sph(rad, lat, lon) { return [rad * Math.cos(lat) * Math.cos(lon), rad * Math.sin(lat), rad * Math.cos(lat) * Math.sin(lon)]; }
    // one segment = two triangles; corners (t, side)
    var CORNERS = [[0, -1], [1, -1], [1, 1], [0, -1], [1, 1], [0, 1]];
    function seg(a, b, shell, width, bright) {
      var seed = r();
      for (var i = 0; i < 6; i++) {
        A.push(a[0], a[1], a[2]);
        B.push(b[0], b[1], b[2]);
        LI.push(shell, seed, width, bright);
        C.push(CORNERS[i][0], CORNERS[i][1]);
      }
    }
    function path(points, shell, width, bright) {
      for (var i = 1; i < points.length; i++) seg(points[i - 1], points[i], shell, width, bright);
    }
    function point(p, shell, size, bright) { P.push(p[0], p[1], p[2]); PI.push(shell, r(), size, bright); }
    // a circle of radius rad tilted by ax (about x) then ay (about y), from a0 to a1
    function circle(rad, ax, ay, a0, a1, n) {
      var pts = [], ca = Math.cos(ax), sa = Math.sin(ax), cb = Math.cos(ay), sb = Math.sin(ay);
      for (var i = 0; i <= n; i++) {
        var a = a0 + (a1 - a0) * i / n, x = rad * Math.cos(a), z = rad * Math.sin(a);
        var y1 = -z * sa, z1 = z * ca, x2 = x * cb + z1 * sb, z2 = -x * sb + z1 * cb;
        pts.push([x2, y1, z2]);
      }
      return pts;
    }

    // 1. ring bands: a few broad bands of parallel lines around the sphere
    [[1.0, 1.25, 0.2], [0.97, -0.5, 1.3], [0.9, 0.15, -0.9]].forEach(function (band, b) {
      for (var l = 0; l < 6; l++) {
        var rad = band[0] - l * 0.012, gap = r() * 6.28, len = 4.6 + r() * 1.4;
        path(circle(rad, band[1] + l * 0.004, band[2], gap, gap + len, 140), 4, l === 0 || l === 5 ? 1.6 : 1.0,
          l === 0 || l === 5 ? 0.95 : 0.5);
      }
    });
    // 2. the eye: rings around the core facing the viewer (shell 5 does not spin), with ticks
    [[0.15, 2.6, 1.0], [0.2, 1.6, 0.85], [0.28, 1.1, 0.6], [0.36, 0.8, 0.4]].forEach(function (ring, i) {
      path(circle(ring[0], 1.5708, 0, 0, 6.29, 72), 5, ring[1], ring[2]);
    });
    for (var t = 0; t < 36; t++) {
      var tick = circle(0.24, 1.5708, 0, t / 36 * 6.283, t / 36 * 6.283, 1)[0];
      seg(tick, [tick[0] * 1.18, tick[1] * 1.18, tick[2] * 1.18], 5, 0.8, 0.5);
    }
    // 3. spokes: lines from the eye out to the shell, slightly bent
    for (var s = 0; s < 14; s++) {
      var lat = (r() - 0.5) * 2.4, lon = r() * 6.283, pts = [];
      for (var k = 0; k <= 12; k++) {
        var rr = 0.16 + 0.86 * k / 12;
        pts.push(sph(rr, lat + Math.sin(k * 0.5) * 0.04, lon + k * 0.012));
      }
      path(pts, 4, s % 3 === 0 ? 1.5 : 0.8, s % 3 === 0 ? 0.9 : 0.5);
    }
    // 4. circuit traces: steps along latitude and meridian with nodes, on the inner shells
    [[0.86, 1], [0.74, 2], [0.62, 3]].forEach(function (sh) {
      for (var c = 0; c < 46; c++) {
        var la = Math.asin(1.7 * r() - 0.85), lo = r() * 6.283, pts2 = [sph(sh[0], la, lo)];
        var steps = 2 + Math.floor(r() * 5);
        for (var st = 0; st < steps; st++) {
          var horiz = st % 2 === 0, d = (r() < 0.5 ? -1 : 1) * (0.05 + r() * 0.18), n2 = 4;
          for (var m = 1; m <= n2; m++) {
            if (horiz) lo += d / n2 / Math.max(0.4, Math.cos(la)); else la += d / n2;
            pts2.push(sph(sh[0], la, lo));
          }
        }
        var bright = 0.35 + r() * 0.55;
        path(pts2, sh[1], 0.75, bright);
        point(pts2[pts2.length - 1], sh[1], 3.2, Math.min(1, bright + 0.3)); // node at the end
      }
    });
    // 5. arc dashes on the outer shell and small panels
    for (var i = 0; i < 120; i++) {
      var la3 = Math.asin(2 * r() - 1), lo3 = r() * 6.283, len3 = 0.05 + Math.pow(r(), 2) * 0.5, pts3 = [];
      for (var k3 = 0; k3 <= 6; k3++) pts3.push(sph(1.02, la3, lo3 + len3 * k3 / 6 / Math.max(0.3, Math.cos(la3))));
      path(pts3, 0, 0.7, 0.3 + r() * 0.5);
    }
    for (var p = 0; p < 24; p++) {
      var la4 = Math.asin(1.6 * r() - 0.8), lo4 = r() * 6.283, w = 0.1 + r() * 0.12, hg = 0.06 + r() * 0.08, rad4 = 0.94;
      var corners = [sph(rad4, la4, lo4), sph(rad4, la4, lo4 + w), sph(rad4, la4 + hg, lo4 + w), sph(rad4, la4 + hg, lo4), sph(rad4, la4, lo4)];
      path(corners, 0, 0.6, 0.55);
      for (var l2 = 1; l2 < 3; l2++) seg(sph(rad4, la4 + hg * l2 / 3, lo4 + 0.01), sph(rad4, la4 + hg * l2 / 3, lo4 + w * r()), 0, 0.5, 0.4);
    }
    // 6. outer frame rings: thin full circles that give the sphere its outline
    path(circle(1.06, 0.02, 0, 0, 6.29, 160), 4, 0.7, 0.45);
    path(circle(1.1, 1.57, 0.3, 0, 6.29, 160), 4, 0.5, 0.3);
    // 7. points: the core, nodes, sparks and soft bokeh
    for (var c2 = 0; c2 < 140; c2++) point(sph(Math.pow(r(), 2) * 0.1, Math.asin(2 * r() - 1), r() * 6.283), 5, 2.5 + r() * 3, 1);
    for (var b2 = 0; b2 < 90; b2++) point(sph(0.2 + r() * 0.9, Math.asin(2 * r() - 1), r() * 6.283), b2 % 5, 9 + r() * 16, 0.08 + r() * 0.16);
    for (var s2 = 0; s2 < 260; s2++) point(sph(0.3 + r() * 0.8, Math.asin(2 * r() - 1), r() * 6.283), s2 % 5, 1.2 + r() * 1.6, 0.3 + r() * 0.5);
    return {
      a: new Float32Array(A), b: new Float32Array(B), li: new Float32Array(LI), c: new Float32Array(C), lines: C.length / 2,
      p: new Float32Array(P), pi: new Float32Array(PI), points: P.length / 3
    };
  }

  var SPIN = [0.08, -0.12, 0.17, -0.22, 0.06, 0], TILT = [0.3, -0.2, 0.45, 0.1, 0.25, 0];
  // rot writes Rx(tilt) * Ry(spin), column-major, into out at off. Nothing in the frame loop
  // allocates: the garbage of new arrays every frame froze the console for 1.5 s every 10 s
  // (29.10.2026, "Davey! duration=1555ms").
  function rot(out, off, spin, tilt) {
    var cs = Math.cos(spin), ss = Math.sin(spin), ct = Math.cos(tilt), st = Math.sin(tilt);
    out[off] = cs; out[off + 1] = st * ss; out[off + 2] = -ct * ss;
    out[off + 3] = 0; out[off + 4] = ct; out[off + 5] = st;
    out[off + 6] = ss; out[off + 7] = -st * cs; out[off + 8] = ct * cs;
  }
  // per state: spin speed, pulse, flicker
  var STATES = { idle: [1, 0, 1], listening: [1.3, 0.5, 1.4], thinking: [2.6, 0.15, 2.2], speaking: [1.5, 1, 1.6] };
  // The Echo's ring colours (the ring states in core/cmd/assistant/main.go): listening turquoise with the
  // voice, thinking purple running over turquoise, speaking turquoise brightening to white with
  // his voice; idle keeps the amber of the machine (tint 0).
  var TEAL = [0, 0.82, 0.9], PURPLE = [0.59, 0, 1], WHITE = [1, 1, 1], PLASMA = [0.88, 0.4, 1];
  function mix3(out, a, b, t) {
    out[0] = a[0] + (b[0] - a[0]) * t; out[1] = a[1] + (b[1] - a[1]) * t; out[2] = a[2] + (b[2] - a[2]) * t;
  }
  // look fills want {tint, mix: tint share, size} for a state at a time (s) and voice level (0..1)
  function look(want, state, t, lv) {
    switch (state) {
      case "listening": mix3(want.tint, TEAL, WHITE, lv * 0.3); want.mix = 0.8; want.size = 1 + 0.06 * lv; break;
      case "thinking": mix3(want.tint, PURPLE, TEAL, 0.5 + 0.5 * Math.sin(t * 3)); want.mix = 0.8; want.size = 1 + 0.035 * Math.sin(t * 5); break;
      case "speaking": mix3(want.tint, TEAL, WHITE, lv * 0.7); want.mix = 0.8; want.size = 1 + 0.1 * lv; break;
      default: mix3(want.tint, TEAL, TEAL, 0); want.mix = 0; want.size = 1;
    }
  }
  // level: the loudness of the voice heard or spoken, set by the app (setLevel in assistant.js) and
  // falling off by itself; without it (no audio bridge) speaking pulses by a rhythm of its own.
  var level = 0, levelAt = 0;

  function program(gl, vs, fs) {
    function compile(type, src) {
      var sh = gl.createShader(type);
      gl.shaderSource(sh, src);
      gl.compileShader(sh);
      if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(sh));
      return sh;
    }
    var p = gl.createProgram();
    gl.attachShader(p, compile(gl.VERTEX_SHADER, vs));
    gl.attachShader(p, compile(gl.FRAGMENT_SHADER, fs));
    gl.linkProgram(p);
    if (!gl.getProgramParameter(p, gl.LINK_STATUS)) throw new Error(gl.getProgramInfoLog(p));
    return p;
  }

  var GEO = null;

  function start(canvas, state) {
    var gl = canvas.getContext("webgl", { alpha: true, premultipliedAlpha: true, antialias: true }) ||
      canvas.getContext("experimental-webgl");
    if (!gl) return null;
    var lp, pp;
    try {
      lp = program(gl, LINE_VS, LINE_FS);
      pp = program(gl, POINT_VS, POINT_FS);
    } catch (e) {
      console.warn("3D orb: " + e.message);
      return null;
    }
    GEO = GEO || geometry();
    function buffer(data) {
      var buf = gl.createBuffer();
      gl.bindBuffer(gl.ARRAY_BUFFER, buf);
      gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW);
      return buf;
    }
    var bufs = { a: buffer(GEO.a), b: buffer(GEO.b), li: buffer(GEO.li), c: buffer(GEO.c), p: buffer(GEO.p), pi: buffer(GEO.pi) };
    function bind(prog, name, buf, size) {
      var loc = gl.getAttribLocation(prog, name);
      gl.bindBuffer(gl.ARRAY_BUFFER, buf);
      gl.enableVertexAttribArray(loc);
      gl.vertexAttribPointer(loc, size, gl.FLOAT, false, 0, 0);
      return loc;
    }
    function uniforms(prog) {
      return { rot: gl.getUniformLocation(prog, "uRot"), time: gl.getUniformLocation(prog, "uTime"),
        pulse: gl.getUniformLocation(prog, "uPulse"), flicker: gl.getUniformLocation(prog, "uFlicker"),
        px: gl.getUniformLocation(prog, "uPx"), ptpx: gl.getUniformLocation(prog, "uPtPx"),
        scale: gl.getUniformLocation(prog, "uScale"), tint: gl.getUniformLocation(prog, "uTint"),
        tintMix: gl.getUniformLocation(prog, "uTintMix") };
    }
    var lu = uniforms(lp), pu = uniforms(pp);
    gl.disable(gl.DEPTH_TEST);
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.ONE, gl.ONE);

    var name = STATES[state] ? state : "idle", target = STATES[name], cur = target.slice(), angle = 0, last = performance.now();
    var tint = TEAL.slice(), tintMix = 0, scale = 1, lv = 0, want = { tint: [0, 0, 0], mix: 0, size: 1 };
    var mats = new Float32Array(54);
    var raf = 0, alive = true, enabled = [], logged = false;
    // Play by touch (the phone, assistant.js orbPlay): the finger turns the sphere, which keeps
    // spinning and slows down; tilted, it springs back; touched, it glows (boost); "wild" spins it
    // fast through every colour for a moment.
    var userAngle = 0, userVel = 0, tiltOff = 0, boost = 0, boostTarget = 0, kick = 0, wildUntil = 0;
    // charged with plasma (0..1): it glows violet, grows and spins faster; explode: it swells,
    // collapses to a point and builds itself anew
    var charged = 0, boomAt = 0;
    var LINE_ATTRS = [["aA", bufs.a, 3], ["aB", bufs.b, 3], ["aInfo", bufs.li, 4], ["aCorner", bufs.c, 2]];
    var POINT_ATTRS = [["aPos", bufs.p, 3], ["aInfo", bufs.pi, 4]];
    // use switches programs: first the old attributes off, then the new ones bound (both programs
    // share attribute numbers, binding first and disabling after left nothing to draw)
    function use(prog, attrs) {
      for (var i = 0; i < enabled.length; i++) gl.disableVertexAttribArray(enabled[i]);
      gl.useProgram(prog);
      enabled.length = 0;
      for (var j = 0; j < attrs.length; j++) enabled.push(bind(prog, attrs[j][0], attrs[j][1], attrs[j][2]));
    }
    function set(u, mats, now, w) {
      gl.uniformMatrix3fv(u.rot, false, mats);
      gl.uniform1f(u.time, now / 1000);
      gl.uniform1f(u.pulse, cur[1]);
      gl.uniform1f(u.flicker, cur[2]);
      gl.uniform1f(u.scale, scale);
      gl.uniform3f(u.tint, tint[0], tint[1], tint[2]);
      gl.uniform1f(u.tintMix, tintMix);
      // line half width in clip units: grows with the canvas (~1.7 px at 420 px), at least 1.2 px
      gl.uniform1f(u.px, Math.max(3.4 / 420, 2.4 / w));
      if (u.ptpx) gl.uniform1f(u.ptpx, w / 420);
    }
    function frame(now) {
      if (!canvas.isConnected) { stop(); return; }
      var dt = Math.min(0.1, (now - last) / 1000);
      last = now;
      for (var i = 0; i < 3; i++) cur[i] += (target[i] - cur[i]) * Math.min(1, dt * 3);
      angle += dt * cur[0];
      var t = now / 1000, heard = level * Math.pow(0.5, (now - levelAt) / 150); // half every 150 ms
      if (name === "speaking" && now - levelAt > 1000) heard = 0.35 + 0.3 * Math.abs(Math.sin(t * 7) * Math.sin(t * 2.3));
      lv += (heard - lv) * Math.min(1, dt * 14);
      look(want, name, t, lv);
      userVel *= Math.pow(0.3, dt);
      userAngle += userVel * dt;
      tiltOff += (0 - tiltOff) * Math.min(1, dt * 1.5);
      boost += (boostTarget - boost) * Math.min(1, dt * 8);
      kick *= Math.pow(0.02, dt);
      if (boost > 0.01 || kick > 0.01) {
        mix3(want.tint, want.tint, PLASMA, 0.7 * boost); // touched: the plasma ball's violet
        mix3(want.tint, want.tint, WHITE, 0.5 * kick);
        want.mix = Math.max(want.mix, 0.9 * Math.max(boost, kick));
        want.size += 0.08 * boost + 0.18 * kick;
      }
      if (charged > 0.01) { // filling with plasma
        mix3(want.tint, want.tint, PLASMA, Math.min(1, 0.4 + charged));
        want.mix = Math.max(want.mix, 0.6 + 0.4 * charged);
        want.size += 0.22 * charged * charged;
        angle += dt * 3 * charged; // spins up
      }
      if (boomAt) { // the explosion: swell, collapse to a point, build anew
        var e = (now - boomAt) / 1000;
        if (e < 0.18) want.size *= 1 + 1.8 * e / 0.18;
        else if (e < 0.4) want.size *= 0.05;
        else if (e < 2) want.size *= 0.05 + 0.95 * (e - 0.4) / 1.6;
        else boomAt = 0;
      }
      if (now < wildUntil) { // every colour, fast
        var hue = (t * 1.5) % 1;
        want.tint = [0.5 + 0.5 * Math.cos(6.283 * hue), 0.5 + 0.5 * Math.cos(6.283 * (hue - 0.33)), 0.5 + 0.5 * Math.cos(6.283 * (hue - 0.67))];
        want.mix = 1;
      }
      var k = Math.min(1, dt * 4);
      mix3(tint, tint, want.tint, k);
      tintMix += (want.mix - tintMix) * k;
      scale += (want.size - scale) * Math.min(1, dt * 16);
      // canvas.paused (assistant.js: the avatar hidden in the console): nothing drawn, the drawing
      // buffer shrunk to free its memory. The console with a hidden second sphere took 237 MB of
      // graphics memory on the stick (1.7 GB): it swapped, the hologram froze every ~10 s.
      if (canvas.paused) {
        if (canvas.width > 1) { canvas.width = 1; canvas.height = 1; }
        raf = requestAnimationFrame(frame);
        return;
      }
      // the size on the screen (the HUD is scaled to fit), not the layout size
      var rect = canvas.getBoundingClientRect(), dpr = window.devicePixels ? devicePixels() : window.devicePixelRatio || 1; // the app's pixels: twice as many on the 4K TVs cost 166 MB
      var w = Math.max(1, Math.round(rect.width * dpr)), h = Math.max(1, Math.round(rect.height * dpr));
      if (w < 2 || h < 2) { raf = requestAnimationFrame(frame); return; } // hidden
      if (canvas.width !== w || canvas.height !== h) { canvas.width = w; canvas.height = h; }
      gl.viewport(0, 0, w, h);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      for (var s = 0; s < 6; s++) rot(mats, s * 9, angle * SPIN[s] * 1.4 + userAngle, TILT[s] + 0.1 * Math.sin(t * 0.3 + s) + tiltOff);
      use(lp, LINE_ATTRS);
      set(lu, mats, now, w);
      gl.drawArrays(gl.TRIANGLES, 0, GEO.lines);
      use(pp, POINT_ATTRS);
      set(pu, mats, now, w);
      gl.drawArrays(gl.POINTS, 0, GEO.points);
      if (!logged) {
        logged = true;
        console.log("3D orb " + w + "x" + h + ": " + GEO.lines / 6 + " segments, " + GEO.points + " points, gl error " + gl.getError());
      }
      raf = requestAnimationFrame(frame);
    }
    function stop() {
      if (!alive) return;
      alive = false;
      cancelAnimationFrame(raf);
      var lose = gl.getExtension("WEBGL_lose_context");
      if (lose) lose.loseContext();
    }
    raf = requestAnimationFrame(frame);
    return {
      setState: function (s) { name = STATES[s] ? s : "idle"; target = STATES[name]; },
      stop: stop,
      // drag: the finger moved by dx, dy (radians) in dt seconds; its speed stays as the spin
      drag: function (dx, dy, dt) {
        userAngle += dx;
        tiltOff = Math.max(-0.7, Math.min(0.7, tiltOff + dy));
        if (dt > 0) userVel = userVel * 0.5 + 0.5 * dx / dt;
      },
      excite: function (v) { boostTarget = v; },     // touched: glow (0..1)
      pulse: function () { kick = 1; },              // a double tap: one strong pulse
      wild: function () { wildUntil = performance.now() + 2500; userVel += 25; }, // the easter egg
      charge: function (v) { charged = Math.max(0, Math.min(1, v)); },          // filled with plasma
      explode: function () { boomAt = performance.now(); charged = 0; kick = 1.5; userVel += 18; }
    };
  }

  function setLevel(v) {
    level = Math.max(0, Math.min(1, v));
    levelAt = performance.now();
  }

  return { start: start, setLevel: setLevel };
})();
