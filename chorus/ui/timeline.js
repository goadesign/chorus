// Chorus dashboard timeline.
//
// app.js owns all swarm data and calls ChorusTimeline.render(rootEl, state,
// actions) whenever state changes. This module draws one SVG from that state
// and reports what the user picks through actions; it keeps no copy of swarm
// data between renders except layout caches keyed by the view's identity.
//
// What it draws, all from fields the server returns (GET /swarms/{s}/view):
//   - an "Overview" strip of every retained message in the swarm, with a
//     draggable window that selects the time range shown below it;
//   - one lane per agent with something drawn in that range (a message, a
//     read mark, or a mention from the selected message), including senders
//     that have left, with lanes of people (declared model "human") styled
//     apart;
//   - a time axis labelled with the window's length and its start and end
//     times, on which idle stretches longer than QUIET_MS are shrunk to a
//     fixed-width "quiet" break (labelled on a row of its own) so bursts of
//     activity stay readable;
//   - for the selected channel only: the current pin (Chorus stores only the
//     current pin, so there is one landmark, placed at the pinned message
//     when it is still retained), each member's read position, and
//     reply_to curves between messages of that channel;
//   - highlights for messages matching the search or agent filter.
// Direct-message reply_to values are ambiguous across the two inboxes, so
// no reply curves are drawn for DMs.
//
// Viewport: the page is always live. While state.timeWindow is null the
// timeline shows the last LIVE_WINDOW_MS and moves with new messages. The
// user picks an earlier interval by dragging or clicking the overview strip,
// paging, or selecting a message outside the view; each sets
// state.timeWindow, which holds the view still (the message list applies it
// too) until "Jump to latest" (actions.jumpToLatest) returns both the
// timeline and the message list to the newest messages.
//
// Inspecting: clicking a message dot, the pin landmark, or a member's read
// mark selects it in place (actions.selectMessage with reveal: false) and
// shows one preview card next to it; the tab, filters, and page scroll stay
// as they are. Only the card's "Show in conversation" button (or Enter)
// opens the message in the conversation. The card is rebuilt from
// state.selectedMessage on every render, so it never outlives the data it
// describes. A read-mark card names the reader it was opened for; when that
// reader's position moves, the card closes instead of describing a new spot.
// The card closes with its close button, Escape, a second click on the same
// mark, or a click on empty chart space.
//
// Keyboard: the timeline is one focusable control. Left/Right select the
// previous/next retained message (the view follows it), Home/End jump to the
// first/last, PageUp/PageDown move the view by its own duration, Enter shows
// the selected message in the conversation, and Escape closes the preview.
// Clicking the overview strip moves the view there.
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
  const OVERVIEW_H = 34;
  // The axis has three rows of text 16px apart (the measured height of a
  // rendered label), so labels never overlap at any width: the window length
  // and its edge clocks, then quiet-break labels, then the pin label just
  // above the lanes.
  const AXIS_ROW = 16;
  const AXIS_H = AXIS_ROW * 3;
  const LANE_H = 38;
  const LABEL_W = 160;
  const PAD = 8;
  // Horizontal space between the label gutter (and the right edge) and the
  // plot, so the first and last dots never touch a label or the border.
  const PLOT_PAD = 18;
  const PLOT_X0 = LABEL_W + PLOT_PAD;
  // Lane identity mark: the provider logo (or initial) drawn at this size,
  // with no circle or plate around it.
  const AVATAR = 22;
  // Dot radius, and radius of the selected dot.
  const DOT_R = 6;
  const DOT_R_SELECTED = 8;

  // Per-root swarm memory: the swarm each root last drew. When it changes, a
  // drag started on the previous swarm's chart is dropped.
  const drawnSwarm = new WeakMap();

  // Per-root drag state while the overview window is being dragged. The
  // pointer is captured on rootEl, which survives re-renders, so a drag keeps
  // working while setTimeWindow redraws the SVG on every frame.
  const drags = new WeakMap();
  // Roots whose drag listeners are installed (once per root element).
  const dragRoots = new WeakSet();
  // Per-card function that places the card again next to its point, used
  // when the scrolling message area moves under an open card.
  const cardPlacers = new WeakMap();
  // Roots whose scroll listener is installed (once per root element).
  const scrollRoots = new WeakSet();

  /**
   * render draws the timeline into rootEl.
   * state fields read: view, selectedTarget, selectedMessage, timeWindow
   * (null while showing the latest messages), filterAgent, searchQuery.
   * selectedTarget is {kind: "channel", name: "#x"} or
   * {kind: "dm", pairKey: "a↔b", a, b} with a and b sorted.
   * selectedMessage is null or {target: "#channel" | "@recipient", seq,
   * reader?} where reader is set when a member's read mark was picked.
   * actions used: selectMessage(target, seq, opts) where target is
   * "#channel" or the recipient inbox "@handle" and opts.reveal === false
   * inspects in place (opts.reader names a picked read mark) while the default
   * shows the message in the conversation; setReplyTo(channel, seq, sender,
   * human) and composeDM(handle) for the card's Reply button;
   * setTimeWindow({startMs, endMs} | null); and jumpToLatest(), which returns
   * both the timeline and the message list to the newest messages.
   */
  function render(rootEl, state, actions) {
    const view = state && state.view;
    // Re-rendering replaces the SVG; keep keyboard focus on the timeline if
    // it had it, so arrow-key navigation survives new messages arriving.
    // Remember which control had focus (the chart or the Jump to latest
    // button) so the same one gets it back.
    const focused = document.activeElement;
    const hadFocus = rootEl.contains(focused);
    const jumpHadFocus = hadFocus && focused.classList.contains("tl-jump");
    // A focused preview-card button is found again by its data-tl-card name.
    const cardFocus = hadFocus && focused.dataset ? focused.dataset.tlCard || "" : "";
    // How far the open card's own contents were scrolled, and which item it
    // described, so a redraw of the same card keeps showing the same part of
    // it while a newly picked item starts at its top.
    const oldCard = rootEl.querySelector(".tl-card");
    const cardScroll = oldCard ? oldCard.scrollTop : 0;
    const oldCardKey = oldCard ? oldCard.dataset.tlItem : "";
    clear(rootEl);
    if (!view) {
      // app.js clears the view when it switches swarms, before loading the
      // new one. The old chart is gone, so a drag on it ends here without
      // applying or restoring anything, and its pointer capture is released.
      const d = drags.get(rootEl);
      dropDrag(rootEl);
      if (d && rootEl.hasPointerCapture(d.pointerId)) rootEl.releasePointerCapture(d.pointerId);
      rootEl.appendChild(note("Loading swarm…"));
      return;
    }
    // Record which swarm this view belongs to before anything else, so an
    // empty swarm still drops a drag started on the previous one.
    noteSwarm(rootEl, view.swarm);
    const events = collectEvents(view);
    if (events.length === 0) {
      rootEl.appendChild(note("No messages yet. Agents' messages will appear here as they arrive."));
      return;
    }

    const width = Math.max(320, rootEl.clientWidth || 960);
    const sel = state.selectedTarget || null;
    const picked = state.selectedMessage || null;
    const axis = buildAxis(events);
    const win = state.timeWindow || liveWindow(axis);
    const inWindow = (t) => t >= win.startMs && t <= win.endMs;
    const channel = sel && sel.kind === "channel"
      ? (view.channels || []).find((c) => c.name === sel.name)
      : null;
    const lanes = laneList(view, events, inWindow, channel, picked);
    const colors = colorMap(view, events);
    const plotW = width - PLOT_X0 - PLOT_PAD;
    // A range with no activity keeps one row for its "No activity" note.
    const height = OVERVIEW_H + AXIS_H + Math.max(1, lanes.length) * LANE_H + PAD * 3;

    const svg = el("svg", {
      class: "tl",
      width: String(width),
      height: String(height),
      viewBox: `0 0 ${width} ${height}`,
      role: "application",
      tabindex: "0",
      "aria-label": `Swarm timeline, ${events.length} messages. Left and Right move between messages, Home and End jump to the first and last, Page Up and Page Down move the view, Enter opens the selected message.`,
    });

    // Scales: overview maps the whole compressed axis; the main plot maps the
    // selected window.
    const ox = (t) => PLOT_X0 + (axis.pos(t) / axis.total) * plotW;
    const w0 = axis.pos(win.startMs);
    const w1 = Math.max(w0 + 1, axis.pos(win.endMs));
    const mx = (t) => PLOT_X0 + ((axis.pos(t) - w0) / (w1 - w0)) * plotW;

    const match = matcher(state);
    const isSelected = (e) => picked != null && picked.target === e.target && picked.seq === e.seq;

    const pan = (next) => actions.setTimeWindow(next);
    // inspect selects a message, pin, or read mark in place; picking the
    // one already shown closes its preview.
    const inspect = (target, seq, reader) => {
      const same = picked != null && picked.target === target && picked.seq === seq &&
        (picked.reader || "") === (reader || "");
      if (same) actions.selectMessage(null, 0, { reveal: false });
      else actions.selectMessage(target, seq, reader ? { reveal: false, reader } : { reveal: false });
    };
    installDrag(rootEl);
    installCardScroll(rootEl);
    drawOverview(svg, rootEl, events, axis, ox, win, colors, plotW, actions, pan, state);
    svg.appendChild(gutterClip(LABEL_W - PAD));
    const top = OVERVIEW_H + PAD;
    drawAxis(svg, axis, win, w0, w1, mx, top, plotW, height);
    const laneTop = top + AXIS_H;
    const laneY = new Map();
    lanes.forEach((lane, i) => {
      const y = laneTop + i * LANE_H + LANE_H / 2;
      laneY.set(lane.handle, y);
      drawLane(svg, lane, y, width, colors);
    });
    if (lanes.length === 0) {
      const empty = el("text", { class: "tl-label tl-empty-range", x: (PLOT_X0 + plotW / 2).toFixed(1), y: String(laneTop + LANE_H / 2 + 4), "text-anchor": "middle" });
      empty.textContent = "No activity in this window";
      svg.appendChild(empty);
    }

    if (channel) {
      drawPin(svg, channel, mx, inWindow, laneTop, lanes.length, inspect);
      drawReplies(svg, channel, mx, inWindow, laneY, colors);
      drawReadPositions(svg, channel, mx, inWindow, laneY, picked, inspect);
    }

    const order = events;
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
        r: String(isSel ? DOT_R_SELECTED : DOT_R),
        fill: colors.get(e.sender),
      });
      const title = el("title", {});
      title.textContent = `${e.target} #${e.seq} · @${e.sender} · ${clock(e.t)} — ${firstLine(e.text, 120)}`;
      dot.appendChild(title);
      dot.addEventListener("click", (ev) => {
        ev.stopPropagation();
        inspect(e.target, e.seq, "");
      });
      svg.appendChild(dot);
    }

    const chosen = order.find(isSelected);
    if (chosen && inWindow(chosen.t)) drawMentions(svg, chosen, view, mx, laneY, colors);

    svg.addEventListener("keydown", (ev) => onKey(ev, order, isSelected, picked, actions, axis, win, pan));
    // A click on empty plot space (below the overview strip, not on a dot or
    // mark, whose handlers stop the click) closes the preview.
    svg.addEventListener("click", (ev) => {
      if (!picked || ev.clientY - svg.getBoundingClientRect().top < OVERVIEW_H + PAD) return;
      actions.selectMessage(null, 0, { reveal: false });
    });
    rootEl.appendChild(svg);

    // The preview card for the picked message or read mark.
    const found = cardAnchor(picked, order, channel, inWindow);
    let card = null;
    if (found && found.stale) {
      // The picked reader moved on: close the preview once this render ends,
      // unless something else replaced the selection first.
      queueMicrotask(() => {
        if (state.selectedMessage === picked) actions.selectMessage(null, 0, { reveal: false });
      });
    } else if (found) {
      const y = laneY.get(found.reader ? found.reader.handle : found.e.sender);
      if (y != null) {
        const lane = lanes.find((l) => l.handle === found.e.sender);
        card = buildCard(found, lane, channel, actions, svg);
        const place = () => placeCard(rootEl, svg, card, mx(found.e.t), y, width);
        cardPlacers.set(card, place);
        place();
        if (card.dataset.tlItem === oldCardKey) card.scrollTop = cardScroll;
      }
    }
    // While an earlier interval is shown, one quiet button returns the whole
    // page to the latest messages; it is a real button so keyboards and
    // screen readers reach it.
    if (state.timeWindow) {
      const jump = document.createElement("button");
      jump.type = "button";
      jump.className = "tl-jump";
      jump.textContent = "Jump to latest";
      jump.addEventListener("click", () => actions.jumpToLatest());
      rootEl.appendChild(jump);
      if (jumpHadFocus) jump.focus({ preventScroll: true });
    }
    // Restoring focus must not scroll the page under the reader. A focused
    // card button keeps focus when its card is drawn again; when the card is
    // gone, focus returns to the chart.
    const cardTarget = card && cardFocus ? card.querySelector(`[data-tl-card="${cardFocus}"]`) : null;
    if (cardTarget) {
      cardTarget.focus({ preventScroll: true });
      revealInCard(card, cardTarget);
    }
    else if (hadFocus && !(jumpHadFocus && state.timeWindow)) svg.focus({ preventScroll: true });
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
  // direct message the recipient's inbox ("@handle") plus the conversation
  // key app.js uses to select it: the two handles exactly as registered
  // (Chorus handles are case-sensitive, so @API and @api are different
  // agents), sorted, joined by ↔.
  function toEvent(target, m) {
    const e = { target, seq: m.seq, sender: m.sender, text: m.text, t: m.sent_at_ms, replyTo: m.reply_to, human: m.human === true };
    if (target.startsWith("@")) {
      const [a, b] = [m.sender, target.slice(1)].sort();
      e.pair = { a, b, pairKey: a + "↔" + b };
    }
    return e;
  }

  // sameTarget reports whether event e belongs to the selected view: every
  // message for "all", one channel, or one DM conversation.
  function sameTarget(sel, e) {
    if (sel.kind === "all") return true;
    return sel.kind === "channel" ? e.target === sel.name : !!e.pair && e.pair.pairKey === sel.pairKey;
  }

  // cardAnchor returns what the preview card describes: {e} for a picked
  // message, {e, reader} for a picked read mark whose reader is still at that
  // message, {stale: true} when the picked reader's position has moved or is
  // gone, or null when nothing is picked or the message is outside the view.
  function cardAnchor(picked, order, channel, inWindow) {
    if (!picked) return null;
    let reader = null;
    if (picked.reader) {
      reader = channel && channel.name === picked.target
        ? (channel.positions || []).find((p) => p.handle === picked.reader && p.read_seq === picked.seq)
        : null;
      if (!reader) return { stale: true };
    }
    const e = order.find((x) => x.target === picked.target && x.seq === picked.seq);
    if (!e || !inWindow(e.t)) return null;
    return reader ? { e, reader } : { e };
  }

  // buildCard returns the preview card for found (see cardAnchor): who sent
  // the message, where and when, why it is picked (pin, reply, or a reader's
  // position), the start of its text, and the Show in conversation, Reply,
  // and close controls. Each control carries a data-tl-card name so focus can
  // return to it after the next render.
  function buildCard(found, lane, channel, actions, svg) {
    const e = found.e;
    const close = () => {
      actions.selectMessage(null, 0, { reveal: false });
      svg.focus({ preventScroll: true });
    };
    const card = html("div", "tl-card");
    // Identifies the inspected item, so a redraw can tell the same card from
    // a newly picked one.
    card.dataset.tlItem = `${e.target} ${e.seq} ${found.reader ? found.reader.handle : ""}`;
    card.setAttribute("role", "group");
    card.setAttribute("aria-label", `Preview of ${e.target} #${e.seq} from @${e.sender}`);
    card.style.touchAction = "pan-y";
    card.addEventListener("keydown", (ev) => {
      if (ev.key !== "Escape") return;
      ev.preventDefault();
      close();
    });

    const header = html("div", "tl-card-header");
    const meta = html("div", "tl-card-meta");
    // The badge follows the message's own human flag. The sender's current
    // model only picks the provider logo for a message not sent as a person,
    // and never marks such a message as a person's when the sender has since
    // declared "human".
    const current = lane ? lane.model : "";
    const nowHuman = current.trim().toLowerCase() === "human";
    const ui = window.ChorusUI;
    meta.appendChild(ui && ui.handleBadge && (e.human || !nowHuman)
      ? ui.handleBadge(e.sender, e.human ? "" : current, "", e.human)
      : html("span", "", "@" + e.sender));
    meta.appendChild(html("span", "", e.pair ? `${e.pair.a} ↔ ${e.pair.b} #${e.seq}` : `${e.target} #${e.seq}`));
    meta.appendChild(html("span", "", clock(e.t)));
    header.appendChild(meta);
    const closeBtn = button("tl-card-close", "×", "close", close);
    closeBtn.setAttribute("aria-label", "Close preview");
    header.appendChild(closeBtn);
    card.appendChild(header);

    const notes = [];
    if (found.reader) {
      const p = found.reader;
      notes.push(p.behind > 0 ? `@${p.handle} read through #${p.read_seq} · ${p.behind} behind` : `@${p.handle} is caught up`);
    }
    if (channel && channel.name === e.target && channel.pin && channel.pin.seq === e.seq) notes.push("Pinned");
    if (e.replyTo) notes.push(`↩ #${e.replyTo}`);
    if (notes.length) card.appendChild(html("span", "tl-card-note", notes.join(" · ")));

    card.appendChild(html("p", "tl-card-body", preview(e.text)));

    const row = html("div", "tl-card-actions");
    row.appendChild(button("", "Show in conversation", "show", () => actions.selectMessage(e.target, e.seq)));
    row.appendChild(button("", "Reply", "reply", () => {
      if (e.pair) actions.composeDM(e.sender);
      else actions.setReplyTo(e.target, e.seq, e.sender, e.human);
    }));
    card.appendChild(row);
    return card;
  }

  // placeCard adds card to rootEl and positions it next to the anchor at
  // (x, y) in chart coordinates, kept horizontally inside the chart. It opens
  // below the anchor, over the lanes below and the page under the chart, so
  // it hides as few other messages as possible. The space that counts is
  // the visible part of the scrolling area that holds the chart (between the
  // page header and the composer), not the browser window: the card opens
  // above when below does not fit there and above does, and otherwise on the
  // side with more room, limited to that room so its buttons stay visible
  // (the card scrolls inside). The card always stays inside that visible
  // area. The card floats above the page, so placing it never moves layout.
  function placeCard(rootEl, svg, card, x, y, width) {
    // Placing again (after the message area scrolled) measures the card at
    // its natural height, then restores how far its contents were scrolled.
    const inner = card.scrollTop;
    card.style.visibility = "hidden";
    card.style.maxHeight = "";
    card.style.overflowY = "";
    if (!card.isConnected) rootEl.appendChild(card);
    const rootBox = rootEl.getBoundingClientRect();
    const svgBox = svg.getBoundingClientRect();
    const area = (rootEl.closest("#stage-container") || document.documentElement).getBoundingClientRect();
    // The browser scales the chart to the section's width, so chart
    // coordinates are converted to the pixels actually on screen.
    const scale = svgBox.width / width;
    const ax = svgBox.left + x * scale;
    const ay = svgBox.top + y * scale;
    const cw = card.offsetWidth;
    const ch = card.offsetHeight;
    const gap = (DOT_R_SELECTED + 6) * scale;
    // Room counts only the visible part of the area on each side of the
    // point, so a point scrolled out of view never claims space nobody sees.
    const roomBelow = area.bottom - PAD - Math.max(ay + gap, area.top + PAD);
    const roomAbove = Math.min(ay - gap, area.bottom - PAD) - (area.top + PAD);
    const below = ch <= roomBelow || (ch > roomAbove && roomBelow >= roomAbove);
    const room = below ? roomBelow : roomAbove;
    if (ch > room) {
      card.style.maxHeight = `${Math.max(0, Math.floor(room))}px`;
      card.style.overflowY = "auto";
    }
    const shown = Math.min(ch, Math.max(0, room));
    const left = Math.max(svgBox.left + PAD, Math.min(ax - cw / 2, svgBox.right - cw - PAD));
    // When the point itself has scrolled out of the visible area, the card
    // still stays wholly inside it, so its controls remain reachable.
    const wanted = below ? ay + gap : ay - gap - shown;
    const top = Math.max(area.top + PAD, Math.min(wanted, area.bottom - PAD - shown));
    // Absolute positions are measured from the section's padding edge.
    card.style.left = `${Math.round(left - rootBox.left - rootEl.clientLeft)}px`;
    card.style.top = `${Math.round(top - rootBox.top - rootEl.clientTop)}px`;
    card.style.visibility = "";
    card.scrollTop = inner;
  }

  // installCardScroll places the open card again whenever the scrolling
  // message area that holds the chart scrolls, so the card stays next to
  // its point and inside the visible area. It is installed once per root.
  function installCardScroll(rootEl) {
    if (scrollRoots.has(rootEl)) return;
    const area = rootEl.closest("#stage-container");
    if (!area) return;
    scrollRoots.add(rootEl);
    area.addEventListener("scroll", () => {
      const card = rootEl.querySelector(".tl-card");
      const place = card && cardPlacers.get(card);
      if (place) place();
    }, { passive: true });
  }

  // revealInCard scrolls only the card's own contents so that control is
  // fully visible; the page and the message area do not move.
  function revealInCard(card, control) {
    const top = control.offsetTop;
    const bottom = top + control.offsetHeight;
    if (top < card.scrollTop) card.scrollTop = top;
    else if (bottom > card.scrollTop + card.clientHeight) card.scrollTop = bottom - card.clientHeight;
  }

  // laneList returns the lanes to draw for the current range: each known
  // handle (registered agents and senders of retained messages) that has
  // something drawn there, namely a message in the range, its read mark in
  // the selected channel, or a mention from the selected message. A range
  // with no activity has no lanes (render shows a note instead). Lanes
  // change as the range moves; the page keeps the chart itself still while
  // that happens. Humans come first,
  // then handles in order. A sender with no current registration keeps its
  // messages but is drawn muted; the data cannot tell whether it left,
  // expired, or disconnected, so the lane makes no claim about why. The human
  // mark reflects the agent's current declared model only.
  function laneList(view, events, inWindow, channel, picked) {
    const live = new Map((view.agents || []).map((a) => [a.handle, a]));
    const known = new Set([...live.keys(), ...events.map((e) => e.sender)]);
    const shown = new Set();
    for (const e of events) if (inWindow(e.t)) shown.add(e.sender);
    if (channel) {
      const bySeq = new Map((channel.messages || []).map((m) => [m.seq, m]));
      for (const p of channel.positions || []) {
        const m = bySeq.get(p.read_seq);
        if (m && inWindow(m.sent_at_ms)) shown.add(p.handle);
      }
    }
    const chosen = picked ? events.find((e) => e.target === picked.target && e.seq === picked.seq) : null;
    if (chosen && inWindow(chosen.t)) {
      for (const h of mentionTargets(chosen, view, known)) shown.add(h);
    }
    const lanes = [...shown].map((h) => {
      const a = live.get(h);
      return {
        handle: h,
        model: a ? a.model || "" : "",
        human: !!(a && a.model === "human"),
        unregistered: !a,
      };
    });
    lanes.sort((a, b) => (b.human - a.human) || a.handle.localeCompare(b.handle));
    return lanes;
  }

  // colorMap gives every registered agent and every retained sender a color
  // from app.js's agentColorHex, so overview ticks keep their sender's color
  // even when that sender has no lane in the current window, and colors match
  // the message list. app.js defines window.ChorusUI before it first renders.
  function colorMap(view, events) {
    const models = new Map((view.agents || []).map((a) => [a.handle, a.model || ""]));
    const handles = new Set([...models.keys(), ...events.map((e) => e.sender)]);
    return new Map([...handles].map((h) => [h, window.ChorusUI.agentColorHex(h, models.get(h) || "")]));
  }

  // buildAxis maps real time to a compressed position: time inside active
  // stretches counts fully, and each gap longer than QUIET_MS counts as
  // QUIET_UNITS. Time before the oldest retained message counts fully too
  // (negative positions), so a view longer than the retained history is
  // drawn at its true scale. pos(t) is monotonic, so ordering is kept.
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
      return p;
    }
    const total = Math.max(1, pos(last) + 60 * 1000);
    return { first, last, gaps, pos, total };
  }

  // liveWindow is the last LIVE_WINDOW_MS ending just after the newest message.
  function liveWindow(axis) {
    const endMs = axis.last + 60 * 1000;
    return { startMs: Math.max(axis.first, endMs - LIVE_WINDOW_MS), endMs };
  }

  // noteSwarm records the swarm rootEl is drawing and drops any drag started
  // while it showed a different swarm, so that drag never applies here.
  function noteSwarm(rootEl, swarm) {
    if (drawnSwarm.get(rootEl) === swarm) return;
    drawnSwarm.set(rootEl, swarm);
    dropDrag(rootEl);
  }

  // A view is a span of real time (state.timeWindow filters the message list
  // by those timestamps), so moving it never changes its duration. The
  // compressed axis only decides where things are drawn: a drag of the
  // overview moves the view's start by the dragged distance on that axis.

  // placeWindow returns a view of win's real duration starting at startMs,
  // clamped to the retained history: it never ends after the newest message
  // and never starts before the oldest one, except when the history is
  // shorter than the view (for example after old messages were trimmed),
  // where the view keeps its duration and ends at the newest message.
  function placeWindow(axis, win, startMs) {
    const dur = win.endMs - win.startMs;
    const lastEnd = axis.last + 60 * 1000;
    const start = Math.max(Math.min(axis.first, lastEnd - dur), Math.min(startMs, lastEnd - dur));
    return { startMs: start, endMs: start + dur };
  }

  // shiftWindow moves win by delta compressed-axis units, measured at its
  // start, keeping its real duration.
  function shiftWindow(axis, win, delta) {
    return placeWindow(axis, win, timeAt(axis, axis.pos(win.startMs) + delta));
  }

  // centerWindow returns a view of win's real duration centered on time t.
  function centerWindow(axis, win, t) {
    return placeWindow(axis, win, t - (win.endMs - win.startMs) / 2);
  }

  // matcher returns a predicate for the active search and agent filters, or
  // null when neither is set.
  function matcher(state) {
    const q = (state.searchQuery || "").trim().toLowerCase();
    const agent = state.filterAgent || "";
    if (!q && !agent) return null;
    return (e) =>
      (!q || e.text.toLowerCase().includes(q) || e.sender.toLowerCase().includes(q)) &&
      (!agent || e.sender === agent);
  }

  // drawOverview draws the whole-history strip and the draggable window.
  function drawOverview(svg, rootEl, events, axis, ox, win, colors, plotW, actions, pan, state) {
    const y0 = PAD;
    const strip = el("rect", { class: "tl-overview", x: String(PLOT_X0), y: String(y0), width: String(plotW), height: String(OVERVIEW_H - 8), rx: "4" });
    // Clicking the strip outside the window centers the view there.
    strip.addEventListener("click", (ev) => {
      const box = svg.getBoundingClientRect();
      const frac = (ev.clientX - box.left - PLOT_X0) / plotW;
      pan(centerWindow(axis, win, timeAt(axis, Math.min(1, Math.max(0, frac)) * axis.total)));
    });
    svg.appendChild(strip);
    for (const e of events) {
      svg.appendChild(el("line", {
        class: "tl-tick",
        x1: ox(e.t).toFixed(1), x2: ox(e.t).toFixed(1),
        y1: String(y0 + 5), y2: String(y0 + OVERVIEW_H - 13),
        stroke: colors.get(e.sender),
      }));
    }
    const label = el("text", { class: "tl-label", x: String(PAD), y: String(y0 + 17) });
    label.textContent = "Overview";
    svg.appendChild(label);

    // The window rectangle can be dragged. While it moves, the new range is
    // sent to app.js on every animation frame, so the plot and the message
    // list follow the drag live.
    // A view longer than the retained history starts before the strip; the
    // box then begins at the strip's left edge.
    const x0 = Math.max(PLOT_X0, ox(win.startMs));
    const x1 = Math.max(x0 + 6, ox(win.endMs));
    const brush = el("rect", {
      class: "tl-window",
      x: x0.toFixed(1), y: String(y0), width: (x1 - x0).toFixed(1), height: String(OVERVIEW_H - 8), rx: "4",
    });
    const t = el("title", {});
    t.textContent = "Drag, or click the strip, to look at another part of the history";
    brush.appendChild(t);
    brush.addEventListener("pointerdown", (ev) => {
      ev.preventDefault();
      drags.set(rootEl, {
        pointerId: ev.pointerId,
        startX: ev.clientX,
        base: win,
        original: state.timeWindow || null,
        axis,
        plotW,
        actions,
        frame: 0,
        next: null,
      });
      // Capture after recording the drag, so pointermove and pointerup reach
      // rootEl even when the pointer leaves the timeline.
      rootEl.setPointerCapture(ev.pointerId);
    });
    svg.appendChild(brush);
  }

  // dropDrag forgets rootEl's drag, cancelling any frame update it scheduled
  // so a stale window can never be applied afterwards.
  function dropDrag(rootEl) {
    const d = drags.get(rootEl);
    if (!d) return;
    drags.delete(rootEl);
    if (d.frame) cancelAnimationFrame(d.frame);
  }

  // installDrag adds the pointer listeners for window drags to rootEl once.
  // They read the drag state stored by the brush's pointerdown.
  function installDrag(rootEl) {
    if (dragRoots.has(rootEl)) return;
    dragRoots.add(rootEl);
    // Stop the browser from scrolling or zooming while a touch drags.
    rootEl.style.touchAction = "pan-y";
    rootEl.addEventListener("pointermove", (ev) => {
      const d = drags.get(rootEl);
      if (!d || ev.pointerId !== d.pointerId) return;
      const delta = ((ev.clientX - d.startX) / d.plotW) * d.axis.total;
      d.next = shiftWindow(d.axis, d.base, delta);
      if (!d.frame) {
        d.frame = requestAnimationFrame(() => {
          d.frame = 0;
          if (drags.get(rootEl) !== d || !d.next) return;
          d.actions.setTimeWindow(d.next);
        });
      }
    });
    const end = (ev, cancelled) => {
      const d = drags.get(rootEl);
      if (!d || ev.pointerId !== d.pointerId) return;
      dropDrag(rootEl);
      if (rootEl.hasPointerCapture(ev.pointerId)) rootEl.releasePointerCapture(ev.pointerId);
      if (cancelled) {
        // An interrupted drag restores exactly the interval set before it
        // (null when the latest messages were showing).
        d.actions.setTimeWindow(d.original);
      } else if (d.next) {
        d.actions.setTimeWindow(d.next);
      }
    };
    rootEl.addEventListener("pointerup", (ev) => end(ev, false));
    rootEl.addEventListener("pointercancel", (ev) => end(ev, true));
    // Losing the capture (another element took it, or the window lost
    // focus) ends the drag the same way as a cancel. After a normal
    // pointerup the drag is already gone, so this does nothing.
    rootEl.addEventListener("lostpointercapture", (ev) => end(ev, true));
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

  // drawAxis draws the quiet breaks and the clock labels at both window edges
  // (the view's own start and end times) on the upper line of the axis row;
  // the pin badge uses the lower line.
  function drawAxis(svg, axis, win, w0, w1, mx, top, plotW, height) {
    const y = top + AXIS_ROW - 4;
    const quietY = top + 2 * AXIS_ROW - 4;
    for (const g of axis.gaps) {
      if (axis.pos(g.to) < w0 || axis.pos(g.from) > w1) continue;
      const xa = mx(g.from);
      const xb = mx(g.to);
      svg.appendChild(el("rect", { class: "tl-quiet", x: xa.toFixed(1), y: String(top), width: Math.max(2, xb - xa).toFixed(1), height: String(height - top - PAD) }));
      const lbl = el("text", { class: "tl-quiet-label", x: ((xa + xb) / 2).toFixed(1), y: String(quietY), "text-anchor": "middle" });
      lbl.textContent = `${duration(g.to - g.from)} quiet`;
      svg.appendChild(lbl);
    }
    // Clock labels at both window edges.
    const left = el("text", { class: "tl-axis", x: String(PLOT_X0), y: String(y) });
    left.textContent = clock(win.startMs);
    const right = el("text", { class: "tl-axis", x: String(PLOT_X0 + plotW), y: String(y), "text-anchor": "end" });
    right.textContent = clock(win.endMs);
    svg.appendChild(left);
    svg.appendChild(right);
    // The gutter names how much real time the plot below shows.
    const span = el("text", { class: "tl-label", x: String(PAD), y: String(y) });
    span.textContent = `${duration(win.endMs - win.startMs)} window`;
    svg.appendChild(span);
  }

  // drawLane draws a lane line and its @handle label, clipped to the label
  // gutter so a long handle never runs into the plot. Details go in the
  // label's tooltip rather than on the line.
  function drawLane(svg, lane, y, width, colors) {
    svg.appendChild(el("line", {
      class: "tl-lane" + (lane.human ? " is-human" : "") + (lane.unregistered ? " is-left" : ""),
      x1: String(LABEL_W), x2: String(width - PLOT_PAD), y1: String(y), y2: String(y),
    }));
    // Identity mark from app.js's shared resolver, so the lane shows the same
    // provider logo (or neutral initial) as the rest of the page, with no
    // circle around it.
    const id = window.ChorusUI.resolveIdentity(lane.handle, lane.model, lane.human);
    const cx = PAD + AVATAR / 2;
    if (id.iconUrl) {
      svg.appendChild(el("image", {
        // The provider class lets one shared CSS rule size each provider's
        // mark (the OpenAI glyph fills less of its canvas than the others).
        class: "tl-avatar-icon provider-" + id.provider, href: id.iconUrl,
        x: String(cx - AVATAR / 2), y: String(y - AVATAR / 2),
        width: String(AVATAR), height: String(AVATAR),
      }));
    } else {
      // No logo (a person, or an agent without a recognized model): draw the
      // same two-letter badge the participant cards use, a soft disc in the
      // lane's color with the letters centered on the lane line.
      const color = colors.get(lane.handle);
      svg.appendChild(el("circle", {
        class: "tl-avatar-disc", cx: String(cx), cy: String(y), r: String(AVATAR / 2),
        fill: color, "fill-opacity": "0.14",
      }));
      const initial = el("text", { class: "tl-avatar-initial", x: String(cx), y: String(y), "text-anchor": "middle" });
      initial.textContent = lane.handle.slice(0, 2).toLowerCase();
      initial.setAttribute("fill", color);
      svg.appendChild(initial);
    }
    const label = el("text", {
      class: "tl-lane-label" + (lane.unregistered ? " is-left" : ""),
      x: String(PAD + AVATAR + 6), y: String(y + 4), "clip-path": "url(#tl-gutter)",
    });
    label.textContent = "@" + lane.handle;
    label.setAttribute("fill", colors.get(lane.handle));
    const title = el("title", {});
    // The exact model the agent declared on join or who, when it has one.
    title.textContent = "@" + lane.handle + (lane.model ? " · " + lane.model : "") +
      (lane.human ? " · human" : "") +
      (lane.unregistered ? " · no current registration (messages kept)" : "");
    label.appendChild(title);
    svg.appendChild(label);
  }

  // gutterClip returns the clipPath (id tl-gutter) that keeps lane labels
  // inside the label column of the given width.
  function gutterClip(width) {
    const clip = el("clipPath", { id: "tl-gutter" });
    clip.appendChild(el("rect", { x: "0", y: "0", width: String(width), height: "100000" }));
    return clip;
  }

  // drawPin places the single current-pin landmark at the pinned message's
  // stored time when that message is still retained in the channel.
  // Clicking the line or its label inspects the pinned message.
  function drawPin(svg, channel, mx, inWindow, laneTop, laneCount, inspect) {
    if (!channel.pin) return;
    const msg = (channel.messages || []).find((m) => m.seq === channel.pin.seq);
    if (!msg || !inWindow(msg.sent_at_ms)) return;
    const x = mx(msg.sent_at_ms).toFixed(1);
    const pick = (ev) => {
      ev.stopPropagation();
      inspect(channel.name, channel.pin.seq, "");
    };
    const line = el("line", { class: "tl-pin", x1: x, x2: x, y1: String(laneTop - 4), y2: String(laneTop + laneCount * LANE_H) });
    line.addEventListener("click", pick);
    svg.appendChild(line);
    const lbl = el("text", { class: "tl-pin-label", x: x, y: String(laneTop - 3), "text-anchor": "middle" });
    lbl.textContent = `Pin #${channel.pin.seq}`;
    lbl.addEventListener("click", pick);
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

  // drawReadPositions marks, on each member's own lane, the message that
  // member last read, sent, or joined at in this channel, when that message
  // is retained and inside the view. Positions elsewhere are not drawn; the
  // participant cards report every member's read state. Clicking a mark
  // inspects that member's position; the picked mark is drawn selected.
  function drawReadPositions(svg, channel, mx, inWindow, laneY, picked, inspect) {
    const bySeq = new Map((channel.messages || []).map((m) => [m.seq, m]));
    for (const p of channel.positions || []) {
      const y = laneY.get(p.handle);
      const m = bySeq.get(p.read_seq);
      if (y == null || !m || !inWindow(m.sent_at_ms)) continue;
      // A short bar centered on the member's own lane line, so it cannot be
      // read as belonging to the lane above or below.
      const x = mx(m.sent_at_ms);
      const chosen = picked != null && picked.reader === p.handle && picked.target === channel.name && picked.seq === p.read_seq;
      const mark = el("rect", {
        class: "tl-read" + (p.behind > 0 ? " is-behind" : "") + (chosen ? " is-selected" : ""),
        x: (x - 1.5).toFixed(1),
        y: String(y - LANE_H / 2 + 6),
        width: "3",
        height: String(LANE_H - 12),
        rx: "1.5",
      });
      const title = el("title", {});
      title.textContent = p.behind > 0
        ? `@${p.handle} last read #${p.read_seq} · ${p.behind} messages since${p.mentioned ? " · @-mentioned since" : ""}`
        : `@${p.handle} is caught up to #${p.read_seq}`;
      mark.appendChild(title);
      mark.addEventListener("click", (ev) => {
        ev.stopPropagation();
        inspect(channel.name, p.read_seq, p.handle);
      });
      svg.appendChild(mark);
    }
  }

  // drawMentions draws, for the selected message only, a dashed connector
  // from its dot to the lane of every agent it literally @-mentions. @all in
  // a channel message reaches every member of that channel, as the server
  // treats it. It shows the text's mentions, not a claim that anyone owes a
  // reply.
  function drawMentions(svg, e, view, mx, laneY, colors) {
    const x = mx(e.t).toFixed(1);
    const y0 = laneY.get(e.sender);
    for (const handle of mentionTargets(e, view, laneY.keys())) {
      const y1 = laneY.get(handle);
      svg.appendChild(el("line", { class: "tl-mention", x1: x, x2: x, y1: String(y0), y2: String(y1), stroke: colors.get(e.sender) }));
      svg.appendChild(el("circle", { class: "tl-mention-end", cx: x, cy: String(y1), r: "3", stroke: colors.get(e.sender) }));
    }
  }

  // mentionTargets returns the handles among candidates that message e
  // literally @-mentions, other than its sender. The server matches mentions
  // case-insensitively, so @api reaches both @API and @api when both exist.
  // "all" is reserved: it never matches a handle by name and expands only to
  // the members of the message's channel, as the server treats it.
  function mentionTargets(e, view, candidates) {
    const mentioned = new Set(mentionedHandles(e.text));
    const broadcast = mentioned.delete("all");
    const channel = e.pair ? null : (view.channels || []).find((c) => c.name === e.target);
    const members = new Set(broadcast && channel ? (channel.positions || []).map((p) => p.handle) : []);
    const out = [];
    for (const h of candidates) {
      if (h !== e.sender && (mentioned.has(h.toLowerCase()) || members.has(h))) out.push(h);
    }
    return out;
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
  // centered on it. Moving the selection inspects in place; only Enter shows
  // the message in the conversation.
  function onKey(ev, order, isSelected, picked, actions, axis, win, pan) {
    if (ev.key === "Escape") {
      if (!picked) return;
      ev.preventDefault();
      actions.selectMessage(null, 0, { reveal: false });
      return;
    }
    if (ev.key === "PageUp" || ev.key === "PageDown") {
      ev.preventDefault();
      const dur = win.endMs - win.startMs;
      pan(placeWindow(axis, win, win.startMs + (ev.key === "PageUp" ? -dur : dur)));
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
      actions.selectMessage(order[i].target, order[i].seq);
      return;
    } else return;
    ev.preventDefault();
    const e = order[i];
    if (e.t < win.startMs || e.t > win.endMs) pan(centerWindow(axis, win, e.t));
    actions.selectMessage(e.target, e.seq, { reveal: false });
  }

  // duration writes a length of time as "45m", "2h", or "2h 15m".
  function duration(ms) {
    const min = Math.round(ms / 60000);
    if (min < 60) return `${min}m`;
    const h = Math.floor(min / 60);
    return `${h}h${min % 60 ? " " + (min % 60) + "m" : ""}`;
  }

  function clock(ms) {
    return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  function firstLine(text, max) {
    const line = String(text || "").split("\n")[0];
    return line.length > max ? line.slice(0, max - 1) + "…" : line;
  }

  // preview returns the first two lines of text, at most 280 characters.
  function preview(text) {
    const lines = String(text || "").split("\n").slice(0, 2).join("\n");
    return lines.length > 280 ? lines.slice(0, 279) + "…" : lines;
  }

  // html creates an HTML element with an optional class and text.
  function html(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text != null) node.textContent = text;
    return node;
  }

  // button creates a card button named by data-tl-card that runs onClick.
  function button(className, text, name, onClick) {
    const b = html("button", className, text);
    b.type = "button";
    b.dataset.tlCard = name;
    b.addEventListener("click", onClick);
    return b;
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

  window.ChorusTimeline = {
    render,
    isDragging: (rootEl) => drags.has(rootEl),
  };
})();
