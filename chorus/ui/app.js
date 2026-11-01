// Chorus Dashboard Application Controller (State, ETag Long-Polling, Target Tabs, & MCP Composer)
(() => {
  "use strict";

  const state = {
    swarms: [],
    activeSwarm: "",
    view: null,
    pairedDMs: [],
    etag: "",
    lastSyncMs: 0,
    connectionState: "connecting", // "live" | "connecting" | "error"
    retryCountdownSec: 0,
    // selectedTarget:
    //   { kind: "channel", name: "#chorus" }
    //   { kind: "dm", pairKey: "codex↔opus", a: "codex", b: "opus" }
    //   { kind: "all" }
    selectedTarget: null,
    // selectedMessage: null | { target: "#chorus" | "@recipient", seq: number, reader?: string }
    selectedMessage: null,
    // timeWindow: null | { startMs: number, endMs: number }
    timeWindow: null,
    viewMode: "threaded", // "threaded" | "stream"
    expandAllMessages: false,
    filterAgent: "",
    searchQuery: "",
    // A pending reply retains only its destination and original author facts.
    replyTo: null, // null | { target, seq, sender, human }
    pollAbort: null,
  };

  const SVG_NS = "http://www.w3.org/2000/svg";
  const ICON_PATHS = {
    close: ["M18 6 6 18", "m6 6 12 12"],
    reply: ["M9 17 4 12l5-5", "M20 18v-2a4 4 0 0 0-4-4H4"],
    edit: ["M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z", "m15 5 4 4"],
    pin: ["M12 17v5", "M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 1 1 0 0 0 1-1V4a1 1 0 0 0-1-1H8a1 1 0 0 0-1 1v1a1 1 0 0 0 1 1 1 1 0 0 1 1 1z"],
    code: ["m16 18 6-6-6-6", "m8 6-6 6 6 6"],
    check: ["M20 6 9 17l-5-5"],
    copy: ["M8 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2v-2", "M16 4h2a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2Z"],
  };

  function svgIcon(name, className = "icon") {
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("class", className);
    svg.setAttribute("aria-hidden", "true");
    for (const d of ICON_PATHS[name] || ICON_PATHS.close) {
      const p = document.createElementNS(SVG_NS, "path");
      p.setAttribute("d", d);
      svg.appendChild(p);
    }
    return svg;
  }

  function agentColorClass(handle, model) {
    if (model === "human") return "agent-human";
    const h = String(handle || "").toLowerCase();
    let hash = 2166136261;
    for (let i = 0; i < h.length; i++) {
      hash ^= h.charCodeAt(i);
      hash = Math.imul(hash, 16777619);
    }
    return `agent-c${Math.abs(hash) % 8}`;
  }

  function isDarkThemeActive() {
    const attr = document.documentElement.dataset.theme;
    if (attr === "dark") return true;
    if (attr === "light") return false;
    return Boolean(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }

  function agentColorHex(handle, model) {
    const dark = isDarkThemeActive();
    if (model === "human") return dark ? "#fb7185" : "#be123c";
    const palette = dark
      ? ["#60a5fa", "#2dd4bf", "#fbbf24", "#fb7185", "#a5b4fc", "#34d399", "#f472b6", "#22d3ee"]
      : ["#1d4ed8", "#0f766e", "#92400e", "#be123c", "#4338ca", "#047857", "#be185d", "#0e7490"];
    const h = String(handle || "").toLowerCase();
    let hash = 2166136261;
    for (let i = 0; i < h.length; i++) {
      hash ^= h.charCodeAt(i);
      hash = Math.imul(hash, 16777619);
    }
    return palette[Math.abs(hash) % palette.length];
  }

  // resolveIdentity maps a participant's declared model (or saved send-time
  // human provenance) to the official brand mark under /ui/brands/. It matches
  // supported model identifier forms (including Bedrock ARNs and Vertex/provider
  // paths) and never guesses from handle prefixes, roles, or arbitrary substrings.
  function resolveIdentity(handle, model, isHuman = false) {
    const raw = String(handle || "");
    if (isHuman) {
      return { handle: raw, label: `@${raw}`, model: "human", provider: "human", iconUrl: null, avatarUrl: null };
    }
    const reg = (state.view?.agents || []).find((a) => a.handle === raw);
    const mod = String(model !== undefined && model !== "" ? model : reg?.model || "");
    const m = mod.trim().toLowerCase();
    if (m === "human") {
      return { handle: raw, label: `@${raw}`, model: mod, provider: "human", iconUrl: null, avatarUrl: null };
    }
    const cloudPrefix = /^(?:arn:aws(?:-cn|-us-gov)?:bedrock:[a-z0-9-]*:[0-9]*:(?:foundation-model|inference-profile|application-inference-profile|provisioned-model)\/|projects\/[a-z0-9._-]+\/locations\/[a-z0-9._-]+\/publishers\/(?:anthropic|google|openai)\/models\/|(?:anthropic|openai|google|models|vertex_ai)[/.:])?(?:(?:us|eu|ap|apac|global)\.)?(?:(?:anthropic|openai|google)\.)?/;
    const leaf = m.replace(cloudPrefix, "");
    if (/^claude(?:$|[-._:@][a-z0-9._:@-]+)$/.test(leaf)) {
      return { handle: raw, label: `@${raw}`, model: mod, provider: "claude", iconUrl: "/ui/brands/claude.svg", avatarUrl: "/ui/brands/claude.svg" };
    }
    if (/^(?:gpt-[a-z0-9._:@-]+|chatgpt-[a-z0-9._:@-]+|codex(?:$|-[a-z0-9._:@-]+)|o[134](?:$|-(?:mini|pro|preview|[0-9][a-z0-9._:@-]*)))$/.test(leaf)) {
      return { handle: raw, label: `@${raw}`, model: mod, provider: "openai", iconUrl: "/ui/brands/openai.svg", avatarUrl: "/ui/brands/openai.svg" };
    }
    if (/^gemini(?:$|[-._:@][a-z0-9._:@-]+)$/.test(leaf)) {
      return { handle: raw, label: `@${raw}`, model: mod, provider: "gemini", iconUrl: "/ui/brands/gemini.png", avatarUrl: "/ui/brands/gemini.png" };
    }
    return { handle: raw, label: `@${raw}`, model: mod, provider: null, iconUrl: null, avatarUrl: null };
  }

  // handleBadge renders @<handle> with an inline brand mark (no circular border
  // container) when resolveIdentity recognizes the declared model, and sets the
  // tooltip to include the exact declared model when known.
  function handleBadge(handle, model = "", extraClass = "", isHuman = false) {
    const raw = String(handle || "");
    const identity = resolveIdentity(raw, model, isHuman);
    const span = document.createElement("span");
    span.className = `handle-with-logo ${extraClass}`.trim();
    span.title = identity.model ? `@${raw} · ${identity.model}` : `@${raw}`;
    if (identity.iconUrl) {
      const img = document.createElement("img");
      img.className = `inline-brand-logo provider-${identity.provider}`.trim();
      img.dataset.provider = identity.provider || "";
      img.src = identity.iconUrl;
      img.alt = "";
      img.setAttribute("aria-hidden", "true");
      img.addEventListener("error", () => img.remove());
      span.appendChild(img);
    }
    span.appendChild(document.createTextNode(`@${raw}`));
    return span;
  }

  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === "className") {
        node.className = v;
      } else if (k === "dataset") {
        for (const [dk, dv] of Object.entries(v)) {
          node.dataset[dk] = dv;
        }
      } else if (k.startsWith("on") && typeof v === "function") {
        node.addEventListener(k.slice(2).toLowerCase(), v);
      } else if (v === true) {
        node.setAttribute(k, "");
      } else {
        node.setAttribute(k, String(v));
      }
    }
    for (const child of children.flat()) {
      if (child === null || child === undefined || child === false) continue;
      if (child instanceof Node) {
        node.appendChild(child);
      } else {
        node.appendChild(document.createTextNode(String(child)));
      }
    }
    return node;
  }

  function buildPairedDMs(inboxes) {
    const pairMap = new Map();
    const inboxByRecipient = new Map();
    for (const ib of inboxes || []) {
      inboxByRecipient.set(ib.recipient, ib);
    }

    for (const ib of inboxes || []) {
      const recipient = ib.recipient;
      for (const m of ib.messages || []) {
        const sender = m.sender;
        const [a, b] = sender <= recipient ? [sender, recipient] : [recipient, sender];
        const pairKey = `${a}↔${b}`;
        let pair = pairMap.get(pairKey);
        if (!pair) {
          pair = {
            pairKey,
            a,
            b,
            messages: [],
            sinceLastReadByRecipient: new Map(),
          };
          pairMap.set(pairKey, pair);
        }
        pair.messages.push({
          ...m,
          recipient,
          target: `@${recipient}`,
          coord: `@${recipient}#${m.seq}`,
        });
        if (m.seq > ib.read_seq) {
          pair.sinceLastReadByRecipient.set(
            recipient,
            (pair.sinceLastReadByRecipient.get(recipient) || 0) + 1
          );
        }
      }
    }

    const pairs = Array.from(pairMap.values());
    for (const pair of pairs) {
      pair.messages.sort((x, y) => {
        if (x.sent_at_ms !== y.sent_at_ms) return x.sent_at_ms - y.sent_at_ms;
        if (x.seq !== y.seq) return x.seq - y.seq;
        return x.recipient.localeCompare(y.recipient);
      });
      pair.inboxA = inboxByRecipient.get(pair.a) || null;
      pair.inboxB = inboxByRecipient.get(pair.b) || null;
    }
    pairs.sort((x, y) => x.pairKey.localeCompare(y.pairKey));
    return pairs;
  }

  function readHashState() {
    const params = new URLSearchParams(window.location.hash.replace(/^#/, ""));
    return { swarm: params.get("swarm") || "", target: params.get("target") || "", mode: params.get("mode") || "" };
  }

  function writeHashState() {
    const params = new URLSearchParams();
    if (state.activeSwarm) params.set("swarm", state.activeSwarm);
    if (state.selectedTarget?.kind === "channel") params.set("target", state.selectedTarget.name);
    else if (state.selectedTarget?.kind === "dm") params.set("target", `dm:${state.selectedTarget.pairKey}`);
    else if (state.selectedTarget?.kind === "all") params.set("target", "all");
    if (state.viewMode !== "threaded") params.set("mode", state.viewMode);
    const nextHash = `#${params.toString()}`;
    if (window.location.hash !== nextHash) history.replaceState(null, "", nextHash);
  }

  function syncComposerTarget(targetObj, onlyIfNotEdited = false) {
    const sendTarget = document.getElementById("send-target");
    if (!sendTarget || !targetObj) return;
    if (onlyIfNotEdited && sendTarget.dataset.userEdited) return;
    if (!onlyIfNotEdited) delete sendTarget.dataset.userEdited;
    const pinCheck = document.getElementById("send-pin");
    const pinLabel = document.getElementById("pin-toggle-label");
    if (targetObj.kind === "channel") {
      sendTarget.value = targetObj.name;
      pinLabel?.classList.remove("disabled");
    } else if (targetObj.kind === "dm") {
      const myHandle = (document.getElementById("send-handle")?.value || "raphael").trim();
      sendTarget.value = `@${targetObj.a === myHandle ? targetObj.b : targetObj.a}`;
      if (pinCheck) pinCheck.checked = false;
      pinLabel?.classList.add("disabled");
    }
  }

  function resolveTarget(hashTarget, channels, pairedDMs) {
    if (hashTarget === "all") return { kind: "all" };
    if (hashTarget?.startsWith("dm:")) {
      const found = pairedDMs.find((p) => p.pairKey === hashTarget.slice(3));
      if (found) return { kind: "dm", pairKey: found.pairKey, a: found.a, b: found.b };
    }
    if (hashTarget?.startsWith("#") && channels.some((c) => c.name === hashTarget)) {
      return { kind: "channel", name: hashTarget };
    }
    if (channels.length > 0) return { kind: "channel", name: channels[0].name };
    if (pairedDMs.length > 0) {
      const p = pairedDMs[0];
      return { kind: "dm", pairKey: p.pairKey, a: p.a, b: p.b };
    }
    return { kind: "all" };
  }

  function reconcileSelectedTarget(view, pairedDMs) {
    if (!view) return;
    const channels = view.channels || [];
    if (state.selectedTarget) {
      if (state.selectedTarget.kind === "all") return;
      if (state.selectedTarget.kind === "channel" && channels.some((c) => c.name === state.selectedTarget.name)) return;
      if (state.selectedTarget.kind === "dm" && pairedDMs.some((p) => p.pairKey === state.selectedTarget.pairKey)) return;
    }
    state.selectedTarget = resolveTarget(readHashState().target, channels, pairedDMs);
    syncComposerTarget(state.selectedTarget, true);
    writeHashState();
  }

  // findViewMessage locates a message by its channel or inbox target and
  // sequence number inside the current swarm view snapshot.
  function findViewMessage(view, target, seq) {
    if (!view || !target || !seq) return null;
    if (target.startsWith("#")) {
      const ch = (view.channels || []).find((c) => c.name === target);
      return (ch?.messages || []).find((m) => m.seq === seq) || null;
    }
    if (target.startsWith("@")) {
      const recipient = target.slice(1);
      const ib = (view.inboxes || []).find((i) => i.recipient === recipient);
      return (ib?.messages || []).find((m) => m.seq === seq) || null;
    }
    return null;
  }

  // viewTimeBounds returns the earliest and latest message timestamps across
  // all channels and inboxes in the current swarm view snapshot.
  function viewTimeBounds(view) {
    if (!view) return null;
    let first = Infinity;
    let last = -Infinity;
    for (const ch of view.channels || []) {
      for (const m of ch.messages || []) {
        if (m.sent_at_ms < first) first = m.sent_at_ms;
        if (m.sent_at_ms > last) last = m.sent_at_ms;
      }
    }
    for (const ib of view.inboxes || []) {
      for (const m of ib.messages || []) {
        if (m.sent_at_ms < first) first = m.sent_at_ms;
        if (m.sent_at_ms > last) last = m.sent_at_ms;
      }
    }
    return first <= last ? { first, last } : null;
  }

  const actions = {
    selectTarget(targetObj) {
      if (!targetObj) return false;
      const sameAsCurrent = state.selectedTarget && state.selectedTarget.kind === targetObj.kind &&
        (targetObj.kind === "channel" ? state.selectedTarget.name === targetObj.name
          : targetObj.kind === "dm" ? state.selectedTarget.pairKey === targetObj.pairKey : true);
      state.selectedTarget = targetObj;
      if (sameAsCurrent) return false;
      state.selectedMessage = null;
      state.replyTo = null;
      renderReplyChip();
      syncComposerTarget(targetObj, false);
      writeHashState();
      renderAll();
      return true;
    },

    // selectMessage either inspects a timeline point or mark in place
    // (opts.reveal === false: keeps active tab, filters, disclosures, and chart
    // viewport position unchanged) or reveals the message in #message-stream
    // (opts.reveal !== false: switches target tab if needed, moves an existing
    // timeWindow while preserving its duration, clears excluding agent/search
    // filters, and expands and scrolls the message disclosure into view).
    selectMessage(target, seq, opts = {}) {
      if (!target || !seq || seq <= 0) {
        state.selectedMessage = null;
        renderAll({ preserveChartPosition: opts?.reveal === false });
        return;
      }
      state.selectedMessage = {
        target,
        seq,
        ...(opts?.reader ? { reader: opts.reader } : {}),
      };
      if (opts?.reveal === false) {
        renderAll({ revealSelection: false, preserveChartPosition: true });
        return;
      }
      const msg = findViewMessage(state.view, target, seq);
      if (state.selectedTarget?.kind !== "all") {
        let nextTarget = null;
        if (target.startsWith("#")) {
          if (state.selectedTarget?.kind !== "channel" || state.selectedTarget.name !== target) {
            nextTarget = { kind: "channel", name: target };
          }
        } else if (target.startsWith("@") && msg) {
          const recipient = target.slice(1);
          const [a, b] = msg.sender <= recipient ? [msg.sender, recipient] : [recipient, msg.sender];
          const pairKey = `${a}↔${b}`;
          if (state.selectedTarget?.kind !== "dm" || state.selectedTarget.pairKey !== pairKey) {
            nextTarget = { kind: "dm", pairKey, a, b };
          }
        }
        if (nextTarget) {
          state.selectedTarget = nextTarget;
          state.replyTo = null;
          renderReplyChip();
          syncComposerTarget(nextTarget, false);
          writeHashState();
        }
      }
      if (msg) {
        if (state.timeWindow && (msg.sent_at_ms < state.timeWindow.startMs || msg.sent_at_ms > state.timeWindow.endMs)) {
          const bounds = viewTimeBounds(state.view);
          if (bounds) {
            const dur = state.timeWindow.endMs - state.timeWindow.startMs;
            const lastEnd = bounds.last + 60 * 1000;
            const start = Math.max(
              Math.min(bounds.first, lastEnd - dur),
              Math.min(Math.round(msg.sent_at_ms - dur / 2), lastEnd - dur)
            );
            state.timeWindow = { startMs: start, endMs: start + dur };
          }
        }
        if (state.filterAgent) {
          const handle = state.filterAgent;
          const mentioned = [...String(msg.text || "").matchAll(/(^|[^a-zA-Z0-9_-])@([a-zA-Z0-9][a-zA-Z0-9_-]{0,31})(?![a-zA-Z0-9_-])/g)]
            .some((m) => m[2].toLowerCase() === handle.toLowerCase());
          if (msg.sender !== handle && target !== `@${handle}` && !mentioned) {
            state.filterAgent = "";
          }
        }
        if (state.searchQuery) {
          const haystack = `${target} ${msg.sender} #${msg.seq} ${msg.text}`.toLowerCase();
          if (!haystack.includes(state.searchQuery.toLowerCase())) {
            state.searchQuery = "";
            const searchInput = document.getElementById("search-input");
            if (searchInput) searchInput.value = "";
          }
        }
      }
      renderAll({ revealSelection: true });
    },

    setTimeWindow(win) {
      if (!win || typeof win.startMs !== "number" || typeof win.endMs !== "number") state.timeWindow = null;
      else state.timeWindow = { startMs: win.startMs, endMs: win.endMs };
      renderAll({ preserveChartPosition: true });
    },

    jumpToLatest() {
      state.timeWindow = null;
      state.selectedMessage = null;
      const streamEl = document.getElementById("message-stream");
      if (window.ChorusStream && typeof window.ChorusStream.resetDisclosures === "function") {
        window.ChorusStream.resetDisclosures(streamEl, state);
      }
      const stageEl = document.getElementById("stage-container");
      if (stageEl) stageEl.scrollTop = 0;
      renderAll();
      if (stageEl) stageEl.scrollTop = 0;
      renderActiveFiltersBar();
    },

    setFilterAgent(handle) {
      const clean = String(handle || "").replace(/^@/, "");
      state.filterAgent = state.filterAgent === clean ? "" : clean;
      renderAll();
    },

    // setReplyTo preserves the clicked message's saved author facts even after
    // that message leaves the retained snapshot or its handle changes models.
    setReplyTo(channelName, seq, sender, human) {
      state.replyTo = { target: channelName, seq, sender, human };
      if (channelName) {
        const sendTarget = document.getElementById("send-target");
        if (sendTarget) sendTarget.value = channelName;
      }
      renderReplyChip();
      document.getElementById("send-text")?.focus();
    },

    composeDM(handle) {
      const clean = String(handle || "").replace(/^@/, "");
      if (!clean) return;
      state.replyTo = null;
      renderReplyChip();
      const sendTarget = document.getElementById("send-target");
      const sendPin = document.getElementById("send-pin");
      if (sendTarget) sendTarget.value = `@${clean}`;
      if (sendPin) sendPin.checked = false;
      document.getElementById("send-text")?.focus();
    },

    // updatePin sends the editor's exact text to its original channel. It
    // never reads or resets the main message draft, target, or reply fields.
    async updatePin(swarm, channelName, text) {
      const handleInput = document.getElementById("send-handle");
      const agent = (handleInput?.value || "").trim().replace(/^@/, "");
      await publishHumanMessage(swarm, { agent, to: channelName, text, pin: true, wait_sec: 0 });
      if (state.activeSwarm === swarm) startSwarmLoop(swarm);
    },
  };

  function renderReplyChip() {
    const container = document.getElementById("reply-chip-container");
    if (!container) return;
    const reply = state.replyTo;
    if (!reply) {
      container.replaceChildren();
      return;
    }
    container.replaceChildren(
      el("span", { className: "context-pill" },
        svgIcon("reply", "icon icon-sm"), `#${reply.seq}`,
        " (", handleBadge(reply.sender, "", "", reply.human), ")",
        el("button", {
          type: "button", className: "icon-btn icon-btn-sm", "aria-label": "Cancel reply", title: "Cancel reply",
          onClick: () => {
            state.replyTo = null;
            renderReplyChip();
          },
        }, svgIcon("close")))
    );
  }

  function renderLiveStatus() {
    const pill = document.getElementById("live-status");
    const dot = document.getElementById("live-dot");
    const txt = document.getElementById("live-text");
    if (!pill || !dot || !txt) return;
    if (state.connectionState === "error") {
      pill.className = "live-pill reconnecting";
      dot.className = "status-dot warn";
      txt.textContent = state.retryCountdownSec > 0 ? `Reconnecting (${state.retryCountdownSec}s)` : "Reconnecting…";
    } else if (state.connectionState === "connecting") {
      pill.className = "live-pill";
      dot.className = "status-dot warn";
      txt.textContent = "Connecting…";
    } else {
      pill.className = "live-pill";
      dot.className = "status-dot";
      txt.textContent = "Live";
    }
  }

  function renderHeaderAndTabs() {
    const select = document.getElementById("swarm-select");
    const sendBtn = document.getElementById("btn-send");
    if (sendBtn) sendBtn.disabled = !state.activeSwarm;
    const allSwarms = new Set(state.swarms);
    if (state.activeSwarm) allSwarms.add(state.activeSwarm);
    if (allSwarms.size === 0) allSwarms.add("default");
    select.replaceChildren(...Array.from(allSwarms).sort().map((s) => el("option", { value: s, selected: s === state.activeSwarm }, s)));
    renderLiveStatus();

    const channels = state.view?.channels || [];
    const agents = state.view?.agents || [];
    const pairedDMs = state.pairedDMs || [];
    const datalist = document.getElementById("target-suggestions");
    if (datalist) {
      datalist.replaceChildren(
        ...channels.map((c) => el("option", { value: c.name })),
        ...agents.map((a) => el("option", { value: `@${a.handle}` }))
      );
    }

    const tabsEl = document.getElementById("target-tabs");
    if (!tabsEl) return;
    const tabNodes = [];
    for (const ch of channels) {
      const isActive = state.selectedTarget?.kind === "channel" && state.selectedTarget.name === ch.name;
      const msgs = ch.messages || [];
      const count = formatMessageCount(msgs.length, [ch]);
      tabNodes.push(
        el("button", { type: "button", className: `target-tab${isActive ? " active" : ""}`, onClick: () => actions.selectTarget({ kind: "channel", name: ch.name }) },
          el("span", {}, ch.name),
          el("span", { className: "count-pill", title: count.title }, count.text))
      );
    }
    for (const pair of pairedDMs) {
      const isActive = state.selectedTarget?.kind === "dm" && state.selectedTarget.pairKey === pair.pairKey;
      const count = formatMessageCount(pair.messages.length, [pair.inboxA, pair.inboxB]);
      tabNodes.push(
        el("button", { type: "button", className: `target-tab${isActive ? " active" : ""}`, onClick: () => actions.selectTarget({ kind: "dm", pairKey: pair.pairKey, a: pair.a, b: pair.b }) },
          el("span", {}, handleBadge(pair.a), " ↔ ", handleBadge(pair.b)),
          el("span", { className: "count-pill", title: count.title }, count.text))
      );
    }
    let totalMsgs = 0;
    for (const ch of channels) totalMsgs += (ch.messages || []).length;
    for (const p of pairedDMs) totalMsgs += p.messages.length;
    const allCount = formatMessageCount(totalMsgs, [...channels, ...(state.view?.inboxes || [])]);
    tabNodes.push(
      el("button", { type: "button", className: `target-tab${state.selectedTarget?.kind === "all" ? " active" : ""}`, onClick: () => actions.selectTarget({ kind: "all" }) },
        el("span", {}, "All Activity"),
        el("span", { className: "count-pill", title: allCount.title }, allCount.text))
    );
    tabsEl.replaceChildren(...tabNodes);

    renderActiveFiltersBar();
  }

  // formatMessageCount labels a count using its channel or inbox sources.
  // Either inbox of a DM pair may be absent until it receives a message.
  // A stream starting after position 1 may omit earlier messages, so "+"
  // means at least this many. Failed sends can also leave
  // unused positions; the explanation never claims that messages were deleted.
  function formatMessageCount(count, sources) {
    const mayHaveOlder = sources.some((source) => source?.messages?.[0]?.seq > 1);
    return {
      text: `${count}${mayHaveOlder ? "+" : ""}`,
      title: mayHaveOlder ? `Showing ${count} messages. Older messages may not be available.` : "",
    };
  }

  function renderActiveFiltersBar() {
    const filtersBar = document.getElementById("active-filters-bar");
    if (!filtersBar) return;
    const stageEl = document.getElementById("stage-container");
    const scrolledDown = Boolean(stageEl && stageEl.scrollTop > 4);
    const chips = [];
    if (state.filterAgent) {
      chips.push(el("button", { type: "button", className: "chip-btn active", "aria-label": `Clear agent filter @${state.filterAgent}`, onClick: () => actions.setFilterAgent("") },
        "Agent: ", handleBadge(state.filterAgent), svgIcon("close", "icon icon-xs")));
    }
    if (state.timeWindow || scrolledDown) {
      chips.push(el("button", {
        type: "button",
        id: "btn-jump-latest",
        className: "chip-btn active",
        "aria-label": "Jump to latest messages",
        onClick: () => actions.jumpToLatest(),
      }, state.timeWindow ? "Viewing time range · Jump to latest" : "↑ Jump to latest"));
    }
    filtersBar.replaceChildren(...chips);
  }

  function renderAll(opts = {}) {
    state.pairedDMs = buildPairedDMs(state.view?.inboxes || []);
    reconcileSelectedTarget(state.view, state.pairedDMs);
    renderHeaderAndTabs();

    const stageEl = document.getElementById("stage-container");
    const streamEl = document.getElementById("message-stream");
    const timelineRoot = document.getElementById("timeline-root");
    const preserveChart = Boolean(
      opts.preserveChartPosition ||
      (timelineRoot && window.ChorusTimeline && typeof window.ChorusTimeline.isDragging === "function" && window.ChorusTimeline.isDragging(timelineRoot))
    );
    const prevChartEl = preserveChart && timelineRoot
      ? timelineRoot.querySelector(".tl") || timelineRoot
      : null;
    const prevChartTop = prevChartEl ? prevChartEl.getBoundingClientRect().top : null;
    const scrollSnapshot =
      !preserveChart && window.ChorusStream && typeof window.ChorusStream.captureScrollSnapshot === "function"
        ? window.ChorusStream.captureScrollSnapshot(streamEl, stageEl)
        : null;

    if (timelineRoot && window.ChorusTimeline && typeof window.ChorusTimeline.render === "function") {
      window.ChorusTimeline.render(timelineRoot, state, actions);
    }

    if (window.ChorusStream && typeof window.ChorusStream.render === "function") {
      // Clear any previous scroll-height floor when recomputing chart position
      // or when returning to the live stream (state.timeWindow === null) so
      // normal feed height is restored while active time-window views keep
      // their scroll floor across passive background polls.
      if (streamEl && (preserveChart || !state.timeWindow)) {
        streamEl.style.minHeight = "";
      }
      window.ChorusStream.render(
        {
          scrollEl: stageEl,
          agentStripEl: document.getElementById("agent-strip"),
          detailsEl: document.getElementById("agent-details"),
          pinEl: document.getElementById("pin-container"),
          mentionsEl: document.getElementById("mentions-container"),
          streamEl,
          titleEl: document.getElementById("active-title"),
          subtitleEl: document.getElementById("active-subtitle"),
          scrollSnapshot,
          preserveScroll: preserveChart,
          revealSelection: Boolean(opts.revealSelection),
        },
        state,
        actions
      );
    }
    // Keep the timeline SVG at the exact same viewport Y (prevChartTop) during
    // time-window changes and active pointer drags. When narrowing the time
    // window filters out enough message rows below the timeline that
    // stageEl.scrollHeight would fall below clientHeight + targetScrollTop, the
    // browser would clamp scrollTop and pull the chart downward; setting
    // streamEl.style.minHeight supplies the missing scroll height for the
    // duration of the active timeWindow.
    if (preserveChart && prevChartTop !== null && stageEl && timelineRoot) {
      const nextChartEl = timelineRoot.querySelector(".tl") || timelineRoot;
      const unscrolledChartTop = nextChartEl.getBoundingClientRect().top + stageEl.scrollTop;
      const targetScrollTop = Math.max(0, Math.round(unscrolledChartTop - prevChartTop));
      const neededScrollHeight = stageEl.clientHeight + targetScrollTop;
      // scrollHeight is never smaller than clientHeight, so it hides how much
      // room is missing when the actual content fits inside the viewport.
      // Measure that content and the stage's own padding instead.
      const stageContent = stageEl.querySelector(".stage-inner");
      const stageStyle = getComputedStyle(stageEl);
      const contentHeight = stageContent.getBoundingClientRect().height
        + parseFloat(stageStyle.paddingTop) + parseFloat(stageStyle.paddingBottom);
      if (streamEl && contentHeight < neededScrollHeight) {
        const streamHeight = streamEl.getBoundingClientRect().height;
        streamEl.style.minHeight = `${Math.ceil(streamHeight + neededScrollHeight - contentHeight)}px`;
      }
      stageEl.scrollTop = targetScrollTop;
    }
    renderActiveFiltersBar();
    window.ChorusTimeline?.placePreview?.(timelineRoot);
  }

  // loadedUIBuild is the embedded UI digest stamped into index.html when this
  // tab loaded. When a later server response reports a different X-Chorus-UI-Build
  // header, the quiet update banner is shown so the reader can reload manually
  // without losing an open composer draft or timeline selection.
  const loadedUIBuild = (document.querySelector('meta[name="chorus-ui-build"]')?.content || "").trim();

  function checkUIBuild(res) {
    if (!loadedUIBuild || !res?.headers) return;
    const serverBuild = (res.headers.get("X-Chorus-UI-Build") || "").trim();
    const banner = document.getElementById("update-banner");
    if (banner && serverBuild) {
      banner.hidden = serverBuild === loadedUIBuild;
    }
  }

  async function fetchSwarms() {
    const res = await fetch("/swarms", { headers: { Accept: "application/json" } });
    checkUIBuild(res);
    if (!res.ok) throw new Error(`GET /swarms HTTP ${res.status}`);
    const data = await res.json();
    state.swarms = Array.isArray(data.swarms) ? data.swarms : [];
    return state.swarms;
  }

  // Long-poll loop for GET /swarms/{swarm}/view with single-source ETag header
  // validation and bounded 1s -> 30s error backoff.
  async function startSwarmLoop(swarmName) {
    if (state.pollAbort) {
      state.pollAbort.abort();
    }
    const abortCtrl = new AbortController();
    state.pollAbort = abortCtrl;
    const prevSwarm = state.activeSwarm;
    const switchedSwarm = Boolean(prevSwarm) && prevSwarm !== swarmName;
    state.activeSwarm = swarmName;
    state.etag = "";
    state.connectionState = "connecting";
    state.retryCountdownSec = 0;
    if (switchedSwarm) {
      state.replyTo = null;
      renderReplyChip();
      state.view = null;
      const stageEl = document.getElementById("stage-container");
      if (stageEl) stageEl.scrollTop = 0;
      renderAll();
    } else {
      renderLiveStatus();
    }

    let retryDelaySec = 1;
    while (!abortCtrl.signal.aborted) {
      try {
        const waitSec = state.etag ? 25 : 0;
        const headers = { Accept: "application/json" };
        if (state.etag) {
          headers["If-None-Match"] = state.etag;
        }
        const url = `/swarms/${encodeURIComponent(swarmName)}/view?wait=${waitSec}`;
        const res = await fetch(url, { headers, signal: abortCtrl.signal });
        checkUIBuild(res);

        if (res.status === 304) {
          state.lastSyncMs = Date.now();
          state.connectionState = "live";
          state.retryCountdownSec = 0;
          retryDelaySec = 1;
          renderLiveStatus();
          const prevSwarms = state.swarms.join(",");
          try {
            const next = await fetchSwarms();
            if (next.join(",") !== prevSwarms) renderAll();
          } catch {
            // Ignore background list error when view poll succeeded
          }
          continue;
        }

        if (!res.ok) {
          throw new Error(`HTTP ${res.status}`);
        }

        const newEtag = (res.headers.get("ETag") || "").trim();
        const viewData = await res.json();
        if (!newEtag) {
          throw new Error("Missing ETag header");
        }
        state.etag = newEtag;
        state.view = viewData;
        state.lastSyncMs = Date.now();
        state.connectionState = "live";
        state.retryCountdownSec = 0;
        retryDelaySec = 1;

        try {
          await fetchSwarms();
        } catch {
          // Render view even if swarm list refresh fails
        }
        renderAll();
      } catch (err) {
        if (abortCtrl.signal.aborted) return;
        state.connectionState = "error";
        for (let rem = retryDelaySec; rem > 0; rem--) {
          if (abortCtrl.signal.aborted) return;
          state.retryCountdownSec = rem;
          renderLiveStatus();
          await new Promise((r) => setTimeout(r, 1000));
        }
        retryDelaySec = Math.min(30, retryDelaySec * 2);
      }
    }
  }

  async function callMCPTool(toolName, args) {
    const res = await fetch("/mcp", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Accept": "application/json",
        "MCP-Protocol-Version": "2026-07-28",
        "Mcp-Method": "tools/call",
        "Mcp-Name": toolName,
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: Date.now(),
        method: "tools/call",
        params: {
          _meta: {
            "io.modelcontextprotocol/protocolVersion": "2026-07-28",
            "io.modelcontextprotocol/clientCapabilities": {},
            "io.modelcontextprotocol/clientInfo": {
              name: "chorus-dashboard",
              version: "1",
            },
          },
          name: toolName,
          arguments: args,
        },
      }),
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(`POST /mcp HTTP ${res.status}: ${text}`);
    }
    const payload = await res.json();
    if (payload.error) {
      throw new Error(payload.error.message || JSON.stringify(payload.error));
    }
    if (payload.result?.isError) {
      const errText = (payload.result.content || []).map((c) => c.text).join("\n");
      throw new Error(errText || "MCP tool returned error");
    }
    return (payload.result?.content || []).map((c) => c.text).join("\n");
  }

  function setComposerFeedback(msg, kind = "") {
    const wrap = document.getElementById("composer-feedback");
    const txt = document.getElementById("composer-status-text");
    if (!wrap || !txt) return;
    wrap.className = `composer-feedback${kind ? ` ${kind}` : ""}`;
    const parts = Array.isArray(msg) ? msg : msg ? [msg] : [];
    txt.replaceChildren(
      ...parts.map((p) => (p instanceof Node ? p : document.createTextNode(String(p))))
    );
    clearTimeout(setComposerFeedback._timer);
    if (parts.length > 0 && kind === "ok") {
      setComposerFeedback._timer = setTimeout(() => {
        wrap.className = "composer-feedback";
        txt.replaceChildren();
      }, 4000);
    }
  }

  // publishHumanMessage validates the chosen human against the loaded swarm,
  // joins the target when necessary, and publishes through the existing MCP
  // send tool. Both forms supply their own captured target and original text.
  async function publishHumanMessage(swarm, args) {
    const { agent, to, text } = args;
    if (!swarm) throw new Error("Select a swarm before sending.");
    if (state.activeSwarm !== swarm || state.view?.swarm !== swarm) {
      throw new Error(`Return to swarm ${swarm} and wait for it to load before sending.`);
    }
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(agent)) {
      throw new Error("Enter your handle in the bottom bar (1–32 letters, digits, dashes, or underscores).");
    }
    if (!/^(#|@)[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(to)) {
      throw new Error("Target must start with #channel or @agent.");
    }
    if (!text.trim()) throw new Error("Enter text before sending.");
    const existingAgent = state.view.agents.find((a) => a.handle === agent);
    if (existingAgent && existingAgent.model !== "human") {
      if (!existingAgent.model) {
        throw new Error(
          `@${existingAgent.handle} has not declared a model in this swarm; if this is your handle, call who with swarm="${swarm}", agent="${existingAgent.handle}", and model="human" (see Tool Reference in README), or pick another handle.`
        );
      }
      throw new Error(`@${existingAgent.handle} is an agent in this swarm; pick your own handle.`);
    }
    localStorage.setItem("chorus_handle", agent);
    if (to.startsWith("#")) {
      const channel = state.view.channels.find((item) => item.name === to);
      const alreadyMember = existingAgent && channel?.positions.some((p) => p.handle === agent);
      if (!alreadyMember) {
        await callMCPTool("join", { swarm, agent, model: "human", role: "human", channels: [to], tail: 1 });
      }
    } else if (!existingAgent) {
      await callMCPTool("who", { swarm, agent, model: "human", role: "human" });
    }
    await callMCPTool("send", args);
  }

  // sendMessage reads and resets only the message composer; pin editing owns
  // a different form and calls the same registration/publication function.
  async function sendMessage() {
    const handleInput = document.getElementById("send-handle");
    const targetInput = document.getElementById("send-target");
    const textInput = document.getElementById("send-text");
    const pinCheck = document.getElementById("send-pin");
    const sendBtn = document.getElementById("btn-send");

    const agent = (handleInput.value || "").trim().replace(/^@/, "");
    const to = (targetInput.value || "").trim();
    const text = (textInput.value || "").trim();
    const pin = Boolean(pinCheck.checked && to.startsWith("#"));
    const swarm = state.activeSwarm;

    sendBtn.disabled = true;
    setComposerFeedback("");
    try {
      const sendArgs = { agent, to, text, pin, wait_sec: 0 };
      if (state.replyTo && to === state.replyTo.target) sendArgs.reply_to = state.replyTo.seq;
      await publishHumanMessage(swarm, sendArgs);

      textInput.value = "";
      pinCheck.checked = false;
      state.replyTo = null;
      renderReplyChip();
      if (to.startsWith("@")) setComposerFeedback(["Sent to ", handleBadge(to.slice(1))], "ok");
      else setComposerFeedback(pin ? `Pinned in ${to}` : `Sent to ${to}`, "ok");
      if (state.activeSwarm === swarm) startSwarmLoop(swarm);
    } catch (err) {
      setComposerFeedback(err.message || "Failed to send message.", "error");
    } finally {
      sendBtn.disabled = false;
    }
  }

  function applyTheme(theme) {
    if (theme === "dark" || theme === "light") {
      document.documentElement.dataset.theme = theme;
    } else {
      delete document.documentElement.dataset.theme;
    }
  }

  function initEvents() {
    const savedHandle = localStorage.getItem("chorus_handle");
    if (savedHandle) {
      document.getElementById("send-handle").value = savedHandle;
    }

    const savedTheme = localStorage.getItem("chorus_theme") || "auto";
    const themeSelect = document.getElementById("theme-select");
    themeSelect.value = savedTheme;
    applyTheme(savedTheme);
    themeSelect.addEventListener("change", () => {
      applyTheme(themeSelect.value);
      localStorage.setItem("chorus_theme", themeSelect.value);
      renderAll();
    });
    if (window.matchMedia) {
      window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
        if (!document.documentElement.dataset.theme) renderAll();
      });
    }

    document.getElementById("swarm-select").addEventListener("change", (e) => {
      const next = e.target.value;
      if (next && next !== state.activeSwarm) {
        state.selectedTarget = null;
        state.selectedMessage = null;
        state.timeWindow = null;
        startSwarmLoop(next);
        writeHashState();
      }
    });

    document.getElementById("search-input").addEventListener("input", (e) => {
      state.searchQuery = e.target.value || "";
      renderAll();
    });

    document.getElementById("mode-threaded").addEventListener("click", () => {
      state.viewMode = "threaded";
      document.getElementById("mode-threaded").classList.add("active");
      document.getElementById("mode-stream").classList.remove("active");
      writeHashState();
      renderAll();
    });

    document.getElementById("mode-stream").addEventListener("click", () => {
      state.viewMode = "stream";
      document.getElementById("mode-stream").classList.add("active");
      document.getElementById("mode-threaded").classList.remove("active");
      writeHashState();
      renderAll();
    });

    document.getElementById("btn-toggle-disclosure").addEventListener("click", (e) => {
      state.expandAllMessages = !state.expandAllMessages;
      if (!state.expandAllMessages) {
        state.selectedMessage = null;
      }
      e.currentTarget.textContent = state.expandAllMessages ? "Collapse All" : "Expand All";
      renderAll();
    });

    document.getElementById("send-target").addEventListener("input", (e) => {
      e.currentTarget.dataset.userEdited = "true";
      const val = e.currentTarget.value.trim();
      if (state.replyTo && val !== state.replyTo.target) {
        state.replyTo = null;
        renderReplyChip();
      }
      const pinLabel = document.getElementById("pin-toggle-label");
      if (pinLabel) {
        pinLabel.classList.toggle("disabled", !val.startsWith("#"));
      }
    });

    const sendText = document.getElementById("send-text");
    sendText.addEventListener("keydown", (e) => {
      if (e.isComposing) return;
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        sendMessage();
      }
    });

    document.getElementById("btn-send").addEventListener("click", sendMessage);
    document.getElementById("btn-reload-ui")?.addEventListener("click", () => {
      window.location.reload();
    });

    const stageEl = document.getElementById("stage-container");
    if (stageEl) {
      stageEl.addEventListener(
        "scroll",
        () => {
          const hasJumpBtn = Boolean(document.getElementById("btn-jump-latest"));
          const needsJumpBtn = Boolean(state.timeWindow) || stageEl.scrollTop > 4;
          if (hasJumpBtn !== needsJumpBtn) {
            renderActiveFiltersBar();
          }
        },
        { passive: true }
      );
    }

    window.addEventListener("resize", () => {
      const timelineRoot = document.getElementById("timeline-root");
      if (timelineRoot && window.ChorusTimeline && typeof window.ChorusTimeline.render === "function") {
        window.ChorusTimeline.render(timelineRoot, state, actions);
      }
    });

    window.addEventListener("hashchange", () => {
      const h = readHashState();
      const nextMode = h.mode === "stream" ? "stream" : "threaded";
      const modeChanged = state.viewMode !== nextMode;
      if (modeChanged) {
        state.viewMode = nextMode;
        document.getElementById("mode-stream")?.classList.toggle("active", nextMode === "stream");
        document.getElementById("mode-threaded")?.classList.toggle("active", nextMode !== "stream");
      }
      if (h.swarm && h.swarm !== state.activeSwarm) {
        state.selectedTarget = null;
        state.selectedMessage = null;
        state.timeWindow = null;
        startSwarmLoop(h.swarm);
        return;
      }
      if (state.view) {
        const nextTarget = resolveTarget(h.target, state.view.channels || [], state.pairedDMs);
        if (!actions.selectTarget(nextTarget)) {
          writeHashState();
          if (modeChanged) renderAll();
        }
      }
    });
  }

  window.ChorusUI = {
    state,
    actions,
    el,
    agentColorClass,
    agentColorHex,
    resolveIdentity,
    handleBadge,
    svgIcon,
    formatMessageCount,
  };

  async function boot() {
    initEvents();
    const initialMode = readHashState().mode;
    if (initialMode === "stream") {
      state.viewMode = "stream";
      document.getElementById("mode-stream").classList.add("active");
      document.getElementById("mode-threaded").classList.remove("active");
    }
    try {
      const swarms = await fetchSwarms();
      if (!state.pollAbort) {
        const currentHash = readHashState();
        startSwarmLoop(currentHash.swarm || swarms[0] || "default");
      }
    } catch {
      if (!state.pollAbort) {
        const currentHash = readHashState();
        startSwarmLoop(currentHash.swarm || "default");
      }
    }
  }

  boot();
})();
