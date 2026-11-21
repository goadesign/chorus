// Exercise the dashboard Markdown lexer/renderer in stream.js: GFM tables
// (including inline-code pipes, <br> breaks, alignment, and accessible scroll
// wrappers), GFM alerts, <details>/<summary>, fenced code headers and diffs,
// footnotes, safe inline tags, CSP-safe image links, XSS inertness, Markdown
// pins, and live-poll disclosure state preservation.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const markedSource = await readFile(new URL('./ui/marked.umd.js', import.meta.url), 'utf8');
const streamSource = await readFile(new URL('./ui/stream.js', import.meta.url), 'utf8');

// classSet simulates DOMTokenList for classList checks.
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
    toString: () => [...set].join(' '),
  };
}

// matchesSelector evaluates simple and descendant selectors used in tests.
function matchesSelector(node, selector) {
  return String(selector).split(',').some((branch) => {
    const chain = branch.trim().split(/\s+/).filter(Boolean);
    if (chain.length === 0 || !matchesPart(node, chain.at(-1))) return false;
    let cur = node.parentElement;
    for (let i = chain.length - 2; i >= 0; i--) {
      while (cur && cur.nodeType === 1 && !matchesPart(cur, chain[i])) {
        cur = cur.parentElement;
      }
      if (!cur || cur.nodeType !== 1) return false;
      cur = cur.parentElement;
    }
    return true;
  });
}
// matchesPart evaluates one compound selector (tag, classes, attributes).
function matchesPart(node, raw) {
  let s = raw.trim();
  if (!s) return false;
  const attrs = [];
  s = s.replace(/\[([^\]=]+)(?:="([^"]*)")?\]/g, (_, name, val) => {
    attrs.push([name, val]);
    return '';
  });
  const parts = s.split(/(?=[.#])/);
  for (const part of parts) {
    if (!part) continue;
    if (part.startsWith('.')) {
      if (!node.classList.contains(part.slice(1))) return false;
    } else if (part.startsWith('#')) {
      if (node.id !== part.slice(1)) return false;
    } else if (node.tagName !== part.toUpperCase()) {
      return false;
    }
  }
  for (const [name, val] of attrs) {
    if (name === 'open') {
      if (!node.open) return false;
    } else if (name.startsWith('data-')) {
      const camel = name.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      if (!(camel in node.dataset)) return false;
      if (val !== undefined && node.dataset[camel] !== val) return false;
    } else {
      const actual = node.attributes[name];
      if (actual === undefined) return false;
      if (val !== undefined && actual !== val) return false;
    }
  }
  return true;
}

// nodeText concatenates visible text content across simulated DOM children.
function nodeText(node) {
  if (node === null || node === undefined) return '';
  if (typeof node === 'string') return node;
  if (node.nodeType === 3) return node.nodeValue;
  return (node.childNodes || []).map(nodeText).join('');
}

// makeText creates a minimal DOM Text node.
function makeText(value) {
  return { nodeType: 3, nodeValue: String(value), parentElement: null };
}

// makeNode constructs a minimal DOM Element or DocumentFragment.
function makeNode(tag, isFragment = false) {
  let _className = '';
  const listeners = new Map();
  const node = {
    nodeType: isFragment ? 11 : 1,
    tagName: isFragment ? '#FRAGMENT' : tag.toUpperCase(),
    id: '',
    hidden: false,
    open: false,
    disabled: false,
    checked: false,
    value: '',
    type: '',
    href: '',
    target: '',
    rel: '',
    title: '',
    scope: '',
    tabIndex: -1,
    start: 1,
    isConnected: true,
    dataset: {},
    attributes: {},
    style: {},
    classList: classSet(''),
    childNodes: [],
    parentElement: null,
    get children() {
      return this.childNodes.filter((c) => c.nodeType === 1);
    },
    get lastChild() {
      return this.childNodes.at(-1) || null;
    },
    get nextElementSibling() {
      if (!this.parentElement) return null;
      const sibs = this.parentElement.children;
      const idx = sibs.indexOf(this);
      return idx >= 0 && idx + 1 < sibs.length ? sibs[idx + 1] : null;
    },
    get previousElementSibling() {
      if (!this.parentElement) return null;
      const sibs = this.parentElement.children;
      const idx = sibs.indexOf(this);
      return idx > 0 ? sibs[idx - 1] : null;
    },
    get className() { return _className; },
    set className(v) {
      _className = String(v || '');
      this.classList = classSet(_className);
    },
    get textContent() { return nodeText(this); },
    set textContent(v) {
      this.childNodes = [];
      if (v !== '' && v !== null && v !== undefined) this.append(makeText(v));
    },
    hasChildNodes() { return this.childNodes.length > 0; },
    setAttribute(k, v) {
      this.attributes[k] = String(v);
      if (k === 'open') this.open = true;
    },
    removeAttribute(k) {
      delete this.attributes[k];
      if (k === 'open') this.open = false;
    },
    getAttribute(k) { return this.attributes[k] ?? null; },
    addEventListener(event, cb) {
      const list = listeners.get(event) || [];
      list.push(cb);
      listeners.set(event, list);
    },
    async click() {
      for (const cb of listeners.get('click') || []) {
        await cb({ currentTarget: this, target: this, stopPropagation() {}, preventDefault() {} });
      }
    },
    append(...items) {
      for (const item of items) {
        if (item === null || item === undefined) continue;
        if (typeof item === 'string') {
          const t = makeText(item);
          t.parentElement = this;
          this.childNodes.push(t);
        } else if (item.nodeType === 11) {
          for (const ch of item.childNodes.splice(0)) {
            ch.parentElement = this;
            this.childNodes.push(ch);
          }
        } else {
          item.parentElement = this;
          this.childNodes.push(item);
        }
      }
    },
    appendChild(item) { this.append(item); return item; },
    replaceChildren(...items) {
      this.childNodes = [];
      this.append(...items);
    },
    remove() {
      if (!this.parentElement) return;
      this.parentElement.childNodes = this.parentElement.childNodes.filter((c) => c !== this);
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
      return matchesSelector(this, selector);
    },
    closest(selector) {
      let cur = this;
      while (cur && cur.nodeType === 1) {
        if (cur.matches(selector)) return cur;
        cur = cur.parentElement;
      }
      return null;
    },
    querySelectorAll(selector) {
      const out = [];
      const walk = (root) => {
        for (const ch of root.childNodes) {
          if (ch.nodeType === 1) {
            if (ch.matches(selector)) out.push(ch);
            walk(ch);
          }
        }
      };
      walk(this);
      return out;
    },
    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    },
    getBoundingClientRect() {
      return { top: 0, bottom: 20, left: 0, right: 200, width: 200, height: 20 };
    },
    focus() {},
    scrollIntoView() {},
  };
  return node;
}

// loadStream evaluates marked.umd.js and stream.js in an isolated vm context
// and returns the ChorusStream API plus clipboard/inspection helpers.
function loadStream() {
  const copied = [];
  const inspected = [];
  let innerHTMLWrites = 0;
  const document = {
    visibilityState: 'visible',
    activeElement: null,
    addEventListener() {},
    getElementById() { return null; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    createDocumentFragment() { return makeNode('#fragment', true); },
    createTextNode(v) { return makeText(v); },
    createElement(tag) {
      const node = makeNode(tag);
      // Guard against accidental innerHTML writes anywhere except the detached
      // entity-decoder <textarea> created once at stream.js load time.
      Object.defineProperty(node, 'innerHTML', {
        get() { return ''; },
        set(val) {
          if (tag.toLowerCase() === 'textarea') {
            node.value = val === '&amp;' ? '&' : val;
            return;
          }
          innerHTMLWrites += 1;
        },
      });
      return node;
    },
  };
  const window = {
    ChorusUI: {
      resolveIdentity(handle) {
        if (handle === 'alice' || handle === 'bob') return { known: true, model: 'claude-sonnet' };
        return null;
      },
      handleBadge(handle) {
        const span = makeNode('span');
        span.className = 'handle-badge';
        span.textContent = `@${handle}`;
        return span;
      },
      svgIcon(name, cls = 'icon') {
        const span = makeNode('span');
        span.className = cls;
        span.dataset.icon = name;
        return span;
      },
      formatMessageCount(n) { return { text: String(n), title: `${n} messages` }; },
      matchesMessage() { return true; },
      agentColorClass() { return 'agent-c0'; },
      actions: { inspectAgent(h, el) { inspected.push({ handle: h, el }); } },
    },
    matchMedia() { return { matches: false }; },
  };
  const context = {
    window,
    document,
    globalThis: null,
    URL,
    Intl,
    navigator: {
      clipboard: {
        async writeText(text) { copied.push(text); },
      },
    },
    IntersectionObserver: class {
      constructor(_, opts = {}) { this.root = opts.root || makeNode('div'); }
      observe() {}
      unobserve() {}
      disconnect() {}
    },
    setTimeout() { return 1; },
    clearTimeout() {},
  };
  context.globalThis = context;
  vm.runInNewContext(markedSource, context);
  vm.runInNewContext(streamSource, context);
  return {
    stream: context.window.ChorusStream,
    copied,
    inspected,
    get innerHTMLWrites() { return innerHTMLWrites; },
    makeNode,
  };
}

test('tables preserve pipes inside inline code, <br> breaks, alignment, and accessible wrappers', () => {
  const { stream, innerHTMLWrites } = loadStream();
  const md = [
    '| Service | Status | Command (`a|b`) | Notes |',
    '|:---|:---:|---:|---|',
    '| auth | Ready | `grep "a|b" | wc -l` | 6/6 pods<br>0 errors |',
    '| api | Degraded | ``a `|` b`` and `c\\|d` | Escaped `x\\\\|y`<br/>Literal `<br>` |',
  ].join('\n');

  const frag = stream.renderMessageText(md, { allowLinks: true, focusPrefix: 't1' });
  const wrap = frag.querySelector('.md-table-wrap');
  assert.ok(wrap);
  assert.equal(wrap.getAttribute('role'), 'region');
  assert.equal(wrap.getAttribute('aria-label'), 'Table');
  assert.equal(wrap.tabIndex, 0);

  const ths = wrap.querySelectorAll('th');
  assert.equal(ths.length, 4);
  assert.equal(ths[0].scope, 'col');
  assert.equal(ths[0].style.textAlign, 'left');
  assert.equal(ths[1].style.textAlign, 'center');
  assert.equal(ths[2].style.textAlign, 'right');
  assert.equal(ths[2].querySelector('code')?.textContent, 'a|b');

  const rows = wrap.querySelectorAll('tbody tr');
  assert.equal(rows.length, 2);
  const row0Cells = rows[0].querySelectorAll('td');
  assert.equal(row0Cells.length, 4);
  assert.equal(row0Cells[2].querySelector('code')?.textContent, 'grep "a|b" | wc -l');
  assert.equal(row0Cells[3].querySelectorAll('br').length, 1);
  assert.equal(row0Cells[3].textContent, '6/6 pods0 errors');

  const row1Cells = rows[1].querySelectorAll('td');
  assert.equal(row1Cells.length, 4);
  const row1Codes = row1Cells[2].querySelectorAll('code').map((c) => c.textContent);
  assert.deepEqual(row1Codes, ['a `|` b', 'c|d']);
  assert.equal(row1Cells[3].querySelectorAll('br').length, 1);
  const row1NoteCodes = row1Cells[3].querySelectorAll('code').map((c) => c.textContent);
  assert.deepEqual(row1NoteCodes, ['x\\\\|y', '<br>']);

  // Non-interactive mode (hover tooltip) omits tabIndex on the scroll wrapper.
  const hoverFrag = stream.renderMessageText(md, { allowLinks: false });
  assert.equal(hoverFrag.querySelector('.md-table-wrap').tabIndex, -1);
  assert.equal(innerHTMLWrites, 0);
});

test('header-only tables and tables nested in blockquotes and list items render intact', () => {
  const { stream } = loadStream();
  const md = [
    '| A | B |',
    '|---|---|',
    '',
    '> | Col1 | Col2 |',
    '> |---|---|',
    '> | `x|y` | ok |',
    '',
    '- Summary:',
    '  | K | V |',
    '  |---|---|',
    '  | `p|q` | 1 |',
  ].join('\n');

  const frag = stream.renderMessageText(md);
  const tables = frag.querySelectorAll('table');
  assert.equal(tables.length, 3);
  assert.equal(tables[0].querySelectorAll('thead th').length, 2);
  assert.equal(tables[0].querySelector('tbody'), null);
  assert.equal(tables[1].querySelector('tbody td code')?.textContent, 'x|y');
  assert.equal(tables[2].querySelector('tbody td code')?.textContent, 'p|q');
});

test('GFM alerts render styled callouts for all five variants while regular blockquotes stay quotes', () => {
  const { stream } = loadStream();
  const md = [
    '> [!NOTE]',
    '> Note line with **bold**.',
    '',
    '> [!tip]',
    '>',
    '> Tip paragraph.',
    '',
    '> [!IMPORTANT]',
    '> Key requirement.',
    '',
    '> [!WARNING]',
    '> Watch out.',
    '',
    '> [!CAUTION]',
    '> Irreversible step.',
    '',
    '> [!CUSTOM]',
    '> Not a GFM alert.',
    '',
    '> [!NOTE] inline text stays a blockquote',
  ].join('\n');

  const frag = stream.renderMessageText(md);
  for (const [kind, label] of [
    ['note', 'Note'],
    ['tip', 'Tip'],
    ['important', 'Important'],
    ['warning', 'Warning'],
    ['caution', 'Caution'],
  ]) {
    const box = frag.querySelector(`.md-alert-${kind}`);
    assert.ok(box, `expected .md-alert-${kind}`);
    assert.equal(box.getAttribute('role'), 'note');
    assert.equal(box.querySelector('.md-alert-title')?.textContent, label);
  }
  assert.equal(frag.querySelectorAll('blockquote.md-quote').length, 2);
});

test('<details>/<summary> supports nested Markdown, ignores fenced </details>, and keeps attributes inert', () => {
  const { stream } = loadStream();
  const md = [
    '<details>',
    '<summary>Logs for **build** `v1`</summary>',
    '',
    '| Step | Result |',
    '|---|---|',
    '| test | `ok|pass` |',
    '',
    '```html',
    '<details><summary>Example</summary></details>',
    '```',
    '',
    '</details>',
    '',
    '<details open>',
    '<summary>Open section</summary>',
    'Visible body.',
    '</details>',
    '',
    '<details onmouseover="alert(1)"><summary>Bad</summary>nope</details>',
  ].join('\n');

  const frag = stream.renderMessageText(md, { allowLinks: true, focusPrefix: 'd1' });
  const blocks = frag.querySelectorAll('details.md-details');
  assert.equal(blocks.length, 2);
  assert.equal(blocks[0].open, false);
  assert.equal(blocks[0].querySelector('.md-details-summary strong')?.textContent, 'build');
  assert.equal(blocks[0].querySelector('.md-details-body td code')?.textContent, 'ok|pass');
  assert.equal(
    blocks[0].querySelector('.md-details-body pre code')?.textContent,
    '<details><summary>Example</summary></details>'
  );
  assert.equal(blocks[1].open, true);
  assert.match(blocks[1].querySelector('.md-details-body')?.textContent || '', /Visible body\./);

  // Attribute-bearing <details> stays plain text, never a real <details> node.
  const paragraphs = frag.querySelectorAll('p.md-p').map((p) => p.textContent);
  assert.ok(paragraphs.some((t) => t.includes('<details onmouseover="alert(1)">')));
});

test('fenced code blocks include language header, working Copy button, and diff line highlighting', async () => {
  const { stream, copied } = loadStream();
  const diffCode = [
    '@@ -1,3 +1,3 @@',
    '--- a/file.go',
    '+++ b/file.go',
    ' unchanged',
    '-oldLine()',
    '+newLine()',
  ].join('\n');
  const md = '```diff\n' + diffCode + '\n```';

  const frag = stream.renderMessageText(md, { allowLinks: true, focusPrefix: 'c1' });
  const block = frag.querySelector('.md-code-block');
  assert.ok(block);
  assert.equal(block.querySelector('.md-code-lang')?.textContent, 'diff');
  const code = block.querySelector('pre.message-code code');
  assert.equal(code.textContent, diffCode);
  assert.equal(code.querySelectorAll('.md-diff-hunk').length, 1);
  assert.equal(code.querySelectorAll('.md-diff-add').length, 1);
  assert.equal(code.querySelectorAll('.md-diff-del').length, 1);

  const copyBtn = block.querySelector('.md-code-copy');
  assert.ok(copyBtn);
  await copyBtn.click();
  assert.deepEqual(copied, [diffCode]);
  assert.match(copyBtn.textContent, /Copied/);

  // Non-interactive hover mode omits header and copy button.
  const hoverFrag = stream.renderMessageText(md, { allowLinks: false });
  assert.equal(hoverFrag.querySelector('.md-code-header'), null);
});

test('safe inline tags, headings h1-h6, CSP-safe image links, and XSS inertness', () => {
  const { stream, innerHTMLWrites } = loadStream();
  const md = [
    '# H1',
    '## H2',
    '### H3',
    '#### H4',
    '##### H5',
    '###### H6',
    '',
    'Press <kbd>Cmd</kbd>+<kbd>K</kbd> for H<sub>2</sub>O, x<sup>2</sup>, and <mark>key</mark>.',
    '',
    '![System diagram](https://example.com/arch.png "Arch") and [![Badge](https://example.com/b.svg)](https://example.com/home)',
    '',
    '![Blocked](javascript:alert(1)) <script>alert(1)</script> <img src=x onerror=alert(1)> <kbd onclick="evil()">no</kbd>',
  ].join('\n');

  const frag = stream.renderMessageText(md, { allowLinks: true, focusPrefix: 's1' });
  for (let level = 1; level <= 6; level++) {
    assert.ok(frag.querySelector(`.md-h${level}`), `expected .md-h${level}`);
  }
  assert.deepEqual(frag.querySelectorAll('kbd.md-kbd').map((n) => n.textContent), ['Cmd', 'K']);
  assert.equal(frag.querySelector('sub.md-sub')?.textContent, '2');
  assert.equal(frag.querySelector('sup.md-sup')?.textContent, '2');
  assert.equal(frag.querySelector('mark.md-mark')?.textContent, 'key');

  // External images render as safe links, never <img> tags (preserves img-src 'self' data:).
  assert.equal(frag.querySelectorAll('img').length, 0);
  const links = frag.querySelectorAll('a.md-link');
  assert.equal(links.length, 2);
  assert.equal(links[0].href, 'https://example.com/arch.png');
  assert.equal(links[0].textContent, 'System diagram');
  assert.ok(links[0].classList.contains('md-image-link'));
  assert.equal(links[1].href, 'https://example.com/home');
  assert.equal(links[1].textContent, 'Badge');
  assert.equal(links[1].querySelector('a'), null);

  // Raw dangerous HTML and javascript: schemes remain inert text.
  assert.equal(frag.querySelector('script'), null);
  assert.match(frag.textContent, /<script>alert\(1\)<\/script>/);
  assert.match(frag.textContent, /<kbd onclick="evil\(\)">no<\/kbd>/);
  assert.equal(innerHTMLWrites, 0);
});

test('GFM footnotes number in reference order while unreferenced definitions and code refs stay literal', () => {
  const { stream } = loadStream();
  const md = [
    'See second[^b] then first[^a] and `[^b]` and undefined[^missing].',
    '',
    '[^a]: First defined, referenced second with **bold**.',
    '[^b]: Referenced first',
    '    with continuation line.',
    '[^unused]: Standalone line must remain visible.',
  ].join('\n');

  const frag = stream.renderMessageText(md, { allowLinks: true, focusPrefix: 'f1' });
  const refs = frag.querySelectorAll('sup.md-footnote-ref');
  assert.deepEqual(refs.map((r) => r.textContent), ['[1]', '[2]']);
  assert.match(refs[0].title, /Referenced first/);

  const items = frag.querySelectorAll('.md-footnotes .md-footnote-item');
  assert.equal(items.length, 2);
  assert.match(items[0].textContent, /Referenced first/);
  assert.match(items[0].textContent, /continuation line\./);
  assert.equal(items[1].querySelector('strong')?.textContent, 'bold');

  // Undefined refs and unreferenced definitions stay visible in body text.
  const bodyText = frag.querySelectorAll('p.md-p').map((p) => p.textContent).join('\n');
  assert.match(bodyText, /\[\^missing\]/);
  assert.match(bodyText, /\[\^unused\]: Standalone line must remain visible\./);
});

test('Markdown pins render formatted tables with toggle, and live polls preserve open <details>', async () => {
  const { stream, makeNode } = loadStream();
  const pinEl = makeNode('div');
  const mentionsEl = makeNode('div');
  const titleEl = makeNode('h1');
  const subtitleEl = makeNode('span');
  const streamEl = makeNode('div');
  const scrollEl = makeNode('div');

  const view = {
    swarm: 'demo',
    agents: [{ handle: 'alice', role: 'lead', model: 'claude-sonnet', channels: ['#ops'], last_seen_unix: 1, watching_on_this_server: true }],
    channels: [{
      name: '#ops',
      head_seq: 2,
      positions: [],
      pin: {
        seq: 1,
        sender: 'alice',
        sent_at_ms: 1000,
        text: '## Release Matrix\n\n| Service | Owner |\n|---|---|\n| auth | @alice |',
      },
      messages: [
        {
          seq: 1,
          sender: 'alice',
          sent_at_ms: 1000,
          text: '> [!WARNING]\n> Check rollout table\n\n<details>\n<summary>Details</summary>\nHidden log\n</details>',
        },
      ],
    }],
    inboxes: [],
  };
  const state = {
    activeSwarm: 'demo',
    view,
    selectedTarget: { kind: 'channel', name: '#ops' },
    viewMode: 'stream',
    expandAllMessages: true,
    filterAgent: '',
    searchQuery: '',
    selectedMessage: null,
    timeWindow: null,
  };
  const els = { pinEl, mentionsEl, titleEl, subtitleEl, streamEl, scrollEl, detailsEl: null };
  const actions = { inspectAgent() {}, revealMessage() {}, clearMessage() {}, setReplyTo() {}, composeDM() {}, updatePin: async () => {} };

  stream.render(els, state, actions);

  // Markdown pin renders a real table and offers Show original text toggle.
  const pinTable = pinEl.querySelector('.pin-markdown-body table.md-table');
  assert.ok(pinTable, 'expected Markdown table inside pin body');
  const rawPre = pinEl.querySelector('.pin-raw-text');
  assert.equal(rawPre.hidden, true);
  const toggleBtn = pinEl.querySelector('.pin-raw-toggle');
  assert.equal(toggleBtn.textContent, 'Show original text');
  await toggleBtn.click();
  assert.equal(rawPre.hidden, false);
  assert.equal(toggleBtn.textContent, 'Show formatted view');

  // Collapsed message headline strips [!WARNING] to readable "Warning: Check rollout table".
  assert.equal(streamEl.querySelector('.message-headline')?.textContent, 'Warning: Check rollout table');

  // Opening a Markdown <details> inside a message survives a live poll redraw.
  const mdDetails = streamEl.querySelector('details.md-details');
  assert.ok(mdDetails);
  assert.equal(mdDetails.open, false);
  mdDetails.open = true;

  stream.render(els, state, actions);
  const mdDetailsAfter = streamEl.querySelector('details.md-details');
  assert.equal(mdDetailsAfter.open, true);

  // resetDisclosures closes message disclosures without corrupting Markdown <details> open state.
  state.expandAllMessages = false;
  stream.resetDisclosures(streamEl, state);
  assert.equal(streamEl.querySelector('details.message-disclosure').open, false);
  assert.equal(streamEl.querySelector('details.md-details').open, true);
});

test('tables never corrupt code blocks or mismatched prose and handle tricky GFM table layouts', () => {
  const { stream } = loadStream();

  // 1. 4-space indented code block containing pipes inside backticks stays untouched.
  const indented = stream.renderMessageText('    | Col | Cmd |\n    |-----|-----|\n    | 1   | `a|b` |');
  assert.equal(indented.querySelector('table'), null);
  assert.equal(indented.querySelector('pre code')?.textContent, '| Col | Cmd |\n|-----|-----|\n| 1   | `a|b` |');

  // 2. 4-space fenced code block inside a list item stays untouched.
  const listFence = stream.renderMessageText('1. item:\n    ```md\n    | A | B |\n    |---|---|\n    | `a|b` | 1 |\n    ```');
  assert.equal(listFence.querySelector('table'), null);
  assert.match(listFence.querySelector('pre code')?.textContent || '', /`a\|b`/);
  assert.doesNotMatch(listFence.querySelector('pre code')?.textContent || '', /`a\\\|b`/);

  // 3. Mismatched column counts fall back to a paragraph without injecting backslashes.
  const mismatch = stream.renderMessageText('| a | b |\n|---|---|---|\n| `x|y` | 1 | 2 |');
  assert.equal(mismatch.querySelector('table'), null);
  assert.equal(mismatch.querySelector('code.md-inline-code')?.textContent, 'x|y');

  // 4. Blockquote with inconsistent spacing, bullet-line table, pipe-less body row, and 1-col table.
  const bq = stream.renderMessageText('> | Col1 | Col2 |\n>|---|---|\n> | `a|b` | ok |');
  assert.equal(bq.querySelector('blockquote table tbody td code')?.textContent, 'a|b');

  const bullet = stream.renderMessageText('- | K | V |\n  |---|---|\n  | `a|b` | 1 |');
  assert.equal(bullet.querySelector('li table tbody td code')?.textContent, 'a|b');

  const sparse = stream.renderMessageText('| A | B |\n|---|---|\nfoo\n| `x|y` | 2 |');
  const sparseRows = sparse.querySelectorAll('tbody tr');
  assert.equal(sparseRows.length, 2);
  assert.equal(sparseRows[1].querySelector('td code')?.textContent, 'x|y');

  const oneCol = stream.renderMessageText('| Cmd\n| ---\n| `a|b`');
  assert.equal(oneCol.querySelector('tbody td code')?.textContent, 'a|b');
});

test('footnotes preserve unreferenced single-word/URL defs, ignore code pseudo-refs, and resolve chains', () => {
  const { stream } = loadStream();

  // Single-word and URL unreferenced definitions remain visible instead of being eaten as link defs.
  const singleWord = stream.renderMessageText('Hello\n\n[^unused]: Target');
  assert.match(singleWord.textContent, /\[\^unused\]: Target/);

  const urlDef = stream.renderMessageText('Hello\n\n[^unused]: https://example.com');
  assert.match(urlDef.textContent, /\[\^unused\]: https:\/\/example\.com/);
  assert.equal(urlDef.querySelector('a.md-link')?.href, 'https://example.com/');

  // 4-space indented code block containing [^a] does not swallow [^a]: real def?
  const indentedRef = stream.renderMessageText('    [^a]\n\n[^a]: real def?');
  assert.equal(indentedRef.querySelector('pre code')?.textContent, '[^a]');
  assert.match(indentedRef.querySelector('p.md-p')?.textContent || '', /\[\^a\]: real def\?/);
  assert.equal(indentedRef.querySelector('.md-footnotes'), null);

  // Chained footnotes and cycles resolve cleanly in order without dropping targets or looping.
  const chained = stream.renderMessageText('Main[^a]\n\n[^a]: Points to [^b] and [docs][d]\n[^b]: Loops to [^a]\n\n[d]: https://example.com/docs');
  const mainRefs = chained.children[0].querySelectorAll('sup.md-footnote-ref');
  assert.deepEqual(mainRefs.map((n) => n.textContent), ['[1]']);
  const fnItems = chained.querySelectorAll('.md-footnotes .md-footnote-item');
  assert.equal(fnItems.length, 2);
  assert.equal(fnItems[0].querySelector('sup.md-footnote-ref')?.textContent, '[2]');
  assert.equal(fnItems[0].querySelector('a.md-link')?.href, 'https://example.com/docs');
  assert.equal(fnItems[1].querySelector('sup.md-footnote-ref')?.textContent, '[1]');

  // Footnotes in <details><summary> number before footnotes in <details> body; image alt does not activate footnotes.
  const detailsOrder = stream.renderMessageText('<details>\n<summary>Sum[^a]</summary>\nBody[^b]\n</details>\n\n![Img[^c]](https://example.com/c.png)\n\n[^a]: First\n[^b]: Second\n[^c]: Unused in alt');
  assert.equal(detailsOrder.querySelector('.md-details-summary sup.md-footnote-ref')?.textContent, '[1]');
  assert.equal(detailsOrder.querySelector('.md-details-body sup.md-footnote-ref')?.textContent, '[2]');
  const orderedItems = detailsOrder.querySelectorAll('.md-footnotes .md-footnote-item');
  assert.deepEqual(orderedItems.map((n) => n.textContent), ['First', 'Second']);

  // Footnote definitions spanning multiple consecutive blank lines before indented continuations stay intact.
  const multiBlank = stream.renderMessageText('Ref[^m]\n\n[^m]: Para 1\n\n\n    Para 2');
  assert.match(multiBlank.querySelector('.md-footnotes')?.textContent || '', /Para 1.*Para 2/s);
});

test('<details> handles indented/list code blocks, attribute-bearing inner tags, nesting, and summary ref links', () => {
  const { stream } = loadStream();

  // 4-space fence inside list item inside <details> does not prematurely close outer <details>.
  const listFence = stream.renderMessageText([
    '<details>',
    '<summary>Outer</summary>',
    '- step:',
    '    ```html',
    '    </details>',
    '    ```',
    '',
    'After code',
    '</details>',
  ].join('\n'));
  const outer1 = listFence.querySelectorAll('details.md-details');
  assert.equal(outer1.length, 1);
  assert.match(outer1[0].querySelector('.md-details-body')?.textContent || '', /After code/);

  // Leading 4-space indented code block containing </details> stays a code block inside <details>.
  const indentedCode = stream.renderMessageText('<details>\n<summary>Code</summary>\n\n    </details>\n\nStill inside\n</details>');
  const outer2 = indentedCode.querySelector('details.md-details');
  assert.ok(outer2);
  assert.equal(outer2.querySelector('.md-details-body pre code')?.textContent, '</details>');
  assert.match(outer2.querySelector('.md-details-body')?.textContent || '', /Still inside/);

  // 4-space indented code block containing an unclosed ``` does not swallow </details>.
  const indentedFence = stream.renderMessageText('<details>\n<summary>Show</summary>\n\n    ```\n    code without closing fence\n\n</details>');
  const outerIndentedFence = indentedFence.querySelector('details.md-details');
  assert.ok(outerIndentedFence);
  assert.match(outerIndentedFence.querySelector('.md-details-body pre code')?.textContent || '', /code without closing fence/);

  // Fenced code block starting directly on a list bullet ignores inner </details>.
  const bulletFence = stream.renderMessageText('<details>\n<summary>Steps</summary>\n\n- ```html\n  </details>\n  ```\n\nDone\n</details>');
  const outerBulletFence = bulletFence.querySelector('details.md-details');
  assert.ok(outerBulletFence);
  assert.match(outerBulletFence.querySelector('.md-details-body')?.textContent || '', /Done/);

  // Escaped \<details> inside <details> does not imbalance nesting depth.
  const escapedDetails = stream.renderMessageText('<details>\n<summary>Escaped</summary>\nLiteral \\<details> tag\n</details>');
  const outerEscaped = escapedDetails.querySelector('details.md-details');
  assert.ok(outerEscaped);
  assert.match(outerEscaped.querySelector('.md-details-body')?.textContent || '', /Literal <details> tag/);

  // Attribute-bearing inner <details> pairs with its closing tag without breaking outer <details>.
  const attrInner = stream.renderMessageText('<details>\n<summary>Outer</summary>\n<details onclick="x">\ninner\n</details>\nstill outer\n</details>');
  assert.equal(attrInner.querySelectorAll('details.md-details').length, 1);
  assert.match(attrInner.querySelector('.md-details-body')?.textContent || '', /still outer/);

  // Nested valid <details> and reference link in <summary>.
  const nested = stream.renderMessageText('<details open>\n<summary>See [spec][1]</summary>\n\n<details>\n<summary>Inner</summary>\nNested text\n</details>\n</details>\n\n[1]: https://example.com');
  assert.equal(nested.querySelectorAll('details.md-details').length, 2);
  assert.equal(nested.querySelector('.md-details-summary')?.textContent, 'See spec');

  // Hover mode (allowLinks: false) renders a non-interactive container rather than <details>.
  const hover = stream.renderMessageText('<details>\n<summary>Summary</summary>\nBody\n</details>', { allowLinks: false });
  assert.equal(hover.querySelector('details'), null);
  assert.equal(hover.querySelector('.md-details-summary')?.textContent, 'Summary');
});

test('collapsed previews stay single-line without <br>, number footnotes consistently, and render Markdown pin titles', () => {
  const { stream, makeNode } = loadStream();
  const makeEnv = (pinText, messages, pinSeq = 1) => {
    const pinEl = makeNode('div');
    const mentionsEl = makeNode('div');
    const titleEl = makeNode('h1');
    const subtitleEl = makeNode('span');
    const streamEl = makeNode('div');
    const scrollEl = makeNode('div');
    const state = {
      activeSwarm: 'demo',
      view: {
        swarm: 'demo',
        agents: [],
        channels: [{
          name: '#ops',
          head_seq: messages.length,
          positions: [],
          pin: pinText ? { seq: pinSeq, sender: 'alice', sent_at_ms: 1000, text: pinText } : null,
          messages,
        }],
        inboxes: [],
      },
      selectedTarget: { kind: 'channel', name: '#ops' },
      viewMode: 'stream',
      expandAllMessages: false,
      filterAgent: '',
      searchQuery: '',
      selectedMessage: null,
      timeWindow: null,
    };
    const els = { pinEl, mentionsEl, titleEl, subtitleEl, streamEl, scrollEl, detailsEl: null };
    const actions = { inspectAgent() {}, revealMessage() {}, clearMessage() {}, setReplyTo() {}, composeDM() {}, updatePin: async () => {} };
    stream.render(els, state, actions);
    return { els, state, actions };
  };

  // 1. Table <br> cells, <details><summary> <br>, multi-line footnotes, and code-first alerts.
  const { els } = makeEnv(null, [
    { seq: 1, sender: 'alice', sent_at_ms: 1000, text: '| H1<br>H2 | Col2 |\n|---|---|\n| R1<br>R2 | val |' },
    { seq: 2, sender: 'alice', sent_at_ms: 2000, text: '<details>\n<summary>Part A<br>Part B</summary>\nBody\n</details>' },
    { seq: 3, sender: 'alice', sent_at_ms: 3000, text: 'Line one[^a]\nLine two[^b]\n\n[^a]: First\n[^b]: Second' },
    { seq: 4, sender: 'alice', sent_at_ms: 4000, text: '> [!NOTE]\n>\n> ```html\n> &lt;div&gt; @alice\n> ```' },
  ]);

  // Stream renders newest first (#4, #3, #2, #1).
  const rows = els.streamEl.querySelectorAll('details.message-disclosure');
  assert.equal(rows.length, 4);
  const codeAlertHeadline = rows[0].querySelector('.message-headline');
  assert.equal(codeAlertHeadline?.textContent, 'Note: &lt;div&gt; @alice');
  assert.equal(codeAlertHeadline?.querySelector('.handle-badge'), null);
  assert.equal(rows[1].querySelector('.message-headline')?.textContent, 'Line one[1]');
  assert.equal(rows[1].querySelector('.message-supporting')?.textContent, 'Line two[2]');
  assert.equal(rows[2].querySelector('.message-headline')?.querySelector('br'), null);
  assert.equal(rows[2].querySelector('.message-headline')?.textContent, 'Part A');
  assert.equal(rows[2].querySelector('.message-supporting')?.textContent, 'Part B');
  assert.equal(rows[3].querySelector('.message-headline')?.querySelector('br'), null);
  assert.equal(rows[3].querySelector('.message-supporting')?.querySelector('br'), null);
  assert.equal(rows[3].querySelector('.message-headline')?.textContent, 'H1 H2 · Col2');
  assert.equal(rows[3].querySelector('.message-supporting')?.textContent, 'R1 R2 · val');

  // 2. Multi-line Markdown pin headlines strip tables, <details>, alerts, and footnotes cleanly.
  assert.equal(makeEnv('| Col1 | Col2 |\n|---|---|\n| a | b |', []).els.pinEl.querySelector('.pin-title')?.textContent, 'Pin #1 · Col1 · Col2');
  assert.equal(makeEnv('<details>\n<summary>Runbook</summary>\nSteps\n</details>', []).els.pinEl.querySelector('.pin-title')?.textContent, 'Pin #1 · Runbook');
  assert.equal(makeEnv('> [!WARNING]\n> Do not deploy', []).els.pinEl.querySelector('.pin-title')?.textContent, 'Pin #1 · Warning: Do not deploy');
  assert.equal(makeEnv('Goal[^a]\n\n[^a]: note', []).els.pinEl.querySelector('.pin-title')?.textContent, 'Pin #1 · Goal');

  // 3. Replacing a pin with a new seq resets <details> open state cleanly.
  const env = makeEnv('<details>\n<summary>Old</summary>\nBody\n</details>', [], 1);
  env.els.pinEl.querySelector('details.md-details').open = true;
  env.state.view.channels[0].pin = { seq: 2, sender: 'alice', sent_at_ms: 2000, text: '<details>\n<summary>New</summary>\nBody\n</details>' };
  stream.render(env.els, env.state, env.actions);
  assert.equal(env.els.pinEl.querySelector('details.md-details').open, false);
});
