// Chorus dashboard interactive sliding timeline.
//
// app.js owns all swarm data and calls ChorusTimeline.render(rootEl, state,
// actions) whenever state changes. This module draws one SVG from that state
// and reports what the user picks through actions; it keeps no copy of swarm
// data between renders except per-root viewport interaction state keyed by the
// swarm name.
(function () {
  "use strict";

  const SVG_NS = "http://www.w3.org/2000/svg";
  // Idle gaps longer than 10 minutes are compressed into a fixed-width quiet break.
  const QUIET_MS = 10 * 60 * 1000;
  // Axis length of one quiet break, equivalent to 2.5 minutes of active time.
  const QUIET_UNITS = QUIET_MS / 4;
  // Default visible window when following live (30 minutes).
  const LIVE_WINDOW_MS = 30 * 60 * 1000;
  // Mentions are recognized using the server's handle boundary rule.
  const MENTION_RE = /(^|[^A-Za-z0-9_-])@([A-Za-z0-9][A-Za-z0-9_-]{0,31})(?![A-Za-z0-9_-])/g;
  // Layout constants in px — sized for comfortable human readability and zero label/dot overlap.
  const OVERVIEW_H = 36;
  const AXIS_H = 26;
  const LANE_H = 38;
  const LABEL_W = 164;
  const PAD = 12;
  const PLOT_PAD_X = 16;

  // Per-root viewport memory: tracks active swarm, lastLive window, frozen
  // window when paused, and any active pointer drag session.
  const viewports = new WeakMap();

  /**
   * render draws the timeline into rootEl.
   */
  function render(rootEl, state, actions) {
    const view = state && state.view;
    const hadFocus = rootEl.contains(document.activeElement);
    clear(rootEl);
    if (!view) {
      rootEl.appendChild(note("Connecting to swarm…"));
      return;
    }
    // Record which swarm this view belongs to before early returns so switching
    // through an empty swarm resets any paused interval from the previous swarm.
    const mem = swarmMemory(rootEl, view.swarm);
    const events = collectEvents(view);
    if (events.length === 0) {
      rootEl.appendChild(note("No messages yet. Activity will appear here as agents collaborate."));
      return;
    }

    const width = Math.max(480, rootEl.clientWidth || 1040);
    const axis = buildAxis(events);
    const win = viewport(mem, state, axis);
    const lanes = laneList(view, events, win, state.selectedMessage);
    const colors = colorMap(lanes);
    const stripW = Math.max(120, width - LABEL_W - PAD * 2);
    const plotX0 = LABEL_W + PAD + PLOT_PAD_X;
    const plotW = Math.max(80, stripW - PLOT_PAD_X * 2);
    const noticeH = state.followLive ? 0 : 22;
    const height = OVERVIEW_H + AXIS_H + lanes.length * LANE_H + PAD * 3 + noticeH;

    const svg = el("svg", {
      class: "tl",
      width: String(width),
      height: String(height),
      viewBox: `0 0 ${width} ${height}`,
      role: "application",
      tabindex: "0",
      "aria-label": `Swarm activity timeline, ${events.length} messages. Drag the top window to scrub live, or use Left and Right arrows to step through messages.`,
    });

    // Clip lane labels strictly inside the left gutter so long handles never
    // bleed across the divider line or overlap message dots.
    const defs = el("defs", {});
    const clip = el("clipPath", { id: "tl-label-clip" });
    clip.appendChild(el("rect", { x: "0", y: "0", width: String(LABEL_W - 10), height: String(height) }));
    defs.appendChild(clip);
    svg.appendChild(defs);

    const ox = (t) => LABEL_W + PAD + (axis.pos(t) / axis.total) * stripW;
    const w0 = axis.pos(win.startMs);
    const w1 = Math.max(w0 + 1, axis.pos(win.endMs));
    const mx = (t) => plotX0 + ((axis.pos(t) - w0) / (w1 - w0)) * plotW;
    const inWindow = (t) => t >= win.startMs && t <= win.endMs;

    const match = matcher(state);
    const sel = state.selectedTarget || null;
    const picked = state.selectedMessage || null;
    const isSelected = (e) => picked != null && picked.target === e.target && picked.seq === e.seq;

    const pan = (next) => panTo(rootEl, state, actions, next);
    drawOverview(rootEl, svg, events, axis, ox, win, colors, stripW, state, actions, pan);

    const top = OVERVIEW_H + PAD;
    const laneTop = top + AXIS_H;
    const channel = sel && sel.kind === "channel"
      ? (view.channels || []).find((c) => c.name === sel.name)
      : null;

    drawAxis(svg, axis, w0, w1, mx, top, stripW, height);

    const laneY = new Map();
    lanes.forEach((lane, i) => {
      const y = laneTop + i * LANE_H + LANE_H / 2;
      laneY.set(lane.handle, y);
      drawLane(svg, lane, y, width, colors);
    });

    if (channel) {
      drawPin(svg, channel, mx, inWindow, top, laneTop, lanes.length, stripW);
      drawReplies(svg, channel, mx, inWindow, laneY, colors);
      drawReadPositions(svg, channel, mx, inWindow, laneY);
    }

    const order = events.filter((e) => laneY.has(e.sender));
    for (const e of order) {
      if (!inWindow(e.t)) continue;
      const y = laneY.get(e.sender);
      if (y == null) continue;
      const isSel = isSelected(e);
      const dim = match && !match(e);
      const dot = el("circle", {
        class: "tl-dot" + (isSel ? " is-selected" : "") + (dim ? " is-dim" : "") + (sel && sameTarget(sel, e) ? " is-target" : ""),
        cx: mx(e.t).toFixed(1),
        cy: String(y),
        r: isSel ? "8" : "6",
        fill: colors.get(e.sender),
      });
      const title = el("title", {});
      title.textContent = `${e.target}#${e.seq} · @${e.sender} · ${clock(e.t)}\n${firstLine(e.text, 140)}`;
      dot.appendChild(title);
      dot.addEventListener("click", () => openEvent(e, actions));
      svg.appendChild(dot);
    }

    const chosen = order.find(isSelected);
    if (chosen && inWindow(chosen.t)) drawMentions(svg, chosen, view, mx, laneY, colors);

    if (!state.followLive) {
      const back = el("text", { class: "tl-review", x: String(width - PAD), y: String(height - 6), "text-anchor": "end" });
      back.textContent = "Viewing history window · press F or click Live in header to return to latest";
      svg.appendChild(back);
    }

    svg.addEventListener("keydown", (ev) => onKey(ev, order, isSelected, actions, state, axis, win, pan));
    rootEl.appendChild(svg);
    if (hadFocus) svg.focus();
  }

  function collectEvents(view) {
    const out = [];
    for (const ch of view.channels || []) {
      for (const m of ch.messages || []) out.push(toEvent(ch.name, m));
    }
    for (const ib of view.inboxes || []) {
      for (const m of ib.messages || []) out.push(toEvent("@" + ib.recipient, m));
    }
    out.sort((a, b) => a.t - b.t || a.seq - b.seq);
    return out;
  }

  function toEvent(target, m) {
    const e = { target, seq: m.seq, sender: m.sender, text: m.text, t: m.sent_at_ms, replyTo: m.reply_to };
    if (target.startsWith("@")) {
      const [a, b] = [m.sender, target.slice(1)].sort();
      e.pair = { a, b, pairKey: a + "↔" + b };
    }
    return e;
  }

  function sameTarget(sel, e) {
    if (sel.kind === "all") return true;
    return sel.kind === "channel" ? e.target === sel.name : !!e.pair && e.pair.pairKey === sel.pairKey;
  }

  function openEvent(e, actions) {
    if (e.pair) {
      actions.selectTarget({ kind: "dm", pairKey: e.pair.pairKey, a: e.pair.a, b: e.pair.b });
    } else {
      actions.selectTarget({ kind: "channel", name: e.target });
    }
    actions.selectMessage(e.target, e.seq);
  }

  // laneList returns one lane per currently active agent plus any historical
  // sender that actually sent a message within the visible time window win (or
  // owns the currently selected message). This prevents departed handles from
  // hours ago from cluttering the current window with empty lanes.
  function laneList(view, events, win, selectedMessage) {
    const live = new Map((view.agents || []).map((a) => [a.handle, a]));
    const handles = new Set(live.keys());
    for (const e of events) {
      if ((e.t >= win.startMs && e.t <= win.endMs) ||
          (selectedMessage && selectedMessage.target === e.target && selectedMessage.seq === e.seq)) {
        handles.add(e.sender);
      }
    }
    if (handles.size === 0) {
      for (const e of events) handles.add(e.sender);
    }
    const lanes = [...handles].map((h) => {
      const a = live.get(h);
      return {
        handle: h,
        human: !!(a && a.role === "human"),
        offline: !a,
        watching: !!(a && a.watching_on_this_server),
      };
    });
    lanes.sort((a, b) => (b.human - a.human) || (a.offline - b.offline) || a.handle.localeCompare(b.handle));
    return lanes;
  }

  function colorMap(lanes) {
    return new Map(lanes.map((l) => [l.handle, window.ChorusUI.agentColorHex(l.handle, l.human ? "human" : "")]));
  }

  function buildAxis(events) {
    const first = events[0].t;
    const last = events[events.length - 1].t;
    const gaps = [];
    for (let i = 1; i < events.length; i++) {
      const a = events[i - 1].t;
      const b = events[i].t;
      if (b - a > QUIET_MS) gaps.push({ from: a, to: b });
    }
    function pos(t) {
      let p = t - first;
      for (const g of gaps) {
        if (t <= g.from) break;
        const covered = Math.min(t, g.to) - g.from;
        p -= covered;
        p += QUIET_UNITS * (covered / (g.to - g.from));
      }
      return Math.max(0, p);
    }
    const total = Math.max(1, pos(last) + 60 * 1000);
    return { first, last, gaps, pos, total };
  }

  function liveWindow(axis) {
    const endMs = axis.last + 60 * 1000;
    return { startMs: Math.max(axis.first, endMs - LIVE_WINDOW_MS), endMs };
  }

  function swarmMemory(rootEl, swarm) {
    let mem = viewports.get(rootEl);
    if (!mem || mem.swarm !== swarm) {
      mem = { swarm, lastLive: null, frozen: null };
      viewports.set(rootEl, mem);
    }
    return mem;
  }

  function viewport(mem, state, axis) {
    if (state.timeWindow) return state.timeWindow;
    if (state.followLive) {
      mem.frozen = null;
      mem.lastLive = liveWindow(axis);
      return mem.lastLive;
    }
    if (!mem.frozen) mem.frozen = mem.lastLive || liveWindow(axis);
    return mem.frozen;
  }

  function panTo(rootEl, state, actions, next) {
    if (state.timeWindow) {
      actions.setTimeWindow(next);
      return;
    }
    viewports.get(rootEl).frozen = next;
    if (state.followLive) actions.setFollowLive(false);
    else render(rootEl, state, actions);
  }

  function shiftWindow(axis, win, delta) {
    const w0 = axis.pos(win.startMs);
    const w1 = axis.pos(win.endMs);
    const len = w1 - w0;
    const start = Math.min(Math.max(0, w0 + delta), Math.max(0, axis.total - len));
    return { startMs: timeAt(axis, start), endMs: timeAt(axis, start + len) };
  }

  function centerWindow(axis, win, t) {
    const len = axis.pos(win.endMs) - axis.pos(win.startMs);
    return shiftWindow(axis, win, axis.pos(t) - len / 2 - axis.pos(win.startMs));
  }

  function matcher(state) {
    const q = (state.searchQuery || "").trim().toLowerCase();
    const agent = state.filterAgent || "";
    const art = state.filterArtifact || "";
    if (!q && !agent && !art) return null;
    return (e) =>
      (!q || e.text.toLowerCase().includes(q) || e.sender.toLowerCase().includes(q)) &&
      (!agent || e.sender === agent) &&
      (!art || e.text.includes(art));
  }

  function drawOverview(rootEl, svg, events, axis, ox, win, colors, stripW, state, actions, pan) {
    const y0 = PAD;
    const stripH = OVERVIEW_H - 10;
    const strip = el("rect", {
      class: "tl-overview",
      x: String(LABEL_W + PAD),
      y: String(y0),
      width: String(stripW),
      height: String(stripH),
      rx: "6",
    });
    strip.addEventListener("click", (ev) => {
      const box = svg.getBoundingClientRect();
      const frac = (ev.clientX - box.left - LABEL_W - PAD) / stripW;
      const next = centerWindow(axis, win, timeAt(axis, Math.min(1, Math.max(0, frac)) * axis.total));
      actions.setTimeWindow(next);
    });
    svg.appendChild(strip);

    for (const e of events) {
      svg.appendChild(el("line", {
        class: "tl-tick",
        x1: ox(e.t).toFixed(1),
        x2: ox(e.t).toFixed(1),
        y1: String(y0 + 4),
        y2: String(y0 + stripH - 4),
        stroke: colors.get(e.sender) || "#3B82F6",
      }));
    }

    const label = el("text", { class: "tl-label", x: String(PAD), y: String(y0 + stripH / 2 + 4) });
    label.textContent = "Scrub history";
    svg.appendChild(label);

    const x0 = ox(win.startMs);
    const x1 = Math.max(x0 + 14, ox(win.endMs));
    const brush = el("rect", {
      class: "tl-window",
      x: x0.toFixed(1),
      y: String(y0),
      width: Math.min(stripW, x1 - x0).toFixed(1),
      height: String(stripH),
      rx: "6",
    });
    const t = el("title", {});
    t.textContent = "Drag left or right to scrub the timeline and messages live";
    brush.appendChild(t);
    brush.addEventListener("pointerdown", (ev) =>
      startLiveDrag(ev, rootEl, brush, axis, stripW, state, actions, pan)
    );
    svg.appendChild(brush);
  }

  // startLiveDrag captures the pointer on rootEl (which persists across SVG
  // re-renders) and updates actions.setTimeWindow on every animation frame as
  // the user drags, so both the timeline lanes and the message stream update
  // live during the drag without waiting for pointer release.
  function startLiveDrag(ev, rootEl, brush, axis, stripW, state, actions, pan) {
    ev.preventDefault();
    ev.stopPropagation();
    const pointerId = ev.pointerId;
    if (typeof rootEl.setPointerCapture === "function") {
      try {
        rootEl.setPointerCapture(pointerId);
      } catch {
        // Fallback when synthetic events omit an active pointerId
      }
    }
    const startX = ev.clientX;
    const baseX = Number(brush.getAttribute("x"));
    const w = Number(brush.getAttribute("width"));
    const minX = LABEL_W + PAD;
    const maxX = Math.max(minX, LABEL_W + PAD + stripW - w);
    const initialWindow = state.timeWindow ? { ...state.timeWindow } : null;
    const initialFollowLive = state.followLive;

    let currentX = baseX;
    let rafId = 0;

    const applyPosition = (xPos) => {
      const frac0 = Math.min(1, Math.max(0, (xPos - minX) / stripW));
      const frac1 = Math.min(1, Math.max(frac0, (xPos + w - minX) / stripW));
      const nextRange = {
        startMs: timeAt(axis, frac0 * axis.total),
        endMs: timeAt(axis, frac1 * axis.total),
      };
      actions.setTimeWindow(nextRange);
    };

    const move = (e) => {
      currentX = Math.min(maxX, Math.max(minX, baseX + (e.clientX - startX)));
      const liveBrush = rootEl.querySelector(".tl-window");
      if (liveBrush) liveBrush.setAttribute("x", currentX.toFixed(1));
      if (!rafId) {
        rafId = window.requestAnimationFrame(() => {
          rafId = 0;
          applyPosition(currentX);
        });
      }
    };

    const stop = () => {
      if (rafId) {
        window.cancelAnimationFrame(rafId);
        rafId = 0;
      }
      rootEl.removeEventListener("pointermove", move);
      rootEl.removeEventListener("pointerup", up);
      rootEl.removeEventListener("pointercancel", cancel);
      if (typeof rootEl.releasePointerCapture === "function") {
        try {
          rootEl.releasePointerCapture(pointerId);
        } catch {
          // Ignore if already released
        }
      }
    };

    const cancel = () => {
      stop();
      if (initialWindow) {
        actions.setTimeWindow(initialWindow);
      } else {
        actions.setFollowLive(initialFollowLive);
      }
    };

    const up = () => {
      stop();
      applyPosition(currentX);
    };

    rootEl.addEventListener("pointermove", move);
    rootEl.addEventListener("pointerup", up);
    rootEl.addEventListener("pointercancel", cancel);
  }

  function timeAt(axis, p) {
    let lo = axis.first;
    let hi = axis.last + 60 * 1000;
    for (let i = 0; i < 40; i++) {
      const mid = (lo + hi) / 2;
      if (axis.pos(mid) < p) lo = mid;
      else hi = mid;
    }
    return Math.round(hi);
  }

  function drawAxis(svg, axis, w0, w1, mx, top, stripW, height) {
    const y = top + AXIS_H - 7;
    for (const g of axis.gaps) {
      if (axis.pos(g.to) < w0 || axis.pos(g.from) > w1) continue;
      const xa = Math.max(LABEL_W + PAD, mx(g.from));
      const xb = Math.min(LABEL_W + PAD + stripW, mx(g.to));
      if (xb <= xa) continue;
      svg.appendChild(el("rect", {
        class: "tl-quiet",
        x: xa.toFixed(1),
        y: String(top),
        width: Math.max(2, xb - xa).toFixed(1),
        height: String(height - top - PAD),
      }));
      if (xb - xa >= 48) {
        const lbl = el("text", {
          class: "tl-quiet-label",
          x: ((xa + xb) / 2).toFixed(1),
          y: String(y),
          "text-anchor": "middle",
        });
        lbl.textContent = quietLabel(g.to - g.from);
        svg.appendChild(lbl);
      }
    }
    const left = el("text", { class: "tl-axis", x: String(LABEL_W + PAD), y: String(y) });
    left.textContent = clock(timeAt(axis, w0));
    const right = el("text", { class: "tl-axis", x: String(LABEL_W + PAD + stripW), y: String(y), "text-anchor": "end" });
    right.textContent = clock(timeAt(axis, w1));
    svg.appendChild(left);
    svg.appendChild(right);
  }

  function drawLane(svg, lane, y, width, colors) {
    svg.appendChild(el("line", {
      class: "tl-lane" + (lane.human ? " is-human" : "") + (lane.offline ? " is-left" : ""),
      x1: String(LABEL_W + PAD),
      x2: String(width - PAD),
      y1: String(y),
      y2: String(y),
    }));
    const labelGroup = el("g", { "clip-path": "url(#tl-label-clip)" });
    const dot = el("circle", {
      cx: String(PAD + 5),
      cy: String(y),
      r: "4.5",
      fill: colors.get(lane.handle),
      class: lane.offline ? "tl-lane-dot is-left" : "tl-lane-dot",
    });
    const label = el("text", {
      class: "tl-lane-label" + (lane.offline ? " is-left" : ""),
      x: String(PAD + 15),
      y: String(y + 4.5),
      fill: colors.get(lane.handle),
    });
    label.textContent = "@" + lane.handle;
    const tip = el("title", {});
    tip.textContent = "@" + lane.handle +
      (lane.human ? " (human)" : lane.offline ? " (earlier message sender)" : lane.watching ? " (watching)" : "");
    labelGroup.appendChild(dot);
    labelGroup.appendChild(label);
    labelGroup.appendChild(tip);
    svg.appendChild(labelGroup);
  }

  // drawPin places the current channel pin landmark and clamps its label away
  // from the left and right clock timestamps so text never overlaps.
  function drawPin(svg, channel, mx, inWindow, axisTop, laneTop, laneCount, stripW) {
    if (!channel.pin) return;
    const msg = (channel.messages || []).find((m) => m.seq === channel.pin.seq);
    if (!msg || !inWindow(msg.sent_at_ms)) return;
    const rawX = mx(msg.sent_at_ms);
    const x = rawX.toFixed(1);
    svg.appendChild(el("line", {
      class: "tl-pin",
      x1: x,
      x2: x,
      y1: String(laneTop - 4),
      y2: String(laneTop + laneCount * LANE_H),
    }));
    const minLabelX = LABEL_W + PAD + 68;
    const maxLabelX = LABEL_W + PAD + stripW - 68;
    if (maxLabelX > minLabelX) {
      const clampedX = Math.min(maxLabelX, Math.max(minLabelX, rawX));
      const lbl = el("text", {
        class: "tl-pin-label",
        x: clampedX.toFixed(1),
        y: String(axisTop + AXIS_H - 7),
        "text-anchor": "middle",
      });
      lbl.textContent = `Pin #${channel.pin.seq}`;
      svg.appendChild(lbl);
    }
  }

  function drawReplies(svg, channel, mx, inWindow, laneY, colors) {
    const bySeq = new Map((channel.messages || []).map((m) => [m.seq, m]));
    for (const m of channel.messages || []) {
      if (!m.reply_to) continue;
      const parent = bySeq.get(m.reply_to);
      if (!parent || !inWindow(m.sent_at_ms) || !inWindow(parent.sent_at_ms)) continue;
      const ya = laneY.get(parent.sender);
      const yb = laneY.get(m.sender);
      if (ya == null || yb == null) continue;
      const xa = mx(parent.sent_at_ms);
      const xb = mx(m.sent_at_ms);
      const bend = Math.min(36, Math.abs(xb - xa) / 2 + 10);
      svg.appendChild(el("path", {
        class: "tl-reply",
        d: `M${xa.toFixed(1)},${ya} C${xa.toFixed(1)},${(ya - bend).toFixed(1)} ${xb.toFixed(1)},${(yb - bend).toFixed(1)} ${xb.toFixed(1)},${yb}`,
        stroke: colors.get(m.sender),
      }));
    }
  }

  // drawReadPositions marks each channel member's read position when inside the
  // visible window using a subtle vertical tick bar with a hover tooltip (never
  // inline text that overlaps message dots).
  function drawReadPositions(svg, channel, mx, inWindow, laneY) {
    const bySeq = new Map((channel.messages || []).map((m) => [m.seq, m]));
    for (const p of channel.positions || []) {
      const y = laneY.get(p.handle);
      if (y == null || p.read_seq === 0) continue;
      const m = bySeq.get(p.read_seq);
      if (!m || !inWindow(m.sent_at_ms)) continue;
      const summary = p.behind > 0
        ? `@${p.handle} last read #${p.read_seq} · ${p.behind} newer${p.mentioned ? " (@mentioned)" : ""}`
        : `@${p.handle} caught up through #${p.read_seq}`;
      const x = mx(m.sent_at_ms);
      const mark = el("rect", {
        class: "tl-read" + (p.behind > 0 ? " is-behind" : ""),
        x: (x + 7).toFixed(1),
        y: String(y - 11),
        width: "3.5",
        height: "22",
        rx: "1.75",
      });
      const title = el("title", {});
      title.textContent = summary;
      mark.appendChild(title);
      svg.appendChild(mark);
    }
  }

  function drawMentions(svg, e, view, mx, laneY, colors) {
    const x = mx(e.t).toFixed(1);
    const y0 = laneY.get(e.sender);
    const mentioned = new Set(mentionedHandles(e.text));
    const broadcast = mentioned.delete("all");
    const channel = e.pair ? null : (view.channels || []).find((c) => c.name === e.target);
    const members = new Set(broadcast && channel ? (channel.positions || []).map((p) => p.handle) : []);
    for (const [handle, y1] of laneY) {
      if (handle === e.sender || !(mentioned.has(handle.toLowerCase()) || members.has(handle))) continue;
      svg.appendChild(el("line", { class: "tl-mention", x1: x, x2: x, y1: String(y0), y2: String(y1), stroke: colors.get(e.sender) }));
      svg.appendChild(el("circle", { class: "tl-mention-end", cx: x, cy: String(y1), r: "4", stroke: colors.get(e.sender) }));
    }
  }

  function mentionedHandles(text) {
    const out = [];
    for (const m of String(text || "").matchAll(MENTION_RE)) {
      const h = m[2].toLowerCase();
      if (!out.includes(h)) out.push(h);
    }
    return out;
  }

  function onKey(ev, order, isSelected, actions, state, axis, win, pan) {
    if (ev.key === "f" || ev.key === "F") {
      ev.preventDefault();
      actions.setFollowLive(!state.followLive);
      return;
    }
    const len = axis.pos(win.endMs) - axis.pos(win.startMs);
    if (ev.key === "PageUp" || ev.key === "PageDown") {
      ev.preventDefault();
      pan(shiftWindow(axis, win, ev.key === "PageUp" ? -len : len));
      return;
    }
    if (order.length === 0) return;
    let i = order.findIndex(isSelected);
    if (ev.key === "ArrowRight") i = Math.min(order.length - 1, i + 1);
    else if (ev.key === "ArrowLeft") i = i < 0 ? order.length - 1 : Math.max(0, i - 1);
    else if (ev.key === "Home") i = 0;
    else if (ev.key === "End") i = order.length - 1;
    else if (ev.key === "Enter" && i >= 0) {
      ev.preventDefault();
      openEvent(order[i], actions);
      return;
    } else return;
    ev.preventDefault();
    const e = order[i];
    if (e.t < win.startMs || e.t > win.endMs) pan(centerWindow(axis, win, e.t));
    openEvent(e, actions);
  }

  function quietLabel(ms) {
    const min = Math.round(ms / 60000);
    if (min < 60) return `${min}m quiet`;
    const h = Math.floor(min / 60);
    return `${h}h${min % 60 ? " " + (min % 60) + "m" : ""} quiet`;
  }

  function clock(ms) {
    return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  function firstLine(text, max) {
    const line = String(text || "").split("\n")[0];
    return line.length > max ? line.slice(0, max - 1) + "…" : line;
  }

  function el(name, attrs) {
    const node = document.createElementNS(SVG_NS, name);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
    return node;
  }

  function note(text) {
    const p = document.createElement("p");
    p.className = "tl-empty";
    p.textContent = text;
    return p;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  window.ChorusTimeline = { render };
})();
