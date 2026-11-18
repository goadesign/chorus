// Exercise the dashboard chat sheet and composer send flow: raising the chat
// on first-ever, folded, DM, pinned, and capacity-evicting sends; waiting-dot
// lifecycle; mid-flight swarm/handle switches; outside pointerdown and Escape
// folding; hidden-tab unseen counts; and accessible #composer-feedback alerts.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const indexHTML = await readFile(new URL('./ui/index.html', import.meta.url), 'utf8');
const chatSource = await readFile(new URL('./ui/chat.js', import.meta.url), 'utf8');
const mentionSource = await readFile(new URL('./ui/mention.js', import.meta.url), 'utf8');
const appSource = await readFile(new URL('./ui/app.js', import.meta.url), 'utf8');

// classSet simulates DOMTokenList for classList assertions.
function classSet(initial = '') {
  const set = new Set(String(initial).split(/\s+/).filter(Boolean));
  return {
    add: (...names) => names.forEach((n) => set.add(n)),
    remove: (...names) => names.forEach((n) => set.delete(n)),
    contains: (n) => set.has(n),
    toggle: (n, force) => {
      const next = force === undefined ? !set.has(n) : Boolean(force);
      if (next) set.add(n);
      else set.delete(n);
      return next;
    },
  };
}

// ElementBase is the simulated Element constructor used for instanceof checks.
class ElementBase {}

// nodeText concatenates visible text content across simulated DOM children.
function nodeText(node) {
  if (node === null || node === undefined || node === false) return '';
  if (typeof node === 'string') return node;
  if (typeof node.nodeValue === 'string') return node.nodeValue;
  if (typeof node._text === 'string' && (!node.children || node.children.length === 0)) return node._text;
  return (node.children || []).map(nodeText).join('');
}

// makeListeners attaches capture/bubble event registration and dispatch to node.
function makeListeners(node) {
  node.listeners = new Map();
  node.addEventListener = (event, callback, options = false) => {
    const capture = typeof options === 'boolean' ? options : Boolean(options?.capture);
    const list = node.listeners.get(event) || [];
    list.push({ callback, capture });
    node.listeners.set(event, list);
  };
  node.emit = (event, payload = {}, phase = 'all') => {
    for (const entry of node.listeners.get(event) || []) {
      if (phase === 'capture' && !entry.capture) continue;
      if (phase === 'bubble' && entry.capture) continue;
      entry.callback({ currentTarget: node, target: node, ...payload });
    }
  };
  return node;
}

