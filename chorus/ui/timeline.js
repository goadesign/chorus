// Chorus dashboard timeline.
//
// app.js owns all swarm data and calls ChorusTimeline.render(rootEl, state,
// actions) whenever state changes. This module draws one SVG from that state
// and reports what the user picks through actions; it keeps no copy of swarm
// data between renders except layout caches keyed by the view's identity.
//
// What it draws, all from fields the server returns (GET /swarms/{s}/view):
//   - an overview strip of every retained message in the swarm, with a
//     draggable window that selects the time range shown below it;
//   - one lane per message sender (live agents and senders that have left),
//     with role "human" lanes styled apart;
//   - a time axis on which idle stretches longer than QUIET_MS are shrunk to
//     a fixed-width "quiet" break so bursts of activity stay readable;
//   - for the selected channel only: the current pin (Chorus stores only the
//     current pin, so there is one landmark, placed at the pinned message
//     when it is still retained), each member's read position, and
//     reply_to curves between messages of that channel;
//   - highlights for messages matching the search, agent, or artifact filter.
// Direct-message reply_to values are ambiguous across the two inboxes, so
// no reply curves are drawn for DMs.
//
// Keyboard: the timeline is one focusable control. Left/Right move the
// selection to the previous/next message in the window, Home/End jump to the
// first/last, Enter opens the selected message, and "f" toggles Follow live.
(function () {
  "use strict";

  const SVG_NS = "http://www.w3.org/2000/svg";
  // Idle gaps longer than this are drawn as a fixed-width quiet break.
  const QUIET_MS = 10 * 60 * 1000;
  // Axis length of one quiet break, in milliseconds of active time: a break
  // is drawn as wide as 2.5 minutes of activity, whatever its real length.
  const QUIET_UNITS = QUIET_MS / 4;
  // Default visible window when following live.
  const LIVE_WINDOW_MS = 30 * 60 * 1000;
  // Layout constants in px.
  const OVERVIEW_H = 28;
  const AXIS_H = 18;
  const LANE_H = 26;
  const LABEL_W = 112;
  const PAD = 8;

  // Agent colors follow the Chorus logo, assigned by sorted handle so a given
  // agent keeps its color across renders of the same swarm.
  const PALETTE = ["#F59E0B", "#14B8A6", "#3B82F6", "#F43F5E", "#8B5CF6", "#22C55E", "#EC4899", "#64748B"];

  // Per-root state that is about interaction only (focus, drag), never data.
  const ui = new WeakMap();

  /**
   * render draws the timeline into rootEl.
   * state fields read: view, selectedTarget, selectedSeq, timeWindow,
   * followLive, filterAgent, filterArtifact, searchQuery.
   * selectedTarget is {kind: "channel", name: "#x"} or {kind: "dm", key: "a|b"}.
   * actions used: selectTarget(selectedTarget), selectMessage(target, seq)
   * where target is "#channel" or the recipient inbox "@handle",
   * setTimeWindow({startMs, endMs}), setFollowLive(bool).
   */
  function render(rootEl, state, actions) {
    const view = state && state.view;
    clear(rootEl);
    if (!view) {
      rootEl.appendChild(note("Loading swarm…"));
      return;
    }
    const events = collectEvents(view);
    if (events.length === 0) {
      rootEl.appendChild(note("No messages yet. Agents' messages will appear here as they arrive."));
      return;
    }

    const width = Math.max(320, rootEl.clientWidth || 960);
    const lanes = laneList(view, events);
    const colors = colorMap(lanes);
    const axis = buildAxis(events);
    const win = currentWindow(state, axis);
    const plotW = width - LABEL_W - PAD * 2;
    const height = OVERVIEW_H + AXIS_H + lanes.length * LANE_H + PAD * 3;

    const svg = el("svg", {
      class: "tl",
      width: String(width),
      height: String(height),
      viewBox: `0 0 ${width} ${height}`,
      role: "application",
      tabindex: "0",
      "aria-label": `Swarm timeline, ${events.length} messages. Left and Right move between messages, Enter opens, F toggles Follow live.`,
    });

    // Scales: overview maps the whole compressed axis; the main plot maps the
    // selected window.
    const ox = (t) => LABEL_W + PAD + (axis.pos(t) / axis.total) * plotW;
    const w0 = axis.pos(win.startMs);
    const w1 = Math.max(w0 + 1, axis.pos(win.endMs));
    const mx = (t) => LABEL_W + PAD + ((axis.pos(t) - w0) / (w1 - w0)) * plotW;
    const inWindow = (t) => t >= win.startMs && t <= win.endMs;

    const match = matcher(state);
    const sel = state.selectedTarget || null;
    const isSelected = (e) => sel != null && state.selectedSeq === e.seq && sameTarget(sel, e);

    drawOverview(svg, events, axis, ox, win, colors, plotW, actions, state);
    const top = OVERVIEW_H + PAD;
    drawAxis(svg, axis, w0, w1, mx, top, plotW, height);
    const laneTop = top + AXIS_H;
    const laneY = new Map();
    lanes.forEach((lane, i) => {
      const y = laneTop + i * LANE_H + LANE_H / 2;
      laneY.set(lane.handle, y);
      drawLane(svg, lane, y, width, colors);
    });

    const channel = sel && sel.kind === "channel"
      ? (view.channels || []).find((c) => c.name === sel.name)
      : null;
    if (channel) {
      drawPin(svg, channel, mx, inWindow, laneTop, lanes.length);
      drawReplies(svg, channel, mx, inWindow, laneY, colors);
      drawReadPositions(svg, channel, mx, win, laneY);
    }

    const visible = events.filter((e) => inWindow(e.t));
    const order = [];
    for (const e of visible) {
      const y = laneY.get(e.sender);
      if (y == null) continue;
      const isSel = isSelected(e);
      const dim = match && !match(e);
      const dot = el("circle", {
        class: "tl-dot" + (isSel ? " is-selected" : "") + (dim ? " is-dim" : "") + (sel && sameTarget(sel, e) ? " is-target" : ""),
        cx: mx(e.t).toFixed(1),
        cy: String(y),
        r: isSel ? "6" : "4",
        fill: colors.get(e.sender),
      });
      const title = el("title", {});
      title.textContent = `${e.target} #${e.seq} · @${e.sender} · ${clock(e.t)} — ${firstLine(e.text, 120)}`;
      dot.appendChild(title);
      dot.addEventListener("click", () => openEvent(e, actions));
      svg.appendChild(dot);
      order.push(e);
    }

    if (!state.followLive) {
      const back = el("text", { class: "tl-review", x: String(width - PAD), y: String(top + 12), "text-anchor": "end" });
      back.textContent = "Reviewing earlier messages · press F to follow live";
      svg.appendChild(back);
    }

    svg.addEventListener("keydown", (ev) => onKey(ev, order, isSelected, actions, state));
    rootEl.appendChild(svg);
    const mem = ui.get(rootEl);
    if (mem && mem.focused) svg.focus();
    svg.addEventListener("focus", () => ui.set(rootEl, { focused: true }));
    svg.addEventListener("blur", () => ui.set(rootEl, { focused: false }));
  }

  // collectEvents flattens every retained channel message and DM into one
  // list sorted by stored time, tagging each with its target ("#chan" or
  // "@recipient").
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

  // toEvent tags a message with where it lives: a channel name, or for a
  // direct message the recipient's inbox ("@handle") plus the pair key
  // ("a|b", handles sorted) that app.js uses to select a DM conversation.
  function toEvent(target, m) {
    const e = { target, seq: m.seq, sender: m.sender, text: m.text, t: m.sent_at_ms, replyTo: m.reply_to };
    if (target.startsWith("@")) e.pairKey = [m.sender, target.slice(1)].sort().join("|");
    return e;
  }

  // sameTarget reports whether event e belongs to the selected channel or DM
  // conversation.
  function sameTarget(sel, e) {
    return sel.kind === "channel" ? e.target === sel.name : e.pairKey === sel.key;
  }

  // openEvent selects the event's channel or DM conversation, then the
  // message itself.
  function openEvent(e, actions) {
    if (e.pairKey) {
      actions.selectTarget({ kind: "dm", key: e.pairKey });
    } else {
      actions.selectTarget({ kind: "channel", name: e.target });
    }
    actions.selectMessage(e.target, e.seq);
  }

  // laneList returns one lane per sender seen in retained messages plus every
  // live agent, sorted with humans first and then by handle. Senders that are
  // not live agents are marked as left.
  function laneList(view, events) {
    const live = new Map((view.agents || []).map((a) => [a.handle, a]));
    const handles = new Set(events.map((e) => e.sender));
    for (const h of live.keys()) handles.add(h);
    const lanes = [...handles].map((h) => {
      const a = live.get(h);
      return { handle: h, human: !!(a && a.role === "human"), left: !a };
    });
    lanes.sort((a, b) => (b.human - a.human) || a.handle.localeCompare(b.handle));
    return lanes;
  }

  function colorMap(lanes) {
    const sorted = lanes.map((l) => l.handle).sort();
    return new Map(sorted.map((h, i) => [h, PALETTE[i % PALETTE.length]]));
  }

  // buildAxis maps real time to a compressed position: time inside active
  // stretches counts fully, and each gap longer than QUIET_MS counts as
  // QUIET_UNITS. pos(t) is monotonic, so ordering is kept.
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

  // currentWindow returns the visible range. Following live, it is the last
  // LIVE_WINDOW_MS ending at the newest message; otherwise the window app.js
  // stored after a drag or selection.
  function currentWindow(state, axis) {
    if (!state.followLive && state.timeWindow) return state.timeWindow;
    const endMs = axis.last + 60 * 1000;
    return { startMs: Math.max(axis.first, endMs - LIVE_WINDOW_MS), endMs };
  }

  // matcher returns a predicate for the active search, agent, and artifact
  // filters, or null when none is set.
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

  function drawOverview(svg, events, axis, ox, win, colors, plotW, actions, state) {
    const y0 = PAD;
    svg.appendChild(el("rect", { class: "tl-overview", x: String(LABEL_W + PAD), y: String(y0), width: String(plotW), height: String(OVERVIEW_H - 8), rx: "4" }));
    for (const e of events) {
      svg.appendChild(el("line", {
        class: "tl-tick",
        x1: ox(e.t).toFixed(1), x2: ox(e.t).toFixed(1),
        y1: String(y0 + 4), y2: String(y0 + OVERVIEW_H - 12),
        stroke: colors.get(e.sender),
      }));
    }
    const label = el("text", { class: "tl-label", x: String(PAD), y: String(y0 + 14) });
    label.textContent = "Whole history";
    svg.appendChild(label);

    // The window rectangle can be dragged; dropping it stops Follow live and
    // stores the new range through actions.setTimeWindow.
    const x0 = ox(win.startMs);
    const x1 = Math.max(x0 + 6, ox(win.endMs));
    const brush = el("rect", {
      class: "tl-window",
      x: x0.toFixed(1), y: String(y0), width: (x1 - x0).toFixed(1), height: String(OVERVIEW_H - 8), rx: "4",
    });
    const t = el("title", {});
    t.textContent = "Drag to look at an earlier part of the history";
    brush.appendChild(t);
    brush.addEventListener("pointerdown", (ev) => startDrag(ev, svg, brush, axis, ox, win, plotW, actions));
    svg.appendChild(brush);
  }

  function startDrag(ev, svg, brush, axis, ox, win, plotW, actions) {
    ev.preventDefault();
    brush.setPointerCapture(ev.pointerId);
    const startX = ev.clientX;
    const baseX = Number(brush.getAttribute("x"));
    const w = Number(brush.getAttribute("width"));
    const minX = LABEL_W + PAD;
    const maxX = LABEL_W + PAD + plotW - w;
    let x = baseX;
    const move = (e) => {
      x = Math.min(maxX, Math.max(minX, baseX + (e.clientX - startX)));
      brush.setAttribute("x", x.toFixed(1));
    };
    const up = () => {
      brush.removeEventListener("pointermove", move);
      brush.removeEventListener("pointerup", up);
      // Convert the dragged pixel offset back to a time range of the same
      // compressed length.
      const frac0 = (x - minX) / plotW;
      const frac1 = (x + w - minX) / plotW;
      actions.setFollowLive(false);
      actions.setTimeWindow({ startMs: timeAt(axis, frac0 * axis.total), endMs: timeAt(axis, frac1 * axis.total) });
    };
    brush.addEventListener("pointermove", move);
    brush.addEventListener("pointerup", up);
  }

  // timeAt inverts axis.pos by binary search over real time.
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

  function drawAxis(svg, axis, w0, w1, mx, top, plotW, height) {
    const y = top + AXIS_H - 6;
    for (const g of axis.gaps) {
      if (axis.pos(g.to) < w0 || axis.pos(g.from) > w1) continue;
      const xa = mx(g.from);
      const xb = mx(g.to);
      svg.appendChild(el("rect", { class: "tl-quiet", x: xa.toFixed(1), y: String(top), width: Math.max(2, xb - xa).toFixed(1), height: String(height - top - PAD) }));
      const lbl = el("text", { class: "tl-quiet-label", x: ((xa + xb) / 2).toFixed(1), y: String(y), "text-anchor": "middle" });
      lbl.textContent = quietLabel(g.to - g.from);
      svg.appendChild(lbl);
    }
    // Clock labels at both window edges.
    const left = el("text", { class: "tl-axis", x: String(LABEL_W + PAD), y: String(y) });
    left.textContent = clock(timeAt(axis, w0));
    const right = el("text", { class: "tl-axis", x: String(LABEL_W + PAD + plotW), y: String(y), "text-anchor": "end" });
    right.textContent = clock(timeAt(axis, w1));
    svg.appendChild(left);
    svg.appendChild(right);
  }

  function drawLane(svg, lane, y, width, colors) {
    svg.appendChild(el("line", {
      class: "tl-lane" + (lane.human ? " is-human" : "") + (lane.left ? " is-left" : ""),
      x1: String(LABEL_W), x2: String(width - PAD), y1: String(y), y2: String(y),
    }));
    const label = el("text", { class: "tl-lane-label" + (lane.left ? " is-left" : ""), x: String(PAD), y: String(y + 4) });
    label.textContent = "@" + lane.handle + (lane.human ? " · human" : "") + (lane.left ? " · left" : "");
    label.setAttribute("fill", colors.get(lane.handle));
    svg.appendChild(label);
  }

  // drawPin places the single current-pin landmark at the pinned message's
  // stored time when that message is still retained in the channel.
  function drawPin(svg, channel, mx, inWindow, laneTop, laneCount) {
    if (!channel.pin) return;
    const msg = (channel.messages || []).find((m) => m.seq === channel.pin.seq);
    if (!msg || !inWindow(msg.sent_at_ms)) return;
    const x = mx(msg.sent_at_ms).toFixed(1);
    svg.appendChild(el("line", { class: "tl-pin", x1: x, x2: x, y1: String(laneTop - 4), y2: String(laneTop + laneCount * LANE_H) }));
    const lbl = el("text", { class: "tl-pin-label", x: x, y: String(laneTop - 6), "text-anchor": "middle" });
    lbl.textContent = `Pin #${channel.pin.seq}`;
    svg.appendChild(lbl);
  }

  // drawReplies draws a curve from each replied-to message to its reply,
  // using channel reply_to only (both ends in the same channel).
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
      const bend = Math.min(40, Math.abs(xb - xa) / 2 + 10);
      svg.appendChild(el("path", {
        class: "tl-reply",
        d: `M${xa.toFixed(1)},${ya} C${xa.toFixed(1)},${(ya - bend).toFixed(1)} ${xb.toFixed(1)},${(yb - bend).toFixed(1)} ${xb.toFixed(1)},${yb}`,
        stroke: colors.get(m.sender),
      }));
    }
  }

  // drawReadPositions marks, on each member's lane, the stored time of the
  // last message that member has read, sent, or joined at in this channel.
  function drawReadPositions(svg, channel, mx, win, laneY) {
    const bySeq = new Map((channel.messages || []).map((m) => [m.seq, m]));
    for (const p of channel.positions || []) {
      const y = laneY.get(p.handle);
      if (y == null) continue;
      const m = bySeq.get(p.read_seq);
      const t = m ? m.sent_at_ms : win.startMs;
      const x = mx(Math.min(win.endMs, Math.max(win.startMs, t)));
      const mark = el("path", {
        class: "tl-read" + (p.behind > 0 ? " is-behind" : ""),
        d: `M${x.toFixed(1)},${y - 9} l5,-6 l-10,0 z`,
      });
      const title = el("title", {});
      title.textContent = p.behind > 0
        ? `@${p.handle} last read #${p.read_seq} · ${p.behind} messages since${p.mentioned ? " · @-mentioned since" : ""}`
        : `@${p.handle} is caught up to #${p.read_seq}`;
      mark.appendChild(title);
      svg.appendChild(mark);
    }
  }

  function onKey(ev, order, isSelected, actions, state) {
    if (ev.key === "f" || ev.key === "F") {
      ev.preventDefault();
      actions.setFollowLive(!state.followLive);
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
    openEvent(order[i], actions);
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
