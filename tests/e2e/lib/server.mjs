import { execFile, execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

export const REPO_ROOT = path.resolve(import.meta.dirname, "..", "..", "..");

const DEFAULT_E2E_VERSION = "v1.0.4-e2e";

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function removeDirBestEffort(dirPath) {
  if (!dirPath) return;
  try {
    fs.rmSync(dirPath, {
      recursive: true,
      force: true,
      maxRetries: 6,
      retryDelay: 100,
    });
  } catch {
    // Ignore best-effort temp cleanup errors on Windows file-lock races.
  }
}

function copyDirIfPresent(srcDir, dstDir) {
  fs.mkdirSync(dstDir, { recursive: true });
  if (!fs.existsSync(srcDir)) {
    return;
  }
  fs.cpSync(srcDir, dstDir, {
    recursive: true,
    filter: (src) => {
      const base = path.basename(src);
      return base !== ".git" && base !== "node_modules" && base !== ".DS_Store";
    },
  });
}

/**
 * Resolve an existing Volo binary from VOLO_E2E_BIN or build ./cmd/volo once
 * into a temporary directory with CGO_ENABLED=0 and GOEXPERIMENT=nojsonv2.
 */
export async function ensureBuiltBinary(opts = {}) {
  const envBin = (process.env.VOLO_E2E_BIN || "").trim();
  if (envBin && !opts.forceBuild) {
    if (!fs.existsSync(envBin)) {
      throw new Error(`VOLO_E2E_BIN points to a missing file: ${envBin}`);
    }
    return {
      binPath: path.resolve(envBin),
      reused: true,
      cleanup: async () => {},
    };
  }

  const version = (opts.version || DEFAULT_E2E_VERSION).trim();
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "volo-e2e-bin-"));
  const binName = process.platform === "win32" ? "volo.exe" : "volo";
  const binPath = path.join(tmpDir, binName);

  const buildEnv = {
    ...process.env,
    CGO_ENABLED: "0",
    GOEXPERIMENT: "nojsonv2",
  };

  try {
    execFileSync(
      "go",
      [
        "build",
        "-buildvcs=false",
        "-trimpath",
        `-ldflags=-s -w -X main.Version=${version}`,
        "-o",
        binPath,
        "./cmd/volo",
      ],
      {
        cwd: REPO_ROOT,
        env: buildEnv,
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
  } catch (err) {
    removeDirBestEffort(tmpDir);
    const stderr = err?.stderr ? String(err.stderr) : "";
    const stdout = err?.stdout ? String(err.stdout) : "";
    throw new Error(
      `Failed to build ./cmd/volo: ${err?.message || err}\n${stdout}\n${stderr}`.trim(),
    );
  }

  return {
    binPath,
    reused: false,
    cleanup: async () => {
      if (process.env.VOLO_E2E_KEEP_TMP === "1") return;
      removeDirBestEffort(tmpDir);
    },
  };
}

/**
 * Allocate an ephemeral free TCP port on 127.0.0.1.
 */
export function allocateFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const addr = srv.address();
      if (!addr || typeof addr === "string") {
        srv.close(() => reject(new Error("Failed to resolve ephemeral port")));
        return;
      }
      const { port } = addr;
      srv.close((closeErr) => {
        if (closeErr) reject(closeErr);
        else resolve(port);
      });
    });
  });
}

/**
 * Execute a Volo CLI subcommand (e.g., version, doctor --json, healthcheck, admin ...).
 */