// makeElement constructs a simulated DOM Element with attributes, classes, and children.
function makeElement(tag, props = {}) {
  const node = Object.defineProperties(new ElementBase(), Object.getOwnPropertyDescriptors({
    tag: tag.toUpperCase(),
    id: props.id || '',
    value: props.value ?? '',
    checked: Boolean(props.checked),
    disabled: Boolean(props.disabled),
    hidden: Boolean(props.hidden),
    inert: Boolean(props.inert),
    selectionStart: 0,
    selectionEnd: 0,
    scrollLeft: 0,
    scrollTop: 0,
    scrollHeight: 120,
    clientHeight: 400,
    clientWidth: 600,
    offsetHeight: 120,
    offsetTop: 20,
    dataset: {},
    attributes: { ...(props.attributes || {}) },
    style: {
      height: '',
      minHeight: '',
      paddingTop: '0',
      paddingBottom: '0',
      setProperty(k, v) { this[k] = v; },
    },
    classList: classSet(props.className || ''),
    children: [],
    parentElement: null,
    _text: '',
    get className() { return ''; },
    set className(v) { this.classList = classSet(v); },
    get textContent() { return nodeText(this); },
    set textContent(v) {
      this.children = [];
      this._text = String(v);
    },
    get lastElementChild() {
      return [...this.children].reverse().find((c) => c instanceof ElementBase) || null;
    },
    setAttribute(k, v) {
      this.attributes[k] = String(v);
      if (k === 'hidden') this.hidden = true;
    },
    removeAttribute(k) {
      delete this.attributes[k];
      if (k === 'hidden') this.hidden = false;
    },
    getAttribute(k) { return this.attributes[k] ?? null; },
    appendChild(child) {
      if (child && typeof child === 'object') child.parentElement = this;
      this.children.push(child);
      return child;
    },
    append(...items) {
      for (const item of items) this.appendChild(item);
    },
    replaceChildren(...items) {
      this.children = [];
      this._text = '';
      this.append(...items);
    },
    remove() {
      if (!this.parentElement) return;
      this.parentElement.children = this.parentElement.children.filter((c) => c !== this);
      this.parentElement = null;
    },
    contains(other) {
      let cur = other;
      while (cur) {
        if (cur === this) return true;
        cur = cur.parentElement;
      }
      return false;
    },
    matches(selector) {
      return String(selector).split(',').some((part) => {
        const s = part.trim();
        if (s.startsWith('.')) return this.classList.contains(s.slice(1));
        if (s.startsWith('#')) return this.id === s.slice(1);
        if (s === '[data-tab-key]') return Boolean(this.dataset.tabKey);
        return this.tag.toLowerCase() === s.toLowerCase();
      });
    },
    closest(selector) {
      let cur = this;
      while (cur) {
        if (cur.matches?.(selector)) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelector(selector) {
      for (const child of this.children) {
        if (child instanceof ElementBase) {
          if (child.matches(selector)) return child;
          const found = child.querySelector(selector);
          if (found) return found;
        }
      }
      return null;
    },
    querySelectorAll(selector) {
      const out = [];
      for (const child of this.children) {
        if (child instanceof ElementBase) {
          if (child.matches(selector)) out.push(child);
          out.push(...child.querySelectorAll(selector));
        }
      }
      return out;
    },
    getBoundingClientRect: props.getBoundingClientRect || (() => ({ top: 0, bottom: 0, left: 0, right: 0, height: 0, width: 0 })),
    scrollIntoView() {},
    scrollTo({ top }) { this.scrollTop = top; },
    focus() {
      if (props.doc) props.doc.activeElement = this;
    },
    setRangeText(text, start, end) {
      this.value = this.value.slice(0, start) + text + this.value.slice(end);
      this.selectionStart = this.selectionEnd = start + text.length;
    },
  }));
  return makeListeners(node);
}

// chatHarness builds the minimal environment for unit-testing ChorusChat.
function chatHarness({ me = 'raphael', target = '#general' } = {}) {
  const storage = new Map();
  const doc = makeListeners({
    title: 'Chorus',
    visibilityState: 'visible',
    activeElement: null,
    documentElement: { dataset: {} },
    body: new ElementBase(),
  });
  const elWithDoc = (tag, props = {}) => makeElement(tag, { ...props, doc });

  const composerCard = elWithDoc('div', { className: 'composer-card' });
  const sheet = elWithDoc('section', { id: 'chat-sheet', className: 'chat-sheet', inert: true });
  const log = elWithDoc('div', { id: 'chat-log', className: 'chat-log' });
  const composerBox = elWithDoc('div', {
    className: 'composer-box',
    getBoundingClientRect: () => ({ top: 620, bottom: 680 }),
  });
  const box = elWithDoc('textarea', {
    id: 'send-text',
    attributes: { 'aria-controls': 'chat-sheet', 'aria-expanded': 'false' },
  });
  const sendHandle = elWithDoc('input', { id: 'send-handle', value: me });
  const sendTarget = elWithDoc('input', { id: 'send-target', value: target });
  const mentionList = elWithDoc('ul', { id: 'mention-suggestions', hidden: true });
  const stage = elWithDoc('main', {
    id: 'stage-container',
    getBoundingClientRect: () => ({ top: 60, bottom: 600 }),
  });
  const timeline = elWithDoc('section', {
    id: 'timeline-root',
    getBoundingClientRect: () => ({ top: 60, bottom: 180 }),
  });
  sheet.append(log);
  composerBox.append(sendHandle, sendTarget, mentionList, box);
  composerCard.append(sheet, composerBox);
  doc.activeElement = box;

  const byId = new Map([
    ['send-text', box],
    ['send-handle', sendHandle],
    ['send-target', sendTarget],
    ['mention-suggestions', mentionList],
    ['chat-sheet', sheet],
    ['chat-log', log],
    ['stage-container', stage],
    ['timeline-root', timeline],
  ]);
  doc.getElementById = (id) => byId.get(id) || null;
  doc.querySelector = (sel) => {
    if (sel === '.composer-card') return composerCard;
    if (sel === '.composer-box') return composerBox;
    return null;
  };
  doc.createElement = (tag) => elWithDoc(tag);
  doc.createTextNode = (text) => ({ nodeValue: String(text) });

  const actions = {
    clearedReplies: 0,
    clearReply() { this.clearedReplies += 1; },
    setReplyTo(channelName) {
      sendTarget.value = channelName;
      box.focus();
    },
    composeDM(handle) {
      sendTarget.value = `@${handle}`;
      box.focus();
    },
  };

  const context = {
    Element: ElementBase,
    Node: ElementBase,
    localStorage: {
      getItem: (k) => storage.get(k) ?? null,
      setItem: (k, v) => storage.set(k, String(v)),
    },
    document: doc,
    window: makeListeners({
      matchMedia: () => ({ matches: true, addEventListener() {} }),
      ChorusUI: {
        el: (tag, attrs = {}, ...children) => {
          const node = elWithDoc(tag, { id: attrs.id, className: attrs.className });
          for (const [k, v] of Object.entries(attrs)) {
            if (v === null || v === undefined || v === false || k === 'className') continue;
            if (k === 'dataset') Object.assign(node.dataset, v);
            else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2).toLowerCase(), v);
            else node.setAttribute(k, v === true ? '' : String(v));
          }
          for (const c of children.flat()) {
            if (c === null || c === undefined || c === false) continue;
            node.appendChild(c instanceof ElementBase ? c : { nodeValue: String(c) });
          }
          return node;
        },
        handleBadge: (h) => `@${h}`,
        matchesMessage: (_target, message, handle) =>
          new RegExp(`(^|[^A-Za-z0-9_-])@${handle}(?![A-Za-z0-9_-])`, 'i').test(message.text),
      },
      ChorusStream: {
        renderMessageText: (text) => ({ nodeValue: String(text) }),
      },
    }),
  };
  vm.runInNewContext(chatSource, context);

  return {
    chat: context.window.ChorusChat,
    doc,
    stage,
    sheet,
    composerCard,
    composerBox,
    log,
    box,
    sendHandle,
    sendTarget,
    mentionList,
    actions,
    get isOpen() { return sheet.classList.contains('is-open') && !sheet.inert; },
    get waitingRow() { return log.querySelector('.chat-waiting'); },
    clickBox() { box.emit('click'); },
    clickOutside(target = stage) { doc.emit('pointerdown', { target, offsetX: 10 }, 'capture'); },
    pressEscape() { composerCard.emit('keydown', { key: 'Escape', isComposing: false }); },
  };
}

