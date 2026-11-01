// Render retained messages and their disclosures from the controller's current
// snapshot. The controller owns network calls, filters, selection, and sending.
(() => {
  "use strict";

  window.ChorusStream = Object.freeze({
    render,
    captureScrollSnapshot,
    restoreScrollSnapshot,
    resetDisclosures,
  });

  // Displayed previews belong only to this page visit and the retained messages
  // in the current swarm. Displaying a preview does not mean reading its text.
  const displayedPreviews = new Set();
  // Switching swarms starts a new display context even when sequence IDs match.
  let previewSwarm = "";
  // The observer is replaced after each render so removed rows are not retained.
  let previewObserver = null;
  document.addEventListener("visibilitychange", observeUnshownPreviews);

  // render updates the message surfaces without keeping another copy of swarm
  // data. Disclosure choices and reading position belong to the existing DOM.
  function render(els, state, actions) {
    previewObserver?.disconnect();
    previewObserver = null;
    if (previewSwarm !== state.activeSwarm) {
      displayedPreviews.clear();
      previewSwarm = state.activeSwarm;
    }
    if (state.view === null) {
      for (const root of [els.agentStripEl, els.mentionsEl, els.detailsEl]) {
        root.replaceChildren();
      }
      renderPin(els.pinEl, state, actions);
      delete els.detailsEl.dataset.agent;
      els.titleEl.textContent = "Messages";
      els.subtitleEl.textContent = "Loading swarm…";
      els.streamEl.replaceChildren(empty("Loading messages", "Connecting to the swarm…"));
      return;
    }
    const entries = retainedMessages(state.view);
    const retainedKeys = new Set(entries.map(coordinate));
    for (const key of displayedPreviews) {
      if (!retainedKeys.has(key)) displayedPreviews.delete(key);
    }
    const targetEntries = selectedMessages(entries, state);
    const visible = targetEntries.filter((entry) => matches(entry, state));
    const scrollSnapshot = els.scrollSnapshot || captureScrollSnapshot(els.streamEl, els.scrollEl);

    renderAgents(els, state, actions, entries);
    renderPin(els.pinEl, state, actions);
    renderMentions(els.mentionsEl, targetEntries, state, actions, entries);
    const selectionChanged = renderMessages(els.streamEl, visible, targetEntries, state, actions);
    renderSelection(els.detailsEl, state, actions, entries, visible);

    restoreScrollSnapshot(els.streamEl, els.scrollEl, scrollSnapshot);
    revealSelectedMessage(els.streamEl, state, selectionChanged);
    previewObserver = new IntersectionObserver(exposePreviews, { root: els.scrollEl });
    observeUnshownPreviews();

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
      let modelNode = null;
      if (agent.model && agent.model !== "human") {
        modelNode = element("span", "agent-model-inline", agent.model);
        modelNode.title = agent.model;
      }
      const headerRow = element("span", "agent-card-top",
        avatarNode(agent.handle, state, agent.registered ? agent.model || "" : "", human),
        element("strong", `agent-name ${colorClass}`.trim(), `@${agent.handle}`),
        ...(human ? [element("span", "human-pill", "Human")] : []),
        ...(modelNode ? [modelNode] : []),
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
        ...(entry ? [`${entry.target}#${entry.message.seq} (${age(entry.message.sent_at_ms)} ago): `, formattedPreview(entry.message.text)] : ["No retained messages"])));
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
    const copyWatchBtn = actionButton([svgIcon("copy", "icon icon-xs"), `Copy: ${watchCmd}`], `copy-watch:${agent.handle}`, async (ev) => {
      const btn = ev.currentTarget;
      try {
        if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
          throw new Error("clipboard unavailable");
        }
        await navigator.clipboard.writeText(watchCmd);
        btn.replaceChildren(svgIcon("check", "icon icon-xs"), `Copied: ${watchCmd}`);
      } catch {
        btn.textContent = "Copy unavailable";
      }
      setTimeout(() => {
        if (btn.isConnected) btn.replaceChildren(svgIcon("copy", "icon icon-xs"), `Copy: ${watchCmd}`);
      }, 2000);
    });
    copyWatchBtn.setAttribute("aria-live", "polite");

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
    if (latest) nodes.push(element("p", "agent-last-message", `Latest message (${latest.target}#${latest.message.seq}): `, formattedPreview(latest.message.text)));
    nodes.push(element("div", "detail-actions",
      actionButton(["Direct message ", handleBadgeNode(agent.handle, state, "", isHuman)], `dm:${agent.handle}`, () => actions.composeDM(agent.handle)),
      actionButton(state.filterAgent === agent.handle ? "Clear agent filter" : ["Filter by ", handleBadgeNode(agent.handle, state, "", isHuman)], `filter-agent:${agent.handle}`, () => actions.setFilterAgent(agent.handle)),
      copyWatchBtn));
    replaceKeepingFocus(root, nodes);
  }

  // renderPin shows the selected channel's pin without rebuilding an open
  // editor. The one editor owns its draft and original swarm/channel until
  // the reader cancels or the server confirms the update.
  function renderPin(root, state, actions) {
    const editor = root.querySelector(".pin-editor");
    if (editor) {
      const pending = editor.getAttribute("aria-busy") === "true";
      const sameSwarm = state.activeSwarm === editor.dataset.swarm
        && state.view?.swarm === editor.dataset.swarm;
      editor.querySelector("[type=submit]").disabled = pending || !sameSwarm;
      const scopeNotice = editor.querySelector(".pin-edit-scope-notice");
      scopeNotice.hidden = sameSwarm;
      scopeNotice.textContent = `Return to swarm ${editor.dataset.swarm} to update this pin.`;
      return;
    }
    const channel = state.selectedTarget?.kind === "channel"
      ? state.view?.channels.find((item) => item.name === state.selectedTarget.name) : null;
    if (!channel?.pin) {
      root.replaceChildren();
      delete root.dataset.pinKey;
      return;
    }
    const pin = channel.pin;
    const key = `${state.activeSwarm}:${channel.name}`;
    const previous = root.querySelector("details");
    const sameChannel = root.dataset.pinKey === key;
    if (!sameChannel) {
      root.dataset.pinMode = "fields";
    }
    const changed = sameChannel && root.dataset.pinSeq !== String(pin.seq);
    const updated = changed || (sameChannel && root.dataset.updated === "true");
    const disclosure = element("details", "pin-disclosure");
    disclosure.open = sameChannel && previous ? previous.open : false;
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

    const clauses = pinClauses(pin.text);
    const raw = element("pre", "pin-raw-text", pin.text);
    const structured = renderPinStructured(clauses);
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
    const editBtn = iconButton("edit", "Edit pin", `edit-pin:${key}`, () => {
      root.replaceChildren(renderPinEditor(root, state.activeSwarm, channel.name, pin.text, key, state, actions));
      root.querySelector(".pin-edit-textarea")?.focus();
    }, "pin-edit-btn");
    wrapper.append(editBtn);
    replaceKeepingFocus(root, [wrapper]);
  }

  // renderPinStructured separates the documented fields for reading. Owners
  // remain authored text: known handles receive badges without inferred work
  // assignments, removed punctuation, or a new ownership grammar.
  function renderPinStructured(clauses) {
    const container = element("div", "pin-structured");
    for (const clause of clauses) {
      const kind = clause.key.toLowerCase();
      if (kind === "goal") {
        container.append(element("div", "pin-section",
          element("h3", "pin-section-label", "Goal"),
          element("div", "pin-goal-block", formattedText(clause.value))));
      } else if (kind === "owners") {
        container.append(element("div", "pin-section",
          element("h3", "pin-section-label", "Owners"),
          element("div", "pin-owner-text", formattedText(clause.value))));
      } else if (kind === "verify") {
        const copyBtn = actionButton("Copy command", `copy-verify:${clause.value}`, async (ev) => {
          const btn = ev.currentTarget;
          try {
            await navigator.clipboard.writeText(clause.value);
            btn.textContent = "Copied";
          } catch {
            btn.textContent = "Copy unavailable";
          }
        });
        copyBtn.setAttribute("aria-live", "polite");
        container.append(element("div", "pin-section",
          element("h3", "pin-section-label", "Verify"),
          element("div", "pin-verify-row", element("code", "pin-verify-code", clause.value), copyBtn)));
      }
    }
    return container;
  }

  // renderPinEditor keeps text in one form, independent of the chat composer.
  // Cancel and Escape perform no request; after submit they are disabled until
  // the server responds because removing a form cannot undo a published pin.
  function renderPinEditor(root, swarm, channelName, originalText, key, state, actions) {
    const form = element("form", "pin-editor pin-disclosure");
    form.dataset.swarm = swarm;
    form.setAttribute("aria-label", `Edit pin in ${channelName}, swarm ${swarm}`);
    const label = element("label", "pin-edit-label", `Edit pin in ${channelName} · ${swarm}`);
    label.htmlFor = "pin-edit-text";
    const textarea = element("textarea", "pin-edit-textarea", originalText);
    textarea.id = "pin-edit-text";
    textarea.rows = 5;
    textarea.dataset.focusKey = `pin-textarea:${key}`;
    textarea.setAttribute("aria-describedby", "pin-edit-help pin-edit-error");
    const help = element("p", "pin-edit-help", "Updating replaces this channel’s pinned message. Your message draft stays separate.");
    help.id = "pin-edit-help";
    const scopeNotice = element("p", "pin-edit-scope-notice");
    scopeNotice.hidden = true;
    const errEl = element("p", "pin-edit-error");
    errEl.id = "pin-edit-error";
    errEl.setAttribute("role", "alert");
    errEl.hidden = true;
    const status = element("p", "pin-edit-status");
    status.setAttribute("role", "status");

    // Closing discards only this unsent form and restores a visible control.
    const closeEditor = () => {
      if (form.getAttribute("aria-busy") === "true") return;
      root.replaceChildren();
      renderPin(root, state, actions);
      root.querySelector("details.pin-disclosure")?.setAttribute("open", "");
      const returnTo = root.querySelector(".pin-edit-btn") || document.getElementById("swarm-select");
      returnTo?.focus({ preventScroll: true });
    };

    form.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && !e.isComposing) {
        e.preventDefault();
        closeEditor();
      }
    });

    const cancelBtn = element("button", "btn-text", "Cancel");
    cancelBtn.type = "button";
    cancelBtn.dataset.focusKey = `pin-cancel:${key}`;
    cancelBtn.addEventListener("click", closeEditor);

    const saveBtn = element("button", "btn-primary pin-save-btn", "Update pin");
    saveBtn.type = "submit";
    saveBtn.dataset.focusKey = `pin-save:${key}`;
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (saveBtn.disabled) return;
      errEl.hidden = true;
      textarea.removeAttribute("aria-invalid");
      form.setAttribute("aria-busy", "true");
      saveBtn.disabled = true;
      cancelBtn.disabled = true;
      textarea.readOnly = true;
      status.textContent = "Updating pin…";
      try {
        await actions.updatePin(swarm, channelName, textarea.value);
        root.replaceChildren();
        renderPin(root, state, actions);
        root.querySelector(".pin-edit-btn")?.focus({ preventScroll: true });
      } catch (err) {
        errEl.textContent = err.message || "Failed to update pin.";
        errEl.hidden = false;
        textarea.setAttribute("aria-invalid", "true");
        status.textContent = "Your changes are kept here. Check the error before trying again.";
      } finally {
        form.setAttribute("aria-busy", "false");
        cancelBtn.disabled = false;
        textarea.readOnly = false;
        if (root.contains(form)) renderPin(root, state, actions);
      }
    });

    form.append(label, help, scopeNotice, textarea, errEl, status,
      element("div", "pin-edit-actions", cancelBtn, saveBtn));
    return form;
  }

  // cleanPinHeadline removes only the display convention's Goal label; the
  // original message remains unchanged in its text view and edit form.
  function cleanPinHeadline(text) {
    const goal = pinClauses(text).find((clause) => clause.key.toLowerCase() === "goal");
    return headline(goal?.value || text).replace(/^\s*Goal\s*:\s*/i, "");
  }

  // pinClauses recognizes complete labelled segments for display only. Any
  // unrecognized segment leaves the entire original message in the text view.
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

  // renderMentions shows written mentions of known participants and @all.
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
    replaceKeepingFocus(root, mentions.length > 0 ? [disclosure] : []);
  }

  // preparePreviewCue names only the text actually shown in this summary: one
  // message, or a thread's root and latest message. Hidden replies are separate.
  function preparePreviewCue(summary, first, latest = first) {
    summary.dataset.previewKey = coordinate(first);
    summary.dataset.latestPreviewKey = coordinate(latest);
    updatePreviewCue(summary);
  }

  // updatePreviewCue keeps the small dot and its accessible description tied to
  // whether this summary's previews have appeared during the current visit.
  function updatePreviewCue(summary) {
    const unshown = !displayedPreviews.has(summary.dataset.previewKey)
      || !displayedPreviews.has(summary.dataset.latestPreviewKey);
    summary.classList.toggle("has-unshown-preview", unshown);
    if (unshown) {
      summary.setAttribute("aria-description", "Preview not yet shown during this visit");
    } else {
      summary.removeAttribute("aria-description");
    }
    return unshown;
  }

  // observeUnshownPreviews also runs when a background tab becomes visible:
  // observing again requests fresh intersections even if no row moved.
  function observeUnshownPreviews() {
    if (!previewObserver || document.visibilityState !== "visible") return;
    previewObserver.disconnect();
    for (const summary of previewObserver.root.querySelectorAll("summary.has-unshown-preview")) {
      previewObserver.observe(summary);
      if (summary.classList.contains("message-summary")) {
        previewObserver.observe(summary.nextElementSibling);
      }
    }
  }

  // exposePreviews settles cues when collapsed previews or expanded message
  // text enter the visible stage. Closed threads never expose hidden replies.
  function exposePreviews(changes, observer) {
    if (observer !== previewObserver || document.visibilityState !== "visible") return;
    for (const change of changes) {
      if (!change.isIntersecting || change.intersectionRect.width <= 0
        || change.intersectionRect.height <= 0) continue;
      const summary = change.target.tagName === "SUMMARY"
        ? change.target : change.target.previousElementSibling;
      if (change.target === summary && summary.classList.contains("message-summary")
        && summary.parentElement.open) continue;
      let parent = change.target === summary
        ? summary.parentElement.parentElement : change.target.parentElement;
      while (parent && (parent.tagName !== "DETAILS" || parent.open)) parent = parent.parentElement;
      if (parent) continue;
      displayedPreviews.add(summary.dataset.previewKey);
      displayedPreviews.add(summary.dataset.latestPreviewKey);
    }
    // A thread preview and an individual message can show the same text. Keep
    // their dots consistent without retaining another map of rendered rows.
    for (const summary of observer.root.querySelectorAll("summary[data-preview-key]")) {
      if (!updatePreviewCue(summary)) {
        observer.unobserve(summary);
        if (summary.classList.contains("message-summary")) observer.unobserve(summary.nextElementSibling);
      }
    }
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
    if (!root || !scrollEl) return { oldTop: 0, hasOpenDisclosure: false, candidates: [] };
    const oldTop = scrollEl.scrollTop;
    const hasOpenDisclosure = Boolean(root.querySelector("details[open]"));
    const rootTop = scrollEl.getBoundingClientRect().top;
    const rootBottom = scrollEl.getBoundingClientRect().bottom;
    const candidates = [...root.querySelectorAll("[data-disclosure-key]")]
      .filter((node) => {
        if (!isVisibleAnchorRow(node, root)) return false;
        const rect = node.getBoundingClientRect();
        return rect.bottom >= rootTop && rect.top <= rootBottom;
      })
      .map((node) => ({
        key: node.dataset.disclosureKey,
        version: node.dataset.anchorVersion || "",
        offset: node.getBoundingClientRect().top - rootTop,
        priority: Boolean(node.open || node.classList.contains("is-selected")),
      }));
    return { oldTop, hasOpenDisclosure, candidates };
  }

  // restoreScrollSnapshot keeps the reader's visible anchor row stationary when
  // scrolled down or reading an open/selected disclosure, while staying at the
  // newest edge when at the top with no open disclosure.
  function restoreScrollSnapshot(root, scrollEl, snapshot) {
    if (!root || !scrollEl || !snapshot) return;
    const hasPriority = snapshot.candidates.some((c) => c.priority);
    if (snapshot.oldTop <= 4 && !snapshot.hasOpenDisclosure && !hasPriority) return;
    scrollEl.scrollTop = snapshot.oldTop;
    const afterRows = [...root.querySelectorAll("[data-disclosure-key]")].filter((node) => isVisibleAnchorRow(node, root));
    const ordered = [
      ...snapshot.candidates.filter((c) => c.priority),
      ...snapshot.candidates.filter((c) => !c.priority),
    ];
    let match = null;
    for (const cand of ordered) {
      const node = afterRows.find((row) => row.dataset.disclosureKey === cand.key && (row.dataset.anchorVersion || "") === cand.version);
      if (node) { match = { cand, node }; break; }
    }
    if (!match) {
      for (const cand of ordered) {
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

  // resetDisclosures resets the stream's disclosure state when Jump to latest
  // returns to the live head, collapsing rows unless Expand All is active.
  function resetDisclosures(root, state) {
    if (!root) return;
    if (state?.expandAllMessages) {
      delete root.dataset.expandAll;
    } else {
      for (const d of root.querySelectorAll("details[open]")) d.open = false;
    }
  }

  // renderMessages stacks newest threads and newest messages at the top of the
  // feed so the latest conversation is immediately visible below the timeline
  // without scrolling to the bottom of the page.
  function renderMessages(root, entries, targetEntries, state, actions) {
    const prevRenderedKeys = new Set([...root.querySelectorAll("details[data-disclosure-key]")].map((node) => node.dataset.disclosureKey));
    const openKeys = new Set([...root.querySelectorAll("details[open][data-disclosure-key]")].map((node) => node.dataset.disclosureKey));
    const expandAll = state.expandAllMessages === true;
    if (root.dataset.expandAll !== String(expandAll)) {
      openKeys.clear();
      prevRenderedKeys.clear();
    }
    if (expandAll) {
      for (const entry of targetEntries) {
        const msgKey = `message:${coordinate(entry)}`;
        const threadKey = `thread:${coordinate(entry)}`;
        if (!prevRenderedKeys.has(msgKey)) openKeys.add(msgKey);
        if (!prevRenderedKeys.has(threadKey)) openKeys.add(threadKey);
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
    details.open = openKeys.has(key) || entries.some((entry) => openKeys.has(`message:${coordinate(entry)}`) || selected(entry, state));

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
      element("strong", "thread-title", formattedPreview(first.message.text)),
      element("span", "thread-latest", "Latest (", handleBadgeNode(last.message.sender, state, "", lastFromHuman), ` #${last.message.seq}): `, formattedPreview(last.message.text)));

    const summary = element("summary", "thread-summary", avatarNode(last.message.sender, state, "", lastFromHuman), contentCol);
    summary.dataset.focusKey = key;
    preparePreviewCue(summary, first, last);
    details.append(summary);
    details.addEventListener("toggle", () => {
      if (!details.open) {
        for (const child of details.querySelectorAll("details[open]")) child.open = false;
        if (entries.some((entry) => selected(entry, state))) actions.selectMessage(null, 0);
      }
    });

    const body = element("div", "thread-messages");
    for (const entry of entries.toReversed()) body.append(messageDisclosure(entry, state, actions, byTarget, openKeys));
    details.append(body);
    return details;
  }

  // renderParentContext builds a clickable quoted-parent banner when a message
  // replies to an earlier message in the same channel or DM stream.
  function renderParentContext(entry, state, actions, byTarget, keyPrefix) {
    const message = entry.message;
    if (!message.reply_to) return null;
    if (entry.target.startsWith("#")) {
      const parent = byTarget?.get(entry.target)?.get(message.reply_to);
      if (parent) {
        const parentFromHuman = isFromHumanMessage(parent.message);
        const btn = element("button", "message-parent-quote",
          svgIcon("reply", "icon icon-xs"),
          element("span", "message-parent-label", "In reply to"),
          handleBadgeNode(parent.message.sender, state, "", parentFromHuman),
          element("span", "message-coordinate", `#${parent.message.seq}`),
          element("span", "message-parent-excerpt", formattedPreview(parent.message.text)));
        btn.type = "button";
        btn.dataset.focusKey = `parent:${keyPrefix}`;
        btn.addEventListener("click", () => actions.selectMessage(entry.target, parent.message.seq));
        return btn;
      }
      return element("div", "message-parent-quote is-unretained",
        svgIcon("reply", "icon icon-xs"),
        `In reply to #${message.reply_to} (older than retained history)`);
    }
    return element("div", "message-parent-quote is-unretained",
      svgIcon("reply", "icon icon-xs"),
      `In reply to #${message.reply_to}`);
  }

  // copyTextButton creates a compact action button that copies text to the
  // clipboard with inline confirmation or accessible failure feedback.
  function copyTextButton(text, focusKey) {
    const btn = actionButton([svgIcon("copy", "icon icon-xs"), "Copy text"], focusKey, async (ev) => {
      const target = ev.currentTarget;
      try {
        if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
          throw new Error("clipboard unavailable");
        }
        await navigator.clipboard.writeText(text);
        target.replaceChildren(svgIcon("check", "icon icon-xs"), "Copied");
      } catch {
        target.textContent = "Copy unavailable";
      }
      setTimeout(() => {
        if (target.isConnected) target.replaceChildren(svgIcon("copy", "icon icon-xs"), "Copy text");
      }, 2000);
    });
    btn.setAttribute("aria-live", "polite");
    return btn;
  }

  // messageDisclosure renders a single message row with a structured M3 detail
  // pane (quoted reply context, full formatted text, and Reply/Copy actions)
  // aligned to the author's content column.
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
      ...(!toHumanBadge && message.reply_to > 0 ? [element("span", "message-reply-badge", `↩ #${message.reply_to}`)] : []),
      element("span", "message-coordinate", `${entry.target}#${message.seq}`),
      element("time", "message-time", `${age(message.sent_at_ms)} ago · ${clock(message.sent_at_ms)}`));

    const second = secondLine(message.text);
    const contentCol = element("span", "row-main",
      meta,
      element("span", "message-headline", formattedPreview(message.text)),
      ...(second ? [element("span", "message-supporting", formattedPreview(message.text, 1))] : []));

    const summary = element("summary", "message-summary", avatarNode(message.sender, state, "", fromHuman), contentCol);
    summary.dataset.focusKey = `message:${key}`;
    preparePreviewCue(summary, entry);
    details.append(summary);
    details.addEventListener("toggle", () => {
      if (!details.open && selected(entry, state)) actions.selectMessage(null, 0);
    });

    const detailPane = element("div", "message-detail-pane");
    const parentQuote = renderParentContext(entry, state, actions, byTarget, key);
    if (parentQuote) detailPane.append(parentQuote);
    detailPane.append(element("div", "message-body", formattedText(message.text)));

    const controls = element("div", "message-actions");
    if (entry.target.startsWith("#")) {
      controls.append(actionButton([svgIcon("reply", "icon icon-xs"), "Reply"], `reply:${key}`, () => {
        actions.setReplyTo(entry.target, message.seq, message.sender, fromHuman);
      }));
    } else {
      controls.append(actionButton(["Message ", handleBadgeNode(message.sender, state, "", fromHuman)], `dm-reply:${key}`, () => {
        actions.composeDM(message.sender);
      }));
    }
    controls.append(copyTextButton(message.text, `copy:${key}`));
    detailPane.append(controls);
    details.append(detailPane);
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
    const byTarget = new Map();
    for (const item of entries) {
      if (!byTarget.has(item.target)) byTarget.set(item.target, new Map());
      byTarget.get(item.target).set(item.message.seq, item);
    }
    const fromHuman = isFromHumanMessage(entry.message);
    const toHumanBadge = humanResponseBadge(entry, byTarget);
    const senderColor = senderColorClass(entry.message.sender, state, fromHuman);
    const headerLeft = element("div", "details-message-header",
      avatarNode(entry.message.sender, state, "", fromHuman),
      element("span", "message-meta",
        element("strong", `message-sender ${senderColor}`.trim(), `@${entry.message.sender}`),
        ...(fromHuman ? [element("span", "human-pill", "Human")] : []),
        ...(toHumanBadge ? [element("span", "human-response-pill", toHumanBadge)] : []),
        element("span", "message-coordinate", `${entry.target}#${entry.message.seq}`),
        element("time", "message-time", `${age(entry.message.sent_at_ms)} ago · ${clock(entry.message.sent_at_ms)}`)));

    const nodes = [
      element("div", "details-heading",
        headerLeft,
        iconButton("close", "Close", "close-message", () => actions.selectMessage(null, 0))),
    ];
    const parentQuote = renderParentContext(entry, state, actions, byTarget, `selected:${coordinate(entry)}`);
    if (parentQuote) nodes.push(parentQuote);
    nodes.push(element("div", "message-body is-standalone", formattedText(entry.message.text)));
    const controls = element("div", "detail-actions");
    if (entry.target.startsWith("#")) {
      controls.append(actionButton([svgIcon("reply", "icon icon-xs"), "Reply"], "reply-selected",
        () => actions.setReplyTo(entry.target, entry.message.seq, entry.message.sender, fromHuman)));
    } else {
      controls.append(actionButton(["Message ", handleBadgeNode(entry.message.sender, state, "", fromHuman)], "dm-selected",
        () => actions.composeDM(entry.message.sender)));
    }
    controls.append(copyTextButton(entry.message.text, "copy-selected"));
    nodes.push(controls);
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

  // formattedText decorates prose mentions while keeping inline and fenced
  // code literal. The same code boundaries govern the collapsed previews.
  function formattedText(text) {
    const fragment = document.createDocumentFragment();
    for (const part of textParts(text)) {
      if (part.kind === "block") {
        fragment.append(element("pre", "message-code", element("code", "", part.text.slice(3, -3).replace(/^[a-zA-Z0-9_-]+\n/, ""))));
      } else if (part.kind === "inline") {
        fragment.append(element("code", "", part.text.slice(1, -1)));
      } else {
        appendTextWithMentions(fragment, part.text);
      }
    }
    return fragment;
  }

  // formattedPreview selects a nonblank source line without losing its code
  // context. For example, an @mention inside a fence stays plain in line two.
  function formattedPreview(text, lineNumber = 0) {
    const fragment = document.createDocumentFragment();
    let line = null;
    let index = 0;
    for (const match of text.matchAll(/[^\n]+/g)) {
      if (!match[0].trim()) continue;
      if (index++ === lineNumber) {
        line = match;
        break;
      }
    }
    if (!line) {
      if (lineNumber === 0) fragment.append("Empty message");
      return fragment;
    }
    const start = line.index + line[0].search(/\S/);
    const end = line.index + line[0].trimEnd().length;
    let offset = 0;
    for (const part of textParts(text)) {
      if (offset >= end) break;
      const from = Math.max(0, start - offset);
      const to = Math.min(part.text.length, end - offset);
      if (from < to) {
        const excerpt = part.text.slice(from, to);
        if (part.kind === "text") appendTextWithMentions(fragment, excerpt);
        else fragment.append(excerpt);
      }
      offset += part.text.length;
    }
    return fragment;
  }

  // textParts preserves the original delimiters and offsets so all text
  // surfaces can distinguish prose from code before decorating mentions.
  function* textParts(text) {
    for (const part of text.split(/(```[\s\S]*?```)/g)) {
      if (part.startsWith("```") && part.endsWith("```")) {
        yield { kind: "block", text: part };
      } else {
        for (const inline of part.split(/(`[^`\n]+`)/g)) {
          yield { kind: inline.startsWith("`") && inline.endsWith("`") ? "inline" : "text", text: inline };
        }
      }
    }
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
