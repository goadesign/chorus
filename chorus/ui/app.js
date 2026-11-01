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
    // selectedMessage: null | { target: "#chorus" | "@recipient", seq: number }
    selectedMessage: null,
    // timeWindow: null | { startMs: number, endMs: number }
    timeWindow: null,
    followLive: true,
    viewMode: "threaded", // "threaded" | "stream"
    expandAllMessages: false,
    filterAgent: "",
    filterArtifact: "",
    searchQuery: "",
    // lastSeenSeqByTarget: { [target]: number } for activeSwarm
    lastSeenSeqByTarget: {},
    replyToSeq: 0,
    replyToTarget: "",
    replyToSender: "",
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
    arrowUp: ["m5 12 7-7 7 7", "M12 19V5"],
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

  function agentColorHex(handle, model) {
    if (model === "human") return "#F43F5E";
    const palette = ["#3B82F6", "#14B8A6", "#F59E0B", "#F43F5E", "#6366F1", "#10B981", "#EC4899", "#06B6D4"];
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
    const mod = String(model !== undefined && model !== "" ? model : reg?.model || "").trim();
    const m = mod.toLowerCase();
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

  function loadLastSeenMap(swarm) {
    if (!swarm) return {};
    try {
      const raw = localStorage.getItem(`chorus_seen:${swarm}`);
      const parsed = raw ? JSON.parse(raw) : null;
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch { return {}; }
  }

  function saveLastSeenMap(swarm, map) {
    if (!swarm) return;
    try { localStorage.setItem(`chorus_seen:${swarm}`, JSON.stringify(map)); } catch {}
  }

  function initializeBaselineSeenIfEmpty(view) {
    if (!view || !state.activeSwarm) return;
    const map = state.lastSeenSeqByTarget;
    let changed = false;
    for (const ch of view.channels || []) {
      if (map[ch.name] === undefined) {
        const msgs = ch.messages || [];
        map[ch.name] = msgs.length > 0 ? msgs[msgs.length - 1].seq : ch.head_seq || 0;
        changed = true;
      }
    }
    for (const ib of view.inboxes || []) {
      const key = `@${ib.recipient}`;
      if (map[key] === undefined) {
        map[key] = ib.head_seq || 0;
        changed = true;
      }
    }
    if (changed) saveLastSeenMap(state.activeSwarm, map);
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

  function reconcileSelectedTarget(view, pairedDMs) {
    if (!view) return;
    const channels = view.channels || [];
    if (state.selectedTarget) {
      if (state.selectedTarget.kind === "all") return;
      if (state.selectedTarget.kind === "channel" && channels.some((c) => c.name === state.selectedTarget.name)) return;
      if (state.selectedTarget.kind === "dm" && pairedDMs.some((p) => p.pairKey === state.selectedTarget.pairKey)) return;
    }
    const hash = readHashState();
    if (hash.target === "all") { state.selectedTarget = { kind: "all" }; return; }
    if (hash.target.startsWith("dm:")) {
      const found = pairedDMs.find((p) => p.pairKey === hash.target.slice(3));
      if (found) { state.selectedTarget = { kind: "dm", pairKey: found.pairKey, a: found.a, b: found.b }; return; }
    }
    if (hash.target.startsWith("#") && channels.some((c) => c.name === hash.target)) {
      state.selectedTarget = { kind: "channel", name: hash.target }; return;
    }
    if (channels.length > 0) {
      state.selectedTarget = { kind: "channel", name: channels[0].name };
      const sendTarget = document.getElementById("send-target");
      if (sendTarget && !sendTarget.dataset.userEdited) sendTarget.value = channels[0].name;
      return;
    }
    if (pairedDMs.length > 0) {
      const p = pairedDMs[0];
      state.selectedTarget = { kind: "dm", pairKey: p.pairKey, a: p.a, b: p.b };
      return;
    }
    state.selectedTarget = { kind: "all" };
  }

  const actions = {
    selectTarget(targetObj) {
      if (!targetObj) return;
      const sameAsCurrent = state.selectedTarget && state.selectedTarget.kind === targetObj.kind &&
        (targetObj.kind === "channel" ? state.selectedTarget.name === targetObj.name
          : targetObj.kind === "dm" ? state.selectedTarget.pairKey === targetObj.pairKey : true);
      state.selectedTarget = targetObj;
      if (sameAsCurrent) return;
      state.selectedMessage = null;
      state.replyToSeq = 0;
      renderReplyChip();
      const sendTarget = document.getElementById("send-target");
      const pinCheck = document.getElementById("send-pin");
      if (targetObj.kind === "channel" && sendTarget) {
        sendTarget.value = targetObj.name;
      } else if (targetObj.kind === "dm" && sendTarget) {
        const myHandle = (document.getElementById("send-handle")?.value || "raphael").trim();
        sendTarget.value = `@${targetObj.a === myHandle ? targetObj.b : targetObj.a}`;
        if (pinCheck) pinCheck.checked = false;
      }
      writeHashState();
      renderAll();
    },

    selectMessage(target, seq) {
      if (!target || !seq || seq <= 0) state.selectedMessage = null;
      else { state.selectedMessage = { target, seq }; state.followLive = false; }
      renderAll();
    },

    setTimeWindow(win) {
      if (!win || typeof win.startMs !== "number" || typeof win.endMs !== "number") state.timeWindow = null;
      else { state.timeWindow = { startMs: win.startMs, endMs: win.endMs }; state.followLive = false; }
      renderAll();
    },

    setFollowLive(follow) {
      state.followLive = Boolean(follow);
      if (state.followLive) {
        state.timeWindow = null;
        state.selectedMessage = null;
        const stageEl = document.getElementById("stage-container");
        if (stageEl) stageEl.scrollTop = 0;
      }
      renderAll();
    },

    setFilterAgent(handle) {
      const clean = String(handle || "").replace(/^@/, "");
      state.filterAgent = state.filterAgent === clean ? "" : clean;
      renderAll();
    },

    setFilterArtifact(artifact) {
      const clean = String(artifact || "");
      state.filterArtifact = state.filterArtifact === clean ? "" : clean;
      renderAll();
    },

    setReplyTo(channelName, seq, sender) {
      state.replyToSeq = Number(seq) || 0;
      state.replyToTarget = channelName || "";
      state.replyToSender = sender || "";
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
      state.replyToSeq = 0;
      state.replyToTarget = "";
      state.replyToSender = "";
      renderReplyChip();
      const sendTarget = document.getElementById("send-target");
      const sendPin = document.getElementById("send-pin");
      if (sendTarget) sendTarget.value = `@${clean}`;
      if (sendPin) sendPin.checked = false;
      document.getElementById("send-text")?.focus();
    },

    async updatePin(channelName, text) {
      const handleInput = document.getElementById("send-handle");
      const agent = (handleInput?.value || "").trim().replace(/^@/, "");
      const cleanText = (text || "").trim();
      const swarm = state.activeSwarm;
      if (!swarm) throw new Error("Select a swarm before updating the pin.");
      if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(agent)) {
        handleInput?.focus();
        throw new Error("Enter your handle in the bottom bar (1–32 letters, digits, dashes, or underscores) to update the pin.");
      }
      if (!cleanText) throw new Error("Pin text cannot be empty.");
      const existingAgent = (state.view?.agents || []).find((a) => a.handle === agent);
      if (existingAgent && existingAgent.model !== "human") {
        throw new Error(`@${existingAgent.handle} is an agent in this swarm; pick your own handle.`);
      }
      localStorage.setItem("chorus_handle", agent);
      const ch = (state.view?.channels || []).find((c) => c.name === channelName);
      const alreadyMember = Boolean(existingAgent) && existingAgent.model === "human" && Boolean(ch) && (ch.positions || []).some((p) => p.handle === agent);
      if (!alreadyMember) {
        await callMCPTool("join", { swarm, agent, model: "human", role: "human", channels: [channelName], tail: 1 });
      }
      await callMCPTool("send", { agent, to: channelName, text: cleanText, pin: true, wait_sec: 0 });
      if (state.activeSwarm === swarm) startSwarmLoop(swarm);
    },

    markSeen(target, seq) {
      if (target && seq > 0) {
        state.lastSeenSeqByTarget[target] = seq;
      } else if (state.selectedTarget?.kind === "channel") {
        const ch = (state.view?.channels || []).find((c) => c.name === state.selectedTarget.name);
        if (ch) {
          const msgs = ch.messages || [];
          state.lastSeenSeqByTarget[ch.name] = msgs.length > 0 ? msgs[msgs.length - 1].seq : ch.head_seq || 0;
        }
      } else if (state.selectedTarget?.kind === "dm") {
        const pair = state.pairedDMs.find((p) => p.pairKey === state.selectedTarget.pairKey);
        if (pair) {
          if (pair.inboxA) state.lastSeenSeqByTarget[`@${pair.inboxA.recipient}`] = pair.inboxA.head_seq || 0;
          if (pair.inboxB) state.lastSeenSeqByTarget[`@${pair.inboxB.recipient}`] = pair.inboxB.head_seq || 0;
        }
      }
      saveLastSeenMap(state.activeSwarm, state.lastSeenSeqByTarget);
      renderAll();
    },
  };

  function renderReplyChip() {
    const container = document.getElementById("reply-chip-container");
    if (!container) return;
    if (!state.replyToSeq || state.replyToSeq <= 0) {
      container.replaceChildren();
      return;
    }
    container.replaceChildren(
      el("span", { className: "context-pill" },
        svgIcon("reply", "icon icon-sm"), `#${state.replyToSeq}`,
        ...(state.replyToSender ? [" (", handleBadge(state.replyToSender), ")"] : []),
        el("button", {
          type: "button", className: "icon-btn icon-btn-sm", "aria-label": "Cancel reply", title: "Cancel reply",
          onClick: () => {
            state.replyToSeq = 0;
            state.replyToTarget = "";
            state.replyToSender = "";
            renderReplyChip();
          },
        }, svgIcon("close")))
    );
  }

  function renderLiveStatus() {
    const btn = document.getElementById("btn-follow-live");
    const dot = document.getElementById("live-dot");
    const txt = document.getElementById("live-text");
    if (!btn || !dot || !txt) return;
    if (state.connectionState === "error") {
      btn.className = "live-pill reconnecting";
      dot.className = "status-dot warn";
      txt.textContent = state.retryCountdownSec > 0 ? `Reconnecting (${state.retryCountdownSec}s)` : "Reconnecting…";
    } else if (state.connectionState === "connecting") {
      btn.className = "live-pill";
      dot.className = "status-dot warn";
      txt.textContent = "Connecting…";
    } else if (!state.followLive) {
      btn.className = "live-pill reviewing";
      dot.className = "status-dot warn";
      txt.textContent = "History view · Return to live";
    } else {
      btn.className = "live-pill";
      dot.className = "status-dot";
      txt.textContent = "Live";
    }
  }

  function renderHeaderAndTabs() {
    const select = document.getElementById("swarm-select");
    const sendBtn = document.getElementById("btn-send");
    if (sendBtn) sendBtn.disabled = !state.activeSwarm;
    const allSwarms = new Set(["default", ...state.swarms]);
    if (state.activeSwarm) allSwarms.add(state.activeSwarm);
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
      const seenSeq = state.lastSeenSeqByTarget[ch.name];
      const msgs = ch.messages || [];
      const headSeq = msgs.length > 0 ? msgs[msgs.length - 1].seq : ch.head_seq || 0;
      const newCount = seenSeq !== undefined ? Math.max(0, headSeq - seenSeq) : 0;
      tabNodes.push(
        el("button", { type: "button", className: `target-tab${isActive ? " active" : ""}`, onClick: () => actions.selectTarget({ kind: "channel", name: ch.name }) },
          el("span", {}, ch.name),
          el("span", { className: `count-pill${newCount > 0 ? " attention" : ""}` }, newCount > 0 ? `+${newCount}` : String(msgs.length)))
      );
    }
    for (const pair of pairedDMs) {
      const isActive = state.selectedTarget?.kind === "dm" && state.selectedTarget.pairKey === pair.pairKey;
      tabNodes.push(
        el("button", { type: "button", className: `target-tab${isActive ? " active" : ""}`, onClick: () => actions.selectTarget({ kind: "dm", pairKey: pair.pairKey, a: pair.a, b: pair.b }) },
          el("span", {}, handleBadge(pair.a), " ↔ ", handleBadge(pair.b)),
          el("span", { className: "count-pill" }, String(pair.messages.length)))
      );
    }
    let totalMsgs = 0;
    for (const ch of channels) totalMsgs += (ch.messages || []).length;
    for (const p of pairedDMs) totalMsgs += p.messages.length;
    tabNodes.push(
      el("button", { type: "button", className: `target-tab${state.selectedTarget?.kind === "all" ? " active" : ""}`, onClick: () => actions.selectTarget({ kind: "all" }) },
        el("span", {}, "All Activity"),
        el("span", { className: "count-pill" }, String(totalMsgs)))
    );
    tabsEl.replaceChildren(...tabNodes);

    const filtersBar = document.getElementById("active-filters-bar");
    if (filtersBar) {
      const chips = [];
      if (state.filterAgent) {
        chips.push(el("button", { type: "button", className: "chip-btn active", "aria-label": `Clear agent filter @${state.filterAgent}`, onClick: () => actions.setFilterAgent("") },
          "Agent: ", handleBadge(state.filterAgent), svgIcon("close", "icon icon-xs")));
      }
      if (state.filterArtifact) {
        chips.push(el("button", { type: "button", className: "chip-btn active", "aria-label": `Clear artifact filter ${state.filterArtifact}`, onClick: () => actions.setFilterArtifact("") },
          `Artifact: ${state.filterArtifact}`, svgIcon("close", "icon icon-xs")));
      }
      if (state.timeWindow) {
        chips.push(el("button", { type: "button", className: "chip-btn active", "aria-label": "Clear time window filter", onClick: () => actions.setTimeWindow(null) },
          "Time window active", svgIcon("close", "icon icon-xs")));
      }
      filtersBar.replaceChildren(...chips);
    }
  }

  function renderAll() {
    state.pairedDMs = buildPairedDMs(state.view?.inboxes || []);
    reconcileSelectedTarget(state.view, state.pairedDMs);
    renderHeaderAndTabs();

    const stageEl = document.getElementById("stage-container");
    const streamEl = document.getElementById("message-stream");
    const scrollSnapshot =
      window.ChorusStream && typeof window.ChorusStream.captureScrollSnapshot === "function"
        ? window.ChorusStream.captureScrollSnapshot(streamEl, stageEl)
        : null;

    const timelineRoot = document.getElementById("timeline-root");
    if (timelineRoot && window.ChorusTimeline && typeof window.ChorusTimeline.render === "function") {
      window.ChorusTimeline.render(timelineRoot, state, actions);
    }

    if (window.ChorusStream && typeof window.ChorusStream.render === "function") {
      window.ChorusStream.render(
        {
          scrollEl: stageEl,
          agentStripEl: document.getElementById("agent-strip"),
          detailsEl: document.getElementById("agent-details"),
          pinEl: document.getElementById("pin-container"),
          mentionsEl: document.getElementById("mentions-artifacts-container"),
          streamEl,
          newSinceEl: document.getElementById("new-since-banner"),
          titleEl: document.getElementById("active-title"),
          subtitleEl: document.getElementById("active-subtitle"),
          scrollSnapshot,
        },
        state,
        actions
      );
    }
  }

  async function fetchSwarms() {
    const res = await fetch("/swarms", { headers: { Accept: "application/json" } });
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
    state.lastSeenSeqByTarget = loadLastSeenMap(swarmName);
    writeHashState();
    if (switchedSwarm) {
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

        initializeBaselineSeenIfEmpty(viewData);

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

    if (!swarm) { setComposerFeedback("Select a swarm before sending.", "error"); return; }
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(agent)) {
      setComposerFeedback("Handle must be 1–32 letters, digits, dashes, or underscores.", "error"); return;
    }
    if (!/^(#|@)[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(to)) {
      setComposerFeedback("Target must start with #channel or @agent.", "error"); return;
    }
    if (!text) { setComposerFeedback("Enter a message before sending.", "error"); return; }

    const existingAgent = (state.view?.agents || []).find((a) => a.handle === agent);
    if (existingAgent && existingAgent.model !== "human") {
      setComposerFeedback([handleBadge(existingAgent.handle, existingAgent.model), " is an agent in this swarm; pick your own handle."], "error");
      return;
    }

    localStorage.setItem("chorus_handle", agent);
    sendBtn.disabled = true;
    setComposerFeedback("");
    try {
      if (to.startsWith("#")) {
        const ch = (state.view?.channels || []).find((c) => c.name === to);
        const alreadyMember = Boolean(existingAgent) && existingAgent.model === "human" && Boolean(ch) && (ch.positions || []).some((p) => p.handle === agent);
        if (!alreadyMember) {
          await callMCPTool("join", { swarm, agent, model: "human", role: "human", channels: [to], tail: 1 });
        }
      } else if (!existingAgent || existingAgent.model !== "human") {
        await callMCPTool("who", { swarm, agent, model: "human", role: "human" });
      }
      const sendArgs = { agent, to, text, pin, wait_sec: 0 };
      if (state.replyToSeq > 0 && to.startsWith("#")) sendArgs.reply_to = state.replyToSeq;
      await callMCPTool("send", sendArgs);

      textInput.value = "";
      pinCheck.checked = false;
      state.replyToSeq = 0;
      state.replyToTarget = "";
      state.replyToSender = "";
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
    });

    document.getElementById("swarm-select").addEventListener("change", (e) => {
      const next = e.target.value;
      if (next && next !== state.activeSwarm) {
        state.selectedTarget = null;
        state.selectedMessage = null;
        state.timeWindow = null;
        state.followLive = true;
        startSwarmLoop(next);
      }
    });

    document.getElementById("btn-custom-swarm").addEventListener("click", () => {
      const name = window.prompt("Enter swarm name:", state.activeSwarm || "default");
      if (!name) return;
      const clean = name.trim();
      if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(clean)) {
        setComposerFeedback("Invalid swarm name.", "error");
        return;
      }
      if (!state.swarms.includes(clean)) state.swarms.push(clean);
      state.selectedTarget = null;
      state.selectedMessage = null;
      state.timeWindow = null;
      state.followLive = true;
      startSwarmLoop(clean);
    });

    document.getElementById("btn-follow-live").addEventListener("click", () => {
      actions.setFollowLive(!state.followLive);
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

    const stageEl = document.getElementById("stage-container");
    if (stageEl) {
      stageEl.addEventListener(
        "wheel",
        (e) => {
          if (e.deltaY > 0 && state.followLive) {
            actions.setFollowLive(false);
          }
        },
        { passive: true }
      );
      stageEl.addEventListener(
        "touchmove",
        () => {
          if (state.followLive) {
            actions.setFollowLive(false);
          }
        },
        { passive: true }
      );
      stageEl.addEventListener(
        "scroll",
        () => {
          if (stageEl.scrollTop > 0 && state.followLive) {
            actions.setFollowLive(false);
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
  };

  async function boot() {
    initEvents();
    const hash = readHashState();
    if (hash.mode === "stream") {
      state.viewMode = "stream";
      document.getElementById("mode-stream").classList.add("active");
      document.getElementById("mode-threaded").classList.remove("active");
    }
    try {
      const swarms = await fetchSwarms();
      const initialSwarm = hash.swarm || swarms[0] || "default";
      startSwarmLoop(initialSwarm);
    } catch {
      startSwarmLoop(hash.swarm || "default");
    }
  }

  boot();
})();
