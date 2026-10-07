# mdchunk design

## Goals

- Turn documents in the supported formats (or any format a custom converter
  handles) into Markdown chunks that trace back to their source: which bytes of
  the Markdown, and which page, slide or sheet of the original file.
- Chunk Markdown well for CJK and Latin text alike: structure-aware boundaries,
  heading paths and table headers as context, bounded overlap.
- Stay framework-agnostic. No models, embeddings, vector stores or retrievers.
- Generality first. The first user does not shape the API or its defaults.

## Non-goals

- Multiple splitter variants (recursive, token, semantic) or token budgets.
- Model calls of any kind: semantic splitting, generated titles, OCR, captions.
- Legacy binary formats (`.doc`, `.xls`, `.ppt`), PDF passwords, image extraction.
- Layout fidelity, byte-level reversibility to the source file, or every chunk
  being independently valid Markdown.
- Streaming of unbounded input, fetching, caching, retries, deduplication.
- Database-specific full-text tokenisation.

## Two ideas the design rests on

1. **Markdown is the only intermediate format.** Converters produce Markdown;
   the splitter consumes Markdown. Users can bring their own Markdown or plug in
   any external parser. There is no public document AST.
2. **Everything is a byte range.** Converters record where each page, slide or
   sheet lands in the final Markdown (`Section`); the splitter records where each
   chunk and each heading it inherits lands (`Chunk`, `Heading`). Intersecting
   ranges answers "chunk 7 comes from pages 3–4" without either stage knowing
   about the other.

```
source ──convert──▶ Document{Markdown, Sections} ──split──▶ []Chunk{Start, End, Headings…}
                          └────────── joined by byte ranges ──────────┘
```

## Module layout

One Go module, `github.com/runforyou-ai/mdchunk`, MIT. The `go` directive is
the lowest version the module and its dependencies support; CI tests that
version and the latest stable release.

| Package | Purpose | Third-party deps |
|---|---|---|
| `split` | Markdown → chunks | none |
| `convert` | `Document`, `Section`, `Input`, `Converter`, `Registry`, `Format`, `Limits`, errors | none |
| `convert/text` | plain text, Markdown, JSON | `golang.org/x/text` |
| `convert/html` | HTML | html-to-markdown v2, `golang.org/x/net`, `golang.org/x/text` |
| `convert/csv` | CSV | `golang.org/x/text` |
| `convert/docx` | Word | none |
| `convert/pptx` | PowerPoint | none |
| `convert/xlsx` | Excel | excelize |
| `convert/pdf` | PDF via PDFium WebAssembly | go-pdfium, wazero |
| `convert/all` | every built-in converter in one registry | all of the above |
| `internal/textdecode` | BOM, declared charset, fallback decoding | `golang.org/x/text` |
| `internal/ooxml` | OOXML package reading with expansion limits | none |
| `internal/mdwrite` | Markdown escaping, tables, fences, output limits | none |
| `internal/source` | reading input under the source limit | none |
| `internal/ooxmltest`, `internal/pdftest` | building test documents | none |

Only imported packages are compiled, so importing `split` or `convert` pulls
in no third-party code. The comparison benchmark against other splitters lives
in a nested module `bench/` so its dependencies never reach the main `go.mod`.

## Package `split`

```go
// Options configures a Splitter. Lengths count code points (see Length).
type Options struct {
	Size    int // target chunk length; required, > 0
	MaxSize int // hard upper bound; 0 means Size + Size/5; must be >= Size
	Overlap int // maximum overlap with the previous chunk; 0 <= Overlap < Size
}

// New validates opts and returns an immutable Splitter, safe for concurrent use.
func New(opts Options) (*Splitter, error)

func (s *Splitter) Split(text string) []Chunk

// Chunk is a contiguous byte range of the input.
type Chunk struct {
	Text        string       // input[Start:End]; shares memory with the input
	Start, End  int          // byte offsets into the input
	Length      int          // length of Text as defined below
	Headings    []Heading    // inherited heading path, outermost first
	TableHeader *TableHeader // set when the chunk starts inside a table body
}

// Heading is a heading the chunk inherits but does not contain.
type Heading struct {
	Level      int    // 1–6
	Text       string // inline Markdown without block markers
	Start, End int    // byte range of the heading block in the input
}

// TableHeader is the header and delimiter rows of the table a chunk starts in.
type TableHeader struct {
	Text       string
	Start, End int
}

// Context renders the heading path and the table header.
func (c Chunk) Context() string

// TextWithContext returns Context, a blank line and Text; Text alone when Context is empty.
func (c Chunk) TextWithContext() string
```

