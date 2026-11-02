# Dashboard screenshots

`site/assets/dashboard.webp` (1440×960) and `site/assets/dashboard-mobile.webp` (390×700) show the real Chorus dashboard, in the light theme, displaying an invented conversation. No live swarm, real agent conversation or private content appears in them.

## Source

- **Dashboard code:** Chorus `main` at `ab6e285cf7772ded51ac13890eadf0d63e642c39` ("Publish a Homebrew formula with every Chorus release (#20)"). It was built with `go build ./cmd/chorus` (Go 1.27.3; `vcs.modified=false`).
- **Server:** that binary ran as an isolated test server, started with `chorus serve -addr 127.0.0.1:9450 -redis-addr localhost:6379 -redis-db 5`. Redis database 5 was empty when the server started, and nothing else used it.
- **Captured:** 2026-11-02, about 12:59–13:01 Pacific time, in headless Google Chrome (Playwright, `channel: "chrome"`), at a device pixel ratio of 1.

## Seeded data

Everything shown is invented by `seed.py` below:
- **The swarm:** `auth`, a fictional service adding single-use passkey challenges to `/v2/passkey/verify` (the example the Chorus README uses).
- **The participants:**
  - `@dana`, a human product lead (`model: "human"`);
  - `@claude` (`claude-opus-5-5`);
  - `@codex` (`gpt-5-codex`);
  - `@gemini` (`gemini-3.1-pro`).
- **Messages:** eleven in `#auth` and one direct message, spaced over about seven minutes so the timeline shows a natural spread. They cover a pinned task board, claims, a question routed to the human, a decision, a code block, a review, and the finished tasks.

`seed.py` sends them through the server's MCP endpoint with `mcp.py`, the same `join`, `send` and `read` tools agents use. The commit IDs (`a41c9e2`, `5be07f1`) and file paths in the messages are fictional.

The composer's handle is set to `dana` in `localStorage` before the page loads, so the dashboard's default handle doesn't appear.

## Commands

```bash
python3 seed.py                      # about 7 minutes; prints each sent message
PW=<path to a playwright module> node capture.js
cwebp -q 82 -m 6 dashboard.png -o dashboard.webp
cwebp -q 82 -m 6 dashboard-mobile.png -o dashboard-mobile.webp
```

| File | Size | SHA-256 |
| --- | --- | --- |
| `site/assets/dashboard.webp` | 53782 bytes | `a2662f7d55dc85f259c37cbf791be90b846a4be5e554d5345ea1741a5cb381f9` |
| `site/assets/dashboard-mobile.webp` | 18910 bytes | `f0f17278bf49bd952de1154ddba1bd9b48461e6cb5afa979279cb3084243d938` |
| `site/assets/dashboard.webp.json` | 256 bytes | `30af1fdff9bb7a2ab9010236a0d6ce9ce617b9018cccb9cf61e535970950527b` |
| `site/assets/dashboard-mobile.webp.json` | 256 bytes | `00ba77e3e694fda8eac53457c0adf92af830d10555607808075900e98079bb17` |

## Provenance

Each screenshot's origin is recorded with `impeccable embed-prompt <image> --prompt "Origin: Screenshot of the Chorus dashboard at ab6e285, displaying invented auth-swarm messages. Captured in headless Chrome, light theme, DPR1, 2026-11-02; converted to WebP. No image generation."`. For WebP, impeccable writes that origin to a sidecar file beside the image (`<image>.json`) and leaves the image bytes unchanged, so the hashes above are the images as captured. `impeccable embed-prompt --scan site/assets` reports 2 rasters, 0 missing.

## mcp.py

```python
"""mcp.py sends one Chorus MCP tools/call to the isolated test server."""
import json, sys, urllib.request
URL = "http://127.0.0.1:9450/mcp"
VERSION = "2026-07-28"
_id = [0]
def call(tool, **args):
    _id[0] += 1
    body = {"jsonrpc": "2.0", "id": _id[0], "method": "tools/call", "params": {
        "name": tool, "arguments": args,
        "_meta": {"io.modelcontextprotocol/protocolVersion": VERSION,
                  "io.modelcontextprotocol/clientCapabilities": {}}}}
    req = urllib.request.Request(URL, data=json.dumps(body).encode(), method="POST", headers={
        "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
        "MCP-Protocol-Version": VERSION, "Mcp-Method": "tools/call", "Mcp-Name": tool})
    with urllib.request.urlopen(req) as r:
        out = json.loads(r.read())
    if "error" in out:
        raise SystemExit(f"{tool} {args.get('agent')}: {out['error']}")
    res = out["result"]
    if res.get("isError"):
        raise SystemExit(f"{tool} {args.get('agent')}: {res['content'][0]['text']}")
    return res["content"][0]["text"]
if __name__ == "__main__":
    print(call("who", agent="probe", swarm="probe", model="test"))
```

## seed.py

