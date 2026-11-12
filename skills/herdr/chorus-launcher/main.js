// Choose one team row per pane; Herdr's terminal labels own existing sessions.
import { View, div } from "gpui-kit";
import { Button, Checkbox, Input, InputState, Popup, Select, VirtualListScrollHandle, h_flex, v_flex, v_virtual_list, Scrollbar } from "gpui-base";
import { appHome, closeOverlay, exec, focused, knownAgents, notify, props, setShared, shared, spaces, theme } from "herdr";

// The default channel and swarm must both accept the chosen name.
const SWARM = /^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$/;

// Per-checkout launch preferences live in Herdr shared state; live terminals remain truth.
const REPO_PREFIX = "chorus-launcher.repo:";

// Only clients with verified per-process additive MCP flags may take role connections.
const CONNECTION_CLIENTS = new Set(["claude", "codex"]);

// Antigravity encodes effort suffixes directly into variant model IDs.
const AGY_SUFFIX = /-(low|medium|high|xhigh|max)$/;

// Requested everyday defaults; model lists and account access still belong to clients.
const MODEL_DEFAULTS = {
  claude: { id: "claude-opus-5-5", name: "Claude Opus 5.5", thinking: null },
  codex: { id: "gpt-6.1-sol", name: "GPT-6.1 Sol", thinking: null },
  cursor: { id: "claude-opus-5-5-high", name: "Claude Opus 5.5 High", thinking: null },
  antigravity: { id: "gemini-4-argon", name: "Gemini 4 Argon", thinking: "high" },
};

// Display labels for client-advertised reasoning effort values.
const THINKING_NAMES = {
  none: "None",
  minimal: "Minimal",
  low: "Low",
  medium: "Medium",
  high: "High",
  xhigh: "Extra high",
  "extra-high": "Extra high",
  max: "Max",
  ultra: "Ultra (delegates tasks)",
};

// Describe the actual client effect rather than promising identical restrictions.
const YOLO_EFFECT = {
  claude: "Bypasses tool permission checks.",
  codex: "Disables approval prompts and sandboxing.",
  antigravity: "Auto-approves tool permission requests.",
  cursor: "Allows commands unless explicitly denied; keeps the sandbox setting.",
  gemini: "Auto-approves tool actions; keeps the sandbox setting.",
};

export default class ChorusLauncher extends View {
  // Capture the project before discovery or selecting another tab can change focus.
  init(_props, cx) {
    const target = focused();
    this.space = spaces().find(s => s.id === target?.space);
    this.folder = target?.cwd;
    this.agents = [];
    this.rows = [];
    this.ordinal = 0;
    this.root = "";
    this.rosterMode = false;
    this.error = "";
    this.busy = false;
    this.loading = true;
    this.update = null;
    this.swarm = "";
    this.picker = null;
    this.input = InputState.new({ placeholder: "Chorus name" });
    this.input.on("change", (_event, cx) => {
      const next = this.input.value();
      if (next === this.swarm) return;
      this.swarm = next;
      this.saveRepo();
      cx.notify();
    });
    this.input.on("submit", (_event, cx) => this.start(cx));
    this.handle = cx.focus_handle();
    this.handle.focus();
    this.buttons = Object.fromEntries(["add", "all", "yolo-all", "reload", "close", "start", "update"].map(key => [key, cx.focus_handle()]));
    cx.spawn(async cx => {
      try {
        if (!this.space || !this.folder) throw new Error("Focus a terminal in your project, then open Start Chorus again.");
        if (this.space.machine) throw new Error("This launcher supports local projects. Select a local terminal and try again.");
        const [initialTeam, agents, chorus] = await Promise.all([
          this.helper("inspect", { space: this.space.id, root: "" }),
          knownAgents(),
          exec("chorus", ["status"], { timeout: 10000 }),
        ]);
        if (chorus.code !== 0 || !chorus.stdout.includes(" is running")) throw new Error("Chorus is not running. Start it with chorus up, then open this launcher again.");
        this.agents = agents.filter(a => a.installed);
        if (initialTeam?.roster) {
          this.rosterMode = true;
          this.root = initialTeam.root;
          this.checkUpdate(cx);
          this.applyTeam(initialTeam, cx);
        } else {
          const repo = await exec("git", ["-C", this.folder, "rev-parse", "--show-toplevel"], { timeout: 10000 });
          if (repo.code !== 0) throw new Error("The focused terminal is outside a Git checkout. Open your project folder and try again.");
          this.root = repo.stdout.replace(/\r?\n$/, "");
          this.checkUpdate(cx);
          if (Array.isArray(initialTeam?.rows)) {
            this.applyTeam(initialTeam, cx);
          } else {
            await this.loadTeam(cx);
          }
          if (!this.rows.length) this.restoreOrAddAll(cx);
        }
        if (!this.agents.length && !this.rows.some(row => row.terminal)) throw new Error("Herdr found no installed agent CLIs. Install an agent from Herdr's New Agent menu, then reopen Start Chorus.");
      } catch (error) { this.error = String(error.message ?? error); }
      this.loading = false;
      cx.notify();
      // Each client supplies its own catalog; a failed lookup leaves an explicit notice.
      await Promise.all(this.agents.map(agent => this.loadModels(agent, cx)));
    });
  }

