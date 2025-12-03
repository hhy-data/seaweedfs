package util

import (
	"net/url"
	"strings"
)

// EncodeFilenameRFC2231 encodes filename according to RFC 2231/5987 for HTTP headers
// Returns the percent-encoded string that preserves all characters including control chars
// Example: "file_\x7Ftest.jpg" -> "file_%7Ftest.jpg"
func EncodeFilenameRFC2231(filename string) string {
	// Use url.PathEscape which encodes according to RFC 3986
	// It will encode all special characters including 0x7F as %XX
	return url.PathEscape(filename)
}

// MakeSafeFilenameFallback creates a safe fallback filename for older clients
// that don't support RFC 2231 filename* parameter
// It replaces problematic characters with underscores
func MakeSafeFilenameFallback(filename string) string {
	var result strings.Builder
	result.Grow(len(filename))

	for _, r := range filename {
		// The 'filename' parameter can be used only with US-ASCII characters.
		// See RFC 6266, Section 4.3.
		// Replace control chars (0x00-0x1F), non-ASCII chars (>= 0x7F), quotes, and backslash.
		if r < 0x20 || r >= 0x7F || r == '"' || r == '\\' || r == '\'' {
			result.WriteRune('_')
		} else {
			result.WriteRune(r)
		}
	}

	name := result.String()
	name = strings.TrimSpace(name)

	if name == "" {
		return "unnamed"
	}
	return name
}

// FormatContentDispositionRFC2231 formats Content-Disposition header with RFC 2231 encoding
// Returns: `attachment; filename="safe_fallback.jpg"; filename*=UTF-8”encoded%20name.jpg`
func FormatContentDispositionRFC2231(disposition, filename string) string {
	if filename == "" {
		return disposition
	}

	// Generate safe fallback for older clients
	safeFallback := MakeSafeFilenameFallback(filename)

	// Generate RFC 2231 encoded filename
	encodedFilename := EncodeFilenameRFC2231(filename)

	var result strings.Builder
	result.WriteString(disposition)

	// Add standard filename parameter (quoted, with escaped quotes and backslashes)
	result.WriteString(`; filename="`)
	result.WriteString(escapeQuotes(safeFallback))
	result.WriteString(`"`)

	// Add RFC 2231 filename* parameter if encoding is needed
	// Only add if:
	// 1. The original filename is not empty
	// 2. The encoded version differs from the safe fallback
	if encodedFilename != safeFallback {
		result.WriteString(`; filename*=UTF-8''`)
		result.WriteString(encodedFilename)
	}

	return result.String()
}

// escapeQuotes escapes backslashes and quotes for use in quoted strings
func escapeQuotes(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// DecodeFilenameRFC2231 decodes a RFC 2231 encoded filename
// Handles both regular filenames and percent-encoded filenames
func DecodeFilenameRFC2231(encoded string) (string, error) {
	original := encoded
	// If it starts with charset encoding (e.g., "UTF-8''..."), strip it
	if pos := strings.IndexByte(encoded, '\''); pos > 0 {
		if pos2 := strings.IndexByte(encoded[pos+1:], '\''); pos2 >= 0 {
			// e.g. UTF-8'en'foo or UTF-8''foo
			encoded = encoded[pos+1+pos2+1:]
		}
	}

	// Decode percent encoding
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		// If decoding fails, return original
		return original, err
	}

	return decoded, nil
}

// SanitizeHttpHeaderValue removes control characters from a string
// to make it safe for use in HTTP header values (RFC 7230 compliant)
// Replaces 0x00-0x1F and 0x7F with underscores
// Preserves forward slashes and other valid path characters
func SanitizeHttpHeaderValue(value string) string {
	var result strings.Builder
	result.Grow(len(value))

	for _, r := range value {
		// Replace control chars (0x00-0x1F, 0x7F) with underscore
		// Keep all other characters including /, :, -, _, ., etc.
		if r < 0x20 || r == 0x7F {
			result.WriteRune('_')
		} else {
			result.WriteRune(r)
		}
	}

	return result.String()
}
