package ui

import (
	"bytes"
	"html"
	"os"
	"regexp"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Snapshot of Pi's builtin dark theme. Status badges deliberately use colors.go.
const (
	mdText        = "#dee0e1"
	mdMuted       = "#9da5a9"
	mdGold        = "#cd9a22"
	mdBlue        = "#69add0"
	mdViolet      = "#a798d7"
	mdGreen       = "#68b78d"
	mdVariable    = "#5db3ba"
	mdString      = "#de8d5a"
	piChromaTheme = "herdr-prs-pi-dark"
)

// Register once, before workers start; never mutate Chroma's global registry
// during rendering. Glamour's embedded Chroma config instead shares "charm".
func init() {
	styles.Register(chroma.MustNewStyle(piChromaTheme, chroma.StyleEntries{
		chroma.Text:          mdGreen,
		chroma.Error:         mdGreen,
		chroma.Comment:       mdMuted,
		chroma.Keyword:       mdBlue,
		chroma.KeywordType:   mdViolet,
		chroma.Name:          mdVariable,
		chroma.NameFunction:  mdGold,
		chroma.NameClass:     mdViolet,
		chroma.NameBuiltin:   mdGold,
		chroma.Operator:      mdMuted,
		chroma.Punctuation:   mdMuted,
		chroma.LiteralString: mdString,
		chroma.LiteralNumber: mdGreen,
	}))
}

func ptr[T any](v T) *T { return &v }

func piDarkMarkdownStyle(profile termenv.Profile) glamouransi.StyleConfig {
	color := func(c string) glamouransi.StylePrimitive { return glamouransi.StylePrimitive{Color: ptr(c)} }
	block := func(s glamouransi.StylePrimitive) glamouransi.StyleBlock {
		return glamouransi.StyleBlock{StylePrimitive: s}
	}
	s := glamouransi.StyleConfig{
		Document:       glamouransi.StyleBlock{StylePrimitive: color(mdText), Margin: ptr(uint(0))},
		Heading:        block(glamouransi.StylePrimitive{Color: ptr(mdGold), Bold: ptr(true), BlockSuffix: "\n"}),
		H1:             block(glamouransi.StylePrimitive{Underline: ptr(true)}),
		H3:             block(glamouransi.StylePrimitive{Prefix: "### "}),
		H4:             block(glamouransi.StylePrimitive{Prefix: "#### "}),
		H5:             block(glamouransi.StylePrimitive{Prefix: "##### "}),
		H6:             block(glamouransi.StylePrimitive{Prefix: "###### "}),
		Strong:         glamouransi.StylePrimitive{Bold: ptr(true)},
		Emph:           glamouransi.StylePrimitive{Italic: ptr(true)},
		Strikethrough:  glamouransi.StylePrimitive{CrossedOut: ptr(true)},
		Code:           block(color(mdViolet)),
		LinkText:       color(mdBlue),
		Link:           color(mdMuted),
		ImageText:      glamouransi.StylePrimitive{Color: ptr(mdMuted), Format: "Image: {{.text}}"},
		Image:          color(mdMuted),
		HorizontalRule: glamouransi.StylePrimitive{Color: ptr(mdMuted), Format: "\n────────\n"},
		BlockQuote:     glamouransi.StyleBlock{StylePrimitive: color(mdMuted), Indent: ptr(uint(1)), IndentToken: ptr(termenv.String("│").Foreground(profile.Color(mdMuted)).String() + " ")},
		List:           glamouransi.StyleList{LevelIndent: 2},
		Item:           glamouransi.StylePrimitive{Color: ptr(mdViolet), Prefix: "• "},
		Enumeration:    glamouransi.StylePrimitive{Color: ptr(mdViolet), Prefix: ". "},
		// Glamour styles task labels as a parent prefix, rather than with Task's
		// color. These trusted SGR labels keep just the checkbox violet.
		Task: glamouransi.StyleTask{
			Ticked:   termenv.String("[✓]").Foreground(profile.Color(mdViolet)).String() + " ",
			Unticked: termenv.String("[ ]").Foreground(profile.Color(mdViolet)).String() + " ",
		},
		CodeBlock: glamouransi.StyleCodeBlock{
			StyleBlock: glamouransi.StyleBlock{StylePrimitive: color(mdMuted), Indent: ptr(uint(2))},
			Theme:      piChromaTheme,
		},
		Table: glamouransi.StyleTable{CenterSeparator: ptr("┼"), ColumnSeparator: ptr("│"), RowSeparator: ptr("─")},
	}
	return s
}

// Lip Gloss detects capabilities, not the background palette. No automatic
// Glamour style, palette query, theme file, or environment style is used.
func markdownProfile() termenv.Profile {
	if os.Getenv("NO_COLOR") != "" {
		return termenv.Ascii
	}
	return lipgloss.ColorProfile()
}

var markdownEntity = regexp.MustCompile(`&(?:#[xX][0-9a-fA-F]+|#[0-9]+|[a-zA-Z][a-zA-Z0-9]+);?`)

func safeMarkdown(s string) string {
	s = sanitize(s)
	// Glamour unescapes HTML entities even in code spans. Remove encoded
	// controls before parsing too, so they cannot inject even a valid SGR.
	return markdownEntity.ReplaceAllStringFunc(s, func(entity string) string {
		decoded := html.UnescapeString(entity)
		clean := sanitize(decoded)
		if clean != decoded {
			return html.EscapeString(clean)
		}
		return entity
	})
}

func renderMarkdown(body string, width int, profile termenv.Profile) []string {
	width = max(1, width)
	formatter := "terminal16m"
	switch profile {
	case termenv.Ascii:
		formatter = "noop"
	case termenv.ANSI:
		formatter = "terminal"
	case termenv.ANSI256:
		formatter = "terminal256"
	}
	// Use Glamour's public Goldmark node renderer, allowing a small AST safety
	// pass without replacing its Markdown parser or formatting implementation.
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	// Set after extensions, as Glamour does: GFM's default HTML renderers must
	// not override the ANSI table/task/strikethrough renderers.
	md.SetRenderer(renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(
		glamouransi.NewRenderer(glamouransi.Options{
			Styles: piDarkMarkdownStyle(profile), ColorProfile: profile,
			ChromaFormatter: formatter, WordWrap: max(16, width),
			TableWrap: ptr(true), InlineTableLinks: true, PreserveNewLines: true,
		}), 1000))))
	source := []byte(safeMarkdown(body))
	doc := md.Parser().Parse(text.NewReader(source))
	// A known plaintext info segment prevents Chroma's language auto-analysis.
	plainInfo := text.NewSegment(len(source), len(source)+4)
	source = append(source, "text"...)
	var remove []ast.Node
	var plainBlocks []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.HTMLBlock, *ast.RawHTML:
			remove = append(remove, n)
			return ast.WalkSkipChildren, nil
		case *ast.FencedCodeBlock:
			lang := string(n.Language(source))
			if lang == "" || lexers.Get(lang) == nil {
				plainBlocks = append(plainBlocks, n)
			}
		case *ast.CodeBlock:
			plainBlocks = append(plainBlocks, n)
		}
		return ast.WalkContinue, nil
	})
	for _, n := range remove {
		n.Parent().RemoveChild(n.Parent(), n)
	}
	for _, n := range plainBlocks {
		f := ast.NewFencedCodeBlock(ast.NewTextSegment(plainInfo))
		f.SetLines(n.Lines())
		n.Parent().ReplaceChild(n.Parent(), n, f)
	}
	var out bytes.Buffer
	if err := md.Renderer().Render(&out, source, doc); err != nil {
		return wrapDetailText(body, width)
	}
	return wrapRenderedANSI(out.String(), width, profile)
}

