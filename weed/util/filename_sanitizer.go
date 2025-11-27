package util

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// SafeFileName sanitizes filename to prevent MIME parsing errors
// It replaces illegal characters with underscores to maintain filename structure
func SafeFileName(name string) string {
	// 1. Remove HTTP header illegal chars: control + DEL
	name = regexp.MustCompile(`[\x00-\x1F\x7F]`).ReplaceAllString(name, "_")

	// 2. Replace header special chars
	name = strings.ReplaceAll(name, `"`, "_")
	name = strings.ReplaceAll(name, `'`, "_")
	name = strings.TrimSpace(name)

	// 3. Filter UTF-16 private use area U+E000–U+F8FF
	cleaned := make([]rune, 0, len(name))
	for _, r := range name {
		if r >= 0xE000 && r <= 0xF8FF {
			cleaned = append(cleaned, '_')
			continue
		}
		cleaned = append(cleaned, r)
	}
	name = string(cleaned)

	// 4. Final UTF-8 validation, replace invalid with _
	if !utf8.ValidString(name) {
		buf := make([]rune, 0, len(name))
		for _, b := range []byte(name) {
			if utf8.Valid([]byte{b}) {
				buf = append(buf, rune(b))
			} else {
				buf = append(buf, '_')
			}
		}
		name = string(buf)
	}

	// 5. Prevent empty filename
	if name == "" {
		name = "unnamed"
	}

	return name
}
