// Exercise the message box's mention suggestions: participant ranking and
// channel/DM wake notes, keyboard/pointer selection, ARIA handoff and polite
// status announcements, mid-word/rollover/clear safety, and live swarm updates.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('./ui/mention.js', import.meta.url), 'utf8');

// nodeText concatenates visible text content across simulated DOM children.
function nodeText(node) {
  if (node === null || node === undefined || node === false) return '';
  if (typeof node === 'string') return node;
  return (node.children || []).map(nodeText).join('');
}

// harness builds the minimal composer DOM and ChorusUI contract used by
// mention.js, supporting multiple listeners per event and capture vs bubble.
function harness({ agents, view = { agents }, me = 'richard', target = '#build', chatOpen = false } = {}) {
  const listeners = (node) => {
    node.listeners = new Map();
    node.addEventListener = (event, callback, capture = false) => {
      const list = node.listeners.get(event) || [];
      list.push({ callback, capture: Boolean(capture) });
      node.listeners.set(event, list);
    };
    node.emit = (event, payload = {}, phase = 'all') => {
      for (const entry of node.listeners.get(event) || []) {
        if (phase === 'capture' && !entry.capture) continue;
        if (phase === 'bubble' && entry.capture) continue;
        entry.callback(payload);
      }
    };
    return node;
  };

  let draws = 0;
  let scrolls = 0;
  let announcements = 0;
  const box = listeners({
    value: '',
    selectionStart: 0,
    selectionEnd: 0,
    attributes: {
      'aria-controls': 'chat-sheet',
      'aria-expanded': String(chatOpen),
      'aria-keyshortcuts': 'ArrowUp Escape',
    },
    setAttribute(key, value) { this.attributes[key] = String(value); },
    removeAttribute(key) { delete this.attributes[key]; },
    setRangeText(text, start, end) {
      if (start > end || start < 0 || end > this.value.length) {
        throw new RangeError(`invalid setRangeText [${start}, ${end}] for length ${this.value.length}`);
      }
      this.value = this.value.slice(0, start) + text + this.value.slice(end);
      this.selectionStart = this.selectionEnd = start + text.length;
    },
  });
  const list = listeners({
    hidden: true,
    children: [],
    replaceChildren(...rows) {
      draws += 1;
      this.children = rows;
    },
  });
  let statusText = '';
  const status = {
    get textContent() { return statusText; },
    set textContent(v) {
      announcements += 1;
      statusText = String(v);
    },
  };
  const chatSheet = { inert: !chatOpen };
  const card = listeners({});
  const elements = {
    'send-text': box,
    'mention-suggestions': list,
    'mention-status': status,
    'chat-sheet': chatSheet,
    'send-handle': { value: me },
    'send-target': { value: target },
  };
  const doc = {
    activeElement: box,
    getElementById: (id) => elements[id],
    querySelector: (sel) => (sel === '.composer-card' ? card : null),
  };
  const state = { view };
  const context = {
    document: doc,
    window: {
      ChorusUI: {
        state,
        el: (tag, attrs = {}, ...children) => {
          const filtered = children.filter((c) => c !== null && c !== undefined && c !== false);
          const item = {
            tag,
            ...attrs,
            attributes: { ...attrs },
            children: filtered,
            setAttribute(key, value) {
              this[key] = String(value);
              this.attributes[key] = String(value);
            },
            get textContent() { return nodeText(this); },
            scrollIntoView() { scrolls += 1; },
            closest(sel) { return sel === '.mention-suggestion' && attrs.className === 'mention-suggestion' ? item : null; },
          };
          return item;
        },
        handleBadge: (handle) => ({ children: [`@${handle}`] }),
      },
    },
  };
  vm.runInNewContext(source, context);

  return {
    box,
    list,
    status,
    chatSheet,
    doc,
    elements,
    state,
    mention: context.window.ChorusMention,
    get draws() { return draws; },
    get scrolls() { return scrolls; },
    get announcements() { return announcements; },
    get listed() { return list.hidden ? null : list.children.map((row) => row.dataset.handle); },
    get rows() {
      return list.hidden ? null : list.children.map((row) => ({
        handle: row.dataset.handle,
        selected: row.attributes['aria-selected'],
        text: row.textContent,
        note: row.children.find((c) => c?.className === 'mention-suggestion-note')?.textContent || '',
        childCount: row.children.length,
      }));
    },
    type(text, caret = text.length) {
      doc.activeElement = box;
      box.value = text;
      box.selectionStart = box.selectionEnd = caret;
      box.emit('input');
      box.emit('keyup', { key: text.at(-1) || 'Backspace' });
    },
    moveCaret(start, end = start) {
      doc.activeElement = box;
      box.selectionStart = start;
      box.selectionEnd = end;
      box.emit(start === end ? 'keyup' : 'select', { key: 'ArrowLeft' });
    },
    key(name, extra = {}) {
      let taken = false;
      let prevented = false;
      const ev = {
        key: name,
        target: box,
        isComposing: false,
        shiftKey: false,
        ctrlKey: false,
        metaKey: false,
        altKey: false,
        ...extra,
        preventDefault() { prevented = true; },
        stopPropagation() { taken = true; },
      };
      card.emit('keydown', ev, 'capture');
      if (!taken) box.emit('keydown', ev, 'bubble');
      box.emit('keyup', { key: name });
      return { taken, prevented };
    },
    blur() {
      doc.activeElement = null;
      box.emit('blur');
    },
  };
}

