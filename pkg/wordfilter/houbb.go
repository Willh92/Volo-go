package wordfilter

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// ErrHoubbPayloadTooLarge is returned when a streamed remote dictionary exceeds maxBytes.
var ErrHoubbPayloadTooLarge = errors.New("wordfilter: remote payload exceeds size limit")

const (
	CategoryPolitics  = "politics"  // houbb tag 0: 政治
	CategoryNarcotics = "narcotics" // houbb tag 1: 毒品
	CategoryPorn      = "porn"      // houbb tag 2: 色情
	CategoryGambling  = "gambling"  // houbb tag 3: 赌博
	CategoryCrime     = "crime"     // houbb tag 4: 违法犯罪 / 黑灰产
	CategoryGeneral   = "general"   // untagged entries in sensitive_word_dict.txt
)

// HoubbConvertStats summarizes an upstream dictionary conversion & compression pass.
type HoubbConvertStats struct {
	WordCount    int `json:"word_count"`
	AllowCount   int `json:"allow_count"`
	TaggedCount  int `json:"tagged_count"`
	DroppedCount int `json:"dropped_count"`
	GzipBytes    int `json:"gzip_bytes"`
}

// isUpstreamHoubbNoiseWord filters well-known everyday English false positives present in
// upstream houbb/sensitive-word-data (sensitive_word_dict_en.txt) with zero heap allocations.
func isUpstreamHoubbNoiseWord(trimmed string) bool {
	n := len(trimmed)
	if n < 4 || n > 7 || trimmed[0] >= 0x80 {
		return false
	}
	return strings.EqualFold(trimmed, "duck") ||
		strings.EqualFold(trimmed, "fowl") ||
		strings.EqualFold(trimmed, "gender") ||
		strings.EqualFold(trimmed, "chicken")
}

func hasCorruptControlBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < 0x20 && b != '\t') || b == 0x7f {
			return true
		}
	}
	return false
}

func isHoubbTagSpec(spec string) bool {
	if spec == "" || len(spec) > 32 {
		return false
	}
	hasDigit := false
	for i := 0; i < len(spec); i++ {
		b := spec[i]
		if b >= '0' && b <= '9' {
			hasDigit = true
			continue
		}
		if b == ',' {
			continue
		}
		return false
	}
	return hasDigit
}

// HoubbTagToCategory maps houbb WordTagType numeric codes ("0".."4") to canonical category names.
func HoubbTagToCategory(code string) string {
	switch strings.TrimSpace(code) {
	case "0":
		return CategoryPolitics
	case "1":
		return CategoryNarcotics
	case "2":
		return CategoryPorn
	case "3":
		return CategoryGambling
	case "4":
		return CategoryCrime
	default:
		return CategoryGeneral
	}
}

// splitHoubbLine parses a single line from sensitive_word_dict.txt ("word"),
// sensitive_word_tags.txt ("word 1,4"), or pre-converted compact format ("word\t1,4").
func splitHoubbLine(line string) (word string, tagSpec string, isAllow bool, ok bool) {
	s := strings.TrimSuffix(line, "\r")
	s = strings.TrimPrefix(s, "\ufeff")
	if s == "" || hasCorruptControlBytes(s) {
		return "", "", false, false
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
		return "", "", false, false
	}

	if strings.HasPrefix(trimmed, "!") && len(trimmed) > 1 {
		aw := strings.TrimSpace(trimmed[1:])
		if aw == "" || runeCountBounded(aw, MaxWordRunes+1) > MaxWordRunes {
			return "", "", false, false
		}
		return aw, "", true, true
	}
	if len(trimmed) > len("@allow:") && strings.EqualFold(trimmed[:len("@allow:")], "@allow:") {
		aw := strings.TrimSpace(trimmed[len("@allow:"):])
		if aw == "" || runeCountBounded(aw, MaxWordRunes+1) > MaxWordRunes {
			return "", "", false, false
		}
		return aw, "", true, true
	}

	// Format 1: explicit tab separator "<word>\t<tagCodes>"
	if tabIdx := strings.LastIndexByte(trimmed, '\t'); tabIdx > 0 {
		candTags := strings.TrimSpace(trimmed[tabIdx+1:])
		candWord := strings.TrimSpace(trimmed[:tabIdx])
		if candWord != "" && isHoubbTagSpec(candTags) {
			if runeCountBounded(candWord, MaxWordRunes+1) > MaxWordRunes || isUpstreamHoubbNoiseWord(candWord) {
				return "", "", false, false
			}
			return candWord, candTags, false, true
		}
	}

	// Format 2: houbb sensitive_word_tags.txt "<word> <digit[,digit...]>"
	if spIdx := strings.LastIndexByte(trimmed, ' '); spIdx > 0 {
		candTags := strings.TrimSpace(trimmed[spIdx+1:])
		candWord := strings.TrimSpace(trimmed[:spIdx])
		if candWord != "" && isHoubbTagSpec(candTags) {
			if runeCountBounded(candWord, MaxWordRunes+1) > MaxWordRunes || isUpstreamHoubbNoiseWord(candWord) {
				return "", "", false, false
			}
			return candWord, candTags, false, true
		}
	}

	// Format 3: plain sensitive_word_dict.txt "<word>"
	if runeCountBounded(trimmed, MaxWordRunes+1) > MaxWordRunes || isUpstreamHoubbNoiseWord(trimmed) {
		return "", "", false, false
	}
	return trimmed, "", false, true
}