// Defence in depth: the only terminal protocol allowed out is renderer-owned
// numeric SGR. Input and entity controls were removed before rendering.
func safeRenderedANSI(s string) string {
	var b strings.Builder
	p := ansi.NewParser()
	var state byte
	for len(s) > 0 {
		seq, _, n, next := ansi.DecodeSequence(s, state, p)
		state, s = next, s[n:]
		if strings.HasPrefix(seq, "\x1b[") && p.Command() == 'm' {
			b.WriteString(seq)
		} else if !strings.ContainsAny(seq, "\x1b\u009b\u009d") {
			b.WriteString(sanitize(seq))
		}
	}
	return b.String()
}

func wrapRenderedANSI(s string, width int, profile termenv.Profile) []string {
	s = safeRenderedANSI(s)
	if profile == termenv.Ascii {
		s = ansi.Strip(s)
	}
	// Glamour's word wrapping doesn't bound code, tables or oversized tokens.
	// Hardwrap is grapheme/ANSI aware; truncate handles a 2-cell grapheme at 1.
	lines := strings.Split(ansi.Hardwrap(s, max(1, width), true), "\n")
	var pen cellbuf.Style
	p := ansi.NewParser()
	for i, line := range lines {
		prefix := ""
		if profile != termenv.Ascii && !pen.Empty() {
			prefix = pen.Sequence()
		}
		var state byte
		for rest := line; len(rest) > 0; {
			seq, _, n, next := ansi.DecodeSequence(rest, state, p)
			state, rest = next, rest[n:]
			if strings.HasPrefix(seq, "\x1b[") && p.Command() == 'm' {
				cellbuf.ReadStyle(p.Params(), &pen)
			}
		}
		// Each row is independently drawable (scrolling and side-by-side panes).
		// Replay the active pen after a wrap, reset before the next UI column.
		lines[i] = ansi.Truncate(prefix+line, max(1, width), "…")
		if profile != termenv.Ascii && strings.Contains(lines[i], "\x1b[") {
			lines[i] += "\x1b[0m"
		}
	}
	// Trim only structural blank lines, never styled text or code indentation.
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}