const agents = [
  { handle: 'codex-c', model: 'arn:aws:bedrock:us-east-1:123456789012:inference-profile/gpt', channels: ['#docs'] },
  { handle: 'claude-lead', model: 'Claude', channels: ['#build'] },
  { handle: 'richard', model: 'human', channels: ['#build'] },
  { handle: 'alice', model: 'Claude', channels: ['#build'] },
];

test('ranks channel members and exact-case DM recipients first, puts wake/visibility warnings ahead of model, prioritizes @all over substring matches, strips @ on As @, reserves @all, and rejects @-/@_', () => {
  const unloaded = harness({ view: null });
  unloaded.type('ask @');
  assert.equal(unloaded.listed, null);

  const channel = harness({
    agents: [
      ...agents,
      { handle: 'marshall', model: '', channels: ['#build'] },
      { handle: 'all', model: 'GPT', channels: ['#build'] },
    ],
    me: '@richard',
  });
  channel.type('ask @');
  assert.deepEqual(channel.rows, [
    { handle: 'alice', selected: 'true', text: '@alice Claude', note: 'Claude', childCount: 3 },
    { handle: 'claude-lead', selected: 'false', text: '@claude-lead Claude', note: 'Claude', childCount: 3 },
    { handle: 'marshall', selected: 'false', text: '@marshall', note: '', childCount: 1 },
    {
      handle: 'codex-c',
      selected: 'false',
      text: '@codex-c not in #build · arn:aws:bedrock:us-east-1:123456789012:inference-profile/gpt',
      note: 'not in #build · arn:aws:bedrock:us-east-1:123456789012:inference-profile/gpt',
      childCount: 3,
    },
    { handle: 'all', selected: 'false', text: '@all everyone in the channel', note: 'everyone in the channel', childCount: 3 },
  ]);

  channel.type('ask @c');
  assert.deepEqual(channel.listed, ['claude-lead', 'codex-c', 'alice']);
  channel.type('ask @AL');
  assert.deepEqual(channel.listed, ['alice', 'all', 'marshall']);
  channel.type('ask @all');
  assert.deepEqual(channel.listed, ['all', 'marshall']);
  channel.type('mail me@c');
  assert.equal(channel.listed, null);
  channel.type('flag @-');
  assert.equal(channel.listed, null);
  channel.type('var @_init');
  assert.equal(channel.listed, null);

  // Exact-case DM recipient (@codex-c) ranks ahead of @claude-lead; wrong-case @Codex-c warns on all handles.
  const direct = harness({ agents, target: '@codex-c' });
  direct.type('@c');
  assert.deepEqual(direct.rows.map((r) => ({ handle: r.handle, note: r.note })), [
    { handle: 'codex-c', note: 'arn:aws:bedrock:us-east-1:123456789012:inference-profile/gpt' },
    { handle: 'claude-lead', note: 'only @codex-c sees this · Claude' },
    { handle: 'alice', note: 'only @codex-c sees this · Claude' },
  ]);

  const wrongCaseDirect = harness({ agents, target: '@Codex-c' });
  wrongCaseDirect.type('@codex');
  assert.deepEqual(wrongCaseDirect.rows.map((r) => ({ handle: r.handle, note: r.note })), [
    { handle: 'codex-c', note: 'only @Codex-c sees this · arn:aws:bedrock:us-east-1:123456789012:inference-profile/gpt' },
  ]);
});

