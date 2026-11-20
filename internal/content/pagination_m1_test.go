package content

import (
	"fmt"
	"testing"
	"time"
)

func TestM1PaginationAndQueryServices(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// 1. Seed 8 categories + published articles for SearchPage and CategoryUsageSummary.
	for i := 0; i < 8; i++ {
		cid := fmt.Sprintf("c_%02d", i)
		name := fmt.Sprintf("Alpha Cat %02d", i)
		if i >= 5 {
			name = fmt.Sprintf("Beta Cat %02d", i)
		}
		if _, err := store.ExecContext(ctx,
			`INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at) VALUES (?, ?, ?, '', ?, ?, ?)`,
			cid, name, fmt.Sprintf("cat-%02d", i), i, now, now); err != nil {
			t.Fatalf("seed cat %d: %v", i, err)
		}
		// Give each category (8-i) published articles so ordering by count DESC is deterministic.
		for a := 0; a < 8-i; a++ {
			aid := fmt.Sprintf("art_%02d_%02d", i, a)
			if _, err := store.ExecContext(ctx, `INSERT INTO articles (
				id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
				cover_url, status, published_at, allow_comment, password_ciphertext,
				audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
			) VALUES (?, 'u_adm', ?, ?, ?, '', '', 'body', '', 'published', ?, 1, '', '', '', 'en', '', '', 10, 1, ?, ?)`,
				aid, cid, aid, aid, now+int64(a), now, now); err != nil {
				t.Fatalf("seed article: %v", err)
			}
		}
	}

	alphaP1, alphaTotal, err := svc.ListCategoriesWithCountsSearchPage(ctx, "alpha", 1, 2)
	if err != nil || alphaTotal != 5 || len(alphaP1) != 2 || alphaP1[0].ArticleCount != 8 {
		t.Fatalf("unexpected alpha search p1: total=%d len=%d items=%+v err=%v", alphaTotal, len(alphaP1), alphaP1, err)
	}
	alphaP3, _, err := svc.ListCategoriesWithCountsSearchPage(ctx, "alpha", 3, 2)
	if err != nil || len(alphaP3) != 1 || alphaP3[0].ID != "c_04" {
		t.Fatalf("unexpected alpha search p3: items=%+v err=%v", alphaP3, err)
	}

	usage, othersCnt, err := svc.ListCategoryUsageSummaryWithOthers(ctx, 5)
	if err != nil || len(usage) != 5 {
		t.Fatalf("expected 5 real positive categories, got len=%d usage=%+v err=%v", len(usage), usage, err)
	}
	for _, u := range usage {
		if u.ID == "__others__" {
			t.Fatalf("ListCategoryUsageSummary must not emit synthetic __others__: %+v", usage)
		}
	}
	if othersCnt != (3 + 2 + 1) {
		t.Fatalf("expected othersCnt=6, got %d", othersCnt)
	}
	usage50, err := svc.ListCategoryUsageSummary(ctx, 50)
	if err != nil || len(usage50) != 8 {
		t.Fatalf("expected all 8 real categories within limit 50, got len=%d err=%v", len(usage50), err)
	}

	// Verify >50 categories caps ListCategoryUsageSummary at 50 real entries and computes exact othersCnt.
	for i := 8; i < 55; i++ {
		cid := fmt.Sprintf("c_%02d", i)
		if _, err := store.ExecContext(ctx,
			`INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at) VALUES (?, ?, ?, '', ?, ?, ?)`,
			cid, fmt.Sprintf("Extra Cat %02d", i), fmt.Sprintf("extra-cat-%02d", i), i, now, now); err != nil {
			t.Fatalf("seed extra cat %d: %v", i, err)
		}
		aid := fmt.Sprintf("art_extra_%02d", i)
		if _, err := store.ExecContext(ctx, `INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, published_at, allow_comment, password_ciphertext,
			audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
		) VALUES (?, 'u_adm', ?, ?, ?, '', '', 'body', '', 'published', ?, 1, '', '', '', 'en', '', '', 10, 1, ?, ?)`,
			aid, cid, aid, aid, now+int64(i), now, now); err != nil {
			t.Fatalf("seed extra article %d: %v", i, err)
		}
	}
	usageCap, othersCap, err := svc.ListCategoryUsageSummaryWithOthers(ctx, 50)
	if err != nil || len(usageCap) != 50 || othersCap != 5 {
		t.Fatalf("expected 50 bounded real categories + 5 others, got len=%d others=%d err=%v", len(usageCap), othersCap, err)
	}
	for _, u := range usageCap {
		if u.ID == "__others__" || u.ArticleCount <= 0 {
			t.Fatalf("unexpected synthetic/non-positive item in bounded summary: %+v", u)
		}
	}

	// 2. Seed links across multi-category buckets + an oversized category (> 48) for ListPublicLinkGroupsPage.
	linkIdx := 0
	insertLink := func(cat, title string, sort int) {
		linkIdx++
		if _, err := store.ExecContext(ctx,
			`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
			 VALUES (?, ?, ?, '', '', ?, ?, 'ok', 200, 10, '', 0, ?, ?)`,
			fmt.Sprintf("lk_%03d", linkIdx), title, fmt.Sprintf("https://example.com/%d", linkIdx), cat, sort, now+int64(linkIdx), now); err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}
	// Group A: 30 links, Group B: 30 links -> 30+30=60 > 48, so Group A on page 1 and Group B on page 2 WITHOUT splitting Group B!
	for i := 0; i < 30; i++ {
		insertLink("Group A", fmt.Sprintf("A Link %02d", i), 10)
	}
	for i := 0; i < 30; i++ {
		title := fmt.Sprintf("B Link %02d", i)
		if i == 25 {
			title = "NeedleOnPage2"
		}
		insertLink("Group B", title, 20)
	}
	// Group Big: 55 links -> chunks into 48 + 7 across pages 3 and 4.
	for i := 0; i < 55; i++ {
		insertLink("Group Big", fmt.Sprintf("Big Link %02d", i), 30)
	}

	gP1, cats, totalLinks, totalPages, clamped, err := svc.ListPublicLinkGroupsPage(ctx, "", "", "Links", 1, 48)
	if err != nil || totalLinks != 115 || totalPages != 4 || clamped != 1 || len(cats) != 3 {
		t.Fatalf("unexpected links p1 meta: total=%d pages=%d clamped=%d cats=%v err=%v", totalLinks, totalPages, clamped, cats, err)
	}
	if len(gP1) != 1 || gP1[0].Category != "Group A" || len(gP1[0].Items) != 30 {
		t.Fatalf("expected Group A unsplit (30) on p1, got %+v", gP1)
	}
	gP2, _, _, _, _, _ := svc.ListPublicLinkGroupsPage(ctx, "", "", "Links", 2, 48)
	if len(gP2) != 1 || gP2[0].Category != "Group B" || len(gP2[0].Items) != 30 {
		t.Fatalf("expected Group B unsplit (30) on p2, got %+v", gP2)
	}
	gP3, _, _, _, _, _ := svc.ListPublicLinkGroupsPage(ctx, "", "", "Links", 3, 48)
	gP4, _, _, _, _, _ := svc.ListPublicLinkGroupsPage(ctx, "", "", "Links", 4, 48)
	if len(gP3) != 1 || len(gP3[0].Items) != 48 || len(gP4) != 1 || len(gP4[0].Items) != 7 {
		t.Fatalf("expected Group Big chunked 48 + 7, got p3=%d p4=%d", len(gP3[0].Items), len(gP4[0].Items))
	}
	// Search query must find NeedleOnPage2 directly on page 1.
	gSearch, _, searchTotal, searchPages, _, _ := svc.ListPublicLinkGroupsPage(ctx, "NeedleOnPage2", "", "Links", 1, 48)
	if searchTotal != 1 || searchPages != 1 || len(gSearch) != 1 || gSearch[0].Items[0].Title != "NeedleOnPage2" {
		t.Fatalf("unexpected link search result: total=%d groups=%+v", searchTotal, gSearch)
	}

	// 3. Seed a book with 250 chapters across volumes for GetBookReaderPage.
	if _, err := store.ExecContext(ctx,
		`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at)
		 VALUES ('bk_big', 'Big Handbook', 'big-handbook', 'Desc', '', 'external', 'https://docs.example.com', '', 'inline', 'reading', 0, ?, 1, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed book: %v", err)
	}
	// Row 0: Volume 1 header (sort_order=0, url="")
	// Rows 1..149: chapters inside Volume 1 (depth=1)
	// Row 150: Volume 2 header (sort_order=150, url="")
	// Rows 151..249: chapters inside Volume 2 (depth=1)
	for i := 0; i < 250; i++ {
		chID := fmt.Sprintf("ch_%03d", i)
		title := fmt.Sprintf("Chapter %03d", i)
		urlVal := fmt.Sprintf("https://docs.example.com/ch-%03d", i)
		depth := 1
		parent := "ch_000"
		if i == 0 || i == 150 {
			title = fmt.Sprintf("Volume %d", i/150+1)
			urlVal = ""
			depth = 0
			parent = ""
		} else if i > 150 {
			parent = "ch_150"
		}
		if _, err := store.ExecContext(ctx,
			`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, 'bk_big', ?, ?, ?, ?, ?)`,
			chID, parent, title, urlVal, depth, i); err != nil {
			t.Fatalf("seed chapter %d: %v", i, err)
		}
	}

	// Request ch_105 without cpage -> should default to cpage=2 (chapters 100..199) AND prepend enclosing Volume 1 header (ch_000)!
	rd, err := svc.GetBookReaderPage(ctx, "big-handbook", "ch_105", 0, 100)
	if err != nil {
		t.Fatalf("GetBookReaderPage: %v", err)
	}
	if rd.Book.ChapterCount != 250 || rd.ChapterPage != 2 || rd.TotalPages != 3 {
		t.Fatalf("unexpected book page metadata: count=%d cpage=%d totalPages=%d", rd.Book.ChapterCount, rd.ChapterPage, rd.TotalPages)
	}
	if rd.ActiveChapter == nil || rd.ActiveChapter.ID != "ch_105" {
		t.Fatalf("expected active ch_105, got %+v", rd.ActiveChapter)
	}
	if rd.PrevChapter == nil || rd.PrevChapter.ID != "ch_104" || rd.NextChapter == nil || rd.NextChapter.ID != "ch_106" {
		t.Fatalf("unexpected neighbors: prev=%+v next=%+v", rd.PrevChapter, rd.NextChapter)
	}
	if len(rd.Book.FlatChapters) != 101 || rd.Book.FlatChapters[0].ID != "ch_000" || rd.Book.FlatChapters[1].ID != "ch_100" {
		t.Fatalf("expected prepended Volume 1 header + 100 window chapters, got len=%d first=%+v second=%+v",
			len(rd.Book.FlatChapters), rd.Book.FlatChapters[0], rd.Book.FlatChapters[1])
	}

	// Boundary chapter ch_100: PrevChapter must seamlessly point to ch_099 (on page 1).
	rdBound, err := svc.GetBookReaderPage(ctx, "big-handbook", "ch_100", 0, 100)
	if err != nil || rdBound.PrevChapter == nil || rdBound.PrevChapter.ID != "ch_099" || rdBound.NextChapter == nil || rdBound.NextChapter.ID != "ch_101" {
		t.Fatalf("boundary neighbor failed: rd=%+v err=%v", rdBound, err)
	}

	// 4. Nested-volume book: Prev/Next skips empty-URL volume headers and cpage=2 preserves SubVolume 1.2 + Volume 1.
	if _, err := store.ExecContext(ctx,
		`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at)
		 VALUES ('bk_nested', 'Nested Book', 'nested-book', 'Desc', '', 'external', 'https://docs.example.com', '', 'iframe', 'reading', 0, ?, 2, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed nested book: %v", err)
	}
	for i := 0; i < 260; i++ {
		chID := fmt.Sprintf("nch_%03d", i)
		title := fmt.Sprintf("Chap %03d", i)
		urlVal := fmt.Sprintf("https://docs.example.com/nch-%03d", i)
		depth := 2
		parent := "nch_001"
		switch {
		case i == 0:
			title, urlVal, depth, parent = "Volume 1", "", 0, ""
		case i == 1:
			title, urlVal, depth, parent = "SubVolume 1.1", "", 1, "nch_000"
		case i == 61:
			title, urlVal, depth, parent = "SubVolume 1.2", "", 1, "nch_000"
		case i > 61 && i <= 140:
			parent = "nch_061"
		case i == 141:
			title, urlVal, depth, parent = "Volume 2", "", 0, ""
		case i >= 142:
			depth, parent = 1, "nch_141"
		}
		if _, err := store.ExecContext(ctx,
			`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, 'bk_nested', ?, ?, ?, ?, ?)`,
			chID, parent, title, urlVal, depth, i); err != nil {
			t.Fatalf("seed nested chapter %d: %v", i, err)
		}
	}

	rdNested142, err := svc.GetBookReaderPage(ctx, "nested-book", "nch_142", 0, 100)
	if err != nil {
		t.Fatalf("GetBookReaderPage nch_142: %v", err)
	}
	if rdNested142.PrevChapter == nil || rdNested142.PrevChapter.ID != "nch_140" {
		t.Fatalf("expected nch_142 PrevChapter == nch_140 (skipping empty-URL nch_141), got %+v", rdNested142.PrevChapter)
	}
	if rdNested142.NextChapter == nil || rdNested142.NextChapter.ID != "nch_143" {
		t.Fatalf("expected nch_142 NextChapter == nch_143, got %+v", rdNested142.NextChapter)
	}

	rdNested140, err := svc.GetBookReaderPage(ctx, "nested-book", "nch_140", 0, 100)
	if err != nil {
		t.Fatalf("GetBookReaderPage nch_140: %v", err)
	}
	if rdNested140.NextChapter == nil || rdNested140.NextChapter.ID != "nch_142" {
		t.Fatalf("expected nch_140 NextChapter == nch_142 (skipping empty-URL nch_141), got %+v", rdNested140.NextChapter)
	}

	rdNestedPage2, err := svc.GetBookReaderPage(ctx, "nested-book", "", 2, 100)
	if err != nil {
		t.Fatalf("GetBookReaderPage cpage=2: %v", err)
	}
	if len(rdNestedPage2.Book.FlatChapters) < 3 ||
		rdNestedPage2.Book.FlatChapters[0].ID != "nch_000" ||
		rdNestedPage2.Book.FlatChapters[1].ID != "nch_061" ||
		rdNestedPage2.Book.FlatChapters[2].ID != "nch_100" {
		t.Fatalf("expected cpage=2 to prepend Volume 1 (nch_000) and SubVolume 1.2 (nch_061) before nch_100, got first 3=%+v",
			rdNestedPage2.Book.FlatChapters[:min(3, len(rdNestedPage2.Book.FlatChapters))])
	}
}
