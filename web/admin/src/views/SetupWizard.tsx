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
import { ConsoleSelect, LocalePopover, SwitchToggle, UploadButton } from "../components/UIControls";
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
  const [dbPingErr, setDbPingErr] = useState<boolean>(false);

  const [siteTitle, setSiteTitle] = useState<string>(() => tFor(initLoc, "setup.default_site_title"));
  const [siteSubtitle, setSiteSubtitle] = useState<string>(() => tFor(initLoc, "setup.default_site_subtitle"));
  const [siteDesc] = useState<string>("");
  const [timezone, setTimezone] = useState<string>("Asia/Shanghai");
  const [accent, setAccent] = useState<string>(accentSig.value || "forest");

  const [username, setUsername] = useState<string>("");
  const [displayName, setDisplayName] = useState<string>(() => tFor(initLoc, "setup.default_display_name"));
  const [email, setEmail] = useState<string>("");
  const [password, setPassword] = useState<string>("");

  // Legacy migration adjustment & URL replacement state
  const [oldSiteUrl, setOldSiteUrl] = useState<string>("");
  const [newSiteUrl, setNewSiteUrl] = useState<string>("");
  const [legSiteTitle, setLegSiteTitle] = useState<string>("");
  const [legSiteSubtitle, setLegSiteSubtitle] = useState<string>("");
  const [legTimezone, setLegTimezone] = useState<string>("");
  const [storageMode, setStorageMode] = useState<string>("keep");
  const [legAdminUser, setLegAdminUser] = useState<string>("");
  const [legAdminEmail, setLegAdminEmail] = useState<string>("");
  const [legAdminPwd, setLegAdminPwd] = useState<string>("");

  const [busy, setBusy] = useState<boolean>(false);
  const [dryRun, setDryRun] = useState<boolean>(false);
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
      setDbPingErr(false);
      setDbPingMsg(t("setup.db_ok", { dialect: res.dialect, ms: res.latency_ms }));
    } catch (err: any) {
      setDbPingErr(true);
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
          driver,
          dsn,
          simple_protocol: simpleProto,
          db_driver: driver,
          db_dsn: dsn,
          db_simple_protocol: simpleProto,
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
      showToast(err.message, "err");
    } finally {
      setBusy(false);
    }
  };

  const handleLegacyUpload = async (file: File) => {
    setBusy(true);
    try {
      const fd = new FormData();
      fd.append("file", file);
      fd.append("driver", driver);
      fd.append("dsn", dsn);
      fd.append("simple_protocol", simpleProto ? "1" : "0");
      if (oldSiteUrl.trim()) fd.append("old_site_url", oldSiteUrl.trim());
      if (newSiteUrl.trim()) fd.append("new_site_url", newSiteUrl.trim());
      if (legSiteTitle.trim()) fd.append("site_title", legSiteTitle.trim());
      if (legSiteSubtitle.trim()) fd.append("site_subtitle", legSiteSubtitle.trim());
      if (legTimezone.trim()) fd.append("timezone", legTimezone.trim());
      fd.append("storage_mode", storageMode);
      if (legAdminUser.trim()) fd.append("admin_username", legAdminUser.trim());
      if (legAdminEmail.trim()) fd.append("admin_email", legAdminEmail.trim());
      if (legAdminPwd) fd.append("admin_password", legAdminPwd);

      const url = dryRun ? "/api/setup/import-legacy?dry_run=1" : "/api/setup/import-legacy";
      const res = await apiFetch<any>(url, {
        method: "POST",
        body: fd,
      });
      setEtlResult(res.report || res);
      if (dryRun) {
        showToast(t("sys.import_dryrun_ok"), "ok");
        return;
      }
      if (res.user) currentUserSig.value = res.user;
      if (res.csrf_token) csrfTokenSig.value = res.csrf_token;
      showToast(t("setup.legacy_done"), "ok");
      const nextUrl = res.admin_path || bootConfig.admin_path || "/console";
      window.setTimeout(() => {
        window.location.href = nextUrl;
      }, 1100);
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setBusy(false);
    }
  };

  const dbSelectorCard = (
    <div class="volo-subcard" style={{ marginTop: 0 }}>
      <div class="volo-subcard-title">{t("setup.step1")}</div>
      <div class="volo-grid-2">
        <div class="volo-field">
          <label>{t("setup.dialect")}</label>
          <ConsoleSelect
            value={driver}
            onChange={(v) => {
              setDriver(v);
              setDbPingMsg("");
            }}
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
            placeholder={
              driver === "sqlite"
                ? "./data/volo.db"
                : driver === "postgres"
                ? "postgres://user:pass@host:5432/volo?sslmode=disable"
                : "user:pass@tcp(host:3306)/volo"
            }
          />
        </div>
      </div>
      {driver === "postgres" && (
        <div style={{ marginBottom: "8px" }}>
          <SwitchToggle
            checked={simpleProto}
            onChange={setSimpleProto}
            label={t("setup.simple_proto")}
          />
        </div>
      )}
      <div class="volo-form-actions no-border" style={{ justifyContent: "flex-end", marginTop: "6px" }}>
        {dbPingMsg && (
          <span class={`volo-badge ${dbPingErr ? "volo-badge-danger" : "volo-badge-ok"}`} style={{ marginRight: "auto" }}>
            {dbPingMsg}
          </span>
        )}
        <button type="button" class="volo-btn volo-btn-sm" disabled={busy} onClick={testDB}>
          {t("setup.test_db")}
        </button>
      </div>
    </div>
  );

  return (
    <div class="volo-center-stage is-setup">
      <div class="volo-wizard-box">
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: "12px", flexWrap: "wrap", marginBottom: "18px" }}>
          <div style={{ display: "flex", alignItems: "center", gap: "12px", minWidth: 0 }}>
            <BrandLogo size={42} />
            <div style={{ minWidth: 0 }}>
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
          <form onSubmit={submitFresh} autoComplete="off" data-form-type="other">
            {dbSelectorCard}

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
                        applyThemeAttributes(themeModeSig.value, ac.id, false);
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
                  data-lpignore="true"
                  data-1p-ignore="true"
                  placeholder="admin"
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
                  data-lpignore="true"
                  data-1p-ignore="true"
                  placeholder="admin@example.com"
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
                  data-lpignore="true"
                  data-1p-ignore="true"
                  required
                  minLength={6}
                  value={password}
                  onInput={(e) => setPassword((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>
            <div class="volo-form-actions">
              <button type="submit" class="volo-btn volo-btn-primary" disabled={busy}>
                {t("setup.launch")}
              </button>
            </div>
          </form>
        ) : (
          <div>
            <p class="volo-help" style={{ marginBottom: "12px" }}>
              {t("setup.legacy_desc")}
            </p>

            {dbSelectorCard}

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("setup.leg_adjust_head")}</div>
              <div class="volo-help" style={{ marginBottom: "10px" }}>{t("setup.leg_adjust_hint")}</div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("setup.leg_old_url")}</label>
                  <input
                    class="volo-input"
                    autoComplete="off"
                    value={oldSiteUrl}
                    placeholder={t("setup.leg_old_url_ph")}
                    onInput={(e) => setOldSiteUrl((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("setup.leg_new_url")}</label>
                  <input
                    class="volo-input"
                    autoComplete="off"
                    value={newSiteUrl}
                    placeholder={t("setup.leg_new_url_ph")}
                    onInput={(e) => setNewSiteUrl((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("setup.leg_site_title")}</label>
                  <input
                    class="volo-input"
                    autoComplete="off"
                    value={legSiteTitle}
                    placeholder={t("setup.leg_keep_ph")}
                    onInput={(e) => setLegSiteTitle((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("setup.leg_site_sub")}</label>
                  <input
                    class="volo-input"
                    autoComplete="off"
                    value={legSiteSubtitle}
                    placeholder={t("setup.leg_keep_ph")}
                    onInput={(e) => setLegSiteSubtitle((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("setup.leg_storage")}</label>
                  <ConsoleSelect
                    value={storageMode}
                    onChange={setStorageMode}
                    options={[
                      { value: "keep", label: t("setup.leg_storage_keep") },
                      { value: "local", label: t("setup.leg_storage_local") },
                    ]}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("setup.timezone")}</label>
                  <input
                    class="volo-input"
                    autoComplete="off"
                    value={legTimezone}
                    placeholder={timezone || "Asia/Shanghai"}
                    onInput={(e) => setLegTimezone((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("setup.leg_admin_head")}</div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("setup.leg_admin_user")}</label>
                  <input
                    class="volo-input"
                    autoComplete="off"
                    data-lpignore="true"
                    data-1p-ignore="true"
                    value={legAdminUser}
                    placeholder={t("setup.leg_keep_ph")}
                    onInput={(e) => setLegAdminUser((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("setup.leg_admin_email")}</label>
                  <input
                    type="email"
                    class="volo-input"
                    autoComplete="off"
                    data-lpignore="true"
                    data-1p-ignore="true"
                    value={legAdminEmail}
                    placeholder={t("setup.leg_keep_ph")}
                    onInput={(e) => setLegAdminEmail((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("setup.leg_admin_pwd")}</label>
                  <input
                    type="password"
                    class="volo-input"
                    autoComplete="new-password"
                    data-lpignore="true"
                    data-1p-ignore="true"
                    minLength={6}
                    value={legAdminPwd}
                    placeholder={t("setup.leg_keep_ph")}
                    onInput={(e) => setLegAdminPwd((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
            </div>

            <div style={{ margin: "12px 0" }}>
              <SwitchToggle
                checked={dryRun}
                onChange={setDryRun}
                label={t("sys.import_dryrun_lbl")}
              />
            </div>
            <UploadButton
              accept=".zip,.json"
              busy={busy}
              busyLabel={t("setup.importing")}
              label={t("setup.import_btn")}
              primary
              small={false}
              className="volo-w-full"
              onFile={(f) => void handleLegacyUpload(f)}
            />
            {etlResult && (
              <pre class="volo-subcard" style={{ marginTop: "14px", fontSize: "12px", overflowX: "auto" }}>
                {JSON.stringify(etlResult, null, 2)}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
