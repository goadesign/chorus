// Exercise team editing, thinking levels, dismissal, and per-repo persistence without paid sessions.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';

const source = (await readFile(new URL('./main.js', import.meta.url), 'utf8'))
  .replace(/^import .*;\n/gm, '').replace('export default class ChorusLauncher', 'class ChorusLauncher')
  + '\nglobalThis.Launcher = ChorusLauncher;';
// Validate deferred row roles against the installed native API, not web ARIA names.
const herdrHome = process.env.HERDR_APP_HOME || path.join(os.homedir(), '.config', 'herdr-app');
const nativeTypes = await readFile(path.join(herdrHome, 'gpui-kit.d.ts'), 'utf8');
const roleDeclaration = nativeTypes.slice(nativeTypes.indexOf('export type Role =')).split(';')[0];
const roles = new Set(Array.from(roleDeclaration.matchAll(/"([a-z_]+)"/g), match => match[1]));

const DEFAULT_AGENTS = [
  { key: 'claude', name: 'Claude Code', installed: true, run: ['claude'] },
  { key: 'codex', name: 'Codex', installed: true, run: ['codex', '--no-daemon'] },
  { key: 'antigravity', name: 'Antigravity', installed: true, run: ['agy'] },
  { key: 'cursor', name: 'Cursor', installed: true, run: ['cursor-agent'] },
];