test('index.html exposes #composer-feedback as role="alert" and avoids hardcoding #chorus on #send-target', () => {
  assert.match(indexHTML, /<div class="composer-feedback" id="composer-feedback" role="alert" hidden><\/div>/);
  assert.doesNotMatch(indexHTML, /composer-status-text/);
  assert.doesNotMatch(indexHTML, /id="send-target"[^>]*value="#chorus"/);
});

test('first-ever send keeps sheet closed and ignores outside clicks until the view arrives, then raises with waiting dots', () => {
  const h = chatHarness();
  const emptyView = {
    swarm: 'chorus',
    agents: [{ handle: 'claude', model: 'Claude', channels: ['#build'] }],
    channels: [{
      name: '#build',
      head_seq: 0,
      positions: [{ handle: 'claude', read_seq: 0, behind: 0, mentioned: false }],
      messages: [],
    }],
    inboxes: [],
  };

  h.chat.render({ activeSwarm: 'chorus', view: emptyView }, h.actions);
  h.clickBox();
  assert.equal(h.isOpen, false);
  assert.equal(h.box.attributes['aria-expanded'], 'false');

  // Person sends their first message; intermediate empty render and outside click do not cancel the pending raise.
  h.chat.sent('chorus', 'raphael', '#build:1');
  h.chat.render({ activeSwarm: 'chorus', view: emptyView }, h.actions);
  assert.equal(h.isOpen, false);
  assert.equal(h.composerCard.classList.contains('has-chat-open'), false);
  assert.equal(h.box.attributes['aria-expanded'], 'false');
  h.clickOutside();

  const sentView = {
    ...emptyView,
    agents: [
      ...emptyView.agents,
      { handle: 'raphael', model: 'human', channels: ['#build'] },
    ],
    channels: [{
      name: '#build',
      head_seq: 1,
      positions: [
        { handle: 'claude', read_seq: 0, behind: 1, mentioned: true },
        { handle: 'raphael', read_seq: 1, behind: 0, mentioned: false },
      ],
      messages: [{ seq: 1, sender: 'raphael', human: true, model: 'human', text: 'hey @claude check this', sent_at_ms: 1000 }],
    }],
  };
  h.chat.render({ activeSwarm: 'chorus', view: sentView }, h.actions);
  assert.equal(h.isOpen, true);
  assert.equal(h.composerCard.classList.contains('has-chat-open'), true);
  assert.equal(h.box.attributes['aria-expanded'], 'true');
  assert.equal(h.log.children.length, 2);
  assert.equal(h.waitingRow.classList.contains('is-read'), false);

  // Agent reads -> dots pulse; agent replies -> dots clear; outside click folds the raised sheet.
  const readView = {
    ...sentView,
    channels: [{
      ...sentView.channels[0],
      positions: [
        { handle: 'claude', read_seq: 1, behind: 0, mentioned: false },
        { handle: 'raphael', read_seq: 1, behind: 0, mentioned: false },
      ],
    }],
  };
  h.chat.render({ activeSwarm: 'chorus', view: readView }, h.actions);
  assert.equal(h.waitingRow.classList.contains('is-read'), true);

  const repliedView = {
    ...readView,
    channels: [{
      ...readView.channels[0],
      head_seq: 2,
      messages: [
        ...readView.channels[0].messages,
        { seq: 2, sender: 'claude', model: 'Claude', reply_to: 1, text: 'on it', sent_at_ms: 2000 },
      ],
    }],
  };
  h.chat.render({ activeSwarm: 'chorus', view: repliedView }, h.actions);
  assert.equal(h.waitingRow, null);
  assert.equal(h.log.children.length, 2);

  h.clickOutside();
  assert.equal(h.isOpen, false);
  assert.equal(h.box.attributes['aria-expanded'], 'false');
});

