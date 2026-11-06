import { useEffect, useState } from "preact/hooks";
import { accentSig, apiFetch, applyThemeAttributes, bootConfig, showToast, themeModeSig } from "../api";
import { localeSig, normalizeLocale, t } from "../i18n";

const ACCENTS = ["forest", "ocean", "teal", "amber", "plum", "rose"] as const;

export function SetupWizard() {
  const [tab, setTab] = useState<"fresh" | "legacy">("fresh");
  const [driver, setDriver] = useState<string>("sqlite");
  const [dsn, setDsn] = useState<string>("");
  const [simpleProto, setSimpleProto] = useState<boolean>(false);
  const [dbPingMsg, setDbPingMsg] = useState<string>("");

  const [siteTitle, setSiteTitle] = useState<string>("Volo Notes");
  const [siteSubtitle, setSiteSubtitle] = useState<string>("Independent thoughts & engineering craft");
  const [siteDesc, setSiteDesc] = useState<string>("");
  const [timezone, setTimezone] = useState<string>("Asia/Shanghai");
  const [accent, setAccent] = useState<string>(accentSig.value || "forest");

  const [username, setUsername] = useState<string>("admin");
  const [displayName, setDisplayName] = useState<string>("Site Owner");
  const [email, setEmail] = useState<string>("admin@example.com");
  const [password, setPassword] = useState<string>("");
  const [busy, setBusy] = useState<boolean>(false);
  const [etlResult, setEtlResult] = useState<any>(null);

  useEffect(() => {
    apiFetch<any>("/api/setup/status")
      .then((s) => {
        if (s.current_driver) setDriver(s.current_driver);
        if (s.default_locale) localeSig.value = normalizeLocale(s.default_locale);
        if (s.timezone) setTimezone(s.timezone);
        if (s.accent) {
          setAccent(s.accent);
          applyThemeAttributes(themeModeSig.value, s.accent);
        }
      })
      .catch(() => {});
  }, []);

  const testDB = async () => {
    setBusy(true);
    setDbPingMsg("");
    try {
      const res = await apiFetch<{ dialect: string; latency_ms: number }>("/api/setup/test-db", {
        method: "POST",
        body: JSON.stringify({ driver, dsn, simple_protocol: simpleProto }),
      });
      setDbPingMsg(`OK (${res.dialect}, ${res.latency_ms} ms)`);
    } catch (err: any) {
      setDbPingMsg(`Error: ${err.message}`);
    } finally {
      setBusy(false);
    }
  };

  const submitFresh = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    try {
      const res = await apiFetch<{ admin_path: string }>("/api/setup/complete", {
        method: "POST",
        body: JSON.stringify({
          site_title: siteTitle,
          site_subtitle: siteSubtitle,
          site_description: siteDesc,
          default_locale: localeSig.value,
          timezone,
          accent,
          admin_username: username,
          admin_email: email,
          admin_display_name: displayName,
          admin_password: password,
        }),
      });
      window.location.href = res.admin_path || bootConfig.admin_path || "/console";
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setBusy(false);
    }
  };

  const handleLegacyUpload = async (file: File) => {
    setBusy(true);
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<any>("/api/setup/import-legacy", {
        method: "POST",
        body: fd,
      });
      setEtlResult(res.report || res);
      showToast("Legacy backup normalized & imported!");
      if (res.admin_path) {
        window.setTimeout(() => {
          window.location.href = res.admin_path;
        }, 1200);
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="volo-center-stage">
      <div class="volo-wizard-box">
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", marginBottom: "18px" }}>
          <div>
            <h1 style={{ margin: "0 0 4px", fontSize: "22px" }}>{t("setup.title")}</h1>
            <div class="volo-help">{t("setup.subtitle")}</div>
          </div>
          <select
            class="volo-select"
            style={{ width: "120px" }}
            value={localeSig.value}
            onChange={(e) => {
              localeSig.value = normalizeLocale((e.currentTarget as HTMLSelectElement).value);
            }}
          >
            <option value="zh-CN">简体中文</option>
            <option value="en-US">English</option>
          </select>
        </div>

        <div class="volo-subtabs">
          <button type="button" class={`volo-tab ${tab === "fresh" ? "active" : ""}`} onClick={() => setTab("fresh")}>
            {t("setup.step2")}
          </button>
          <button type="button" class={`volo-tab ${tab === "legacy" ? "active" : ""}`} onClick={() => setTab("legacy")}>
            {t("setup.legacy_tab")}
          </button>
        </div>

        {tab === "fresh" ? (
          <form onSubmit={submitFresh}>
            <div class="volo-card" style={{ padding: "14px", marginBottom: "14px" }}>
              <div style={{ fontWeight: 650, marginBottom: "10px" }}>{t("setup.step1")}</div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>Dialect</label>
                  <select class="volo-select" value={driver} onChange={(e) => setDriver((e.currentTarget as HTMLSelectElement).value)}>
                    <option value="sqlite">SQLite (Pure-Go WAL)</option>
                    <option value="postgres">PostgreSQL / Supabase / Neon / CockroachDB</option>
                    <option value="mysql">MySQL 8+ / MariaDB / TiDB</option>
                  </select>
                </div>
                <div class="volo-field">
                  <label>DSN (blank = active runtime DB)</label>
                  <input
                    class="volo-input"
                    value={dsn}
                    onInput={(e) => setDsn((e.currentTarget as HTMLInputElement).value)}
                    placeholder={driver === "sqlite" ? "./data/volo.db" : driver === "postgres" ? "postgres://..." : "user:pass@tcp(host:3306)/volo"}
                  />
                </div>
              </div>
              {driver === "postgres" && (
                <label style={{ display: "inline-flex", alignItems: "center", gap: "6px", fontSize: "12.5px", marginBottom: "8px" }}>
                  <input type="checkbox" checked={simpleProto} onChange={(e) => setSimpleProto((e.currentTarget as HTMLInputElement).checked)} />
                  Simple Protocol (PgBouncer / Pooler mode)
                </label>
              )}
              <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
                <button type="button" class="volo-btn volo-btn-sm" disabled={busy} onClick={testDB}>
                  {t("setup.test_db")}
                </button>
                {dbPingMsg && <span class="volo-badge">{dbPingMsg}</span>}
              </div>
            </div>

            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Site Title</label>
                <input class="volo-input" required value={siteTitle} onInput={(e) => setSiteTitle((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Timezone</label>
                <input class="volo-input" value={timezone} onInput={(e) => setTimezone((e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>

            <div class="volo-field">
              <label>Subtitle</label>
              <input class="volo-input" value={siteSubtitle} onInput={(e) => setSiteSubtitle((e.currentTarget as HTMLInputElement).value)} />
            </div>

            <div class="volo-field">
              <label>Studio Accent</label>
              <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
                {ACCENTS.map((ac) => (
                  <button
                    key={ac}
                    type="button"
                    class={`volo-btn volo-btn-sm ${accent === ac ? "volo-btn-primary" : ""}`}
                    onClick={() => {
                      setAccent(ac);
                      applyThemeAttributes(themeModeSig.value, ac);
                    }}
                  >
                    {ac}
                  </button>
                ))}
              </div>
            </div>

            <hr style={{ border: 0, borderTop: "1px solid var(--volo-line)", margin: "16px 0" }} />
            <div style={{ fontWeight: 650, marginBottom: "10px" }}>{t("setup.step3")}</div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Admin Username</label>
                <input class="volo-input" required value={username} onInput={(e) => setUsername((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Display Name</label>
                <input class="volo-input" value={displayName} onInput={(e) => setDisplayName((e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Admin Email</label>
                <input type="email" class="volo-input" required value={email} onInput={(e) => setEmail((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Password (min 10 chars)</label>
                <input
                  type="password"
                  class="volo-input"
                  required
                  minLength={10}
                  value={password}
                  onInput={(e) => setPassword((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>
            <button type="submit" class="volo-btn volo-btn-primary" style={{ width: "100%", marginTop: "8px" }} disabled={busy}>
              {t("setup.launch")}
            </button>
          </form>
        ) : (
          <div>
            <p class="volo-help" style={{ marginBottom: "14px" }}>
              Upload a legacy 15-table archive (.zip or .json) to stream-normalize articles, direct categories, standalone pages, series, bookshelf, analytics, webhooks, and 301 historical permalinks without loading the archive into RAM.
            </p>
            <label class="volo-btn volo-btn-primary" style={{ width: "100%", cursor: "pointer" }}>
              {busy ? "Streaming ETL..." : t("setup.import_btn")}
              <input
                type="file"
                accept=".zip,.json"
                style={{ display: "none" }}
                disabled={busy}
                onChange={(e) => {
                  const f = (e.currentTarget as HTMLInputElement).files?.[0];
                  if (f) void handleLegacyUpload(f);
                }}
              />
            </label>
            {etlResult && (
              <pre class="volo-card" style={{ marginTop: "14px", fontSize: "12px", overflowX: "auto" }}>
                {JSON.stringify(etlResult, null, 2)}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