`Context` is byte-exact and part of the compatibility promise: the heading
texts joined by `" > "` (not escaped), then `TableHeader.Text` unchanged; the
two parts are joined by a single `"\n"` and an empty part adds no separator.
Both empty yields `""`. Neither method adds a trailing newline. `Heading` and
`TableHeader` ranges start at the first line's start (including indentation)
and end at the last line's end (excluding its terminator).

### Length

Length is the number of code points after treating CRLF and lone CR as one LF.
Each byte of an invalid UTF-8 sequence counts as one. `Size`, `MaxSize`,
`Overlap` and `Chunk.Length` all use this measure, so `Length` may differ from
`utf8.RuneCountInString(Text)` on CRLF input. The splitter never rewrites text.

### Contract

These are tested properties; exact chunk boundaries are not part of the
compatibility promise and may improve between versions.

1. Whitespace-only input yields nil. Otherwise the first chunk starts at 0, the
   last ends at `len(input)`, `Start` and `End` strictly increase, and each
   chunk starts at or before the previous chunk's end (no gaps).
2. `Text == input[Start:End]`; no offset falls inside a UTF-8 sequence or
   between the bytes of a CRLF.
3. Concatenating `input[prev.End:c.End]` over all chunks reproduces the input.
4. `Length <= MaxSize` for every chunk.
5. Overlap with the previous chunk is at most `Overlap` and is best effort: it
   is zero at headings and when no boundary exists inside the overlap window.
6. A chunk contains heading blocks only as a run of consecutive headings at
   its start (blank lines between them allowed). A heading run longer than
   `MaxSize` is cut between headings first; a single heading longer than
   `MaxSize` is cut inside.
7. `Headings` is the heading path in effect at the first non-blank byte of the
   chunk (the anchor), excluding a heading that starts at or contains the
   anchor. A chunk that continues a heading cut inside therefore inherits only
   that heading's ancestors; the heading itself enters `Headings` from the
   chunks of its body.

### Preferences

Within the contract, in priority order:

1. Cut at heading blocks. A run of consecutive headings stays together, and a
   heading stays with the start of its body, when they fit in `MaxSize`.
2. When the text up to the next heading fits in `MaxSize`, take it whole;
   otherwise split it into near-equal pieces around `Size`.
3. Choose the highest-ranked boundary nearest each piece's target position:
   block > line break > sentence end > clause > whitespace > enclosed position >
   any code point. Beyond `Size` only block boundaries count at first; when
   that leaves only enclosed or no boundaries, a line break, sentence end,
   clause or whitespace up to `MaxSize` keeps a line or heading whole; failing
   that, a heading that fits ends the chunk without its body's first line. Pieces
   are at least `max(Overlap+1, Size/2)` long when the text allows, avoiding
   fragments.
4. Start the next chunk at the highest-ranked, earliest boundary inside the
   overlap window; never start it at trailing whitespace.
5. Every chunk holds some non-whitespace text, unless it lies in a whitespace
   run of at least `Size` code points or cannot join either neighbouring chunk
   within `MaxSize`.

Sentence ends are `。！？`, and `.!?` followed by whitespace; clauses are `，；`,
and `,;` followed by whitespace. Adjacent plain-text lines where the first ends
in a letter or digit and the next starts with a letter, digit or mid-sentence
punctuation are a soft wrap: the line break ranks as whitespace. A soft wrap
never spans a blank line or a hard line break (two trailing spaces or `\`).

Enclosed regions (fenced and indented code, tables, HTML blocks, front matter,
inline code spans) only offer line breaks; positions inside a line are used only
when a single line exceeds the budget.

### Markdown coverage

The splitter is a line scanner, not a CommonMark parser. Block constructs allow
at most three spaces of indentation. Each line is classified in this order:

1. Inside an open fence: code until a closing fence of the same character and
   at least the same length. An unclosed fence runs to the end of input.
2. Front matter: only when the first line (after an optional BOM) is `---` and a
   closing `---` or `...` line exists; otherwise the line is not front matter.
3. Inside an HTML block: until its end condition. Recognised starts are
   `<script`, `<pre`, `<style`, `<textarea` (end at the closing tag), `<!--`
   (end at `-->`) and CommonMark block-level tag names (end at a blank line).
   Other `<…>` lines are text.
4. Fence opening (three or more backticks or tildes).
5. Blank line.
6. Setext underline (`=` or `-` run) directly after one or more paragraph lines:
   the whole paragraph becomes a level 1 or 2 heading.
7. Thematic break (`***`, `---`, `___` with optional spaces).
8. List item (`-`, `*`, `+` or `1.`/`1)` followed by a space). Inside a
   paragraph only an item with content, and for ordered lists numbered 1,
   opens a list; otherwise the line continues the paragraph. Indented lines
   after an item belong to it.
9. ATX heading (`#`–`######` followed by space, tab or end of line).
10. GFM table: a header row and a delimiter row with the same cell count start a
   table; it continues while lines contain an unescaped `|`. Leading and
   trailing pipes are optional. A header whose cells are all empty gives no
   `TableHeader`.
