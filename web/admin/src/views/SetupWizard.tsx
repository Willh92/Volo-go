import { useEffect, useState } from "preact/hooks";
import {
  accentSig,
  apiFetch,
  applyThemeAttributes,
  bootConfig,
  csrfTokenSig,
  currentUserSig,
  showToast,
  themeModeSig,
} from "../api";
import { BrandLogo } from "../components/BrandLogo";
import { ConsoleSelect, LocalePopover } from "../components/UIControls";
import {
  availableLocalesSig,
  ensureLocaleLoaded,
  LocaleCode,
  localeSig,
  normalizeLocale,
  registerExternalLocales,
  t,
  tFor,
} from "../i18n";

const ACCENTS: Array<{ id: string; swatch: string }> = [
  { id: "forest", swatch: "#246b51" },
  { id: "ocean", swatch: "#2d6583" },
  { id: "teal", swatch: "#286c70" },
  { id: "amber", swatch: "#855c27" },
  { id: "plum", swatch: "#70517d" },
  { id: "rose", swatch: "#87525f" },
];

export function SetupWizard() {
  const initLoc = normalizeLocale(bootConfig.default_locale);
  const locales = availableLocalesSig.value;
  const [tab, setTab] = useState<"fresh" | "legacy">("fresh");
  const [driver, setDriver] = useState<string>("sqlite");
  const [dsn, setDsn] = useState<string>("");
  const [simpleProto, setSimpleProto] = useState<boolean>(false);
  const [dbPingMsg, setDbPingMsg] = useState<string>("");

  const [siteTitle, setSiteTitle] = useState<string>(() => tFor(initLoc, "setup.default_site_title"));
  const [siteSubtitle, setSiteSubtitle] = useState<string>(() => tFor(initLoc, "setup.default_site_subtitle"));
  const [siteDesc, setSiteDesc] = useState<string>("");
  const [timezone, setTimezone] = useState<string>("Asia/Shanghai");
  const [accent, setAccent] = useState<string>(accentSig.value || "forest");

  const [username, setUsername] = useState<string>("admin");
  const [displayName, setDisplayName] = useState<string>(() => tFor(initLoc, "setup.default_display_name"));
  const [email, setEmail] = useState<string>("admin@example.com");
  const [password, setPassword] = useState<string>("");
  const [busy, setBusy] = useState<boolean>(false);
  const [etlResult, setEtlResult] = useState<any>(null);

  const syncLocalizedDefaults = (prev: LocaleCode, next: LocaleCode) => {
    setSiteTitle((cur) => (cur === tFor(prev, "setup.default_site_title") ? tFor(next, "setup.default_site_title") : cur));
    setSiteSubtitle((cur) => (cur === tFor(prev, "setup.default_site_subtitle") ? tFor(next, "setup.default_site_subtitle") : cur));
    setDisplayName((cur) => (cur === tFor(prev, "setup.default_display_name") ? tFor(next, "setup.default_display_name") : cur));
  };

  useEffect(() => {
    apiFetch<any>("/api/setup/status")
      .then((s) => {
        if (s.locales) registerExternalLocales(s.locales);
        if (s.current_driver) setDriver(s.current_driver);
        if (s.default_locale) {
          const prev = localeSig.value;
          const next = normalizeLocale(s.default_locale);
          void ensureLocaleLoaded(next).then(() => {
            syncLocalizedDefaults(prev, next);
          });
        }
        if (s.timezone) setTimezone(s.timezone);
        if (s.accent) {
          setAccent(s.accent);
          applyThemeAttributes(themeModeSig.value, s.accent, false);
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
      setDbPingMsg(t("setup.db_ok", { dialect: res.dialect, ms: res.latency_ms }));
    } catch (err: any) {
      setDbPingMsg(t("setup.db_err", { msg: err.message }));
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
      if (res.user) currentUserSig.value = res.user;
      if (res.csrf_token) csrfTokenSig.value = res.csrf_token;
      setEtlResult(res.report || res);
      showToast(t("setup.legacy_done"));
      const nextUrl = res.admin_path || bootConfig.admin_path || "/console";
      window.setTimeout(() => {
        window.location.href = nextUrl;
      }, 1100);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="volo-center-stage">
      <div class="volo-wizard-box">
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: "12px", marginBottom: "18px" }}>
          <div style={{ display: "flex", alignItems: "center", gap: "12px" }}>
            <BrandLogo size={42} />
            <div>
              <h1 style={{ margin: "0 0 4px", fontSize: "21px" }}>{t("setup.title")}</h1>
              <div class="volo-help">{t("setup.subtitle")}</div>
            </div>
          </div>
          <LocalePopover
            value={localeSig.value}
            locales={locales}
            onChange={(nextRaw) => {
              const prev = localeSig.value;
              const next = normalizeLocale(nextRaw);
              void ensureLocaleLoaded(next).then(() => {
                syncLocalizedDefaults(prev, next);
              });
            }}
          />
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
          <form onSubmit={submitFresh} autoComplete="off">
            <div class="volo-card" style={{ padding: "14px", marginBottom: "14px" }}>
              <div style={{ fontWeight: 650, marginBottom: "10px" }}>{t("setup.step1")}</div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("setup.dialect")}</label>
                  <ConsoleSelect
                    value={driver}
                    onChange={(v) => setDriver(v)}
                    options={[
                      { value: "sqlite", label: t("setup.dialect_sqlite") },
                      { value: "postgres", label: t("setup.dialect_postgres") },
                      { value: "mysql", label: t("setup.dialect_mysql") },
                    ]}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("setup.dsn")}</label>
                  <input
                    class="volo-input"
                    name="setup_db_dsn_str"
                    autoComplete="off"
                    value={dsn}
                    onInput={(e) => setDsn((e.currentTarget as HTMLInputElement).value)}
                    placeholder={driver === "sqlite" ? "./data/volo.db" : driver === "postgres" ? "postgres://..." : "user:pass@tcp(host:3306)/volo"}
                  />
                </div>
              </div>
              {driver === "postgres" && (
                <label style={{ display: "inline-flex", alignItems: "center", gap: "6px", fontSize: "12.5px", marginBottom: "8px" }}>
                  <input type="checkbox" checked={simpleProto} onChange={(e) => setSimpleProto((e.currentTarget as HTMLInputElement).checked)} />
                  {t("setup.simple_proto")}
                </label>
              )}
              <div style={{ display: "flex", alignItems: "center", gap: "10px", flexWrap: "wrap" }}>
                <button type="button" class="volo-btn volo-btn-sm" disabled={busy} onClick={testDB}>
                  {t("setup.test_db")}
                </button>
                {dbPingMsg && <span class="volo-badge">{dbPingMsg}</span>}
              </div>
            </div>

            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("setup.site_title")}</label>
                <input
                  class="volo-input"
                  name="setup_blog_title"
                  required
                  value={siteTitle}
                  onInput={(e) => setSiteTitle((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("setup.timezone")}</label>
                <input
                  class="volo-input"
                  name="setup_tz_iana"
                  value={timezone}
                  onInput={(e) => setTimezone((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>

            <div class="volo-field">
              <label>{t("setup.site_subtitle")}</label>
              <input
                class="volo-input"
                name="setup_blog_subtitle"
                value={siteSubtitle}
                onInput={(e) => setSiteSubtitle((e.currentTarget as HTMLInputElement).value)}
              />
            </div>

            <div class="volo-field">
              <label>{t("setup.studio_accent")}</label>
              <div style={{ display: "flex", alignItems: "center", gap: "10px", flexWrap: "wrap", paddingTop: "2px" }}>
                {ACCENTS.map((ac) => {
                  const isAct = accent === ac.id;
                  return (
                    <button
                      key={ac.id}
                      type="button"
                      class={`volo-accent-dot-btn ${isAct ? "active" : ""}`}
                      style={{ background: ac.swatch }}
                      title={t(`accent.${ac.id}`)}
                      aria-label={t(`accent.${ac.id}`)}
                      onClick={() => {
                        setAccent(ac.id);
                        applyThemeAttributes(themeModeSig.value, ac.id, true);
                      }}
                    >
                      {isAct && <span>✓</span>}
                    </button>
                  );
                })}
                <span class="volo-help" style={{ marginLeft: "4px" }}>{t(`accent.${accent}`)}</span>
              </div>
            </div>

            <hr style={{ border: 0, borderTop: "1px solid var(--volo-line)", margin: "16px 0" }} />
            <div style={{ fontWeight: 650, marginBottom: "10px" }}>{t("setup.step3")}</div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("setup.admin_username")}</label>
                <input
                  class="volo-input"
                  name="setup_owner_handle"
                  autoComplete="off"
                  required
                  value={username}
                  onInput={(e) => setUsername((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("setup.display_name")}</label>
                <input
                  class="volo-input"
                  name="setup_owner_nick"
                  autoComplete="off"
                  value={displayName}
                  onInput={(e) => setDisplayName((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("setup.admin_email")}</label>
                <input
                  type="email"
                  class="volo-input"
                  name="setup_owner_email"
                  autoComplete="off"
                  required
                  value={email}
                  onInput={(e) => setEmail((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("setup.admin_password")}</label>
                <input
                  type="password"
                  class="volo-input"
                  name="setup_owner_new_pwd"
                  autoComplete="new-password"
                  required
                  minLength={6}
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
              {t("setup.legacy_desc")}
            </p>
            <label class="volo-btn volo-btn-primary" style={{ width: "100%", cursor: "pointer" }}>
              {busy ? t("setup.importing") : t("setup.import_btn")}
              <input
                type="file"
                accept=".zip,.json"
                style={{ display: "none" }}
                disabled={busy}
                onChange={(e) => {
                  const f = (e.currentTarget as HTMLInputElement).files?.[0];
                  if (f) void handleLegacyUpload(f);
                  (e.currentTarget as HTMLInputElement).value = "";
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