  // Checking releases runs independently of team/model discovery and never
  // prevents opening a team when GitHub or an older CLI cannot answer.
  checkUpdate(cx) {
    cx.spawn(async cx => {
      try {
        const result = await exec("chorus", ["update", "-check"], { cwd: this.root, timeout: 20000 });
        if (result.code !== 0) throw new Error(result.stderr.trim() || result.stdout.trim() || "The update check failed.");
        const message = result.stdout.trim();
        const target = message.match(/^Update available: chorus v\S+ -> v(\d+\.\d+\.\d+)(?:\s|$)/)
          ?? message.match(/^Restart needed: chorus binary is v(\d+\.\d+\.\d+), running server is v\S+/);
        this.update = target ? { status: "available", message, version: target[1] } : null;
        cx.notify();
      } catch (error) {
        if (this.update?.status === "checking") {
          this.update = { status: "error", message: `Could not check Chorus updates: ${error.message ?? error}. Check again when ready.` };
          cx.notify();
        }
        // Availability is optional; existing panes and launch choices remain usable.
      }
    });
  }

  // A failed update can have a stale target. Refresh its notice before the
  // next approval instead of silently switching versions on a retry.
  refreshUpdate(cx) {
    this.update = { status: "checking", message: "Checking available Chorus updates…" };
    cx.notify();
    this.checkUpdate(cx);
  }

  // Clicking this control approves the CLI's install/setup/restart operation.
  // Herdr still owns running conversations; updating never recreates a pane.
  updateChorus(cx) {
    if (this.update?.status !== "available") return;
    const version = this.update.version;
    this.update = { status: "installing", message: "Updating Chorus and its integrations…" };
    cx.notify();
    cx.spawn(async cx => {
      try {
        const result = await exec("chorus", ["update", "-yes", "-version", version], { cwd: this.root, timeout: 660000 });
        if (result.code !== 0) throw new Error(result.stderr.trim() || result.stdout.trim() || "The update command failed.");
        this.update = { status: "ready", message: "Chorus is ready. Existing panes remain available." };
      } catch (error) {
        this.update = { status: "error", message: `Could not update Chorus to v${version}: ${error.message ?? error}` };
      }
      cx.notify();
    });
  }

  // Keep the helper's output decoding and command failures at the process boundary.
  async helper(action, request) {
    const reply = await exec("python3", [`${appHome()}/mods/chorus-launcher/launch.py`, appHome(), action, JSON.stringify(request)], { cwd: this.root || this.folder, timeout: 120000 });
    if (reply.code !== 0) throw new Error(reply.stderr.trim() || reply.stdout.trim() || "The launcher could not finish. Reload the team and try again.");
    return JSON.parse(reply.stdout);
  }

  // Apply inspected team rows, keeping pending edits and caller roster order.
  applyTeam(team, cx) {
    this.session ??= team.session;
    if (team.roster) {
      this.rosterMode = true;
      this.root = team.root || this.root;
      const previous = new Map(this.rows.map(row => [row.id, row]));
      const savedPrefs = shared(`${REPO_PREFIX}${this.root}`);
      const savedRoles = savedPrefs?.roles && typeof savedPrefs.roles === "object" ? savedPrefs.roles : {};
      this.rows = team.rows.map(item => {
        if (item.terminal) return this.newRow(item.key, cx, item);
        const prior = previous.get(item.id);
        if (prior && !prior.terminal) {
          prior.role = item.role;
          prior.title = item.title;
          prior.handle = item.handle;
          prior.hasConnections = Boolean(item.has_connections);
          return prior;
        }
        const persisted = savedRoles[item.handle] ?? null;
        const defaultKey = persisted && typeof persisted.key === "string" && this.agents.some(agent => agent.key === persisted.key)
          ? persisted.key
          : item.has_connections
            ? (this.agents.find(agent => CONNECTION_CLIENTS.has(agent.key))?.key ?? this.agents[0]?.key ?? "claude")
            : (this.agents[0]?.key ?? "claude");
        return this.newRow(defaultKey, cx, item, persisted);
      });
      this.teamExists = true;
      this.swarm = team.swarm;
      this.input.set_value(this.swarm);
      cx.notify();
      return;
    }
    const pending = this.rows.filter(row => !row.terminal && !team.rows.some(saved => saved.id === row.id));
    this.rows = [...team.rows.map(saved => this.newRow(saved.key, cx, saved)), ...pending];
    this.teamExists = Boolean(team.tab);
    this.swarm = team.swarm ?? (this.swarm || this.root.split("/").pop());
    this.input.set_value(this.swarm);
    cx.notify();
  }

