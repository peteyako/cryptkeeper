// Chart readout: pressing on a chart shows the time and value of the point under the
// pointer, following it while the button (or finger) stays down and gone on release;
// moving over a chart without pressing shows nothing. Each .chart-wrap[data-chart] carries
// its points as JSON ({t, v, y}: time
// text, value text, and the line's height there as a fraction of the plot), spread evenly
// along the plot, so the pointer's x picks the point. Listening on the document means
// charts that Datastar patches in later work without any wiring.
(() => {
  const cache = new WeakMap();

  function pointsOf(wrap) {
    const raw = wrap.dataset.chart;
    let entry = cache.get(wrap);
    if (!entry || entry.raw !== raw) {
      let pts = [];
      try {
        pts = JSON.parse(raw);
      } catch {
        pts = [];
      }
      entry = { raw, pts };
      cache.set(wrap, entry);
    }
    return entry.pts;
  }

  let shown = null; // the .chart__hover currently visible

  function hide() {
    if (shown) {
      shown.hidden = true;
      shown = null;
    }
  }

  function show(wrap, clientX) {
    const plot = wrap.querySelector(".chart__plot");
    const hover = wrap.querySelector(".chart__hover");
    const pts = pointsOf(wrap);
    if (!plot || !hover || pts.length < 2) {
      hide();
      return;
    }
    const r = plot.getBoundingClientRect();
    if (r.width <= 0 || clientX < r.left || clientX > r.right) {
      hide();
      return;
    }
    const n = pts.length;
    const i = Math.round(((clientX - r.left) / r.width) * (n - 1));
    const p = pts[Math.min(Math.max(i, 0), n - 1)];
    const x = (i / (n - 1)) * r.width;
    if (shown && shown !== hover) {
      shown.hidden = true;
    }
    hover.hidden = false;
    shown = hover;
    const cross = hover.querySelector(".chart__cross");
    const dot = hover.querySelector(".chart__dot");
    if (cross) cross.style.left = x.toFixed(1) + "px";
    if (dot) {
      dot.style.left = x.toFixed(1) + "px";
      dot.style.top = (p.y * r.height).toFixed(1) + "px";
    }
    const t = hover.querySelector(".chart__tip-t");
    const v = hover.querySelector(".chart__tip .fig-v") || hover.querySelector(".chart__tip-v");
    if (t) t.textContent = p.t;
    if (v) v.textContent = p.v;
    // Keep the tip inside the plot: centred on the point, clamped to the edges.
    const tip = hover.querySelector(".chart__tip");
    if (tip) {
      const w = tip.offsetWidth;
      const left = Math.min(Math.max(x - w / 2, 0), Math.max(r.width - w, 0));
      tip.style.left = left.toFixed(1) + "px";
    }
  }

  function onMove(e) {
    // Only while the primary button or a finger is down; the right button opens the
    // context menu.
    const pressed = (e.buttons & 1) === 1;
    const wrap = pressed && e.target instanceof Element ? e.target.closest(".chart-wrap[data-chart]") : null;
    if (!wrap) {
      hide();
      return;
    }
    show(wrap, e.clientX);
  }

  document.addEventListener("pointermove", onMove, { passive: true });
  document.addEventListener("pointerdown", onMove, { passive: true });
  document.addEventListener("pointerup", hide, { passive: true });
  document.addEventListener("pointercancel", hide, { passive: true });
  document.documentElement.addEventListener("pointerleave", hide, { passive: true });
})();