export async function runVoloCommand(args, opts = {}) {
  let ownedBuild = null;
  let binPath = opts.binPath || (process.env.VOLO_E2E_BIN || "").trim();
  if (!binPath) {
    ownedBuild = await ensureBuiltBinary({ version: opts.version });
    binPath = ownedBuild.binPath;
  }

  try {
    const { stdout, stderr } = await execFileAsync(binPath, args, {
      cwd: opts.cwd || REPO_ROOT,
      env: { ...process.env, ...(opts.env || {}) },
      timeout: opts.timeoutMs ?? 30000,
      maxBuffer: 10 * 1024 * 1024,
      windowsHide: true,
    });
    return { code: 0, stdout: String(stdout), stderr: String(stderr) };
  } catch (err) {
    return {
      code: typeof err?.code === "number" ? err.code : 1,
      stdout: err?.stdout ? String(err.stdout) : "",
      stderr: err?.stderr ? String(err.stderr) : String(err?.message || ""),
    };
  } finally {
    if (ownedBuild) {
      await ownedBuild.cleanup();
    }
  }
}

async function stopChildProcess(child) {
  if (!child) return;
  if (child.exitCode !== null || child.signalCode !== null) {
    return;
  }

  await new Promise((resolve) => {
    let settled = false;
    const done = () => {
      if (settled) return;
      settled = true;
      clearTimeout(forceTimer);
      resolve();
    };

    child.once("exit", done);

    try {
      child.kill("SIGTERM");
    } catch {
      done();
      return;
    }

    const forceTimer = setTimeout(() => {
      if (child.exitCode === null && child.signalCode === null) {
        try {
          child.kill("SIGKILL");
        } catch {
          // Ignore if already exited.
        }
      }
      setTimeout(done, 500);
    }, 5000);
    forceTimer.unref();
  });
}

/**
 * Start an isolated Volo server instance on 127.0.0.1:<ephemeralPort>
 * with dedicated temp data/ and copied themes/, plugins/, locales/.
 */