test('Arrow keys update highlight and polite status without rebuilding DOM or overriding textbox role, Tab/Enter pick, and close restores chat-sheet ARIA', () => {
  const h = harness({ agents, chatOpen: true });
  h.type('@c');
  assert.equal(h.draws, 1);
  assert.equal(h.box.attributes.role, undefined);
  assert.equal(h.box.attributes['aria-multiline'], undefined);
  assert.equal(h.box.attributes['aria-activedescendant'], undefined);
  assert.equal(h.box.attributes['aria-autocomplete'], 'list');
  assert.equal(h.box.attributes['aria-controls'], 'mention-suggestions');
  assert.equal(h.box.attributes['aria-expanded'], 'true');
  assert.equal(h.status.textContent, '@claude-lead Claude (1 of 3)');

  for (const mod of [{ metaKey: true }, { ctrlKey: true }, { altKey: true }, { shiftKey: true }, { isComposing: true }]) {
    assert.equal(h.key('ArrowDown', mod).taken, false);
  }
  assert.equal(h.status.textContent, '@claude-lead Claude (1 of 3)');

  assert.equal(h.key('ArrowDown').taken, true);
  assert.equal(h.draws, 1);
  assert.equal(h.rows[0].selected, 'false');
  assert.equal(h.rows[1].selected, 'true');
  assert.match(h.status.textContent, /^@codex-c not in #build .* \(2 of 3\)$/);

  assert.equal(h.key('Tab').taken, true);
  assert.equal(h.box.value, '@codex-c ');
  assert.equal(h.listed, null);
  assert.equal(h.status.textContent, '');
  assert.equal(h.box.attributes['aria-autocomplete'], undefined);
  assert.equal(h.box.attributes['aria-controls'], 'chat-sheet');
  assert.equal(h.box.attributes['aria-expanded'], 'true');
  assert.equal(h.key('Enter').taken, false);
});

test('mid-handle carets, key rollover before keyup, and programmatic composer clears never corrupt text or throw', () => {
  const h = harness({ agents });
  h.type('ask @codex-c about this', 7);
  assert.equal(h.listed, null);

  h.type('ask @c about this', 6);
  assert.deepEqual(h.listed, ['claude-lead', 'codex-c', 'alice']);
  assert.equal(h.key('Enter').taken, true);
  assert.equal(h.box.value, 'ask @claude-lead about this');
  assert.equal(h.box.selectionStart, 'ask @claude-lead '.length);
  assert.equal(h.listed, null);

  h.type('hi @c');
  assert.equal(h.listed.length, 3);
  h.box.selectionStart = h.box.selectionEnd = 0;
  assert.equal(h.key('Enter').taken, false);
  assert.equal(h.listed, null);
  assert.equal(h.box.value, 'hi @c');

  h.type('hi @c');
  h.box.value = '';
  h.box.selectionStart = h.box.selectionEnd = 0;
  h.mention.refresh();
  assert.equal(h.listed, null);
  assert.equal(h.key('Enter').taken, false);

  h.type('@c');
  assert.equal(h.key('Escape').taken, true);
  h.box.value = '';
  h.box.selectionStart = h.box.selectionEnd = 0;
  h.mention.refresh();
  h.type('@');
  assert.deepEqual(h.listed, ['alice', 'claude-lead', 'codex-c', 'all']);

  let downPrevented = false;
  const firstRow = h.list.children[0];
  h.list.emit('mousedown', { button: 0, target: firstRow, preventDefault() { downPrevented = true; } });
  assert.equal(downPrevented, true);
  assert.equal(h.box.value, '@');

  h.list.emit('click', { button: 2, target: firstRow });
  assert.equal(h.box.value, '@');

  h.list.emit('click', { button: 0, target: firstRow });
  assert.equal(h.box.value, '@alice ');
  assert.equal(h.listed, null);
});

test('passive heartbeat polls preserve DOM nodes, scroll position, and live-region quietness, while membership changes rebuild cleanly', () => {
  const h = harness({ agents });
  h.type('hi @c');
  assert.equal(h.key('Escape').taken, true);
  h.type('hi @co');
  assert.equal(h.listed, null);
  assert.equal(h.key('Escape').taken, false);

  h.moveCaret(3, 6);
  h.type('hi @al');
  assert.deepEqual(h.listed, ['alice', 'all']);

  h.key('ArrowDown'); // highlight 'all'
  const firstNodeBeforePoll = h.list.children[0];
  const drawsBeforePoll = h.draws;
  const scrollsBeforePoll = h.scrolls;
  const announcementsBeforePoll = h.announcements;

  // Heartbeat poll changes only last_seen_unix: DOM nodes stay attached, scroll does not jump, and live region stays quiet.
  h.state.view = {
    agents: agents.map((a) => ({ ...a, last_seen_unix: 9999 })),
  };
  h.mention.refresh();
  assert.equal(h.draws, drawsBeforePoll);
  assert.equal(h.scrolls, scrollsBeforePoll);
  assert.equal(h.announcements, announcementsBeforePoll);
  assert.equal(h.list.children[0], firstNodeBeforePoll);

  // Roster change adds 'alex': list rebuilds and preserves highlighted 'all' at index 2.
  h.state.view = {
    agents: [...agents, { handle: 'alex', model: 'Gemini', channels: ['#build'] }],
  };
  h.mention.refresh();
  assert.equal(h.draws, drawsBeforePoll + 1);
  assert.equal(h.scrolls, scrollsBeforePoll);
  assert.equal(h.announcements, announcementsBeforePoll);
  assert.deepEqual(h.listed, ['alex', 'alice', 'all']);
  assert.equal(h.rows[2].selected, 'true');

  h.blur();
  assert.equal(h.listed, null);
  assert.equal(h.status.textContent, '');
});