11. Blockquote line (`>`).
12. Indented code: four or more spaces after a blank line, outside a list.
13. Paragraph text.

Lines inside blockquotes and list items are not scanned for headings or tables,
so content there never enters the global heading path. Block boundaries are
placed before headings, after blank-line runs, and on entering or leaving a
fence, table, HTML block, list item or blockquote.

### Complexity

Scanning is linear in the input length; choosing cuts scans at most
`MaxSize` positions per chunk, so splitting is linear for a fixed
`MaxSize / Size` ratio. Working memory is about 6 bytes per input byte (an
`int8` boundary rank and an `int32` code point index per byte, plus line
tables). Inputs longer than `math.MaxInt32` bytes panic.

## Package `convert`

```go
// Format is a normalised file extension: lower case, no dot, aliases resolved
// (markdown→md, text→txt, htm→html, yml→yaml).
type Format string

// Formats of the built-in converters.
const (
	Text     Format = "txt"
	Markdown Format = "md"
	JSON     Format = "json"
	HTML     Format = "html"
	CSV      Format = "csv"
	DOCX     Format = "docx"
	PPTX     Format = "pptx"
	XLSX     Format = "xlsx"
	PDF      Format = "pdf"
)

func ParseFormat(s string) Format          // accepts "PDF", ".pdf", "markdown"
func FormatOf(filename string) (Format, bool)

// Input is one source document.
type Input struct {
	Reader  io.Reader // not closed by converters
	Name    string    // optional, for error messages
	Charset string    // optional declared encoding, e.g. from an HTTP header
	BaseURL string    // optional, resolves relative links in HTML
}

type Document struct {
	Markdown string
	Sections []Section // in document order by Start
}

type SectionKind string

const (
	KindPage  SectionKind = "page"
	KindSlide SectionKind = "slide"
	KindSheet SectionKind = "sheet"
)

// Section is one citable unit of the source and its byte range in Markdown.
type Section struct {
	Kind       SectionKind
	Number     int    // 1-based position in the source, counting empty and skipped units
	Name       string // sheet name, slide title or page label when known
	Start, End int
}

// SectionsIn returns a new slice of the sections whose ranges intersect
// [start, end): start < end && s.Start < s.End && s.Start < end && start < s.End.
// Empty sections and empty queries never match.
func (d Document) SectionsIn(start, end int) []Section

type Converter interface {
	Convert(ctx context.Context, in Input) (Document, error)
}

type ConverterFunc func(ctx context.Context, in Input) (Document, error)

// Registry maps formats to converters. The zero value is ready to use and safe
// for concurrent use. Formats are normalised with ParseFormat on Register and
// Lookup; a later registration replaces an earlier one. It never closes the
// converters it holds.
type Registry struct{ /* … */ }

func (r *Registry) Register(c Converter, formats ...Format)
func (r *Registry) Lookup(f Format) (Converter, bool)
func (r *Registry) Formats() []Format
func (r *Registry) Convert(ctx context.Context, f Format, in Input) (Document, error)

// Limits bounds resource use per converter. Every built-in package's Options
// embeds Limits; Registry.Convert applies no limits of its own.
// Zero fields take the defaults; negative fields disable the limit.
type Limits struct {
	MaxBytes         int64 // source bytes read; default 32 MiB
	MaxExpandedBytes int64 // decompressed archive bytes and HTML table copies; default 128 MiB
	MaxOutputBytes   int64 // Markdown bytes produced; default 64 MiB
}

// Effective returns the limits with defaults applied; -1 means unlimited.
func (l Limits) Effective() Limits

var (
	ErrUnsupported = errors.New("mdchunk/convert: unsupported format")
	ErrEncrypted   = errors.New("mdchunk/convert: encrypted document")
	ErrCorrupt     = errors.New("mdchunk/convert: malformed document")
	ErrTooLarge    = errors.New("mdchunk/convert: limit exceeded")
	ErrClosed      = errors.New("mdchunk/convert: converter closed")
)

// LimitError reports which limit was exceeded; errors.Is(err, ErrTooLarge) holds.
type LimitError struct {
	Limit string // "source", "expanded" or "output"
	Max   int64
}
```