test('folded, DM, pinned, and stream-capacity sends raise the chat, while swarm/handle switches keep other chats closed', () => {
  const h = chatHarness({ target: '#general' });
  const incomingView = {
    swarm: 'chorus',
    agents: [
      { handle: 'claude', model: 'Claude', channels: ['#build'] },
      { handle: 'raphael', model: 'human', channels: ['#build'] },
      { handle: 'alice', model: 'human', channels: ['#build'] },
    ],
    channels: [{
      name: '#build',
      head_seq: 2,
      positions: [
        { handle: 'claude', read_seq: 2, behind: 0, mentioned: false },
        { handle: 'raphael', read_seq: 0, behind: 2, mentioned: true },
      ],
      messages: [
        { seq: 1, sender: 'alice', human: true, model: 'human', text: 'hello @claude', sent_at_ms: 500 },
        { seq: 2, sender: 'claude', model: 'Claude', text: 'hi @raphael', sent_at_ms: 1000 },
      ],
    }],
    inboxes: [],
  };

  // Auto-raise aims composer at #build; folding empty box with Escape restores #general.
  h.chat.render({ activeSwarm: 'chorus', view: incomingView }, h.actions);
  assert.equal(h.isOpen, true);
  assert.equal(h.sendTarget.value, '#build');
  h.pressEscape();
  assert.equal(h.isOpen, false);
  assert.equal(h.sendTarget.value, '#general');

  // Sending a DM (@claude) while folded raises the chat with DM waiting dots.
  h.sendTarget.value = '@claude';
  h.chat.sent('chorus', 'raphael', '@claude:1');
  const dmSentView = {
    ...incomingView,
    inboxes: [{
      recipient: 'claude',
      head_seq: 1,
      read_seq: 0,
      behind: 1,
      messages: [{ seq: 1, sender: 'raphael', human: true, model: 'human', text: 'quick question', sent_at_ms: 1500 }],
    }],
  };
  h.chat.render({ activeSwarm: 'chorus', view: dmSentView }, h.actions);
  assert.equal(h.isOpen, true);
  assert.match(h.waitingRow.attributes['aria-label'], /@claude/);

  // Sending again at stream capacity (older entry #build:2 evicted so entries.length stays 2) still detects the new send.
  h.pressEscape();
  assert.equal(h.isOpen, false);
  h.chat.sent('chorus', 'raphael', '#build:3');
  const evictedView = {
    ...dmSentView,
    channels: [{
      ...dmSentView.channels[0],
      head_seq: 3,
      pin: { seq: 3, sender: 'raphael', human: true, model: 'human', text: 'Goal: ship' },
      messages: [
        { seq: 3, sender: 'raphael', human: true, model: 'human', text: 'Goal: ship', sent_at_ms: 2500 },
      ],
    }],
  };
  h.chat.render({ activeSwarm: 'chorus', view: evictedView }, h.actions);
  assert.equal(h.isOpen, true);
  assert.ok(h.waitingRow);

  // Switching swarms or editing As @ while awaiting keeps the other swarm/handle's chat closed.
  h.pressEscape();
  assert.equal(h.isOpen, false);
  h.chat.sent('chorus', 'raphael', '#build:4');
  h.chat.render({ activeSwarm: 'other', view: { ...evictedView, swarm: 'other' } }, h.actions);
  assert.equal(h.isOpen, false);
  assert.equal(h.waitingRow, null);

  h.chat.render({ activeSwarm: 'chorus', view: incomingView }, h.actions);
  assert.equal(h.isOpen, false);
  h.chat.sent('chorus', 'raphael', '#build:4');
  h.sendHandle.value = 'alice';
  h.sendHandle.emit('input');
  assert.equal(h.isOpen, false);
  assert.equal(h.waitingRow, null);
});

