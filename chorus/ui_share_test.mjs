// Verify ChorusShare: PR Markdown summary formatting (pipe/newline/backtick
// escaping, User provenance, selected channel pin), accessible clipboard and
// export feedback (external role="status", VoiceOver repeat \u00A0, timer
// handoff across buttons, never disabling trigger), and standalone SVG 1.1
// GitHub-CSP-safe timeline serialization.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const shareSource = await readFile(new URL("./ui/share.js", import.meta.url), "utf8");

function makeNode(tag, attrs = {}, children = []) {
  const node = {
    tagName: tag,
    attributes: { ...attrs },
    children: [...children],
    textContent: "",
    ownerDocument: null,
    classList: {
      set: new Set(),
      add(...names) { names.forEach((n) => this.set.add(n)); },
      remove(...names) { names.forEach((n) => this.set.delete(n)); },
      toggle(name, force) {
        const on = force === undefined ? !this.set.has(name) : Boolean(force);
        if (on) this.set.add(name);
        else this.set.delete(name);
      },
      contains(name) { return this.set.has(name); },
    },
    get firstChild() { return this.children[0] || null; },
    getAttribute(k) { return Object.hasOwn(this.attributes, k) ? this.attributes[k] : null; },
    getAttributeNames() { return Object.keys(this.attributes); },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    removeAttribute(k) { delete this.attributes[k]; },
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    insertBefore(child, ref) {
      child.parentNode = this;
      const idx = this.children.indexOf(ref);
      if (idx < 0) this.children.push(child);
      else this.children.splice(idx, 0, child);
      return child;
    },
    replaceWith(replacement) {
      if (!this.parentNode) return;
      const idx = this.parentNode.children.indexOf(this);
      if (idx >= 0) {
        replacement.parentNode = this.parentNode;
        this.parentNode.children[idx] = replacement;
      }
    },
    remove() {
      if (!this.parentNode) return;
      const idx = this.parentNode.children.indexOf(this);
      if (idx >= 0) this.parentNode.children.splice(idx, 1);
    },
    cloneNode(deep) {
      const copy = makeNode(this.tagName, this.attributes, deep ? this.children.map((c) => c.cloneNode(true)) : []);
      copy.textContent = this.textContent;
      copy.ownerDocument = this.ownerDocument;
      for (const c of copy.children) c.parentNode = copy;
      return copy;
    },
    querySelectorAll(selector) {
      const out = [];
      const walk = (el) => {
        for (const ch of el.children) {
          if (selector === "*" || ch.tagName === selector) out.push(ch);
          walk(ch);
        }
      };
      walk(this);
      return out;
    },
  };
  for (const c of node.children) c.parentNode = node;
  return node;
}

function serializeXML(node) {
  const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  const attrs = Object.entries(node.attributes).map(([k, v]) => ` ${k}="${esc(v)}"`).join("");
  if (node.children.length === 0 && !node.textContent) {
    return `<${node.tagName}${attrs}/>`;
  }
  const inner = node.textContent ? esc(node.textContent) : node.children.map(serializeXML).join("");
  return `<${node.tagName}${attrs}>${inner}</${node.tagName}>`;
}

