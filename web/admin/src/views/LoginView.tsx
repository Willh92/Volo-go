import { useState } from "preact/hooks";
import { apiFetch, b64urlToBytes, bytesToB64url, csrfTokenSig, currentUserSig, refreshMe, showToast, StaffUser } from "../api";
import { BrandLogo } from "../components/BrandLogo";
import { LocalePopover } from "../components/UIControls";
import { availableLocalesSig, ensureLocaleLoaded, localeSig, normalizeLocale, t } from "../i18n";

export function LoginView() {
  const locales = availableLocalesSig.value;
  const [ident, setIdent] = useState("");
  const [password, setPassword] = useState("");
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

  return (
    <div class="volo-center-stage">
      <div class="volo-wizard-box" style={{ maxWidth: "430px" }}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: "10px", marginBottom: "18px" }}>
          <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
            <BrandLogo size={36} />
            <h1 style={{ margin: 0, fontSize: "20px" }}>{t("auth.sign_in")}</h1>
          </div>
          <LocalePopover
            value={localeSig.value}
            locales={locales}
            onChange={(nextRaw) => {
              const next = normalizeLocale(nextRaw);
              void ensureLocaleLoaded(next);
            }}
          />
        </div>

        {!challengeToken ? (
          <form onSubmit={handlePasswordSubmit}>
            <div class="volo-field">
              <label>{t("auth.ident")}</label>
              <input
                class="volo-input"
                name="username"
                autoComplete="username"
                required
                autoFocus
                value={ident}
                onInput={(e) => setIdent((e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div class="volo-field">
              <label>{t("auth.password")}</label>
              <input
                type="password"
                class="volo-input"
                name="password"
                autoComplete="current-password"
                required
                value={password}
                onInput={(e) => setPassword((e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <button type="submit" class="volo-btn volo-btn-primary" style={{ width: "100%", marginTop: "6px" }} disabled={busy}>
              {t("auth.sign_in")}
            </button>
            <button
              type="button"
              class="volo-btn"
              style={{ width: "100%", marginTop: "10px" }}
              disabled={busy}
              onClick={handlePasskeyLogin}
            >
              {t("auth.passkey")}
            </button>
          </form>
        ) : (
          <form onSubmit={handle2FASubmit} autoComplete="off">
            <div class="volo-field">
              <label>{t("auth.totp_code")}</label>
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
            <label style={{ display: "flex", alignItems: "center", gap: "8px", marginBottom: "14px", fontSize: "13px" }}>
              <input
                type="checkbox"
                checked={rememberDevice}
                onChange={(e) => setRememberDevice((e.currentTarget as HTMLInputElement).checked)}
              />
              {t("auth.remember_2fa")}
            </label>
            <button type="submit" class="volo-btn volo-btn-primary" style={{ width: "100%" }} disabled={busy}>
              {t("auth.verify")}
            </button>
          </form>
        )}
      </div>
    </div>
  );
}