  // Use the current terminals as truth, keeping only rows that have not opened yet.
  async loadTeam(cx) {
    const team = await this.helper("inspect", { space: this.space.id, root: this.root });
    this.applyTeam(team, cx);
  }

  // Restore saved project preferences when no live team exists, falling back to installed defaults.
  restoreOrAddAll(cx) {
    const saved = shared(`${REPO_PREFIX}${this.root}`);
    if (saved?.swarm && SWARM.test(saved.swarm)) {
      this.swarm = saved.swarm;
      this.input.set_value(this.swarm);
    }
    const valid = Array.isArray(saved?.rows)
      ? saved.rows.filter(item => item && typeof item.key === "string" && this.agents.some(agent => agent.key === item.key))
      : [];
    if (valid.length) {
      for (const item of valid) this.rows.push(this.newRow(item.key, cx, null, item));
      cx.notify();
      return;
    }
    for (const agent of this.agents) this.rows.push(this.newRow(agent.key, cx));
    cx.notify();
  }

  // Persist only explicit user choices so failed or in-flight discovery never overwrites defaults.
  saveRepo() {
    if (!this.root) return;
    if (this.rosterMode) {
      const existing = shared(`${REPO_PREFIX}${this.root}`) ?? {};
      const roles = { ...(existing.roles ?? {}) };
      for (const row of this.rows) {
        if (!row.handle || (row.terminal && !row.recorded)) continue;
        const entry = { key: row.key, yolo: Boolean(row.yolo) };
        if (row.terminal) {
          entry.model = row.model;
          if (row.key !== "cursor") entry.thinking = row.thinking;
        } else {
          if (row.chosenModel !== undefined && row.chosenModel !== "") entry.model = row.chosenModel;
          if (row.key !== "cursor" && row.chosenThinking !== undefined) entry.thinking = row.chosenThinking;
        }
        roles[row.handle] = entry;
      }
      setShared(`${REPO_PREFIX}${this.root}`, { ...existing, swarm: this.swarm, roles });
      return;
    }
    const savable = this.rows.filter(row => !row.terminal || row.recorded);
    if (this.rows.length > 0 && savable.length === 0) return;
    setShared(`${REPO_PREFIX}${this.root}`, {
      swarm: this.swarm,
      rows: savable.map(row => {
        const entry = { key: row.key, yolo: Boolean(row.yolo) };
        if (row.terminal) {
          entry.model = row.model;
          if (row.key !== "cursor") entry.thinking = row.thinking;
        } else {
          if (row.chosenModel !== undefined && row.chosenModel !== "") entry.model = row.chosenModel;
          if (row.key !== "cursor" && row.chosenThinking !== undefined) entry.thinking = row.chosenThinking;
        }
        return entry;
      }),
    });
  }

  // Retain input and focus state for one row; undefined chosen values inherit verified defaults.
  newRow(key, cx, saved = null, persisted = null) {
    const self = this;
    const hasSaved = Boolean(saved?.terminal);
    const persistedModel = persisted && "model" in persisted && persisted.model !== "" ? (persisted.model ?? null) : undefined;
    const row = {
      id: saved?.id ?? `${this.session}-${++this.ordinal}`,
      role: saved?.role ?? null,
      title: saved?.title ?? null,
      handle: saved?.handle ?? null,
      hasConnections: Boolean(saved?.has_connections),
      key,
      terminal: saved?.terminal ?? null,
      chosenModel: hasSaved ? (saved.model || null) : persistedModel,
      chosenThinking: key === "cursor" ? undefined : (hasSaved ? (saved.thinking ?? null) : (persisted && "thinking" in persisted ? (persisted.thinking ?? null) : undefined)),
      yolo: hasSaved ? (saved.yolo ?? false) : persisted ? Boolean(persisted.yolo) : false,
      recorded: hasSaved ? saved.yolo !== null : true,
      custom: false,
      input: InputState.new({ placeholder: "Exact model ID" }),
      remove: cx.focus_handle(),
      retry: cx.focus_handle(),
      get model() {
        if (this.custom) return this.chosenModel ?? "";
        if (this.terminal || this.chosenModel !== undefined) return this.chosenModel || null;
        const details = self.agents.find(agent => agent.key === this.key)?.details;
        return details?.model_supported ? (MODEL_DEFAULTS[this.key]?.id ?? null) : null;
      },
      set model(value) {
        this.chosenModel = value;
      },
      get thinking() {
        if (this.key === "cursor") return null;
        if (this.terminal || this.chosenThinking !== undefined) return this.chosenThinking ?? null;
        const details = self.agents.find(agent => agent.key === this.key)?.details;
        const entry = !this.custom ? self.modelEntry(this, details) : null;
        const preset = MODEL_DEFAULTS[this.key]?.thinking;
        return preset && details?.thinking_supported && entry?.efforts?.includes(preset) ? preset : null;
      },
      set thinking(value) {
        this.chosenThinking = this.key === "cursor" ? undefined : value;
      },
    };
    for (const field of ["agent", "model", "thinking"]) {
      row[`${field}Picker`] = { trigger: cx.focus_handle(), list: cx.focus_handle(), scroll: VirtualListScrollHandle.new(), index: 0 };
    }
    row.input.on("change", (_event, cx) => {
      if (!row.custom) return;
      row.chosenModel = row.input.value().trim();
      const details = this.agents.find(agent => agent.key === row.key)?.details;
      this.syncThinking(row, details);
      this.saveRepo();
      cx.notify();
    });
    return row;
  }