function bootShare({ state = {}, theme = "light", clipboardWrite } = {}) {
  const copyBtn = makeNode("button", { id: "btn-copy-pr" });
  const svgBtn = makeNode("button", { id: "btn-export-svg" });
  const statusEl = makeNode("span", { id: "share-status", role: "status" });
  const body = makeNode("body");
  const createdLinks = [];
  let svgRoot = null;
  const listeners = new Map();
  let nextTimerId = 1;
  const activeTimers = new Map();

  for (const btn of [copyBtn, svgBtn]) {
    btn.addEventListener = (ev, fn) => listeners.set(`${btn.attributes.id}:${ev}`, fn);
    btn.disabled = false;
  }

  const doc = {
    documentElement: { dataset: { theme } },
    body,
    createElement: (tag) => {
      const el = makeNode(tag);
      el.click = () => { el.clicked = true; };
      if (tag === "a") createdLinks.push(el);
      return el;
    },
    createElementNS: (_ns, tag) => {
      const el = makeNode(tag);
      el.ownerDocument = doc;
      return el;
    },
    getElementById: (id) => ({ "btn-copy-pr": copyBtn, "btn-export-svg": svgBtn, "share-status": statusEl }[id] || null),
    querySelector: (sel) => (sel === "#timeline-root svg" ? svgRoot : null),
  };

  const ctx = vm.createContext({
    window: { ChorusUI: { state }, matchMedia: () => ({ matches: theme === "dark" }) },
    document: doc,
    navigator: clipboardWrite ? { clipboard: { writeText: clipboardWrite } } : {},
    XMLSerializer: class { serializeToString(n) { return serializeXML(n); } },
    encodeURIComponent,
    setTimeout: (fn) => {
      const id = nextTimerId++;
      activeTimers.set(id, fn);
      return id;
    },
    clearTimeout: (id) => {
      activeTimers.delete(id);
    },
  });
  ctx.window.document = doc;
  ctx.window.navigator = ctx.navigator;
  vm.runInContext(shareSource, ctx);
  ctx.window.ChorusShare.start();
  return {
    ChorusShare: ctx.window.ChorusShare,
    copyBtn,
    svgBtn,
    statusEl,
    listeners,
    createdLinks,
    activeTimers,
    setSVG: (el) => { el.ownerDocument = doc; svgRoot = el; },
    flushAllTimers: () => {
      for (const [id, fn] of Array.from(activeTimers.entries())) {
        activeTimers.delete(id);
        fn();
      }
    },
  };
}

