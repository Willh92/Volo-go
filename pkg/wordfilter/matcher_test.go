package wordfilter

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
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
		{"傻\u0336逼", "profanity"},
		{"f\u0336o\u0336r\u0336b\u0336i\u0336d\u0336d\u0336e\u0336n", "english"},
		{"禁\u3164词", "profanity"},
		{"禁\u034f词", "profanity"},
		{"𝟏𝟎𝟎𝐛𝐛直播", "gambling"},
		{"[点击访问](https://赌-博-网-站.example/promo)", "gambling"},
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
		"if (x < 10 && arr[0] > 1) { return true; }",
	}

	for _, text := range mustAllow {
		if res := m.Match(text); res.Matched {
			t.Errorf("false positive on %q: matched word=%q category=%q", text, res.Word, res.Category)
		}
	}
}

func TestMatcher_UnicodeImpersonationAndKanaPreservation(t *testing.T) {
	m := Compile(Options{
		Groups: []RuleGroup{
			{Category: "reserved", Words: []string{"admin", "root", "かき"}},
		},
		IgnoreSeparators: true,
		IgnoreRepeat:     true,
	})

	spoofed := []string{
		"𝐚𝐝𝐦𝐢𝐧",
		"𝑎𝑑𝑚𝑖𝑛",
		"𝓪𝓭𝓶𝓲𝓷",
		"𝔞𝔡𝔪𝔦𝔫",
		"𝕒𝕕𝕞𝕚𝕟",
		"𝖺𝖽𝗆𝗂𝗇",
		"𝗮𝗱𝗺𝗶𝗻",
		"𝚊𝚍𝚖𝚒𝚗",
		"🅰🅳🅼🅸🅽",
		"🇦🇩🇲🇮🇳",
		"ℛℴℴ𝓉",
		"a\u0336d\u0336m\u0336i\u0336n\u0336",
		"\u0430dmin", // Cyrillic 'а'
		"\u0391dmin", // Greek 'Α'
		"r\u043e\u043et", // Cyrillic 'о'
	}
	for _, s := range spoofed {
		if res := m.Match(s); !res.Matched || res.Category != "reserved" {
			t.Errorf("expected spoofed nickname %q to match reserved category, got %+v", s, res)
		}
	}

	// Voiced Japanese Kana (NFD か + U+3099 dakuten) must NOT collapse to unvoiced かき.
	if m.Contains("か\u3099き") {
		t.Fatal("expected Japanese Kana dakuten U+3099 to be preserved rather than stripped")
	}
}

