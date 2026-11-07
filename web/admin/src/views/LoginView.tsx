import { useState } from "preact/hooks";
import {
  apiFetch,
  b64urlToBytes,
  bootConfig,
  bytesToB64url,
  csrfTokenSig,
  currentUserSig,
  refreshMe,
  showToast,
  StaffUser,
} from "../api";
import { LocalePopover } from "../components/UIControls";
import { availableLocalesSig, ensureLocaleLoaded, localeSig, normalizeLocale, t } from "../i18n";

export function LoginView() {
  const locales = availableLocalesSig.value;
  const [ident, setIdent] = useState("");
  const [password, setPassword] = useState("");
  const [showPwd, setShowPwd] = useState(false);
  const [challengeToken, setChallengeToken] = useState("");
  const [totpCode, setTotpCode] = useState("");
  const [rememberDevice, setRememberDevice] = useState(true);
  const [busy, setBusy] = useState(false);

  const handlePasswordSubmit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    try {
      const res = await apiFetch<{
        requires_2fa: boolean;
        challenge_token?: string;
        user?: StaffUser;
        csrf_token?: string;
      }>("/api/admin/auth/login", {
        method: "POST",
        body: JSON.stringify({ ident, password }),
      });
      if (res.requires_2fa && res.challenge_token) {
        setChallengeToken(res.challenge_token);
        return;
      }
      if (res.user) {
        currentUserSig.value = res.user;
        csrfTokenSig.value = res.csrf_token || "";
        await refreshMe();
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setBusy(false);
    }
  };

  const handle2FASubmit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    try {
      const res = await apiFetch<{ user: StaffUser; csrf_token: string }>("/api/admin/auth/2fa", {
        method: "POST",
        body: JSON.stringify({
          challenge_token: challengeToken,
          code: totpCode,
          remember_device: rememberDevice,
        }),
      });
      currentUserSig.value = res.user;
      csrfTokenSig.value = res.csrf_token || "";
      await refreshMe();
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setBusy(false);
    }
  };

  const handlePasskeyLogin = async () => {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      showToast(t("auth.passkey_https_only"));
      return;
    }
    setBusy(true);
    try {
      const ceremony = await apiFetch<{
        state_token: string;
        challenge: string;
        rp_id: string;
      }>("/api/admin/auth/passkey/login/begin", { method: "POST", body: "{}" });

      const cred = (await navigator.credentials.get({
        publicKey: {
          challenge: b64urlToBytes(ceremony.challenge),
          rpId: ceremony.rp_id,
          userVerification: "preferred",
          timeout: 60000,
        },
      })) as PublicKeyCredential | null;
      if (!cred) return;
      const assertion = cred.response as AuthenticatorAssertionResponse;

      const res = await apiFetch<{ user: StaffUser; csrf_token: string }>("/api/admin/auth/passkey/login/finish", {
        method: "POST",
        body: JSON.stringify({
          state_token: ceremony.state_token,
          credential_id: bytesToB64url(cred.rawId),
          client_data_json: bytesToB64url(assertion.clientDataJSON),
          authenticator_data: bytesToB64url(assertion.authenticatorData),
          signature: bytesToB64url(assertion.signature),
        }),
      });
      currentUserSig.value = res.user;
      csrfTokenSig.value = res.csrf_token || "";
      await refreshMe();
    } catch (err: any) {
      showToast(err?.message || t("auth.passkey_cancelled"));
    } finally {
      setBusy(false);
    }
  };

  const siteTitle = bootConfig.site_title || "Volo";
  const curYear = new Date().getFullYear();

  return (
    <div class="volo-auth-page">
      <header class="volo-auth-header">
        <a class="volo-auth-brand" href="/">
          <span class="volo-auth-mark">V.</span>
          <span class="volo-auth-brand-text">
            <strong>{siteTitle}</strong>
            <small>VOLO</small>
          </span>
        </a>

        <div class="volo-auth-header-right">
          <LocalePopover
            value={localeSig.value}
            locales={locales}
            onChange={(nextRaw) => {
              const next = normalizeLocale(nextRaw);
              void ensureLocaleLoaded(next);
            }}
          />
          <a class="volo-auth-back" href="/">
            ← {t("auth.back_blog")}
          </a>
        </div>
      </header>

      <main class="volo-auth-stage">
        <div class="volo-auth-layout">
          <section class="volo-auth-story" aria-hidden="true">
            <div>
              <div class="volo-auth-story-eyebrow">{t("auth.story_eyebrow")}</div>
              <h1>
                {t("auth.story_title_1")}
                <br />
                {t("auth.story_title_2")}
              </h1>
              <p>{t("auth.story_desc")}</p>
            </div>

            <div class="volo-auth-story-art">
              <span class="volo-auth-shape s1" />
              <span class="volo-auth-shape s2" />
              <span class="volo-auth-shape s3" />
            </div>

            <div class="volo-auth-story-foot">
              <span class="volo-rail-dot" style={{ marginRight: 0 }} />
              <span>{t("auth.story_foot")}</span>
            </div>
          </section>

          <section class="volo-auth-card">
            <div>
              <div class="volo-auth-card-top">
                <span class="volo-auth-kicker">{t("auth.kicker")}</span>
                <span class="volo-auth-version">VOLO v1.0.0</span>
              </div>

              <h2>{t("auth.welcome_title")}</h2>
              <p class="volo-auth-sub">{t("auth.welcome_sub")}</p>

              {!challengeToken ? (
                <form onSubmit={handlePasswordSubmit} autoComplete="off" data-form-type="other">
                  <div class="volo-field">
                    <label>{t("auth.ident")}</label>
                    <div class="volo-auth-input-wrap">
                      <span class="volo-auth-input-icon">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                          <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2" />
                          <circle cx="12" cy="7" r="4" />
                        </svg>
                      </span>
                      <input
                        class="volo-input"
                        name="volo_staff_ident"
                        autoComplete="off"
                        data-lpignore="true"
                        data-1p-ignore="true"
                        data-form-type="other"
                        required
                        autoFocus
                        placeholder={t("auth.ident_ph")}
                        value={ident}
                        onInput={(e) => setIdent((e.currentTarget as HTMLInputElement).value)}
                      />
                    </div>
                  </div>

                  <div class="volo-field">
                    <label>{t("auth.password")}</label>
                    <div class="volo-auth-input-wrap">
                      <span class="volo-auth-input-icon">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                          <rect x="3" y="11" width="18" height="11" rx="2" />
                          <path d="M7 11V7a5 5 0 0 1 10 0v4" />
                        </svg>
                      </span>
                      <input
                        type={showPwd ? "text" : "password"}
                        class="volo-input"
                        name="volo_staff_secret"
                        autoComplete="new-password"
                        data-lpignore="true"
                        data-1p-ignore="true"
                        data-form-type="other"
                        required
                        placeholder={t("auth.password_ph")}
                        value={password}
                        onInput={(e) => setPassword((e.currentTarget as HTMLInputElement).value)}
                      />
                      <button
                        type="button"
                        class="volo-auth-eye"
                        aria-label={showPwd ? t("auth.hide_pwd") : t("auth.show_pwd")}
                        title={showPwd ? t("auth.hide_pwd") : t("auth.show_pwd")}
                        onClick={() => setShowPwd((v) => !v)}
                      >
                        {showPwd ? (
                          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                            <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94" />
                            <path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19" />
                            <line x1="1" y1="1" x2="23" y2="23" />
                          </svg>
                        ) : (
                          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                            <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z" />
                            <circle cx="12" cy="12" r="3" />
                          </svg>
                        )}
                      </button>
                    </div>
                  </div>

                  <button type="submit" class="volo-btn volo-btn-primary volo-auth-submit" disabled={busy}>
                    <span>{t("auth.submit_btn")}</span>
                    <span>→</span>
                  </button>

                  <div class="volo-auth-divider">{t("auth.or_passkey")}</div>

                  <button
                    type="button"
                    class="volo-btn"
                    style={{ width: "100%", height: "40px" }}
                    disabled={busy}
                    onClick={handlePasskeyLogin}
                  >
                    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                      <circle cx="7.5" cy="15.5" r="5.5" />
                      <path d="m21 2-9.6 9.6" />
                      <path d="m15.5 7.5 3 3L22 7l-3-3" />
                    </svg>
                    <span>{t("auth.passkey")}</span>
                  </button>
                </form>
              ) : (
                <form onSubmit={handle2FASubmit} autoComplete="off">
                  <div class="volo-field">
                    <label>{t("auth.totp_code")}</label>
                    <div class="volo-auth-input-wrap">
                      <span class="volo-auth-input-icon">
                        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
                        </svg>
                      </span>
                      <input
                        class="volo-input"
                        name="totp_otp_code"
                        autoComplete="one-time-code"
                        required
                        autoFocus
                        placeholder="123456 / ABCD-EFGH"
                        value={totpCode}
                        onInput={(e) => setTotpCode((e.currentTarget as HTMLInputElement).value)}
                      />
                    </div>
                  </div>
                  <label style={{ display: "flex", alignItems: "center", gap: "8px", marginBottom: "14px", fontSize: "13px" }}>
                    <input
                      type="checkbox"
                      checked={rememberDevice}
                      onChange={(e) => setRememberDevice((e.currentTarget as HTMLInputElement).checked)}
                    />
                    {t("auth.remember_2fa")}
                  </label>
                  <button type="submit" class="volo-btn volo-btn-primary volo-auth-submit" disabled={busy}>
                    {t("auth.verify")}
                  </button>
                  <button
                    type="button"
                    class="volo-btn"
                    style={{ width: "100%", marginTop: "10px" }}
                    onClick={() => {
                      setChallengeToken("");
                      setTotpCode("");
                    }}
                  >
                    ← {t("auth.back_login")}
                  </button>
                </form>
              )}
            </div>

            <div class="volo-auth-card-foot">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
                <polyline points="9 12 11 14 15 10" />
              </svg>
              <span>{t("auth.staff_only_tip")}</span>
            </div>
          </section>
        </div>
      </main>

      <footer class="volo-auth-footer">
        <span>© {curYear} {siteTitle}</span>
        <a href="https://github.com/Willh92/Volo-go" target="_blank" rel="noreferrer">
          {t("auth.oss_motto")} ↗
        </a>
      </footer>
    </div>
  );
}
