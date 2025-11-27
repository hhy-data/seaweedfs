package util

import (
	"testing"
)

func TestSafeFileName(t *testing.T) {
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
			input:    "my file name.pdf",
			expected: "my file name.pdf",
		},
		{
			name:     "filename with Chinese characters",
			input:    "文档_2024.pdf",
			expected: "文档_2024.pdf",
		},
		{
			name:     "control characters in filename",
			input:    "test\x00file.jpg",
			expected: "test_file.jpg",
		},
		{
			name:     "DEL character in filename",
			input:    "test\x7Ffile.jpg",
			expected: "test_file.jpg",
		},
		{
			name:     "double quotes in filename",
			input:    `test"file".jpg`,
			expected: "test_file_.jpg",
		},
		{
			name:     "single quotes in filename",
			input:    "test'file'.jpg",
			expected: "test_file_.jpg",
		},
		{
			name:     "mix of control and quotes",
			input:    "test\x01\"file\"\x1F.jpg",
			expected: "test__file__.jpg",
		},
		{
			name:     "Windows problematic filename (from user report)",
			input:    "3518(2024-06-21)_^?\u007F_rgb.jpg",
			expected: "3518(2024-06-21)_^?__rgb.jpg",
		},
		{
			name:     "UTF-16 private use area",
			input:    "test\uE000file.jpg",
			expected: "test_file.jpg",
		},
		{
			name:     "UTF-16 private use area F8FF",
			input:    "test\uF8FFfile.jpg",
			expected: "test_file.jpg",
		},
		{
			name:     "newline in filename",
			input:    "test\nfile.jpg",
			expected: "test_file.jpg",
		},
		{
			name:     "tab in filename",
			input:    "test\tfile.jpg",
			expected: "test_file.jpg",
		},
		{
			name:     "carriage return in filename",
			input:    "test\rfile.jpg",
			expected: "test_file.jpg",
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
		{
			name:     "only control characters",
			input:    "\x00\x01\x02",
			expected: "___",
		},
		{
			name:     "complex real-world example",
			input:    "GXF_3979MD1.1_3518(2024-06-21)_\x7F\u007F_rgb.jpg",
			expected: "GXF_3979MD1.1_3518(2024-06-21)____rgb.jpg",
		},
		{
			name:     "filename with parentheses and brackets",
			input:    "file(2024)[backup].zip",
			expected: "file(2024)[backup].zip",
		},
		{
			name:     "filename with hyphen and underscore",
			input:    "my-file_name_v2.txt",
			expected: "my-file_name_v2.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SafeFileName(tt.input)
			if result != tt.expected {
				t.Errorf("SafeFileName(%q) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}