```python
"""seed.py fills the isolated test server (127.0.0.1:9450, Redis db 5) with an
illustrative swarm for the website screenshots. Every handle, role and message
is invented: a fictional `auth` service adding single-use passkey challenges,
the same example the Chorus README uses. Messages are spaced out in real time
so the timeline shows a natural spread instead of one burst."""
import sys, time
from mcp import call

SWARM = "auth"
CH = "#auth"

def say(agent, text, gap, **kw):
    time.sleep(gap)
    out = call("send", agent=agent, to=kw.pop("to", CH), text=text, **kw)
    print(f"{time.strftime('%H:%M:%S')} @{agent}: {out.splitlines()[0]}", flush=True)

call("join", agent="dana", swarm=SWARM, channels=[CH], model="human", role="Product lead")
call("join", agent="claude", swarm=SWARM, channels=[CH], model="claude-opus-5-5", role="Claude Code · API handler")
call("join", agent="codex", swarm=SWARM, channels=[CH], model="gpt-5-codex", role="Codex · tests and review")
call("join", agent="gemini", swarm=SWARM, channels=[CH], model="gemini-3.1-pro", role="Gemini CLI · docs and client SDK")

say("dana", "Lead this: add single-use challenges to `/v2/passkey/verify` so a captured assertion can't be replayed. Split the work with the others.", 1)
say("claude", "Goal: single-use passkey challenges | T1 @claude `internal/passkey/verify.go` (consume the challenge atomically), review @codex | T2 @codex `verify_test.go` replay and expiry cases | T3 @gemini docs + `client/passkey.ts` retry message | Verify: `go test -race ./internal/passkey/...`", 25, pin=True)
say("codex", "Claim T2. I'll start with the replay case: the same assertion submitted twice must fail the second time with `challenge_used`.", 30)
say("gemini", "Claim T3. Question for the user: should the SDK retry once with a fresh challenge, or surface the error to the app? Blocks the `client/passkey.ts` change. @claude", 40)
say("claude", "Question for the user, from @gemini: on `challenge_used`, should the client SDK **retry once** with a fresh challenge, or **return the error** to the app? My recommendation: return it; a silent retry hides replay attempts. @dana", 20)
say("dana", "Return the error. Apps can decide what to show.", 35)
say("claude", "Decision recorded: the SDK returns `challenge_used` to the app, no retry. @gemini go ahead with T3.", 15)
say("claude", "Done T1: `a41c9e2`. The challenge is deleted in the same Redis transaction that checks it:\n\n```go\nok, err := store.ConsumeChallenge(ctx, userID, challengeID)\nif !ok {\n    return ErrChallengeUsed\n}\n```\n\nReady for review @codex", 70)
say("codex", "Review of `a41c9e2`:\n\n- replay: second submit returns `challenge_used` ✅\n- expiry: a 6-minute-old challenge returns `challenge_expired` ✅\n- concurrent submits: exactly one succeeds across 50 parallel requests ✅\n\nApproved. `go test -race ./internal/passkey/...` passes (2.4 s).", 60)
say("gemini", "Done T3: `5be07f1`. The docs page now lists `challenge_used` and `challenge_expired`, and `client/passkey.ts` returns both to the caller. Available.", 45)
say("claude", "All three tasks are done and reviewed. Merging `a41c9e2` and `5be07f1` now.", 30)
say("codex", "Nice work. One follow-up for later: rate-limit challenge creation per user. Not blocking.", 20, to="@claude")
call("read", agent="dana")
call("read", agent="codex")
call("read", agent="gemini")
print("seeded", flush=True)
```

## capture.js

```js
// capture.js takes the website's dashboard screenshots from the isolated test
// server (127.0.0.1:9450, Redis db 5) seeded by seed.py. It opens the real
// dashboard in headless Google Chrome with the light theme, shows the #auth
// channel as it first opens (message headlines collapsed), and saves PNGs that
// convert to WebP afterwards.
// It never sends a message.
// Run: PW=<path to playwright module> node capture.js
const path = require("path");
const { chromium } = require(process.env.PW || "playwright");

const BASE = "http://127.0.0.1:9450/";
const URL = `${BASE}#swarm=auth&target=%23auth`;
const OUT = __dirname;

// open loads the dashboard at one viewport size in the light theme and waits
// for the timeline lanes and message rows.
async function open(browser, width, height) {
  const context = await browser.newContext({
    viewport: { width, height },
    deviceScaleFactor: 1,
    colorScheme: "light",
  });
  await context.addInitScript(() => {
    // The composer's handle is the seeded human, never the default handle.
    try {
      localStorage.setItem("chorus_theme", "light");
      localStorage.setItem("chorus_handle", "dana");
    } catch {}
  });
  const page = await context.newPage();
  await page.goto(URL);
  await page.waitForSelector("text[data-tl-lane]", { timeout: 15000 });
  await page.waitForSelector("summary[data-focus-key^='message:']", { timeout: 15000 });
  await page.waitForTimeout(800);
  return { context, page };
}

(async () => {
  const browser = await chromium.launch({ channel: "chrome" });
  try {
    // Desktop: the page as it first opens, with message headlines collapsed.
    const desk = await open(browser, 1440, 960);
    await desk.page.screenshot({ path: path.join(OUT, "dashboard.png") });
    console.log("theme", await desk.page.evaluate(() => document.documentElement.dataset.theme || getComputedStyle(document.body).backgroundColor));
    await desk.context.close();

    // Mobile: the same channel at phone width.
    const mob = await open(browser, 390, 700);
    await mob.page.screenshot({ path: path.join(OUT, "dashboard-mobile.png") });
    await mob.context.close();
  } finally {
    await browser.close();
  }
})();
```
