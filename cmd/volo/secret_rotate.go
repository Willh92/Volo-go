package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

type rotateCounts struct {
	DBJSON             int `json:"db_json"`
	Settings           int `json:"settings"`
	UserTOTP           int `json:"user_credentials_totp"`
	Plugins            int `json:"plugins"`
	Webhooks           int `json:"webhooks"`
	Articles           int `json:"articles"`
	Comments           int `json:"comments"`
	ThemeConfigSecrets int `json:"appearance_theme_configs"`
}

func (c rotateCounts) total() int {
	return c.DBJSON + c.Settings + c.UserTOTP + c.Plugins + c.Webhooks + c.Articles + c.Comments + c.ThemeConfigSecrets
}

func cmdSecret(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: volo secret rotate [--new-key <key> | --generate] [--write-keyfile] [--dry-run]")
		return 2
	}
	switch args[0] {
	case "rotate":
		return cmdSecretRotate(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "usage: volo secret rotate [--new-key <key> | --generate] [--write-keyfile] [--dry-run]")
		return 0
	default:
		fmt.Fprintf(stderr, "volo secret: unknown subcommand %q (expected rotate)\n", args[0])
		return 2
	}
}

func cmdSecretRotate(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("volo secret rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var common boundCommonFlags
	registerCommonFlags(fs, &common)

	newKeyFlag := fs.String("new-key", "", "New master secret key (or set VOLO_NEW_SECRET_KEY)")
	generateFlag := fs.Bool("generate", false, "Generate a fresh 32-byte CSPRNG master key")
	writeKeyfile := fs.Bool("write-keyfile", false, "Atomically write the new key to ${DataDir}/.secret.key (mode 0600)")
	dryRun := fs.Bool("dry-run", false, "Verify all existing encrypted secrets decrypt cleanly without mutating DB or files")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	newKeySet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "new-key" {
			newKeySet = true
		}
	})
	if newKeySet && *generateFlag {
		fmt.Fprintln(stderr, "volo secret rotate: --new-key and --generate are mutually exclusive")
		return 2
	}

	rawNewKey := strings.TrimSpace(*newKeyFlag)
	if !*generateFlag && rawNewKey == "" {
		rawNewKey = strings.TrimSpace(os.Getenv("VOLO_NEW_SECRET_KEY"))
	}
	if !*generateFlag && rawNewKey == "" {
		fmt.Fprintln(stderr, "volo secret rotate: specify --new-key <key>, VOLO_NEW_SECRET_KEY, or --generate")
		return 2
	}

	overrides := buildCLIOverrides(fs, &common)
	cfg, err := config.LoadForDoctor(overrides)
	if err != nil {
		fmt.Fprintf(stderr, "volo secret rotate: config load failed: %v\n", err)
		return 1
	}

	oldKeyRings, err := collectCandidateOldKeyRings(cfg, overrides.EnvFile)
	if err != nil {
		fmt.Fprintf(stderr, "volo secret rotate: failed loading old keys: %v\n", err)
		return 1
	}
	if len(oldKeyRings) == 0 {
		fmt.Fprintln(stderr, "volo secret rotate: no active VOLO_SECRET_KEY, VOLO_SECRET_OLD_KEYS, or .secret.key found to decrypt existing data")
		return 1
	}

	newKeyRing, generatedKeyString, keyFileBytes, err := buildTargetNewKeyRing(rawNewKey, *generateFlag, *writeKeyfile)
	if err != nil {
		fmt.Fprintf(stderr, "volo secret rotate: invalid new key: %v\n", err)
		return 2
	}

	// 1/8: Inspect & decrypt ${DataDir}/db.json if present (and resolve Wizard DB DSN if DB flags/env were not explicitly pinned)
	var wizSel *config.WizardDBSelection
	var origDBJSON []byte
	var counts rotateCounts
	dbJSONPath := filepath.Join(cfg.DataDir, "db.json")
	if rawBytes, rErr := os.ReadFile(dbJSONPath); rErr == nil {
		origDBJSON = rawBytes
		openedSel, oErr := openWizardDBWithAny(cfg.DataDir, oldKeyRings)
		if oErr != nil {
			fmt.Fprintf(stderr, "volo secret rotate: cannot decrypt %s with current/old keys: %v\n", dbJSONPath, oErr)
			return 1
		}
		wizSel = openedSel
		counts.DBJSON = 1
		if overrides.DBDriver == nil && overrides.DBDSN == nil &&
			strings.TrimSpace(os.Getenv("VOLO_DB_DRIVER")) == "" &&
			strings.TrimSpace(os.Getenv("VOLO_DB_DSN")) == "" && wizSel != nil {
			cfg.SetDBSpec(wizSel.Driver, wizSel.DSN, wizSel.SimpleProtocol)
		}
	} else if !os.IsNotExist(rErr) {
		fmt.Fprintf(stderr, "volo secret rotate: cannot read %s: %v\n", dbJSONPath, rErr)
		return 1
	}

	drv, dsn, simple := cfg.GetDBSpec()
	var store *sqlx.Store
	if drv == "sqlite" && dsn != "" && dsn != ":memory:" {
		if _, statErr := os.Stat(dsn); os.IsNotExist(statErr) {
			// Uninitialized SQLite file on disk: no DB rows exist to rotate.
			store = nil
		}
	}
	if store == nil && !(drv == "sqlite" && dsn != "" && dsn != ":memory:" && fileNotExist(dsn)) {
		store, err = sqlx.Open(ctx, sqlx.Options{
			Driver:         drv,
			DSN:            dsn,
			SimpleProtocol: simple,
			MaxOpenConns:   2,
			MaxIdleConns:   1,
			ConnLifetime:   cfg.DBConnLifetime,
		})
		if err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: database open failed: %v\n", err)
			return 1
		}
		defer store.Close()
	}

	type kvPair struct {
		id         string
		ciphertext string
	}
	var (
		reSettings   []kvPair
		reTOTP       []kvPair
		rePlugins    []kvPair
		reWebhooks   []kvPair
		reArticles   []kvPair
		reComments   []kvPair
		newAppJSON   string
		hasAppUpdate bool
	)

	if store != nil {
		// 2/8: settings.secret_ciphertext
		var settingRows []struct {
			Domain           string `db:"domain"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if err := selectOptionalTable(ctx, store, &settingRows,
			`SELECT domain, secret_ciphertext FROM settings WHERE secret_ciphertext <> '' ORDER BY domain ASC`); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: query settings failed: %v\n", err)
			return 1
		}
		for _, r := range settingRows {
			pt, oErr := openStringWithAny(oldKeyRings, r.SecretCiphertext, "settings:"+r.Domain)
			if oErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — cannot decrypt settings.%s: %v\n", r.Domain, oErr)
				return 1
			}
			ct, sErr := newKeyRing.SealString(pt, "settings:"+r.Domain)
			if sErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: seal settings.%s failed: %v\n", r.Domain, sErr)
				return 1
			}
			reSettings = append(reSettings, kvPair{id: r.Domain, ciphertext: ct})
		}
		counts.Settings = len(reSettings)

		// 3/8: user_credentials (kind='totp')
		var totpRows []struct {
			ID               string `db:"id"`
			UserID           string `db:"user_id"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if err := selectOptionalTable(ctx, store, &totpRows,
			`SELECT id, user_id, secret_ciphertext FROM user_credentials WHERE kind = 'totp' AND secret_ciphertext <> '' ORDER BY id ASC`); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: query user_credentials failed: %v\n", err)
			return 1
		}
		for _, r := range totpRows {
			aad := "cred:totp:" + r.UserID
			pt, oErr := openStringWithAny(oldKeyRings, r.SecretCiphertext, aad)
			if oErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — cannot decrypt user_credentials(totp) id=%s user=%s: %v\n", r.ID, r.UserID, oErr)
				return 1
			}
			ct, sErr := newKeyRing.SealString(pt, aad)
			if sErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: seal user_credentials(totp) id=%s failed: %v\n", r.ID, sErr)
				return 1
			}
			reTOTP = append(reTOTP, kvPair{id: r.ID, ciphertext: ct})
		}
		counts.UserTOTP = len(reTOTP)

		// 4/8: plugins.secret_ciphertext
		var pluginRows []struct {
			ID               string `db:"id"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if err := selectOptionalTable(ctx, store, &pluginRows,
			`SELECT id, secret_ciphertext FROM plugins WHERE secret_ciphertext <> '' ORDER BY id ASC`); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: query plugins failed: %v\n", err)
			return 1
		}
		newSB := newKeyRing.SecretBox()
		for _, r := range pluginRows {
			pt, oErr := openSecretBoxWithAny(oldKeyRings, r.SecretCiphertext)
			if oErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — cannot decrypt plugins.%s: %v\n", r.ID, oErr)
				return 1
			}
			ct, sErr := newSB.Seal(pt)
			if sErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: seal plugins.%s failed: %v\n", r.ID, sErr)
				return 1
			}
			rePlugins = append(rePlugins, kvPair{id: r.ID, ciphertext: ct})
		}
		counts.Plugins = len(rePlugins)

		// 5/8: webhooks.secret_ciphertext
		var webhookRows []struct {
			ID               string `db:"id"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if err := selectOptionalTable(ctx, store, &webhookRows,
			`SELECT id, secret_ciphertext FROM webhooks WHERE secret_ciphertext <> '' ORDER BY id ASC`); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: query webhooks failed: %v\n", err)
			return 1
		}
		for _, r := range webhookRows {
			pt, oErr := openStringWithAny(oldKeyRings, r.SecretCiphertext, config.WebhookSecretAAD(r.ID), "webhooks:secret:"+r.ID)
			if oErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — cannot decrypt webhooks.%s: %v\n", r.ID, oErr)
				return 1
			}
			ct, sErr := newKeyRing.SealString(pt, config.WebhookSecretAAD(r.ID))
			if sErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: seal webhooks.%s failed: %v\n", r.ID, sErr)
				return 1
			}
			reWebhooks = append(reWebhooks, kvPair{id: r.ID, ciphertext: ct})
		}
		counts.Webhooks = len(reWebhooks)

		// 6/8: articles.password_ciphertext
		var articleRows []struct {
			ID                 string `db:"id"`
			PasswordCiphertext string `db:"password_ciphertext"`
		}
		if err := selectOptionalTable(ctx, store, &articleRows,
			`SELECT id, password_ciphertext FROM articles WHERE password_ciphertext <> '' ORDER BY id ASC`); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: query articles failed: %v\n", err)
			return 1
		}
		for _, r := range articleRows {
			pt, oErr := openStringWithAny(oldKeyRings, r.PasswordCiphertext, config.ArticlePasswordAAD(r.ID), "articles:password:"+r.ID)
			if oErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — cannot decrypt articles.%s: %v\n", r.ID, oErr)
				return 1
			}
			ct, sErr := newKeyRing.SealString(pt, config.ArticlePasswordAAD(r.ID))
			if sErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: seal articles.%s failed: %v\n", r.ID, sErr)
				return 1
			}
			reArticles = append(reArticles, kvPair{id: r.ID, ciphertext: ct})
		}
		counts.Articles = len(reArticles)

		// 7/8: comments.notify_email_ciphertext
		var commentRows []struct {
			ID                    string `db:"id"`
			NotifyEmailCiphertext string `db:"notify_email_ciphertext"`
		}
		if err := selectOptionalTable(ctx, store, &commentRows,
			`SELECT id, notify_email_ciphertext FROM comments WHERE notify_email_ciphertext <> '' ORDER BY id ASC`); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: query comments failed: %v\n", err)
			return 1
		}
		for _, r := range commentRows {
			pt, oErr := openStringWithAny(
				oldKeyRings,
				r.NotifyEmailCiphertext,
				config.CommentNotifyEmailAAD(r.ID),
				"comments:notify_email:"+r.ID,
				"comments:email:"+r.ID,
				"comment:email",
			)
			if oErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — cannot decrypt comments.%s: %v\n", r.ID, oErr)
				return 1
			}
			ct, sErr := newKeyRing.SealString(pt, config.CommentNotifyEmailAAD(r.ID))
			if sErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: seal comments.%s failed: %v\n", r.ID, sErr)
				return 1
			}
			reComments = append(reComments, kvPair{id: r.ID, ciphertext: ct})
		}
		counts.Comments = len(reComments)

		// 8/8: settings(appearance).value_json -> theme_configs[themeID][key] ("enc:v1:...")
		var appRaw string
		qErr := store.QueryRowContext(ctx, store.Dialect().Rebind(`SELECT value_json FROM settings WHERE domain = ?`), "appearance").Scan(&appRaw)
		if qErr == nil && strings.TrimSpace(appRaw) != "" {
			updatedJSON, nLeaves, aErr := rotateAppearanceThemeConfigJSON(appRaw, oldKeyRings, newKeyRing)
			if aErr != nil {
				fmt.Fprintf(stderr, "volo secret rotate: aborting — %v\n", aErr)
				return 1
			}
			if nLeaves > 0 {
				newAppJSON = updatedJSON
				hasAppUpdate = true
				counts.ThemeConfigSecrets = nLeaves
			}
		} else if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) && !isMissingTableErr(qErr) {
			fmt.Fprintf(stderr, "volo secret rotate: query appearance settings failed: %v\n", qErr)
			return 1
		}
	}

	if *dryRun {
		fmt.Fprintf(stdout, "Dry-run OK: verified %d encrypted secret(s) across 8 stores (db.json=%d, settings=%d, totp=%d, plugins=%d, webhooks=%d, articles=%d, comments=%d, theme_configs=%d)\n",
			counts.total(), counts.DBJSON, counts.Settings, counts.UserTOTP, counts.Plugins, counts.Webhooks, counts.Articles, counts.Comments, counts.ThemeConfigSecrets)
		return 0
	}

	wroteDBJSON := false
	restoreDBJSON := func() {
		if wroteDBJSON && len(origDBJSON) > 0 {
			_ = os.WriteFile(dbJSONPath, origDBJSON, 0o600)
		}
	}

	if store != nil {
		txErr := store.WithTx(ctx, func(tx *sqlx.Tx) error {
			for _, item := range reSettings {
				if _, err := tx.ExecContext(ctx, `UPDATE settings SET secret_ciphertext = ? WHERE domain = ?`, item.ciphertext, item.id); err != nil {
					return err
				}
			}
			for _, item := range reTOTP {
				if _, err := tx.ExecContext(ctx, `UPDATE user_credentials SET secret_ciphertext = ? WHERE id = ?`, item.ciphertext, item.id); err != nil {
					return err
				}
			}
			for _, item := range rePlugins {
				if _, err := tx.ExecContext(ctx, `UPDATE plugins SET secret_ciphertext = ? WHERE id = ?`, item.ciphertext, item.id); err != nil {
					return err
				}
			}
			for _, item := range reWebhooks {
				if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET secret_ciphertext = ? WHERE id = ?`, item.ciphertext, item.id); err != nil {
					return err
				}
			}
			for _, item := range reArticles {
				if _, err := tx.ExecContext(ctx, `UPDATE articles SET password_ciphertext = ? WHERE id = ?`, item.ciphertext, item.id); err != nil {
					return err
				}
			}
			for _, item := range reComments {
				if _, err := tx.ExecContext(ctx, `UPDATE comments SET notify_email_ciphertext = ? WHERE id = ?`, item.ciphertext, item.id); err != nil {
					return err
				}
			}
			if hasAppUpdate {
				if _, err := tx.ExecContext(ctx, `UPDATE settings SET value_json = ? WHERE domain = ?`, newAppJSON, "appearance"); err != nil {
					return err
				}
			}
			if wizSel != nil {
				if err := config.SaveWizardDBConfig(cfg.DataDir, newKeyRing, *wizSel); err != nil {
					return fmt.Errorf("update %s: %w", dbJSONPath, err)
				}
				wroteDBJSON = true
			}
			return nil
		})
		if txErr != nil {
			restoreDBJSON()
			fmt.Fprintf(stderr, "volo secret rotate: transaction failed (rolled back): %v\n", txErr)
			return 1
		}
	} else if wizSel != nil {
		if err := config.SaveWizardDBConfig(cfg.DataDir, newKeyRing, *wizSel); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: update %s failed: %v\n", dbJSONPath, err)
			return 1
		}
		wroteDBJSON = true
	}

	if *writeKeyfile {
		if err := config.WriteSecretKeyFile(cfg.DataDir, keyFileBytes); err != nil {
			fmt.Fprintf(stderr, "volo secret rotate: failed writing .secret.key: %v\n", err)
			return 1
		}
	}

	fmt.Fprintf(stdout, "Rotated %d encrypted secret(s) across 8 stores (db.json=%d, settings=%d, totp=%d, plugins=%d, webhooks=%d, articles=%d, comments=%d, theme_configs=%d)\n",
		counts.total(), counts.DBJSON, counts.Settings, counts.UserTOTP, counts.Plugins, counts.Webhooks, counts.Articles, counts.Comments, counts.ThemeConfigSecrets)
	if *writeKeyfile {
		fmt.Fprintf(stdout, "Updated keyfile: %s\n", filepath.Join(cfg.DataDir, ".secret.key"))
	} else if generatedKeyString != "" {
		fmt.Fprintf(stdout, "VOLO_SECRET_KEY=%s\n", generatedKeyString)
	}
	return 0
}

func fileNotExist(p string) bool {
	_, err := os.Stat(p)
	return os.IsNotExist(err)
}

func isMissingTableErr(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "no such table") ||
		strings.Contains(low, "doesn't exist") ||
		strings.Contains(low, "does not exist")
}

func selectOptionalTable[T any](ctx context.Context, store *sqlx.Store, dest *[]T, query string, args ...any) error {
	err := sqlx.SelectInto(ctx, store, dest, query, args...)
	if isMissingTableErr(err) {
		return nil
	}
	return err
}

func collectCandidateOldKeyRings(cfg *config.Config, envFileOverride string) ([]*config.KeyRing, error) {
	var out []*config.KeyRing
	if cfg != nil && cfg.KeyRing != nil {
		out = append(out, cfg.KeyRing)
	}
	oldRaw := strings.TrimSpace(os.Getenv("VOLO_SECRET_OLD_KEYS"))
	if oldRaw == "" {
		envPath := strings.TrimSpace(envFileOverride)
		if envPath == "" {
			envPath = ".env"
		}
		oldRaw = readDotEnvKey(envPath, "VOLO_SECRET_OLD_KEYS")
	}
	if oldRaw != "" {
		parsed, err := config.ParseEnvKeyRings(oldRaw)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed...)
	}
	if cfg != nil && strings.TrimSpace(cfg.DataDir) != "" {
		keyPath := filepath.Join(cfg.DataDir, ".secret.key")
		if st, err := os.Lstat(keyPath); err == nil && st.Mode().IsRegular() {
			if kr, _, kErr := config.LoadOrInitKeyRing("", cfg.DataDir); kErr == nil && kr != nil {
				out = append(out, kr)
			}
		}
	}
	return out, nil
}

func readDotEnvKey(path string, targetKey string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		idx := strings.IndexByte(trimmed, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(trimmed[:idx])
		if k != targetKey {
			continue
		}
		v := strings.TrimSpace(trimmed[idx+1:])
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		return v
	}
	return ""
}

func buildTargetNewKeyRing(rawNewKey string, generate bool, writeKeyfile bool) (*config.KeyRing, string, []byte, error) {
	if writeKeyfile {
		var rawBytes []byte
		if generate {
			rawBytes = make([]byte, 32)
			if _, err := rand.Read(rawBytes); err != nil {
				return nil, "", nil, err
			}
		} else {
			parsed, err := decodeOrDeriveKeyFileBytes(rawNewKey)
			if err != nil {
				return nil, "", nil, err
			}
			rawBytes = parsed
		}
		kr, err := config.NewKeyRingFromKeyFileBytes(rawBytes)
		if err != nil {
			return nil, "", nil, err
		}
		return kr, "", rawBytes, nil
	}

	var keyStr string
	var genOut string
	if generate {
		rawBytes := make([]byte, 32)
		if _, err := rand.Read(rawBytes); err != nil {
			return nil, "", nil, err
		}
		keyStr = base64.RawURLEncoding.EncodeToString(rawBytes)
		genOut = keyStr
	} else {
		keyStr = strings.TrimSpace(rawNewKey)
	}
	kr, _, err := config.LoadOrInitKeyRing(keyStr, "")
	if err != nil {
		return nil, "", nil, err
	}
	return kr, genOut, nil, nil
}

func decodeOrDeriveKeyFileBytes(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 16 {
		return nil, errors.New("new key must be at least 16 characters")
	}
	if len(trimmed) >= 64 && len(trimmed)%2 == 0 {
		if dec, err := hex.DecodeString(trimmed); err == nil && len(dec) >= 32 {
			return dec, nil
		}
	}
	if dec, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil && len(dec) >= 32 {
		return dec, nil
	}
	if dec, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(dec) >= 32 {
		return dec, nil
	}
	sum := sha256.Sum256([]byte("volo-keyfile-explicit:v1:" + trimmed))
	return sum[:], nil
}

func openWizardDBWithAny(dataDir string, krs []*config.KeyRing) (*config.WizardDBSelection, error) {
	var lastErr error
	for _, kr := range krs {
		sel, err := config.LoadWizardDBConfig(dataDir, kr)
		if err == nil {
			return sel, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no candidate keyring available")
	}
	return nil, lastErr
}

func openStringWithAny(krs []*config.KeyRing, ct string, primaryAAD string, fallbackAADs ...string) (string, error) {
	var lastErr error
	for _, kr := range krs {
		pt, err := kr.OpenStringFallback(ct, primaryAAD, fallbackAADs...)
		if err == nil {
			return pt, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("decryption failed")
	}
	return "", lastErr
}

func openSecretBoxWithAny(krs []*config.KeyRing, ct string) (string, error) {
	var lastErr error
	for _, kr := range krs {
		pt, err := kr.SecretBox().Open(ct)
		if err == nil {
			return pt, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("plugin secretbox decryption failed")
	}
	return "", lastErr
}

func rotateAppearanceThemeConfigJSON(rawJSON string, oldKRs []*config.KeyRing, newKR *config.KeyRing) (string, int, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &root); err != nil {
		return "", 0, nil
	}
	tcAny, ok := root["theme_configs"]
	if !ok || tcAny == nil {
		return rawJSON, 0, nil
	}
	tcMap, ok := tcAny.(map[string]any)
	if !ok || len(tcMap) == 0 {
		return rawJSON, 0, nil
	}

	rotated := 0
	var rotateVal func(v any, aad string) (any, error)
	rotateVal = func(v any, aad string) (any, error) {
		switch tv := v.(type) {
		case string:
			if strings.HasPrefix(tv, "enc:v1:") {
				pt, err := openStringWithAny(oldKRs, tv, aad)
				if err != nil {
					return nil, err
				}
				ct, err := newKR.SealString(pt, aad)
				if err != nil {
					return nil, err
				}
				rotated++
				return ct, nil
			}
			return tv, nil
		case []any:
			cp := make([]any, len(tv))
			for i, elem := range tv {
				nv, err := rotateVal(elem, aad)
				if err != nil {
					return nil, err
				}
				cp[i] = nv
			}
			return cp, nil
		case map[string]any:
			cp := make(map[string]any, len(tv))
			for k, elem := range tv {
				nv, err := rotateVal(elem, aad)
				if err != nil {
					return nil, err
				}
				cp[k] = nv
			}
			return cp, nil
		default:
			return v, nil
		}
	}

	for themeID, cfgAny := range tcMap {
		cfgMap, ok := cfgAny.(map[string]any)
		if !ok {
			continue
		}
		cleanTheme := strings.TrimSpace(themeID)
		for key, val := range cfgMap {
			cleanKey := strings.TrimSpace(key)
			aad := "theme.config:" + cleanTheme + ":" + cleanKey
			nv, err := rotateVal(val, aad)
			if err != nil {
				return "", 0, fmt.Errorf("cannot decrypt appearance.theme_configs.%s.%s: %w", cleanTheme, cleanKey, err)
			}
			cfgMap[key] = nv
		}
	}
	if rotated == 0 {
		return rawJSON, 0, nil
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return "", 0, err
	}
	return string(encoded), rotated, nil
}