func resolveEnabledCategory(tagSpec string, enabled map[string]bool) (string, bool) {
	if tagSpec == "" {
		if len(enabled) == 0 || enabled[CategoryGeneral] {
			return CategoryGeneral, true
		}
		return "", false
	}
	rem := tagSpec
	for len(rem) > 0 {
		var code string
		if comma := strings.IndexByte(rem, ','); comma >= 0 {
			code = rem[:comma]
			rem = rem[comma+1:]
		} else {
			code = rem
			rem = ""
		}
		cat := HoubbTagToCategory(code)
		if len(enabled) == 0 || enabled[cat] {
			return cat, true
		}
	}
	return "", false
}

func categoryNameToTagSpec(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", CategoryGeneral, "none", "untagged":
		return "", true
	case CategoryPolitics, "0":
		return "0", true
	case CategoryNarcotics, "1":
		return "1", true
	case CategoryPorn, "2":
		return "2", true
	case CategoryGambling, "3":
		return "3", true
	case CategoryCrime, "4":
		return "4", true
	default:
		if isHoubbTagSpec(name) {
			return internHoubbTagSpec(name), true
		}
		return "", false
	}
}

func decodeBase36Digit(b byte) (int, bool) {
	if b >= '1' && b <= '9' {
		return int(b - '0'), true
	}
	if b >= 'a' && b <= 'z' {
		return 10 + int(b-'a'), true
	}
	return 0, false
}

// scanHoubbStream decodes standard houbb lines ("word", "word 1,4", "word\t1,4", "!allow") as well as
// compact grouped/front-coded streams ("@tag:1,4", "@category:gambling", "^<1-9a-z><suffix>").
func scanHoubbStream(r io.Reader, onEntry func(word string, tagSpec string, isAllow bool)) (dropped int, err error) {
	if r == nil {
		return 0, nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 16*1024)

	var (
		lineNo        int
		compactMode   bool
		activeTagSpec string
		prevRunes     []rune
		runeBuf       []rune
	)

	for scanner.Scan() {
		rawLine := scanner.Text()
		if lineNo == 0 {
			rawLine = strings.TrimPrefix(rawLine, "\ufeff")
		}
		lineNo++
		rawLine = strings.TrimSuffix(rawLine, "\r")
		if rawLine == "" || rawLine[0] == '#' || strings.HasPrefix(rawLine, "//") {
			continue
		}

		// Handle @tag:<spec> and @category:<name> section headers (enables compact front-coded decoding).
		if rawLine[0] == '@' {
			trimmedDir := strings.TrimSpace(rawLine)
			if len(trimmedDir) >= len("@tag:") && strings.EqualFold(trimmedDir[:len("@tag:")], "@tag:") {
				spec := strings.TrimSpace(trimmedDir[len("@tag:"):])
				if spec == "" {
					activeTagSpec = ""
					compactMode = true
					prevRunes = prevRunes[:0]
					continue
				}
				if isHoubbTagSpec(spec) {
					activeTagSpec = internHoubbTagSpec(spec)
					compactMode = true
					prevRunes = prevRunes[:0]
					continue
				}
			}
			if len(trimmedDir) >= len("@category:") && strings.EqualFold(trimmedDir[:len("@category:")], "@category:") {
				if mapped, ok := categoryNameToTagSpec(trimmedDir[len("@category:"):]); ok {
					activeTagSpec = mapped
					compactMode = true
					prevRunes = prevRunes[:0]
					continue
				}
			}
		}

		if compactMode && len(rawLine) >= 2 && rawLine[0] == '^' {
			if pfxLen, ok := decodeBase36Digit(rawLine[1]); ok && pfxLen <= len(prevRunes) {
				suffix := rawLine[2:]
				if !hasCorruptControlBytes(suffix) {
					runeBuf = append(runeBuf[:0], prevRunes[:pfxLen]...)
					for _, rn := range suffix {
						runeBuf = append(runeBuf, rn)
					}
					if len(runeBuf) > 0 && len(runeBuf) <= MaxWordRunes {
						rebuilt := string(runeBuf)
						if !isUpstreamHoubbNoiseWord(rebuilt) {
							prevRunes = append(prevRunes[:0], runeBuf...)
							onEntry(rebuilt, activeTagSpec, false)
						} else {
							dropped++
						}
						continue
					}
				}
				dropped++
				continue
			}
		}

		word, tagSpec, isAllow, ok := splitHoubbLine(rawLine)
		if !ok {
			if t := strings.TrimSpace(rawLine); t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "//") {
				dropped++
			}
			continue
		}
		if !isAllow {
			if tagSpec == "" && compactMode {
				tagSpec = activeTagSpec
			} else if tagSpec != "" {
				tagSpec = internHoubbTagSpec(tagSpec)
			}
			if compactMode {
				prevRunes = prevRunes[:0]
				for _, rn := range word {
					prevRunes = append(prevRunes, rn)
				}
			}
		}
		onEntry(word, tagSpec, isAllow)
	}
	return dropped, scanner.Err()
}

