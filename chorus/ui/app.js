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
    timelineExpanded: true,
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

  function agentColorClass(handle, role) {
    if (role === "human") return "agent-human";
    const h = String(handle || "").toLowerCase();
    let hash = 2166136261;
    for (let i = 0; i < h.length; i++) {
      hash ^= h.charCodeAt(i);
      hash = Math.imul(hash, 16777619);
    }
    return `agent-c${Math.abs(hash) % 8}`;
  }

  function agentColorHex(handle, role) {
    if (role === "human") return "#F43F5E";
    const palette = [
      "#3B82F6", "#14B8A6", "#F59E0B", "#F43F5E",
      "#6366F1", "#10B981", "#EC4899", "#06B6D4",
    ];
    const h = String(handle || "").toLowerCase();
    let hash = 2166136261;
    for (let i = 0; i < h.length; i++) {
      hash ^= h.charCodeAt(i);
      hash = Math.imul(hash, 16777619);
    }
    return palette[Math.abs(hash) % palette.length];
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
      inboxByRecipient.set(ib.recipient.toLowerCase(), ib);
    }

    for (const ib of inboxes || []) {
      const recipient = ib.recipient;
      const rLower = recipient.toLowerCase();
      for (const m of ib.messages || []) {
        const sender = m.sender;
        const sLower = sender.toLowerCase();
        const [a, b] = sLower <= rLower ? [sLower, rLower] : [rLower, sLower];
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
      if (!raw) return {};
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch {
      return {};
    }
  }

  function saveLastSeenMap(swarm, map) {
    if (!swarm) return;
    try {
      localStorage.setItem(`chorus_seen:${swarm}`, JSON.stringify(map));
    } catch {
      // Ignore storage quota errors
    }
  }

  function initializeBaselineSeenIfEmpty(view) {
    if (!view || !state.activeSwarm) return;
    const map = state.lastSeenSeqByTarget;
    let changed = false;
    for (const ch of view.channels || []) {
      if (map[ch.name] === undefined) {
        const msgs = ch.messages || [];
        const maxSeq = msgs.length > 0 ? msgs[msgs.length - 1].seq : ch.head_seq || 0;
        map[ch.name] = maxSeq;
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
    if (changed) {
      saveLastSeenMap(state.activeSwarm, map);
    }
  }

  function readHashState() {
    const raw = window.location.hash.replace(/^#/, "");
    const params = new URLSearchParams(raw);
    return {
      swarm: params.get("swarm") || "",
      target: params.get("target") || "",
      mode: params.get("mode") || "",
    };
  }

  function writeHashState() {
    const params = new URLSearchParams();
    if (state.activeSwarm) params.set("swarm", state.activeSwarm);
    if (state.selectedTarget) {
      if (state.selectedTarget.kind === "channel") {
        params.set("target", state.selectedTarget.name);
      } else if (state.selectedTarget.kind === "dm") {
        params.set("target", `dm:${state.selectedTarget.pairKey}`);
      } else if (state.selectedTarget.kind === "all") {
        params.set("target", "all");
      }
    }
    if (state.viewMode !== "threaded") {
      params.set("mode", state.viewMode);
    }
    const nextHash = `#${params.toString()}`;
    if (window.location.hash !== nextHash) {
      history.replaceState(null, "", nextHash);
    }
  }

  function reconcileSelectedTarget(view, pairedDMs) {
    const channels = view?.channels || [];
    if (state.selectedTarget) {
      if (state.selectedTarget.kind === "all") return;
      if (state.selectedTarget.kind === "channel") {
        if (channels.some((c) => c.name === state.selectedTarget.name)) return;
      }
      if (state.selectedTarget.kind === "dm") {
        if (pairedDMs.some((p) => p.pairKey === state.selectedTarget.pairKey)) return;
      }
    }
    const hash = readHashState();
    if (hash.target === "all") {
      state.selectedTarget = { kind: "all" };
      return;
    }
    if (hash.target.startsWith("dm:")) {
      const pk = hash.target.slice(3);
      const found = pairedDMs.find((p) => p.pairKey === pk);
      if (found) {
        state.selectedTarget = { kind: "dm", pairKey: found.pairKey, a: found.a, b: found.b };
        return;
      }
    }
    if (hash.target.startsWith("#") && channels.some((c) => c.name === hash.target)) {
      state.selectedTarget = { kind: "channel", name: hash.target };
      return;
    }
    if (channels.length > 0) {
      state.selectedTarget = { kind: "channel", name: channels[0].name };
      const sendTarget = document.getElementById("send-target");
      if (sendTarget && !sendTarget.dataset.userEdited) {
        sendTarget.value = channels[0].name;
      }
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
      const sameAsCurrent =
        state.selectedTarget &&
        state.selectedTarget.kind === targetObj.kind &&
        (targetObj.kind === "channel"
          ? state.selectedTarget.name === targetObj.name
          : targetObj.kind === "dm"
            ? state.selectedTarget.pairKey === targetObj.pairKey
            : true);

      state.selectedTarget = targetObj;
      if (!sameAsCurrent) {
        state.selectedMessage = null;
        state.replyToSeq = 0;
        renderReplyChip();
      }
      const sendTarget = document.getElementById("send-target");
      const pinCheck = document.getElementById("send-pin");
      if (targetObj.kind === "channel" && sendTarget) {
        sendTarget.value = targetObj.name;
      } else if (targetObj.kind === "dm" && sendTarget) {
        const myHandle = (document.getElementById("send-handle")?.value || "raphael").trim().toLowerCase();
        const other = targetObj.a === myHandle ? targetObj.b : targetObj.a;
        sendTarget.value = `@${other}`;
        if (pinCheck) pinCheck.checked = false;
      }
      writeHashState();
      renderAll();
    },

    selectMessage(target, seq) {
      if (!target || !seq || seq <= 0) {
        state.selectedMessage = null;
      } else {
        state.selectedMessage = { target, seq };
        state.followLive = false;
      }
      renderAll();
      if (state.selectedMessage) {
        const domId = target.startsWith("@")
          ? `msg-dm-${target.slice(1)}-${seq}`
          : `msg-${target}-${seq}`;
        const node = document.getElementById(domId);
        if (node) {
          const parentDetails = node.closest("details");
          if (parentDetails && !parentDetails.open) parentDetails.open = true;
          if (node.tagName === "DETAILS" && !node.open) node.open = true;
          node.scrollIntoView({ behavior: "smooth", block: "center" });
        }
      }
    },

    setTimeWindow(win) {
      if (!win || typeof win.startMs !== "number" || typeof win.endMs !== "number") {
        state.timeWindow = null;
        state.followLive = true;
      } else {
        state.timeWindow = { startMs: win.startMs, endMs: win.endMs };
        state.followLive = false;
      }
      renderAll();
    },

    setFollowLive(follow) {
      state.followLive = Boolean(follow);
      if (state.followLive) {
        state.timeWindow = null;
        state.selectedMessage = null;
      }
      renderAll();
    },

    setTimelineExpanded(expanded) {
      state.timelineExpanded = Boolean(expanded);
      renderAll();
    },

    setFilterAgent(handle) {
      const clean = String(handle || "").replace(/^@/, "");
      state.filterAgent =
        state.filterAgent.toLowerCase() === clean.toLowerCase() ? "" : clean;
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

    editPin(channelName, text) {
      const sendTarget = document.getElementById("send-target");
      const sendPin = document.getElementById("send-pin");
      const sendText = document.getElementById("send-text");
      if (sendTarget && channelName) sendTarget.value = channelName;
      if (sendPin) sendPin.checked = true;
      if (sendText) {
        sendText.value = text || "";
        syncComposerHeight();
        sendText.focus();
      }
    },

    markSeen(target, seq) {
      if (target && seq > 0) {
        state.lastSeenSeqByTarget[target] = seq;
      } else if (state.selectedTarget?.kind === "channel") {
        const ch = (state.view?.channels || []).find((c) => c.name === state.selectedTarget.name);
        if (ch) {
          const msgs = ch.messages || [];
          const maxSeq = msgs.length > 0 ? msgs[msgs.length - 1].seq : ch.head_seq || 0;
          state.lastSeenSeqByTarget[ch.name] = maxSeq;
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
      el(
        "span",
        { className: "context-pill" },
        `↩ #${state.replyToSeq}${state.replyToSender ? ` (@${state.replyToSender})` : ""}`,
        el(
          "button",
          {
            type: "button",
            className: "btn-icon",
            "aria-label": "Cancel reply",
            onClick: () => {
              state.replyToSeq = 0;
              state.replyToTarget = "";
              state.replyToSender = "";
              renderReplyChip();
            },
          },
          "×"
        )
      )
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
      txt.textContent =
        state.retryCountdownSec > 0
          ? `Reconnecting (${state.retryCountdownSec}s)`
          : "Reconnecting…";
    } else if (state.connectionState === "connecting") {
      btn.className = "live-pill";
      dot.className = "status-dot warn";
      txt.textContent = "Connecting…";
    } else if (!state.followLive) {
      btn.className = "live-pill reviewing";
      dot.className = "status-dot warn";
      txt.textContent = "Reviewing earlier · Resume live";
    } else {
      btn.className = "live-pill";
      dot.className = "status-dot";
      txt.textContent = "Follow live";
    }
  }

  function renderHeaderAndTabs() {
    const select = document.getElementById("swarm-select");
    const sendBtn = document.getElementById("btn-send");
    if (sendBtn) sendBtn.disabled = !state.activeSwarm;

    const allSwarms = new Set(["default", ...state.swarms]);
    if (state.activeSwarm) allSwarms.add(state.activeSwarm);
    const sortedSwarms = Array.from(allSwarms).sort();
    select.replaceChildren(
      ...sortedSwarms.map((s) => el("option", { value: s, selected: s === state.activeSwarm }, s))
    );

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
      const isActive =
        state.selectedTarget?.kind === "channel" && state.selectedTarget.name === ch.name;
      const seenSeq = state.lastSeenSeqByTarget[ch.name];
      const msgs = ch.messages || [];
      const headSeq = msgs.length > 0 ? msgs[msgs.length - 1].seq : ch.head_seq || 0;
      const newCount = seenSeq !== undefined ? Math.max(0, headSeq - seenSeq) : 0;

      tabNodes.push(
        el(
          "button",
          {
            type: "button",
            className: `target-tab${isActive ? " active" : ""}`,
            onClick: () => actions.selectTarget({ kind: "channel", name: ch.name }),
          },
          el("span", {}, ch.name),
          el(
            "span",
            { className: `count-pill${newCount > 0 ? " attention" : ""}` },
            newCount > 0 ? `+${newCount}` : String(msgs.length)
          )
        )
      );
    }

    for (const pair of pairedDMs) {
      const isActive =
        state.selectedTarget?.kind === "dm" && state.selectedTarget.pairKey === pair.pairKey;
      tabNodes.push(
        el(
          "button",
          {
            type: "button",
            className: `target-tab${isActive ? " active" : ""}`,
            onClick: () =>
              actions.selectTarget({
                kind: "dm",
                pairKey: pair.pairKey,
                a: pair.a,
                b: pair.b,
              }),
          },
          el("span", {}, `@${pair.a} ↔ @${pair.b}`),
          el("span", { className: "count-pill" }, String(pair.messages.length))
        )
      );
    }

    let totalMsgs = 0;
    for (const ch of channels) totalMsgs += (ch.messages || []).length;
    for (const p of pairedDMs) totalMsgs += p.messages.length;

    tabNodes.push(
      el(
        "button",
        {
          type: "button",
          className: `target-tab${state.selectedTarget?.kind === "all" ? " active" : ""}`,
          onClick: () => actions.selectTarget({ kind: "all" }),
        },
        el("span", {}, "All Activity"),
        el("span", { className: "count-pill" }, String(totalMsgs))
      )
    );

    tabsEl.replaceChildren(...tabNodes);

    // Active filters bar
    const filtersBar = document.getElementById("active-filters-bar");
    if (filtersBar) {
      const chips = [];
      if (state.filterAgent) {
        chips.push(
          el(
            "button",
            {
              type: "button",
              className: "chip-btn active",
              onClick: () => actions.setFilterAgent(""),
            },
            `Agent: @${state.filterAgent} ×`
          )
        );
      }
      if (state.filterArtifact) {
        chips.push(
          el(
            "button",
            {
              type: "button",
              className: "chip-btn active",
              onClick: () => actions.setFilterArtifact(""),
            },
            `Artifact: ${state.filterArtifact} ×`
          )
        );
      }
      if (state.timeWindow) {
        chips.push(
          el(
            "button",
            {
              type: "button",
              className: "chip-btn active",
              onClick: () => actions.setTimeWindow(null),
            },
            "Time window active ×"
          )
        );
      }
      filtersBar.replaceChildren(...chips);
    }
  }

  function renderAll() {
    state.pairedDMs = buildPairedDMs(state.view?.inboxes || []);
    reconcileSelectedTarget(state.view, state.pairedDMs);
    renderHeaderAndTabs();

    const timelineRoot = document.getElementById("timeline-root");
    if (timelineRoot && window.ChorusTimeline && typeof window.ChorusTimeline.render === "function") {
      window.ChorusTimeline.render(timelineRoot, state, actions);
    }

    if (window.ChorusStream && typeof window.ChorusStream.render === "function") {
      window.ChorusStream.render(
        {
          agentStripEl: document.getElementById("agent-strip"),
          detailsEl: document.getElementById("agent-details"),
          pinEl: document.getElementById("pin-container"),
          mentionsEl: document.getElementById("mentions-artifacts-container"),
          streamEl: document.getElementById("message-stream"),
          newSinceEl: document.getElementById("new-since-banner"),
          titleEl: document.getElementById("active-title"),
          subtitleEl: document.getElementById("active-subtitle"),
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
    state.activeSwarm = swarmName;
    state.etag = "";
    state.connectionState = "connecting";
    state.retryCountdownSec = 0;
    state.lastSeenSeqByTarget = loadLastSeenMap(swarmName);
    writeHashState();
    renderLiveStatus();

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
    txt.textContent = msg;
    clearTimeout(setComposerFeedback._timer);
    if (msg && kind === "ok") {
      setComposerFeedback._timer = setTimeout(() => {
        wrap.className = "composer-feedback";
        txt.textContent = "";
      }, 4000);
    }
  }

  function syncComposerHeight() {
    // Sizing is handled via CSS field-sizing: content in styles.css so no
    // inline style mutation is needed under strict default-src 'self' CSP.
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

    if (!swarm) {
      setComposerFeedback("Select a swarm before sending.", "error");
      return;
    }
    if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(agent)) {
      setComposerFeedback("Handle must be 1–32 letters, digits, dashes, or underscores.", "error");
      return;
    }
    if (!/^(#|@)[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$/.test(to)) {
      setComposerFeedback("Target must start with #channel or @agent.", "error");
      return;
    }
    if (!text) {
      setComposerFeedback("Enter a message before sending.", "error");
      return;
    }

    const agentLower = agent.toLowerCase();
    const existingAgent = (state.view?.agents || []).find(
      (a) => a.handle.toLowerCase() === agentLower
    );
    if (existingAgent && existingAgent.role !== "human") {
      setComposerFeedback(
        `@${existingAgent.handle} is an agent in this swarm; pick your own handle.`,
        "error"
      );
      return;
    }

    localStorage.setItem("chorus_handle", agent);
    sendBtn.disabled = true;
    setComposerFeedback("");
    try {
      if (to.startsWith("#")) {
        const ch = (state.view?.channels || []).find((c) => c.name === to);
        const alreadyMember =
          Boolean(existingAgent) &&
          Boolean(ch) &&
          (ch.positions || []).some((p) => p.handle.toLowerCase() === agentLower);
        if (!alreadyMember) {
          await callMCPTool("join", {
            swarm,
            agent,
            role: "human",
            channels: [to],
            tail: 1,
          });
        }
      } else if (!existingAgent) {
        await callMCPTool("who", { swarm, agent, role: "human" });
      }

      const sendArgs = { agent, to, text, pin, wait_sec: 0 };
      if (state.replyToSeq > 0 && to.startsWith("#")) {
        sendArgs.reply_to = state.replyToSeq;
      }
      await callMCPTool("send", sendArgs);

      textInput.value = "";
      syncComposerHeight();
      pinCheck.checked = false;
      state.replyToSeq = 0;
      state.replyToTarget = "";
      state.replyToSender = "";
      renderReplyChip();
      setComposerFeedback(pin ? `Pinned in ${to}` : `Sent to ${to}`, "ok");

      startSwarmLoop(swarm);
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
    sendText.addEventListener("input", syncComposerHeight);
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
          if (e.deltaY < 0 && state.followLive) {
            actions.setFollowLive(false);
          }
        },
        { passive: true }
      );
      stageEl.addEventListener(
        "touchmove",
        () => {
          if (
            state.followLive &&
            stageEl.scrollHeight - stageEl.scrollTop - stageEl.clientHeight > 80
          ) {
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
