package convert_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/runforyou-ai/mdchunk/convert"
)

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]convert.Format{
		"PDF": convert.PDF, ".pdf": convert.PDF, "Markdown": convert.Markdown, "htm": convert.HTML,
		"yml": "yaml", "text": convert.Text, "docx": convert.DOCX,
	} {
		if got := convert.ParseFormat(in); got != want {
			t.Errorf("ParseFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatOf(t *testing.T) {
	for name, want := range map[string]convert.Format{
		"report.PDF": convert.PDF, "dir.v2/notes.markdown": convert.Markdown, `C:\docs\a.HTM`: convert.HTML,
		"archive.tar.gz": "gz",
	} {
		if got, ok := convert.FormatOf(name); !ok || got != want {
			t.Errorf("FormatOf(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"README", ".bashrc", "dir.d/file", "trailing."} {
		if got, ok := convert.FormatOf(name); ok {
			t.Errorf("FormatOf(%q) = %q, want none", name, got)
		}
	}
}

func TestSectionsIn(t *testing.T) {
	doc := convert.Document{Sections: []convert.Section{
		{Kind: convert.KindPage, Number: 1, Start: 0, End: 10},
		{Kind: convert.KindPage, Number: 2, Start: 10, End: 10},
		{Kind: convert.KindPage, Number: 3, Start: 12, End: 20},
	}}
	numbers := func(start, end int) []int {
		var got []int
		for _, s := range doc.SectionsIn(start, end) {
			got = append(got, s.Number)
		}
		return got
	}
	for _, tc := range []struct {
		start, end int
		want       []int
	}{
		{0, 20, []int{1, 3}},
		{5, 15, []int{1, 3}},
		{10, 12, nil},
		{9, 10, []int{1}},
		{10, 13, []int{3}},
		{5, 5, nil},
		{15, 15, nil},
		{20, 30, nil},
	} {
		if got := numbers(tc.start, tc.end); !slices.Equal(got, tc.want) {
			t.Errorf("SectionsIn(%d, %d) = %v, want %v", tc.start, tc.end, got, tc.want)
		}
	}
	got := doc.SectionsIn(0, 20)
	got[0].Number = 99
	if doc.Sections[0].Number != 1 {
		t.Error("SectionsIn shares memory with Document.Sections")
	}
}

// fixed returns a converter producing markdown.
func fixed(markdown string) convert.Converter {
	return convert.ConverterFunc(func(context.Context, convert.Input) (convert.Document, error) {
		return convert.Document{Markdown: markdown}, nil
	})
}

func TestRegistry(t *testing.T) {
	var r convert.Registry
	if _, err := r.Convert(context.Background(), convert.PDF, convert.Input{}); !errors.Is(err, convert.ErrUnsupported) {
		t.Fatalf("empty registry: %v", err)
	}
	r.Register(fixed("a"), "TXT", ".md")
	r.Register(fixed("b"), convert.Markdown)
	if got := r.Formats(); !slices.Equal(got, []convert.Format{convert.Markdown, convert.Text}) {
		t.Errorf("Formats() = %v", got)
	}
	for format, want := range map[convert.Format]string{"txt": "a", "markdown": "b", ".MD": "b"} {
		doc, err := r.Convert(context.Background(), format, convert.Input{})
		if err != nil || doc.Markdown != want {
			t.Errorf("Convert(%q) = %q, %v; want %q", format, doc.Markdown, err, want)
		}
	}
}

func TestRegistryConcurrent(t *testing.T) {
	var r convert.Registry
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			format := convert.Format(fmt.Sprint("f", i%3))
			r.Register(fixed("x"), format)
			_, _ = r.Convert(context.Background(), format, convert.Input{})
			_ = r.Formats()
		})
	}
	wg.Wait()
	if len(r.Formats()) != 3 {
		t.Errorf("Formats() = %v", r.Formats())
	}
}

func TestLimits(t *testing.T) {
	got := convert.Limits{MaxBytes: 10, MaxExpandedBytes: -5}.Effective()
	want := convert.Limits{MaxBytes: 10, MaxExpandedBytes: -1, MaxOutputBytes: convert.DefaultMaxOutputBytes}
	if got != want {
		t.Errorf("Effective() = %+v, want %+v", got, want)
	}
	err := fmt.Errorf("wrapped: %w", &convert.LimitError{Limit: convert.LimitExpanded, Max: 7})
	var limit *convert.LimitError
	if !errors.Is(err, convert.ErrTooLarge) || !errors.As(err, &limit) || limit.Limit != convert.LimitExpanded {
		t.Errorf("LimitError not matched: %v", err)
	}
	if errors.Is(err, convert.ErrCorrupt) {
		t.Error("LimitError matches ErrCorrupt")
	}
	if !strings.Contains(err.Error(), "expanded exceeds 7 bytes") {
		t.Errorf("message = %q", err)
	}
}
