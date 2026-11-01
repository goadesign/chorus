// Render retained messages and their disclosures from the controller's current
// snapshot. The controller owns network calls, filters, selection, and sending.
(() => {
  "use strict";

  window.ChorusStream = Object.freeze({
    render,
    captureScrollSnapshot,
    restoreScrollSnapshot,
  });

  // render updates the message surfaces without keeping another copy of swarm
  // data. Disclosure choices and reading position belong to the existing DOM.
  function render(els, state, actions) {
    if (state.view === null) {
      for (const root of [els.agentStripEl, els.pinEl, els.mentionsEl, els.newSinceEl, els.detailsEl]) {
        root.replaceChildren();
      }
      delete els.detailsEl.dataset.agent;
      els.titleEl.textContent = "Messages";
      els.subtitleEl.textContent = "Loading swarm…";
      els.streamEl.replaceChildren(empty("Loading messages", "Connecting to the swarm…"));
      return;
    }
    const entries = retainedMessages(state.view);
    const targetEntries = selectedMessages(entries, state);
    const visible = targetEntries.filter((entry) => matches(entry, state));
    const scrollSnapshot = els.scrollSnapshot || captureScrollSnapshot(els.streamEl, els.scrollEl);

    renderAgents(els, state, actions, entries);
    renderPin(els.pinEl, state, actions);
    renderMentions(els.mentionsEl, targetEntries, state, actions, entries);
    renderNewMessages(els.newSinceEl, targetEntries, state, actions);
    const selectionChanged = renderMessages(els.streamEl, visible, targetEntries, state, actions);
    renderSelection(els.detailsEl, state, actions, entries, visible);

    restoreScrollSnapshot(els.streamEl, els.scrollEl, scrollSnapshot, state);
    revealSelectedMessage(els.streamEl, state, selectionChanged);

    const target = state.selectedTarget;
    if (target?.kind === "channel") {
      els.titleEl.textContent = target.name;
    } else if (target?.kind === "dm") {
      els.titleEl.replaceChildren(handleBadgeNode(target.a, state), " ↔ ", handleBadgeNode(target.b, state));
    } else {
      els.titleEl.textContent = "All messages";
    }
    els.subtitleEl.textContent = `${visible.length} of ${targetEntries.length} messages · newest first`;
  }

  // retainedMessages attaches each message's real channel or recipient inbox.
  // A sequence number is unique only within that source, including for DMs.
  function retainedMessages(view) {
    const entries = [];
    for (const channel of view.channels) {
      for (const message of channel.messages) entries.push({ target: channel.name, message });
    }
    for (const inbox of view.inboxes) {
      for (const message of inbox.messages) entries.push({ target: `@${inbox.recipient}`, message });
    }
    entries.sort(compareEntries);
    return entries;
  }

  // selectedMessages limits the view to the chosen channel or pair of people.
  // The all-messages view preserves inbox coordinates instead of merging IDs.
  function selectedMessages(entries, state) {
    const target = state.selectedTarget;
    if (target?.kind === "channel") return entries.filter((entry) => entry.target === target.name);
    if (target?.kind !== "dm") return entries;
    return entries.filter((entry) => {
      if (!entry.target.startsWith("@")) return false;
      const sender = entry.message.sender;
      const recipient = entry.target.slice(1);
      return (sender === target.a && recipient === target.b)
        || (sender === target.b && recipient === target.a);
    });
  }

  // compareEntries gives equal-time messages from separate streams a stable
  // ascending display order; presentation reverses this to show newest on top.
  function compareEntries(a, b) {
    return a.message.sent_at_ms - b.message.sent_at_ms
      || a.target.localeCompare(b.target) || a.message.seq - b.message.seq;
  }

  // matches applies only presentation choices supplied by the controller.
  function matches(entry, state) {
    const message = entry.message;
    if (state.timeWindow && (message.sent_at_ms < state.timeWindow.startMs
      || message.sent_at_ms > state.timeWindow.endMs)) return false;
    if (state.filterAgent) {
      const handle = state.filterAgent;
      if (message.sender !== handle
        && entry.target !== `@${handle}`
        && !mentionedHandles(message.text).some((mention) => mention.toLowerCase() === handle.toLowerCase())) return false;
    }
    if (state.filterArtifact && !artifacts(message.text).includes(state.filterArtifact)) return false;
    const query = state.searchQuery.toLowerCase();
    return !query || `${entry.target} ${message.sender} #${message.seq} ${message.text}`
      .toLowerCase().includes(query);
  }

  // renderAgents renders each active and historical participant card with their
  // 32px circular avatar, role, watch state on this server, last call/heartbeat
  // or latest message age, compact sequence-position gap badge, and latest message.
  function renderAgents(els, state, actions, entries) {
    const latest = new Map();
    for (const entry of entries) latest.set(entry.message.sender, entry);
    const activeChannel = state.selectedTarget?.kind === "channel"
      ? state.view.channels.find((ch) => ch.name === state.selectedTarget.name)
      : null;
    const inboxByHandle = new Map((state.view.inboxes || []).map((ib) => [ib.recipient, ib]));

    const registeredByHandle = new Map();
    const participants = [];
    for (const agent of state.view.agents || []) {
      const item = { ...agent, registered: true };
      registeredByHandle.set(agent.handle, item);
      participants.push(item);
    }
    for (const [handle, entry] of latest) {
      if (registeredByHandle.has(handle)) continue;
      const seenChannels = [...new Set(entries.filter((e) => e.message.sender === handle && e.target.startsWith("#")).map((e) => e.target))];
      participants.push({
        handle, role: "", last_seen_unix: 0, last_message_ms: entry.message.sent_at_ms,
        channels: seenChannels, watching_on_this_server: false, registered: false,
      });
    }
    const participantByHandle = new Map(participants.map((p) => [p.handle, p]));

    const nodes = participants.map((agent) => {
      const entry = latest.get(agent.handle);
      const pos = activeChannel?.positions?.find((p) => p.handle === agent.handle) || null;
      const inbox = inboxByHandle.get(agent.handle) || null;
      const human = Boolean(agent.registered && agent.model === "human");
      const colorClass = senderColorClass(agent.handle, state, human);
      const statusNode = agent.registered && agent.watching_on_this_server
        ? element("span", "agent-status-inline is-watching", element("span", "presence-dot"), "Watching")
        : element("span", "agent-status-inline",
            agent.registered ? `Active ${age(agent.last_seen_unix * 1000)} ago` : (entry ? `Last post ${age(entry.message.sent_at_ms)} ago` : "Earlier"));
      const headerRow = element("span", "agent-card-top",
        avatarNode(agent.handle, state, agent.registered ? agent.model || "" : "", human),
        element("strong", `agent-name ${colorClass}`.trim(), `@${agent.handle}`),
        ...(human ? [element("span", "human-pill", "Human")] : []),
        ...(agent.model && agent.model !== "human" ? [element("span", "agent-model-inline", agent.model)] : []),
        statusNode);

      if (!human && pos && pos.behind > 0) {
        const gapLabel = `Position gap ${pos.behind} · Read through #${pos.read_seq}${pos.mentioned ? " · @mentioned" : ""}`;
        const pill = element("span", "agent-behind-pill", `${pos.behind} behind`);
        pill.title = gapLabel;
        pill.setAttribute("aria-label", gapLabel);
        headerRow.append(pill);
        if (pos.mentioned) {
          const atDot = element("span", "agent-mention-dot", "@ mention");
          atDot.title = `@mentioned after #${pos.read_seq}`;
          atDot.setAttribute("aria-label", `@mentioned after #${pos.read_seq}`);
          headerRow.append(atDot);
        }
      } else if (!human && inbox && inbox.behind > 0) {
        const dmGapLabel = `DM position gap ${inbox.behind} · Read through #${inbox.read_seq}`;
        const pill = element("span", "agent-behind-pill", `DM ${inbox.behind} behind`);
        pill.title = dmGapLabel;
        pill.setAttribute("aria-label", dmGapLabel);
        headerRow.append(pill);
      } else if (!human && pos && pos.read_seq > 0) {
        const readLabel = `Read through #${pos.read_seq}`;
        const pill = element("span", "agent-caught-up-pill", `read #${pos.read_seq}`);
        pill.title = readLabel;
        pill.setAttribute("aria-label", readLabel);
        headerRow.append(pill);
      }

      const cardChildren = [headerRow];
      if (agent.role && agent.role !== "human") cardChildren.push(element("span", "agent-role-inline", agent.role));
      cardChildren.push(element("span", "agent-latest",
        entry ? `${entry.target}#${entry.message.seq} (${age(entry.message.sent_at_ms)} ago): ${headline(entry.message.text)}` : "No retained messages"));
      const button = element("button",
        `agent-summary${agent.registered ? "" : " is-historical"}${human ? " is-human-agent" : ""}${state.filterAgent === agent.handle ? " is-selected" : ""}`,
        ...cardChildren);
      button.type = "button";
      button.dataset.focusKey = `agent:${agent.handle}`;
      button.setAttribute("aria-label", `Inspect @${agent.handle}`);
      button.addEventListener("click", () => {
        actions.selectMessage(null, 0);
        if (els.detailsEl.dataset.agent === agent.handle) {
          delete els.detailsEl.dataset.agent;
          els.detailsEl.replaceChildren();
          return;
        }
        els.detailsEl.dataset.agent = agent.handle;
        showAgent(els.detailsEl, agent, state, actions, entry);
      });
      return button;
    });

    if (nodes.length === 0) {
      nodes.push(element("p", "empty-inline", "No active agents in the last 5 minutes. Historical messages remain visible below."));
    }
    replaceKeepingFocus(els.agentStripEl, nodes);
    const inspected = els.detailsEl.dataset.agent;
    if (inspected) {
      const agent = participantByHandle.get(inspected);
      if (agent) showAgent(els.detailsEl, agent, state, actions, latest.get(inspected));
      else {
        delete els.detailsEl.dataset.agent;
        els.detailsEl.replaceChildren();
      }
    }
  }

  // showAgent describes current registration or historical message facts,
  // declared model, per-channel and DM sequence-position gaps and read watermarks,
  // and 1-click actions to message, filter, or copy the chorus watch command.
  function showAgent(root, agent, state, actions, latest) {
    const close = iconButton("close", "Close", "close-agent", () => {
      delete root.dataset.agent;
      root.replaceChildren();
    });

    const posSummaries = [];
    for (const ch of state.view.channels || []) {
      const p = (ch.positions || []).find((item) => item.handle === agent.handle);
      if (p) {
        posSummaries.push(p.behind > 0
          ? `${ch.name}: Position gap ${p.behind} · Read through #${p.read_seq} (head #${ch.head_seq}${p.mentioned ? ", @mentioned" : ""})`
          : `${ch.name}: Read through #${p.read_seq} (caught up)`);
      }
    }
    const inbox = (state.view.inboxes || []).find((ib) => ib.recipient === agent.handle);
    if (inbox) {
      posSummaries.push(inbox.behind > 0
        ? `DMs: Position gap ${inbox.behind} · Read through #${inbox.read_seq} (head #${inbox.head_seq})`
        : `DMs: Read through #${inbox.read_seq} (caught up)`);
    }

    const facts = element("dl", "agent-facts",
      element("dt", "", "Model"),
      element("dd", "", agent.model || "Not declared"),
      element("dt", "", agent.registered !== false ? "Last call or heartbeat" : "Last message"),
      element("dd", "", agent.registered !== false ? `${age(agent.last_seen_unix * 1000)} ago` : (latest ? `${age(latest.message.sent_at_ms)} ago` : "Unknown")),
      element("dt", "", "Watching on this server"),
      element("dd", "", agent.watching_on_this_server ? "Yes (listening)" : "No"),
      element("dt", "", "Channels"),
      element("dd", "", (agent.channels || []).join(", ") || "None"),
      element("dt", "", "Read watermarks"),
      element("dd", "", posSummaries.join(" · ") || "No channel watermarks"));

    const watchCmd = `chorus watch ${agent.handle}`;
    const copyWatchBtn = actionButton([svgIcon("copy", "icon icon-xs"), `Copy: ${watchCmd}`], `copy-watch:${agent.handle}`, (ev) => {
      const btn = ev.currentTarget;
      if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
        navigator.clipboard.writeText(watchCmd).then(() => {
          btn.replaceChildren(svgIcon("check", "icon icon-xs"), `Copied: ${watchCmd}`);
        }).catch(() => {});
      }
    });

    const isHuman = Boolean(agent.registered && agent.model === "human");
    const nodes = [
      element("div", "details-heading",
        element("h3", "details-agent-title",
          avatarNode(agent.handle, state, agent.registered !== false ? agent.model || "" : "", isHuman),
          `@${agent.handle}`),
        close),
    ];
    if (agent.role) nodes.push(element("p", "agent-role", agent.role));
    nodes.push(facts);
    if (latest) nodes.push(element("p", "agent-last-message", `Latest message (${latest.target}#${latest.message.seq}): ${headline(latest.message.text)}`));
    nodes.push(element("div", "detail-actions",
      actionButton(["Direct message ", handleBadgeNode(agent.handle, state, "", isHuman)], `dm:${agent.handle}`, () => actions.composeDM(agent.handle)),
      actionButton(state.filterAgent === agent.handle ? "Clear agent filter" : ["Filter by ", handleBadgeNode(agent.handle, state, "", isHuman)], `filter-agent:${agent.handle}`, () => actions.setFilterAgent(agent.handle)),
      copyWatchBtn));
    replaceKeepingFocus(root, nodes);
  }

  // renderPin renders the channel pin with a clean summary bar, a structured
  // M3 Lists layout (full-width Goal, per-Owner rows with avatar + handle and
  // wrapping monospace scope, and a Verify command row with Copy), a pencil
  // icon button in the header, and an in-place M3 editor with Cancel / Update pin.
  function renderPin(root, state, actions) {
    const channel = state.selectedTarget?.kind === "channel"
      ? state.view.channels.find((item) => item.name === state.selectedTarget.name) : null;
    if (!channel?.pin) {
      root.replaceChildren();
      delete root.dataset.pinKey;
      delete root.dataset.editing;
      return;
    }
    const pin = channel.pin;
    const key = `${state.activeSwarm}:${channel.name}`;
    const previous = root.querySelector("details");
    const sameChannel = root.dataset.pinKey === key;
    if (!sameChannel) {
      delete root.dataset.editing;
      root.dataset.pinMode = "fields";
    }
    const editing = sameChannel && root.dataset.editing === "true";
    if (editing && root.querySelector(".pin-editor")) {
      return;
    }
    const changed = sameChannel && root.dataset.pinSeq !== String(pin.seq);
    const updated = changed || (sameChannel && root.dataset.updated === "true");
    const disclosure = element("details", "pin-disclosure");
    disclosure.open = editing || (sameChannel && previous ? previous.open : false);
    disclosure.dataset.disclosureKey = `pin:${key}`;
    const newer = channel.messages.filter((message) => message.seq > pin.seq).length;
    const pinFromHuman = pin.human === true;
    const summary = element("summary", "pin-summary",
      svgIcon("pin", "icon icon-sm pin-icon"),
      element("strong", "pin-title", `Pin #${pin.seq} · ${cleanPinHeadline(pin.text)}`),
      element("span", "pin-meta", "by ", handleBadgeNode(pin.sender, state, "", pinFromHuman), ` · ${newer} newer`));
    summary.dataset.focusKey = `pin:${key}`;
    if (updated) summary.append(element("span", "pin-updated", "Updated"));
    disclosure.append(summary);

    if (editing) {
      disclosure.append(renderPinEditor(root, channel.name, pin.text, key, state, actions));
    } else {
      const clauses = pinClauses(pin.text);
      const raw = element("pre", "pin-raw-text", pin.text);
      const structured = renderPinStructured(clauses, state);
      const rawMode = sameChannel && root.dataset.pinMode === "raw";
      raw.hidden = clauses.length > 0 && !rawMode;
      structured.hidden = clauses.length === 0 || rawMode;
      const body = element("div", "pin-body", structured, raw);
      if (clauses.length > 0) {
        const toggleBtn = element("button", "pin-raw-toggle", rawMode ? "Show structured view" : "Show original text");
        toggleBtn.type = "button";
        toggleBtn.dataset.focusKey = `pin-mode:${key}`;
        toggleBtn.addEventListener("click", () => {
          const showRaw = root.dataset.pinMode !== "raw";
          root.dataset.pinMode = showRaw ? "raw" : "fields";
          raw.hidden = !showRaw;
          structured.hidden = showRaw;
          toggleBtn.textContent = showRaw ? "Show structured view" : "Show original text";
        });
        body.append(toggleBtn);
      }
      disclosure.append(body);
    }

    disclosure.addEventListener("toggle", () => {
      if (disclosure.open && root.contains(disclosure)) {
        root.dataset.updated = "false";
        disclosure.querySelector(".pin-updated")?.remove();
      }
    });
    root.dataset.pinKey = key;
    root.dataset.pinSeq = String(pin.seq);
    root.dataset.updated = String(updated);

    const wrapper = element("div", "disclosure-item pin-item", disclosure);
    if (!editing) {
      const editBtn = iconButton("edit", "Edit pin", `edit-pin:${key}`, () => {
        root.dataset.editing = "true";
        renderPin(root, state, actions);
        root.querySelector(".pin-edit-textarea")?.focus();
      }, "pin-edit-btn");
      wrapper.append(editBtn);
    }
    replaceKeepingFocus(root, [wrapper]);
  }

  function renderPinStructured(clauses, state) {
    const container = element("div", "pin-structured");
    for (const clause of clauses) {
      const kind = clause.key.toLowerCase();
      if (kind === "goal") {
        container.append(element("div", "pin-section",
          element("div", "pin-section-label", "Goal"),
          element("div", "pin-goal-block", formattedText(clause.value))));
      } else if (kind === "owners") {
        const list = element("div", "pin-owners-list");
        for (const item of clause.value.split(";").map((s) => s.trim()).filter(Boolean)) {
          const m = /^@([a-zA-Z0-9][a-zA-Z0-9_-]{0,31})\s+([\s\S]+)$/.exec(item);
          if (m) {
            list.append(element("div", "pin-owner-row",
              element("span", "pin-owner-who", avatarNode(m[1], state), handleBadgeNode(m[1], state)),
              element("span", "pin-owner-scope", formattedText(m[2].trim()))));
          } else {
            list.append(element("div", "pin-owner-row", element("span", "pin-owner-scope", formattedText(item))));
          }
        }
        container.append(element("div", "pin-section", element("div", "pin-section-label", "Owners"), list));
      } else if (kind === "verify") {
        const copyBtn = iconButton("copy", "Copy verification command", `copy-verify:${clause.value}`, (ev) => {
          const btn = ev.currentTarget;
          if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
            navigator.clipboard.writeText(clause.value).then(() => {
              btn.replaceChildren(svgIcon("check"));
            }).catch(() => {});
          }
        });
        container.append(element("div", "pin-section",
          element("div", "pin-section-label", "Verify"),
          element("div", "pin-verify-row", element("code", "pin-verify-code", clause.value), copyBtn)));
      }
    }
    return container;
  }

  function renderPinEditor(root, channelName, originalText, key, state, actions) {
    const textarea = element("textarea", "pin-edit-textarea", originalText);
    textarea.rows = 3;
    textarea.dataset.focusKey = `pin-textarea:${key}`;
    const errEl = element("p", "pin-edit-error");
    errEl.hidden = true;

    const closeEditor = () => {
      delete root.dataset.editing;
      renderPin(root, state, actions);
      root.querySelector(".pin-summary")?.focus();
    };

    textarea.addEventListener("keydown", (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        closeEditor();
      }
    });

    const cancelBtn = element("button", "btn-text", "Cancel");
    cancelBtn.type = "button";
    cancelBtn.dataset.focusKey = `pin-cancel:${key}`;
    cancelBtn.addEventListener("click", closeEditor);

    const saveBtn = element("button", "btn-primary pin-save-btn", "Update pin");
    saveBtn.type = "button";
    saveBtn.dataset.focusKey = `pin-save:${key}`;
    saveBtn.addEventListener("click", async () => {
      errEl.hidden = true;
      saveBtn.disabled = true;
      try {
        await actions.updatePin(channelName, textarea.value);
        delete root.dataset.editing;
        renderPin(root, state, actions);
      } catch (err) {
        errEl.textContent = err.message || "Failed to update pin.";
        errEl.hidden = false;
      } finally {
        saveBtn.disabled = false;
      }
    });

    return element("div", "pin-editor", textarea, errEl, element("div", "pin-edit-actions", cancelBtn, saveBtn));
  }

  function cleanPinHeadline(text) {
    return headline(text).replace(/^\s*Goal\s*:\s*/i, "");
  }

  function pinClauses(text) {
    const segments = text.split("|");
    if (segments.length < 2) return [];
    const clauses = [];
    for (const segment of segments) {
      const match = /^\s*(Goal|Owners|Verify)\s*:\s*([\s\S]+)$/i.exec(segment);
      if (!match) return [];
      clauses.push({ key: match[1], value: match[2].trim() });
    }
    return clauses;
  }

  // renderMentions shows written mentions of known participants and @all, plus
  // clickable references for commits, PRs, and files.
  function renderMentions(root, entries, state, actions, allEntries) {
    const mentionTargets = new Set(["all", ...state.view.agents.map((agent) => agent.handle.toLowerCase())]);
    for (const entry of allEntries) {
      mentionTargets.add(entry.message.sender.toLowerCase());
      if (entry.target.startsWith("@")) mentionTargets.add(entry.target.slice(1).toLowerCase());
    }
    const mentions = [];
    for (const entry of entries) {
      for (const handle of mentionedHandles(entry.message.text)) {
        if (mentionTargets.has(handle.toLowerCase())) mentions.push({ entry, handle });
      }
    }
    const open = root.querySelector(".mentions-disclosure")?.open === true;
    const disclosure = element("details", "mentions-disclosure");
    disclosure.open = open;
    const last = mentions.at(-1);
    const mentionsSummary = element("summary", "mentions-summary",
      ...(last
        ? ["Recent mentions: ", handleBadgeNode(last.entry.message.sender, state, "", isFromHumanMessage(last.entry.message)), " → ", handleBadgeNode(last.handle, state), ` (${age(last.entry.message.sent_at_ms)} ago) · ${mentions.length} total`]
        : ["No mentions in this view"]));
    mentionsSummary.dataset.focusKey = "mentions-summary";
    disclosure.append(mentionsSummary);
    const list = element("ul", "mention-list");
    for (const { entry, handle } of mentions.toReversed()) {
      const row = element("li", "mention-row",
        actionButton(
          [handleBadgeNode(entry.message.sender, state, "", isFromHumanMessage(entry.message)), " mentioned ", handleBadgeNode(handle, state), ` (${entry.target}#${entry.message.seq})`],
          `mention:${coordinate(entry)}:${handle}`,
          () => actions.selectMessage(entry.target, entry.message.seq)
        ),
        element("span", "message-time", `${age(entry.message.sent_at_ms)} ago`));
      if (entry.target.startsWith("#")) {
        const reply = entries.find((item) => item.target === entry.target && item.message.reply_to === entry.message.seq);
        if (reply) row.append(actionButton(["Reply by ", handleBadgeNode(reply.message.sender, state, "", isFromHumanMessage(reply.message))], `mention-reply:${coordinate(reply)}`,
          () => actions.selectMessage(reply.target, reply.message.seq)));
      }
      list.append(row);
    }
    disclosure.append(list);
    const references = [...new Set(entries.flatMap((entry) => artifacts(entry.message.text)))];
    const filters = element("details", "artifact-disclosure");
    filters.open = root.querySelector(".artifact-disclosure")?.open === true;
    const artifactSummary = element("summary", "artifact-summary", `Referenced files, commits & PRs (${references.length})`);
    artifactSummary.dataset.focusKey = "artifact-summary";
    filters.append(artifactSummary);
    const buttons = element("div", "artifact-filters");
    for (const reference of references) {
      const button = actionButton(reference, `artifact:${reference}`, () => actions.setFilterArtifact(reference === state.filterArtifact ? "" : reference));
      button.classList.add("artifact-filter");
      button.setAttribute("aria-pressed", String(reference === state.filterArtifact));
      buttons.append(button);
    }
    filters.append(buttons);
    replaceKeepingFocus(root, [
      ...(mentions.length > 0 ? [disclosure] : []),
      ...(references.length > 0 ? [filters] : []),
    ]);
  }

  // renderNewMessages separates the retained new-message count from the
  // "Jump to new" and "Mark seen" actions so jumping never silently marks seen.
  function renderNewMessages(root, entries, state, actions) {
    const fresh = entries.filter((entry) => Object.hasOwn(state.lastSeenSeqByTarget, entry.target)
      && entry.message.seq > state.lastSeenSeqByTarget[entry.target]);
    if (fresh.length === 0) {
      root.replaceChildren();
      return;
    }
    const newest = fresh.at(-1);
    const bar = element("div", "new-messages-bar",
      element("span", "new-messages-count", `${fresh.length} new since you looked`),
      actionButton([svgIcon("arrowUp", "icon icon-xs"), "Jump to new"], "jump-new-messages", () => {
        actions.selectMessage(newest.target, newest.message.seq);
      }),
      iconButton("check", "Mark seen", "new-messages", () => {
        for (const target of new Set(fresh.map((entry) => entry.target))) {
          const latest = entries.filter((entry) => entry.target === target).at(-1);
          actions.markSeen(target, latest.message.seq);
        }
      }));
    replaceKeepingFocus(root, [bar]);
  }

  // isVisibleAnchorRow returns true for collapsed thread rows and visible
  // message rows, skipping expanded thread containers (whose inner message rows
  // are the real reading anchors) and hidden children inside closed disclosures.
  function isVisibleAnchorRow(node, root) {
    if (node.classList.contains("thread-disclosure") && node.open) return false;
    let parent = node.parentElement;
    while (parent && parent !== root) {
      if (parent.tagName === "DETAILS" && !parent.open) return false;
      parent = parent.parentElement;
    }
    return true;
  }

  // captureScrollSnapshot records the scroll offset and visible reading rows
  // before any section in the scrollable stage replaces its DOM children.
  function captureScrollSnapshot(root, scrollEl) {
    if (!root || !scrollEl) return { oldTop: 0, candidates: [] };
    const oldTop = scrollEl.scrollTop;
    const rootTop = scrollEl.getBoundingClientRect().top;
    const candidates = [...root.querySelectorAll("[data-disclosure-key]")]
      .filter((node) => isVisibleAnchorRow(node, root) && node.getBoundingClientRect().bottom >= rootTop)
      .map((node) => ({
        key: node.dataset.disclosureKey,
        version: node.dataset.anchorVersion || "",
        offset: node.getBoundingClientRect().top - rootTop,
      }));
    return { oldTop, candidates };
  }

  // restoreScrollSnapshot restores the reader's visible anchor row offset after
  // all stage sections have updated their DOM.
  function restoreScrollSnapshot(root, scrollEl, snapshot, state) {
    if (!root || !scrollEl || !snapshot || state.followLive) return;
    scrollEl.scrollTop = snapshot.oldTop;
    const afterRows = [...root.querySelectorAll("[data-disclosure-key]")].filter((node) => isVisibleAnchorRow(node, root));
    let match = null;
    for (const cand of snapshot.candidates) {
      const node = afterRows.find((row) => row.dataset.disclosureKey === cand.key && (row.dataset.anchorVersion || "") === cand.version);
      if (node) { match = { cand, node }; break; }
    }
    if (!match) {
      for (const cand of snapshot.candidates) {
        const node = afterRows.find((row) => row.dataset.disclosureKey === cand.key);
        if (node) { match = { cand, node }; break; }
      }
    }
    if (match) {
      scrollEl.scrollTop += match.node.getBoundingClientRect().top - scrollEl.getBoundingClientRect().top - match.cand.offset;
    }
  }

  // revealSelectedMessage opens and scrolls to the explicitly selected message
  // when selection changes.
  function revealSelectedMessage(root, state, selectionChanged) {
    const selected = state.selectedMessage;
    if (!selected || !root) return;
    const node = [...root.querySelectorAll("[data-message-key]")].find((item) => item.dataset.messageKey === `${selected.target}:${selected.seq}`);
    if (node) {
      node.open = true;
      const thread = node.closest(".thread-disclosure");
      if (thread) thread.open = true;
      if (selectionChanged) node.scrollIntoView({ block: "nearest" });
    }
  }

  // renderMessages stacks newest threads and newest messages at the top of the
  // feed so the latest conversation is immediately visible below the timeline
  // without scrolling to the bottom of the page.
  function renderMessages(root, entries, targetEntries, state, actions) {
    const openKeys = new Set([...root.querySelectorAll("details[open][data-disclosure-key]")].map((node) => node.dataset.disclosureKey));
    const expandAll = state.expandAllMessages === true;
    if (root.dataset.expandAll !== String(expandAll)) {
      openKeys.clear();
      if (expandAll) {
        for (const entry of targetEntries) {
          openKeys.add(`message:${coordinate(entry)}`);
          openKeys.add(`thread:${coordinate(entry)}`);
        }
      }
    }
    const selectionKey = state.selectedMessage ? `${state.selectedMessage.target}:${state.selectedMessage.seq}` : "";
    const selectionChanged = root.dataset.selectedMessage !== selectionKey;

    const byTarget = new Map();
    for (const entry of targetEntries) {
      if (!byTarget.has(entry.target)) byTarget.set(entry.target, new Map());
      byTarget.get(entry.target).set(entry.message.seq, entry);
    }

    const nodes = [];
    if (entries.length === 0) {
      nodes.push(empty(targetEntries.length === 0 ? "Waiting for the first message" : "No matching messages",
        targetEntries.length === 0 ? "Messages will appear here when someone posts." : "Change the time range, search, or filters to see more."));
    } else if (state.viewMode === "threaded") {
      const groups = threadGroups(entries, byTarget).sort((a, b) => {
        const lastA = a.at(-1).message;
        const lastB = b.at(-1).message;
        return lastB.sent_at_ms - lastA.sent_at_ms || lastB.seq - lastA.seq;
      });
      for (const group of groups) {
        nodes.push(group.length === 1 ? messageDisclosure(group[0], state, actions, byTarget, openKeys) : threadDisclosure(group, state, actions, byTarget, openKeys));
      }
    } else {
      for (const entry of entries.toReversed()) {
        nodes.push(messageDisclosure(entry, state, actions, byTarget, openKeys));
      }
    }

    replaceKeepingFocus(root, nodes);
    root.dataset.selectedMessage = selectionKey;
    root.dataset.expandAll = String(expandAll);
    return selectionChanged;
  }

  function threadGroups(entries, byTarget) {
    const parents = new Map();
    function root(key) {
      if (!parents.has(key)) parents.set(key, key);
      let cursor = key;
      while (parents.get(cursor) !== cursor) cursor = parents.get(cursor);
      let child = key;
      while (parents.get(child) !== cursor) {
        const parent = parents.get(child);
        parents.set(child, cursor);
        child = parent;
      }
      return cursor;
    }
    for (const [target, messages] of byTarget) {
      if (!target.startsWith("#")) continue;
      for (const entry of messages.values()) {
        const parent = messages.get(entry.message.reply_to);
        if (!parent) continue;
        const a = root(coordinate(entry));
        const b = root(coordinate(parent));
        if (a !== b) parents.set(a, b);
      }
    }
    const groups = new Map();
    for (const entry of entries) {
      const key = root(coordinate(entry));
      if (!groups.has(key)) {
        const list = [];
        const colon = key.indexOf(":");
        list.rootEntry = byTarget.get(colon >= 0 ? key.slice(0, colon) : entry.target)?.get(colon >= 0 ? Number(key.slice(colon + 1)) : entry.message.seq) || entry;
        groups.set(key, list);
      }
      groups.get(key).push(entry);
    }
    return [...groups.values()];
  }

  // threadDisclosure keeps the original root headline and the newest reply
  // visible in a consistent M3 list-row layout (32px avatar, primary headline,
  // supporting latest-reply line, and metadata).
  function threadDisclosure(entries, state, actions, byTarget, openKeys) {
    const first = entries.rootEntry || entries[0];
    const last = entries.at(-1);
    const key = `thread:${coordinate(first)}`;
    const rootFromHuman = isFromHumanMessage(first.message);
    const latestReplyToHuman = entries.toReversed().map((e) => humanResponseBadge(e, byTarget)).find(Boolean) || "";
    const details = element("details", `thread-disclosure${rootFromHuman ? " is-human-message" : ""}${latestReplyToHuman ? " is-human-response" : ""}`);
    details.dataset.disclosureKey = key;
    details.dataset.anchorVersion = `${entries.length}:${last.message.seq}`;
    details.open = openKeys.has(key) || entries.some((entry) => selected(entry, state));

    const lastFromHuman = isFromHumanMessage(last.message);
    const senderColor = senderColorClass(last.message.sender, state, lastFromHuman);
    const metaRow = element("span", "message-meta",
      element("strong", `message-sender ${senderColor}`.trim(), `@${last.message.sender}`),
      ...(rootFromHuman ? [element("span", "human-pill", "Human thread (", handleBadgeNode(first.message.sender, state, "", rootFromHuman), ")")] : []),
      ...(latestReplyToHuman ? [element("span", "human-response-pill", latestReplyToHuman)] : []),
      element("span", "message-coordinate", `Thread ${first.target}#${first.message.seq} → #${last.message.seq}`),
      element("span", "thread-count-pill", `${entries.length} messages`),
      element("time", "message-time", `${age(last.message.sent_at_ms)} ago · ${clock(last.message.sent_at_ms)}`));

    const contentCol = element("span", "row-main",
      metaRow,
      element("strong", "thread-title", headline(first.message.text)),
      element("span", "thread-latest", "Latest (", handleBadgeNode(last.message.sender, state, "", lastFromHuman), ` #${last.message.seq}): ${headline(last.message.text)}`));

    const summary = element("summary", "thread-summary", avatarNode(last.message.sender, state, "", lastFromHuman), contentCol);
    summary.dataset.focusKey = key;
    details.append(summary);
    details.addEventListener("toggle", () => {
      if (!details.open && entries.some((entry) => selected(entry, state))) actions.selectMessage(null, 0);
    });

    const body = element("div", "thread-messages");
    for (const entry of entries.toReversed()) body.append(messageDisclosure(entry, state, actions, byTarget, openKeys));
    details.append(body);
    return details;
  }

  // messageDisclosure renders a single message row with its Reply action inside
  // the expanded .message-actions bar so collapsed rows stay uncluttered.
  function messageDisclosure(entry, state, actions, byTarget, openKeys) {
    const message = entry.message;
    const key = coordinate(entry);
    const fromHuman = isFromHumanMessage(message);
    const toHumanBadge = humanResponseBadge(entry, byTarget);
    const details = element("details", `message-disclosure${fromHuman ? " is-human-message" : ""}${toHumanBadge ? " is-human-response" : ""}`);
    details.dataset.messageKey = key;
    details.dataset.disclosureKey = `message:${key}`;
    details.dataset.anchorVersion = `1:${message.seq}`;
    details.open = openKeys.has(`message:${key}`) || selected(entry, state);
    details.classList.toggle("is-selected", selected(entry, state));

    const senderColor = senderColorClass(message.sender, state, fromHuman);
    const meta = element("span", "message-meta",
      element("strong", `message-sender ${senderColor}`.trim(), `@${message.sender}`),
      ...(fromHuman ? [element("span", "human-pill", "Human")] : []),
      ...(toHumanBadge ? [element("span", "human-response-pill", toHumanBadge)] : []),
      element("span", "message-coordinate", `${entry.target}#${message.seq}`),
      element("time", "message-time", `${age(message.sent_at_ms)} ago · ${clock(message.sent_at_ms)}`));

    const second = secondLine(message.text);
    const contentCol = element("span", "row-main",
      meta,
      element("span", "message-headline", headline(message.text)),
      ...(second ? [element("span", "message-supporting", second)] : []));

    const summary = element("summary", "message-summary", avatarNode(message.sender, state, "", fromHuman), contentCol);
    summary.dataset.focusKey = `message:${key}`;
    details.append(summary);
    details.addEventListener("toggle", () => {
      if (!details.open && selected(entry, state)) actions.selectMessage(null, 0);
    });

    const body = element("div", "message-body", formattedText(message.text));
    const controls = element("div", "message-actions");
    if (entry.target.startsWith("#")) {
      controls.append(actionButton([svgIcon("reply", "icon icon-xs"), "Reply"], `reply:${key}`, () => {
        actions.setReplyTo(entry.target, message.seq, message.sender);
      }));
      const parent = byTarget.get(entry.target)?.get(message.reply_to);
      if (parent) {
        controls.append(actionButton(
          [`In reply to #${parent.message.seq} (`, handleBadgeNode(parent.message.sender, state, "", isFromHumanMessage(parent.message)), ")"],
          `parent:${key}`,
          () => actions.selectMessage(entry.target, parent.message.seq)
        ));
      } else if (message.reply_to) {
        controls.append(element("span", "message-reference", `In reply to #${message.reply_to} (older than retained history)`));
      }
    } else if (message.reply_to) {
      controls.append(element("span", "message-reference", `Reply reference #${message.reply_to}`));
    }
    for (const reference of artifacts(message.text)) {
      controls.append(actionButton(reference, `message-artifact:${key}:${reference}`, () => actions.setFilterArtifact(reference)));
    }
    details.append(body);
    if (controls.childElementCount > 0) details.append(controls);
    return details;
  }

  function renderSelection(root, state, actions, entries, visible) {
    const choice = state.selectedMessage;
    if (!choice) {
      if (!root.dataset.agent) root.replaceChildren();
      return;
    }
    const entry = entries.find((item) => item.target === choice.target && item.message.seq === choice.seq);
    delete root.dataset.agent;
    if (visible.some((item) => selected(item, state))) {
      root.replaceChildren();
      return;
    }
    if (!entry) {
      root.replaceChildren(empty("Message is no longer retained", "Choose another message from the timeline."));
      return;
    }
    const nodes = [
      element("div", "details-heading",
        element("h3", "", `${entry.target}#${entry.message.seq}`),
        iconButton("close", "Close", "close-message", () => actions.selectMessage(null, 0))),
      element("p", "message-meta", handleBadgeNode(entry.message.sender, state, "", isFromHumanMessage(entry.message)), ` · ${age(entry.message.sent_at_ms)} ago · ${clock(entry.message.sent_at_ms)}`),
      element("div", "message-body", formattedText(entry.message.text)),
    ];
    if (entry.target.startsWith("#")) {
      nodes.push(actionButton([svgIcon("reply", "icon icon-xs"), "Reply"], "reply-selected",
        () => actions.setReplyTo(entry.target, entry.message.seq, entry.message.sender)));
    }
    replaceKeepingFocus(root, nodes);
  }

  function isFromHumanMessage(message) {
    return message?.human === true;
  }

  function humanResponseBadge(entry, byTarget) {
    const msg = entry.message;
    if (isFromHumanMessage(msg)) return "";
    if (entry.target.startsWith("#") && msg.reply_to > 0) {
      const parent = byTarget.get(entry.target)?.get(msg.reply_to);
      if (parent && isFromHumanMessage(parent.message)) {
        return `↩ Reply to @${parent.message.sender} (#${parent.message.seq})`;
      }
    }
    return "";
  }

  function avatarNode(handle, state, model = "", isHuman = false) {
    const colorClass = senderColorClass(handle, state, isHuman);
    const label = String(handle || "?").slice(0, 2);
    const node = element("span", `agent-avatar ${colorClass}`.trim());
    node.setAttribute("aria-hidden", "true");
    const modelHint = model || (state?.view?.agents || []).find((a) => a.handle === handle)?.model || "";
    const identity = window.ChorusUI?.resolveIdentity ? window.ChorusUI.resolveIdentity(handle, modelHint, isHuman) : null;
    if (identity?.model && identity.model !== "human") node.title = `@${handle} · ${identity.model}`;
    if ((identity?.iconUrl || identity?.avatarUrl) && !isHuman) {
      node.dataset.provider = identity.provider || "";
      const img = document.createElement("img");
      img.className = `agent-brand-logo provider-${identity.provider}`.trim();
      img.dataset.provider = identity.provider || "";
      img.src = identity.iconUrl || identity.avatarUrl;
      img.alt = "";
      img.addEventListener("error", () => { delete node.dataset.provider; node.textContent = label; });
      node.append(img);
    } else {
      node.textContent = label;
    }
    return node;
  }

  function handleBadgeNode(handle, state, extraClass = "", isHuman = false) {
    const modelHint = (state?.view?.agents || []).find((a) => a.handle === handle)?.model || "";
    return window.ChorusUI?.handleBadge
      ? window.ChorusUI.handleBadge(handle, modelHint, extraClass, isHuman)
      : element("span", extraClass, `@${handle}`);
  }

  function mentionedHandles(text) {
    const result = new Map();
    for (const match of text.matchAll(/(^|[^a-zA-Z0-9_-])@([a-zA-Z0-9][a-zA-Z0-9_-]{0,31})(?![a-zA-Z0-9_-])/g)) {
      if (!result.has(match[2].toLowerCase())) result.set(match[2].toLowerCase(), match[2]);
    }
    return [...result.values()];
  }

  function artifacts(text) {
    const result = new Set();
    const patterns = [/\bPR\s+#\d+\b/g, /\b[0-9a-f]{7,40}\b/gi, /\b(?:[a-zA-Z0-9_.-]+\/)+[a-zA-Z0-9_.-]+\.[a-zA-Z0-9]+\b/g];
    for (const pattern of patterns) for (const match of text.matchAll(pattern)) result.add(match[0]);
    return [...result];
  }

  function formattedText(text) {
    const fragment = document.createDocumentFragment();
    for (const part of text.split(/(```[\s\S]*?```)/g)) {
      if (part.startsWith("```") && part.endsWith("```")) {
        fragment.append(element("pre", "message-code", element("code", "", part.slice(3, -3).replace(/^[a-zA-Z0-9_-]+\n/, ""))));
      } else {
        for (const inline of part.split(/(`[^`\n]+`)/g)) {
          if (inline.startsWith("`") && inline.endsWith("`")) fragment.append(element("code", "", inline.slice(1, -1)));
          else appendTextWithMentions(fragment, inline);
        }
      }
    }
    return fragment;
  }

  function appendTextWithMentions(target, text) {
    const pattern = /(^|[^a-zA-Z0-9_-])@([a-zA-Z0-9][a-zA-Z0-9_-]{0,31})(?![a-zA-Z0-9_-])/g;
    let lastIndex = 0;
    for (const match of text.matchAll(pattern)) {
      const prefix = match[1] || "";
      const handle = match[2];
      const matchStart = match.index + prefix.length;
      if (matchStart > lastIndex) target.append(document.createTextNode(text.slice(lastIndex, matchStart)));
      const identity = window.ChorusUI?.resolveIdentity ? window.ChorusUI.resolveIdentity(handle) : null;
      target.append(identity?.iconUrl && window.ChorusUI?.handleBadge
        ? window.ChorusUI.handleBadge(handle) : document.createTextNode(`@${handle}`));
      lastIndex = matchStart + 1 + handle.length;
    }
    if (lastIndex < text.length) target.append(document.createTextNode(text.slice(lastIndex)));
  }

  function headline(text) {
    return text.split(/\r?\n/).find((line) => line.trim().length > 0)?.trim() || "Empty message";
  }

  function secondLine(text) {
    const lines = text.split(/\r?\n/).map((line) => line.trim()).filter((line) => line.length > 0);
    return lines.length > 1 ? lines[1] : "";
  }

  function coordinate(entry) { return `${entry.target}:${entry.message.seq}`; }
  function senderColorClass(handle, state, isHuman = false) {
    return window.ChorusUI?.agentColorClass ? window.ChorusUI.agentColorClass(handle, isHuman ? "human" : "") : "";
  }
  function selected(entry, state) {
    return state.selectedMessage?.target === entry.target && state.selectedMessage.seq === entry.message.seq;
  }
  function clock(milliseconds) {
    return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(milliseconds);
  }
  function age(milliseconds) {
    const seconds = Math.max(0, Math.floor((Date.now() - milliseconds) / 1000));
    if (seconds < 60) return `${seconds}s`;
    return seconds < 3600 ? `${Math.floor(seconds / 60)}m` : `${Math.floor(seconds / 3600)}h`;
  }
  function svgIcon(name, className = "icon") {
    return window.ChorusUI?.svgIcon ? window.ChorusUI.svgIcon(name, className) : document.createElement("span");
  }
  function iconButton(iconName, label, key, action, extraClass = "") {
    const button = element("button", `icon-btn ${extraClass}`.trim(), svgIcon(iconName));
    button.type = "button";
    button.title = label;
    button.setAttribute("aria-label", label);
    button.dataset.focusKey = key;
    button.addEventListener("click", action);
    return button;
  }
  function actionButton(label, key, action) {
    const button = element("button", "stream-action", ...(Array.isArray(label) ? label : [label]));
    button.type = "button";
    button.dataset.focusKey = key;
    button.addEventListener("click", action);
    return button;
  }
  function element(tag, className, ...children) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    for (const child of children) node.append(typeof child === "string" ? document.createTextNode(child) : child);
    return node;
  }
  function empty(title, description) {
    return element("div", "empty-state", element("h3", "", title), element("p", "", description));
  }
  function replaceKeepingFocus(root, nodes) {
    const active = document.activeElement;
    const focusKey = root && root.contains(active) ? active.dataset.focusKey : null;
    root.replaceChildren(...nodes);
    if (focusKey) [...root.querySelectorAll("[data-focus-key]")]
      .find((node) => node.dataset.focusKey === focusKey)?.focus({ preventScroll: true });
  }
})();