// AddHoubbStream streams a plain, tagged, or compact front-coded houbb dictionary reader directly into
// the Builder using a bounded 4 KiB line buffer (zero full-file string or []string allocations).
func (b *Builder) AddHoubbStream(r io.Reader, enabledCategories map[string]bool) (added int, dropped int, err error) {
	dropped, err = scanHoubbStream(r, func(word string, tagSpec string, isAllow bool) {
		if isAllow {
			if b.AddAllow(word) {
				added++
			}
			return
		}
		cat, keep := resolveEnabledCategory(tagSpec, enabledCategories)
		if !keep {
			return
		}
		if b.AddDeny(word, cat) {
			added++
		}
	})
	return added, dropped, err
}

func internHoubbTagSpec(spec string) string {
	switch spec {
	case "0":
		return "0"
	case "1":
		return "1"
	case "2":
		return "2"
	case "3":
		return "3"
	case "4":
		return "4"
	case "0,4":
		return "0,4"
	case "1,4":
		return "1,4"
	case "2,4":
		return "2,4"
	case "3,4":
		return "3,4"
	case "0,2":
		return "0,2"
	case "2,3":
		return "2,3"
	default:
		return strings.Clone(spec)
	}
}

// LoadHoubbTagMap extracts word -> tagSpec ("0".."4") mappings from a tagged or compact front-coded
// houbb stream so untagged remote updates from sensitive_word_dict.txt automatically inherit category tags.
func LoadHoubbTagMap(r io.Reader) (map[string]string, error) {
	out := make(map[string]string, 44_000)
	_, err := scanHoubbStream(r, func(word string, tagSpec string, isAllow bool) {
		if !isAllow && tagSpec != "" {
			out[word] = tagSpec
		}
	})
	return out, err
}

type byteCountReader struct {
	r io.Reader
	n int64
}