test("formatPRSummary formats pinned goal, models, multi-backtick spans, human User label, and escapes pipes", () => {
  const { ChorusShare } = bootShare();
  const md = ChorusShare.formatPRSummary({
    activeSwarm: "auth",
    selectedTarget: { kind: "channel", name: "#auth" },
    view: {
      swarm: "auth",
      agents: [
        { handle: "codex", model: "GPT-6.1-Sol", role: "Architect | API contract" },
        { handle: "claude", model: "Claude ``Opus`` 5.5", role: "Store\nhandler" },
        { handle: "raphael", model: "human", role: "Product Lead" },
      ],
      channels: [
        {
          name: "#auth",
          pin: { seq: 1, sender: "codex", text: "Single-use passkey challenges" },
          messages: [{ seq: 1 }, { seq: 2 }],
        },
      ],
      inboxes: [{ recipient: "claude", messages: [{ seq: 1 }] }],
    },
  });

  assert.match(md, /### Multi-Agent Collaboration \(`#auth` · swarm `auth`\)/);
  assert.match(md, /\*\*Pinned Goal \(`#auth#1` by `@codex`\):\*\*\n> Single-use passkey challenges/);
  assert.match(md, /\| `@claude` \| ``` Claude ``Opus`` 5\.5 ``` \| Store handler \|/);
  assert.match(md, /\| `@codex` \| `GPT-6\.1-Sol` \| Architect \\\| API contract \|/);
  assert.match(md, /\| `@raphael` \| User \| Product Lead \|/);
  assert.match(md, /2 channel messages · 1 direct message — coordinated in real time with \[Chorus\]/);
});

test("copyPRSummary and exportTimelineSVG handle errors, hand off shared timer cleanly, and never disable buttons", async () => {
  const emptyEnv = bootShare({ state: {} });
  assert.equal(await emptyEnv.ChorusShare.copyPRSummary(emptyEnv.copyBtn, emptyEnv.statusEl), false);
  assert.equal(emptyEnv.copyBtn.getAttribute("data-failed"), "");
  assert.equal(emptyEnv.statusEl.classList.contains("is-error"), true);
  assert.equal(emptyEnv.statusEl.textContent, "No active swarm yet");
  assert.equal(emptyEnv.ChorusShare.exportTimelineSVG(emptyEnv.svgBtn, emptyEnv.statusEl), false);
  assert.equal(emptyEnv.statusEl.textContent, "No timeline to export");

  const copied = [];
  const state = {
    activeSwarm: "auth",
    view: {
      swarm: "auth",
      agents: [{ handle: "claude", model: "Claude Opus 5.5", role: "Backend" }],
      channels: [{ name: "#auth", messages: [{ seq: 1 }] }],
      inboxes: [],
    },
  };
  const failClip = bootShare({
    state,
    clipboardWrite: async () => { throw new Error("denied"); },
  });
  assert.equal(await failClip.ChorusShare.copyPRSummary(failClip.copyBtn, failClip.statusEl), false);
  assert.equal(failClip.statusEl.textContent, "Copy failed");

  const env = bootShare({
    state,
    clipboardWrite: async (text) => { copied.push(text); },
  });
  env.setSVG(makeNode("svg", { viewBox: "0 0 960 180" }, []));

  // Trigger via registered click handlers and verify Copy -> Export handoff cancels the earlier timer.
  await env.ChorusShare.copyPRSummary(env.copyBtn, env.statusEl);
  assert.equal(env.copyBtn.disabled, false);
  assert.equal(env.copyBtn.getAttribute("data-copied"), "");
  assert.equal(env.statusEl.textContent, "Copied PR summary");

  await env.ChorusShare.copyPRSummary(env.copyBtn, env.statusEl);
  assert.equal(env.statusEl.textContent, "Copied PR summary\u00A0");
  assert.equal(copied.length, 2);

  env.listeners.get("btn-export-svg:click")();
  assert.equal(env.activeTimers.size, 1, "shared status timer must cancel copy timer on export");
  assert.equal(env.copyBtn.getAttribute("data-copied"), null, "previous button highlight clears on handoff");
  assert.equal(env.svgBtn.getAttribute("data-copied"), "");
  assert.equal(env.statusEl.textContent, "Exported SVG");
  assert.equal(env.createdLinks.length, 1);
  assert.equal(env.createdLinks[0].clicked, true);
  assert.equal(env.createdLinks[0].getAttribute("download"), "chorus-auth-timeline.svg");

  env.flushAllTimers();
  assert.equal(env.svgBtn.getAttribute("data-copied"), null);
  assert.equal(env.statusEl.textContent, "");
});

test("serializeTimelineSVG strips root and child interactive/data attributes, avoids rgba(), and renders all provider glyphs", () => {
  const env = bootShare({
    state: { activeSwarm: "auth" },
    theme: "dark",
  });
  const winRect = makeNode("rect", { class: "tl-window", x: "120", y: "12", width: "200", height: "16" });
  const lane = makeNode("line", { class: "tl-lane", x1: "120", x2: "900", y1: "90", y2: "90" });
  const imgGemini = makeNode("image", { class: "tl-avatar-icon provider-gemini", href: "/ui/brands/gemini.png", x: "16", y: "80", width: "20", height: "20" });
  const imgClaude = makeNode("image", { class: "tl-avatar-icon provider-claude", href: "/ui/brands/claude.svg", x: "16", y: "110", width: "20", height: "20" });
  const imgOpenAI = makeNode("image", { class: "tl-avatar-icon provider-openai", href: "/ui/brands/openai.svg", x: "16", y: "140", width: "20", height: "20" });
  const dot = makeNode("circle", {
    class: "tl-dot is-selected",
    cx: "240",
    cy: "90",
    r: "5",
    fill: "#2dd4bf",
    "data-tl-hover": "secret hover preview",
    "data-short": "@claude",
  });
  const svg = makeNode(
    "svg",
    {
      class: "tl-svg",
      viewBox: "0 0 960 180",
      tabindex: "0",
      "aria-label": "Swarm timeline, press Left and Right",
      "data-tl-root": "1",
    },
    [winRect, lane, imgGemini, imgClaude, imgOpenAI, dot]
  );
  env.setSVG(svg);

  const xml = env.ChorusShare.serializeTimelineSVG(svg, { activeSwarm: "auth" });
  assert.match(xml, /role="img"/);
  assert.match(xml, /<title>Chorus swimlane collaboration timeline for swarm auth<\/title>/);
  assert.doesNotMatch(xml, /<image/);
  assert.doesNotMatch(xml, /class=/);
  assert.doesNotMatch(xml, /tabindex=/);
  assert.doesNotMatch(xml, /aria-label=/);
  assert.doesNotMatch(xml, /data-/);
  assert.doesNotMatch(xml, /rgba\(/);
  assert.match(xml, /fill-opacity="0\.2"/);
  assert.match(xml, /fill="#101827"/);
  assert.match(xml, /Chorus · swarm auth · github\.com\/goadesign\/chorus/);
});