### Rules every built-in converter follows

- Output is valid UTF-8 with LF line endings, no BOM and no NUL bytes. Content
  is otherwise not trimmed or rewritten beyond what the format requires.
- Sections are recorded against the final Markdown; nothing rewrites it after.
  A section covers everything generated for its unit, including generated
  headings. Built-in sections never overlap; text between them belongs to none.
  Units without text keep an empty section so numbering stays stable.
- Generated Markdown escapes source text that would otherwise form structure:
  line-leading `#`, `>`, list markers, fences and table pipes.
- Classified failures wrap a sentinel with `%w` and keep the cause in the chain.
  I/O and runtime failures (reader errors, PDFium start-up) are returned with
  context but unclassified. Cancellation satisfies
  `errors.Is(err, context.Canceled)` or `context.DeadlineExceeded`. On failure
  the Document is zero.
- Empty input to a text format (text, Markdown, JSON, CSV, HTML) converts to an
  empty Document without error. Empty input to a binary format (DOCX, PPTX,
  XLSX, PDF) is `ErrCorrupt`. A valid PDF without a text layer yields empty
  Markdown and one empty page section per page.
- Sources are read up to `MaxBytes+1` to detect `MaxBytes` overflow. Output is
  limited while it is built, not after; where a third-party library builds the
  output, the input to it is bounded first (see `convert/html`).
- Cancellation is checked between reads, while waiting for a worker, and per
  page, slide, sheet, row or block. A blocked `Read` or a single third-party
  call is not interrupted.
- Built-in converters are safe for concurrent use. A custom converter is
  responsible for its own safety; the Registry releases its lock before calling.

### Options

Every built-in package exports `Options` and a constructor taking it by value;
the zero value is the default configuration. `Options` embeds `convert.Limits`.
Packages that decode text (text, HTML, CSV) also have
`Fallback encoding.Encoding`. `all.Options` has one field per built-in package
holding that package's `Options`, plus shared `Limits` and `Fallback`.

### Text decoding

Shared by text, HTML and CSV: a BOM (UTF-8, UTF-16 or UTF-32) wins, then
`Input.Charset`, then (HTML only) the first `<meta>` declaration before
`<body>`, then UTF-8. A declared charset is used only when it names a known
encoding other than the replacement encoding. Undeclared input that is not
valid UTF-8 is decoded with `Options.Fallback` when set (for example
GB18030). Bytes that cannot be decoded become U+FFFD.

### Built-in converters

- `convert/text`: `text.New(opts)` for plain text and Markdown emits the
  decoded text; plain text is read as Markdown by the splitter, as most plain
  text is. `text.JSON(opts)` wraps the decoded text in a `json` fence longer
  than any backtick run it contains.
- `convert/html`: `html.New(opts)`. CommonMark with GFM tables. Before
  conversion every table, innermost first, is expanded to a rectangular grid:
  spans, clamped to the HTML standard's 1000 columns and 65534 rows (a leading
  `+` and overlong numbers parse as browsers do) and kept within their row
  group, repeat their cell; short rows are padded; pipes in inline code
  (`code`, `var`, `samp`, `kbd`, `tt`) inside cells are escaped. Before a table
  is expanded, a heuristic score of its copies (64 per copied node, 16 per byte
  of copied text or attribute whether rendered or not, links and images also
  charged `Input.BaseURL`) is charged against what is left of
  `MaxExpandedBytes`, or the conversion fails with an expanded `LimitError`.
  The score approximates conversion work and is not a hard memory bound. The
  output limit applies to the final Markdown.
  A table without header cells promotes its first
  row; relative links resolve against `Input.BaseURL`. A table whose cells
  contain another table is not expanded; the underlying library renders it as
  text around the inner table. That library builds the whole output and ignores
  the context, so the output limit and cancellation are checked again once it
  returns.
- `convert/csv`: `csv.New(opts)` with `Comma`, `NoHeader` and `LazyQuotes`.
  One table; the first row is the header. With `NoHeader` every row is a body
  row under an empty header row of the same width, so the output stays a GFM
  table (and the splitter gives no `TableHeader`).
