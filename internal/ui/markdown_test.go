package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func markdownOutput(body string, width int, profile termenv.Profile) string {
	return strings.Join(renderMarkdown(body, width, profile), "\n")
}

func styleAt(t *testing.T, out, target string) cellbuf.Style {
	t.Helper()
	var plain strings.Builder
	var pen cellbuf.Style
	pens := map[int]cellbuf.Style{}
	p := ansi.NewParser()
	var state byte
	for len(out) > 0 {
		seq, _, n, next := ansi.DecodeSequence(out, state, p)
		state, out = next, out[n:]
		if strings.HasPrefix(seq, "\x1b[") && p.Command() == 'm' {
			cellbuf.ReadStyle(p.Params(), &pen)
		} else {
			for i := 0; i < len(seq); i++ {
				pens[plain.Len()+i] = pen
			}
			plain.WriteString(seq)
		}
	}
	index := strings.Index(plain.String(), target)
	if index < 0 {
		t.Fatalf("missing %q in %q", target, plain.String())
	}
	return pens[index]
}

func assertMDColor(t *testing.T, out, target, want string) cellbuf.Style {
	t.Helper()
	pen := styleAt(t, out, target)
	if pen.Fg == nil {
		t.Fatalf("%q has no foreground", target)
	}
	r, g, b, _ := pen.Fg.RGBA()
	got := fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	if got != want {
		t.Fatalf("%q: color %s != %s", target, got, want)
	}
	return pen
}

func TestPiDarkMarkdownSemantics(t *testing.T) {
	out := markdownOutput("# First\n\n## Second\n\n### Third\n\nordinary **bold** *italic* ~~struck~~ `inline` [label](https://example.com)\n\n- bullet\n- [x] done\n- [ ] todo\n\n1. numbered\n\n> quote\n\n---\n\n| Column | Value |\n| --- | --- |\n| cell | value |", 90, termenv.TrueColor)
	plain := ansi.Strip(out)
	for _, s := range []string{"First", "Second", "### Third", "ordinary bold italic struck inline label", "•", "[✓]", "[ ]", "1.", "│", "quote", "Column", "cell", "┼", "────────"} {
		if !strings.Contains(plain, s) {
			t.Fatalf("missing %q in %q", s, plain)
		}
	}
	for _, s := range []string{"# First", "## Second", "**bold**", "*italic*", "~~struck~~", "`inline`", "[label]("} {
		if strings.Contains(plain, s) {
			t.Fatalf("raw Markdown %q", s)
		}
	}
	first := assertMDColor(t, out, "First", mdGold)
	if first.Attrs&cellbuf.BoldAttr == 0 || first.UlStyle != cellbuf.SingleUnderline {
		t.Fatal("H1 not bold/underlined")
	}
	second := assertMDColor(t, out, "Second", mdGold)
	if second.Attrs&cellbuf.BoldAttr == 0 || second.UlStyle != cellbuf.NoUnderline {
		t.Fatal("H2 style")
	}
	assertMDColor(t, out, "### Third", mdGold)
	assertMDColor(t, out, "ordinary", mdText)
	assertMDColor(t, out, "inline", mdViolet)
	assertMDColor(t, out, "label", mdBlue)
	assertMDColor(t, out, "https://example.com", mdMuted)
	assertMDColor(t, out, "•", mdViolet)
	assertMDColor(t, out, "[✓]", mdViolet)
	assertMDColor(t, out, "quote", mdMuted)
	assertMDColor(t, out, "│", mdMuted)
	assertMDColor(t, out, "────────", mdMuted)
	if styleAt(t, out, "bold").Attrs&cellbuf.BoldAttr == 0 {
		t.Fatal("bold missing")
	}
	if styleAt(t, out, "italic").Attrs&cellbuf.ItalicAttr == 0 {
		t.Fatal("italic missing")
	}
	if styleAt(t, out, "struck").Attrs&cellbuf.StrikethroughAttr == 0 {
		t.Fatal("strike missing")
	}
}

