package visibility

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/render"
)

// DefaultRenderCacheMaxBytes bounds resident Markdown card/detail/feed render cache memory to <= 1.5 MiB,
// ensuring Volo-go stays comfortably within its < 25 MiB RSS budget while eliminating hot-path re-parsing.
const DefaultRenderCacheMaxBytes = 1536 << 10

type cachedCardMeta struct {
	summary        string
	coverURL       string
	wordCount      int
	readingMinutes int
}

type renderCacheValue struct {
	key    string
	cost   int
	card   *cachedCardMeta
	output *render.Output
}

type boundedRenderLRU struct {
	mu       sync.Mutex
	maxBytes int
	used     int
	ll       *list.List
	items    map[string]*list.Element
}

func newBoundedRenderLRU(maxBytes int) *boundedRenderLRU {
	if maxBytes <= 0 {
		maxBytes = DefaultRenderCacheMaxBytes
	}
	return &boundedRenderLRU{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[string]*list.Element, 256),
	}
}

func (c *boundedRenderLRU) getCard(key string) (cachedCardMeta, bool) {
	if c == nil {
		return cachedCardMeta{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		val := el.Value.(*renderCacheValue)
		if val.card != nil {
			return *val.card, true
		}
	}
	return cachedCardMeta{}, false
}

func (c *boundedRenderLRU) putCard(key string, meta cachedCardMeta) {
	if c == nil {
		return
	}
	cost := len(key) + len(meta.summary) + len(meta.coverURL) + 128
	c.put(key, &renderCacheValue{
		key:  key,
		cost: cost,
		card: &meta,
	})
}

func (c *boundedRenderLRU) getOutput(key string) (render.Output, bool) {
	if c == nil {
		return render.Output{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		val := el.Value.(*renderCacheValue)
		if val.output != nil {
			return *val.output, true
		}
	}
	return render.Output{}, false
}

func (c *boundedRenderLRU) putOutput(key string, out render.Output) {
	if c == nil {
		return
	}
	tocBytes := 0
	for _, t := range out.TOC {
		tocBytes += len(t.ID) + len(t.Text) + 24
	}
	cost := len(key) + len(out.HTML) + len(out.PlainText) + len(out.FirstImageURL) + tocBytes + 160
	cp := out
	if len(out.TOC) > 0 {
		cp.TOC = append([]render.TOCItem(nil), out.TOC...)
	}
	c.put(key, &renderCacheValue{
		key:    key,
		cost:   cost,
		output: &cp,
	})
}

func (c *boundedRenderLRU) put(key string, entry *renderCacheValue) {
	if entry.cost <= 0 || entry.cost > c.maxBytes/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.items[key]; ok {
		old := existing.Value.(*renderCacheValue)
		c.used -= old.cost
		existing.Value = entry
		c.used += entry.cost
		c.ll.MoveToFront(existing)
	} else {
		el := c.ll.PushFront(entry)
		c.items[key] = el
		c.used += entry.cost
	}

	for c.used > c.maxBytes && c.ll.Len() > 0 {
		back := c.ll.Back()
		if back == nil {
			break
		}
		ev := back.Value.(*renderCacheValue)
		delete(c.items, ev.key)
		c.ll.Remove(back)
		c.used -= ev.cost
		if c.used < 0 {
			c.used = 0
		}
	}
}

func (c *boundedRenderLRU) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.ll.Init()
	clear(c.items)
	c.used = 0
	c.mu.Unlock()
}

func articleDigestHex(raw domain.RawArticle) string {
	h := sha256.New()
	_, _ = h.Write([]byte(raw.SummaryMD))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.ContentMD))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.CoverURL))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(raw.AudioURL))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:12])
}

func cardCacheKey(raw domain.RawArticle, locale string, hasDims bool) string {
	return fmt.Sprintf("card|%s|%d|%s|%s|d=%t", raw.ID, raw.UpdatedAt, articleDigestHex(raw), locale, hasDims)
}

func outputCacheKey(raw domain.RawArticle, mode render.RenderMode, locale string, unlocked bool, isReader bool, lineNums bool, hasDims bool) string {
	return fmt.Sprintf("out|%s|%d|%s|%s|%s|u=%t|r=%t|ln=%t|d=%t",
		raw.ID, raw.UpdatedAt, articleDigestHex(raw), mode, locale, unlocked, isReader, lineNums, hasDims)
}
