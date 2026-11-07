package content

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

type dailyTrafficDelta struct {
	pv int64
	uv int64
}

type topItemBucketKey struct {
	statDate string
	kind     string
	itemKey  string
}

// viewAnalyticsBuffer decouples high-concurrency public GET requests from synchronous single-writer
// SQLite transactions by coalescing view_count, redirect hit_count, analytics_daily, and analytics_top_items
// increments in memory and flushing them in a single batched transaction every 2s and on graceful shutdown.
type viewAnalyticsBuffer struct {
	store *sqlx.Store

	mu           sync.RWMutex
	dirty        bool
	articleViews map[string]int64
	pageViews    map[string]int64
	redirectHits map[string]int64
	daily        map[string]dailyTrafficDelta
	topItems     map[topItemBucketKey]int64

	loopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	closeMu  sync.Mutex
	closed   bool
}

func newViewAnalyticsBuffer(store *sqlx.Store) *viewAnalyticsBuffer {
	return &viewAnalyticsBuffer{
		store:        store,
		articleViews: make(map[string]int64),
		pageViews:    make(map[string]int64),
		redirectHits: make(map[string]int64),
		daily:        make(map[string]dailyTrafficDelta),
		topItems:     make(map[topItemBucketKey]int64),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

func (b *viewAnalyticsBuffer) ensureLoop() {
	if b == nil || b.store == nil {
		return
	}
	b.loopOnce.Do(func() {
		go func() {
			defer close(b.doneCh)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-b.stopCh:
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = b.Flush(ctx)
					cancel()
				}
			}
		}()
	})
}

func (b *viewAnalyticsBuffer) RecordView(targetType string, targetID string, statDate string, uvDelta int64, refererHost string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.dirty = true
	if targetType == domain.TargetTypeArticle && targetID != "" {
		b.articleViews[targetID]++
		if len(b.topItems) < 8192 {
			b.topItems[topItemBucketKey{statDate: statDate, kind: "article", itemKey: targetID}]++
		}
	} else if targetType == domain.TargetTypePage && targetID != "" {
		b.pageViews[targetID]++
	}
	if cleanRef := strings.ToLower(strings.TrimSpace(refererHost)); cleanRef != "" && len(b.topItems) < 8192 {
		b.topItems[topItemBucketKey{statDate: statDate, kind: "referer", itemKey: cleanRef}]++
	}
	cur := b.daily[statDate]
	cur.pv++
	cur.uv += uvDelta
	b.daily[statDate] = cur
	b.mu.Unlock()

	b.ensureLoop()
}