func TestPiDarkSyntaxAndPlainFallback(t *testing.T) {
	out := markdownOutput("```python\n# comment\ndef function(variable):\n    return \"string\" + 42\n```", 80, termenv.TrueColor)
	for target, color := range map[string]string{
		"# comment": mdMuted, "def": mdBlue, "function": mdGold, "variable": mdVariable,
		"string": mdString, "42": mdGreen, "+": mdMuted, "(": mdMuted,
	} {
		assertMDColor(t, out, target, color)
	}
	typed := markdownOutput("```go\nvar value int\n```", 80, termenv.TrueColor)
	assertMDColor(t, typed, "int", mdViolet)
	for _, fence := range []string{"", "unknown-herdr-language", "text"} {
		out := markdownOutput("```"+fence+"\ndef function(variable): return 42\n```", 80, termenv.TrueColor)
		assertMDColor(t, out, "def", mdGreen)
		assertMDColor(t, out, "function", mdGreen)
		if !strings.HasPrefix(strings.TrimLeft(ansi.Strip(out), "\n"), "  def") {
			t.Fatal("missing code indent")
		}
	}
	out = markdownOutput("    def function(): return 42\n", 80, termenv.TrueColor)
	assertMDColor(t, out, "def", mdGreen)
}

func TestMarkdownTerminalSafetyAndHTML(t *testing.T) {
	input := "# safe\x1b[31mred\x1b[0m\n\n" +
		"body\x1b]52;c;PAYLOAD\a\x1b]8;;https://evil\x1b\\link\x1b]8;;\x1b\\\x1b[2J\u202e\u2066\x00\n\n" +
		"<script>HTMLPAYLOAD</script>\n\ntext <b>bold</b> &#27;[31m entity &#x1b;]52;c;ENCODED&#7;\n\n" +
		"![alt](https://example.invalid/image.png)\n\n`&#27;[32mcode`"
	for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
		out := markdownOutput(input, 80, profile)
		for _, bad := range []string{"PAYLOAD", "HTMLPAYLOAD", "<script>", "<b>", "\x1b]", "\x1b[2J", "\x1b[31m", "\x1b[32m", "\u202e", "\u2066", "\x00"} {
			if strings.Contains(out, bad) {
				t.Fatalf("unsafe output %q: %q", bad, out)
			}
		}
		if profile == termenv.Ascii && strings.Contains(out, "\x1b") {
			t.Fatal("NO_COLOR output has ANSI")
		}
		if !strings.Contains(ansi.Strip(out), "alt") {
			t.Fatal("image alt missing")
		}
		if out != safeRenderedANSI(out) {
			t.Fatal("non-SGR output")
		}
	}
	if got := safeRenderedANSI("\x1b[31mOK\x1b]52;c;hidden\a\x1b[2J\x1b[0m"); got != "\x1b[31mOK\x1b[0m" {
		t.Fatalf("protocol allowlist: %q", got)
	}
}

func TestMarkdownWidthAndIndependentANSI(t *testing.T) {
	text := strings.Repeat("界👩‍💻e\u0301", 24)
	body := "# " + text + "\n\n**" + text + "**\n\n```text\n" + text + "\n```\n\n| long | wide |\n| --- | --- |\n| " + text + " | " + text + " |\n\n> " + text
	for _, width := range []int{1, 2, 3, 7, 16, 28, 55, 120} {
		for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
			lines := renderMarkdown(body, width, profile)
			for _, line := range lines {
				if ansi.StringWidth(line) > width {
					t.Fatalf("%d cols: %q", width, line)
				}
				if profile != termenv.Ascii && strings.Contains(line, "\x1b[") && !strings.HasSuffix(line, "\x1b[0m") {
					t.Fatalf("style bleed %q", line)
				}
			}
		}
	}
	lines := wrapRenderedANSI("\x1b[1;38;2;205;154;34mabcdefghij\x1b[0m", 3, termenv.TrueColor)
	for _, line := range lines {
		pen := assertMDColor(t, line, strings.TrimSpace(ansi.Strip(line)), mdGold)
		if pen.Attrs&cellbuf.BoldAttr == 0 {
			t.Fatal("lost style at wrap")
		}
	}
}

func finishRender(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if _, ok := msg.(renderedDetailMsg); !ok {
		t.Fatalf("expected render command, got %T", msg)
	}
	m.Update(msg)
}

