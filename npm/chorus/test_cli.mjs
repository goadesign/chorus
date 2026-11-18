import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawnSync } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { ensureBinary, parseChecksum, resolvePlatform } from "./bin/chorus.js";

test("resolvePlatform maps supported OS/arch pairs and rejects unsupported ones", () => {
  assert.deepEqual(resolvePlatform("darwin", "arm64"), { goOS: "darwin", goArch: "arm64" });
  assert.deepEqual(resolvePlatform("linux", "x64"), { goOS: "linux", goArch: "amd64" });
  assert.throws(() => resolvePlatform("win32", "x64"), /Unsupported platform/);
});

test("ensureBinary verifies SHA-256, extracts chorus, reuses cache, and rejects tampered tarballs", async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "chorus-npm-test-"));
  const { goOS, goArch } = resolvePlatform();
  const version = "0.99.0";
  const archiveName = `chorus_${version}_${goOS}_${goArch}.tar.gz`;

  // Build a tiny executable shell script packaged as chorus inside the tarball.
  const buildDir = path.join(tmp, "build");
  fs.mkdirSync(buildDir, { recursive: true });
  const fakeBin = path.join(buildDir, "chorus");
  fs.writeFileSync(fakeBin, '#!/bin/sh\nprintf "chorus-mock %s\\n" "$*"\n', { mode: 0o755 });
  const tarPath = path.join(tmp, archiveName);
  execFileSync("tar", ["-czf", tarPath, "-C", buildDir, "chorus"]);
  const tarBytes = fs.readFileSync(tarPath);
  const sha = crypto.createHash("sha256").update(tarBytes).digest("hex");

  let downloads = 0;
  let corrupt = false;
  const server = http.createServer((req, res) => {
    if (req.url === `/${archiveName}`) {
      downloads += 1;
      res.writeHead(200, { "Content-Type": "application/gzip" });
      res.end(corrupt ? Buffer.from("tampered") : tarBytes);
      return;
    }
    if (req.url === "/checksums.txt") {
      res.writeHead(200, { "Content-Type": "text/plain" });
      res.end(`${sha}  ${archiveName}\n`);
      return;
    }
    res.writeHead(404).end();
  });

  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  const releaseBaseURL = `http://127.0.0.1:${port}`;

  try {
    const cacheRoot = path.join(tmp, "cache");
    const binPath = await ensureBinary({ version, cacheRoot, releaseBaseURL });
    assert.equal(downloads, 1);
    const out = execFileSync(binPath, ["version"], { encoding: "utf8" });
    assert.equal(out.trim(), "chorus-mock version");

    // Second call uses the cached binary without hitting the network again.
    const cachedPath = await ensureBinary({ version, cacheRoot, releaseBaseURL });
    assert.equal(cachedPath, binPath);
    assert.equal(downloads, 1);

    // Tampered archive in a fresh cache directory fails closed without writing a binary.
    corrupt = true;
    const badCache = path.join(tmp, "bad-cache");
    await assert.rejects(
      ensureBinary({ version, cacheRoot: badCache, releaseBaseURL }),
      /SHA-256 mismatch/
    );
    assert.equal(fs.existsSync(path.join(badCache, version, `${goOS}_${goArch}`, "chorus")), false);

    assert.equal(parseChecksum(`${sha}  ${archiveName}\n`, archiveName), sha);

    // Verify CLI entrypoint forwards arguments, exit code, and fatal signals accurately.
    const cliEntry = fileURLToPath(new URL("./bin/chorus.js", import.meta.url));
    const pkgVersion = JSON.parse(fs.readFileSync(new URL("./package.json", import.meta.url), "utf8")).version;
    const sigBinDir = path.join(cacheRoot, pkgVersion, `${goOS}_${goArch}`);
    fs.mkdirSync(sigBinDir, { recursive: true });
    fs.writeFileSync(
      path.join(sigBinDir, "chorus"),
      '#!/bin/sh\nif [ "$1" = "--sigterm" ]; then kill -TERM $$; fi\nprintf "ok:%s\\n" "$*"\n',
      { mode: 0o755 }
    );
    const runRes = spawnSync(process.execPath, [cliEntry, "status", "--flag"], {
      env: { ...process.env, CHORUS_CACHE_DIR: cacheRoot },
      encoding: "utf8",
    });
    assert.equal(runRes.status, 0);
    assert.equal(runRes.stdout.trim(), "ok:status --flag");

    const sigRes = spawnSync(process.execPath, [cliEntry, "--sigterm"], {
      env: { ...process.env, CHORUS_CACHE_DIR: cacheRoot },
      encoding: "utf8",
    });
    assert.equal(sigRes.signal, "SIGTERM");
  } finally {
    server.close();
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
