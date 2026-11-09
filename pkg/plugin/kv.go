package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

const (
	MaxKVKeyBytes          = 191
	MaxKVValueBytes        = 512 * 1024 // 512 KiB per value (supports lexicon/dictionary payloads)
	MaxKVKeysPerPlugin     = 1000
	maxKVListLimit         = 200
	defaultKVListLimit     = 50
	kvBufferedFlushPeriod  = 5 * time.Second
)

// KVItem represents a key-value entry returned by ListPrefix.
type KVItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	IntValue  int64  `json:"int_value"`
	ExpiresAt int64  `json:"expires_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type kvPluginKey struct {
	pluginID string
	key      string
}

type kvCounterEntry struct {
	base       int64
	baseLoaded bool
	delta      int64
}

type kvBufferHub struct {
	store       *sqlx.Store
	flushMu     sync.Mutex
	mu          sync.Mutex
	gen         uint64
	entries     map[kvPluginKey]*kvCounterEntry
	dirty       map[kvPluginKey]int64
	stopCh      chan struct{}
	doneCh      chan struct{}
	loopRunning bool
	closed      bool
}

var kvHubRegistry sync.Map // map[*sqlx.Store]*kvBufferHub

func lookupKVHub(store *sqlx.Store) *kvBufferHub {
	if store == nil {
		return nil
	}
	if v, ok := kvHubRegistry.Load(store); ok {
		return v.(*kvBufferHub)
	}
	return nil
}

func getOrCreateKVHub(store *sqlx.Store) *kvBufferHub {
	if store == nil {
		return nil
	}
	if v, ok := kvHubRegistry.Load(store); ok {
		return v.(*kvBufferHub)
	}
	cand := &kvBufferHub{
		store:   store,
		entries: make(map[kvPluginKey]*kvCounterEntry),
		dirty:   make(map[kvPluginKey]int64),
	}
	actual, _ := kvHubRegistry.LoadOrStore(store, cand)
	return actual.(*kvBufferHub)
}

func (h *kvBufferHub) ensureFlushLoopLocked() {
	if h.closed || h.loopRunning || len(h.dirty) == 0 {
		return
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	h.stopCh = stopCh
	h.doneCh = doneCh
	h.loopRunning = true
	go h.runFlushLoop(stopCh, doneCh)
}

func (h *kvBufferHub) runFlushLoop(stopCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)
	ticker := time.NewTicker(kvBufferedFlushPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			h.mu.Lock()
			hasDirty := len(h.dirty) > 0 && !h.closed
			if !hasDirty {
				h.loopRunning = false
				h.mu.Unlock()
				return
			}
			h.mu.Unlock()

			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.flush(flushCtx, "")
			cancel()
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "closed") {
				h.mu.Lock()
				h.loopRunning = false
				h.mu.Unlock()
				return
			}
		}
	}
}

func (h *kvBufferHub) flush(ctx context.Context, filterPluginID string) error {
	if h == nil || h.store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	h.flushMu.Lock()
	defer h.flushMu.Unlock()

	type deltaItem struct {
		pk    kvPluginKey
		delta int64
	}

	h.mu.Lock()
	if len(h.dirty) == 0 {
		h.mu.Unlock()
		return nil
	}
	batch := make([]deltaItem, 0, len(h.dirty))
	for pk, d := range h.dirty {
		if d == 0 {
			delete(h.dirty, pk)
			continue
		}
		if filterPluginID != "" && pk.pluginID != filterPluginID {
			continue
		}
		batch = append(batch, deltaItem{pk: pk, delta: d})
	}
	h.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	sort.Slice(batch, func(i, j int) bool {
		if batch[i].pk.pluginID != batch[j].pk.pluginID {
			return batch[i].pk.pluginID < batch[j].pk.pluginID
		}
		return batch[i].pk.key < batch[j].pk.key
	})

	now := time.Now().Unix()
	committed := make(map[kvPluginKey]int64, len(batch))

	err := h.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		for _, item := range batch {
			nextVal, stepErr := applyCounterDeltaTx(ctx, tx, item.pk.pluginID, item.pk.key, item.delta, 0, now)
			if stepErr != nil {
				return stepErr
			}
			committed[item.pk] = nextVal
		}
		return nil
	})
	if err != nil {
		return err
	}

	h.mu.Lock()
	h.gen++
	for _, item := range batch {
		ent := h.entries[item.pk]
		if ent == nil {
			continue
		}
		ent.base = committed[item.pk]
		ent.baseLoaded = true
		ent.delta -= item.delta
		if ent.delta == 0 {
			delete(h.dirty, item.pk)
		} else {
			h.dirty[item.pk] = ent.delta
		}
	}
	h.mu.Unlock()
	return nil
}

// FlushPluginKV deterministically flushes any dirty buffered KV counters for pluginID in a single transaction.
func FlushPluginKV(ctx context.Context, store *sqlx.Store, pluginID string) error {
	if pluginID == "" {
		return nil
	}
	hub := lookupKVHub(store)
	if hub == nil {
		return nil
	}
	return hub.flush(ctx, pluginID)
}

// FlushAllKV deterministically flushes all dirty buffered KV counters across all plugins on store.
func FlushAllKV(ctx context.Context, store *sqlx.Store) error {
	hub := lookupKVHub(store)
	if hub == nil {
		return nil
	}
	return hub.flush(ctx, "")
}

// CloseKVBuffer stops any background flush ticker for store and flushes all dirty buffered KV counters.
func CloseKVBuffer(ctx context.Context, store *sqlx.Store) error {
	hub := lookupKVHub(store)
	if hub == nil {
		return nil
	}
	hub.mu.Lock()
	hub.closed = true
	wasRunning := hub.loopRunning
	stopCh := hub.stopCh
	doneCh := hub.doneCh
	hub.loopRunning = false
	hub.mu.Unlock()

	if wasRunning && stopCh != nil {
		close(stopCh)
		if doneCh != nil {
			<-doneCh
		}
	}
	err := hub.flush(ctx, "")
	kvHubRegistry.Delete(store)
	return err
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

func parseCounterRowBase(row domain.PluginKV) int64 {
	if row.ValueInt != 0 {
		return row.ValueInt
	}
	if s := strings.TrimSpace(row.ValueText); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// Get fetches a non-expired value by key from ReadDB (zero write side-effects),
// overlaying any un-flushed in-memory buffered counter delta for immediate visibility.
func (kv *KVStore) Get(ctx context.Context, key string) (string, bool, error) {
	k, err := validateKVKey(key)
	if err != nil {
		return "", false, err
	}
	now := kv.nowFn().Unix()
	var row domain.PluginKV
	err = sqlx.GetInto(
		ctx,
		kv.store,
		&row,
		`SELECT plugin_id, k, value_text, value_int, expires_at, updated_at
		 FROM plugin_kv
		 WHERE plugin_id = ? AND k = ?`,
		kv.pluginID, k,
	)
	dbFound := err == nil && (row.ExpiresAt == 0 || row.ExpiresAt > now)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}

	if hub := lookupKVHub(kv.store); hub != nil {
		pk := kvPluginKey{pluginID: kv.pluginID, key: k}
		hub.mu.Lock()
		var pendingDelta int64
		if ent := hub.entries[pk]; ent != nil {
			pendingDelta = ent.delta
		}
		hub.mu.Unlock()

		if pendingDelta != 0 {
			var base int64
			if dbFound {
				base = parseCounterRowBase(row)
			}
			return strconv.FormatInt(base+pendingDelta, 10), true, nil
		}
	}

	if !dbFound {
		return "", false, nil
	}
	return row.ValueText, true, nil
}

// IncrBuffered accumulates counter deltas in O(1) RAM under a lightweight mutex with
// zero synchronous SQLite write transactions, returning the combined (base + pending delta)
// value immediately and flushing dirty counters in batched background transactions.
func (kv *KVStore) IncrBuffered(ctx context.Context, key string, delta int64) (int64, error) {
	k, err := validateKVKey(key)
	if err != nil {
		return 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hub := getOrCreateKVHub(kv.store)
	pk := kvPluginKey{pluginID: kv.pluginID, key: k}

	for {
		hub.mu.Lock()
		if ent := hub.entries[pk]; ent != nil && ent.baseLoaded {
			ent.delta += delta
			if ent.delta == 0 {
				delete(hub.dirty, pk)
			} else {
				hub.dirty[pk] = ent.delta
				hub.ensureFlushLoopLocked()
			}
			combined := ent.base + ent.delta
			hub.mu.Unlock()
			return combined, nil
		}
		readGen := hub.gen
		hub.mu.Unlock()

		// Cold-miss only: read initial base from ReadDB (zero write transactions).
		now := kv.nowFn().Unix()
		var row domain.PluginKV
		qErr := sqlx.GetInto(
			ctx,
			kv.store,
			&row,
			`SELECT plugin_id, k, value_text, value_int, expires_at, updated_at
			 FROM plugin_kv
			 WHERE plugin_id = ? AND k = ?`,
			kv.pluginID, k,
		)
		if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
			return 0, qErr
		}
		var loadedBase int64
		if qErr == nil && (row.ExpiresAt == 0 || row.ExpiresAt > now) {
			loadedBase = parseCounterRowBase(row)
		}

		hub.mu.Lock()
		if ent := hub.entries[pk]; ent != nil && ent.baseLoaded {
			ent.delta += delta
			if ent.delta == 0 {
				delete(hub.dirty, pk)
			} else {
				hub.dirty[pk] = ent.delta
				hub.ensureFlushLoopLocked()
			}
			combined := ent.base + ent.delta
			hub.mu.Unlock()
			return combined, nil
		}
		if hub.gen != readGen {
			hub.mu.Unlock()
			continue
		}
		ent := hub.entries[pk]
		if ent == nil {
			if len(hub.entries) >= MaxKVKeysPerPlugin*16 {
				// Evict clean entries before growing unboundedly.
				for epk, cand := range hub.entries {
					if cand.delta == 0 {
						delete(hub.entries, epk)
					}
				}
			}
			ent = &kvCounterEntry{}
			hub.entries[pk] = ent
		}
		ent.base = loadedBase
		ent.baseLoaded = true
		ent.delta += delta
		if ent.delta == 0 {
			delete(hub.dirty, pk)
		} else {
			hub.dirty[pk] = ent.delta
			hub.ensureFlushLoopLocked()
		}
		combined := ent.base + ent.delta
		hub.mu.Unlock()
		return combined, nil
	}
}

// Flush deterministically flushes any dirty buffered counters for this plugin.
func (kv *KVStore) Flush(ctx context.Context) error {
	return FlushPluginKV(ctx, kv.store, kv.pluginID)
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
	intVal, parsedErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)

	hub := lookupKVHub(kv.store)
	if hub != nil {
		hub.flushMu.Lock()
		defer hub.flushMu.Unlock()
	}

	upsertSQL := kv.store.Dialect().UpsertSQL(
		"plugin_kv",
		[]string{"plugin_id", "k", "value_text", "value_int", "expires_at", "updated_at"},
		[]string{"plugin_id", "k"},
		[]string{"value_text", "value_int", "expires_at", "updated_at"},
	)

	err = kv.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		if err := enforcePluginKVQuotaTx(ctx, tx, kv.pluginID, k, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, upsertSQL, kv.pluginID, k, value, intVal, exp, now)
		return err
	})
	if err != nil {
		return err
	}

	if hub != nil {
		pk := kvPluginKey{pluginID: kv.pluginID, key: k}
		hub.mu.Lock()
		hub.gen++
		delete(hub.dirty, pk)
		if ttlSeconds <= 0 && parsedErr == nil {
			hub.entries[pk] = &kvCounterEntry{base: intVal, baseLoaded: true, delta: 0}
		} else {
			delete(hub.entries, pk)
		}
		hub.mu.Unlock()
	}
	return nil
}

// IncrBy atomically increments an integer counter by delta across SQLite, PostgreSQL, and MySQL,
// resetting an expired counter to 0 before applying delta, and updating value_text to match.
func (kv *KVStore) IncrBy(ctx context.Context, key string, delta int64, ttlSeconds int64) (int64, error) {
	k, err := validateKVKey(key)
	if err != nil {
		return 0, err
	}
	hub := lookupKVHub(kv.store)
	var pending int64
	pk := kvPluginKey{pluginID: kv.pluginID, key: k}
	if hub != nil {
		hub.flushMu.Lock()
		defer hub.flushMu.Unlock()
		hub.mu.Lock()
		if ent := hub.entries[pk]; ent != nil {
			pending = ent.delta
		}
		hub.mu.Unlock()
	}

	now := kv.nowFn().Unix()
	var nextVal int64
	effectiveDelta := delta + pending

	err = kv.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		var stepErr error
		nextVal, stepErr = applyCounterDeltaTx(ctx, tx, kv.pluginID, k, effectiveDelta, ttlSeconds, now)
		return stepErr
	})
	if err != nil {
		return 0, err
	}

	if hub != nil {
		hub.mu.Lock()
		hub.gen++
		if ent := hub.entries[pk]; ent != nil {
			ent.delta -= pending
			if ttlSeconds <= 0 {
				ent.base = nextVal
				ent.baseLoaded = true
			} else {
				ent.baseLoaded = false
			}
			if ent.delta == 0 {
				delete(hub.dirty, pk)
				if ttlSeconds > 0 {
					delete(hub.entries, pk)
				}
			} else {
				hub.dirty[pk] = ent.delta
			}
		}
		hub.mu.Unlock()
	}
	return nextVal, nil
}

func applyCounterDeltaTx(ctx context.Context, tx *sqlx.Tx, pluginID string, k string, delta int64, ttlSeconds int64, now int64) (int64, error) {
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
			delta, exp, now, pluginID, k, now,
		)
	} else {
		updateRes, uErr = tx.ExecContext(
			ctx,
			`UPDATE plugin_kv
			 SET value_int = value_int + ?, updated_at = ?
			 WHERE plugin_id = ? AND k = ? AND (expires_at = 0 OR expires_at > ?)`,
			delta, now, pluginID, k, now,
		)
	}
	if uErr != nil {
		return 0, uErr
	}

	affected, _ := updateRes.RowsAffected()
	if affected > 0 {
		var cur int64
		if err := tx.QueryRowContext(
			ctx,
			`SELECT value_int FROM plugin_kv WHERE plugin_id = ? AND k = ?`,
			pluginID, k,
		).Scan(&cur); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE plugin_kv SET value_text = ? WHERE plugin_id = ? AND k = ?`,
			strconv.FormatInt(cur, 10), pluginID, k,
		); err != nil {
			return 0, err
		}
		return cur, nil
	}

	// Step 2: Key does not exist or had expired; enforce quota, purge expired row if present, and insert.
	if err := enforcePluginKVQuotaTx(ctx, tx, pluginID, k, now); err != nil {
		return 0, err
	}
	_, _ = tx.ExecContext(
		ctx,
		`DELETE FROM plugin_kv WHERE plugin_id = ? AND k = ? AND expires_at > 0 AND expires_at <= ?`,
		pluginID, k, now,
	)
	var exp int64
	if ttlSeconds > 0 {
		exp = now + ttlSeconds
	}
	nextVal := delta
	upsertSQL := tx.Dialect().UpsertSQL(
		"plugin_kv",
		[]string{"plugin_id", "k", "value_text", "value_int", "expires_at", "updated_at"},
		[]string{"plugin_id", "k"},
		[]string{"value_text", "value_int", "expires_at", "updated_at"},
	)
	if _, err := tx.ExecContext(ctx, upsertSQL, pluginID, k, strconv.FormatInt(nextVal, 10), nextVal, exp, now); err != nil {
		return 0, err
	}
	return nextVal, nil
}

