import assert from "node:assert/strict";
import path from "node:path";
import { after, before, describe, it } from "node:test";
import {
  assertCookieSet,
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertRedirect,
  assertSSRFRejected,
  assertStatus,
} from "../lib/assertions.mjs";
import { createClient, generateTOTP } from "../lib/client.mjs";
import { runVoloCommand, startVoloServer } from "../lib/server.mjs";

function extractBootConfig(html) {
  const match = String(html || "").match(
    /<script\s+id=["']volo-boot-config["'][^>]*>([\s\S]*?)<\/script>/i,
  );
  assert.ok(match, "Expected <script id=\"volo-boot-config\"> tag in HTML");
  return JSON.parse(match[1].trim());
}

async function resolveCliVersion() {
  const res = await runVoloCommand(["version"]);
  assert.equal(res.code, 0, `volo version failed: ${res.stderr}`);
  const m = res.stdout.trim().match(/^volo\s+(v[^\s]+)\s+\(/i);
  assert.ok(m, `Unexpected volo version output: ${res.stdout}`);
  return { version: m[1], stdout: res.stdout.trim() };
}

describe("F1: CLI, Boot, /healthz, BootConfig.version & /api/admin/about/project-info", () => {
  let server;
  let cliVersion;

  before(async () => {
    const v = await resolveCliVersion();
    cliVersion = v.version;
    server = await startVoloServer();
  });

  after(async () => {
    if (server) {
      await server.stop();
    }
  });

  it("CLI version and doctor --json report build version and healthy runtime checks", async () => {
    const verRes = await runVoloCommand(["version"]);
    assert.equal(verRes.code, 0);
    assert.match(verRes.stdout, /^volo\s+v[^\s]+\s+\(go/i);
    assert.match(verRes.stdout, /pure-go static/i);

    const docRes = await runVoloCommand([
      "doctor",
      "--json",
      "--data-dir",
      server.dataDir,
      "--themes-dir",
      server.themesDir,
      "--plugins-dir",
      server.pluginsDir,
      "--locales-dir",
      server.localesDir,
    ]);
    assert.equal(docRes.code, 0, `volo doctor --json failed: ${docRes.stderr}`);
    const report = JSON.parse(docRes.stdout);
    assert.equal(report.healthy, true);
    assert.ok(Array.isArray(report.checks) && report.checks.length > 0);
  });

  it("/healthz responds to GET and HEAD, rejects POST with 405, and passes volo healthcheck CLI", async () => {
    const client = createClient(server.baseUrl);
    const getRes = await client.get("/healthz");
    assertStatus(getRes, 200, "GET /healthz");
    assertHeaderIncludes(getRes, "cache-control", "no-store", "GET /healthz");
    assertHeaderIncludes(getRes, "content-type", "application/json", "GET /healthz");
    assertJsonHasKeys(
      getRes.data,
      ["ok", "ready", "status", "initialized", "database", "uptime_sec"],
      "GET /healthz",
    );
    assert.equal(getRes.data.ok, true);
    assert.equal(getRes.data.ready, true);
    assert.equal(getRes.data.status, "ok");
    assert.equal(getRes.data.database, "ok");
    assert.equal(getRes.data.initialized, true);
    assert.ok(typeof getRes.data.uptime_sec === "number" && getRes.data.uptime_sec >= 0);

    const headRes = await client.request("HEAD", "/healthz");
    assertStatus(headRes, 200, "HEAD /healthz");
    assert.equal(headRes.text, "");

    const postRes = await client.post("/healthz", {});
    assertStatus(postRes, 405, "POST /healthz");
    assertHeaderIncludes(postRes, "allow", "GET", "POST /healthz");
    assertHeaderIncludes(postRes, "allow", "HEAD", "POST /healthz");

    const cliHealth = await runVoloCommand(["healthcheck", "--url", server.baseUrl]);
    assert.equal(cliHealth.code, 0, `volo healthcheck failed: ${cliHealth.stderr}`);
  });

  it("Pre-init GET /setup injects BootConfig with mode=setup and matching runtime version", async () => {
    const preInit = await startVoloServer({ skipSetup: true });
    try {
      const client = createClient(preInit.baseUrl);
      const res = await client.get("/setup");
      assertStatus(res, 200, "GET /setup");
      assertHeaderIncludes(res, "content-type", "text/html", "GET /setup");
      assertHeaderIncludes(res, "cache-control", "no-store", "GET /setup");

      const boot = extractBootConfig(res.text);
      assert.equal(boot.mode, "setup");
      assert.equal(boot.version, cliVersion);
      assert.equal(boot.admin_path, "/console");
      assert.equal(boot.public_url, preInit.baseUrl);
      assert.ok(Array.isArray(boot.locales) && boot.locales.length >= 2);
    } finally {
      await preInit.stop();
    }
  });

  it("Console SPA respects custom --admin-path, injects BootConfig.version, and hides default /console", async () => {
    const customSrv = await startVoloServer({ adminPath: "/panel-e2e" });
    try {
      const client = createClient(customSrv.baseUrl);
      const panelRes = await client.get("/panel-e2e");
      assertStatus(panelRes, 200, "GET /panel-e2e");
      assertHeaderIncludes(panelRes, "content-type", "text/html", "GET /panel-e2e");
      assert.match(panelRes.text, /<base\s+href=["']\/panel-e2e\/["']/i);

      const boot = extractBootConfig(panelRes.text);
      assert.equal(boot.mode, "console");
      assert.equal(boot.version, cliVersion);
      assert.equal(boot.admin_path, "/panel-e2e");

      const oldRes = await client.get("/console");
      assertStatus(oldRes, 404, "GET /console when custom admin-path active");
    } finally {
      await customSrv.stop();
    }
  });

  it("GET /api/admin/about/project-info?offline=1 enforces auth and returns complete project telemetry", async () => {
    const anon = createClient(server.baseUrl);
    const unauthRes = await anon.get("/api/admin/about/project-info?offline=1");
    assertStatus(unauthRes, 401, "Unauthenticated project-info");

    const admin = createClient(server.baseUrl);
    const loginRes = await admin.login(server.adminCreds.username, server.adminCreds.password);
    assertStatus(loginRes, 200, "Admin login for project-info");

    const infoRes = await admin.get("/api/admin/about/project-info?offline=1");
    assertStatus(infoRes, 200, "GET /api/admin/about/project-info?offline=1");
    assertJsonHasKeys(
      infoRes.data,
      [
        "project_name",
        "version",
        "latest_version",
        "has_update",
        "go_version",
        "os_arch",
        "db_dialect",
        "admin_gzip_kb",
        "online",
        "checked_at",
        "repo",
        "latest_release",
        "repo_url",
        "issues_url",
        "releases_url",
        "license",
        "live_fetched",
      ],
      "project-info response",
    );
    assert.equal(infoRes.data.project_name, "Volo-go");
    assert.equal(infoRes.data.version, cliVersion);
    assert.equal(infoRes.data.db_dialect, "sqlite");
    assert.equal(infoRes.data.online, false);
    assert.equal(infoRes.data.live_fetched, false);
    assert.equal(infoRes.data.license, "Apache-2.0");
    assert.ok(typeof infoRes.data.admin_gzip_kb === "number" && infoRes.data.admin_gzip_kb > 0);
    assertJsonHasKeys(
      infoRes.data.repo,
      ["full_name", "html_url", "description", "stars", "forks", "open_issues", "language", "license"],
      "project-info.repo",
    );
    assert.equal(infoRes.data.repo.full_name, "Willh92/Volo-go");
    assertJsonHasKeys(
      infoRes.data.latest_release,
      ["tag_name", "name", "html_url", "published_at"],
      "project-info.latest_release",
    );
    assert.ok(String(infoRes.data.latest_release.tag_name).length > 0);
  });
});

describe("F2: Setup Wizard (/setup, /api/setup/*) & Post-Init Lockout", () => {
  it("Pre-init root redirects 302 to /setup and /api/setup/status exposes uninitialized state", async () => {
    const srv = await startVoloServer({ skipSetup: true });
    try {
      const client = createClient(srv.baseUrl);
      const rootRes = await client.get("/");
      assertRedirect(rootRes, 302, "/setup", "Pre-init GET /");
      assertHeaderIncludes(rootRes, "cache-control", "no-store", "Pre-init GET /");

      const healthRes = await client.get("/healthz");
      assertStatus(healthRes, 200);
      assert.equal(healthRes.data.initialized, false);

      const statusRes = await client.get("/api/setup/status");
      assertStatus(statusRes, 200, "GET /api/setup/status");
      assertJsonHasKeys(
        statusRes.data,
        ["initialized", "version", "current_driver", "default_locale", "supported_locales", "admin_path", "timezone", "accent"],
        "setup/status",
      );
      assert.equal(statusRes.data.initialized, false);
      assert.equal(statusRes.data.current_driver, "sqlite");
      assert.ok(Array.isArray(statusRes.data.supported_locales) && statusRes.data.supported_locales.length >= 2);
    } finally {
      await srv.stop();
    }
  });

  it("POST /api/setup/test-db validates sqlite paths and rejects path traversal and SSRF targets", async () => {
    const srv = await startVoloServer({ skipSetup: true });
    try {
      const client = createClient(srv.baseUrl);
      const validPath = path.join(srv.dataDir, "probe-valid.db");
      const okRes = await client.post("/api/setup/test-db", {
        driver: "sqlite",
        dsn: validPath,
      });
      assertStatus(okRes, 200, "Valid sqlite test-db");
      assert.equal(okRes.data.ok, true);
      assert.equal(okRes.data.dialect, "sqlite");

      const travRes = await client.post("/api/setup/test-db", {
        driver: "sqlite",
        dsn: "../escape.db",
      });
      assertStatus(travRes, 400, "Traversal sqlite test-db");
      assert.notEqual(travRes.data?.ok, true);

      const ssrfRes = await client.post("/api/setup/test-db", {
        driver: "postgres",
        dsn: "postgres://user:pass@169.254.169.254:5432/volo?sslmode=disable",
      });
      assertSSRFRejected(ssrfRes, "Metadata IP postgres test-db");
    } finally {
      await srv.stop();
    }
  });

  it("POST /api/setup/complete rejects weak password, invalid username/email, and cross-origin requests", async () => {
    const srv = await startVoloServer({ skipSetup: true });
    try {
      const client = createClient(srv.baseUrl);

      const weakPwd = await client.post("/api/setup/complete", {
        admin_username: "validuser",
        admin_email: "admin@e2e.local",
        admin_password: "123",
      });
      assertStatus(weakPwd, 400, "Weak password");

      const badUser = await client.post("/api/setup/complete", {
        admin_username: "bad@name",
        admin_email: "admin@e2e.local",
        admin_password: "StrongPassword!1",
      });
      assertStatus(badUser, 400, "Invalid username");

      const badEmail = await client.post("/api/setup/complete", {
        admin_username: "validuser",
        admin_email: "not-an-email",
        admin_password: "StrongPassword!1",
      });
      assertStatus(badEmail, 400, "Invalid email");

      const crossOrigin = await client.post(
        "/api/setup/complete",
        {
          admin_username: "validuser",
          admin_email: "admin@e2e.local",
          admin_password: "StrongPassword!1",
        },
        { headers: { Origin: "https://evil.example" } },
      );
      assertStatus(crossOrigin, 403, "Cross-origin setup/complete");

      const stillUnlocked = await client.get("/api/setup/status");
      assertStatus(stillUnlocked, 200, "Setup still unlocked after rejected attempts");
      assert.equal(stillUnlocked.data.initialized, false);
    } finally {
      await srv.stop();
    }
  });

  it("POST /api/setup/complete initializes site, sets session cookie, seeds /about, and unlocks /", async () => {
    const srv = await startVoloServer({ skipSetup: true });
    try {
      const client = createClient(srv.baseUrl);
      const res = await client.setupComplete({
        admin_username: "wizardadmin",
        admin_email: "wizard@e2e.local",
        admin_password: "WizardPassword!123",
        admin_display_name: "Wizard Admin",
        site_title: "Wizard E2E Blog",
        default_locale: "en-US",
        accent: "ocean",
        public_url: srv.baseUrl,
      });
      assertStatus(res, 200, "POST /api/setup/complete");
      assert.equal(res.data.ok, true);
      assert.equal(res.data.target_dialect, "sqlite");
      assert.equal(res.data.admin_path, "/console");
      assert.ok(res.data.csrf_token && res.data.csrf_token.length > 0);
      assert.equal(res.data.user?.username, "wizardadmin");
      assertCookieSet(res, "__volo_sid", {
        httpOnly: true,
        sameSite: "Lax",
        nonEmpty: true,
        contextMsg: "setup/complete cookie",
      });

      const homeRes = await client.get("/");
      assertStatus(homeRes, 200, "Post-init GET /");
      assert.ok(homeRes.text.includes("Wizard E2E Blog"));
      assert.ok(homeRes.text.includes('data-accent="ocean"'));

      const aboutRes = await client.get("/about");
      assertStatus(aboutRes, 200, "Seeded /about page");

      const healthRes = await client.get("/healthz");
      assertStatus(healthRes, 200);
      assert.equal(healthRes.data.initialized, true);
    } finally {
      await srv.stop();
    }
  });

  it("Post-init lockout returns 404 for /setup and all /api/setup/* endpoints", async () => {
    const srv = await startVoloServer();
    try {
      const client = createClient(srv.baseUrl);
      assertStatus(await client.get("/setup"), 404, "Locked GET /setup");
      assertStatus(await client.get("/api/setup/status"), 404, "Locked GET /api/setup/status");
      assertStatus(
        await client.post("/api/setup/test-db", { driver: "sqlite", dsn: "x.db" }),
        404,
        "Locked POST /api/setup/test-db",
      );
      assertStatus(
        await client.post("/api/setup/configure-db", { driver: "sqlite", dsn: "x.db" }),
        404,
        "Locked POST /api/setup/configure-db",
      );
      assertStatus(
        await client.post("/api/setup/complete", {
          admin_username: "attacker",
          admin_email: "a@b.c",
          admin_password: "Password!123",
        }),
        404,
        "Locked POST /api/setup/complete",
      );
    } finally {
      await srv.stop();
    }
  });

  it("POST /api/setup/complete falls back invalid accent to forest and persists site title", async () => {
    const srv = await startVoloServer({ skipSetup: true });
    try {
      const client = createClient(srv.baseUrl);
      const res = await client.setupComplete({
        admin_username: "fallbackadmin",
        admin_email: "fb@e2e.local",
        admin_password: "FallbackPassword!123",
        site_title: "Accent Fallback Blog",
        accent: "neon-invalid",
        public_url: srv.baseUrl,
      });
      assertStatus(res, 200);
      assert.equal(res.data.ok, true);

      const homeRes = await client.get("/");
      assertStatus(homeRes, 200);
      assert.ok(homeRes.text.includes('data-accent="forest"'));
      assert.ok(homeRes.text.includes("Accent Fallback Blog"));
    } finally {
      await srv.stop();
    }
  });
});

describe("F3: Staff Auth, Session Cookies, CSRF, 2FA & PAT Scopes", () => {
  let server;
  let adminClient;

  before(async () => {
    server = await startVoloServer();
    adminClient = createClient(server.baseUrl);
    const loginRes = await adminClient.login(
      server.adminCreds.username,
      server.adminCreds.password,
    );
    assertStatus(loginRes, 200, "Admin suite login");
  });

  after(async () => {
    if (server) {
      await server.stop();
    }
  });

  it("Login accepts username or email, rejects bad password, sets HttpOnly cookie, and logout revokes session", async () => {
    const userClient = createClient(server.baseUrl);

    const badLogin = await userClient.login(server.adminCreds.username, "WrongPassword!999");
    assertStatus(badLogin, 401, "Wrong password login");

    const byUsername = await userClient.login(
      server.adminCreds.username,
      server.adminCreds.password,
    );
    assertStatus(byUsername, 200, "Login by username");
    assert.equal(byUsername.data.requires_2fa, false);
    assert.ok(byUsername.data.csrf_token && byUsername.data.csrf_token.length > 0);
    assertCookieSet(byUsername, "__volo_sid", {
      httpOnly: true,
      sameSite: "Lax",
      nonEmpty: true,
    });

    const logoutRes = await userClient.logout();
    assertStatus(logoutRes, 200, "Logout");
    assert.equal(logoutRes.data.ok, true);
    assertCookieSet(logoutRes, "__volo_sid", { deleted: true });

    const afterLogoutMe = await userClient.me(true);
    assertStatus(afterLogoutMe, 401, "GET /api/admin/me after logout");

    const byEmail = await userClient.login(
      server.adminCreds.email,
      server.adminCreds.password,
    );
    assertStatus(byEmail, 200, "Login by email");
    assert.equal(byEmail.data.user?.email, server.adminCreds.email);
    await userClient.logout();
  });

  it("GET /api/admin/me gates csrf_token on X-Volo-Console and mutating routes require X-CSRF-Token", async () => {
    const client = createClient(server.baseUrl);
    const loginRes = await client.login(
      server.adminCreds.username,
      server.adminCreds.password,
    );
    assertStatus(loginRes, 200);

    const meWithoutHeader = await client.me(false);
    assertStatus(meWithoutHeader, 200, "GET /api/admin/me without X-Volo-Console");
    assert.equal(meWithoutHeader.data.csrf_token, "");

    const meWithHeader = await client.me(true);
    assertStatus(meWithHeader, 200, "GET /api/admin/me with X-Volo-Console");
    assert.ok(meWithHeader.data.csrf_token && meWithHeader.data.csrf_token.length > 0);

    const missingCsrf = await client.patch(
      "/api/admin/settings/site",
      { subtitle: "Blocked without CSRF" },
      { omitCsrf: true },
    );
    assertStatus(missingCsrf, 403, "Mutating PATCH without X-CSRF-Token");

    const badCsrf = await client.patch(
      "/api/admin/settings/site",
      { subtitle: "Blocked wrong CSRF" },
      { headers: { "X-CSRF-Token": "invalid-csrf-token" } },
    );
    assertStatus(badCsrf, 403, "Mutating PATCH with wrong X-CSRF-Token");

    const validCsrf = await client.patch("/api/admin/settings/site", {
      subtitle: "Allowed with valid CSRF",
    });
    assertStatus(validCsrf, 200, "Mutating PATCH with valid X-CSRF-Token");
    assert.equal(validCsrf.data.value?.subtitle, "Allowed with valid CSRF");
    await client.logout();
  });

  it("Reader accounts are rejected with 401 on staff login /api/admin/auth/login", async () => {
    const createRes = await adminClient.post("/api/admin/users", {
      username: "reader_e2e_1",
      email: "reader_e2e_1@e2e.local",
      display_name: "Reader E2E",
      role: "reader",
      password: "ReaderPass!123",
    });
    assertStatus(createRes, 201, "Create reader user");
    const readerId = createRes.data.user.id;

    try {
      const readerClient = createClient(server.baseUrl);
      const loginRes = await readerClient.login("reader_e2e_1", "ReaderPass!123");
      assertStatus(loginRes, 401, "Reader blocked from staff login");
    } finally {
      await adminClient.del(`/api/admin/users/${readerId}`);
    }
  });

  it("TOTP 2FA lifecycle: setup, enroll with step-up, challenge login, replay block, recovery code, and disable", async () => {
    const createRes = await adminClient.post("/api/admin/users", {
      username: "editor_2fa_e2e",
      email: "editor_2fa@e2e.local",
      display_name: "Editor 2FA",
      role: "editor",
      password: "EditorPass!123",
      current_password: server.adminCreds.password,
    });
    assertStatus(createRes, 201, "Create editor user for 2FA");
    const editorId = createRes.data.user.id;

    try {
      const editor = createClient(server.baseUrl);
      assertStatus(await editor.login("editor_2fa_e2e", "EditorPass!123"), 200);

      const setupRes = await editor.post("/api/admin/security/totp/setup", {});
      assertStatus(setupRes, 200, "POST /api/admin/security/totp/setup");
      assertJsonHasKeys(setupRes.data, ["secret", "otpauth_uri", "qr_svg"]);
      assert.ok(setupRes.data.otpauth_uri.startsWith("otpauth://totp/"));
      assert.ok(setupRes.data.qr_svg.includes("<svg"));

      const secret = setupRes.data.secret;
      const enrollStepMinus1 = generateTOTP(secret, -1);

      const noStepUp = await editor.post("/api/admin/security/totp/enroll", {
        secret,
        code: enrollStepMinus1,
      });
      assertStatus(noStepUp, 403, "TOTP enroll without current_password");

      const enrollOk = await editor.post("/api/admin/security/totp/enroll", {
        secret,
        code: enrollStepMinus1,
        current_password: "EditorPass!123",
      });
      assertStatus(enrollOk, 200, "TOTP enroll with valid step-up");
      assert.ok(
        Array.isArray(enrollOk.data.recovery_codes) &&
          enrollOk.data.recovery_codes.length === 8,
        "Expected 8 recovery codes",
      );

      await editor.logout();

      const challenge1Res = await editor.login("editor_2fa_e2e", "EditorPass!123");
      assertStatus(challenge1Res, 200, "2FA login step 1");
      assert.equal(challenge1Res.data.requires_2fa, true);
      assert.ok(challenge1Res.data.challenge_token);
      assert.equal(editor.cookies.has("__volo_sid"), false, "No session cookie before 2FA");

      const currentStepCode = generateTOTP(secret, 0);
      const verify1 = await editor.verify2FA(
        challenge1Res.data.challenge_token,
        currentStepCode,
        false,
      );
      assertStatus(verify1, 200, "2FA verification with current TOTP step");
      assert.ok(verify1.data.csrf_token && verify1.data.csrf_token.length > 0);
      assertCookieSet(verify1, "__volo_sid", { httpOnly: true, nonEmpty: true });

      await editor.logout();

      const challenge2Res = await editor.login("editor_2fa_e2e", "EditorPass!123");
      assertStatus(challenge2Res, 200);
      assert.equal(challenge2Res.data.requires_2fa, true);

      const replayRes = await editor.verify2FA(
        challenge2Res.data.challenge_token,
        currentStepCode,
        false,
      );
      assertStatus(replayRes, 401, "Reused TOTP step blocked by replay guard");

      const recoveryRes = await editor.verify2FA(
        challenge2Res.data.challenge_token,
        enrollOk.data.recovery_codes[0],
        false,
      );
      assertStatus(recoveryRes, 200, "Recovery code 2FA login");
      assertCookieSet(recoveryRes, "__volo_sid", { httpOnly: true, nonEmpty: true });

      const disableRes = await editor.del("/api/admin/security/totp", {
        current_password: "EditorPass!123",
      });
      assertStatus(disableRes, 200, "DELETE /api/admin/security/totp");
      assert.equal(disableRes.data.ok, true);

      await editor.logout();
      const postDisableLogin = await editor.login("editor_2fa_e2e", "EditorPass!123");
      assertStatus(postDisableLogin, 200, "Login after 2FA disabled");
      assert.equal(postDisableLogin.data.requires_2fa, false);
    } finally {
      await adminClient.del(`/api/admin/users/${editorId}`);
    }
  });

  it("PAT creation, scope enforcement, interactive-session isolation, and revocation", async () => {
    const { res: createRes, plaintextToken, token } = await adminClient.createPAT(
      "e2e-articles-read",
      ["articles:read"],
      server.adminCreds.password,
      30,
    );
    assertStatus(createRes, 201, "Create PAT");
    assert.ok(plaintextToken.startsWith("volo_pat_"), "Expected volo_pat_ prefix");
    assert.ok(token?.id);

    try {
      const listRes = await adminClient.get("/api/admin/security/tokens");
      assertStatus(listRes, 200, "List PATs");
      const found = (listRes.data.items || []).find((item) => item.id === token.id);
      assert.ok(found, "Created PAT should appear in list");
      assert.equal(found.plaintext_token, undefined, "List must not leak plaintext_token");

      const patClient = adminClient.withBearer(plaintextToken);

      const readArticles = await patClient.get("/api/admin/articles");
      assertStatus(readArticles, 200, "PAT articles:read GET /api/admin/articles");

      const writeArticle = await patClient.post("/api/admin/articles", {
        title: "PAT Denied Write",
        content_md: "denied",
      });
      assertStatus(writeArticle, 403, "PAT articles:read denied POST /api/admin/articles");

      const settingsRes = await patClient.get("/api/admin/settings");
      assertStatus(settingsRes, 403, "PAT denied interactive-only /api/admin/settings");

      const logoutRes = await patClient.post("/api/admin/auth/logout", {});
      assertStatus(logoutRes, 403, "PAT denied /api/admin/auth/logout");

      const revokeRes = await adminClient.del(`/api/admin/security/tokens/${token.id}`);
      assertStatus(revokeRes, 200, "Revoke PAT");

      const afterRevoke = await patClient.get("/api/admin/articles");
      assertStatus(afterRevoke, 401, "Revoked PAT returns 401");
    } finally {
      await adminClient.del(`/api/admin/security/tokens/${token.id}`);
    }
  });

  it("Step-up password required for sensitive PATCH /api/admin/me and PAT creation", async () => {
    const createRes = await adminClient.post("/api/admin/users", {
      username: "editor_stepup_e2e",
      email: "editor_stepup@e2e.local",
      display_name: "Editor StepUp",
      role: "editor",
      password: "EditorPass!123",
      current_password: server.adminCreds.password,
    });
    assertStatus(createRes, 201);
    const editorId = createRes.data.user.id;

    try {
      const editor = createClient(server.baseUrl);
      assertStatus(await editor.login("editor_stepup_e2e", "EditorPass!123"), 200);

      const dispRes = await editor.patch("/api/admin/me", {
        display_name: "Renamed Editor",
      });
      assertStatus(dispRes, 200, "Update display_name without step-up");
      assert.equal(dispRes.data.user?.display_name, "Renamed Editor");

      const emailNoPwd = await editor.patch("/api/admin/me", {
        email: "changed@e2e.local",
      });
      assertStatus(emailNoPwd, 403, "Update email without current_password");

      const emailWrongPwd = await editor.patch("/api/admin/me", {
        email: "changed@e2e.local",
        current_password: "WrongPassword!999",
      });
      assertStatus(emailWrongPwd, 403, "Update email with wrong current_password");

      const emailOk = await editor.patch("/api/admin/me", {
        email: "changed@e2e.local",
        current_password: "EditorPass!123",
      });
      assertStatus(emailOk, 200, "Update email with valid current_password");
      assert.equal(emailOk.data.user?.email, "changed@e2e.local");

      const badPatPwd = await editor.post("/api/admin/security/tokens", {
        name: "bad-stepup",
        scopes: ["articles:read"],
        expires_in_days: 7,
        current_password: "WrongPassword!999",
      });
      assertStatus(badPatPwd, 401, "Create PAT with wrong current_password");
    } finally {
      await adminClient.del(`/api/admin/users/${editorId}`);
    }
  });
});