  // Clear an explicit thinking choice when the selected model no longer supports it.
  syncThinking(row, details) {
    if (!row.chosenThinking) return;
    const levels = this.thinkingLevels(row, details);
    if ((row.key === "antigravity" && row.model && AGY_SUFFIX.test(row.model)) || (levels.length > 0 && !levels.includes(row.chosenThinking))) {
      row.chosenThinking = null;
    }
  }

  // Match a row's selected model against catalog entries, including Cursor variant families.
  modelEntry(row, details) {
    if (!row.model || !details?.models) return null;
    return details.models.find(item => item.id === row.model || Object.values(item.variants ?? {}).includes(row.model)) ?? null;
  }

  // Return advertised thinking levels for the row's current client and model.
  thinkingLevels(row, details) {
    const entry = !row.custom ? this.modelEntry(row, details) : null;
    if (row.key === "cursor") return entry?.efforts ?? [];
    if (row.key === "antigravity" && row.model && AGY_SUFFIX.test(row.model)) return [];
    return entry?.efforts?.length ? entry.efforts : details?.thinking_supported ? (details.efforts ?? []) : [];
  }

  // Compute valid thinking choices for the row's current client and model.
  thinkingConfig(row, details) {
    const levels = this.thinkingLevels(row, details);
    if (row.key === "cursor") {
      const entry = !row.custom ? this.modelEntry(row, details) : null;
      if (levels.length && entry?.variants) {
        const value = Object.entries(entry.variants).find(([, slug]) => slug === row.model)?.[0] ?? "";
        return { options: levels.map(id => ({ id, name: THINKING_NAMES[id] ?? id })), enabled: true, value };
      }
      const label = !details ? "Checking…" : row.model ? "Set by model" : "App default";
      return { options: [{ id: "", name: label }], enabled: false, value: "" };
    }
    if (row.key === "antigravity" && row.model && AGY_SUFFIX.test(row.model)) {
      return { options: [{ id: "", name: "Set by model" }], enabled: false, value: "" };
    }
    const options = [{ id: "", name: !details ? "Checking…" : levels.length ? "App default" : "Set by client" },
      ...levels.map(id => ({ id, name: THINKING_NAMES[id] ?? id }))];
    if (row.thinking && !options.some(option => option.id === row.thinking)) {
      options.push({ id: row.thinking, name: `${THINKING_NAMES[row.thinking] ?? row.thinking} (selected)` });
    }
    return { options, enabled: Boolean(details) && (levels.length > 0 || Boolean(row.thinking)), value: row.thinking ?? "" };
  }

  // Repeated apps are intentional: each row starts a distinct conversation.
  addAgent(cx) {
    if (this.rosterMode || !this.agents.length || this.busy || this.loading) return;
    this.rows.push(this.newRow(this.agents[0].key, cx));
    this.saveRepo();
    cx.notify();
  }

  // Preserve running and edited rows while filling missing installed apps once each.
  addAll(cx) {
    if (this.rosterMode) return;
    for (const agent of this.agents) {
      if (!this.rows.some(row => row.key === agent.key)) this.rows.push(this.newRow(agent.key, cx));
    }
    this.saveRepo();
    cx.notify();
  }

  // A catalog is local to this dialog, never a saved or hard-coded model list.
  async loadModels(agent, cx) {
    agent.loading = true;
    agent.details = null;
    cx.notify();
    try { agent.details = await this.helper("models", { key: agent.key, name: agent.name, run: agent.run }); }
    catch (error) { agent.details = { models: [], model_supported: false, thinking_supported: false, efforts: [], yolo_flag: null, error: true, note: `Could not check client options: ${error.message ?? error}. Retry to choose a model, thinking level, or YOLO mode.` }; }
    agent.loading = false;
    cx.notify();
  }

