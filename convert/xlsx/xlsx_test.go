package xlsx_test

import (
	"bytes"
	"context"
	"errors"
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
