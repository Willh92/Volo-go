package content

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
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

const (
	maxRefererHostLen      = 253
	maxBufferedRefererKeys = 512
	maxBufferedTopItems    = 8192
)

// viewAnalyticsBuffer decouples high-concurrency public GET requests from synchronous single-writer
// SQLite transactions by coalescing view_count, redirect hit_count, analytics_daily, and analytics_top_items
// increments in memory and flushing them in a single batched transaction every 2s and on graceful shutdown.
type viewAnalyticsBuffer struct {
	store *sqlx.Store

	mu                       sync.RWMutex
	dirty                    bool
	articleViews             map[string]int64
	pageViews                map[string]int64
	redirectHits             map[string]int64
	daily                    map[string]dailyTrafficDelta
	topItems                 map[topItemBucketKey]int64
	refererKeys              int
	totalPendingArticleViews atomic.Int64

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

func normalizeRefererHost(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || len(s) > maxRefererHostLen {
		return "", false
	}
	if s[0] == '.' || s[0] == '-' || s[len(s)-1] == '.' || s[len(s)-1] == '-' {
		return "", false
	}
	labelLen := 0
	var prev byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '.' && c != '-' {
			return "", false
		}
		if c == '.' {
			if prev == '.' || prev == '-' || labelLen == 0 || labelLen > 63 {
				return "", false
			}
			labelLen = 0
		} else {
			if c == '-' && prev == '.' {
				return "", false
			}
			labelLen++
			if labelLen > 63 {
				return "", false
			}
		}
		prev = c
	}
	return s, true
}

func normalizeCountryCode(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if len(s) < 2 || len(s) > 3 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if !isUpper && !isDigit {
			return "", false
		}
		if i == 0 && !isUpper {
			return "", false
		}
	}
	return s, true
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
	b.RecordViewWithGeo(targetType, targetID, statDate, uvDelta, refererHost, "")
}

func (b *viewAnalyticsBuffer) RecordViewWithGeo(targetType string, targetID string, statDate string, uvDelta int64, refererHost string, countryCode string) {
	if b == nil {
		return
	}
	cleanRef, hasRef := normalizeRefererHost(refererHost)
	cleanCountry, hasCountry := normalizeCountryCode(countryCode)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.dirty = true
	if targetType == domain.TargetTypeArticle && targetID != "" {
		b.articleViews[targetID]++
		b.totalPendingArticleViews.Add(1)
		artKey := topItemBucketKey{statDate: statDate, kind: "article", itemKey: targetID}
		if _, exists := b.topItems[artKey]; exists || len(b.topItems) < maxBufferedTopItems {
			b.topItems[artKey]++
		}
	} else if targetType == domain.TargetTypePage && targetID != "" {
		b.pageViews[targetID]++
	}
	if hasRef {
		refKey := topItemBucketKey{statDate: statDate, kind: "referer", itemKey: cleanRef}
		if _, exists := b.topItems[refKey]; exists {
			b.topItems[refKey]++
		} else if b.refererKeys < maxBufferedRefererKeys && len(b.topItems) < maxBufferedTopItems {
			b.topItems[refKey] = 1
			b.refererKeys++
		}
	}
	if hasCountry {
		geoKey := topItemBucketKey{statDate: statDate, kind: "country", itemKey: cleanCountry}
		if _, exists := b.topItems[geoKey]; exists || len(b.topItems) < maxBufferedTopItems {
			b.topItems[geoKey]++
		}
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

func (b *viewAnalyticsBuffer) TotalPendingArticleViews() int64 {
	if b == nil {
		return 0
	}
	if n := b.totalPendingArticleViews.Load(); n > 0 {
		return n
	}
	return 0
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

	var drainedArtViews int64
	for _, v := range artSnap {
		if v > 0 {
			drainedArtViews += v
		}
	}
	if drainedArtViews > 0 {
		b.totalPendingArticleViews.Add(-drainedArtViews)
	}

	b.articleViews = make(map[string]int64)
	b.pageViews = make(map[string]int64)
	b.redirectHits = make(map[string]int64)
	b.daily = make(map[string]dailyTrafficDelta)
	b.topItems = make(map[topItemBucketKey]int64)
	b.refererKeys = 0
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
		upsertDaily := tx.Dialect().UpsertIncrementSQL(
			"analytics_daily",
			[]string{"stat_date", "pv", "uv", "updated_at"},
			[]string{"stat_date"},
			[]string{"pv", "uv"},
			[]string{"updated_at"},
		)
		for statDate, d := range dailySnap {
			if d.pv <= 0 && d.uv <= 0 {
				continue
			}
			if _, e := tx.ExecContext(ctx, upsertDaily, statDate, d.pv, d.uv, now); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		const maxFailedRestoreKeys = 4096
		// Restore deltas (bounded to maxFailedRestoreKeys) so transient lock/IO errors neither drop counts nor grow unbounded.
		b.mu.Lock()
		var restoredArtViews int64
		for k, v := range artSnap {
			if _, ok := b.articleViews[k]; ok || len(b.articleViews) < maxFailedRestoreKeys {
				b.articleViews[k] += v
				if v > 0 {
					restoredArtViews += v
				}
			}
		}
		if restoredArtViews > 0 {
			b.totalPendingArticleViews.Add(restoredArtViews)
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
			if _, ok := b.topItems[k]; ok {
				b.topItems[k] += v
			} else if len(b.topItems) < maxFailedRestoreKeys {
				if k.kind == "referer" {
					if b.refererKeys >= maxBufferedRefererKeys {
						continue
					}
					b.refererKeys++
				}
				b.topItems[k] = v
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
	b.refererKeys = 0
	b.dirty = false
	b.totalPendingArticleViews.Store(0)
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
	upsert := tx.Dialect().UpsertIncrementSQL(
		"analytics_top_items",
		[]string{"stat_date", "kind", "item_key", "hits"},
		[]string{"stat_date", "kind", "item_key"},
		[]string{"hits"},
		nil,
	)
	_, err := tx.ExecContext(ctx, upsert, statDate, kind, itemKey, delta)
	return err
}