  // Start only new rows; existing rows are references rather than copied settings.
  start(cx) {
    if (!this.ready()) return;
    this.busy = true;
    this.error = "";
    this.picker = null;
    this.saveRepo();
    const request = { space: this.space.id, root: this.root, swarm: this.swarm,
      agents: this.rows.map(row => {
        if (row.terminal) return { id: row.id };
        const agent = this.agents.find(agent => agent.key === row.key);
        return { id: row.id, key: agent.key, name: agent.name, run: agent.run, model: row.model, thinking: row.thinking ?? null, yolo: row.yolo };
      }) };
    // Start the helper subprocess synchronously before spawning the continuation.
    const launch = this.helper("launch", request);
    cx.spawn(async cx => {
      let result;
      try {
        result = await launch;
      } catch (error) {
        this.error = String(error.message ?? error);
        // Even a partial launch records completed rows, so retrying never duplicates them.
        try { await this.loadTeam(cx); }
        catch (reloadError) { this.error += `${this.error ? "\n" : ""}Could not reload the team: ${reloadError.message ?? reloadError}`; }
        this.busy = false;
        cx.notify();
        return;
      }
      try {
        notify({
          title: "Chorus team ready",
          body: `Opened ${result.created} new pane${result.created === 1 ? "" : "s"}; reused ${result.reused}. Give your goal to one agent and check each pane for prompts.`,
          level: "info",
        });
      } catch {}
      closeOverlay(props()?.overlay);
    });
  }

  // Explain the first setting that prevents launching so the person knows what to change.
  blockingReason() {
    for (let index = 0; index < this.rows.length; index += 1) {
      const row = this.rows[index];
      if (row.terminal) continue;
      const label = row.title || `Agent ${index + 1}`;
      const agent = this.agents.find(item => item.key === row.key);
      const name = agent?.name ?? row.key;
      const details = agent?.details;
      if (row.hasConnections && !CONNECTION_CLIENTS.has(row.key)) {
        return `${label} (${name}): this agent app cannot yet be given a role's connections.`;
      }
      if (row.custom && !row.model) return `${label} (${name}): enter a model ID or choose another model.`;
      if (!details) return `Checking client options for ${name}…`;
      if ((row.chosenModel === undefined && !row.model && MODEL_DEFAULTS[row.key]?.id) || (row.model && !details.model_supported)) {
        return `${label} (${name}): retry client options or choose App default model.`;
      }
      const levels = this.thinkingLevels(row, details);
      const thinkingValid = !row.thinking || (row.key !== "cursor" && Boolean(details.thinking_supported) && levels.includes(row.thinking));
      if (!thinkingValid) {
        return `${label} (${name}): thinking level "${THINKING_NAMES[row.thinking] ?? row.thinking}" is unavailable for this model; choose another thinking level.`;
      }
      if (row.yolo && !details.yolo_flag) {
        return `${label} (${name}): YOLO mode could not be verified; retry client options or turn YOLO off.`;
      }
    }
    return "";
  }

  // Empty or incomplete custom model IDs and unverified options cannot silently launch.
  ready() {
    return !this.loading && !this.busy && Boolean(this.root) && SWARM.test(this.swarm) && this.rows.length > 0 && !this.blockingReason();
  }

  // Native buttons provide keyboard activation, disabled semantics and visible focus.
  button(id, label, handle, action, enabled = true, primary = false) {
    const t = theme();
    return Button.new(id).track_focus(handle).tab_stop(true).disabled(!enabled).px(12).py(8).rounded(6).border_1()
      .border_color(handle.is_focused() ? t.accent : primary ? t.solid : t.border)
      .bg(primary && enabled ? t.solid : t.popover).text_size(13)
      .text_color(enabled ? primary ? t.solid_foreground : t.foreground : t.dim)
      .when(enabled, el => el.cursor_pointer().hover(s => s.bg(primary ? t.solid : t.hover)))
      .on_click((_event, cx) => action(cx)).child(label);
  }

