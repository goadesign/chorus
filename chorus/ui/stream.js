// Render retained messages and their disclosures from the controller's current
// snapshot. The controller owns network calls, filters, selection, and sending.
(() => {
  "use strict";

  window.ChorusStream = Object.freeze({ render });

  // render updates the message surfaces without keeping another copy of swarm
  // data. Disclosure choices and reading position belong to the existing DOM.
  function render(els, state, actions) {
    if (state.view === null) {
      for (const root of [els.agentStripEl, els.pinEl, els.mentionsEl, els.newSinceEl, els.detailsEl]) root.replaceChildren();
      delete els.detailsEl.dataset.agent;
      els.titleEl.textContent = "Messages";
      els.subtitleEl.textContent = "Loading swarm";
      els.streamEl.replaceChildren(empty("Loading messages", "Connecting to the swarm…"));
      return;
    }
    const entries = retainedMessages(state.view);
    const targetEntries = selectedMessages(entries, state);
    const visible = targetEntries.filter((entry) => matches(entry, state));
    renderAgents(els, state, actions, entries);
    renderPin(els.pinEl, state, actions);
    renderMentions(els.mentionsEl, targetEntries, state, actions);
    renderNewMessages(els.newSinceEl, targetEntries, state, actions);
    renderMessages(els.streamEl, els.scrollEl, visible, targetEntries, state, actions);
    renderSelection(els.detailsEl, state, actions, entries, visible);
    const target = state.selectedTarget;
    els.titleEl.textContent = target?.kind === "channel" ? target.name
      : target?.kind === "dm" ? `@${target.a} and @${target.b}` : "All messages";
    els.subtitleEl.textContent = `${visible.length} of ${targetEntries.length} retained messages`;
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
      const sender = entry.message.sender.toLowerCase();
      const recipient = entry.target.slice(1).toLowerCase();
      return (sender === target.a && recipient === target.b)
        || (sender === target.b && recipient === target.a);
    });
  }

  // compareEntries gives equal-time messages from separate streams a stable
  // display order; it does not claim that one caused the other.
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
      const handle = state.filterAgent.toLowerCase();
      if (message.sender.toLowerCase() !== handle
        && entry.target.toLowerCase() !== `@${handle}`
        && !mentionedHandles(message.text).some((mention) => mention.toLowerCase() === handle)) return false;
    }
    if (state.filterArtifact && !artifacts(message.text).includes(state.filterArtifact)) return false;
    const query = state.searchQuery.toLowerCase();
    return !query || `${entry.target} ${message.sender} #${message.seq} ${message.text}`
      .toLowerCase().includes(query);
  }

  // renderAgents shows what each current agent last said, with its full role
  // and heartbeat facts available on demand rather than repeated as badges.
  function renderAgents(els, state, actions, entries) {
    const latest = new Map();
    for (const entry of entries) latest.set(entry.message.sender, entry);
    const nodes = state.view.agents.map((agent) => {
      const entry = latest.get(agent.handle);
      const button = element("button", "agent-summary",
        element("strong", "agent-name", `@${agent.handle}`),
        element("span", "agent-latest", entry ? headline(entry.message.text) : "No retained messages"));
      button.type = "button";
      button.dataset.focusKey = `agent:${agent.handle}`;
      button.setAttribute("aria-label", `Show details for @${agent.handle}`);
      button.addEventListener("click", () => {
        actions.selectMessage(null, 0);
        els.detailsEl.dataset.agent = agent.handle;
        showAgent(els.detailsEl, agent, actions, entry);
      });
      return button;
    });
    if (nodes.length === 0) nodes.push(element("p", "empty-inline", "No agents are currently registered."));
    replaceKeepingFocus(els.agentStripEl, nodes);
    const inspected = els.detailsEl.dataset.agent;
    if (inspected) {
      const agent = state.view.agents.find((item) => item.handle === inspected);
      if (agent) showAgent(els.detailsEl, agent, actions, latest.get(inspected));
      else {
        delete els.detailsEl.dataset.agent;
        els.detailsEl.replaceChildren();
      }
    }
  }

  // showAgent describes current registration facts and sends composing work
  // back to app.js. A heartbeat never becomes a claim that an agent is working.
  function showAgent(root, agent, actions, latest) {
    const close = actionButton("Close", "close-agent", () => {
      delete root.dataset.agent;
      root.replaceChildren();
    });
    const facts = element("dl", "agent-facts",
      element("dt", "", "Last Chorus call or heartbeat"),
      element("dd", "", `${age(agent.last_seen_unix * 1000)} ago`),
      element("dt", "", "Watching on this server"),
      element("dd", "", agent.watching_on_this_server ? "Yes" : "No"),
      element("dt", "", "Channels"),
      element("dd", "", agent.channels.join(", ") || "None"));
    const nodes = [element("div", "details-heading", element("h3", "", `@${agent.handle}`), close)];
    if (agent.role) nodes.push(element("p", "agent-role", agent.role));
    nodes.push(facts);
    if (latest) nodes.push(element("p", "agent-last-message", `Latest message: ${headline(latest.message.text)}`));
    nodes.push(element("div", "detail-actions",
      actionButton("Direct message", `dm:${agent.handle}`, () => actions.composeDM(agent.handle)),
      actionButton("Show messages", `filter-agent:${agent.handle}`, () => actions.setFilterAgent(agent.handle))));
    replaceKeepingFocus(root, nodes);
  }

  // renderPin keeps one pin disclosure and one content mode. A replacement
  // receives an Updated cue without reopening a disclosure the reader closed.
  function renderPin(root, state, actions) {
    const channel = state.selectedTarget?.kind === "channel"
      ? state.view.channels.find((item) => item.name === state.selectedTarget.name) : null;
    if (!channel?.pin) {
      root.replaceChildren();
      delete root.dataset.pinKey;
      return;
    }
    const pin = channel.pin;
    const key = `${state.activeSwarm}:${channel.name}`;
    const previous = root.querySelector("details");
    const sameChannel = root.dataset.pinKey === key;
    const changed = sameChannel && root.dataset.pinSeq !== String(pin.seq);
    const updated = changed || (sameChannel && root.dataset.updated === "true");
    const disclosure = element("details", "pin-disclosure");
    disclosure.open = sameChannel && previous ? previous.open : false;
    disclosure.dataset.disclosureKey = `pin:${key}`;
    const newer = channel.messages.filter((message) => message.seq > pin.seq).length;
    const summary = element("summary", "pin-summary",
      element("strong", "pin-title", `Pin: ${headline(pin.text)}`),
      element("span", "pin-meta", `Pinned at #${pin.seq} · ${newer} newer retained messages`));
    summary.dataset.focusKey = `pin:${key}`;
    if (updated) summary.append(element("span", "pin-updated", "Updated"));
    disclosure.append(summary);
    const clauses = pinClauses(pin.text);
    const raw = element("pre", "pin-raw-text", pin.text);
    const fields = element("dl", "pin-clauses");
    for (const clause of clauses) fields.append(element("dt", "", clause.key), element("dd", "", formattedText(clause.value)));
    const rawMode = sameChannel && root.dataset.pinMode === "raw";
    raw.hidden = clauses.length > 0 && !rawMode;
    fields.hidden = clauses.length === 0 || rawMode;
    const controls = element("div", "pin-actions",
      actionButton("Edit pin", `edit-pin:${key}`, () => actions.editPin(channel.name, pin.text)));
    if (clauses.length > 0) {
      controls.append(actionButton(rawMode ? "Show fields" : "Show full text", `pin-mode:${key}`, (event) => {
        const showRaw = root.dataset.pinMode !== "raw";
        root.dataset.pinMode = showRaw ? "raw" : "fields";
        raw.hidden = !showRaw;
        fields.hidden = showRaw;
        event.currentTarget.textContent = showRaw ? "Show fields" : "Show full text";
      }));
    }
    disclosure.append(controls, fields, raw);
    disclosure.addEventListener("toggle", () => {
      if (disclosure.open && root.contains(disclosure)) {
        root.dataset.updated = "false";
        disclosure.querySelector(".pin-updated")?.remove();
      }
    });
    root.dataset.pinKey = key;
    root.dataset.pinSeq = String(pin.seq);
    root.dataset.updated = String(updated);
    if (!sameChannel) root.dataset.pinMode = "fields";
    replaceKeepingFocus(root, [disclosure]);
  }

  // pinClauses recognizes the documented display convention only when every
  // segment is preserved. Other pin text remains available verbatim.
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

  // renderMentions shows explicit mentions, never inferred requests, waiting
  // states, or work completion. Replies are linked only inside their channel.
  function renderMentions(root, entries, state, actions) {
    const mentions = [];
    for (const entry of entries) {
      for (const handle of mentionedHandles(entry.message.text)) mentions.push({ entry, handle });
    }
    const open = root.querySelector("details")?.open === true;
    const disclosure = element("details", "mentions-disclosure");
    disclosure.open = open;
    const last = mentions.at(-1);
    disclosure.append(element("summary", "mentions-summary", last
      ? `@${last.entry.message.sender} mentioned @${last.handle} · ${age(last.entry.message.sent_at_ms)} ago`
      : "No mentions in these retained messages"));
    const list = element("ul", "mention-list");
    for (const { entry, handle } of mentions.toReversed()) {
      const row = element("li", "mention-row",
        actionButton(`@${entry.message.sender} mentioned @${handle}`, `mention:${coordinate(entry)}:${handle}`,
          () => actions.selectMessage(entry.target, entry.message.seq)),
        element("span", "message-time", `${age(entry.message.sent_at_ms)} ago`));
      if (entry.target.startsWith("#")) {
        const reply = entries.find((item) => item.target === entry.target && item.message.reply_to === entry.message.seq);
        if (reply) row.append(actionButton(`Reply by @${reply.message.sender}`, `mention-reply:${coordinate(reply)}`,
          () => actions.selectMessage(reply.target, reply.message.seq)));
      }
      list.append(row);
    }
    disclosure.append(list);
    const references = [...new Set(entries.flatMap((entry) => artifacts(entry.message.text)))];
    const filters = element("details", "artifact-disclosure");
    filters.open = root.querySelector(".artifact-disclosure")?.open === true;
    filters.append(element("summary", "", `Find in messages (${references.length} references)`));
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

  // renderNewMessages counts retained messages after this browser's per-source
  // position. Missing positions mean a first visit, not an unread backlog.
  function renderNewMessages(root, entries, state, actions) {
    const fresh = entries.filter((entry) => Object.hasOwn(state.lastSeenSeqByTarget, entry.target)
      && entry.message.seq > state.lastSeenSeqByTarget[entry.target]);
    if (fresh.length === 0) {
      root.replaceChildren();
      return;
    }
    const button = actionButton(`${fresh.length} new since you looked`, "new-messages", () => {
      const first = fresh[0];
      actions.setFollowLive(false);
      actions.setTimeWindow(null);
      actions.selectMessage(first.target, first.message.seq);
      for (const target of new Set(fresh.map((entry) => entry.target))) {
        const latest = entries.filter((entry) => entry.target === target).at(-1);
        actions.markSeen(target, latest.message.seq);
      }
    });
    replaceKeepingFocus(root, [button]);
  }

  // renderMessages preserves disclosure choices, focus, and the visible reading
  // anchor across snapshots. New arrivals scroll only while following live.
  function renderMessages(root, scrollEl, entries, targetEntries, state, actions) {
    const openKeys = new Set([...root.querySelectorAll("details[open][data-disclosure-key]")]
      .map((node) => node.dataset.disclosureKey));
    const expandAll = state.expandAllMessages === true;
    if (root.dataset.expandAll !== String(expandAll)) {
      openKeys.clear();
      if (expandAll) {
        for (const entry of entries) {
          openKeys.add(`message:${coordinate(entry)}`);
          openKeys.add(`thread:${coordinate(entry)}`);
        }
      }
    }
    const selectionKey = state.selectedMessage ? `${state.selectedMessage.target}:${state.selectedMessage.seq}` : "";
    const selectionChanged = root.dataset.selectedMessage !== selectionKey;
    const oldTop = scrollEl.scrollTop;
    const rootTop = scrollEl.getBoundingClientRect().top;
    const anchor = [...root.querySelectorAll("[data-message-key]")]
      .find((node) => node.getBoundingClientRect().bottom >= rootTop);
    const anchorKey = anchor?.dataset.messageKey;
    const anchorOffset = anchor ? anchor.getBoundingClientRect().top - rootTop : 0;
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
      for (const group of threadGroups(entries, byTarget)) {
        if (group.length === 1) nodes.push(messageDisclosure(group[0], state, actions, byTarget, openKeys));
        else nodes.push(threadDisclosure(group, state, actions, byTarget, openKeys));
      }
    } else {
      for (const entry of entries) nodes.push(messageDisclosure(entry, state, actions, byTarget, openKeys));
    }
    const firstNew = entries.find((entry) => Object.hasOwn(state.lastSeenSeqByTarget, entry.target)
      && entry.message.seq > state.lastSeenSeqByTarget[entry.target]);
    if (firstNew) {
      const divider = element("div", "new-messages-divider", "New since you looked");
      const index = nodes.findIndex((node) => node.dataset.messageKey === coordinate(firstNew)
        || node.querySelector(`[data-message-key="${coordinate(firstNew)}"]`));
      if (index >= 0) nodes.splice(index, 0, divider);
    }
    replaceKeepingFocus(root, nodes);
    if (state.followLive && !state.selectedMessage) scrollEl.scrollTop = scrollEl.scrollHeight;
    else {
      scrollEl.scrollTop = oldTop;
      const restored = anchorKey ? [...root.querySelectorAll("[data-message-key]")]
        .find((node) => node.dataset.messageKey === anchorKey) : null;
      if (restored) scrollEl.scrollTop += restored.getBoundingClientRect().top - scrollEl.getBoundingClientRect().top - anchorOffset;
    }
    const selected = state.selectedMessage;
    if (selected) {
      const node = [...root.querySelectorAll("[data-message-key]")]
        .find((item) => item.dataset.messageKey === `${selected.target}:${selected.seq}`);
      if (node) {
        node.open = true;
        const thread = node.closest(".thread-disclosure");
        if (thread) thread.open = true;
        if (selectionChanged) node.scrollIntoView({ block: "nearest" });
      }
    }
    root.dataset.selectedMessage = selectionKey;
    root.dataset.expandAll = String(expandAll);
  }

  // threadGroups joins only explicit channel reply links, including cycles and
  // forward references. Each visible message belongs to exactly one group.
  function threadGroups(entries, byTarget) {
    const parents = new Map();
    // root follows the current component links; only distinct roots are joined.
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
    // Join every retained channel edge once. Filtered replies still share their
    // real ancestor without repeatedly walking each message's entire history.
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
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(entry);
    }
    return [...groups.values()];
  }

  // threadDisclosure names the first visible message and the latest visible
  // reply. "Latest" describes message order, not task completion.
  function threadDisclosure(entries, state, actions, byTarget, openKeys) {
    const first = entries[0];
    const last = entries.at(-1);
    const key = `thread:${coordinate(first)}`;
    const details = element("details", "thread-disclosure");
    details.dataset.disclosureKey = key;
    details.open = openKeys.has(key) || entries.some((entry) => selected(entry, state));
    details.append(element("summary", "thread-summary",
      element("strong", "thread-title", headline(first.message.text)),
      element("span", "thread-latest", `${entries.length} messages · latest @${last.message.sender}: ${headline(last.message.text)}`)));
    const summary = details.querySelector("summary");
    summary.dataset.focusKey = key;
    summary.addEventListener("click", (event) => {
      event.preventDefault();
      const closing = details.open;
      details.open = !closing;
      if (closing && entries.some((entry) => selected(entry, state))) actions.selectMessage(null, 0);
      actions.setFollowLive(false);
    });
    const body = element("div", "thread-messages");
    for (const entry of entries) body.append(messageDisclosure(entry, state, actions, byTarget, openKeys));
    details.append(body);
    return details;
  }

  // messageDisclosure always retains the full body behind the first-line
  // headline. DMs keep their inbox coordinate and do not invent reply links.
  function messageDisclosure(entry, state, actions, byTarget, openKeys) {
    const message = entry.message;
    const key = coordinate(entry);
    const details = element("details", "message-disclosure");
    details.dataset.messageKey = key;
    details.dataset.disclosureKey = `message:${key}`;
    details.open = openKeys.has(`message:${key}`) || selected(entry, state);
    details.classList.toggle("is-selected", selected(entry, state));
    const summary = element("summary", "message-summary",
      element("span", "message-meta", element("strong", "message-sender", `@${message.sender}`),
        element("span", "message-coordinate", `${entry.target}#${message.seq}`),
        element("time", "message-time", clock(message.sent_at_ms))),
      element("span", "message-headline", headline(message.text)));
    summary.dataset.focusKey = `message:${key}`;
    summary.addEventListener("click", (event) => {
      event.preventDefault();
      details.open = !details.open;
      if (details.open) {
        actions.setFollowLive(false);
        actions.selectMessage(entry.target, message.seq);
      } else if (!details.open && selected(entry, state)) actions.selectMessage(null, 0);
    });
    details.append(summary);
    const body = element("div", "message-body", formattedText(message.text));
    const controls = element("div", "message-actions");
    if (entry.target.startsWith("#")) {
      controls.append(actionButton("Reply", `reply:${key}`,
        () => actions.setReplyTo(entry.target, message.seq, message.sender)));
      const parent = byTarget.get(entry.target).get(message.reply_to);
      if (parent) controls.append(actionButton(`Reply to #${parent.message.seq}`, `parent:${key}`,
        () => actions.selectMessage(entry.target, parent.message.seq)));
      else if (message.reply_to) controls.append(element("span", "message-reference", `Reply to #${message.reply_to} · not in retained history`));
    } else if (message.reply_to) controls.append(element("span", "message-reference", `Reply reference #${message.reply_to}`));
    for (const reference of artifacts(message.text)) {
      controls.append(actionButton(reference, `message-artifact:${key}:${reference}`, () => actions.setFilterArtifact(reference)));
    }
    details.append(body, controls);
    return details;
  }

  // renderSelection refreshes an already selected message's details while
  // preserving the separate agent inspector until the user selects a message.
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
    const nodes = [element("div", "details-heading", element("h3", "", `${entry.target}#${entry.message.seq}`),
      actionButton("Close", "close-message", () => actions.selectMessage(null, 0))),
    element("p", "message-meta", `@${entry.message.sender} · ${clock(entry.message.sent_at_ms)}`),
    element("div", "message-body", formattedText(entry.message.text))];
    if (entry.target.startsWith("#")) nodes.push(actionButton("Reply", "reply-selected",
      () => actions.setReplyTo(entry.target, entry.message.seq, entry.message.sender)));
    replaceKeepingFocus(root, nodes);
  }

  // mentionedHandles uses the server's handle boundaries, excluding embedded
  // email fragments, and returns each written mention once per message.
  function mentionedHandles(text) {
    const result = new Map();
    const pattern = /(^|[^a-zA-Z0-9_-])@([a-zA-Z0-9][a-zA-Z0-9_-]{0,31})(?![a-zA-Z0-9_-])/g;
    for (const match of text.matchAll(pattern)) {
      if (!result.has(match[2].toLowerCase())) result.set(match[2].toLowerCase(), match[2]);
    }
    return [...result.values()];
  }

  // artifacts extracts literal references for local text filtering; it does
  // not claim that a commit, pull request, or file exists in any repository.
  function artifacts(text) {
    const result = new Set();
    const patterns = [/\bPR\s+#\d+\b/g, /\b[0-9a-f]{7,40}\b/gi,
      /\b(?:[a-zA-Z0-9_.-]+\/)+[a-zA-Z0-9_.-]+\.[a-zA-Z0-9]+\b/g];
    for (const pattern of patterns) for (const match of text.matchAll(pattern)) result.add(match[0]);
    return [...result];
  }

  // formattedText creates text and code nodes only; message-authored markup
  // never becomes HTML, an inline style, or a script.
  function formattedText(text) {
    const fragment = document.createDocumentFragment();
    for (const part of text.split(/(```[\s\S]*?```)/g)) {
      if (part.startsWith("```") && part.endsWith("```")) {
        fragment.append(element("pre", "message-code", element("code", "", part.slice(3, -3).replace(/^[a-zA-Z0-9_-]+\n/, ""))));
      } else {
        for (const inline of part.split(/(`[^`\n]+`)/g)) {
          fragment.append(inline.startsWith("`") && inline.endsWith("`")
            ? element("code", "", inline.slice(1, -1)) : document.createTextNode(inline));
        }
      }
    }
    return fragment;
  }

  // headline keeps the sender's first nonempty line; CSS may truncate its
  // display, while expanding the message always reveals the original body.
  function headline(text) {
    return text.split(/\r?\n/).find((line) => line.trim().length > 0)?.trim() || "Empty message";
  }

  // coordinate keeps a source and its sequence number together for all DOM
  // identities, including two different DM inboxes both containing message #1.
  function coordinate(entry) {
    return `${entry.target}:${entry.message.seq}`;
  }

  // selected compares both source and sequence, never a swarm-wide number.
  function selected(entry, state) {
    return state.selectedMessage?.target === entry.target && state.selectedMessage.seq === entry.message.seq;
  }

  // clock formats the stored Redis message time in the reader's local zone.
  function clock(milliseconds) {
    return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(milliseconds);
  }

  // age describes elapsed time since a recorded event, never time spent on work.
  function age(milliseconds) {
    const seconds = Math.max(0, Math.floor((Date.now() - milliseconds) / 1000));
    if (seconds < 60) return `${seconds}s`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
    return `${Math.floor(seconds / 3600)}h`;
  }

  // actionButton creates an ordinary keyboard-accessible control whose click
  // invokes the controller action rather than embedding an event attribute.
  function actionButton(label, key, action) {
    const button = element("button", "stream-action", label);
    button.type = "button";
    button.dataset.focusKey = key;
    button.addEventListener("click", action);
    return button;
  }

  // element constructs safe DOM content without parsing authored strings.
  function element(tag, className, ...children) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    for (const child of children) node.append(typeof child === "string" ? document.createTextNode(child) : child);
    return node;
  }

  // empty provides a readable state for loading, no matches, or trimmed data.
  function empty(title, description) {
    return element("div", "empty-state", element("h3", "", title), element("p", "", description));
  }

  // replaceKeepingFocus installs current callbacks and returns keyboard focus
  // to the same control when a changed snapshot rebuilds its subtree.
  function replaceKeepingFocus(root, nodes) {
    const active = document.activeElement;
    const focusKey = root.contains(active) ? active.dataset.focusKey : null;
    root.replaceChildren(...nodes);
    if (focusKey) [...root.querySelectorAll("[data-focus-key]")]
      .find((node) => node.dataset.focusKey === focusKey)?.focus({ preventScroll: true });
  }
})();
