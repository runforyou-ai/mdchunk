// Command bench compares mdchunk's splitter with langchaingo's
// MarkdownTextSplitter and eino-ext's header plus recursive splitters on the
// split package's golden inputs and a long Chinese paragraph.
//
// Run it from this directory: go run . [-size 200] [-overlap 30]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino-ext/components/document/transformer/splitter/markdown"
	"github.com/cloudwego/eino-ext/components/document/transformer/splitter/recursive"
	"github.com/cloudwego/eino/schema"
	"github.com/tmc/langchaingo/textsplitter"

	"github.com/runforyou-ai/mdchunk/split"
)

// splitter splits a document into chunk texts.
type splitter struct {
	name  string
	split func(string) ([]string, error)
}

// stats summarises one splitter's output on one document.
type stats struct {
	chunks, maxLen, overLimit, midSentence int
	preserved                              bool
	elapsed                                time.Duration
}

func main() {
	size := flag.Int("size", 200, "target chunk length in code points")
	overlap := flag.Int("overlap", 30, "overlap in code points")
	flag.Parse()

	mdchunk, err := split.New(split.Options{Size: *size, Overlap: *overlap})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	headers, err := markdown.NewHeaderSplitter(ctx, &markdown.HeaderConfig{
		Headers: map[string]string{"#": "h1", "##": "h2", "###": "h3"}, TrimHeaders: true,
	})
	if err != nil {
		panic(err)
	}
	recursiveSplitter, err := recursive.NewSplitter(ctx, &recursive.Config{
		ChunkSize: *size, OverlapSize: *overlap, LenFunc: utf8.RuneCountInString, KeepType: recursive.KeepTypeEnd,
		Separators: []string{"\n\n", "\n", "。", "；", "，", ". ", " ", ""},
	})
	if err != nil {
		panic(err)
	}
	langchain := textsplitter.NewMarkdownTextSplitter(
		textsplitter.WithChunkSize(*size), textsplitter.WithChunkOverlap(*overlap),
		textsplitter.WithHeadingHierarchy(true), textsplitter.WithCodeBlocks(true), textsplitter.WithJoinTableRows(true),
	)
	splitters := []splitter{
		{"mdchunk", func(text string) ([]string, error) {
			var out []string
			for _, c := range mdchunk.Split(text) {
				out = append(out, c.Text)
			}
			return out, nil
		}},
		{"langchaingo", langchain.SplitText},
		{"eino-ext", func(text string) ([]string, error) {
			sections, err := headers.Transform(ctx, []*schema.Document{{Content: text}})
			if err != nil {
				return nil, err
			}
			docs, err := recursiveSplitter.Transform(ctx, sections)
			if err != nil {
				return nil, err
			}
			var out []string
			for _, d := range docs {
				out = append(out, d.Content)
			}
			return out, nil
		}},
	}

	documents := map[string]string{"zh-paragraph": strings.Repeat("退款申请需在签收后七天内提交，客服会在两个工作日内审核。审核通过后款项原路退回，到账时间取决于支付渠道；", 20)}
	files, _ := filepath.Glob("../split/testdata/*.md")
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			panic(err)
		}
		documents[strings.TrimSuffix(filepath.Base(file), ".md")] = string(data)
	}

	limit := *size + *size/5
	fmt.Printf("Size %d, overlap %d; over limit means longer than %d code points.\n\n", *size, *overlap, limit)
	fmt.Println("| document | splitter | chunks | longest | over limit | ends mid-sentence | source preserved | time |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	for _, name := range sortedKeys(documents) {
		text := documents[name]
		for _, s := range splitters {
			st := measure(s, text, limit)
			fmt.Printf("| %s | %s | %d | %d | %d | %d | %v | %s |\n", name, s.name, st.chunks, st.maxLen, st.overLimit,
				st.midSentence, st.preserved, st.elapsed.Round(time.Microsecond))
		}
	}
}

// measure runs s on text and summarises the chunks.
func measure(s splitter, text string, limit int) stats {
	start := time.Now()
	chunks, err := s.split(text)
	elapsed := time.Since(start)
	if err != nil {
		panic(err)
	}
	st := stats{chunks: len(chunks), preserved: true, elapsed: elapsed}
	for i, c := range chunks {
		n := utf8.RuneCountInString(c)
		st.maxLen = max(st.maxLen, n)
		if n > limit {
			st.overLimit++
		}
		if !strings.Contains(text, c) {
			st.preserved = false
		}
		// A chunk other than the last that ends inside CJK prose ends mid-sentence.
		trimmed := strings.TrimRight(c, " \n")
		last, _ := utf8.DecodeLastRuneInString(trimmed)
		if i < len(chunks)-1 && last >= 0x4e00 && last <= 0x9fff {
			st.midSentence++
		}
	}
	return st
}

// sortedKeys returns the map's keys in order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