  // Native Select owns focus and dismissal; the virtual list adds arrow navigation.
  select(id, label, value, options, state, choose, enabled) {
    const t = theme();
    const open = this.picker === id;
    const accept = (index, cx) => {
      this.picker = null;
      state.trigger.focus();
      choose(options[index].id, cx);
      cx.notify();
    };
    const setOpen = (opened, cx) => {
      this.picker = opened ? id : null;
      if (opened) {
        state.index = Math.max(0, options.findIndex(option => option.id === value));
        state.scroll.scroll_to_item(state.index, "center");
        state.list.focus();
      }
      cx.notify();
    };
    const trigger = h_flex().id(`${id}-trigger`).track_focus(state.trigger).tab_stop(true).w_full().min_w(0).h(36).px(10).gap(8)
      .items_center().justify_between().rounded(6).border_1().border_color(state.trigger.is_focused() ? t.accent : t.input)
      .bg(t.background).text_color(enabled ? t.foreground : t.dim)
      .on_click((_event, cx) => { if (enabled) setOpen(!open, cx); })
      .child(div().min_w(0).truncate().child(options.find(o => o.id === value)?.name ?? value))
      .child(div().text_size(11).text_color(t.muted).child("Choose"));
    const list = v_flex().id(`${id}-content`).role("list_box").relative().track_focus(state.list).w(320).h(Math.min(options.length * 34, 238)).bg(t.popover).border_1().border_color(t.border).rounded(6)
      .on_key_down((event, cx) => {
        const moves = { up: -1, down: 1, home: -options.length, end: options.length };
        if (event.key in moves) {
          state.index = Math.max(0, Math.min(options.length - 1, state.index + moves[event.key]));
          state.scroll.scroll_to_item(state.index, "center");
          cx.stop_propagation();
          cx.notify();
        }
      })
      .child(div().size_full().child(v_virtual_list(`${id}-options`, options.length, 34, i => String(i), range => options.slice(range.start, range.end).map((option, offset) => {
        const index = range.start + offset;
        return div().role("list_box_option").aria_selected(option.id === value)
          .when(index === state.index, el => el.aria_active_descendant()).h(34).px(10).py(7).text_size(13)
          .text_color(t.foreground).bg(index === state.index ? t.selected : t.popover).child(option.name);
      })).size_full().track_scroll(state.scroll).on_item_click((key, cx) => accept(Number(key), cx))))
      .child(Scrollbar.vertical(`${id}-options`).absolute().inset_0());
    return Select.new(id).w_full().min_w(0).accessibility_label(label).disabled(!enabled).open(open)
      .track_focus(state.trigger).content_focus_handle(state.list)
      .on_open_change(setOpen)
      .on_dismiss((_event, cx) => cx.stop_propagation())
      .on_confirm((_event, cx) => accept(state.index, cx))
      .child(Popup.new(`${id}-popup`, trigger).anchor("bottom_left").when(open, el => el.content(list)));
  }

