package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ParseDotEnvFile reads a .env file and returns key-value pairs without mutating os.Environ().
// Missing files return an empty map and nil error to support zero-config startup.
func ParseDotEnvFile(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]string{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("config: open .env file %q: %w", path, err)
	}
	defer f.Close()

	return ParseDotEnv(f)
}

// ParseDotEnv parses standard .env content supporting comments (#), optional "export " prefix,
// single/double quoted strings, escape sequences in double quotes, and inline comments.
func ParseDotEnv(r *os.File) (map[string]string, error) {
	return parseDotEnvScanner(bufio.NewScanner(r))
}

func parseDotEnvString(raw string) (map[string]string, error) {
	return parseDotEnvScanner(bufio.NewScanner(strings.NewReader(raw)))
}

func parseDotEnvScanner(sc *bufio.Scanner) (map[string]string, error) {
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)

	out := make(map[string]string)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
			line = strings.TrimSpace(line)
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimSpace(line[len("export"):])
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("config: invalid .env syntax at line %d", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		if !isValidEnvKey(key) {
			return nil, fmt.Errorf("config: invalid .env key %q at line %d", key, lineNo)
		}
		valRaw := strings.TrimSpace(line[eq+1:])
		val, err := parseDotEnvValue(valRaw, lineNo)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: scan .env: %w", err)
	}
	return out, nil
}

func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha && c != '_' {
			return false
		}
		if !isAlpha && !isDigit && c != '_' {
			return false
		}
	}
	return true
}

func parseDotEnvValue(raw string, lineNo int) (string, error) {
	if raw == "" {
		return "", nil
	}
	quote := raw[0]
	if quote == '\'' {
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("config: unterminated single quote at line %d", lineNo)
		}
		rest := strings.TrimSpace(raw[2+end:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("config: unexpected trailing characters after single quote at line %d", lineNo)
		}
		return raw[1 : 1+end], nil
	}
	if quote == '"' {
		var b strings.Builder
		escaped := false
		closedAt := -1
		for i := 1; i < len(raw); i++ {
			ch := raw[i]
			if escaped {
				switch ch {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '"', '\\', '$':
					b.WriteByte(ch)
				default:
					b.WriteByte('\\')
					b.WriteByte(ch)
				}
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				closedAt = i
				break
			}
			b.WriteByte(ch)
		}
		if closedAt < 0 {
			return "", fmt.Errorf("config: unterminated double quote at line %d", lineNo)
		}
		rest := strings.TrimSpace(raw[closedAt+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("config: unexpected trailing characters after double quote at line %d", lineNo)
		}
		return b.String(), nil
	}

	// Unquoted value: strip inline comment preceded by whitespace (" #...")
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			return strings.TrimSpace(raw[:i]), nil
		}
	}
	return strings.TrimSpace(raw), nil
}