export async function startVoloServer(opts = {}) {
  let ownedBuild = null;
  let binPath = opts.binPath || (process.env.VOLO_E2E_BIN || "").trim();
  if (!binPath) {
    ownedBuild = await ensureBuiltBinary({ version: opts.version });
    binPath = ownedBuild.binPath;
  } else if (!fs.existsSync(binPath)) {
    throw new Error(`Volo binary not found at ${binPath}`);
  }

  const workDir = fs.mkdtempSync(path.join(os.tmpdir(), "volo-e2e-srv-"));
  const dataDir = path.join(workDir, "data");
  const themesDir = path.join(workDir, "themes");
  const pluginsDir = path.join(workDir, "plugins");
  const localesDir = path.join(workDir, "locales");

  fs.mkdirSync(dataDir, { recursive: true });
  if (opts.copyExtensions !== false) {
    copyDirIfPresent(path.join(REPO_ROOT, "themes"), themesDir);
    copyDirIfPresent(path.join(REPO_ROOT, "plugins"), pluginsDir);
    copyDirIfPresent(path.join(REPO_ROOT, "locales"), localesDir);
  } else {
    fs.mkdirSync(themesDir, { recursive: true });
    fs.mkdirSync(pluginsDir, { recursive: true });
    fs.mkdirSync(localesDir, { recursive: true });
  }

  const port = opts.port || (await allocateFreePort());
  const baseUrl = `http://127.0.0.1:${port}`;
  const adminPath = opts.adminPath || "/console";
  const defaultLocale = opts.defaultLocale || "zh-CN";
  const extraArgs = Array.isArray(opts.extraArgs) ? opts.extraArgs : [];

  const args = [
    "serve",
    "--listen-addr",
    `127.0.0.1:${port}`,
    "--data-dir",
    dataDir,
    "--public-url",
    baseUrl,
    "--themes-dir",
    themesDir,
    "--plugins-dir",
    pluginsDir,
    "--locales-dir",
    localesDir,
    "--admin-path",
    adminPath,
    "--default-locale",
    defaultLocale,
  ];
  if (opts.secretKey) {
    args.push("--secret-key", opts.secretKey);
  }
  args.push(...extraArgs);

  const stdoutChunks = [];
  const stderrChunks = [];
  const logs = {
    stdout: stdoutChunks,
    stderr: stderrChunks,
    combined() {
      return `--- STDOUT ---\n${stdoutChunks.join("")}\n--- STDERR ---\n${stderrChunks.join("")}`;
    },
  };

  const child = spawn(binPath, args, {
    cwd: workDir,
    env: { ...process.env, ...(opts.env || {}) },
    stdio: ["ignore", "pipe", "pipe"],
    windowsHide: true,
  });

  child.stdout?.on("data", (chunk) => stdoutChunks.push(String(chunk)));
  child.stderr?.on("data", (chunk) => stderrChunks.push(String(chunk)));

  let earlyExit = null;
  let spawnErr = null;
  child.once("exit", (code, signal) => {
    earlyExit = { code, signal };
  });
  child.once("error", (err) => {
    spawnErr = err;
  });

  let stopped = false;
  const stop = async () => {
    if (stopped) return;
    stopped = true;
    await stopChildProcess(child);
    if (process.env.VOLO_E2E_KEEP_TMP !== "1") {
      removeDirBestEffort(workDir);
    }
    if (ownedBuild) {
      await ownedBuild.cleanup();
    }
  };

  const timeoutMs = opts.readyTimeoutMs ?? 20000;
  const deadline = Date.now() + timeoutMs;
  let ready = false;
  let lastHealthErr = "";

  while (Date.now() < deadline) {
    if (spawnErr) {
      await stop();
      throw new Error(`Failed to spawn volo server: ${spawnErr.message}\n${logs.combined()}`);
    }
    if (earlyExit) {
      await stop();
      throw new Error(
        `volo server exited prematurely during boot (code=${earlyExit.code}, signal=${earlyExit.signal})\n${logs.combined()}`,
      );
    }

    try {
      const res = await fetch(`${baseUrl}/healthz`, {
        signal: AbortSignal.timeout(1500),
      });
      if (res.status === 200) {
        const body = await res.json();
        if (body?.ok === true) {
          ready = true;
          break;
        }
        lastHealthErr = `unexpected /healthz body: ${JSON.stringify(body)}`;
      } else {
        lastHealthErr = `/healthz returned HTTP ${res.status}`;
      }
    } catch (err) {
      lastHealthErr = err?.message || String(err);
    }
    await sleep(100);
  }

  if (!ready) {
    await stop();
    throw new Error(
      `Timed out waiting ${timeoutMs}ms for ${baseUrl}/healthz (${lastHealthErr})\n${logs.combined()}`,
    );
  }

  const setupOverrides = opts.setupPayload || {};
  const adminCreds = {
    username: setupOverrides.admin_username || "e2eadmin",
    email: setupOverrides.admin_email || "admin@e2e.local",
    password: setupOverrides.admin_password || "E2ePassword!123",
    displayName: setupOverrides.admin_display_name || "E2E Admin",
  };

  let setupResult = null;
  if (!opts.skipSetup) {
    const payload = {
      admin_username: adminCreds.username,
      admin_email: adminCreds.email,
      admin_password: adminCreds.password,
      admin_display_name: adminCreds.displayName,
      site_title: "Volo E2E",
      default_locale: defaultLocale,
      public_url: baseUrl,
      ...setupOverrides,
    };

    const setupRes = await fetch(`${baseUrl}/api/setup/complete`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "User-Agent": "Volo-E2E-Setup/1.0",
      },
      body: JSON.stringify(payload),
    });
    const setupText = await setupRes.text();
    let setupData = null;
    try {
      setupData = JSON.parse(setupText);
    } catch {
      setupData = null;
    }
    if (setupRes.status !== 200 || setupData?.ok !== true) {
      await stop();
      throw new Error(
        `Initial setup failed (HTTP ${setupRes.status}): ${setupText}\n${logs.combined()}`,
      );
    }
    setupResult = setupData;
  }

  return {
    baseUrl,
    port,
    workDir,
    dataDir,
    themesDir,
    pluginsDir,
    localesDir,
    adminCreds,
    setupResult,
    binPath,
    process: child,
    logs,
    stop,
  };
}