test('hidden-tab sends and fast same-snapshot replies preserve unseen title counts until visible again', () => {
  const h = chatHarness();
  const emptyView = {
    swarm: 'chorus',
    agents: [{ handle: 'claude', model: 'Claude', channels: ['#build'] }],
    channels: [{ name: '#build', head_seq: 0, positions: [], messages: [] }],
    inboxes: [],
  };
  h.chat.render({ activeSwarm: 'chorus', view: emptyView }, h.actions);

  // User sends and switches tabs before view arrives; both sent message and agent reply land while hidden.
  h.chat.sent('chorus', 'raphael', '#build:1');
  h.doc.visibilityState = 'hidden';
  const bothView = {
    swarm: 'chorus',
    agents: [
      { handle: 'claude', model: 'Claude', channels: ['#build'] },
      { handle: 'raphael', model: 'human', channels: ['#build'] },
    ],
    channels: [{
      name: '#build',
      head_seq: 2,
      positions: [
        { handle: 'claude', read_seq: 2, behind: 0, mentioned: false },
        { handle: 'raphael', read_seq: 1, behind: 1, mentioned: false },
      ],
      messages: [
        { seq: 1, sender: 'raphael', human: true, model: 'human', text: 'ping @claude', sent_at_ms: 1000 },
        { seq: 2, sender: 'claude', model: 'Claude', reply_to: 1, text: 'pong', sent_at_ms: 1100 },
      ],
    }],
    inboxes: [],
  };
  h.chat.render({ activeSwarm: 'chorus', view: bothView }, h.actions);
  assert.equal(h.isOpen, true);
  assert.equal(h.waitingRow, null);
  assert.equal(h.doc.title, '(1) Chorus');

  // Returning to the tab marks the visible reply seen and clears the title badge.
  h.doc.visibilityState = 'visible';
  h.doc.emit('visibilitychange');
  assert.equal(h.doc.title, 'Chorus');
});

