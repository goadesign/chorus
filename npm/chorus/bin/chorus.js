#!/usr/bin/env node
// Downloads and SHA-256-verifies the matching Chorus release binary on first
// run, caches it per version under ~/.chorus/npm/<version>/chorus, and runs it
// with all CLI arguments forwarded.
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const pkgPath = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "package.json");
const pkg = JSON.parse(fs.readFileSync(pkgPath, "utf8"));

// resolvePlatform maps Node's process.platform and process.arch to Chorus's
// release archive naming convention (darwin/linux and arm64/amd64).
export function resolvePlatform(platform = process.platform, arch = process.arch) {
  const osMap = { darwin: "darwin", linux: "linux" };
  const archMap = { arm64: "arm64", x64: "amd64" };
  const goOS = osMap[platform];
  const goArch = archMap[arch];
  if (!goOS || !goArch) {
    throw new Error(`Unsupported platform ${platform}/${arch}; Chorus provides darwin and linux binaries for arm64 and x64.`);
  }
  return { goOS, goArch };
}

// parseChecksum extracts the expected hex SHA-256 digest for archiveName from
// a shasum-formatted checksums.txt document.
export function parseChecksum(checksumsText, archiveName) {
  for (const rawLine of checksumsText.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line) continue;
    const match = /^([0-9a-fA-F]{64})\s+\*?(\S+)$/.exec(line);
    if (match && match[2] === archiveName) {
      return match[1].toLowerCase();
    }
  }
  throw new Error(`Checksum for ${archiveName} not found in checksums.txt`);
}

// fetchBuffer downloads url using Node's built-in fetch and returns a Buffer.
async function fetchBuffer(url) {
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) {
    throw new Error(`GET ${url} failed with HTTP ${res.status}`);
  }
  return Buffer.from(await res.arrayBuffer());
}

// ensureBinary returns the path to the verified Chorus binary for version,
// downloading and verifying it atomically if not already cached.
export async function ensureBinary({
  version = pkg.version,
  cacheRoot = process.env.CHORUS_CACHE_DIR || path.join(os.homedir(), ".chorus", "npm"),
  releaseBaseURL = process.env.CHORUS_RELEASE_BASE_URL || `https://github.com/goadesign/chorus/releases/download/v${version}`,
  platform = process.platform,
  arch = process.arch,
} = {}) {
  const { goOS, goArch } = resolvePlatform(platform, arch);
  const versionDir = path.join(cacheRoot, version, `${goOS}_${goArch}`);
  const binPath = path.join(versionDir, "chorus");
  if (fs.existsSync(binPath)) {
    return binPath;
  }

  const archiveName = `chorus_${version}_${goOS}_${goArch}.tar.gz`;
  const [archiveBuf, checksumsBuf] = await Promise.all([
    fetchBuffer(`${releaseBaseURL}/${archiveName}`),
    fetchBuffer(`${releaseBaseURL}/checksums.txt`),
  ]);

  const expected = parseChecksum(checksumsBuf.toString("utf8"), archiveName);
  const actual = crypto.createHash("sha256").update(archiveBuf).digest("hex");
  if (actual !== expected) {
    throw new Error(`SHA-256 mismatch for ${archiveName}: expected ${expected}, got ${actual}`);
  }

  fs.mkdirSync(versionDir, { recursive: true });
  const stageDir = fs.mkdtempSync(path.join(versionDir, `.stage-${process.pid}-`));
  try {
    const archivePath = path.join(stageDir, archiveName);
    fs.writeFileSync(archivePath, archiveBuf);
    execFileSync("tar", ["-xzf", archivePath, "-C", stageDir, "chorus"], { stdio: "pipe" });
    const stagedBin = path.join(stageDir, "chorus");
    fs.chmodSync(stagedBin, 0o755);
    fs.renameSync(stagedBin, binPath);
  } finally {
    fs.rmSync(stageDir, { recursive: true, force: true });
  }
  return binPath;
}

async function main() {
  const binPath = await ensureBinary();
  const child = spawn(binPath, process.argv.slice(2), { stdio: "inherit" });
  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, () => {
      try {
        child.kill(sig);
      } catch {}
    });
  }
  child.on("exit", (code, signal) => {
    if (signal) {
      process.removeAllListeners(signal);
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code ?? 0);
  });
  child.on("error", (err) => {
    process.stderr.write(`chorus: ${err.message}\n`);
    process.exit(1);
  });
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))) {
  main().catch((err) => {
    process.stderr.write(`chorus: ${err.message}\n`);
    process.exit(1);
  });
}
