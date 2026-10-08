// Package ooxmltest builds OOXML packages for tests.
package ooxmltest

import (
	"archive/zip"
	"bytes"
	"testing"
)

// Build zips files, keyed by part name, into a package.
func Build(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Encrypted returns the start of a Compound File Binary that holds an EncryptionInfo stream.
func Encrypted() []byte {
	data := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	data = append(data, make([]byte, 64)...)
	return append(data, []byte("E\x00n\x00c\x00r\x00y\x00p\x00t\x00i\x00o\x00n\x00I\x00n\x00f\x00o\x00")...)
}

// Legacy returns the start of a Compound File Binary without encryption, as legacy .doc files are.
func Legacy() []byte {
	return append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 64)...)
}
