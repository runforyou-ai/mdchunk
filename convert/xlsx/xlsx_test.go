package xlsx_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/convert/xlsx"
	"github.com/runforyou-ai/mdchunk/internal/ooxmltest"
)

// workbook builds a workbook with a data sheet, an empty sheet and a hidden sheet.
func workbook(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.SetSheetName("Sheet1", "销售"))
	must(f.SetSheetRow("销售", "A1", &[]any{"地区", "占比", "备注"}))
	must(f.SetSheetRow("销售", "A2", &[]any{"华东", 0.25, "a|b"}))
	must(f.SetSheetRow("销售", "A4", &[]any{"华南", 0.5}))
	style, err := f.NewStyle(&excelize.Style{NumFmt: 9})
	must(err)
	must(f.SetCellStyle("销售", "B2", "B4", style))
	_, err = f.NewSheet("空表")
	must(err)
	_, err = f.NewSheet("隐藏")
	must(err)
	must(f.SetCellValue("隐藏", "A1", "secret"))
	must(f.SetSheetVisible("隐藏", false))
	var buf bytes.Buffer
	must(f.Write(&buf))
	return buf.Bytes()
}

// run converts data with opts.
func run(t *testing.T, opts xlsx.Options, data []byte) convert.Document {
	t.Helper()
	doc, err := xlsx.New(opts).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSheets(t *testing.T) {
	doc := run(t, xlsx.Options{}, workbook(t))
	want := "## 销售\n\n| 地区 | 占比 | 备注 |\n| --- | --- | --- |\n| 华东 | 25% | a\\|b |\n| 华南 | 50% |  |"
	if doc.Markdown != want {
		t.Fatalf("got:\n%s\nwant:\n%s", doc.Markdown, want)
	}
	if len(doc.Sections) != 3 {
		t.Fatalf("sections = %+v", doc.Sections)
	}
	for i, name := range []string{"销售", "空表", "隐藏"} {
		if s := doc.Sections[i]; s.Kind != convert.KindSheet || s.Number != i+1 || s.Name != name {
			t.Errorf("section %d = %+v", i, s)
		}
	}
	if s := doc.Sections[0]; s.Start != 0 || s.End != len(doc.Markdown) {
		t.Errorf("first section = %+v", s)
	}
	if s := doc.Sections[2]; s.Start != s.End {
		t.Errorf("hidden section not empty: %+v", s)
	}
}

func TestOptions(t *testing.T) {
	doc := run(t, xlsx.Options{RawValues: true, IncludeHidden: true}, workbook(t))
	want := "## 销售\n\n| 地区 | 占比 | 备注 |\n| --- | --- | --- |\n| 华东 | 0.25 | a\\|b |\n| 华南 | 0.5 |  |\n\n## 隐藏\n\n| secret |\n| --- |"
	if doc.Markdown != want {
		t.Fatalf("got:\n%s\nwant:\n%s", doc.Markdown, want)
	}
	if s := doc.Sections[2]; doc.Markdown[s.Start:s.End] != "## 隐藏\n\n| secret |\n| --- |" {
		t.Errorf("hidden section = %q", doc.Markdown[s.Start:s.End])
	}
}

// rezip rewrites the entries of a zip archive with edit applied to the named entry.
func rezip(t *testing.T, data []byte, name string, edit func(string) string) []byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range archive.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name] = string(content)
		if entry.Name == name {
			files[entry.Name] = edit(string(content))
		}
	}
	return ooxmltest.Build(t, files)
}

func TestCorruptSheetAndNUL(t *testing.T) {
	data := workbook(t)
	truncated := rezip(t, data, "xl/worksheets/sheet1.xml", func(s string) string { return s[:strings.Index(s, "<row r=\"2\"")+20] })
	if _, err := xlsx.New(xlsx.Options{}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(truncated)}); !errors.Is(err, convert.ErrCorrupt) {
		t.Errorf("truncated sheet: %v", err)
	}
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if err := f.SetCellValue("Sheet1", "A1", "a_x0000_b"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	doc := run(t, xlsx.Options{}, buf.Bytes())
	if strings.ContainsRune(doc.Markdown, 0) {
		t.Errorf("NUL in output: %q", doc.Markdown)
	}
}

func TestSourceLimitAndHiddenFirst(t *testing.T) {
	data := workbook(t)
	if _, err := xlsx.New(xlsx.Options{Limits: convert.Limits{MaxBytes: int64(len(data))}}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)}); err != nil {
		t.Errorf("at limit: %v", err)
	}
	var limit *convert.LimitError
	if _, err := xlsx.New(xlsx.Options{Limits: convert.Limits{MaxBytes: int64(len(data)) - 1}}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)}); !errors.As(err, &limit) || limit.Limit != convert.LimitSource {
		t.Errorf("one byte over: %v", err)
	}
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.SetCellValue("Sheet1", "A1", "hidden"))
	_, err := f.NewSheet("visible")
	must(err)
	must(f.SetCellValue("visible", "A1", "shown"))
	f.SetActiveSheet(1)
	must(f.SetSheetVisible("Sheet1", false))
	var buf bytes.Buffer
	must(f.Write(&buf))
	doc := run(t, xlsx.Options{}, buf.Bytes())
	if len(doc.Sections) != 2 || doc.Sections[0].Start != doc.Sections[0].End || doc.Sections[1].Number != 2 || doc.Sections[1].Name != "visible" {
		t.Errorf("sections = %+v", doc.Sections)
	}
}

func TestErrors(t *testing.T) {
	data := workbook(t)
	for name, tc := range map[string]struct {
		data   []byte
		limits convert.Limits
		want   error
	}{
		"empty":     {nil, convert.Limits{}, convert.ErrCorrupt},
		"encrypted": {ooxmltest.Encrypted(), convert.Limits{}, convert.ErrEncrypted},
		"not xlsx":  {ooxmltest.Build(t, map[string]string{"a.txt": "x"}), convert.Limits{}, convert.ErrCorrupt},
		"expanded":  {data, convert.Limits{MaxExpandedBytes: 100}, convert.ErrTooLarge},
		"output":    {data, convert.Limits{MaxOutputBytes: 20}, convert.ErrTooLarge},
	} {
		_, err := xlsx.New(xlsx.Options{Limits: tc.limits}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(tc.data)})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func FuzzConvert(f *testing.F) {
	f.Add([]byte("PK"))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := xlsx.New(xlsx.Options{Limits: convert.Limits{MaxOutputBytes: 1 << 20}}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
		if err != nil && doc.Markdown != "" {
			t.Fatalf("partial document on error")
		}
	})
}
