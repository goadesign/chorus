// Chorus Progressive Disclosure Stream Renderer (Agent Strip, Pin Bar, Mentions/Artifacts, & Messages)
(function () {
  "use strict";

  const pinOpenByChannel = new Map();
  let pinShowRaw = false;
  const lastPinSeqByChannel = new Map();
  let updatedPinChannel = "";

  function el(tag, attrs = {}, ...children) {
    if (window.ChorusUI && typeof window.ChorusUI.el === "function") {
      return window.ChorusUI.el(tag, attrs, ...children);
    }
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === "className") node.className = v;
      else if (k.startsWith("on") && typeof v === "function") {
        node.addEventListener(k.slice(2).toLowerCase(), v);
      } else if (v === true) node.setAttribute(k, "");
      else node.setAttribute(k, String(v));
    }
    for (const child of children.flat()) {
      if (child === null || child === undefined || child === false) continue;
      node.appendChild(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  function agentColorClass(handle, role) {
    if (window.ChorusUI && typeof window.ChorusUI.agentColorClass === "function") {
      return window.ChorusUI.agentColorClass(handle, role);
    }
    return "agent-c0";
  }

  function formatAgeSec(seconds) {
    const s = Math.max(0, Math.floor(seconds));
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60);
    return `${h}h ${m % 60}m`;
  }

  function formatTimeMs(ms) {
    if (!ms || ms <= 0) return "";
    return new Date(ms).toLocaleTimeString([], {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  }

  function firstHeadline(text, maxLen = 110) {
    const line = String(text || "").trim().split("\n")[0] || "";
    return line.length > maxLen ? `${line.slice(0, maxLen - 1)}…` : line;
  }

  function renderFormattedText(text, actions) {
    const frag = document.createDocumentFragment();
    const raw = String(text || "");
    const fenceParts = raw.split(/(```[\s\S]*?```)/g);

    for (const part of fenceParts) {
      if (part.startsWith("```") && part.endsWith("```") && part.length >= 6) {
        const inner = part.slice(3, -3).replace(/^[a-zA-Z0-9_-]+\n/, "");
        frag.appendChild(el("pre", {}, inner));
        continue;
      }
      const inlineParts = part.split(/(`[^`\n]+`)/g);
      for (const seg of inlineParts) {
        if (seg.startsWith("`") && seg.endsWith("`") && seg.length >= 2) {
          frag.appendChild(el("code", {}, seg.slice(1, -1)));
          continue;
        }
        const tokenRegex = /(#[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}|@[a-zA-Z0-9][a-zA-Z0-9_-]{0,31})/g;
        let lastIdx = 0;
        let match;
        while ((match = tokenRegex.exec(seg)) !== null) {
          if (match.index > lastIdx) {
            frag.appendChild(document.createTextNode(seg.slice(lastIdx, match.index)));
          }
          const token = match[0];
          if (token.startsWith("@")) {
            frag.appendChild(el("span", { className: "token-mention" }, token));
          } else {
            frag.appendChild(
              el(
                "span",
                {
                  className: "token-channel",
                  onClick: (e) => {
                    e.stopPropagation();
                    actions.selectTarget({ kind: "channel", name: token });
                  },
                },
                token
              )
            );
          }
          lastIdx = tokenRegex.lastIndex;
        }
        if (lastIdx < seg.length) {
          frag.appendChild(document.createTextNode(seg.slice(lastIdx)));
        }
      }
    }
    return frag;
  }

  function renderAgentStrip(agentStripEl, detailsEl, state, actions) {
    if (!agentStripEl) return;
    const view = state.view;
    const agents = view?.agents || [];

    // Find each agent's most recent message across channels and DMs
    const latestBySender = new Map();
    for (const ch of view?.channels || []) {
      for (const m of ch.messages || []) {
        const prev = latestBySender.get(m.sender.toLowerCase());
        if (!prev || m.sent_at_ms >= prev.sent_at_ms) {
          latestBySender.set(m.sender.toLowerCase(), {
            text: m.text,
            sent_at_ms: m.sent_at_ms,
            coord: `${ch.name}#${m.seq}`,
          });
        }
      }
    }
    for (const ib of view?.inboxes || []) {
      for (const m of ib.messages || []) {
        const prev = latestBySender.get(m.sender.toLowerCase());
        if (!prev || m.sent_at_ms >= prev.sent_at_ms) {
          latestBySender.set(m.sender.toLowerCase(), {
            text: m.text,
            sent_at_ms: m.sent_at_ms,
            coord: `@${ib.recipient}#${m.seq}`,
          });
        }
      }
    }

    if (agents.length === 0) {
      agentStripEl.replaceChildren();
      if (detailsEl) detailsEl.replaceChildren();
      return;
    }

    const cards = agents.map((ag) => {
      const isSelected = state.filterAgent.toLowerCase() === ag.handle.toLowerCase();
      const latest = latestBySender.get(ag.handle.toLowerCase());
      const quoteText = latest
        ? `${latest.coord}: ${firstHeadline(latest.text, 92)}`
        : ag.watching_on_this_server
          ? "Watching on this server"
          : "Registered in swarm";

      return el(
        "button",
        {
          type: "button",
          className: `agent-status-card${isSelected ? " active" : ""}`,
          onClick: () => actions.setFilterAgent(ag.handle),
        },
        el(
          "div",
          { className: "agent-card-top" },
          el(
            "span",
            { className: "agent-card-identity" },
            el("span", {
              className: `status-dot${ag.watching_on_this_server ? "" : " idle"}`,
              title: ag.watching_on_this_server
                ? "Watching on this server"
                : "No active watch on this server",
            }),
            el("span", { className: `agent-handle ${agentColorClass(ag.handle, ag.role)}` }, `@${ag.handle}`),
            ag.role ? el("span", { className: "agent-role" }, ag.role) : null
          )
        ),
        el("div", { className: "agent-latest-quote" }, quoteText)
      );
    });

    agentStripEl.replaceChildren(el("div", { className: "agent-cards-row" }, ...cards));

    if (!detailsEl) return;
    if (!state.filterAgent) {
      detailsEl.replaceChildren();
      return;
    }

    const ag = agents.find((a) => a.handle.toLowerCase() === state.filterAgent.toLowerCase());
    const nowSec = Math.floor(Date.now() / 1000);
    const ageSec = ag ? Math.max(0, nowSec - ag.last_seen_unix) : 0;

    detailsEl.replaceChildren(
      el(
        "div",
        { className: "agent-detail-bar" },
        el(
          "div",
          { className: "agent-detail-facts" },
          el("strong", { className: "mono" }, `@${ag ? ag.handle : state.filterAgent}`),
          ag?.role ? el("span", {}, ag.role) : null,
          ag ? el("span", {}, `Last call/heartbeat ${formatAgeSec(ageSec)} ago`) : null,
          ag
            ? el(
                "span",
                {},
                ag.watching_on_this_server
                  ? "Watching on this server"
                  : "No active watch on this server"
              )
            : null,
          ag?.channels?.length
            ? el("span", { className: "mono" }, `Channels: ${ag.channels.join(", ")}`)
            : null
        ),
        el(
          "div",
          { className: "stream-toolbar-right" },
          el(
            "button",
            {
              type: "button",
              className: "btn-tonal",
              onClick: () => actions.composeDM(ag ? ag.handle : state.filterAgent),
            },
            `Message @${ag ? ag.handle : state.filterAgent}`
          ),
          el(
            "button",
            {
              type: "button",
              className: "btn-text",
              onClick: () => actions.setFilterAgent(""),
            },
            "Clear ×"
          )
        )
      )
    );
  }

  function renderPinBar(pinEl, channel, actions) {
    if (!pinEl) return;
    if (!channel || !channel.pin) {
      pinEl.replaceChildren();
      return;
    }
    const pin = channel.pin;
    const msgs = channel.messages || [];
    const headSeq = msgs.length > 0 ? msgs[msgs.length - 1].seq : channel.head_seq || pin.seq;
    const ageMsgs = Math.max(0, headSeq - pin.seq);

    const prevSeq = lastPinSeqByChannel.get(channel.name);
    if (prevSeq !== undefined && prevSeq !== pin.seq) {
      updatedPinChannel = channel.name;
    }
    lastPinSeqByChannel.set(channel.name, pin.seq);

    const isOpen = Boolean(pinOpenByChannel.get(channel.name));
    const segments = String(pin.text || "")
      .split("|")
      .map((s) => s.trim())
      .filter(Boolean);
    const clauses = [];
    for (const seg of segments) {
      const idx = seg.indexOf(":");
      if (idx > 0 && idx < 28) {
        clauses.push({
          key: seg.slice(0, idx).trim(),
          val: seg.slice(idx + 1).trim(),
        });
      }
    }

    const firstSummary = clauses.length > 0 ? `${clauses[0].key}: ${clauses[0].val}` : firstHeadline(pin.text, 120);
    const hasStructured = clauses.length >= 2;
    const showRaw = !hasStructured || pinShowRaw;

    pinEl.replaceChildren(
      el(
        "details",
        {
          className: "pin-panel",
          open: isOpen,
          onToggle: (e) => {
            pinOpenByChannel.set(channel.name, e.currentTarget.open);
            if (e.currentTarget.open && updatedPinChannel === channel.name) {
              updatedPinChannel = "";
            }
          },
        },
        el(
          "summary",
          {},
          el(
            "div",
            { className: "pin-summary-left" },
            el("strong", { className: "mono" }, `Pinned at #${pin.seq}`),
            el("span", { className: "muted" }, `by @${pin.sender}`),
            el(
              "span",
              { className: `count-pill${ageMsgs >= 15 ? " attention" : ""}` },
              ageMsgs === 0 ? "current" : `${ageMsgs} messages ago`
            ),
            updatedPinChannel === channel.name
              ? el("span", { className: "count-pill attention" }, "Updated")
              : null,
            el("span", { className: "pin-summary-text" }, firstSummary)
          ),
          el(
            "div",
            { className: "pin-summary-right" },
            el(
              "button",
              {
                type: "button",
                className: "btn-text",
                onClick: (e) => {
                  e.preventDefault();
                  e.stopPropagation();
                  actions.editPin(channel.name, pin.text);
                },
              },
              "Edit"
            )
          )
        ),
        el(
          "div",
          { className: "pin-body" },
          hasStructured
            ? el(
                "button",
                {
                  type: "button",
                  className: "btn-text",
                  onClick: () => {
                    pinShowRaw = !pinShowRaw;
                    renderPinBar(pinEl, channel, actions);
                  },
                },
                showRaw ? "Show structured clauses" : "Show raw text"
              )
            : null,
          showRaw
            ? el("div", { className: "pin-raw" }, renderFormattedText(pin.text, actions))
            : el(
                "div",
                { className: "pin-clauses-grid" },
                ...clauses.map((c) =>
                  el(
                    "div",
                    { className: "pin-clause" },
                    el("div", { className: "pin-clause-label" }, c.key),
                    el("div", { className: "pin-clause-content" }, renderFormattedText(c.val, actions))
                  )
                )
              )
        )
      )
    );
  }

  function renderMentionsAndArtifacts(mentionsEl, channel, state, actions) {
    if (!mentionsEl) return;
    if (!channel) {
      mentionsEl.replaceChildren();
      return;
    }
    const msgs = channel.messages || [];
    const byReplyParent = new Map();
    for (const m of msgs) {
      if (m.reply_to > 0 && !byReplyParent.has(m.reply_to)) {
        byReplyParent.set(m.reply_to, m);
      }
    }

    // Recent mentions (last 4 in retained window)
    const recentMentions = [];
    const nowMs = Date.now();
    for (let i = msgs.length - 1; i >= 0 && recentMentions.length < 3; i--) {
      const m = msgs[i];
      const matches = [...String(m.text || "").matchAll(/@([a-zA-Z0-9][a-zA-Z0-9_-]{0,31})/g)];
      for (const mm of matches) {
        const targetHandle = mm[1];
        if (targetHandle.toLowerCase() === "all" || targetHandle.toLowerCase() === m.sender.toLowerCase()) {
          continue;
        }
        const replyMsg = byReplyParent.get(m.seq);
        const ageSec = Math.max(0, Math.floor((nowMs - m.sent_at_ms) / 1000));
        recentMentions.push({
          seq: m.seq,
          sender: m.sender,
          target: targetHandle,
          age: formatAgeSec(ageSec),
          replySeq: replyMsg ? replyMsg.seq : 0,
        });
        break;
      }
    }

    // Extract unambiguous artifacts: PR #N, 7-40 hex commit SHAs, file paths
    const artifactCounts = new Map();
    const addArt = (a) => artifactCounts.set(a, (artifactCounts.get(a) || 0) + 1);
    for (const m of msgs) {
      const t = String(m.text || "");
      for (const pr of t.matchAll(/\bPR\s+#\d+/gi)) addArt(pr[0]);
      for (const pathMatch of t.matchAll(/\b(?:chorus|cmd|design|gen|skills|docs)\/[a-zA-Z0-9_./-]+\.[a-zA-Z0-9]+/g)) {
        addArt(pathMatch[0]);
      }
      for (const sha of t.matchAll(/\b[0-9a-f]{7,12}\b/g)) {
        if (/[a-f]/.test(sha[0]) && /\d/.test(sha[0])) addArt(sha[0]);
      }
    }

    const topArtifacts = [...artifactCounts.entries()]
      .sort((a, b) => b[1] - a[1])
      .slice(0, 6);

    if (recentMentions.length === 0 && topArtifacts.length === 0) {
      mentionsEl.replaceChildren();
      return;
    }

    const chips = [];
    for (const rm of recentMentions) {
      chips.push(
        el(
          "button",
          {
            type: "button",
            className: "chip-btn mention-chip",
            onClick: () => actions.selectMessage(channel.name, rm.seq),
          },
          `@${rm.sender} mentioned @${rm.target} in #${rm.seq} · ${rm.age} ago${rm.replySeq ? ` (↩#${rm.replySeq})` : ""}`
        )
      );
    }

    for (const [art, count] of topArtifacts) {
      const active = state.filterArtifact === art;
      chips.push(
        el(
          "button",
          {
            type: "button",
            className: `chip-btn mono${active ? " active" : ""}`,
            onClick: () => actions.setFilterArtifact(art),
          },
          `${art} (${count})`
        )
      );
    }

    mentionsEl.replaceChildren(el("div", { className: "shelf-bar" }, ...chips));
  }

  function renderMessageCard(msg, opts) {
    const { isDM, channelName, bySeq, isReplyChild, selected, humanHandles, actions } = opts;
    const target = isDM ? `@${msg.recipient}` : channelName;
    const coord = isDM ? `@${msg.recipient}#${msg.seq}` : `${channelName}#${msg.seq}`;
    const isHuman = humanHandles.has(String(msg.sender || "").toLowerCase());

    let replyNode = null;
    if (msg.reply_to && msg.reply_to > 0) {
      if (isDM) {
        replyNode = el("span", { className: "seq-label" }, `↩#${msg.reply_to}`);
      } else {
        const parent = bySeq ? bySeq.get(msg.reply_to) : null;
        replyNode = el(
          "a",
          {
            href: `#msg-${channelName}-${msg.reply_to}`,
            className: "reply-ref-link",
            onClick: (e) => {
              e.preventDefault();
              e.stopPropagation();
              actions.selectMessage(channelName, msg.reply_to);
            },
          },
          parent ? `↩#${msg.reply_to} (@${parent.sender})` : `↩#${msg.reply_to}`
        );
      }
    }

    const lines = String(msg.text || "").trim().split("\n");
    const isLong = lines.length > 2 || String(msg.text || "").length > 180;
    const domId = isDM ? `msg-dm-${msg.recipient}-${msg.seq}` : `msg-${channelName}-${msg.seq}`;

    const metaRow = el(
      "div",
      { className: "msg-meta" },
      el(
        "div",
        { className: "msg-meta-left" },
        el(
          "span",
          { className: `sender-name ${agentColorClass(msg.sender, isHuman ? "human" : "")}` },
          el("span", { className: "sender-dot" }),
          `@${msg.sender}`
        ),
        isDM ? el("span", { className: "seq-label" }, `→ @${msg.recipient}`) : null,
        el("span", { className: "seq-label" }, coord),
        replyNode
      ),
      el(
        "div",
        { className: "msg-meta-right" },
        el("span", { className: "seq-label" }, formatTimeMs(msg.sent_at_ms)),
        !isDM
          ? el(
              "button",
              {
                type: "button",
                className: "btn-text",
                onClick: (e) => {
                  e.stopPropagation();
                  actions.setReplyTo(channelName, msg.seq, msg.sender);
                },
              },
              "Reply"
            )
          : null
      )
    );

    if (isLong && !isReplyChild && !opts.expandAll && !selected) {
      return el(
        "details",
        {
          id: domId,
          className: `msg-disclosure${selected ? " selected" : ""}`,
        },
        el(
          "summary",
          {},
          el(
            "div",
            { className: "disclosure-main" },
            metaRow,
            el("div", { className: "headline-text" }, firstHeadline(msg.text, 130))
          )
        ),
        el(
          "div",
          { className: "thread-items" },
          el("div", { className: "msg-body" }, renderFormattedText(msg.text, actions))
        )
      );
    }

    return el(
      "article",
      {
        id: domId,
        className: `msg-card${isReplyChild ? " msg-reply" : ""}${isHuman ? " human-card" : ""}${selected ? " selected" : ""}`,
        onClick: () => actions.selectMessage(target, msg.seq),
      },
      metaRow,
      el("div", { className: "msg-body" }, renderFormattedText(msg.text, actions))
    );
  }

  function render(els, state, actions) {
    const {
      agentStripEl,
      detailsEl,
      pinEl,
      mentionsEl,
      streamEl,
      newSinceEl,
      titleEl,
      subtitleEl,
    } = els;
    const view = state.view;
    const pairedDMs = state.pairedDMs || [];

    renderAgentStrip(agentStripEl, detailsEl, state, actions);

    const humanHandles = new Set(
      (view?.agents || [])
        .filter((a) => a.role === "human")
        .map((a) => a.handle.toLowerCase())
    );

    const matchesFilters = (m) => {
      if (state.timeWindow) {
        if (m.sent_at_ms < state.timeWindow.startMs || m.sent_at_ms > state.timeWindow.endMs) {
          return false;
        }
      }
      if (state.filterAgent) {
        const want = state.filterAgent.toLowerCase();
        const sMatch = String(m.sender || "").toLowerCase() === want;
        const rMatch = String(m.recipient || "").toLowerCase() === want;
        const mMatch = String(m.text || "").toLowerCase().includes(`@${want}`);
        if (!sMatch && !rMatch && !mMatch) return false;
      }
      if (state.filterArtifact) {
        if (!String(m.text || "").includes(state.filterArtifact)) return false;
      }
      if (state.searchQuery) {
        const q = state.searchQuery.toLowerCase();
        const hay = `${m.sender} ${m.recipient || ""} #${m.seq} ${m.text}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    };

    const target = state.selectedTarget;
    if (target?.kind === "channel") {
      const ch = (view?.channels || []).find((c) => c.name === target.name);
      renderPinBar(pinEl, ch, actions);
      renderMentionsAndArtifacts(mentionsEl, ch, state, actions);

      if (titleEl) titleEl.textContent = ch ? ch.name : target.name;
      if (subtitleEl) {
        subtitleEl.textContent = ch
          ? [
              ch.topic || null,
              `${(ch.positions || []).length} member(s)`,
              `${(ch.messages || []).length} retained`,
            ]
              .filter(Boolean)
              .join(" · ")
          : "";
      }

      if (!ch || (ch.messages || []).length === 0) {
        if (newSinceEl) newSinceEl.replaceChildren();
        streamEl.replaceChildren(
          el("div", { className: "empty-state" }, "Waiting for the first message in this channel.")
        );
        return;
      }

      const msgs = (ch.messages || []).filter(matchesFilters);
      const bySeq = new Map((ch.messages || []).map((m) => [m.seq, m]));
      const seenSeq = state.lastSeenSeqByTarget[ch.name];
      const maxSeq = ch.messages[ch.messages.length - 1].seq;
      const newCount = seenSeq !== undefined ? Math.max(0, maxSeq - seenSeq) : 0;

      if (newSinceEl) {
        if (newCount > 0) {
          const firstNewSeq = seenSeq + 1;
          newSinceEl.replaceChildren(
            el(
              "div",
              { className: "new-since-divider" },
              el("span", {}, `${newCount} new message(s) since you last looked (#${firstNewSeq}..#${maxSeq})`),
              el(
                "div",
                { className: "stream-toolbar-right" },
                el(
                  "button",
                  {
                    type: "button",
                    className: "btn-tonal",
                    onClick: () => actions.selectMessage(ch.name, firstNewSeq),
                  },
                  `Jump to #${firstNewSeq}`
                ),
                el(
                  "button",
                  {
                    type: "button",
                    className: "btn-text",
                    onClick: () => actions.markSeen(ch.name, maxSeq),
                  },
                  "Mark seen"
                )
              )
            )
          );
        } else {
          newSinceEl.replaceChildren();
        }
      }

      const positionsBySeq = new Map();
      for (const p of ch.positions || []) {
        if (p.read_seq > 0) {
          const list = positionsBySeq.get(p.read_seq) || [];
          list.push(p);
          positionsBySeq.set(p.read_seq, list);
        }
      }

      if (state.viewMode === "stream") {
        const nodes = [];
        for (const m of msgs) {
          const selected =
            state.selectedMessage?.target === ch.name && state.selectedMessage?.seq === m.seq;
          nodes.push(
            renderMessageCard(m, {
              isDM: false,
              channelName: ch.name,
              bySeq,
              isReplyChild: false,
              selected,
              expandAll: state.expandAllMessages,
              humanHandles,
              actions,
            })
          );
          const posAt = positionsBySeq.get(m.seq);
          if (posAt && posAt.length > 0) {
            nodes.push(
              el(
                "div",
                { className: "read-line" },
                `Read through #${m.seq} by ${posAt.map((p) => `@${p.handle}`).join(", ")}`
              )
            );
          }
        }
        streamEl.replaceChildren(...nodes);
        return;
      }

      // Threaded grouping with "how a thread ended" outcome summary
      const rootOf = (m) => {
        const visited = new Set([m.seq]);
        let cur = m;
        while (
          cur.reply_to &&
          cur.reply_to > 0 &&
          cur.reply_to !== cur.seq &&
          bySeq.has(cur.reply_to) &&
          !visited.has(cur.reply_to)
        ) {
          visited.add(cur.reply_to);
          cur = bySeq.get(cur.reply_to);
        }
        return cur.seq;
      };

      const groups = new Map();
      const orderedRoots = [];
      for (const m of msgs) {
        const rSeq = rootOf(m);
        if (!groups.has(rSeq)) {
          groups.set(rSeq, { root: bySeq.get(rSeq) || m, items: [] });
          orderedRoots.push(rSeq);
        }
        groups.get(rSeq).items.push(m);
      }

      const nodes = [];
      orderedRoots.forEach((rSeq, idx) => {
        const group = groups.get(rSeq);
        const replies = group.items.filter((x) => x.seq !== group.root.seq);
        const hasSelected = group.items.some(
          (x) => state.selectedMessage?.target === ch.name && state.selectedMessage?.seq === x.seq
        );

        if (replies.length === 0) {
          nodes.push(
            renderMessageCard(group.root, {
              isDM: false,
              channelName: ch.name,
              bySeq,
              isReplyChild: false,
              selected: hasSelected,
              expandAll: state.expandAllMessages,
              humanHandles,
              actions,
            })
          );
        } else {
          const lastReply = replies[replies.length - 1];
          const isOpen =
            state.expandAllMessages || hasSelected || idx >= orderedRoots.length - 2;

          nodes.push(
            el(
              "details",
              {
                id: `msg-${ch.name}-${group.root.seq}`,
                className: `thread-disclosure${hasSelected ? " selected" : ""}`,
                open: isOpen,
              },
              el(
                "summary",
                {},
                el(
                  "div",
                  { className: "disclosure-main" },
                  el(
                    "div",
                    { className: "disclosure-header-line" },
                    el(
                      "span",
                      { className: `sender-name ${agentColorClass(group.root.sender, humanHandles.has(group.root.sender.toLowerCase()) ? "human" : "")}` },
                      el("span", { className: "sender-dot" }),
                      `@${group.root.sender}`
                    ),
                    el("span", { className: "seq-label" }, `${ch.name}#${group.root.seq}`),
                    el("span", { className: "headline-text" }, firstHeadline(group.root.text, 100))
                  ),
                  el(
                    "div",
                    { className: "thread-outcome-line" },
                    `↩ ${replies.length} ${replies.length === 1 ? "reply" : "replies"} · ended with @${lastReply.sender} (#${lastReply.seq}): ${firstHeadline(lastReply.text, 90)}`
                  )
                ),
                el("span", { className: "seq-label" }, formatTimeMs(lastReply.sent_at_ms))
              ),
              el(
                "div",
                { className: "thread-items" },
                renderMessageCard(group.root, {
                  isDM: false,
                  channelName: ch.name,
                  bySeq,
                  isReplyChild: false,
                  selected:
                    state.selectedMessage?.target === ch.name &&
                    state.selectedMessage?.seq === group.root.seq,
                  expandAll: true,
                  humanHandles,
                  actions,
                }),
                ...replies.map((r) =>
                  renderMessageCard(r, {
                    isDM: false,
                    channelName: ch.name,
                    bySeq,
                    isReplyChild: true,
                    selected:
                      state.selectedMessage?.target === ch.name &&
                      state.selectedMessage?.seq === r.seq,
                    expandAll: true,
                    humanHandles,
                    actions,
                  })
                )
              )
            )
          );
        }

        for (const item of group.items) {
          const posAt = positionsBySeq.get(item.seq);
          if (posAt && posAt.length > 0) {
            nodes.push(
              el(
                "div",
                { className: "read-line" },
                `Read through #${item.seq} by ${posAt.map((p) => `@${p.handle}`).join(", ")}`
              )
            );
          }
        }
      });

      streamEl.replaceChildren(...nodes);
    } else if (target?.kind === "dm") {
      renderPinBar(pinEl, null, actions);
      renderMentionsAndArtifacts(mentionsEl, null, state, actions);
      if (newSinceEl) newSinceEl.replaceChildren();

      const pair = pairedDMs.find(
        (p) => p.pairKey === target.pairKey || p.key === target.key
      );
      if (titleEl) titleEl.textContent = pair ? `@${pair.a} ↔ @${pair.b}` : target.pairKey;
      if (subtitleEl) {
        subtitleEl.textContent = pair ? `${pair.messages.length} retained direct message(s)` : "";
      }

      if (!pair || pair.messages.length === 0) {
        streamEl.replaceChildren(
          el("div", { className: "empty-state" }, "No direct messages in this conversation.")
        );
        return;
      }

      const nodes = [];
      for (const m of pair.messages.filter(matchesFilters)) {
        const selected =
          state.selectedMessage?.target === `@${m.recipient}` &&
          state.selectedMessage?.seq === m.seq;
        nodes.push(
          renderMessageCard(m, {
            isDM: true,
            channelName: "",
            bySeq: null,
            isReplyChild: false,
            selected,
            expandAll: true,
            humanHandles,
            actions,
          })
        );
        const ib = m.recipient.toLowerCase() === pair.a ? pair.inboxA : pair.inboxB;
        if (ib && ib.read_seq === m.seq) {
          nodes.push(
            el(
              "div",
              { className: "read-line" },
              `@${ib.recipient} last read @${ib.recipient}#${ib.read_seq}`
            )
          );
        }
      }
      streamEl.replaceChildren(...nodes);
    } else {
      renderPinBar(pinEl, null, actions);
      renderMentionsAndArtifacts(mentionsEl, null, state, actions);
      if (newSinceEl) newSinceEl.replaceChildren();
      if (titleEl) titleEl.textContent = "All Activity";
      if (subtitleEl) {
        subtitleEl.textContent = "Retained messages across all channels and direct messages";
      }

      const allItems = [];
      for (const ch of view?.channels || []) {
        const bySeq = new Map((ch.messages || []).map((m) => [m.seq, m]));
        for (const m of ch.messages || []) {
          if (matchesFilters(m)) {
            allItems.push({ m, isDM: false, channelName: ch.name, bySeq });
          }
        }
      }
      for (const p of pairedDMs) {
        for (const m of p.messages) {
          if (matchesFilters(m)) {
            allItems.push({ m, isDM: true, channelName: "", bySeq: null });
          }
        }
      }
      allItems.sort((a, b) => a.m.sent_at_ms - b.m.sent_at_ms || a.m.seq - b.m.seq);

      if (allItems.length === 0) {
        streamEl.replaceChildren(
          el("div", { className: "empty-state" }, "No matching activity in this swarm.")
        );
        return;
      }

      streamEl.replaceChildren(
        ...allItems.map((item) =>
          renderMessageCard(item.m, {
            isDM: item.isDM,
            channelName: item.channelName,
            bySeq: item.bySeq,
            isReplyChild: false,
            selected: false,
            expandAll: state.expandAllMessages,
            humanHandles,
            actions,
          })
        )
      );
    }
  }

  window.ChorusStream = { render };
})();
