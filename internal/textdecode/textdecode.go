// Package textdecode decodes source text to UTF-8 and normalises it for Markdown output.
package textdecode

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Decode returns data as UTF-8. A BOM wins, then the declared charset when it
// names a known encoding, then UTF-8. Invalid UTF-8 is decoded with fallback
// when it is set; otherwise invalid bytes become U+FFFD.
func Decode(data []byte, declared string, fallback encoding.Encoding) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return strings.ToValidUTF8(string(data[3:]), "\uFFFD")
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeWith(unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM), data)
	}
	if declared != "" {
		if e, err := htmlindex.Get(declared); err == nil && e != unicode.UTF8 {
			return decodeWith(e, data)
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	if fallback != nil {
		return decodeWith(fallback, data)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// decodeWith decodes data with e, replacing undecodable bytes with U+FFFD.
func decodeWith(e encoding.Encoding, data []byte) string {
	decoded, _, err := transform.Bytes(e.NewDecoder(), data)
	if err != nil {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD")
}

// Normalize converts CRLF and lone CR to LF and removes a leading BOM and NUL bytes.
func Normalize(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	if strings.ContainsAny(s, "\r\x00") {
		s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "").Replace(s)
	}
	return s
}
