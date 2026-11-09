package wordfilter

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestACStateStructSizeAndPointerFree(t *testing.T) {
	if got := unsafe.Sizeof(acState{}); got != 16 {
		t.Fatalf("expected acState size to be exactly 16 bytes, got %d", got)
	}
}

func TestMatcher_AntiBypassAndAllowlistParity(t *testing.T) {
	m := Compile(Options{
		Groups: []RuleGroup{
			{Category: "profanity", Words: []string{"傻逼", "禁词", "傻b"}},
			{Category: "gambling", Words: []string{"赌博网站", "娱乐城", "卖淫", "脑残", "100bb"}},
			{Category: "english", Words: []string{"forbidden", "ass", "ab12"}},
		},
		Allow:            []string{"非禁词示例", "傻瓜相机"},
		IgnoreSeparators: true,
		IgnoreRepeat:     true,
	})

	if m.WordCount() != 11 || m.AllowCount() != 2 {
		t.Fatalf("unexpected counts: words=%d allow=%d", m.WordCount(), m.AllowCount())
	}

	mustBlock := []struct {
		text string
		cat  string
	}{
		{"你就是个傻逼", "profanity"},
		{"包含禁词的留言", "profanity"},
		{"ＦＯＲＢＩＤＤＥＮ", "english"},
		{"FORBIDDEN", "english"},
		{"禁\u200b词", "profanity"},
		{"禁**词**", "profanity"},
		{"禁`词`", "profanity"},
		{"[禁](https://example.test)词", "profanity"},
		{"禁<span>词</span>", "profanity"},
		{"&#x7981;词", "profanity"},
		{"傻*逼", "profanity"},
		{"傻 . 逼", "profanity"},
		{"傻傻傻逼逼", "profanity"},
		{"傻*傻*逼*逼", "profanity"},
		{"你真傻b！", "profanity"},
		{"這是一個賭博網站推廣", "gambling"},
		{"澳門娛樂城上線", "gambling"},
		{"組織賣淫違法", "gambling"},
		{"真是腦殘言論", "gambling"},
		{"⒈⓪⓪⒝⒝视频", "gambling"},
		{"you are an ass!", "english"},
		{"test ⒜Ⓑ❶₂ token", "english"},
		{"非禁词示例，但是 forbidden 仍需拦截。", "english"},
		{"非禁词示例，另外出现禁词。", "profanity"},
	}

	for _, tc := range mustBlock {
		res := m.Match(tc.text)
		if !res.Matched {
			t.Errorf("expected %q to be blocked, but it passed", tc.text)
			continue
		}
		if res.Category != tc.cat {
			t.Errorf("for %q expected category %q, got %q (word=%q)", tc.text, tc.cat, res.Category, res.Word)
		}
	}

	mustAllow := []string{
		"The Java class compiles successfully.",
		"pass the glass of bass and book a coffee",
		"Java 17 / Nginx 配置：https://example.test/，邮箱 user@example.test，端口 8080，IP 192.0.2.1。",
		"这是非禁词示例。",
		"这也是非*禁*词*示*例。",
		"购买了一台傻瓜相机拍照",
		"傻傻瓜瓜相机",
		"傻非禁词示例逼", // Allowlist must never splice surrounding runes together!
		"他是个傻boy哈哈", // Trailing ASCII boundary protects "傻b" from matching "傻boy"
		"测试一条正常的留言",
	}

	for _, text := range mustAllow {
		if res := m.Match(text); res.Matched {
			t.Errorf("false positive on %q: matched word=%q category=%q", text, res.Word, res.Category)
		}
	}
}

// TestMatcher_AllowlistOverlapDoesNotBlindOverlappingDeny verifies our improvement over
// houbb/sensitive-word's index-jumping bug: when allowlist "曹操" ([0,2)) overlaps deny "操他妈" ([1,4)),
// "曹操孟德" is allowed while "曹操他妈" is still caught.
func TestMatcher_AllowlistOverlapDoesNotBlindOverlappingDeny(t *testing.T) {
	m := Compile(Options{
		Deny:             []string{"操", "操他妈"},
		Allow:            []string{"曹操"},
		IgnoreSeparators: true,
	})
	if m.Contains("三国英雄曹操孟德") {
		t.Fatal("expected '曹操孟德' to be exempted by allowlist '曹操'")
	}
	if !m.Contains("曹操他妈") {
		t.Fatal("expected overlapping '曹操他妈' to be blocked despite '曹操' prefix allowlist")
	}
}