func TestDetailRendersProseAndKeepsMetadataLiteral(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	p := fixture(1)
	p.Title, p.MergeState = "**literal title**", "CLEAN"
	p.Checks = []prs.Check{{Name: "**literal check**", Conclusion: "SUCCESS"}}
	d := prs.Detail{PR: p, Body: "# description",
		Reviews:  []prs.Review{{Author: "**author**", State: "APPROVED", Body: "**review**"}},
		Threads:  []prs.Thread{{Path: "*path*.go\x1b]52;c;METADATA-PAYLOAD\a", Line: 7, Comments: []prs.Comment{{Author: "# reviewer", Body: "`inline-thread`"}}}, {Resolved: true, Comments: []prs.Comment{{Body: "hidden"}}}},
		Comments: []prs.Comment{{Author: "*person*", CreatedAt: "today", Body: "*conversation*"}},
		Warnings: []string{"**literal warning**\u202e"},
	}
	lines := formatDetail(d, 80, func(body string) []string {
		return renderMarkdown(body, 80, termenv.TrueColor)
	})
	out := strings.Join(lines, "\n")
	plain := ansi.Strip(out)
	for _, literal := range []string{"**literal title**", "**literal check** · SUCCESS", "**author** · APPROVED", "*path*.go:7", "# reviewer:", "*person* · today", "**literal warning**"} {
		if !strings.Contains(plain, literal) {
			t.Fatalf("metadata parsed: %q in %q", literal, plain)
		}
	}
	if ansi.Strip(lines[0]) != "**literal title**" || !strings.Contains(ansi.Strip(lines[1]), "ready · owner/repo #1") || ansi.Strip(lines[2]) != p.URL || strings.Count(plain, "**literal title**") != 1 {
		t.Fatalf("detail header lost title/status/URL order: %q", plain)
	}
	if styleAt(t, out, "**literal title**").Fg != nil {
		t.Fatal("detail title inherited badge/prose coloring")
	}
	assertRenderedForeground(t, out, "ready", "#a3be8c")
	if strings.Contains(out, "METADATA-PAYLOAD") || strings.ContainsAny(out, "\u202e\a") {
		t.Fatal("literal metadata retained terminal controls")
	}
	assertMDColor(t, out, "description", mdGold)
	assertMDColor(t, out, "inline-thread", mdViolet)
	for _, width := range []int{1, 3, 28, 55, 120} {
		lines := formatDetail(d, width, func(body string) []string { return renderMarkdown(body, width, termenv.TrueColor) })
		for _, line := range lines {
			if ansi.StringWidth(line) > width {
				t.Fatalf("integrated width %d: %q", width, line)
			}
		}
	}
	if styleAt(t, out, "review").Attrs&cellbuf.BoldAttr == 0 {
		t.Fatal("review not rendered")
	}
	if styleAt(t, out, "conversation").Attrs&cellbuf.ItalicAttr == 0 {
		t.Fatal("conversation not rendered")
	}
	if strings.Contains(plain, "hidden") {
		t.Fatal("resolved thread rendered")
	}
}

func TestMarkdownNoFetchingOrSharedRendererState(t *testing.T) {
	var fetched atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	body := "# title\n\n![alt](" + server.URL + "/image) [link](" + server.URL + "/page)\n\n```go\n// comment\nfunc f() { println(42) }\n```"
	want := markdownOutput(body, 40, termenv.TrueColor)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := markdownOutput(body, 40, termenv.TrueColor); got != want {
				t.Error("concurrent rendering changed output")
			}
		}()
	}
	wg.Wait()
	if fetched.Load() != 0 {
		t.Fatal("renderer fetched a URL")
	}
	for _, profile := range []termenv.Profile{termenv.ANSI, termenv.ANSI256} {
		out := markdownOutput(body, 40, profile)
		if strings.Contains(out, "38;2;") {
			t.Fatal("renderer ignored limited color profile")
		}
	}
}

func TestMarkdownNoColorProfile(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if markdownProfile() != termenv.Ascii {
		t.Fatal("NO_COLOR ignored")
	}
	out := markdownOutput("# heading\n\n**bold** `code`\n\n```go\nvar i int = 42\n```", 40, markdownProfile())
	if strings.Contains(out, "\x1b") || strings.Contains(out, "**") {
		t.Fatalf("no-color rendering %q", out)
	}
}
