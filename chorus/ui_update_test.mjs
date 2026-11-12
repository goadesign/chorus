// Exercise dashboard approval, restart readiness, and existing draft/reload behavior.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('./ui/update.js', import.meta.url), 'utf8');
const available = {
  current_version: '0.9.0', latest_version: '0.10.0', update_available: true,
  release_url: 'https://github.com/goadesign/chorus/releases/tag/v0.10.0',
};

function response(status, body, headers = {}) {
  return { status, ok: status >= 200 && status < 300,
    json: async () => body, text: async () => typeof body === 'string' ? body : JSON.stringify(body),
    headers: new Headers({ 'Content-Type': 'application/json', ...headers }) };
}

// The fixture implements only browser APIs the served controller uses. Its
// timer advances the accepted 30s restart window without waiting in real time.
function harness(reply) {
  const elements = new Map();
  for (const id of ['update-banner', 'update-message', 'update-release', 'btn-update-chorus', 'btn-reload-ui', 'send-text']) {
    elements.set(id, { hidden: true, disabled: false, textContent: '', value: '', attributes: {}, handlers: {},
      setAttribute(key, value) { this.attributes[key] = value; },
      addEventListener(event, callback) { this.handlers[event] = callback; } });
  }
  const calls = [];
  let reloads = 0;
  let elapsed = 0;
  let deadline;
  const context = {
    document: { querySelector: () => ({ content: 'loaded-build' }), getElementById: id => elements.get(id) },
    window: { location: { reload() { reloads += 1; } } }, URL, AbortController,
    fetch: async (url, options = {}) => {
      calls.push({ url, options });
      return reply(url, options);
    },
    setTimeout(callback, delay) {
      if (delay === 30000) { deadline = callback; return 1; }
      elapsed += delay;
      if (elapsed >= 30000) deadline?.();
      queueMicrotask(callback);
      return 2;
    },
    clearTimeout() { deadline = undefined; },
  };
  vm.runInNewContext(source, context);
  return { controller: context.window.ChorusUpdate, elements, calls,
    get reloads() { return reloads; },
    expire() { deadline(); },
    click(id) { return elements.get(id).handlers.click(); } };
}

test('approval installs once, waits through a disconnect and old version, then offers manual Reload without changing a draft', async () => {
  let health = 0;
  const h = harness((url, options) => {
    if (url === '/update' && !options.method) return response(200, available);
    if (url === '/update') return response(202, { status: 'restarting', version: '0.10.0' });
    if (++health === 1) throw new Error('connection refused');
    if (health === 2) {
      h.controller.checkUIBuild(response(304, null, { 'X-Chorus-UI-Build': 'new-build' }));
      assert.equal(h.elements.get('btn-reload-ui').hidden, true);
    }
    return response(200, null, { 'X-Chorus-Version': health < 3 ? '0.9.0' : '0.10.0' });
  });
  h.elements.get('send-text').value = 'Keep this unsent instruction';
  await h.controller.start();
  assert.match(h.elements.get('update-message').textContent, /Chorus v0.10.0 is available/);
  assert.equal(h.elements.get('update-release').href, available.release_url);
  assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 0);
  await h.click('btn-update-chorus');
  assert.equal(health, 3);
  assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 1);
  const approved = h.calls.find(call => call.options.method === 'POST').options;
  assert.equal(approved.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(approved.body), { version: '0.10.0' });
  assert.equal(h.elements.get('btn-reload-ui').hidden, false);
  assert.match(h.elements.get('update-message').textContent, /v0.10.0 is running/);
  assert.equal(h.elements.get('send-text').value, 'Keep this unsent instruction');
  assert.equal(h.reloads, 0);
  h.click('btn-reload-ui');
  assert.equal(h.reloads, 1);
});

test('the approved version stays bound to the notice when another release appears before the click', async () => {
  let latest = '0.10.0';
  let target;
  const h = harness((_url, options) => {
    if (!options.method) return response(200, { ...available, latest_version: latest });
    target = JSON.parse(options.body).version;
    return response(200, { status: 'up_to_date', version: target });
  });
  await h.controller.start();
  latest = '0.11.0';
  await h.click('btn-update-chorus');
  assert.equal(target, '0.10.0');
});

test('a changed dashboard keeps its existing Reload notice instead of offering installation', async () => {
  const h = harness(() => response(200, available));
  await h.controller.start();
  h.controller.checkUIBuild(response(304, null, { 'X-Chorus-UI-Build': 'new-build' }));
  assert.equal(h.elements.get('update-message').textContent, 'Dashboard updated');
  assert.equal(h.elements.get('btn-update-chorus').hidden, true);
  assert.equal(h.elements.get('btn-reload-ui').hidden, false);
});