  // Show immutable launch settings for existing panes and editable controls for new ones.
  renderRow(row, index) {
    const t = theme();
    const label = row.title || `Agent ${index + 1}`;
    const agent = this.agents.find(agent => agent.key === row.key);
    const details = agent?.details;
    const editable = !row.terminal && !this.busy;
    const connectionBlocked = !row.terminal && row.hasConnections && !CONNECTION_CLIENTS.has(row.key);
    const entry = !row.custom ? this.modelEntry(row, details) : null;
    const selectedModelId = row.custom ? "__custom__" : entry ? entry.id : (row.model ?? "");
    const catalog = details?.models ?? [];
    const modelOptions = [
      { id: "", name: "App default" },
      ...catalog.map(item => ({
        id: item.id,
        name: !item.variants && catalog.some(other => other !== item && other.name === item.name) ? `${item.name} (${item.id})` : item.name,
      })),
    ];
    if (row.model && !row.custom && !entry && !modelOptions.some(option => option.id === row.model)) {
      modelOptions.push({ id: row.model, name: MODEL_DEFAULTS[row.key]?.id === row.model ? MODEL_DEFAULTS[row.key].name : `${row.model} (selected)` });
    }
    if (details?.model_supported || row.custom) modelOptions.push({ id: "__custom__", name: "Enter model ID…" });
    const thinking = this.thinkingConfig(row, details);
    const cursorLevel = entry?.variants
      ? Object.entries(entry.variants).find(([, slug]) => slug === row.model)?.[0]
      : null;
    const recordedThinking = row.key === "cursor"
      ? (THINKING_NAMES[cursorLevel] ?? (row.model ? "Set by model" : "App default at launch"))
      : (THINKING_NAMES[row.thinking] ?? row.thinking ?? "App default at launch");
    const name = agent?.name ?? row.key;
    return v_flex().id(row.id).flex_none().gap(8).py(12).border_b_1().border_color(t.border)
      .child(h_flex().items_end().gap(10)
        .child(v_flex().flex_1().min_w(0).gap(5).child(div().text_size(12).text_color(t.muted).child(label))
          .child(row.terminal ? div().h(36).py(8).child(name) : this.select(`${row.id}-agent`, `${label} client`, row.key,
            this.agents.map(a => ({ id: a.key, name: a.name })), row.agentPicker,
            key => {
              row.key = key;
              row.chosenModel = undefined;
              row.chosenThinking = undefined;
              row.custom = false;
              row.input.set_value("");
              if (!this.agents.find(a => a.key === key)?.details?.yolo_flag) row.yolo = false;
              this.saveRepo();
            }, editable)))
        .child(v_flex().flex_1().min_w(0).gap(5).child(div().text_size(12).text_color(t.muted).child("Model"))
          .child(row.terminal ? div().h(36).py(8).truncate().child(row.recorded ? row.model ?? "App default at launch" : "Not recorded")
            : this.select(`${row.id}-model`, `${label} model`, selectedModelId, modelOptions, row.modelPicker,
              model => {
                row.custom = model === "__custom__";
                const prevVariant = row.key === "cursor" && !row.custom ? Object.entries(entry?.variants ?? {}).find(([, slug]) => slug === row.model)?.[0] : null;
                row.chosenModel = row.custom ? row.input.value().trim() : model || null;
                const nextEntry = !row.custom ? this.modelEntry(row, details) : null;
                if (row.key === "cursor" && nextEntry?.variants && prevVariant && nextEntry.variants[prevVariant]) {
                  row.chosenModel = nextEntry.variants[prevVariant];
                } else {
                  this.syncThinking(row, details);
                }
                this.saveRepo();
                if (row.custom) row.input.focus();
              }, editable && Boolean(details))))
        .child(v_flex().w(170).min_w(0).gap(5).child(div().text_size(12).text_color(t.muted).child("Thinking"))
          .child(row.terminal ? div().h(36).py(8).truncate().child(row.recorded ? recordedThinking : "Not recorded")
            : this.select(`${row.id}-thinking`, `${label} thinking`, thinking.value, thinking.options, row.thinkingPicker,
              level => {
                if (row.key === "cursor") {
                  if (entry?.variants?.[level]) row.chosenModel = entry.variants[level];
                } else {
                  row.chosenThinking = level || null;
                }
                this.saveRepo();
              }, editable && thinking.enabled)))
        .when(!row.terminal && !this.rosterMode, el => el.child(this.button(`${row.id}-remove`, "Remove", row.remove, cx => {
          this.rows = this.rows.filter(item => item.id !== row.id);
          this.picker = null;
          this.saveRepo();
          cx.notify();
        }, editable))))
      .when(row.custom && !row.terminal, el => el.child(Input.new(row.input).accessibility_label(`${label} exact model ID`).w_full().h(36).px(10).rounded(6).border_1().border_color(t.input).bg(t.background)))
      .child(h_flex().items_center().gap(9)
        .when(!row.terminal, el => el.child(Checkbox.new(`${row.id}-yolo`).accessibility_label(`${label} YOLO mode`).checked(row.yolo)
          .disabled(!editable || (!details?.yolo_flag && !row.yolo)).on_change((checked, cx) => { row.yolo = checked; this.saveRepo(); cx.notify(); })
          .child(h_flex().gap(8).items_center()
            .child(div().size(16).border_1().rounded(3).border_color(row.yolo ? t.accent : t.input).bg(row.yolo ? t.accent : t.background)
              .child(div().m(3).size(8).rounded(1).bg(t.background)))
            .child(div().text_size(13).child("YOLO mode")))))
        .child(div().text_size(12).text_color(t.muted).child(row.terminal
          ? `Existing pane · ${row.recorded ? row.yolo ? `YOLO · ${YOLO_EFFECT[row.key]}` : "Client settings at launch" : "Launch settings not recorded"}`
          : row.yolo ? details?.yolo_flag ? YOLO_EFFECT[row.key] : "YOLO not verified · retry or turn it off to continue" : agent?.loading || !details ? "Checking client options…" : details.yolo_flag ? "Off · use client settings" : "YOLO unavailable · use client settings")))
      .when(connectionBlocked, el => el.child(div().text_size(12).text_color(t.blocked).child("This agent app cannot yet be given a role's connections.")))
      .when(!row.terminal && Boolean(details?.note), el => el.child(div().text_size(12).text_color(t.muted).child(details.note)))
      .when(!row.terminal && Boolean(details?.error), el => el.child(this.button(`${row.id}-retry`, "Retry client options", row.retry, cx => {
        cx.spawn(async cx => this.loadModels(agent, cx));
      }, !this.busy && !agent.loading)));
  }

