import fs from "node:fs";
import path from "node:path";
import { run } from "node:test";
import { spec } from "node:test/reporters";
import { ensureBuiltBinary } from "./lib/server.mjs";

const E2E_ROOT = import.meta.dirname;
const VALID_TIERS = new Set(["1", "2", "3", "4", "all"]);

function printUsage() {
  console.log(`Volo-go E2E Test Runner

Usage:
  node tests/e2e/run.mjs [options]

Options:
  --tier=1|2|3|4|all   Select tier to run (default: all)
  --filter=<substr>    Filter by test file path substring, or by test name if no file matches
  --concurrency=<n>    Max parallel test files per tier (default: VOLO_E2E_CONCURRENCY or 1)
  --keep-tmp           Preserve temporary server data/extension dirs (VOLO_E2E_KEEP_TMP=1)
  -h, --help           Show this help message
`);
}

function parseArgs(argv) {
  let tier = "all";
  let filter = "";
  let concurrency = Number(process.env.VOLO_E2E_CONCURRENCY || "1");
  let keepTmp = process.env.VOLO_E2E_KEEP_TMP === "1";
  let help = false;

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "-h" || arg === "--help") {
      help = true;
    } else if (arg === "--keep-tmp") {
      keepTmp = true;
    } else if (arg.startsWith("--tier=")) {
      tier = arg.slice("--tier=".length).trim().toLowerCase();
    } else if (arg === "--tier" && i + 1 < argv.length) {
      tier = argv[++i].trim().toLowerCase();
    } else if (arg.startsWith("--filter=")) {
      filter = arg.slice("--filter=".length).trim();
    } else if (arg === "--filter" && i + 1 < argv.length) {
      filter = argv[++i].trim();
    } else if (arg.startsWith("--concurrency=")) {
      concurrency = Number(arg.slice("--concurrency=".length).trim());
    } else if (arg === "--concurrency" && i + 1 < argv.length) {
      concurrency = Number(argv[++i].trim());
    } else {
      throw new Error(`Unknown CLI argument: ${arg}`);
    }
  }

  if (!VALID_TIERS.has(tier)) {
    throw new Error(`Invalid --tier="${tier}". Expected 1, 2, 3, 4, or all.`);
  }
  if (!Number.isInteger(concurrency) || concurrency < 1) {
    throw new Error(`Invalid --concurrency="${concurrency}". Expected integer >= 1.`);
  }

  return { tier, filter, concurrency, keepTmp, help };
}

