package util

import (
	"testing"
)

func TestEncodeFilenameRFC2231(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal filename",
			input:    "photo.jpg",
			expected: "photo.jpg",
		},
		{
			name:     "filename with spaces",
			input:    "my file.jpg",
			expected: "my%20file.jpg",
		},
		{
			name:     "filename with DEL character (0x7F)",
			input:    "file_\x7Ftest.jpg",
			expected: "file_%7Ftest.jpg",
		},
		{
			name:     "filename with control characters",
			input:    "test\x01\x02file.jpg",
			expected: "test%01%02file.jpg",
		},
		{
			name:     "filename with Chinese characters",
			input:    "文档_2024.pdf",
			expected: "%E6%96%87%E6%A1%A3_2024.pdf",
		},
		{
			name:     "Windows problematic filename",
			input:    "3518(2024-06-21)_\x7F_rgb.jpg",
			expected: "3518%282024-06-21%29_%7F_rgb.jpg",
		},
		{
			name:     "filename with quotes",
			input:    `test"file".jpg`,
			expected: "test%22file%22.jpg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EncodeFilenameRFC2231(tt.input)
			if result != tt.expected {
				t.Errorf("EncodeFilenameRFC2231(%q) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestMakeSafeFilenameFallback(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal filename",
			input:    "photo.jpg",
			expected: "photo.jpg",
		},
		{
			name:     "filename with DEL character",
			input:    "file_\x7Ftest.jpg",
			expected: "file__test.jpg",
		},
		{
			name:     "filename with control characters",
			input:    "test\x01\x02file.jpg",
			expected: "test__file.jpg",
		},
		{
			name:     "filename with quotes",
			input:    `test"file".jpg`,
			expected: "test_file_.jpg",
		},
		{
			name:     "filename with Chinese (kept as-is)",
			input:    "文档_2024.pdf",
			expected: "文档_2024.pdf",
		},
		{
			name:     "empty filename",
			input:    "",
			expected: "unnamed",
		},
		{
			name:     "only spaces",
			input:    "   ",
			expected: "unnamed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MakeSafeFilenameFallback(tt.input)
			if result != tt.expected {
				t.Errorf("MakeSafeFilenameFallback(%q) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFormatContentDispositionRFC2231(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		filename    string
		expected    string
	}{
		{
			name:        "normal filename",
			disposition: "attachment",
			filename:    "photo.jpg",
			expected:    `attachment; filename="photo.jpg"`,
		},
		{
			name:        "filename with spaces",
			disposition: "attachment",
			filename:    "my file.jpg",
			expected:    `attachment; filename="my file.jpg"; filename*=UTF-8''my%20file.jpg`,
		},
		{
			name:        "filename with DEL character",
			disposition: "attachment",
			filename:    "file_\x7Ftest.jpg",
			expected:    `attachment; filename="file__test.jpg"; filename*=UTF-8''file_%7Ftest.jpg`,
		},
		{
			name:        "filename with Chinese",
			disposition: "inline",
			filename:    "文档.pdf",
			expected:    `inline; filename="文档.pdf"; filename*=UTF-8''%E6%96%87%E6%A1%A3.pdf`,
		},
		{
			name:        "Windows problematic filename",
			disposition: "attachment",
			filename:    "3518(2024-06-21)_\x7F_rgb.jpg",
			expected:    `attachment; filename="3518(2024-06-21)___rgb.jpg"; filename*=UTF-8''3518%282024-06-21%29_%7F_rgb.jpg`,
		},
		{
			name:        "filename with quotes",
			disposition: "attachment",
			filename:    `test"file".jpg`,
			expected:    `attachment; filename="test_file_.jpg"; filename*=UTF-8''test%22file%22.jpg`,
		},
		{
			name:        "empty filename",
			disposition: "attachment",
			filename:    "",
			expected:    "attachment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatContentDispositionRFC2231(tt.disposition, tt.filename)
			if result != tt.expected {
				t.Errorf("FormatContentDispositionRFC2231(%q, %q)\ngot:  %q\nwant: %q",
					tt.disposition, tt.filename, result, tt.expected)
			}
		})
	}
}

func TestDecodeFilenameRFC2231(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "encoded with charset prefix",
			input:    "UTF-8''file_%7Ftest.jpg",
			expected: "file_\x7Ftest.jpg",
		},
		{
			name:     "encoded without charset prefix",
			input:    "file_%7Ftest.jpg",
			expected: "file_\x7Ftest.jpg",
		},
		{
			name:     "normal filename",
			input:    "photo.jpg",
			expected: "photo.jpg",
		},
		{
			name:     "Chinese filename encoded",
			input:    "UTF-8''%E6%96%87%E6%A1%A3.pdf",
			expected: "文档.pdf",
		},
		{
			name:     "filename with spaces",
			input:    "my%20file.jpg",
			expected: "my file.jpg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := DecodeFilenameRFC2231(tt.input)
			if err != nil {
				t.Errorf("DecodeFilenameRFC2231(%q) returned error: %v", tt.input, err)
			}
			if result != tt.expected {
				t.Errorf("DecodeFilenameRFC2231(%q) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNeedsRFC2231Encoding(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "normal ASCII filename",
			input:    "photo.jpg",
			expected: false,
		},
		{
			name:     "filename with space needs encoding",
			input:    "my file.jpg",
			expected: false, // space is ASCII 0x20, allowed in URL
		},
		{
			name:     "filename with DEL character",
			input:    "file_\x7Ftest.jpg",
			expected: true,
		},
		{
			name:     "filename with Chinese",
			input:    "文档.pdf",
			expected: true,
		},
		{
			name:     "filename with control char",
			input:    "test\x01file.jpg",
			expected: true,
		},
		{
			name:     "filename with quotes",
			input:    `test"file".jpg`,
			expected: true,
		},
		{
			name:     "filename with backslash",
			input:    `test\file.jpg`,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NeedsRFC2231Encoding(tt.input)
			if result != tt.expected {
				t.Errorf("NeedsRFC2231Encoding(%q) = %v; want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	// Test that encoding and decoding preserves the original filename
	testCases := []string{
		"photo.jpg",
		"file_\x7Ftest.jpg",
		"文档_2024.pdf",
		"test\x01\x02file.jpg",
		`test"file".jpg`,
		"3518(2024-06-21)_\x7F_rgb.jpg",
	}

	for _, original := range testCases {
		t.Run(original, func(t *testing.T) {
			encoded := EncodeFilenameRFC2231(original)
			decoded, err := DecodeFilenameRFC2231(encoded)
			if err != nil {
				t.Errorf("Decode failed: %v", err)
			}
			if decoded != original {
				t.Errorf("Round trip failed: original=%q, encoded=%q, decoded=%q",
					original, encoded, decoded)
			}
		})
	}
}