  // Keep project and action fixed while the editable team list scrolls.
  render(cx) {
    const t = theme();
    const pending = this.rows.filter(row => !row.terminal);
    const supported = pending.filter(row => (!row.hasConnections || CONNECTION_CLIENTS.has(row.key)) && (row.yolo || this.agents.find(agent => agent.key === row.key)?.details?.yolo_flag));
    const allYolo = supported.length > 0 && supported.every(row => row.yolo);
    const reason = !this.loading && !this.busy && !this.error && Boolean(this.root) && SWARM.test(this.swarm) && this.rows.length > 0 ? this.blockingReason() : "";
    return v_flex().id("chorus-launcher").size_full().p(22).gap(14).bg(t.popover).text_color(t.foreground).track_focus(this.handle)
      .on_key_down((event, cx) => { if (event.key === "escape" && !this.picker) { closeOverlay(props().overlay); cx.stop_propagation(); } })
      .child(div().text_size(20).font_semibold().child("Start Chorus"))
      .child(div().text_size(13).text_color(t.muted).child("Choose a client, model, and thinking level for each pane. New agents join and wait for work."))
      .when(Boolean(this.update), el => el.child(v_flex().gap(6).p(10).rounded(6).border_1().border_color(t.border)
        .child(div().text_size(13).text_color(this.update.status === "error" ? t.blocked : t.foreground).child(this.update.message))
        .when(this.update.status !== "ready", el => el.child(this.button("chorus-update", this.update.status === "installing" ? "Updating…" : this.update.status === "checking" ? "Checking…" : this.update.status === "error" ? "Check for updates" : "Update & restart", this.buttons.update, cx => this.update?.status === "error" ? this.refreshUpdate(cx) : this.updateChorus(cx), this.update.status !== "installing" && this.update.status !== "checking")))))
      .child(h_flex().gap(20)
        .child(v_flex().flex_1().min_w(0).gap(5).child(div().text_size(12).text_color(t.muted).child("Project"))
          .child(div().text_size(13).truncate().child(this.root || this.folder || "Focus a project terminal")))
        .child(v_flex().w(210).gap(5).child(div().text_size(12).text_color(t.muted).child("Chorus name"))
          .child(this.teamExists || this.rosterMode ? div().h(36).py(8).child(this.swarm) : Input.new(this.input).accessibility_label("Chorus name").w_full().h(36).px(10).rounded(6).border_1().border_color(t.input).bg(t.background))))
      .when(Boolean(this.swarm) && !SWARM.test(this.swarm), el => el.child(div().text_size(12).text_color(t.blocked).child("Use 1–32 letters, numbers, underscores or hyphens, starting with a letter or number.")))
      .child(h_flex().items_center().gap(8)
        .when(!this.rosterMode, el => el.child(this.button("chorus-add", "Add agent", this.buttons.add, cx => this.addAgent(cx), !this.loading && !this.busy && this.agents.length > 0)))
        .when(!this.rosterMode, el => el.child(this.button("chorus-all", "Add all installed", this.buttons.all, cx => this.addAll(cx), !this.loading && !this.busy && this.agents.length > 0)))
        .child(div().flex_1())
        .child(this.button("chorus-yolo-all", allYolo ? "Turn YOLO off for all" : "Apply YOLO to all", this.buttons["yolo-all"], cx => {
          for (const row of supported) row.yolo = !allYolo;
          this.saveRepo();
          cx.notify();
        }, !this.busy && supported.length > 0)))
      .child(v_flex().id("chorus-team-rows").flex_1().min_h(0).overflow_y_scrollbar().text_size(13)
        .when(this.loading, el => el.child(div().py(16).text_color(t.muted).child("Checking project and existing team…")))
        .when(!this.loading && this.rows.length === 0, el => el.child(div().py(16).text_color(t.muted).child("No agents selected. Add an agent to open your team.")))
        .children(this.rows.map((row, index) => this.renderRow(row, index))))
      .when(Boolean(this.error), el => el.child(div().text_size(12).text_color(t.blocked).child(this.error)))
      .when(Boolean(reason), el => el.child(div().text_size(12).text_color(t.muted).child(reason)))
      .child(div().text_size(12).text_color(t.muted).child("YOLO off uses your client’s existing permissions. Choices are remembered per project folder; existing panes keep their launch settings."))
      .child(h_flex().items_center().gap(10)
        .child(this.button("chorus-reload", "Reload team", this.buttons.reload, cx => {
          this.busy = true; this.error = "";
          cx.spawn(async cx => { try { await this.loadTeam(cx); } catch (error) { this.error = String(error.message ?? error); } this.busy = false; cx.notify(); });
        }, !this.loading && !this.busy && Boolean(this.root)))
        .child(div().flex_1())
        .child(this.button("chorus-close", "Close", this.buttons.close, () => closeOverlay(props().overlay)))
        .child(this.button("chorus-start", this.busy ? "Opening team…" : "Open Chorus team", this.buttons.start, cx => this.start(cx), this.ready(), true)));
  }
}
