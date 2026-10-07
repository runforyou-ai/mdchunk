// Package mdwrite builds converter output under the output limit.
package mdwrite

import (
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
)

// Writer accumulates Markdown. After the output limit is exceeded it keeps
// the error and ignores further writes.
type Writer struct {
	b   strings.Builder
	max int64
	err error
}

// New returns a Writer limited to maxBytes; maxBytes < 0 means unlimited.
func New(maxBytes int64) *Writer {
	return &Writer{max: maxBytes}
}

// WriteString appends s.
func (w *Writer) WriteString(s string) {
	if w.err != nil {
		return
	}
	if w.max >= 0 && int64(w.b.Len()+len(s)) > w.max {
		w.err = &convert.LimitError{Limit: convert.LimitOutput, Max: w.max}
		return
	}
	w.b.WriteString(s)
}

// Len returns the number of bytes written so far.
func (w *Writer) Len() int {
	return w.b.Len()
}

// Err returns the limit error, if any.
func (w *Writer) Err() error {
	return w.err
}

// String returns the Markdown written so far.
func (w *Writer) String() string {
	return w.b.String()
}

// Fence wraps content in a code fence longer than any backtick run it contains.
func Fence(info, content string) string {
	longest, run := 0, 0
	for i := 0; i < len(content); i++ {
		if content[i] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fence + info + "\n" + content + fence
}