function catalogFor(key) {
  if (key === 'claude') {
    return { models: [{ id: 'claude-opus-5-5', name: 'Claude Opus 5.5' }, { id: 'opus', name: 'Opus' }, { id: 'sonnet', name: 'Sonnet' }], model_supported: true, thinking_supported: true, efforts: ['low', 'medium', 'high', 'xhigh', 'max'], yolo_flag: '--dangerously-skip-permissions', note: '' };
  }
  if (key === 'codex') {
    return {
      models: [
        { id: 'gpt-6.1-sol', name: 'GPT-6.1-Sol', efforts: ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'] },
        { id: 'gpt-6-luna', name: 'GPT-6-Luna', efforts: ['low', 'medium', 'high', 'xhigh', 'max'] },
      ],
      model_supported: true,
      thinking_supported: true,
      efforts: ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'],
      yolo_flag: '--dangerously-bypass-approvals-and-sandbox',
      note: '',
    };
  }
  if (key === 'antigravity') {
    return {
      models: [
        { id: 'gemini-4-argon', name: 'Gemini 4 Argon', efforts: ['low', 'medium', 'high'] },
        { id: 'gpt-oss-120b', name: 'GPT-OSS 120B', efforts: ['medium'] },
      ],
      model_supported: true,
      thinking_supported: true,
      efforts: ['low', 'medium', 'high', 'xhigh', 'max'],
      yolo_flag: '--dangerously-skip-permissions',
      note: '',
    };
  }
  return {
    models: [
      { id: 'claude-opus-5-5-high', name: 'Claude Opus 5.5 1M', efforts: ['low', 'medium', 'high', 'max'], variants: { low: 'claude-opus-5-5-low', medium: 'claude-opus-5-5-medium', high: 'claude-opus-5-5-high', max: 'claude-opus-5-5-max' } },
      { id: 'composer-2.5', name: 'Composer 2.5' },
    ],
    model_supported: true,
    thinking_supported: true,
    efforts: [],
    yolo_flag: '--yolo',
    note: '',
  };
}

// Native rendering is checked by Herdr; this host records interactive callbacks.
function harness({ initial = [], agents = DEFAULT_AGENTS.slice(0, 2), root = '/project', sharedStore = {} } = {}) {
  const controls = new Map();
  const focus = [];
  const scrolls = [];
  const closed = [];
  const notices = [];
  function element(id) {
    const data = {};
    const el = new Proxy(data, { get(target, key) {
      if (key === 'when') return (condition, branch) => condition ? branch(el) : el;
      if (key === 'id') return value => { controls.set(value, data); return el; };
      if (key === 'role') return value => { assert.ok(roles.has(value), `Unsupported native accessibility role: ${value}`); return el; };
      return (...args) => { data[key] = args; return el; };
    } });
    if (id) controls.set(id, data);
    return el;
  }
  const native = { new: id => element(id) };
  const input = () => ({ text: '', events: {}, on(event, callback) { this.events[event] = callback; },
    value() { return this.text; }, set_value(value) { this.text = value; }, focus() { focus.push('input'); } });
  const handles = () => ({ focus() { focus.push('handle'); }, is_focused() { return false; } });
  const tasks = [];
  const cx = { notify() {}, stop_propagation() {}, focus_handle: handles,
    spawn(callback) { tasks.push(Promise.resolve().then(() => callback(cx))); } };
  const clonedAgents = agents.map(item => ({ ...item }));
  const context = { View: class {}, div: () => element(), h_flex: () => element(), v_flex: () => element(),
    Button: native, Checkbox: native, Input: native, Popup: native, Select: native,
    VirtualListScrollHandle: { new: () => ({ scroll_to_item(index) { scrolls.push(index); } }) },
    v_virtual_list: (_id, count, _height, _key, render) => { render({ start: 0, end: Math.min(count, 3) }); return element(); },
    Scrollbar: { vertical: () => element() },
    InputState: { new: input }, theme: () => ({}), focused: () => ({ space: 'sp1', cwd: root }),
    spaces: () => [{ id: 'sp1' }], knownAgents: async () => clonedAgents, appHome: () => '/app',
    props: () => ({ overlay: 'ov-chorus' }), closeOverlay(id) { closed.push(id); },
    notify(item) { notices.push(item); },
    shared: key => sharedStore[key],
    setShared: (key, value) => { sharedStore[key] = JSON.parse(JSON.stringify(value)); },
    exec: async command => ({ code: 0, stdout: command === 'git' ? `${root}\n` : 'Chorus is running', stderr: '' }) };
  vm.runInNewContext(source, context);
  const view = new context.Launcher();
  let saved = initial;
  let failAfter = null;
  let inspects = 0;
  const launches = [];
  view.helper = async (action, request) => {
    if (action === 'inspect') {
      inspects += 1;
      return { rows: saved, session: 'session', tab: saved.length ? 'tab' : null, swarm: saved.length ? 'project' : null };
    }
    if (action === 'models') return catalogFor(request.key);
    launches.push(JSON.parse(JSON.stringify(request)));
    let created = 0;
    for (const row of request.agents) {
      if (saved.some(old => old.id === row.id)) continue;
      saved = [...saved, { ...row, terminal: `t${saved.length + 1}` }];
      if (++created === failAfter) throw new Error('Injected partial launch failure');
    }
    return { created, reused: request.agents.length - created };
  };
  view.init({}, cx);
  return { view, cx, controls, launches, focus, scrolls, closed, notices, sharedStore,
    get inspects() { return inspects; },
    async settle() { while (tasks.length) await tasks.shift(); },
    partialAfter(count) { failAfter = count; },
    render() { view.render(cx); },
    click(id, event = 'on_click', value = {}) { controls.get(id)[event][0](value, cx); } };
}

test('successful launch dismisses the dialog without a dead post-close inspect while duplicate rows retain independent choices', async () => {
  const h = harness();
  await h.settle();
  assert.equal(h.inspects, 1);
  assert.deepEqual(Array.from(h.view.rows, row => row.key), ['claude', 'codex']);
  h.view.addAgent(h.cx);
  const duplicate = h.view.rows[2];
  duplicate.model = 'sonnet';
  duplicate.thinking = 'high';
  duplicate.yolo = true;
  h.view.start(h.cx);
  await h.settle();
  assert.deepEqual(h.closed, ['ov-chorus']);
  assert.equal(h.notices.length, 1);
  assert.match(h.notices[0].body, /Opened 3 new panes; reused 0/);
  assert.equal(h.inspects, 1);
  assert.equal(h.launches[0].agents[0].model, 'claude-opus-5-5');
  assert.equal(h.launches[0].agents[0].thinking, null);
  assert.equal(h.launches[0].agents[1].model, 'gpt-6.1-sol');
  assert.equal(h.launches[0].agents[1].thinking, null);
  assert.equal(h.launches[0].agents[2].model, 'sonnet');
  assert.equal(h.launches[0].agents[2].thinking, 'high');
  assert.equal(h.launches[0].agents[2].yolo, true);
  await h.view.loadTeam(h.cx);
  h.view.busy = false;
  h.view.start(h.cx);
  await h.settle();
  assert.ok(h.launches[1].agents.every(row => Object.keys(row).join() === 'id'));
});

test('partial launch failure keeps the dialog open with actionable error, then dismisses on retry', async () => {
  const h = harness();
  await h.settle();
  h.view.rows[1].model = 'gpt-6-luna';
  h.view.rows[1].thinking = 'max';
  h.view.rows[1].yolo = true;
  const pendingID = h.view.rows[1].id;
  h.partialAfter(1);
  h.view.start(h.cx);
  await h.settle();
  assert.deepEqual(h.closed, []);
  assert.match(h.view.error, /Injected partial launch failure/);
  assert.equal(h.view.rows.filter(row => row.terminal).length, 1);
  const pending = h.view.rows.find(row => row.id === pendingID);
  assert.equal(pending.model, 'gpt-6-luna');
  assert.equal(pending.thinking, 'max');
  assert.equal(pending.yolo, true);
  h.partialAfter(null);
  h.view.start(h.cx);
  await h.settle();
  assert.deepEqual(h.closed, ['ov-chorus']);
  assert.deepEqual(h.launches[1].agents[0], { id: 'session-1' });
  assert.equal(h.launches[1].agents[1].thinking, 'max');
});

test('thinking selector adapts across Claude, Codex, Antigravity, and Cursor models without duplicating Cursor thinking state', async () => {
  const h = harness({ agents: DEFAULT_AGENTS });
  await h.settle();
  const [claudeRow, codexRow, agyRow, cursorRow] = h.view.rows;
  assert.equal(agyRow.model, 'gemini-4-argon');
  assert.equal(agyRow.thinking, 'high');
  assert.equal(cursorRow.model, 'claude-opus-5-5-high');
  assert.equal(cursorRow.thinking, null);

  // Codex: select Ultra on GPT-6.1-Sol, then switch to GPT-6-Luna (which lacks ultra) -> resets thinking cleanly.
  h.render();
  h.click(`${codexRow.id}-thinking`, 'on_open_change', true);
  h.render();
  h.click(`${codexRow.id}-thinking-content`, 'on_key_down', { key: 'end' });
  h.click(`${codexRow.id}-thinking`, 'on_confirm');
  assert.equal(codexRow.thinking, 'ultra');
  assert.ok(h.scrolls.length >= 2);
  h.render();
  codexRow.modelPicker.index = 2; // GPT-6-Luna
  h.click(`${codexRow.id}-model`, 'on_confirm');
  assert.equal(codexRow.model, 'gpt-6-luna');
  assert.equal(codexRow.thinking, null);

  // Cursor: switching Thinking to Max selects exact claude-opus-5-5-max slug while keeping row.thinking null; switching to Composer 2.5 disables Thinking.
  h.render();
  cursorRow.thinkingPicker.index = 3; // max
  h.click(`${cursorRow.id}-thinking`, 'on_confirm');
  assert.equal(cursorRow.thinking, null);
  assert.equal(cursorRow.model, 'claude-opus-5-5-max');
  h.render();
  cursorRow.modelPicker.index = 2; // Composer 2.5
  h.click(`${cursorRow.id}-model`, 'on_confirm');
  assert.equal(cursorRow.model, 'composer-2.5');
  assert.equal(cursorRow.thinking, null);
  h.render();
  assert.equal(h.controls.get(`${cursorRow.id}-thinking`).disabled[0], true);

  // A Cursor row with an unlisted/stale model ID remains ready to launch.
  cursorRow.model = 'claude-opus-5-5-xhigh';
  assert.equal(h.view.ready(), true);

  // Antigravity: custom suffixed model ID clears and disables separate Thinking flag.
  h.render();
  agyRow.modelPicker.index = 3; // Enter model ID…
  h.click(`${agyRow.id}-model`, 'on_confirm');
  agyRow.input.set_value('gemini-4-argon-low');
  agyRow.input.events.change({}, h.cx);
  assert.equal(agyRow.model, 'gemini-4-argon-low');
  assert.equal(agyRow.thinking, null);
  assert.equal(h.view.ready(), true);

  // Claude: explicit thinking level is preserved alongside model selection.
  h.render();
  claudeRow.thinkingPicker.index = 3; // high
  h.click(`${claudeRow.id}-thinking`, 'on_confirm');
  assert.equal(claudeRow.thinking, 'high');
});

test('reopening after untouched first-run or failed discovery preserves defaults once discovery succeeds', async () => {
  const sharedStore = {};
  const first = harness({ agents: DEFAULT_AGENTS, root: '/repos/untouched', sharedStore });
  first.view.helper = async action => {
    if (action === 'inspect') return { rows: [], session: 'session', tab: null, swarm: null };
    throw new Error('temporary discovery failure');
  };
  await first.settle();
  // Even if the person edits the Chorus name while discovery is down, unchosen rows keep their defaults later.
  first.view.input.set_value('custom-swarm');
  first.view.input.events.change({}, first.cx);

  const reopened = harness({ agents: DEFAULT_AGENTS, root: '/repos/untouched', sharedStore });
  await reopened.settle();
  assert.equal(reopened.view.swarm, 'custom-swarm');
  assert.deepEqual(
    Array.from(reopened.view.rows, row => ({ key: row.key, model: row.model, thinking: row.thinking })),
    [
      { key: 'claude', model: 'claude-opus-5-5', thinking: null },
      { key: 'codex', model: 'gpt-6.1-sol', thinking: null },
      { key: 'antigravity', model: 'gemini-4-argon', thinking: 'high' },
      { key: 'cursor', model: 'claude-opus-5-5-high', thinking: null },
    ],
  );
});

test('per-repository preferences persist across reopenings without affecting other repositories or live teams', async () => {
  const sharedStore = {};
  const first = harness({ agents: DEFAULT_AGENTS, root: '/repos/alpha', sharedStore });
  await first.settle();
  first.view.input.set_value('alpha-team');
  first.view.input.events.change({}, first.cx);
  // Keep only Claude (with App default model + max thinking + YOLO) and a second Codex row.
  first.view.rows = [first.view.rows[0], first.view.rows[1]];
  first.render();
  first.view.rows[0].modelPicker.index = 0; // App default
  first.click(`${first.view.rows[0].id}-model`, 'on_confirm');
  first.render();
  first.view.rows[0].thinkingPicker.index = 5; // max
  first.click(`${first.view.rows[0].id}-thinking`, 'on_confirm');
  first.click(`${first.view.rows[0].id}-yolo`, 'on_change', true);

  const reopened = harness({ agents: DEFAULT_AGENTS, root: '/repos/alpha', sharedStore });
  await reopened.settle();
  assert.equal(reopened.view.swarm, 'alpha-team');
  assert.equal(reopened.view.rows.length, 2);
  assert.equal(reopened.view.rows[0].key, 'claude');
  assert.equal(reopened.view.rows[0].model, null);
  assert.equal(reopened.view.rows[0].thinking, 'max');
  assert.equal(reopened.view.rows[0].yolo, true);

  // Another repository still starts with all installed defaults.
  const other = harness({ agents: DEFAULT_AGENTS, root: '/repos/beta', sharedStore });
  await other.settle();
  assert.equal(other.view.swarm, 'beta');
  assert.equal(other.view.rows.length, 4);

  // Picking a suffixed Antigravity model (or single-effort model without "high") stays ready on reopen, and untouched rows omit model/thinking.
  const agySaved = harness({ agents: DEFAULT_AGENTS, root: '/repos/gamma', sharedStore });
  await agySaved.settle();
  agySaved.render();
  const agyRow = agySaved.view.rows[2];
  agyRow.modelPicker.index = 3; // Enter model ID…
  agySaved.click(`${agyRow.id}-model`, 'on_confirm');
  agyRow.input.set_value('gemini-4-argon-low');
  agyRow.input.events.change({}, agySaved.cx);
  assert.deepEqual(sharedStore['chorus-launcher.repo:/repos/gamma'].rows[0], { key: 'claude', yolo: false });
  assert.deepEqual(sharedStore['chorus-launcher.repo:/repos/gamma'].rows[2], { key: 'antigravity', yolo: false, model: 'gemini-4-argon-low' });

  const agyReopened = harness({ agents: DEFAULT_AGENTS, root: '/repos/gamma', sharedStore });
  await agyReopened.settle();
  assert.equal(agyReopened.view.rows[2].model, 'gemini-4-argon-low');
  assert.equal(agyReopened.view.rows[2].thinking, null);
  assert.equal(agyReopened.view.ready(), true);

  // A saved thinking level no longer supported by the model blocks launch with an explicit reason.
  agyReopened.view.rows[1].model = 'gpt-6-luna';
  agyReopened.view.rows[1].thinking = 'ultra';
  assert.equal(agyReopened.view.ready(), false);
  assert.match(agyReopened.view.blockingReason(), /Unavailable|unavailable/);
});

test('adding all preserves edited rows and fills only missing client apps', async () => {
  const h = harness();
  await h.settle();
  h.view.rows = [h.view.rows[1]];
  h.view.rows[0].model = 'gpt-6-luna';
  h.view.addAll(h.cx);
  h.view.addAll(h.cx);
  assert.equal(h.view.rows.length, 2);
  assert.equal(h.view.rows[0].model, 'gpt-6-luna');
});

test('choosing App default survives a later model refresh instead of restoring the preset', async () => {
  const h = harness();
  await h.settle();
  const row = h.view.rows[0];
  assert.equal(row.model, 'claude-opus-5-5');
  h.render();
  row.modelPicker.index = 0;
  h.click(`${row.id}-model`, 'on_confirm');
  assert.equal(row.model, null);
  await h.view.loadModels(h.view.agents[0], h.cx);
  assert.equal(row.model, null);
});

test('apply YOLO to all affects only supported new panes and never existing settings', async () => {
  const h = harness({ initial: [{ id: 'existing', key: 'claude', terminal: 't1', model: null, thinking: null, yolo: null }] });
  await h.settle();
  h.view.addAll(h.cx);
  h.view.addAgent(h.cx);
  h.render();
  h.click('chorus-yolo-all');
  assert.equal(h.view.rows[0].recorded, false);
  assert.equal(h.view.rows[0].yolo, false);
  assert.ok(h.view.rows.filter(row => !row.terminal).every(row => row.yolo));
  h.render();
  h.click('chorus-yolo-all');
  assert.ok(h.view.rows.filter(row => !row.terminal).every(row => !row.yolo));
});

test('an empty custom model cannot silently use the default; picking custom focuses the input', async () => {
  const h = harness();
  await h.settle();
  const row = h.view.rows[0];
  row.yolo = true;
  h.render();
  h.click(`${row.id}-model`, 'on_open_change', true);
  row.modelPicker.index = 3; // sonnet
  h.render();
  h.click(`${row.id}-model`, 'on_confirm');
  assert.equal(row.model, 'sonnet');
  assert.equal(row.yolo, true);
  h.render();
  row.modelPicker.index = 4; // Enter model ID…
  h.focus.length = 0;
  h.click(`${row.id}-model`, 'on_confirm');
  assert.deepEqual(h.focus, ['handle', 'input']);
  row.custom = true;
  row.model = '';
  assert.equal(h.view.ready(), false);
  row.input.set_value('my-model');
  row.input.events.change({}, h.cx);
  assert.equal(h.view.ready(), true);
});

test('failed discovery exposes retry, keeps explicit model and thinking choices honest, and blocks launch until cleared', async () => {
  const h = harness();
  await h.settle();
  const row = h.view.rows[0];
  row.model = 'sonnet';
  row.thinking = 'high';
  row.yolo = true;
  h.view.helper = async () => { throw new Error('client unavailable'); };
  await h.view.loadModels(h.view.agents[0], h.cx);
  h.render();
  assert.ok(h.controls.has(`${row.id}-retry`));
  assert.equal(h.view.ready(), false);
  assert.equal(row.model, 'sonnet');
  assert.equal(row.thinking, 'high');
  assert.equal(row.yolo, true);
  h.click(`${row.id}-yolo`, 'on_change', false);
  assert.equal(h.view.ready(), false);
  row.modelPicker.index = 0;
  h.click(`${row.id}-model`, 'on_confirm');
  assert.equal(row.model, null);
  assert.equal(h.view.ready(), false);
  h.render();
  row.thinkingPicker.index = 0;
  h.click(`${row.id}-thinking`, 'on_confirm');
  assert.equal(row.thinking, null);
  assert.equal(h.view.ready(), true);
});

test('removing every new row gives an empty team and disables launch', async () => {
  const h = harness();
  await h.settle();
  for (const id of h.view.rows.map(row => row.id)) {
    h.render();
    h.click(`${id}-remove`);
  }
  assert.equal(h.view.rows.length, 0);
  assert.equal(h.view.ready(), false);
  h.view.start(h.cx);
  await h.settle();
  assert.equal(h.launches.length, 0);
});
