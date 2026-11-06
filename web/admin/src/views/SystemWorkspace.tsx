import { useEffect, useState } from "preact/hooks";
import { apiFetch, b64urlToBytes, bytesToB64url, currentUserSig, refreshMe, showToast } from "../api";
import { formatDateTime, t } from "../i18n";

type SysTab = "account" | "settings" | "users" | "waf" | "backup" | "audit";

const PAT_SCOPES = [
  "articles:read",
  "articles:write",
  "pages:write",
  "media:write",
  "backup:read",
] as const;

export function SystemWorkspace() {
  const isAdmin = currentUserSig.value?.role === "admin";
  const [sub, setSub] = useState<SysTab>(isAdmin ? "settings" : "account");

  // Account Security state
  const [meInfo, setMeInfo] = useState<any>(null);
  const [profileForm, setProfileForm] = useState({ display_name: "", bio: "", website_url: "", current_password: "", new_password: "" });
  const [totpEnroll, setTotpEnroll] = useState<{ secret: string; otpauth_uri: string } | null>(null);
  const [totpCode, setTotpCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [passkeys, setPasskeys] = useState<any[]>([]);
  const [pkName, setPkName] = useState("My Laptop");
  const [sessions, setSessions] = useState<any[]>([]);
  const [tokens, setTokens] = useState<any[]>([]);
  const [patName, setPatName] = useState("Obsidian CLI");
  const [patScopes, setPatScopes] = useState<string[]>(["articles:read", "articles:write", "media:write"]);
  const [issuedPAT, setIssuedPAT] = useState<string>("");

  // Atomic Domain Settings state (`domains` + `versions`)
  const [domains, setDomains] = useState<Record<string, any> | null>(null);
  const [versions, setVersions] = useState<Record<string, number>>({});
  const [smtpPwd, setSmtpPwd] = useState("");
  const [testMailTo, setTestMailTo] = useState("");
  const [s3Secret, setS3Secret] = useState("");

  // Users / WAF / Backup / Audit
  const [users, setUsers] = useState<any[]>([]);
  const [userForm, setUserForm] = useState({ username: "", email: "", display_name: "", role: "editor", password: "" });
  const [bans, setBans] = useState<any[]>([]);
  const [banForm, setBanForm] = useState({ cidr: "", note: "manual_ban", expires_hours: 24 });
  const [geoRules, setGeoRules] = useState<any[]>([]);
  const [geoForm, setGeoForm] = useState({ name: "Geo Rule", action: "deny", countries: "", regions: "", sort_order: 10 });
  const [doctorReport, setDoctorReport] = useState<any>(null);
  const [auditLogs, setAuditLogs] = useState<any[]>([]);

  const loadSub = async () => {
    try {
      if (sub === "account") {
        const [me, pks, sess, tks] = await Promise.all([
          apiFetch<any>("/api/admin/me"),
          apiFetch<{ items: any[] }>("/api/admin/security/passkeys"),
          apiFetch<{ items: any[] }>("/api/admin/security/sessions"),
          apiFetch<{ items: any[] }>("/api/admin/security/tokens"),
        ]);
        setMeInfo(me);
        setProfileForm({
          display_name: me.user?.display_name || "",
          bio: me.user?.bio || "",
          website_url: me.user?.website_url || "",
          current_password: "",
          new_password: "",
        });
        setPasskeys(pks.items || []);
        setSessions(sess.items || []);
        setTokens(tks.items || []);
      } else if (sub === "settings" && isAdmin) {
        const res = await apiFetch<any>("/api/admin/settings");
        setDomains(res.domains || {});
        setVersions(res.versions || {});
      } else if (sub === "users" && isAdmin) {
        const res = await apiFetch<{ items: any[] }>("/api/admin/users");
        setUsers(res.items || []);
      } else if (sub === "waf" && isAdmin) {
        const [b, g] = await Promise.all([
          apiFetch<{ items: any[] }>("/api/admin/security/ip-bans"),
          apiFetch<{ items: any[] }>("/api/admin/security/geo-rules"),
        ]);
        setBans(b.items || []);
        setGeoRules(g.items || []);
      } else if (sub === "backup" && isAdmin) {
        const doc = await apiFetch<any>("/api/admin/doctor");
        setDoctorReport(doc);
      } else if (sub === "audit" && isAdmin) {
        const logs = await apiFetch<{ items: any[] }>("/api/admin/audit-logs");
        setAuditLogs(logs.items || []);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    void loadSub();
  }, [sub]);

  const patchDomain = async (domainName: string, bodyObj: Record<string, any>) => {
    try {
      const ver = versions[domainName] ?? 0;
      const res = await apiFetch<any>(`/api/admin/settings/${domainName}`, {
        method: "PATCH",
        body: JSON.stringify({ expected_version: ver, ...bodyObj }),
      });
      setDomains((prev) => ({ ...(prev || {}), [domainName]: res.value }));
      setVersions((prev) => ({ ...prev, [domainName]: res.version }));
      showToast(t("common.saved"));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const registerPasskey = async () => {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      showToast("WebAuthn requires HTTPS or localhost.");
      return;
    }
    try {
      const begin = await apiFetch<any>("/api/admin/security/passkeys/begin", { method: "POST", body: "{}" });
      const cred = (await navigator.credentials.create({
        publicKey: {
          challenge: b64urlToBytes(begin.challenge),
          rp: { name: "Volo-go", id: begin.rp_id },
          user: {
            id: new TextEncoder().encode(currentUserSig.value?.id || "staff"),
            name: currentUserSig.value?.username || "admin",
            displayName: currentUserSig.value?.display_name || "Admin",
          },
          pubKeyCredParams: [
            { type: "public-key", alg: -7 },
            { type: "public-key", alg: -257 },
          ],
          authenticatorSelection: { userVerification: "preferred" },
          timeout: 60000,
        },
      })) as PublicKeyCredential | null;
      if (!cred) return;
      const resp = cred.response as AuthenticatorAttestationResponse;
      await apiFetch("/api/admin/security/passkeys/finish", {
        method: "POST",
        body: JSON.stringify({
          state_token: begin.state_token,
          name: pkName || "Passkey",
          client_data_json: bytesToB64url(resp.clientDataJSON),
          attestation_object: bytesToB64url(resp.attestationObject),
        }),
      });
      showToast("Passkey registered!");
      void loadSub();
    } catch (err: any) {
      showToast(err?.message || "Passkey enrollment cancelled");
    }
  };

  const tabs: SysTab[] = isAdmin
    ? ["settings", "account", "users", "waf", "backup", "audit"]
    : ["account"];

  const setDomField = (dom: string, field: string, val: any) => {
    setDomains((prev) => ({
      ...(prev || {}),
      [dom]: { ...(prev?.[dom] || {}), [field]: val },
    }));
  };

  return (
    <div>
      <div class="volo-subtabs">
        {tabs.map((k) => (
          <button key={k} type="button" class={`volo-tab ${sub === k ? "active" : ""}`} onClick={() => setSub(k)}>
            {t(`sys.${k}`)}
          </button>
        ))}
      </div>

      {sub === "settings" && isAdmin && domains && (
        <div class="volo-grid-2">
          {/* Site Domain */}
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>1. Site Identity (`PATCH /settings/site` v{versions.site ?? 0})</h3>
            <div class="volo-field">
              <label>Title & Subtitle</label>
              <input class="volo-input" value={domains.site?.title || ""} onInput={(e) => setDomField("site", "title", (e.currentTarget as HTMLInputElement).value)} />
              <input class="volo-input" style={{ marginTop: "6px" }} value={domains.site?.subtitle || ""} onInput={(e) => setDomField("site", "subtitle", (e.currentTarget as HTMLInputElement).value)} />
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Default Locale</label>
                <select class="volo-select" value={domains.site?.default_locale || "zh-CN"} onChange={(e) => setDomField("site", "default_locale", (e.currentTarget as HTMLSelectElement).value)}>
                  <option value="zh-CN">zh-CN</option>
                  <option value="en-US">en-US</option>
                </select>
              </div>
              <div class="volo-field">
                <label>Timezone</label>
                <input class="volo-input" value={domains.site?.timezone || "Asia/Shanghai"} onInput={(e) => setDomField("site", "timezone", (e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <div class="volo-field">
              <label>Global Copyright / License Template</label>
              <input class="volo-input" value={domains.site?.license_template || ""} onInput={(e) => setDomField("site", "license_template", (e.currentTarget as HTMLInputElement).value)} />
            </div>
            <button type="button" class="volo-btn volo-btn-primary" onClick={() => patchDomain("site", domains.site || {})}>
              Save Site Domain
            </button>
          </div>

          {/* Reading & Anti-Spam Domain */}
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>2. Reading & PoW (`PATCH /settings/reading` v{versions.reading ?? 0})</h3>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Posts Per Page</label>
                <input type="number" class="volo-input" value={domains.reading?.posts_per_page ?? 15} onInput={(e) => setDomField("reading", "posts_per_page", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
              <div class="volo-field">
                <label>Stateless PoW Difficulty Bits</label>
                <input type="number" class="volo-input" value={domains.reading?.pow_difficulty_bits ?? 16} onInput={(e) => setDomField("reading", "pow_difficulty_bits", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: "7px", marginBottom: "12px" }}>
              <label><input type="checkbox" checked={domains.reading?.allow_guest_comment !== false} onChange={(e) => setDomField("reading", "allow_guest_comment", (e.currentTarget as HTMLInputElement).checked)} /> Allow Persistent Guest Comments</label>
              <label><input type="checkbox" checked={!!domains.reading?.allow_reader_register} onChange={(e) => setDomField("reading", "allow_reader_register", (e.currentTarget as HTMLInputElement).checked)} /> Allow Reader Email Registration</label>
              <label><input type="checkbox" checked={domains.reading?.enable_instant_search !== false} onChange={(e) => setDomField("reading", "enable_instant_search", (e.currentTarget as HTMLInputElement).checked)} /> Enable Two-Stage Ctrl/Cmd+K Search</label>
              <label><input type="checkbox" checked={domains.reading?.enable_pjax !== false} onChange={(e) => setDomField("reading", "enable_pjax", (e.currentTarget as HTMLInputElement).checked)} /> Enable Seamless PJAX Navigation</label>
            </div>
            <button type="button" class="volo-btn volo-btn-primary" onClick={() => patchDomain("reading", domains.reading || {})}>
              Save Reading Domain
            </button>
          </div>

          {/* Notifications SMTP Domain */}
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>3. Strict-TLS SMTP (`PATCH /settings/notifications` v{versions.notifications ?? 0})</h3>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>SMTP Host</label>
                <input class="volo-input" value={domains.notifications?.smtp_host || ""} onInput={(e) => setDomField("notifications", "smtp_host", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Port</label>
                <input type="number" class="volo-input" value={domains.notifications?.smtp_port ?? 465} onInput={(e) => setDomField("notifications", "smtp_port", Number((e.currentTarget as HTMLInputElement).value))} />
              </div>
              <div class="volo-field">
                <label>TLS Mode</label>
                <select class="volo-select" value={domains.notifications?.smtp_tls_mode || "tls"} onChange={(e) => setDomField("notifications", "smtp_tls_mode", (e.currentTarget as HTMLSelectElement).value)}>
                  <option value="tls">Implicit TLS (465)</option>
                  <option value="starttls">STARTTLS (587)</option>
                  <option value="plain_internal">Plain Internal Relay Only</option>
                </select>
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Username / From Address</label>
                <input class="volo-input" placeholder="SMTP Username" value={domains.notifications?.smtp_username || ""} onInput={(e) => setDomField("notifications", "smtp_username", (e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" style={{ marginTop: "6px" }} placeholder="From <noreply@example.com>" value={domains.notifications?.from_address || ""} onInput={(e) => setDomField("notifications", "from_address", (e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>
                  SMTP Password {domains.notifications?.smtp_password_set && <span class="volo-badge volo-badge-ok">Sealed</span>}
                </label>
                <input type="password" class="volo-input" placeholder="Leave blank to keep existing" value={smtpPwd} onInput={(e) => setSmtpPwd((e.currentTarget as HTMLInputElement).value)} />
                <input class="volo-input" style={{ marginTop: "6px" }} placeholder="Test recipient email" value={testMailTo} onInput={(e) => setTestMailTo((e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <label style={{ display: "inline-flex", gap: "6px", marginBottom: "10px" }}>
              <input type="checkbox" checked={!!domains.notifications?.smtp_enabled} onChange={(e) => setDomField("notifications", "smtp_enabled", (e.currentTarget as HTMLInputElement).checked)} />
              Enable SMTP Mail Delivery
            </label>
            <div style={{ display: "flex", gap: "8px" }}>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={() => {
                  const body: any = { ...(domains.notifications || {}) };
                  if (smtpPwd) body.smtp_password = smtpPwd;
                  void patchDomain("notifications", body);
                  setSmtpPwd("");
                }}
              >
                Save Notifications
              </button>
              <button
                type="button"
                class="volo-btn"
                onClick={async () => {
                  try {
                    await apiFetch("/api/admin/settings/notifications/test-smtp", {
                      method: "POST",
                      body: JSON.stringify({ to_email: testMailTo, settings: domains.notifications, smtp_password: smtpPwd }),
                    });
                    showToast("SMTP test email sent!");
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              >
                Test SMTP
              </button>
            </div>
          </div>

          {/* Storage & S3 Domain */}
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>4. Media Storage (`PATCH /settings/storage` v{versions.storage ?? 0})</h3>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>Active Driver</label>
                <select class="volo-select" value={domains.storage?.active_driver || "local"} onChange={(e) => setDomField("storage", "active_driver", (e.currentTarget as HTMLSelectElement).value)}>
                  <option value="local">Local Disk (`./uploads`)</option>
                  <option value="s3">S3 / Cloudflare R2 / MinIO (SigV4)</option>
                </select>
              </div>
              <div class="volo-field">
                <label>S3 Endpoint</label>
                <input class="volo-input" placeholder="https://s3.amazonaws.com" value={domains.storage?.s3_endpoint || ""} onInput={(e) => setDomField("storage", "s3_endpoint", (e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>
            <div class="volo-grid-3">
              <input class="volo-input" placeholder="Bucket" value={domains.storage?.s3_bucket || ""} onInput={(e) => setDomField("storage", "s3_bucket", (e.currentTarget as HTMLInputElement).value)} />
              <input class="volo-input" placeholder="Access Key" value={domains.storage?.s3_access_key || ""} onInput={(e) => setDomField("storage", "s3_access_key", (e.currentTarget as HTMLInputElement).value)} />
              <input type="password" class="volo-input" placeholder={domains.storage?.s3_secret_key_set ? "Secret Sealed (blank=keep)" : "Secret Key"} value={s3Secret} onInput={(e) => setS3Secret((e.currentTarget as HTMLInputElement).value)} />
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "12px" }}>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={() => {
                  const body: any = { ...(domains.storage || {}) };
                  if (s3Secret) body.s3_secret_key = s3Secret;
                  void patchDomain("storage", body);
                  setS3Secret("");
                }}
              >
                Save Storage Domain
              </button>
              <button
                type="button"
                class="volo-btn"
                onClick={async () => {
                  try {
                    await apiFetch("/api/admin/settings/storage/test", {
                      method: "POST",
                      body: JSON.stringify({ settings: domains.storage, s3_secret_key: s3Secret }),
                    });
                    showToast("Storage write/read check OK!");
                  } catch (err: any) {
                    showToast(err.message);
                  }
                }}
              >
                Test Storage
              </button>
            </div>
          </div>
        </div>
      )}

      {sub === "account" && (
        <div class="volo-grid-2">
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Profile & Password (`PATCH /api/admin/me`)</h3>
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                await apiFetch("/api/admin/me", { method: "PATCH", body: JSON.stringify(profileForm) });
                setProfileForm({ ...profileForm, current_password: "", new_password: "" });
                await refreshMe();
                showToast(t("common.saved"));
              }}
            >
              <div class="volo-field">
                <label>Display Name</label>
                <input class="volo-input" value={profileForm.display_name} onInput={(e) => setProfileForm({ ...profileForm, display_name: (e.currentTarget as HTMLInputElement).value })} />
              </div>
              <div class="volo-field">
                <label>Bio</label>
                <textarea class="volo-textarea" rows={2} value={profileForm.bio} onInput={(e) => setProfileForm({ ...profileForm, bio: (e.currentTarget as HTMLTextAreaElement).value })} />
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>Current Password (to change)</label>
                  <input type="password" class="volo-input" value={profileForm.current_password} onInput={(e) => setProfileForm({ ...profileForm, current_password: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>New Argon2id Password</label>
                  <input type="password" class="volo-input" value={profileForm.new_password} onInput={(e) => setProfileForm({ ...profileForm, new_password: (e.currentTarget as HTMLInputElement).value })} />
                </div>
              </div>
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.save")}</button>
            </form>
          </div>

          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>TOTP 2FA & Named WebAuthn Passkeys</h3>
            <div style={{ marginBottom: "16px" }}>
              {meInfo?.totp_enabled ? (
                <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
                  <span class="volo-badge volo-badge-ok">TOTP 2FA Enabled</span>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm volo-btn-danger"
                    onClick={async () => {
                      if (!window.confirm("Disable TOTP 2FA on your account?")) return;
                      await apiFetch("/api/admin/security/totp", { method: "DELETE" });
                      void loadSub();
                    }}
                  >
                    Disable TOTP
                  </button>
                </div>
              ) : totpEnroll ? (
                <div>
                  <div class="volo-help" style={{ wordBreak: "break-all", marginBottom: "8px" }}>
                    Secret: <code>{totpEnroll.secret}</code><br />
                    URI: <code>{totpEnroll.otpauth_uri}</code>
                  </div>
                  <div style={{ display: "flex", gap: "8px" }}>
                    <input class="volo-input" placeholder="6-digit code" value={totpCode} onInput={(e) => setTotpCode((e.currentTarget as HTMLInputElement).value)} />
                    <button
                      type="button"
                      class="volo-btn volo-btn-primary"
                      onClick={async () => {
                        const res = await apiFetch<{ recovery_codes: string[] }>("/api/admin/security/totp/enroll", {
                          method: "POST",
                          body: JSON.stringify({ secret: totpEnroll.secret, code: totpCode }),
                        });
                        setRecoveryCodes(res.recovery_codes || []);
                        setTotpEnroll(null);
                        void loadSub();
                      }}
                    >
                      Verify & Activate
                    </button>
                  </div>
                </div>
              ) : (
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  onClick={async () => {
                    const res = await apiFetch<{ secret: string; otpauth_uri: string }>("/api/admin/security/totp/setup", { method: "POST", body: "{}" });
                    setTotpEnroll(res);
                  }}
                >
                  + Enroll TOTP Authenticator
                </button>
              )}
              {recoveryCodes && (
                <pre style={{ background: "var(--volo-raised)", padding: "10px", borderRadius: "8px", fontSize: "12px", marginTop: "8px" }}>
                  Save One-Time Recovery Codes:{"\n"}{recoveryCodes.join("   ")}
                </pre>
              )}
            </div>

            <hr style={{ border: 0, borderTop: "1px solid var(--volo-line)", margin: "14px 0" }} />
            <div style={{ display: "flex", gap: "8px", marginBottom: "10px" }}>
              <input class="volo-input" placeholder="Passkey Device Name" value={pkName} onInput={(e) => setPkName((e.currentTarget as HTMLInputElement).value)} />
              <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={registerPasskey}>+ Add Passkey</button>
            </div>
            {passkeys.map((pk) => (
              <div key={pk.id} style={{ display: "flex", justifyContent: "space-between", padding: "6px 0", fontSize: "13px" }}>
                <span>🔑 <strong>{pk.name}</strong> ({formatDateTime(pk.created_at)})</span>
                <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                  await apiFetch(`/api/admin/security/passkeys/${pk.id}`, { method: "DELETE" });
                  void loadSub();
                }}>×</button>
              </div>
            ))}
          </div>

          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Scoped API Tokens (`volo_pat_...` for Obsidian / PicGo / CLI)</h3>
            <div style={{ display: "flex", gap: "8px", marginBottom: "8px" }}>
              <input class="volo-input" placeholder="Token Label" value={patName} onInput={(e) => setPatName((e.currentTarget as HTMLInputElement).value)} />
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={async () => {
                  const res = await apiFetch<{ plaintext_token: string }>("/api/admin/security/tokens", {
                    method: "POST",
                    body: JSON.stringify({ name: patName, scopes: patScopes }),
                  });
                  setIssuedPAT(res.plaintext_token);
                  void loadSub();
                }}
              >
                Generate Token
              </button>
            </div>
            <div style={{ display: "flex", gap: "10px", flexWrap: "wrap", marginBottom: "10px", fontSize: "12px" }}>
              {PAT_SCOPES.map((sc) => (
                <label key={sc}>
                  <input
                    type="checkbox"
                    checked={patScopes.includes(sc)}
                    onChange={(e) => {
                      const on = (e.currentTarget as HTMLInputElement).checked;
                      setPatScopes(on ? [...patScopes, sc] : patScopes.filter((x) => x !== sc));
                    }}
                  />{" "}
                  {sc}
                </label>
              ))}
            </div>
            {issuedPAT && (
              <div class="volo-card" style={{ padding: "10px", background: "var(--volo-accent-soft)", wordBreak: "break-all", fontSize: "12px" }}>
                <strong>Copy now (shown once):</strong> <code>{issuedPAT}</code>
              </div>
            )}
            {tokens.map((tk) => (
              <div key={tk.id} style={{ display: "flex", justifyContent: "space-between", padding: "6px 0", borderBottom: "1px solid var(--volo-line)" }}>
                <span><strong>{tk.name}</strong> <code>{tk.token_prefix}...</code> <span class="volo-help">{tk.scopes_json}</span></span>
                <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                  await apiFetch(`/api/admin/security/tokens/${tk.id}`, { method: "DELETE" });
                  void loadSub();
                }}>Revoke</button>
              </div>
            ))}
          </div>

          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Active Device Sessions (`user_sessions`)</h3>
            {sessions.map((s) => (
              <div key={s.id} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "8px 0", borderBottom: "1px solid var(--volo-line)" }}>
                <div>
                  <div>
                    <code>{s.client_ip}</code> {s.geo_region ? `(${s.geo_region})` : ""}{" "}
                    {s.id === meInfo?.session_id && <span class="volo-badge volo-badge-ok">Current</span>}
                  </div>
                  <div class="volo-help">{String(s.user_agent || "").slice(0, 68)} · {formatDateTime(s.last_seen_at)}</div>
                </div>
                {s.id !== meInfo?.session_id && (
                  <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                    await apiFetch(`/api/admin/security/sessions/${s.id}`, { method: "DELETE" });
                    void loadSub();
                  }}>Kick Offline</button>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {sub === "users" && isAdmin && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("sys.users")} (`admin` · `editor` · `reader`)</h3>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/users", { method: "POST", body: JSON.stringify(userForm) });
              setUserForm({ username: "", email: "", display_name: "", role: "editor", password: "" });
              void loadSub();
            }}
            class="volo-grid-3"
            style={{ marginBottom: "16px" }}
          >
            <input class="volo-input" required placeholder="Username" value={userForm.username} onInput={(e) => setUserForm({ ...userForm, username: (e.currentTarget as HTMLInputElement).value })} />
            <input type="email" class="volo-input" required placeholder="Email" value={userForm.email} onInput={(e) => setUserForm({ ...userForm, email: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "6px" }}>
              <select class="volo-select" value={userForm.role} onChange={(e) => setUserForm({ ...userForm, role: (e.currentTarget as HTMLSelectElement).value })}>
                <option value="editor">Editor (Collaborator)</option>
                <option value="reader">Reader (Front-end Only)</option>
                <option value="admin">Admin (Full Sovereign)</option>
              </select>
              <input type="password" class="volo-input" required minLength={10} placeholder="Password (>=10)" value={userForm.password} onInput={(e) => setUserForm({ ...userForm, password: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
            </div>
          </form>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead><tr><th class="col-fluid">User</th><th class="col-status">Role</th><th class="col-status">Status</th><th class="col-date">Created</th><th class="col-act">Actions</th></tr></thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id}>
                    <td class="col-fluid"><strong>{u.display_name || u.username}</strong> (@{u.username}) <span class="volo-help">{u.email}</span></td>
                    <td class="col-status"><span class="volo-badge">{u.role}</span></td>
                    <td class="col-status">{u.status}</td>
                    <td class="col-date">{formatDateTime(u.created_at)}</td>
                    <td class="col-act">
                      {u.id !== currentUserSig.value?.id && (
                        <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                          await apiFetch(`/api/admin/users/${u.id}`, { method: "DELETE" });
                          void loadSub();
                        }}>×</button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {sub === "waf" && isAdmin && (
        <div class="volo-grid-2">
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>IP / CIDR Bans (Self-Lockout Guarded)</h3>
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                await apiFetch("/api/admin/security/ip-bans", { method: "POST", body: JSON.stringify(banForm) });
                setBanForm({ cidr: "", note: "manual_ban", expires_hours: 24 });
                void loadSub();
              }}
              style={{ display: "flex", gap: "8px", marginBottom: "14px" }}
            >
              <input class="volo-input" required placeholder="203.0.113.9 or 198.51.100.0/24" value={banForm.cidr} onInput={(e) => setBanForm({ ...banForm, cidr: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">Ban</button>
            </form>
            {bans.map((b) => (
              <div key={b.id} style={{ display: "flex", justifyContent: "space-between", padding: "7px 0", borderBottom: "1px solid var(--volo-line)" }}>
                <span><code>{b.cidr}</code> · {b.note || b.reason}</span>
                <button type="button" class="volo-btn volo-btn-sm" onClick={async () => {
                  await apiFetch(`/api/admin/security/ip-bans/${b.id}`, { method: "DELETE" });
                  void loadSub();
                }}>Unban</button>
              </div>
            ))}
          </div>

          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Offline GeoIP Rules (`VOLO_GEO_ACCESS_ENABLED` Circuit Breaker)</h3>
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                await apiFetch("/api/admin/security/geo-rules", { method: "POST", body: JSON.stringify({ ...geoForm, is_enabled: true }) });
                setGeoForm({ name: "Geo Rule", action: "deny", countries: "", regions: "", sort_order: 10 });
                void loadSub();
              }}
              class="volo-grid-3"
              style={{ marginBottom: "14px" }}
            >
              <select class="volo-select" value={geoForm.action} onChange={(e) => setGeoForm({ ...geoForm, action: (e.currentTarget as HTMLSelectElement).value })}>
                <option value="deny">Deny</option>
                <option value="allow">Allow</option>
              </select>
              <input class="volo-input" placeholder="ISO (e.g. KP, US)" value={geoForm.countries} onInput={(e) => setGeoForm({ ...geoForm, countries: (e.currentTarget as HTMLInputElement).value })} />
              <div style={{ display: "flex", gap: "6px" }}>
                <input class="volo-input" placeholder="Regions" value={geoForm.regions} onInput={(e) => setGeoForm({ ...geoForm, regions: (e.currentTarget as HTMLInputElement).value })} />
                <button type="submit" class="volo-btn volo-btn-primary">+</button>
              </div>
            </form>
            {geoRules.map((g) => (
              <div key={g.id} style={{ display: "flex", justifyContent: "space-between", padding: "7px 0", borderBottom: "1px solid var(--volo-line)" }}>
                <span><span class="volo-badge">{g.action}</span> ISO={g.countries || "*"} Regions={g.regions || "*"}</span>
                <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                  await apiFetch(`/api/admin/security/geo-rules/${g.id}`, { method: "DELETE" });
                  void loadSub();
                }}>×</button>
              </div>
            ))}
          </div>
        </div>
      )}

      {sub === "backup" && isAdmin && (
        <div class="volo-grid-2">
          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Cross-Dialect Streaming Backup & Legacy ETL</h3>
            <p class="volo-help" style={{ marginBottom: "14px" }}>
              Native NDJSON `.zip` archives stream directly to/from disk with SHA-256 integrity manifests, enabling zero-OOM migration across SQLite ⇄ PostgreSQL ⇄ MySQL.
            </p>
            <div style={{ display: "flex", gap: "10px", flexWrap: "wrap" }}>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={async () => {
                  const res = await fetch("/api/admin/backup/export?media=1", {
                    method: "POST",
                    headers: { "X-CSRF-Token": (await apiFetch<any>("/api/admin/me")).csrf_token },
                    credentials: "same-origin",
                  });
                  if (!res.ok) {
                    showToast("Backup export failed");
                    return;
                  }
                  const blob = await res.blob();
                  const a = document.createElement("a");
                  a.href = URL.createObjectURL(blob);
                  a.download = `volo-backup-${Date.now()}.zip`;
                  a.click();
                }}
              >
                Download Native Backup (.zip)
              </button>

              <label class="volo-btn" style={{ cursor: "pointer" }}>
                Restore Native Backup (.zip)
                <input
                  type="file"
                  accept=".zip"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = (e.currentTarget as HTMLInputElement).files?.[0];
                    if (!f || !window.confirm("Restore backup into active database?")) return;
                    const fd = new FormData();
                    fd.append("file", f);
                    await apiFetch("/api/admin/backup/restore", { method: "POST", body: fd });
                    showToast("Backup restored cleanly!");
                  }}
                />
              </label>

              <label class="volo-btn" style={{ cursor: "pointer" }}>
                Import Legacy 15-Table Archive
                <input
                  type="file"
                  accept=".zip,.json"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = (e.currentTarget as HTMLInputElement).files?.[0];
                    if (!f) return;
                    const fd = new FormData();
                    fd.append("file", f);
                    await apiFetch("/api/admin/backup/import-legacy", { method: "POST", body: fd });
                    showToast("Legacy archive normalized & imported!");
                  }}
                />
              </label>
            </div>
          </div>

          <div class="volo-card">
            <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>Live `volo doctor` Diagnostics</h3>
            <pre style={{ margin: 0, maxHeight: "300px", overflow: "auto", fontSize: "12px", background: "var(--volo-raised)", padding: "12px", borderRadius: "8px" }}>
              {doctorReport ? JSON.stringify(doctorReport, null, 2) : "Loading diagnostics..."}
            </pre>
          </div>
        </div>
      )}

      {sub === "audit" && isAdmin && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("sys.audit")}</h3>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead><tr><th class="col-date">Time</th><th class="col-cat">Actor</th><th class="col-cat">Action</th><th class="col-fluid">Resource & Masked IP</th></tr></thead>
              <tbody>
                {auditLogs.map((l) => (
                  <tr key={l.id}>
                    <td class="col-date">{formatDateTime(l.created_at)}</td>
                    <td class="col-cat">{l.actor_name}</td>
                    <td class="col-cat"><span class="volo-badge">{l.action}</span></td>
                    <td class="col-fluid">{l.resource_type}:{l.resource_id} · <code>{l.client_ip}</code></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
