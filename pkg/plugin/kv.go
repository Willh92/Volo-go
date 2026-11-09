package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

const (
	MaxKVKeyBytes      = 191
	MaxKVValueBytes    = 64 * 1024 // 64 KiB per value
	MaxKVKeysPerPlugin = 1000
	maxKVListLimit     = 200
	defaultKVListLimit = 50
)

// KVItem represents a key-value entry returned by ListPrefix.
type KVItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	IntValue  int64  `json:"int_value"`
	ExpiresAt int64  `json:"expires_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// KVStore provides a cross-database, quota-enforced key-value and atomic counter store
// isolated by pluginID inside the canonical plugin_kv table (automatically included in backups).
type KVStore struct {
	store    *sqlx.Store
	pluginID string
	nowFn    func() time.Time
}

// NewKVStore creates a scoped KVStore for pluginID.
func NewKVStore(store *sqlx.Store, pluginID string) *KVStore {
	return &KVStore{
		store:    store,
		pluginID: pluginID,
		nowFn:    time.Now,
	}
}

func validateKVKey(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", errors.New("plugin_kv: key cannot be empty")
	}
	if len(k) > MaxKVKeyBytes {
		return "", fmt.Errorf("plugin_kv: key exceeds %d bytes", MaxKVKeyBytes)
	}
	if strings.ContainsRune(k, '\x00') {
		return "", errors.New("plugin_kv: key cannot contain NUL byte")
	}
	return k, nil
}

// Get fetches a non-expired value by key from ReadDB (zero write side-effects).
func (kv *KVStore) Get(ctx context.Context, key string) (string, bool, error) {
	k, err := validateKVKey(key)
	if err != nil {
		return "", false, err
	}
	now := kv.nowFn().Unix()
	var row domain.PluginKV
	err = sqlx.GetInto(ctx,
		kv.store, &row, `SELECT plugin_id, k, value_text, value_int, expires_at, updated_at
		 FROM plugin_kv
		 WHERE plugin_id = ? AND k = ?`,
		kv.pluginID, k)

	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if row.ExpiresAt > 0 && row.ExpiresAt <= now {
		return "", false, nil
	}
	return row.ValueText, true, nil
}

// Set stores a string value with optional ttlSeconds (<= 0 means never expires).
func (kv *KVStore) Set(ctx context.Context, key string, value string, ttlSeconds int64) error {
	k, err := validateKVKey(key)
	if err != nil {
		return err
	}
	if len(value) > MaxKVValueBytes {
		return fmt.Errorf("plugin_kv: value exceeds %d bytes", MaxKVValueBytes)
	}
	now := kv.nowFn().Unix()
	var exp int64
	if ttlSeconds > 0 {
		exp = now + ttlSeconds
	}
	intVal, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)

	upsertSQL := kv.store.Dialect().UpsertSQL(
		"plugin_kv",
		[]string{"plugin_id", "k", "value_text", "value_int", "expires_at", "updated_at"},
		[]string{"plugin_id", "k"},
		[]string{"value_text", "value_int", "expires_at", "updated_at"},
	)

	return kv.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		if err := kv.enforceQuotaTx(ctx, tx, k, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, upsertSQL, kv.pluginID, k, value, intVal, exp, now)
		return err
	})
}

// IncrBy atomically increments an integer counter by delta across SQLite, PostgreSQL, and MySQL,
// resetting an expired counter to 0 before applying delta, and updating value_text to match.
func (kv *KVStore) IncrBy(ctx context.Context, key string, delta int64, ttlSeconds int64) (int64, error) {
	k, err := validateKVKey(key)
	if err != nil {
		return 0, err
	}
	now := kv.nowFn().Unix()
	var nextVal int64

	err = kv.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		// Step 1: Attempt single-statement atomic SQL update on a live, non-expired row.
		// Even on Postgres/MySQL READ COMMITTED, `SET value_int = value_int + ?` acquires a row-level X lock
		// and evaluates atomically against the latest committed value!
		var updateRes sql.Result
		var uErr error
		if ttlSeconds > 0 {
			exp := now + ttlSeconds
			updateRes, uErr = tx.ExecContext(
				ctx,
				`UPDATE plugin_kv
				 SET value_int = value_int + ?, expires_at = ?, updated_at = ?
				 WHERE plugin_id = ? AND k = ? AND (expires_at = 0 OR expires_at > ?)`,
				delta, exp, now, kv.pluginID, k, now,
			)
		} else {
			updateRes, uErr = tx.ExecContext(
				ctx,
				`UPDATE plugin_kv
				 SET value_int = value_int + ?, updated_at = ?
				 WHERE plugin_id = ? AND k = ? AND (expires_at = 0 OR expires_at > ?)`,
				delta, now, kv.pluginID, k, now,
			)
		}
		if uErr != nil {
			return uErr
		}

		affected, _ := updateRes.RowsAffected()
		if affected > 0 {
			var cur int64
			if err := tx.QueryRowContext(
				ctx,
				`SELECT value_int FROM plugin_kv WHERE plugin_id = ? AND k = ?`,
				kv.pluginID, k,
			).Scan(&cur); err != nil {
				return err
			}
			nextVal = cur
			_, err := tx.ExecContext(
				ctx,
				`UPDATE plugin_kv SET value_text = ? WHERE plugin_id = ? AND k = ?`,
				strconv.FormatInt(cur, 10), kv.pluginID, k,
			)
			return err
		}

		// Step 2: Key does not exist or had expired; enforce quota, purge expired row if present, and insert.
		if err := kv.enforceQuotaTx(ctx, tx, k, now); err != nil {
			return err
		}
		_, _ = tx.ExecContext(
			ctx,
			`DELETE FROM plugin_kv WHERE plugin_id = ? AND k = ? AND expires_at > 0 AND expires_at <= ?`,
			kv.pluginID, k, now,
		)
		var exp int64
		if ttlSeconds > 0 {
			exp = now + ttlSeconds
		}
		nextVal = delta
		upsertSQL := tx.Dialect().UpsertSQL(
			"plugin_kv",
			[]string{"plugin_id", "k", "value_text", "value_int", "expires_at", "updated_at"},
			[]string{"plugin_id", "k"},
			[]string{"value_text", "value_int", "expires_at", "updated_at"},
		)
		_, err := tx.ExecContext(ctx, upsertSQL, kv.pluginID, k, strconv.FormatInt(nextVal, 10), nextVal, exp, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	return nextVal, nil
}

// Delete removes key for this plugin without bumping content dataRev.
func (kv *KVStore) Delete(ctx context.Context, key string) error {
	k, err := validateKVKey(key)
	if err != nil {
		return err
	}
	_, err = kv.store.ExecBgContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ? AND k = ?`, kv.pluginID, k)
	return err
}