- `convert/docx`: `docx.New(opts)`. Headings from outline levels and heading
  styles, numbered and bulleted lists, tables (horizontal spans repeated,
  vertical merges carry the value down), external hyperlinks. No sections.
  Encrypted files (an OLE compound file, not a zip) return `ErrEncrypted`.
- `convert/pptx`: `pptx.New(opts)`. One `KindSlide` section per slide, the title
  placeholder as a level-1 heading, text boxes, tables, chart data as tables
  and speaker notes. `SkipNotes`, `IncludeHidden` options.
- `convert/xlsx`: `xlsx.New(opts)`. One `KindSheet` section per sheet with the
  sheet name as a level-2 heading and a table; empty rows skipped. Displayed
  (number-formatted) values by default, `RawValues` for stored values;
  `IncludeHidden` option. Before the spreadsheet library reads the package,
  every XML part it may read is checked: parts declared as XML, parts it loads
  from fixed paths, targets of core relationships (resolved both ways the
  library may resolve them) and other targets that start like XML. The check
  guards against damaged files: core parts a relationship names must exist,
  and two entries whose names differ only in case or separators are rejected.
  A package crafted so that the library resolves a part differently, or that
  drops a part together with its relationship, can still convert to less
  text, as a file holding less text would.
- `convert/pdf`: `pdf.New(opts)` returns `*pdf.Converter` with `Close() error`.
  One `KindPage` section per page. Characters are read once each, in PDFium's
  text order: CRLF pairs in its character stream end lines, the spaces it
  generates or reads separate words, and other control characters count as
  spaces. The dominant font size is body text, larger short lines become
  headings (`NoHeadings` disables this), and paragraphs break on spacing or
  size changes. Multi-column layout and table recovery are not attempted.
  Documents that need a password to open return `ErrEncrypted`; documents
  restricted only by an owner password convert normally, and permission flags
  are not checked.
- `convert/all`: `all.New(opts) *all.Registry`. Embeds `*convert.Registry`
  with every format above registered, and `Close` releases the converters it
  created (the PDF pool), not ones registered later. `all.Options` also has
  shared `Limits`, applied field by field to each converter whose own field is
  zero, and a shared `Fallback` for converters whose own is nil.

### Lifecycle of `pdf.Converter`

- PDFium instances start lazily on first use; a failed start is retried on the
  next call.
- `Workers` (default 2) is both the instance count and a semaphore acquired
  before the source is read; waiting honours the context.
- `Close` is idempotent. It marks the converter closed; calls that hold a
  worker finish and are waited for; calls still waiting for a worker, or woken
  after the mark, return `ErrClosed`. Once the calls in flight finish, the
  instances are released; every call to `Close` returns only after that.

## Testing

- Unit tests with `-race`; examples as `example_test.go` per package.
- Golden files in `testdata/` per converter and for the splitter (Markdown in,
  JSON chunks out), refreshed with a package-level `-update` flag. PDF goldens
  assert structure (pages, headings, text presence) rather than exact bytes.
- Property tests and a fuzz target for `Splitter.Split` covering the contract,
  with seeds for CRLF, lone CR, BOM, invalid UTF-8, emoji, very long lines,
  tiny budgets, long heading runs, unclosed fences and front matter.
- Fuzz targets for malformed DOCX, PPTX, XLSX, CSV and HTML.
- Lifecycle tests for the PDF converter: concurrent first use, cancellation
  while waiting, `Close` racing `Convert`, retry after a failed start.
- Limit tests at exactly the limit and one byte over, for source, expansion and
  output growth.
- End-to-end tests: convert → split → `SectionsIn`, including multibyte text,
  empty pages and chunks spanning pages.
- Benchmarks report throughput and allocations; the nested `bench/` module
  compares output with langchaingo and eino-ext splitters.

## Delivery

Each pull request is reviewed before merge. `v0.1.0` is tagged after the last.

1. Scaffolding (license, READMEs, CI, lint, Makefile, this document) and `split`.
2. `convert`, `internal/textdecode`, `internal/mdwrite`, `convert/text`.
3. `convert/html`.
4. `internal/ooxml`, `convert/docx`, `convert/pptx`.
5. `convert/csv`, `convert/xlsx`.
6. `convert/pdf`.
7. `convert/all`, end-to-end tests, `bench/`, release `v0.1.0`.