func TestHoubbConvertAndFilteredStreaming(t *testing.T) {
	tagData := strings.Join([]string{
		"08宪章 0",
		"0售k粉qq 1,4",
		"成人小电影 2",
		"澳门百家乐 3",
		"代开发票微信 4",
	}, "\n")
	tagMap, err := LoadHoubbTagMap(strings.NewReader(tagData))
	if err != nil {
		t.Fatalf("LoadHoubbTagMap: %v", err)
	}

	rawUpstream := strings.Join([]string{
		"\ufeff# Upstream houbb sensitive_word_dict.txt sample",
		"08宪章",
		"0售k粉qq",
		"成人小电影",
		"澳门百家乐",
		"代开发票微信",
		"未分类违规词",
		"2kshr7b\x16yrwq", // Corrupted control-byte noise in upstream houbb
		"b\x7f",           // Corrupted DEL noise in upstream houbb
		"duck",            // Upstream English false-positive noise
		"gender",          // Upstream English false-positive noise
		"fuck",            // Legitimate English profanity
		"!生日快乐",
	}, "\n")

	stats, gzBytes, err := ConvertHoubbPayload([]byte(rawUpstream), tagMap)
	if err != nil {
		t.Fatalf("ConvertHoubbPayload: %v", err)
	}
	if stats.WordCount != 7 || stats.AllowCount != 1 || stats.TaggedCount != 5 || stats.DroppedCount != 4 {
		t.Fatalf("unexpected ConvertHoubbStats: %+v", stats)
	}

	// Stream into Builder with politics ("politics") disabled; all other categories enabled.
	zr, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()

	b := NewBuilder(BuilderOptions{IgnoreSeparators: true, IgnoreRepeat: true})
	_, _, err = b.AddHoubbStream(zr, map[string]bool{
		CategoryPolitics:  false,
		CategoryNarcotics: true,
		CategoryPorn:      true,
		CategoryGambling:  true,
		CategoryCrime:     true,
		CategoryGeneral:   true,
	})
	if err != nil {
		t.Fatalf("AddHoubbStream: %v", err)
	}
	m := b.Build()

	if m.Contains("讨论08宪章") {
		t.Error("expected disabled politics category word to be skipped")
	}
	if m.Contains("祝你生日快乐") {
		t.Error("expected !生日快乐 allowlist word to pass")
	}
	if m.Contains("duck typing and gender field") {
		t.Error("expected upstream English false positives (duck, gender) to be filtered out")
	}
	if res := m.Match("澳門百家樂平台"); !res.Matched || res.Category != CategoryGambling {
		t.Errorf("expected Traditional Chinese '澳門百家樂' to match gambling, got %+v", res)
	}
	if res := m.Match("0售K粉QQ联系"); !res.Matched || res.Category != CategoryNarcotics {
		t.Errorf("expected multi-tag '0售K粉QQ' to match narcotics, got %+v", res)
	}
	if res := m.Match("what the FUCK!"); !res.Matched || res.Category != CategoryGeneral {
		t.Errorf("expected 'FUCK' to match general, got %+v", res)
	}
}

func TestParseWordLines(t *testing.T) {
	raw := `
# Comment line
// Another comment
赌博平台, 刷单兼职，色情网站
!博彩业历史
@allow:黄色预警
正常敏感词
`
	deny, allow := ParseWordLines(raw)
	if len(deny) != 4 {
		t.Fatalf("expected 4 deny words, got %d (%v)", len(deny), deny)
	}
	if len(allow) != 2 || allow[0] != "博彩业历史" || allow[1] != "黄色预警" {
		t.Fatalf("unexpected allow words: %v", allow)
	}
}

func TestExactStateCapacityZeroSlack(t *testing.T) {
	bld := NewBuilder(BuilderOptions{IgnoreSeparators: true, IgnoreRepeat: true})
	bld.Reserve(64_500, 32)
	for i := 0; i < 64_500; i++ {
		bld.AddDeny(fmt.Sprintf("违规敏感测试样本词库条目%05d", i), CategoryCrime)
	}
	bld.AddDeny("违规敏感测试样本词库条目00001", CategoryCrime) // duplicate entry
	bld.AddAllow("正常技术术语")
	bld.AddAllow("开源软件架构")

	m := bld.Build()
	if m.WordCount() != 64_500 || m.AllowCount() != 2 {
		t.Fatalf("unexpected counts: words=%d allow=%d", m.WordCount(), m.AllowCount())
	}
	if len(m.deny.states) != cap(m.deny.states) {
		t.Fatalf("expected zero state slice slack on 64k deny automaton: len=%d cap=%d", len(m.deny.states), cap(m.deny.states))
	}
	if len(m.allow.states) != cap(m.allow.states) {
		t.Fatalf("expected zero state slice slack on allow automaton: len=%d cap=%d", len(m.allow.states), cap(m.allow.states))
	}
}

func TestConvertHoubbStream_MaxBytesEnforcement(t *testing.T) {
	oversized := strings.Repeat("敏感词行样本\n", 200)
	_, _, err := ConvertHoubbStream(strings.NewReader(oversized), 256, nil)
	if err != ErrHoubbPayloadTooLarge {
		t.Fatalf("expected ErrHoubbPayloadTooLarge, got %v", err)
	}
}

func BenchmarkCompile_64kWords(b *testing.B) {
	words := make([]string, 64_500)
	for i := range words {
		words[i] = fmt.Sprintf("违规敏感测试样本词库条目%05d", i)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bld := NewBuilder(BuilderOptions{IgnoreSeparators: true, IgnoreRepeat: true})
		bld.Reserve(len(words), 4)
		for _, w := range words {
			bld.AddDeny(w, CategoryCrime)
		}
		bld.AddAllow("正常技术术语")
		m := bld.Build()
		if m.WordCount() != len(words) {
			b.Fatal("unexpected word count")
		}
	}
}

func BenchmarkMatcher_64kWords_ScanClean4K(b *testing.B) {
	bld := NewBuilder(BuilderOptions{IgnoreSeparators: true, IgnoreRepeat: true})
	bld.Reserve(64_500, 4)
	for i := 0; i < 64_500; i++ {
		bld.AddDeny(fmt.Sprintf("违规敏感测试样本词库条目%05d", i), CategoryCrime)
	}
	bld.AddAllow("正常技术术语")
	bld.AddAllow("开源软件架构")
	m := bld.Build()
	runtime.GC()

	comment := strings.Repeat("这是一段正常的技术博客读者评论，探讨 Go 语言高并发架构、SQLite WAL 模式以及静态编译部署实践。The Java class compiles cleanly. ", 30)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if m.Contains(comment) {
			b.Fatal("unexpected match")
		}
	}
}