// Delete removes key for this plugin without bumping content dataRev,
// atomically discarding any un-flushed buffered delta so deleted keys cannot resurrect.
func (kv *KVStore) Delete(ctx context.Context, key string) error {
	k, err := validateKVKey(key)
	if err != nil {
		return err
	}
	hub := lookupKVHub(kv.store)
	if hub != nil {
		hub.flushMu.Lock()
		defer hub.flushMu.Unlock()
		pk := kvPluginKey{pluginID: kv.pluginID, key: k}
		hub.mu.Lock()
		hub.gen++
		delete(hub.dirty, pk)
		delete(hub.entries, pk)
		hub.mu.Unlock()
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
	if err := sqlx.SelectInto(ctx, kv.store, &rows, paginated, args...); err != nil {
		return nil, err
	}

	var pendingByKey map[string]int64
	if hub := lookupKVHub(kv.store); hub != nil {
		hub.mu.Lock()
		for pk, d := range hub.dirty {
			if d != 0 && pk.pluginID == kv.pluginID && (p == "" || strings.HasPrefix(pk.key, p)) && (cursor == "" || pk.key > cursor) {
				if pendingByKey == nil {
					pendingByKey = make(map[string]int64)
				}
				pendingByKey[pk.key] = d
			}
		}
		hub.mu.Unlock()
	}

	out := make([]KVItem, 0, len(rows)+len(pendingByKey))
	for _, r := range rows {
		if p != "" && !strings.HasPrefix(r.Key, p) {
			break
		}
		item := KVItem{
			Key:       r.Key,
			Value:     r.ValueText,
			IntValue:  r.ValueInt,
			ExpiresAt: r.ExpiresAt,
			UpdatedAt: r.UpdatedAt,
		}
		if d, ok := pendingByKey[r.Key]; ok {
			combined := parseCounterRowBase(r) + d
			item.IntValue = combined
			item.Value = strconv.FormatInt(combined, 10)
			item.UpdatedAt = now
			delete(pendingByKey, r.Key)
		}
		out = append(out, item)
	}

	if len(pendingByKey) > 0 {
		for k, d := range pendingByKey {
			out = append(out, KVItem{
				Key:       k,
				Value:     strconv.FormatInt(d, 10),
				IntValue:  d,
				ExpiresAt: 0,
				UpdatedAt: now,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		if len(out) > limit {
			out = out[:limit]
		}
	}

	return out, nil
}

func (kv *KVStore) enforceQuotaTx(ctx context.Context, tx *sqlx.Tx, key string, now int64) error {
	return enforcePluginKVQuotaTx(ctx, tx, kv.pluginID, key, now)
}

func enforcePluginKVQuotaTx(ctx context.Context, tx *sqlx.Tx, pluginID string, key string, now int64) error {
	var exists int
	err := tx.QueryRowContext(
		ctx,
		`SELECT 1 FROM plugin_kv WHERE plugin_id = ? AND k = ?`,
		pluginID, key,
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
		pluginID, now,
	)

	var count int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM plugin_kv WHERE plugin_id = ?`,
		pluginID,
	).Scan(&count); err != nil {
		return err
	}
	if count >= MaxKVKeysPerPlugin {
		return fmt.Errorf("plugin_kv: plugin %q reached maximum quota of %d keys", pluginID, MaxKVKeysPerPlugin)
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

// DeletePrefix deletes all keys belonging to this plugin whose key begins with prefix
// without bumping content dataRev. Requires a non-empty prefix (use ClearPluginKV to wipe all keys).
func (kv *KVStore) DeletePrefix(ctx context.Context, prefix string) (int64, error) {
	p := strings.TrimSpace(prefix)
	if p == "" {
		return 0, errors.New("plugin_kv: deletePrefix requires a non-empty prefix")
	}
	if len(p) > MaxKVKeyBytes {
		return 0, fmt.Errorf("plugin_kv: prefix exceeds %d bytes", MaxKVKeyBytes)
	}
	if strings.ContainsRune(p, '\x00') {
		return 0, errors.New("plugin_kv: prefix cannot contain NUL byte")
	}
	hub := lookupKVHub(kv.store)
	var removedBuffered int64
	if hub != nil {
		hub.flushMu.Lock()
		defer hub.flushMu.Unlock()
		hub.mu.Lock()
		hub.gen++
		for pk := range hub.entries {
			if pk.pluginID == kv.pluginID && strings.HasPrefix(pk.key, p) {
				if hub.dirty[pk] != 0 {
					removedBuffered++
				}
				delete(hub.dirty, pk)
				delete(hub.entries, pk)
			}
		}
		hub.mu.Unlock()
	}
	res, err := kv.store.ExecBgContext(
		ctx,
		`DELETE FROM plugin_kv WHERE plugin_id = ? AND k LIKE ? ESCAPE '\'`,
		kv.pluginID,
		escapeLikePrefix(p)+"%",
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 && removedBuffered > 0 {
		return removedBuffered, nil
	}
	return n, nil
}

func escapeLikePrefix(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '%' || c == '_' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ClearPluginKV deletes all stored keys belonging to pluginID without bumping content dataRev.
func ClearPluginKV(ctx context.Context, store *sqlx.Store, pluginID string) error {
	hub := lookupKVHub(store)
	if hub != nil {
		hub.flushMu.Lock()
		defer hub.flushMu.Unlock()
		hub.mu.Lock()
		hub.gen++
		for pk := range hub.entries {
			if pk.pluginID == pluginID {
				delete(hub.dirty, pk)
				delete(hub.entries, pk)
			}
		}
		hub.mu.Unlock()
	}
	_, err := store.ExecBgContext(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ?`, pluginID)
	return err
}