func (b *viewAnalyticsBuffer) RecordRedirectHit(redirectID string) {
	if b == nil || redirectID == "" {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.dirty = true
	b.redirectHits[redirectID]++
	b.mu.Unlock()

	b.ensureLoop()
}

func (b *viewAnalyticsBuffer) PendingArticleViews(articleID string) int64 {
	if b == nil || articleID == "" {
		return 0
	}
	b.mu.RLock()
	n := b.articleViews[articleID]
	b.mu.RUnlock()
	return n
}

func (b *viewAnalyticsBuffer) PendingPageViews(pageID string) int64 {
	if b == nil || pageID == "" {
		return 0
	}
	b.mu.RLock()
	n := b.pageViews[pageID]
	b.mu.RUnlock()
	return n
}

// Flush atomically snapshots and drains all accumulated deltas into a single background write transaction.
func (b *viewAnalyticsBuffer) Flush(ctx context.Context) error {
	if b == nil || b.store == nil {
		return nil
	}

	b.mu.Lock()
	if !b.dirty {
		b.mu.Unlock()
		return nil
	}
	artSnap := b.articleViews
	pageSnap := b.pageViews
	redSnap := b.redirectHits
	dailySnap := b.daily
	topSnap := b.topItems

	b.articleViews = make(map[string]int64)
	b.pageViews = make(map[string]int64)
	b.redirectHits = make(map[string]int64)
	b.daily = make(map[string]dailyTrafficDelta)
	b.topItems = make(map[topItemBucketKey]int64)
	b.dirty = false
	b.mu.Unlock()

	now := time.Now().UnixMilli()
	err := b.store.WithBgTx(ctx, func(tx *sqlx.Tx) error {
		for artID, delta := range artSnap {
			if delta <= 0 {
				continue
			}
			if _, e := tx.ExecContext(ctx, `UPDATE articles SET view_count = view_count + ? WHERE id = ?`, delta, artID); e != nil {
				return e
			}
		}
		for pgID, delta := range pageSnap {
			if delta <= 0 {
				continue
			}
			if _, e := tx.ExecContext(ctx, `UPDATE pages SET view_count = view_count + ? WHERE id = ?`, delta, pgID); e != nil {
				return e
			}
		}
		for redID, delta := range redSnap {
			if delta <= 0 {
				continue
			}
			if _, e := tx.ExecContext(ctx, `UPDATE redirects SET hit_count = hit_count + ? WHERE id = ?`, delta, redID); e != nil {
				return e
			}
		}
		for k, delta := range topSnap {
			if delta <= 0 {
				continue
			}
			if e := upsertDailyTopItemDeltaTx(ctx, tx, k.statDate, k.kind, k.itemKey, delta); e != nil {
				return e
			}
		}
		for statDate, d := range dailySnap {
			if d.pv <= 0 && d.uv <= 0 {
				continue
			}
			res, e := tx.ExecContext(
				ctx,
				`UPDATE analytics_daily SET pv = pv + ?, uv = uv + ?, updated_at = ? WHERE stat_date = ?`,
				d.pv, d.uv, now, statDate,
			)
			if e != nil {
				return e
			}
			if n, _ := res.RowsAffected(); n == 0 {
				upsert := tx.Dialect().UpsertSQL(
					"analytics_daily",
					[]string{"stat_date", "pv", "uv", "updated_at"},
					[]string{"stat_date"},
					[]string{"updated_at"},
				)
				if _, iErr := tx.ExecContext(ctx, upsert, statDate, d.pv, d.uv, now); iErr != nil {
					if _, uErr := tx.ExecContext(
						ctx,
						`UPDATE analytics_daily SET pv = pv + ?, uv = uv + ?, updated_at = ? WHERE stat_date = ?`,
						d.pv, d.uv, now, statDate,
					); uErr != nil {
						return iErr
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		const maxFailedRestoreKeys = 4096
		// Restore deltas (bounded to maxFailedRestoreKeys) so transient lock/IO errors neither drop counts nor grow unbounded.
		b.mu.Lock()
		for k, v := range artSnap {
			if _, ok := b.articleViews[k]; ok || len(b.articleViews) < maxFailedRestoreKeys {
				b.articleViews[k] += v
			}
		}
		for k, v := range pageSnap {
			if _, ok := b.pageViews[k]; ok || len(b.pageViews) < maxFailedRestoreKeys {
				b.pageViews[k] += v
			}
		}
		for k, v := range redSnap {
			if _, ok := b.redirectHits[k]; ok || len(b.redirectHits) < maxFailedRestoreKeys {
				b.redirectHits[k] += v
			}
		}
		for k, v := range dailySnap {
			if _, ok := b.daily[k]; ok || len(b.daily) < maxFailedRestoreKeys {
				cur := b.daily[k]
				cur.pv += v.pv
				cur.uv += v.uv
				b.daily[k] = cur
			}
		}
		for k, v := range topSnap {
			if _, ok := b.topItems[k]; ok || len(b.topItems) < maxFailedRestoreKeys {
				b.topItems[k] += v
			}
		}
		b.dirty = true
		b.mu.Unlock()
		return err
	}
	return nil
}

// Reset discards buffered analytics deltas (used after full database restore/swap).
func (b *viewAnalyticsBuffer) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.articleViews = make(map[string]int64)
	b.pageViews = make(map[string]int64)
	b.redirectHits = make(map[string]int64)
	b.daily = make(map[string]dailyTrafficDelta)
	b.topItems = make(map[topItemBucketKey]int64)
	b.dirty = false
	b.mu.Unlock()
}

func (b *viewAnalyticsBuffer) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.closeMu.Lock()
	defer b.closeMu.Unlock()

	b.mu.Lock()
	wasClosed := b.closed
	b.closed = true
	b.mu.Unlock()
	if wasClosed {
		return nil
	}

	// Ensure loopOnce cannot start a fresh goroutine after Close, and wait if one was running.
	started := true
	b.loopOnce.Do(func() {
		started = false
		close(b.doneCh)
	})
	close(b.stopCh)
	if started {
		select {
		case <-b.doneCh:
		case <-ctx.Done():
		}
	}
	return b.Flush(ctx)
}

func upsertDailyTopItemDeltaTx(ctx context.Context, tx *sqlx.Tx, statDate string, kind string, itemKey string, delta int64) error {
	res, err := tx.ExecContext(
		ctx,
		`UPDATE analytics_top_items SET hits = hits + ? WHERE stat_date = ? AND kind = ? AND item_key = ?`,
		delta, statDate, kind, itemKey,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	upsert := tx.Dialect().UpsertSQL(
		"analytics_top_items",
		[]string{"stat_date", "kind", "item_key", "hits"},
		[]string{"stat_date", "kind", "item_key"},
		[]string{},
	)
	if _, iErr := tx.ExecContext(ctx, upsert, statDate, kind, itemKey, delta); iErr != nil {
		_, uErr := tx.ExecContext(
			ctx,
			`UPDATE analytics_top_items SET hits = hits + ? WHERE stat_date = ? AND kind = ? AND item_key = ?`,
			delta, statDate, kind, itemKey,
		)
		if uErr != nil {
			return iErr
		}
	}
	return nil
}