func (c *byteCountReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ConvertHoubbStream streams, sanitizes, deduplicates, tag-enriches, and gzip-compresses a remote
// dictionary reader (supporting line-oriented houbb sensitive_word_dict.txt / sensitive_word_tags.txt,
// !allow lines, and JSON arrays/objects) without buffering full line-oriented files in memory.
func ConvertHoubbStream(r io.Reader, maxBytes int64, baseTagLookup map[string]string) (HoubbConvertStats, []byte, error) {
	var stats HoubbConvertStats
	if r == nil {
		return stats, nil, nil
	}

	src := r
	var counter *byteCountReader
	if maxBytes > 0 {
		counter = &byteCountReader{r: io.LimitReader(r, maxBytes+1)}
		src = counter
	}
	br := bufio.NewReaderSize(src, 4096)

	// Strip optional UTF-8 BOM and advance to first non-whitespace byte to detect JSON vs line stream.
	if bom, _ := br.Peek(3); len(bom) == 3 && bom[0] == 0xef && bom[1] == 0xbb && bom[2] == 0xbf {
		_, _ = br.Discard(3)
	}
	var firstByte byte
	hasContent := false
	for {
		b, rErr := br.ReadByte()
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				break
			}
			return stats, nil, rErr
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		firstByte = b
		hasContent = true
		_ = br.UnreadByte()
		break
	}
	if counter != nil && counter.n > maxBytes {
		return stats, nil, ErrHoubbPayloadTooLarge
	}
	if !hasContent {
		return stats, nil, nil
	}

	var gzBuf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	if err != nil {
		return stats, nil, err
	}

	seenDeny := make(map[string]struct{}, 1024)
	seenAllow := make(map[string]struct{}, 64)

	emitLine := func(rawLine string) {
		t := strings.TrimSpace(rawLine)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			return
		}
		word, tagSpec, isAllow, ok := splitHoubbLine(rawLine)
		if !ok {
			stats.DroppedCount++
			return
		}
		if isAllow {
			if _, dup := seenAllow[word]; dup || stats.AllowCount >= MaxTotalWords {
				return
			}
			seenAllow[word] = struct{}{}
			_, _ = io.WriteString(zw, "!")
			_, _ = io.WriteString(zw, word)
			_, _ = io.WriteString(zw, "\n")
			stats.AllowCount++
			return
		}
		if _, dup := seenDeny[word]; dup || stats.WordCount >= MaxTotalWords {
			return
		}
		seenDeny[word] = struct{}{}
		if tagSpec == "" && len(baseTagLookup) > 0 {
			tagSpec = baseTagLookup[word]
		}
		_, _ = io.WriteString(zw, word)
		if tagSpec != "" {
			_, _ = io.WriteString(zw, "\t")
			_, _ = io.WriteString(zw, tagSpec)
			stats.TaggedCount++
		}
		_, _ = io.WriteString(zw, "\n")
		stats.WordCount++
	}

	var lineSource io.Reader = br
	parsedJSON := false
	if firstByte == '[' || firstByte == '{' {
		rawJSON, rErr := io.ReadAll(br)
		if rErr != nil {
			_ = zw.Close()
			return stats, nil, rErr
		}
		if counter != nil && counter.n > maxBytes {
			_ = zw.Close()
			return stats, nil, ErrHoubbPayloadTooLarge
		}
		trimmedBody := bytes.TrimSpace(rawJSON)
		if firstByte == '[' {
			var arr []string
			if json.Unmarshal(trimmedBody, &arr) == nil {
				parsedJSON = true
				for _, item := range arr {
					emitLine(item)
				}
			}
		} else {
			var obj struct {
				Words     []string `json:"words"`
				Deny      []string `json:"deny"`
				Allow     []string `json:"allow"`
				Allowlist []string `json:"allowlist"`
			}
			if json.Unmarshal(trimmedBody, &obj) == nil {
				parsedJSON = true
				for _, w := range obj.Words {
					emitLine(w)
				}
				for _, w := range obj.Deny {
					emitLine(w)
				}
				for _, a := range obj.Allow {
					if clean := strings.TrimSpace(a); clean != "" {
						emitLine("!" + clean)
					}
				}
				for _, a := range obj.Allowlist {
					if clean := strings.TrimSpace(a); clean != "" {
						emitLine("!" + clean)
					}
				}
			}
		}
		if !parsedJSON {
			lineSource = bytes.NewReader(trimmedBody)
		}
	}

	if !parsedJSON {
		scanner := bufio.NewScanner(lineSource)
		scanner.Buffer(make([]byte, 4096), 16*1024)
		for scanner.Scan() {
			if counter != nil && counter.n > maxBytes {
				_ = zw.Close()
				return stats, nil, ErrHoubbPayloadTooLarge
			}
			emitLine(scanner.Text())
		}
		if sErr := scanner.Err(); sErr != nil {
			_ = zw.Close()
			return stats, nil, sErr
		}
		if counter != nil && counter.n > maxBytes {
			_ = zw.Close()
			return stats, nil, ErrHoubbPayloadTooLarge
		}
	}

	if cErr := zw.Close(); cErr != nil {
		return stats, nil, cErr
	}
	if stats.WordCount == 0 && stats.AllowCount == 0 {
		return stats, nil, nil
	}
	outBytes := gzBuf.Bytes()
	stats.GzipBytes = len(outBytes)
	return stats, outBytes, nil
}

// ConvertHoubbPayload sanitizes, deduplicates, tag-enriches, and gzip-compresses an in-memory
// remote dictionary payload for compact persistence in plugin_kv.
func ConvertHoubbPayload(rawBody []byte, baseTagLookup map[string]string) (HoubbConvertStats, []byte, error) {
	return ConvertHoubbStream(bytes.NewReader(rawBody), 0, baseTagLookup)
}