test('app.js sendMessage raises chat without ok banner, announces errors, and guards mid-flight swarm/handle switches', async () => {
  const storage = new Map();
  const doc = makeListeners({
    title: 'Chorus',
    visibilityState: 'visible',
    activeElement: null,
    documentElement: { dataset: {} },
    body: new ElementBase(),
  });
  const el = (tag, props = {}) => makeElement(tag, { ...props, doc });

  const composerCard = el('div', { className: 'composer-card' });
  const sheet = el('section', { id: 'chat-sheet', className: 'chat-sheet', inert: true });
  const log = el('div', { id: 'chat-log', className: 'chat-log' });
  const composerBox = el('div', { className: 'composer-box', getBoundingClientRect: () => ({ top: 620, bottom: 680 }) });
  const sendHandle = el('input', { id: 'send-handle', value: 'raphael' });
  const sendTarget = el('input', { id: 'send-target', value: '' });
  const sendText = el('textarea', {
    id: 'send-text',
    attributes: { 'aria-controls': 'chat-sheet', 'aria-expanded': 'false', 'aria-keyshortcuts': 'ArrowUp Escape' },
  });
  const sendPin = el('input', { id: 'send-pin' });
  const pinLabel = el('label', { id: 'pin-toggle-label' });
  const sendBtn = el('button', { id: 'btn-send' });
  const feedback = el('div', { id: 'composer-feedback', hidden: true, attributes: { role: 'alert', hidden: '' } });
  const mentionList = el('ul', { id: 'mention-suggestions', hidden: true });
  const mentionStatus = el('span', { id: 'mention-status' });
  const swarmSelect = el('select', { id: 'swarm-select', value: 'alpha' });
  const stage = el('main', { id: 'stage-container', getBoundingClientRect: () => ({ top: 60, bottom: 600 }) });
  const stageInner = el('div', { className: 'stage-inner' });
  stage.append(stageInner);
  sheet.append(log);
  composerBox.append(sendHandle, sendTarget, mentionList, mentionStatus, sendText, pinLabel, sendBtn);
  composerCard.append(sheet, composerBox, feedback);

  const ids = {
    'theme-toggle': el('button', { id: 'theme-toggle' }),
    'swarm-select': swarmSelect,
    'search-input': el('input', { id: 'search-input' }),
    'mode-threaded': el('button', { id: 'mode-threaded', className: 'active' }),
    'mode-stream': el('button', { id: 'mode-stream' }),
    'btn-toggle-disclosure': el('button', { id: 'btn-toggle-disclosure' }),
    'send-handle': sendHandle,
    'send-target': sendTarget,
    'send-text': sendText,
    'send-pin': sendPin,
    'pin-toggle-label': pinLabel,
    'btn-send': sendBtn,
    'composer-feedback': feedback,
    'reply-chip-container': el('span', { id: 'reply-chip-container' }),
    'live-status': el('div', { id: 'live-status' }),
    'live-dot': el('span', { id: 'live-dot' }),
    'live-text': el('span', { id: 'live-text' }),
    'target-suggestions': el('datalist', { id: 'target-suggestions' }),
    'target-tabs': el('nav', { id: 'target-tabs' }),
    'active-filters-bar': el('div', { id: 'active-filters-bar' }),
    'message-stream': el('div', { id: 'message-stream' }),
    'timeline-root': el('section', { id: 'timeline-root', getBoundingClientRect: () => ({ top: 60, bottom: 180 }) }),
    'agent-details': el('div', { id: 'agent-details', hidden: true }),
    'pin-container': el('div', { id: 'pin-container' }),
    'mentions-container': el('div', { id: 'mentions-container' }),
    'active-title': el('h1', { id: 'active-title' }),
    'active-subtitle': el('span', { id: 'active-subtitle' }),
    'stage-container': stage,
    'chat-sheet': sheet,
    'chat-log': log,
    'mention-suggestions': mentionList,
    'mention-status': mentionStatus,
  };
  doc.getElementById = (id) => ids[id] || null;
  doc.querySelector = (sel) => {
    if (sel === '.composer-card') return composerCard;
    if (sel === '.composer-box') return composerBox;
    return null;
  };
  doc.createElement = (tag) => el(tag);
  doc.createElementNS = (_ns, tag) => el(tag);
  doc.createTextNode = (text) => ({ nodeValue: String(text) });

  const views = {
    alpha: {
      etag: '"alpha-1"',
      doc: {
        swarm: 'alpha',
        agents: [{ handle: 'claude', model: 'Claude', channels: ['#build'] }],
        channels: [{
          name: '#build',
          head_seq: 0,
          positions: [{ handle: 'claude', read_seq: 0, behind: 0, mentioned: false }],
          messages: [],
        }],
        inboxes: [],
      },
    },
    beta: {
      etag: '"beta-1"',
      doc: {
        swarm: 'beta',
        agents: [
          { handle: 'claude', model: 'Claude', channels: ['#ops'] },
          { handle: 'raphael', model: 'human', channels: ['#ops'] },
        ],
        channels: [{
          name: '#ops',
          head_seq: 1,
          positions: [{ handle: 'claude', read_seq: 1, behind: 0, mentioned: false }],
          messages: [{ seq: 1, sender: 'raphael', human: true, model: 'human', text: 'earlier in beta', sent_at_ms: 500 }],
        }],
        inboxes: [],
      },
    },
    surla: {
      etag: '"surla-1"',
      doc: {
        swarm: 'surla',
        agents: [
          { handle: 'agy', model: 'Gemini 4 Argon (High)', channels: ['#surla'] },
          { handle: 'codex-terminal', model: 'GPT-6.1-Sol', channels: ['#surla'] },
          { handle: 'fable', model: 'Claude Opus 5.5', channels: ['#surla'] },
        ],
        channels: [{
          name: '#surla',
          head_seq: 0,
          positions: [
            { handle: 'agy', read_seq: 0, behind: 0, mentioned: false },
            { handle: 'codex-terminal', read_seq: 0, behind: 0, mentioned: false },
            { handle: 'fable', read_seq: 0, behind: 0, mentioned: false },
          ],
          messages: [],
        }],
        inboxes: [],
      },
    },
    fresh: {
      etag: '"fresh-1"',
      doc: {
        swarm: 'fresh',
        agents: [],
        channels: [],
        inboxes: [],
      },
    },
  };

  let holdSend = null;
  let resolveLongPoll = null;
  const mcpCalls = [];
  const fetchMock = async (url, opts = {}) => {
    if (url === '/swarms') {
      return {
        ok: true,
        status: 200,
        headers: { get: () => null },
        json: async () => ({ swarms: ['alpha', 'beta', 'fresh', 'surla'] }),
      };
    }
    const viewMatch = /^\/swarms\/([^/]+)\/view\?wait=(\d+)$/.exec(url);
    if (viewMatch) {
      const [, swarmName, waitStr] = viewMatch;
      if (Number(waitStr) > 0) {
        return new Promise((resolve, reject) => {
          resolveLongPoll = () => {
            const entry = views[swarmName];
            resolve({
              ok: true,
              status: 200,
              headers: { get: (k) => (k === 'ETag' ? entry.etag : null) },
              json: async () => entry.doc,
            });
          };
          opts.signal?.addEventListener('abort', () => reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
        });
      }
      const entry = views[swarmName];
      return {
        ok: true,
        status: 200,
        headers: { get: (k) => (k === 'ETag' ? entry.etag : null) },
        json: async () => entry.doc,
      };
    }
    if (url === '/mcp') {
      const body = JSON.parse(opts.body);
      mcpCalls.push(body.params);
      if (body.params.name === 'send' && body.params.arguments.text === 'hey @claude') {
        views.alpha = {
          etag: '"alpha-2"',
          doc: {
            swarm: 'alpha',
            agents: [
              { handle: 'claude', model: 'Claude', channels: ['#build'] },
              { handle: 'raphael', model: 'human', channels: ['#build'] },
            ],
            channels: [{
              name: '#build',
              head_seq: 1,
              positions: [
                { handle: 'claude', read_seq: 0, behind: 1, mentioned: true },
                { handle: 'raphael', read_seq: 1, behind: 0, mentioned: false },
              ],
              messages: [{ seq: 1, sender: 'raphael', human: true, model: 'human', text: 'hey @claude', sent_at_ms: 1000 }],
            }],
            inboxes: [],
          },
        };
        // Simulate view.go waking the active ?wait=25 long-poll BEFORE POST /mcp returns.
        resolveLongPoll?.();
        await new Promise((r) => setTimeout(r, 0));
        await new Promise((r) => setTimeout(r, 0));
      }
      if (body.params.name === 'send' && holdSend) await holdSend;
      const to = body.params.arguments.to || '#build';
      return {
        ok: true,
        status: 200,
        json: async () => ({ result: { content: [{ text: `sent ${to}#1` }] } }),
      };
    }
    throw new Error(`unexpected fetch ${url}`);
  };

  const win = makeListeners({
    location: { hash: '' },
    matchMedia: () => ({ matches: true, addEventListener() {} }),
    ChorusUpdate: { start() {}, checkUIBuild() {} },
    ChorusStream: {
      renderMessageText: (text) => ({ nodeValue: String(text) }),
    },
  });
  const context = {
    Element: ElementBase,
    Node: ElementBase,
    AbortController,
    URLSearchParams,
    setTimeout,
    clearTimeout,
    getComputedStyle: () => ({ paddingTop: '0', paddingBottom: '0' }),
    history: { replaceState(_s, _t, hash) { win.location.hash = hash; } },
    localStorage: {
      getItem: (k) => storage.get(k) ?? null,
      setItem: (k, v) => storage.set(k, String(v)),
    },
    fetch: fetchMock,
    document: doc,
    window: win,
  };
  win.document = doc;
  vm.runInNewContext(chatSource, context);
  vm.runInNewContext(mentionSource, context);
  vm.runInNewContext(appSource, context);
  const flush = async () => {
    for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));
  };
  await flush();

  // 1. Blank send fails: #composer-feedback reveals error alert, chat stays closed, typing clears alert.
  sendBtn.emit('click');
  await flush();
  assert.equal(feedback.hidden, false);
  assert.equal(feedback.textContent, 'Enter text before sending.');
  assert.equal(sheet.classList.contains('is-open'), false);

  sendText.value = 'hey @claude';
  sendText.emit('input');
  assert.equal(feedback.hidden, true);
  assert.equal(feedback.textContent, '');

  // 2. Valid send without ever clicking #send-text: joins + sends, keeps #composer-feedback hidden, raises #chat-sheet with dots.
  sendBtn.emit('click');
  await flush();
  await flush();
  await flush();
  assert.equal(feedback.hidden, true);
  assert.equal(sendText.value, '');
  assert.equal(sheet.classList.contains('is-open'), true);
  assert.equal(sheet.inert, false);
  assert.ok(log.querySelector('.chat-waiting'));

  // 3. Mid-flight swarm switch during slow publishHumanMessage does not touch beta's draft or open beta's chat.
  let releaseSend;
  holdSend = new Promise((r) => { releaseSend = r; });
  sendText.value = 'slow alpha send';
  sendBtn.emit('click');
  await flush();

  swarmSelect.value = 'beta';
  swarmSelect.emit('change', { target: swarmSelect });
  await flush();
  await flush();
  assert.equal(sheet.classList.contains('is-open'), false);
  sendText.value = 'beta draft';

  releaseSend();
  holdSend = null;
  await flush();
  await flush();
  assert.equal(sendText.value, 'beta draft');
  assert.equal(sheet.classList.contains('is-open'), false);
  assert.equal(log.querySelector('.chat-waiting'), null);

  // 4. Mid-flight As @ handle edit during slow publishHumanMessage does not open the edited handle's chat.
  holdSend = new Promise((r) => { releaseSend = r; });
  sendTarget.value = '#ops';
  sendText.value = 'slow beta send';
  sendBtn.emit('click');
  await flush();

  sendHandle.value = 'alice';
  sendHandle.emit('input');
  releaseSend();
  holdSend = null;
  await flush();
  await flush();
  assert.equal(sheet.classList.contains('is-open'), false);
  assert.equal(log.querySelector('.chat-waiting'), null);

  // 5. Switching to surla (even after editing To in beta or selecting All Activity) syncs To to #surla
  // so @mention suggestions never show "not in #chorus" for watching #surla members.
  sendTarget.value = '#custom-beta';
  sendTarget.emit('input');
  assert.equal(sendTarget.dataset.userEdited, 'true');

  win.location.hash = '#swarm=surla&target=all';
  win.emit('hashchange');
  await flush();
  await flush();
  assert.equal(sendTarget.dataset.userEdited, undefined);
  assert.equal(sendTarget.value, '#surla');

  doc.activeElement = sendText;
  sendText.value = '@';
  sendText.selectionStart = sendText.selectionEnd = 1;
  sendText.emit('input');
  assert.equal(mentionList.hidden, false);
  const notes = mentionList.children.map((li) => li.textContent);
  assert.equal(notes.some((t) => t.includes('not in ')), false);

  // 6. An empty-at-boot swarm omits target=all from location.hash, defaults To to #<swarm>,
  // and promotes selectedTarget + To to #build as soon as the first channel appears.
  win.location.hash = '#swarm=fresh';
  win.emit('hashchange');
  await flush();
  await flush();
  assert.equal(win.location.hash, '#swarm=fresh');
  assert.equal(sendTarget.value, '#fresh');

  views.fresh = {
    etag: '"fresh-2"',
    doc: {
      swarm: 'fresh',
      agents: [{ handle: 'claude', model: 'Claude Opus 5.5', channels: ['#build'] }],
      channels: [{
        name: '#build',
        head_seq: 0,
        positions: [{ handle: 'claude', read_seq: 0, behind: 0, mentioned: false }],
        messages: [],
      }],
      inboxes: [],
    },
  };
  resolveLongPoll?.();
  await flush();
  await flush();
  assert.equal(win.ChorusUI.state.selectedTarget.kind, 'channel');
  assert.equal(win.ChorusUI.state.selectedTarget.name, '#build');
  assert.equal(sendTarget.value, '#build');
  assert.equal(win.location.hash, '#swarm=fresh&target=%23build');

  // 7. composeDM and cross-channel setReplyTo survive passive background /view polls while a channel tab stays selected.
  win.ChorusUI.actions.composeDM('claude');
  assert.equal(sendTarget.value, '@claude');
  assert.equal(pinLabel.classList.contains('disabled'), true);
  views.fresh = {
    etag: '"fresh-3"',
    doc: {
      ...views.fresh.doc,
      channels: [
        views.fresh.doc.channels[0],
        { name: '#ops', head_seq: 5, positions: [{ handle: 'claude', read_seq: 5, behind: 0, mentioned: false }], messages: [] },
      ],
    },
  };
  resolveLongPoll?.();
  await flush();
  await flush();
  assert.equal(win.ChorusUI.state.selectedTarget.name, '#build');
  assert.equal(sendTarget.value, '@claude');
  assert.equal(pinLabel.classList.contains('disabled'), true);

  win.ChorusUI.actions.setReplyTo('#ops', 5, 'claude', false, 'Claude Opus 5.5');
  assert.equal(sendTarget.value, '#ops');
  assert.equal(pinLabel.classList.contains('disabled'), false);
  views.fresh = { etag: '"fresh-4"', doc: { ...views.fresh.doc } };
  resolveLongPoll?.();
  await flush();
  await flush();
  assert.equal(sendTarget.value, '#ops');
  assert.deepEqual({ ...win.ChorusUI.state.replyTo }, { target: '#ops', seq: 5, sender: 'claude', human: false, model: 'Claude Opus 5.5' });

  // 8. resolveIdentity maps display-formatted models with effort/ID parentheticals to official provider marks.
  const { resolveIdentity } = win.ChorusUI;
  assert.equal(resolveIdentity('agy', 'Gemini 4 Argon (High)').provider, 'gemini');
  assert.equal(resolveIdentity('fable', 'Claude Opus 5.5 (High)').provider, 'claude');
  assert.equal(resolveIdentity('guide', 'Sonnet 5.5 (claude-sonnet-5-5)').provider, 'claude');
  assert.equal(resolveIdentity('codex', 'GPT-6.1-Sol').provider, 'openai');
  assert.equal(resolveIdentity('oss', 'GPT-OSS 120B (Medium)').provider, 'openai');
  assert.equal(resolveIdentity('other', 'custom-wrapper (claude)').provider, null);
});
