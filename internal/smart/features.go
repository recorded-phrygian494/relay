// Package smart implements the tiered smart-routing classifiers
// (DESIGN §0.3): tier 1 is a pure-Go lexical/heuristic difficulty+domain
// classifier (zero external calls, deterministic); tier 2 is opt-in
// embedding KNN over a reference set. Every decision carries a Why string
// naming the features that produced it — "the model felt like it" is
// banned by construction.
package smart

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/llmrelay/relay/internal/core"
)

// Features are the named signals tier 1 extracts from a request. All of
// them are cheap, deterministic string statistics.
type Features struct {
	Words        int // words in the last user message
	TotalWords   int // words across all user messages
	MsgDepth     int // conversation turns
	ToolCount    int // tools offered on the request
	CodeFences   int // ``` blocks
	CodeHits     int // code keywords (func, def, SELECT, import, ...)
	MathHits     int // math/probability markers
	ReasonHits   int // prove/derive/design/compare/step-by-step markers
	EasyHits     int // greetings, thanks, "just reply", venting markers
	ExtractHits  int // "extract"/"as JSON"/"list the" markers
	SummaryHits  int // summarize/tl;dr/condense markers
	Numbers      int // tokens containing digits — math word problems are digit-heavy
	QuestionOnly bool
	JSONMode     bool
	matched      map[string][]string // feature -> matched keywords, for Why
}

var (
	codeWords = []string{"func ", "def ", "class ", "import ", "select ", "from ", "return ", "const ", "var ", "=>", "async ", "await ", "public ", "print(", "console.", "err !=", "null", "regex", "compile", "script", "sql", "query", "function", "typescript", "python", "golang", " go ", "bash", "debug", "stack trace", "exception", "deadlock", "segfault"}
	mathWords = []string{"probability", "prove", "theorem", "integral", "derivative", "equation", "compound", "fraction", "percent", "km/h", "l/min", "modulo", "prime", "irrational", "matrix", "vector", "sqrt", "logarithm", "geometry", "combinator", "permutation", "how far", "how long until", "without replacement", "show your work", "show the calculation"}
	reasonWords = []string{"prove", "derive", "why does", "explain why", "step by step", "walk through", "design a", "compare", "trade-off", "tradeoff", "justify", "systematic", "generalize", "rigorous", "optimal", "strategy", "architecture", "debugging plan", "root cause", "halting problem", "diagonaliz"}
	easyWords = []string{"hey", "hi ", "hello", "thanks", "thank you", "lol", "good morning", "good night", "wish me luck", "just reply", "no advice", "venting", "friendlier", "icebreaker", "what's up", "how's it going"}
	extractWords = []string{"extract", "as json", "json array", "comma-separated", "list the", "pull out", "return json", "iso format", "ticker", "parse"}
	summaryWords = []string{"summarize", "summary", "tl;dr", "tldr", "condense", "shorten", "in two sentences", "one paragraph", "three bullets"}
)

var fenceRe = regexp.MustCompile("```")

func countHits(text string, words []string, matched *[]string) int {
	n := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			n++
			*matched = append(*matched, strings.TrimSpace(w))
		}
	}
	return n
}

// Featurize extracts Features from the request IR. Only user-authored
// text is consulted; system prompts describe the app, not the query.
func Featurize(req *core.Request) Features {
	var last, all strings.Builder
	depth := 0
	for _, m := range req.Messages {
		depth++
		if m.Role != core.RoleUser {
			continue
		}
		var txt strings.Builder
		for _, p := range m.Parts {
			if tp, ok := p.(core.TextPart); ok {
				txt.WriteString(tp.Text)
				txt.WriteString(" ")
			}
		}
		last.Reset()
		last.WriteString(txt.String())
		all.WriteString(txt.String())
	}
	lastLower := strings.ToLower(last.String())
	f := Features{
		Words:      len(strings.Fields(last.String())),
		TotalWords: len(strings.Fields(all.String())),
		MsgDepth:   depth,
		ToolCount:  len(req.Tools),
		CodeFences: len(fenceRe.FindAllString(last.String(), -1)) / 2,
		JSONMode:   req.ResponseFormat != nil && req.ResponseFormat.Type != "" && req.ResponseFormat.Type != "text",
		matched:    map[string][]string{},
	}
	var m []string
	f.CodeHits = countHits(lastLower, codeWords, &m)
	f.matched["code"], m = m, nil
	f.MathHits = countHits(lastLower, mathWords, &m)
	f.matched["math"], m = m, nil
	f.ReasonHits = countHits(lastLower, reasonWords, &m)
	f.matched["reason"], m = m, nil
	f.EasyHits = countHits(lastLower, easyWords, &m)
	f.matched["easy"], m = m, nil
	f.ExtractHits = countHits(lastLower, extractWords, &m)
	f.matched["extract"], m = m, nil
	f.SummaryHits = countHits(lastLower, summaryWords, &m)
	f.matched["summary"], m = m, nil
	for _, tok := range strings.Fields(lastLower) {
		if strings.ContainsAny(tok, "0123456789") {
			f.Numbers++
		}
	}
	f.QuestionOnly = strings.HasSuffix(strings.TrimSpace(lastLower), "?") && f.Words < 25
	return f
}

// explain renders the non-zero features compactly for the Why string.
func (f Features) explain() string {
	parts := []string{fmt.Sprintf("words=%d", f.Words)}
	add := func(name string, n int) {
		if n == 0 {
			return
		}
		if kws := f.matched[name]; len(kws) > 0 {
			if len(kws) > 3 {
				kws = kws[:3]
			}
			sort.Strings(kws)
			parts = append(parts, fmt.Sprintf("%s=%d(%s)", name, n, strings.Join(kws, ",")))
			return
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, n))
	}
	add("code", f.CodeHits)
	if f.CodeFences > 0 {
		parts = append(parts, fmt.Sprintf("fences=%d", f.CodeFences))
	}
	add("math", f.MathHits)
	add("reason", f.ReasonHits)
	add("easy", f.EasyHits)
	add("extract", f.ExtractHits)
	add("summary", f.SummaryHits)
	if f.Numbers > 0 {
		parts = append(parts, fmt.Sprintf("numbers=%d", f.Numbers))
	}
	if f.JSONMode {
		parts = append(parts, "json-mode")
	}
	if f.ToolCount > 0 {
		parts = append(parts, fmt.Sprintf("tools=%d", f.ToolCount))
	}
	if f.QuestionOnly {
		parts = append(parts, "short-question")
	}
	return strings.Join(parts, " ")
}