function escapeRegExp(str) {
  return str.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function discoverTierFiles(tierNum) {
  const dir = path.join(E2E_ROOT, `tier${tierNum}`);
  if (!fs.existsSync(dir)) return [];
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .filter((d) => d.isFile() && d.name.endsWith(".test.mjs"))
    .map((d) => path.join(dir, d.name))
    .sort((a, b) => a.localeCompare(b));
}

async function runTier(tierNum, files, { concurrency, testNamePatterns, env }) {
  const startedAt = Date.now();
  console.log(`\n=== Tier ${tierNum} (${files.length} file${files.length === 1 ? "" : "s"}) ===`);

  const stream = run({
    files,
    isolation: "process",
    concurrency,
    testNamePatterns,
    env,
  });

  let rootSummary = null;
  const fileSummaries = [];
  let sawFailure = false;
  let innerTests = 0;

  stream.on("test:pass", (ev) => {
    if ((ev?.nesting ?? 0) >= 1 && ev?.details?.type !== "suite") {
      innerTests += 1;
    }
  });

  stream.on("test:fail", (ev) => {
    if ((ev?.nesting ?? 0) >= 1 && ev?.details?.type !== "suite") {
      innerTests += 1;
    }
    if (ev?.nesting === 0 || ev?.file) {
      sawFailure = true;
    } else {
      sawFailure = true;
    }
  });

  stream.on("test:skip", (ev) => {
    if ((ev?.nesting ?? 0) >= 1 && ev?.details?.type !== "suite") {
      innerTests += 1;
    }
  });

  stream.on("test:summary", (ev) => {
    if (!ev?.file) {
      rootSummary = ev;
    } else {
      fileSummaries.push(ev);
    }
  });

  const reported = stream.compose(spec);
  reported.pipe(process.stdout, { end: false });

  await new Promise((resolve, reject) => {
    reported.on("end", resolve);
    reported.on("error", reject);
    stream.on("error", reject);
  });

  const elapsedMs = Date.now() - startedAt;
  const counts =
    fileSummaries.length > 0
      ? fileSummaries.reduce(
          (acc, item) => ({
            tests: acc.tests + (item.counts?.tests || 0),
            passed: acc.passed + (item.counts?.passed || 0),
            failed: acc.failed + (item.counts?.failed || 0),
            skipped: acc.skipped + (item.counts?.skipped || 0),
            cancelled: acc.cancelled + (item.counts?.cancelled || 0),
          }),
          { tests: 0, passed: 0, failed: 0, skipped: 0, cancelled: 0 },
        )
      : rootSummary?.counts || { tests: 0, passed: 0, failed: 0, skipped: 0, cancelled: 0 };

  const success =
    !sawFailure &&
    (rootSummary ? rootSummary.success !== false : true) &&
    innerTests > 0 &&
    (counts.tests || 0) > 0 &&
    (counts.failed || 0) === 0 &&
    (counts.cancelled || 0) === 0;

  return {
    tier: tierNum,
    files: files.length,
    tests: counts.tests || 0,
    innerTests,
    passed: counts.passed || 0,
    failed: counts.failed || 0,
    skipped: counts.skipped || 0,
    cancelled: counts.cancelled || 0,
    durationMs: Math.round(rootSummary?.duration_ms ?? elapsedMs),
    success,
  };
}

async function main() {
  let opts;
  try {
    opts = parseArgs(process.argv.slice(2));
  } catch (err) {
    console.error(`Error: ${err.message}`);
    printUsage();
    process.exitCode = 1;
    return;
  }

  if (opts.help) {
    printUsage();
    return;
  }

  if (opts.keepTmp) {
    process.env.VOLO_E2E_KEEP_TMP = "1";
  }

  const targetTiers = opts.tier === "all" ? [1, 2, 3, 4] : [Number(opts.tier)];
  const discoveredByTier = targetTiers.map((t) => ({
    tier: t,
    files: discoverTierFiles(t),
  }));

  const allDiscovered = discoveredByTier.flatMap((entry) => entry.files);
  let selectedByTier = discoveredByTier;
  let testNamePatterns;

  if (opts.filter) {
    const needle = opts.filter.toLowerCase();
    const pathMatched = discoveredByTier
      .map((entry) => ({
        tier: entry.tier,
        files: entry.files.filter((f) =>
          path.relative(E2E_ROOT, f).replace(/\\/g, "/").toLowerCase().includes(needle),
        ),
      }))
      .filter((entry) => entry.files.length > 0);

    if (pathMatched.length > 0) {
      selectedByTier = pathMatched;
    } else {
      selectedByTier = discoveredByTier.filter((entry) => entry.files.length > 0);
      testNamePatterns = [new RegExp(escapeRegExp(opts.filter), "i")];
    }
  } else {
    selectedByTier = discoveredByTier.filter((entry) => entry.files.length > 0);
  }

  const totalFiles = selectedByTier.reduce((sum, e) => sum + e.files.length, 0);
  if (totalFiles === 0) {
    const scopeDesc =
      allDiscovered.length === 0
        ? `No *.test.mjs files found for tier=${opts.tier} under ${E2E_ROOT}.`
        : `No test files matched --tier=${opts.tier} --filter="${opts.filter}".`;
    console.error(`Error: ${scopeDesc}`);
    process.exitCode = 1;
    return;
  }

  const buildStart = Date.now();
  console.log("Preparing Volo-go E2E binary...");
  const built = await ensureBuiltBinary();
  console.log(
    `Binary ready (${built.reused ? "reused VOLO_E2E_BIN" : "built fresh"} in ${Date.now() - buildStart}ms): ${built.binPath}`,
  );

  let cleanedUp = false;
  const cleanupBuilt = async () => {
    if (cleanedUp) return;
    cleanedUp = true;
    await built.cleanup();
  };

  let signalHandled = false;
  const onSignal = (sig) => {
    if (signalHandled) return;
    signalHandled = true;
    console.error(`\nReceived ${sig}, cleaning up temporary binary...`);
    cleanupBuilt().finally(() => {
      process.exit(sig === "SIGINT" ? 130 : 143);
    });
  };
  process.on("SIGINT", onSignal);
  process.on("SIGTERM", onSignal);

  const childEnv = {
    ...process.env,
    VOLO_E2E_BIN: built.binPath,
    ...(opts.keepTmp ? { VOLO_E2E_KEEP_TMP: "1" } : {}),
  };

  const results = [];
  try {
    for (const entry of selectedByTier) {
      const res = await runTier(entry.tier, entry.files, {
        concurrency: opts.concurrency,
        testNamePatterns,
        env: childEnv,
      });
      results.push(res);
    }
  } finally {
    process.off("SIGINT", onSignal);
    process.off("SIGTERM", onSignal);
    await cleanupBuilt();
  }

  const totals = results.reduce(
    (acc, r) => ({
      files: acc.files + r.files,
      tests: acc.tests + r.tests,
      innerTests: acc.innerTests + r.innerTests,
      passed: acc.passed + r.passed,
      failed: acc.failed + r.failed,
      skipped: acc.skipped + r.skipped,
      cancelled: acc.cancelled + r.cancelled,
      durationMs: acc.durationMs + r.durationMs,
      success: acc.success && r.success,
    }),
    {
      files: 0,
      tests: 0,
      innerTests: 0,
      passed: 0,
      failed: 0,
      skipped: 0,
      cancelled: 0,
      durationMs: 0,
      success: true,
    },
  );

  console.log("\n=== E2E Summary ===");
  for (const r of results) {
    const status = r.success ? "PASS" : "FAIL";
    console.log(
      `Tier ${r.tier} [${status}]: tests=${r.tests}, passed=${r.passed}, failed=${r.failed}, skipped=${r.skipped}, duration=${(r.durationMs / 1000).toFixed(2)}s`,
    );
  }
  const hasExecutedTests = totals.tests > 0 && totals.innerTests > 0;
  console.log(
    `Total [${totals.success && hasExecutedTests ? "PASS" : "FAIL"}]: files=${totals.files}, tests=${totals.tests}, passed=${totals.passed}, failed=${totals.failed}, skipped=${totals.skipped}, duration=${(totals.durationMs / 1000).toFixed(2)}s`,
  );

  if (!hasExecutedTests) {
    const filterMsg = opts.filter ? ` matching --filter="${opts.filter}"` : "";
    console.error(
      `Error: Zero inner test cases executed (tier=${opts.tier}${filterMsg ? `,${filterMsg}` : ""}).`,
    );
  }

  if (!totals.success || !hasExecutedTests || totals.failed > 0 || totals.cancelled > 0) {
    process.exitCode = 1;
  } else {
    process.exitCode = 0;
  }
}

await main();
