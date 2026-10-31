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
// Viewport: the visible interval is state.timeWindow when app.js has set an
// explicit time filter (the message list applies it too). Otherwise, while
// following live, it is the last LIVE_WINDOW_MS; when the page pauses
// (followLive false) the timeline keeps showing the interval it last showed
// live, so later arrivals do not move what the user is looking at.
//
// Keyboard: the timeline is one focusable control. Left/Right move the
// selection to the previous/next retained message (the view follows it),
// Home/End jump to the first/last, PageUp/PageDown move the view by one
// window width, Enter opens the selected message, and "f" toggles Follow
// live. Clicking the overview strip moves the view there.
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
  // Mentions are recognized exactly as the server does (chorus/wire.go
  // extractMentionedHandles): '@' not preceded by a handle character, then a
  // letter or digit, then up to 31 more letters, digits, '_' or '-'.
  const MENTION_RE = /(^|[^A-Za-z0-9_-])@([A-Za-z0-9][A-Za-z0-9_-]{0,31})(?![A-Za-z0-9_-])/g;
  // Layout constants in px.
  const OVERVIEW_H = 28;
  const AXIS_H = 18;
  const LANE_H = 26;
  const LABEL_W = 112;
  const PAD = 8;

  // Agent colors follow the Chorus logo, assigned by sorted handle so a given
  // agent keeps its color across renders of the same swarm.
  const PALETTE = ["#F59E0B", "#14B8A6", "#3B82F6", "#F43F5E", "#8B5CF6", "#22C55E", "#EC4899", "#64748B"];

  // Per-root viewport memory. It is interaction state, never swarm data:
  // lastLive is the interval most recently shown while following live, and
  // frozen is the interval shown while paused without an explicit filter.
  const viewports = new WeakMap();

  /**
   * render draws the timeline into rootEl.
   * state fields read: view, selectedTarget, selectedMessage, timeWindow,
   * followLive, filterAgent, filterArtifact, searchQuery.
   * selectedTarget is {kind: "channel", name: "#x"} or
   * {kind: "dm", pairKey: "a↔b", a, b} with a and b sorted.
   * selectedMessage is null or {target: "#channel" | "@recipient", seq}.
   * actions used: selectTarget(selectedTarget), selectMessage(target, seq)
   * where target is "#channel" or the recipient inbox "@handle",
   * setTimeWindow({startMs, endMs}), setFollowLive(bool).
   */
  function render(rootEl, state, actions) {
    const view = state && state.view;
    // Re-rendering replaces the SVG; keep keyboard focus on the timeline if
    // it had it, so arrow-key navigation survives new messages arriving.
    const hadFocus = rootEl.contains(document.activeElement);
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
    const win = viewport(rootEl, state, axis);
    const plotW = width - LABEL_W - PAD * 2;
    // While reviewing earlier messages, one extra row under the lanes holds
    // the notice, so it never overlaps the axis labels.
    const noticeH = state.followLive ? 0 : 18;
    const height = OVERVIEW_H + AXIS_H + lanes.length * LANE_H + PAD * 3 + noticeH;

    const svg = el("svg", {
      class: "tl",
      width: String(width),
      height: String(height),
      viewBox: `0 0 ${width} ${height}`,
      role: "application",
      tabindex: "0",
      "aria-label": `Swarm timeline, ${events.length} messages. Left and Right move between messages, Page Up and Page Down move the view, Enter opens, F toggles Follow live.`,
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
    const picked = state.selectedMessage || null;
    const isSelected = (e) => picked != null && picked.target === e.target && picked.seq === e.seq;

    const pan = (next) => panTo(rootEl, state, actions, next);
    drawOverview(svg, events, axis, ox, win, colors, plotW, actions, pan);
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
      drawReadPositions(svg, channel, mx, inWindow, win, laneY, plotW);
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
        r: isSel ? "6" : "4",
        fill: colors.get(e.sender),
      });
      const title = el("title", {});
      title.textContent = `${e.target} #${e.seq} · @${e.sender} · ${clock(e.t)} — ${firstLine(e.text, 120)}`;
      dot.appendChild(title);
      dot.addEventListener("click", () => openEvent(e, actions));
      svg.appendChild(dot);
    }

    const chosen = order.find(isSelected);
    if (chosen && inWindow(chosen.t)) drawMentions(svg, chosen, mx, laneY, colors);

    if (!state.followLive) {
      const back = el("text", { class: "tl-review", x: String(width - PAD), y: String(height - PAD), "text-anchor": "end" });
      back.textContent = "Reviewing earlier messages · press F to follow live";
      svg.appendChild(back);
    }

    svg.addEventListener("keydown", (ev) => onKey(ev, order, isSelected, actions, state, axis, win, pan));
    rootEl.appendChild(svg);
    if (hadFocus) svg.focus();
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
  // direct message the recipient's inbox ("@handle") plus the sorted pair
  // (a, b) that app.js uses to select a DM conversation.
  function toEvent(target, m) {
    const e = { target, seq: m.seq, sender: m.sender, text: m.text, t: m.sent_at_ms, replyTo: m.reply_to };
    if (target.startsWith("@")) {
      const [a, b] = [m.sender, target.slice(1)].sort();
      e.pair = { a, b, pairKey: a + "↔" + b };
    }
    return e;
  }

  // sameTarget reports whether event e belongs to the selected channel or DM
  // conversation.
  function sameTarget(sel, e) {
    return sel.kind === "channel" ? e.target === sel.name : !!e.pair && e.pair.pairKey === sel.pairKey;
  }

  // openEvent selects the event's channel or DM conversation, then the
  // message itself.
  function openEvent(e, actions) {
    if (e.pair) {
      actions.selectTarget({ kind: "dm", pairKey: e.pair.pairKey, a: e.pair.a, b: e.pair.b });
    } else {
      actions.selectTarget({ kind: "channel", name: e.target });
    }
    actions.selectMessage(e.target, e.seq);
  }

  // laneList returns one lane per sender seen in retained messages plus every
  // registered agent, sorted with humans first and then by handle. A sender
  // with no current registration is marked "not registered": it may have
  // left, expired, or disconnected, which the data cannot tell apart. The
  // human mark reflects the agent's current role only.
  function laneList(view, events) {
    const live = new Map((view.agents || []).map((a) => [a.handle, a]));
    const handles = new Set(events.map((e) => e.sender));
    for (const h of live.keys()) handles.add(h);
    const lanes = [...handles].map((h) => {
      const a = live.get(h);
      return { handle: h, human: !!(a && a.role === "human"), unregistered: !a };
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

  // liveWindow is the last LIVE_WINDOW_MS ending just after the newest message.
  function liveWindow(axis) {
    const endMs = axis.last + 60 * 1000;
    return { startMs: Math.max(axis.first, endMs - LIVE_WINDOW_MS), endMs };
  }

  // viewport returns the interval to draw (see the file comment) and updates
  // the per-root memory used to keep a paused view still.
  function viewport(rootEl, state, axis) {
    const mem = viewports.get(rootEl) || { lastLive: null, frozen: null };
    viewports.set(rootEl, mem);
    if (state.timeWindow) return state.timeWindow;
    if (state.followLive) {
      mem.frozen = null;
      mem.lastLive = liveWindow(axis);
      return mem.lastLive;
    }
    if (!mem.frozen) mem.frozen = mem.lastLive || liveWindow(axis);
    return mem.frozen;
  }

  // panTo shows the interval next. With an explicit filter it updates that
  // filter through app.js; otherwise it moves the timeline's own paused view
  // and pauses following live.
  function panTo(rootEl, state, actions, next) {
    if (state.timeWindow) {
      actions.setTimeWindow(next);
      return;
    }
    const mem = viewports.get(rootEl) || { lastLive: null };
    mem.frozen = next;
    viewports.set(rootEl, mem);
    if (state.followLive) actions.setFollowLive(false);
    else render(rootEl, state, actions);
  }

  // shiftWindow moves win by delta compressed-axis units, keeping its length.
  function shiftWindow(axis, win, delta) {
    const w0 = axis.pos(win.startMs);
    const w1 = axis.pos(win.endMs);
    const len = w1 - w0;
    const start = Math.min(Math.max(0, w0 + delta), Math.max(0, axis.total - len));
    return { startMs: timeAt(axis, start), endMs: timeAt(axis, start + len) };
  }

  // centerWindow returns a window of win's length centered on time t.
  function centerWindow(axis, win, t) {
    const len = axis.pos(win.endMs) - axis.pos(win.startMs);
    return shiftWindow(axis, win, axis.pos(t) - len / 2 - axis.pos(win.startMs));
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

  function drawOverview(svg, events, axis, ox, win, colors, plotW, actions, pan) {
    const y0 = PAD;
    const strip = el("rect", { class: "tl-overview", x: String(LABEL_W + PAD), y: String(y0), width: String(plotW), height: String(OVERVIEW_H - 8), rx: "4" });
    // Clicking the strip outside the window centers the view there.
    strip.addEventListener("click", (ev) => {
      const box = svg.getBoundingClientRect();
      const frac = (ev.clientX - box.left - LABEL_W - PAD) / plotW;
      pan(centerWindow(axis, win, timeAt(axis, Math.min(1, Math.max(0, frac)) * axis.total)));
    });
    svg.appendChild(strip);
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
    t.textContent = "Drag, or click the strip, to look at another part of the history";
    brush.appendChild(t);
    // Stop the browser from scrolling or zooming while a touch drags it.
    brush.style.touchAction = "none";
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
    const stop = () => {
      brush.removeEventListener("pointermove", move);
      brush.removeEventListener("pointerup", up);
      brush.removeEventListener("pointercancel", cancel);
    };
    // An interrupted drag (pointercancel) puts the window back unchanged.
    const cancel = () => {
      stop();
      brush.setAttribute("x", baseX.toFixed(1));
    };
    const up = () => {
      stop();
      // Convert the dragged pixel offset back to a time range of the same
      // compressed length.
      const frac0 = (x - minX) / plotW;
      const frac1 = (x + w - minX) / plotW;
      actions.setFollowLive(false);
      actions.setTimeWindow({ startMs: timeAt(axis, frac0 * axis.total), endMs: timeAt(axis, frac1 * axis.total) });
    };
    brush.addEventListener("pointermove", move);
    brush.addEventListener("pointerup", up);
    brush.addEventListener("pointercancel", cancel);
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
      class: "tl-lane" + (lane.human ? " is-human" : "") + (lane.unregistered ? " is-left" : ""),
      x1: String(LABEL_W), x2: String(width - PAD), y1: String(y), y2: String(y),
    }));
    const label = el("text", { class: "tl-lane-label" + (lane.unregistered ? " is-left" : ""), x: String(PAD), y: String(y + 4) });
    label.textContent = "@" + lane.handle + (lane.human ? " · human" : "") + (lane.unregistered ? " · not registered" : "");
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

  // drawReadPositions shows where each member of the channel last read, sent,
  // or joined. A bar on the member's own lane marks that message when it is
  // retained and inside the view. Otherwise a short note at the edge of the
  // lane says where it is instead of placing it at a time it does not have:
  // earlier or later than the view, or older than the kept history.
  function drawReadPositions(svg, channel, mx, inWindow, win, laneY, plotW) {
    const bySeq = new Map((channel.messages || []).map((m) => [m.seq, m]));
    for (const p of channel.positions || []) {
      const y = laneY.get(p.handle);
      if (y == null || p.read_seq === 0) continue;
      const m = bySeq.get(p.read_seq);
      const summary = p.behind > 0
        ? `@${p.handle} last read #${p.read_seq} · ${p.behind} messages since${p.mentioned ? " · @-mentioned since" : ""}`
        : `@${p.handle} is caught up to #${p.read_seq}`;
      let mark;
      if (m && inWindow(m.sent_at_ms)) {
        // A short bar centered on the member's own lane line, so it cannot
        // be read as belonging to the lane above or below.
        const x = mx(m.sent_at_ms);
        mark = el("rect", {
          class: "tl-read" + (p.behind > 0 ? " is-behind" : ""),
          x: (x - 1.5).toFixed(1),
          y: String(y - LANE_H / 2 + 3),
          width: "3",
          height: String(LANE_H - 6),
          rx: "1.5",
        });
      } else {
        const later = m && m.sent_at_ms > win.endMs;
        mark = el("text", {
          class: "tl-read-edge" + (p.behind > 0 ? " is-behind" : ""),
          x: String(later ? LABEL_W + PAD + plotW : LABEL_W + PAD),
          y: String(y - 4),
          "text-anchor": later ? "end" : "start",
        });
        mark.textContent = !m
          ? `◂ read #${p.read_seq} (older than kept history)`
          : later ? `read #${p.read_seq} ▸` : `◂ read #${p.read_seq}`;
      }
      const title = el("title", {});
      title.textContent = summary;
      mark.appendChild(title);
      svg.appendChild(mark);
    }
  }

  // drawMentions draws, for the selected message only, a dashed connector
  // from its dot to the lane of every agent it literally @-mentions. It
  // shows the text's mentions, not a claim that anyone owes a reply.
  function drawMentions(svg, e, mx, laneY, colors) {
    const x = mx(e.t).toFixed(1);
    const y0 = laneY.get(e.sender);
    // The server compares handles case-insensitively; so does this lookup.
    const byLower = new Map([...laneY].map(([h, y]) => [h.toLowerCase(), y]));
    for (const handle of mentionedHandles(e.text)) {
      if (handle === e.sender.toLowerCase()) continue;
      const y1 = byLower.get(handle);
      if (y1 == null) continue;
      svg.appendChild(el("line", { class: "tl-mention", x1: x, x2: x, y1: String(y0), y2: String(y1), stroke: colors.get(e.sender) }));
      svg.appendChild(el("circle", { class: "tl-mention-end", cx: x, cy: String(y1), r: "3", stroke: colors.get(e.sender) }));
    }
  }

  // mentionedHandles returns the lowercased handles @-mentioned in text, in
  // order and without repeats, matching the server's rule.
  function mentionedHandles(text) {
    const out = [];
    for (const m of String(text || "").matchAll(MENTION_RE)) {
      const h = m[2].toLowerCase();
      if (!out.includes(h)) out.push(h);
    }
    return out;
  }

  // onKey implements the keyboard contract in the file comment. order holds
  // every retained message with a lane, oldest first, so earlier history is
  // reachable; when the new selection is outside the view, the view is
  // centered on it.
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