// ListPrefix returns up to limit non-expired entries whose key begins with prefix (and optionally > afterKey), ordered by key ASC.
func (kv *KVStore) ListPrefix(ctx context.Context, prefix string, limit int, afterKey ...string) ([]KVItem, error) {
	if limit <= 0 {
		limit = defaultKVListLimit
	}
	if limit > maxKVListLimit {
		limit = maxKVListLimit
	}
	now := kv.nowFn().Unix()
	p := strings.TrimSpace(prefix)
	cursor := ""
	if len(afterKey) > 0 {
		cursor = strings.TrimSpace(afterKey[0])
	}

	baseQuery := `SELECT plugin_id, k, value_text, value_int, expires_at, updated_at
		FROM plugin_kv
		WHERE plugin_id = ? AND (expires_at = 0 OR expires_at > ?)`
	args := []any{kv.pluginID, now}
	if cursor != "" && (p == "" || cursor >= p) {
		baseQuery += ` AND k > ?`
		args = append(args, cursor)
	} else if p != "" {
		// Use range lower-bound k >= prefix and filter exact prefix in Go to avoid SQL LIKE wildcard ('%' / '_') pitfalls!
		baseQuery += ` AND k >= ?`
		args = append(args, p)
	}
	baseQuery += ` ORDER BY k ASC`
	paginated, pageArgs := kv.store.Dialect().PaginateSQL(baseQuery, limit, 0)
	args = append(args, pageArgs...)
	var rows []domain.PluginKV
	err := sqlx.SelectInto(ctx, kv.store, &rows, paginated, args...)
	if err != nil {
		return nil, err
	}
	out := make([]KVItem, 0, len(rows))
	for _, r := range rows {
		if p != "" && !strings.HasPrefix(r.Key, p) {
			break
		}
		out = append(out, KVItem{
			Key:       r.Key,
			Value:     r.ValueText,
			IntValue:  r.ValueInt,
			ExpiresAt: r.ExpiresAt,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

func (kv *KVStore) enforceQuotaTx(ctx context.Context, tx *sqlx.Tx, key string, now int64) error {
	var exists int
	err := tx.QueryRowContext(
		ctx,
		`SELECT 1 FROM plugin_kv WHERE plugin_id = ? AND k = ?`,
		kv.pluginID, key,
	).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Purge expired keys for this plugin before counting quota.
	_, _ = tx.ExecContext(
		ctx,
		`DELETE FROM plugin_kv WHERE plugin_id = ? AND expires_at > 0 AND expires_at <= ?`,
		kv.pluginID, now,
	)

	var count int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM plugin_kv WHERE plugin_id = ?`,
		kv.pluginID,
	).Scan(&count); err != nil {
		return err
	}
	if count >= MaxKVKeysPerPlugin {
		return fmt.Errorf("plugin_kv: plugin %q reached maximum quota of %d keys", kv.pluginID, MaxKVKeysPerPlugin)
	}
	return nil
}

// PurgeExpiredKV deletes all expired keys across all plugins without bumping content dataRev.
func PurgeExpiredKV(ctx context.Context, store *sqlx.Store, nowUnix int64) (int64, error) {
	res, err := store.ExecBgContext(ctx, `DELETE FROM plugin_kv WHERE expires_at > 0 AND expires_at <= ?`, nowUnix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearPluginKV deletes all stored keys belonging to pluginID without bumping content dataRev.
func ClearPluginKV(ctx context.Context, store *sqlx.Store, pluginID string) error {
	_, err := store.ExecBgContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ?`, pluginID)
	return err
}