func TestBundledCompactHoubbDictionaryEquivalence(t *testing.T) {
	dictPath := filepath.Join("..", "..", "plugins", "sensitive-words", "dicts", "houbb_base.txt.gz")
	f, err := os.Open(dictPath)
	if err != nil {
		t.Fatalf("open %s: %v", dictPath, err)
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	var rawBuf bytes.Buffer
	if _, err := rawBuf.ReadFrom(zr); err != nil {
		t.Fatalf("decompress %s: %v", dictPath, err)
	}
	_ = zr.Close()

	tagMap, err := LoadHoubbTagMap(bytes.NewReader(rawBuf.Bytes()))
	if err != nil {
		t.Fatalf("LoadHoubbTagMap: %v", err)
	}
	if len(tagMap) < 40000 {
		t.Fatalf("expected >= 40,000 tagged entries in compact houbb_base.txt.gz, got %d", len(tagMap))
	}

	bld := NewBuilder(BuilderOptions{IgnoreSeparators: true, IgnoreRepeat: true})
	bld.Reserve(65536, 64)
	added, dropped, err := bld.AddHoubbStream(bytes.NewReader(rawBuf.Bytes()), nil)
	if err != nil {
		t.Fatalf("AddHoubbStream: %v", err)
	}
	if dropped != 0 || added < 60000 {
		t.Fatalf("unexpected compact stream stats: added=%d dropped=%d", added, dropped)
	}
	m := bld.Build()
	if res := m.Match("08宪章"); !res.Matched || res.Category != CategoryPolitics {
		t.Fatalf("expected '08宪章' -> politics, got %+v", res)
	}
	if res := m.Match("澳門百家樂"); !res.Matched || res.Word != "百家乐" {
		t.Fatalf("expected '澳門百家樂' -> '百家乐', got %+v", res)
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

func TestMaskText_Comprehensive(t *testing.T) {
	var nilMatcher *Matcher
	if got := nilMatcher.MaskText("原样返回"); got != "原样返回" {
		t.Fatalf("nil matcher mutated text: %q", got)
	}

	m := Compile(Options{
		Deny:             []string{"傻逼", "傻瓜", "赌博平台", "管理员", "系统管理员", "admin", "ass", "bad word"},
		Allow:            []string{"傻瓜相机", "傻瓜式"},
		IgnoreSeparators: true,
		IgnoreRepeat:     true,
	})

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "clean", in: "正常的技术文章讨论，class 与 pass 不受影响", want: "正常的技术文章讨论，class 与 pass 不受影响"},
		{name: "direct_cjk", in: "这个人真是傻逼行为", want: "这个人真是**行为"},
		{name: "traditional_hanzi", in: "遠離賭博平台詐騙", want: "遠離****詐騙"},
		{name: "separator_piercing", in: "别发傻*逼和赌·博·平·台", want: "别发***和*******"},
		{name: "whitespace_preserved", in: "加 赌 博 平 台\n还有 bad word", want: "加 * * * *\n还有 *** ****"},
		{name: "repeat_collapsing", in: "傻傻傻逼逼", want: "*****"},
		{name: "allowlist_preserved", in: "买了傻瓜相机，但不是傻瓜", want: "买了傻瓜相机，但不是**"},
		{name: "overlapping_longest", in: "冒充系统管理员或管理员", want: "冒充*****或***"},
		{name: "ascii_boundaries", in: "classic pass vs bad ass and ADMIN!", want: "classic pass vs bad *** and *****!"},
		{name: "homoglyph_and_math_bold", in: "伪装 аdmin 与 𝐚𝐝𝐦𝐢𝐧 账号", want: "伪装 ***** 与 ***** 账号"},
		{name: "zero_width_and_zalgo", in: "测试傻\u200b逼与傻\u0300\u0301逼\u0302结束", want: "测试**与**结束"},
		{name: "emoji_and_nfd_preserved", in: "❤️😊傻逼👍 café\u0301", want: "❤️😊**👍 café\u0301"},
	}

	for _, tc := range cases {
		got := m.MaskText(tc.in)
		if !utf8.ValidString(got) {
			t.Fatalf("[%s] invalid UTF-8 output: %q", tc.name, got)
		}
		if got != tc.want {
			t.Errorf("[%s] MaskText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestMaskHTML_ComprehensiveAndSafety(t *testing.T) {
	m := Compile(Options{
		Deny:             []string{"傻逼", "傻瓜", "赌博", "porn", "ass"},
		Allow:            []string{"傻瓜相机"},
		IgnoreSeparators: true,
		IgnoreRepeat:     true,
	})

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "clean_html_fast_path",
			in:   `<p class="ass-card">Hello &amp; welcome to <a href="https://example.com/porn-filter?x=1&amp;y=2">傻瓜相机评测</a>!</p>`,
			want: `<p class="ass-card">Hello &amp; welcome to <a href="https://example.com/porn-filter?x=1&amp;y=2">傻瓜相机评测</a>!</p>`,
		},
		{
			name: "preserves_tags_and_attributes_masks_text",
			in:   `<p class="ass"><a href="https://example.com/porn" title="a > b">点击赌博</a> &amp; 傻逼</p>`,
			want: `<p class="ass"><a href="https://example.com/porn" title="a > b">点击**</a> &amp; **</p>`,
		},
		{
			name: "cross_inline_tags_split_word",
			in:   `<p>这种傻<strong>逼</strong>和<em>赌</em><code>博</code>都该过滤</p>`,
			want: `<p>这种*<strong>*</strong>和<em>*</em><code>*</code>都该过滤</p>`,
		},
		{
			name: "html_entities_and_xss_escaped_text",
			in:   `<p>&lt;script&gt;alert(1)&lt;/script&gt; &#20667;&#36924; &amp; Tom</p>`,
			want: `<p>&lt;script&gt;alert(1)&lt;/script&gt; ** &amp; Tom</p>`,
		},
		{
			name: "double_escaped_entities_roundtrip",
			in:   `<p>&amp;lt;img src=x onerror=alert(1)&amp;gt; 傻逼</p>`,
			want: `<p>&amp;lt;img src=x onerror=alert(1)&amp;gt; **</p>`,
		},
		{
			name: "non_tag_angle_brackets_and_malformed_markup",
			in:   `<p>1 &lt; 2 and 3 < 4 傻逼 <unclosed</p>`,
			want: `<p>1 &lt; 2 and 3 &lt; 4 ** &lt;unclosed</p>`,
		},
		{
			name: "block_boundaries_preserve_ascii_words",
			in:   `<p>cl</p><p>ass</p><p>p</p><p>ass</p>`,
			want: `<p>cl</p><p>***</p><p>p</p><p>***</p>`,
		},
	}

	for _, tc := range cases {
		got := m.MaskHTML(tc.in)
		if !utf8.ValidString(got) {
			t.Fatalf("[%s] invalid UTF-8 HTML output: %q", tc.name, got)
		}
		if got != tc.want {
			t.Errorf("[%s] MaskHTML(%q)\n got: %s\nwant: %s", tc.name, tc.in, got, tc.want)
		}
	}
}

