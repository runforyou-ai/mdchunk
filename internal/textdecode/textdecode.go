// Package textdecode decodes source text to UTF-8 and normalises it for Markdown output.
package textdecode

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
	"golang.org/x/text/transform"
)

// Decode returns data as UTF-8. A BOM (UTF-8, UTF-16 or UTF-32) wins, then the
// declared charset when it names a known encoding, then UTF-8. Undeclared input
// that is not valid UTF-8 is decoded with fallback when it is set. Bytes that
// cannot be decoded become U+FFFD.
func Decode(data []byte, declared string, fallback encoding.Encoding) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return strings.ToValidUTF8(string(data[3:]), "\uFFFD")
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0x00, 0x00}), bytes.HasPrefix(data, []byte{0x00, 0x00, 0xFE, 0xFF}):
		return decodeWith(utf32.UTF32(utf32.BigEndian, utf32.ExpectBOM), data)
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeWith(unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM), data)
	}
	if e := Lookup(declared); e != nil {
		if e == unicode.UTF8 {
			return strings.ToValidUTF8(string(data), "\uFFFD")
		}
		return decodeWith(e, data)
	}
	if utf8.Valid(data) {
		return string(data)
	}
	if fallback != nil {
		return decodeWith(fallback, data)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// Lookup returns the encoding a charset label names, ignoring surrounding
// quotes, or nil when the label is empty, unknown or maps to the replacement
// encoding.
func Lookup(label string) encoding.Encoding {
	label = strings.Trim(strings.TrimSpace(label), `"'`)
	if label == "" {
		return nil
	}
	e, err := htmlindex.Get(label)
	if err != nil || e == encoding.Replacement {
		return nil
	}
	return e
}

// decodeWith decodes data with e; undecodable bytes become U+FFFD.
func decodeWith(e encoding.Encoding, data []byte) string {
	decoded, _, _ := transform.Bytes(e.NewDecoder(), data)
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