for (const status of [404, 502]) {
  test(`availability HTTP ${status} leaves coordination and dashboard reload usable`, async () => {
    const h = harness(() => response(status, { error: 'unavailable' }));
    await h.controller.start();
    assert.equal(h.elements.get('update-banner').hidden, true);
    h.controller.checkUIBuild(response(304, null, { 'X-Chorus-UI-Build': 'new-build' }));
    assert.equal(h.elements.get('btn-reload-ui').hidden, false);
  });
}

test('offline availability checks and up-to-date or unsafe release notices stay quiet', async () => {
  for (const result of [null, { ...available, update_available: false }, { ...available, release_url: 'javascript:alert(1)' }]) {
    const h = harness(() => {
      if (!result) throw new Error('offline');
      return response(200, result);
    });
    await h.controller.start();
    assert.equal(h.elements.get('update-banner').hidden, true);
  }
});

for (const status of [403, 409, 500, 502]) {
  test(`installation HTTP ${status} reports the owner error and requires a fresh click to retry`, async () => {
    const h = harness((_url, options) => options.method
      ? response(status, { error: 'installation is unavailable' }) : response(200, available));
    await h.controller.start();
    await h.click('btn-update-chorus');
    assert.match(h.elements.get('update-message').textContent, /installation is unavailable/);
    assert.equal(h.elements.get('btn-update-chorus').textContent, 'Try again');
    assert.equal(h.elements.get('btn-update-chorus').disabled, false);
    assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 1);
    assert.equal(h.reloads, 0);
  });
}

test('the existing request guard can explain a rejection in plain text', async () => {
  const h = harness((_url, options) => options.method
    ? response(403, 'forbidden: request must be addressed to this machine', { 'Content-Type': 'text/plain' })
    : response(200, available));
  await h.controller.start();
  await h.click('btn-update-chorus');
  assert.match(h.elements.get('update-message').textContent, /request must be addressed to this machine/);
  assert.equal(h.elements.get('btn-update-chorus').disabled, false);
});

test('readiness timeout offers a read-only check and never repeats the accepted installation', async () => {
  let healthy = false;
  const h = harness((url, options) => {
    if (url === '/update' && !options.method) return response(200, available);
    if (url === '/update') return response(202, { status: 'restarting', version: '0.10.0' });
    return response(200, null, { 'X-Chorus-Version': healthy ? '0.10.0' : '0.9.0' });
  });
  await h.controller.start();
  await h.click('btn-update-chorus');
  assert.match(h.elements.get('update-message').textContent, /has not become healthy yet/);
  assert.equal(h.elements.get('btn-update-chorus').textContent, 'Check again');
  healthy = true;
  await h.click('btn-update-chorus');
  assert.equal(h.elements.get('btn-reload-ui').hidden, false);
  assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 1);
});

test('an already-current response offers Reload without waiting for a restart', async () => {
  const h = harness((_url, options) => options.method
    ? response(200, { status: 'up_to_date', version: '0.10.0' }) : response(200, available));
  await h.controller.start();
  await h.click('btn-update-chorus');
  assert.equal(h.elements.get('btn-reload-ui').hidden, false);
  assert.equal(h.calls.some(call => call.url === '/healthz'), false);
});

test('the readiness deadline cancels a health request that never answers', async () => {
  const h = harness((url, options) => {
    if (url === '/update' && !options.method) return response(200, available);
    if (url === '/update') return response(202, { status: 'restarting', version: '0.10.0' });
    return new Promise((_resolve, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))));
  });
  await h.controller.start();
  const pending = h.click('btn-update-chorus');
  await new Promise(resolve => setImmediate(resolve));
  h.expire();
  await pending;
  assert.equal(h.elements.get('btn-update-chorus').textContent, 'Check again');
  assert.equal(h.elements.get('btn-reload-ui').hidden, true);
});

test('duplicate clicks during installation do not send a second POST', async () => {
  let finish;
  const h = harness((_url, options) => options.method
    ? new Promise(resolve => { finish = resolve; }) : response(200, available));
  await h.controller.start();
  const pending = h.click('btn-update-chorus');
  assert.equal(h.elements.get('btn-update-chorus').disabled, true);
  await h.click('btn-update-chorus');
  assert.equal(h.calls.filter(call => call.options.method === 'POST').length, 1);
  finish(response(200, { status: 'up_to_date', version: '0.10.0' }));
  await pending;
});
