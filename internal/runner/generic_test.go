package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// genericCfg loads a provider described in testdata and checks that the
// config accepts it.
func genericCfg(t *testing.T, file string) config.ProviderCfg {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var pc config.ProviderCfg
	if err := yaml.Unmarshal(data, &pc); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Providers["described"] = pc
	if err := c.Validate(); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return pc
}

// A plain local model through `ollama run` (the ollama-run preset),
// recorded with Ollama 0.35.1 on Windows: the answer is stdout, the token
// counts are on stderr between spinner escapes.
func TestGenericOllamaRealRecording(t *testing.T) {
	pc, _ := providerCfg(t, "ollama-run")
	stderr, _ := filepath.Abs(filepath.Join("testdata", "ollama_real_hi.err"))
	dump := fakeExe(t, &pc, "ollama_real_hi.txt", 0, "@"+stderr)
	var c collector
	res := New(withProvider(t, "ollama-run", pc))["ollama-run"].Run(context.Background(),
		Spec{AgentID: "o", Role: event.RoleJudge, Model: "qwen3.6:35b-a3b-coding", ReadOnly: true, Prompt: "Reply with exactly: hi", Dir: t.TempDir()}, c.emit)
	if !res.OK() || res.Final != "hi" {
		t.Fatalf("result = %+v", res)
	}
	if res.Tokens.Input != 17 || res.Tokens.Output != 2 || res.Tokens.Cached != 0 {
		t.Errorf("tokens = %+v", res.Tokens)
	}
	if res.SessionID != "" {
		t.Errorf("ollama run has no sessions, got %q", res.SessionID)
	}
	d := readDump(t, dump)
	if got := strings.Join(d.Args, " "); got != "run qwen3.6:35b-a3b-coding --think=false --nowordwrap --verbose" {
		t.Errorf("args = %s", got)
	}
	if d.Stdin != "Reply with exactly: hi" {
		t.Errorf("stdin = %q", d.Stdin)
	}
	for _, e := range c.evs {
		if strings.ContainsRune(e.Text, '\x1b') {
			t.Errorf("escape sequence in an event: %q", e.Text)
		}
	}
}

// The same recording with CRLF line ends (a Windows checkout with
// core.autocrlf, or a CLI that writes them): the counts on stderr must
// still be found. stripANSI took a line end's \r for a spinner's and kept
// only the empty rest of each line.
func TestGenericOllamaCRLF(t *testing.T) {
	pc, _ := providerCfg(t, "ollama-run")
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		lf := strings.ReplaceAll(string(data), "\r\n", "\n")
		return strings.ReplaceAll(lf, "\n", "\r\n")
	}
	p := newGenericParser(pc.Generic).(*textParser)
	for _, l := range strings.SplitAfter(read("ollama_real_hi.txt"), "\n") {
		p.Line([]byte(l))
	}
	p.Stderr(read("ollama_real_hi.err"))
	var res Result
	p.Finish(&res)
	if res.Final != "hi" {
		t.Errorf("final = %q", res.Final)
	}
	if res.Tokens.Input != 17 || res.Tokens.Output != 2 {
		t.Errorf("tokens = %+v", res.Tokens)
	}
}

// A missing model: Ollama tries to pull it and fails on stderr.
func TestGenericOllamaMissingModel(t *testing.T) {
	pc, _ := providerCfg(t, "ollama-run")
	stderr, _ := filepath.Abs(filepath.Join("testdata", "ollama_real_nomodel.err"))
	fakeExe(t, &pc, "", 1, "@"+stderr)
	var c collector
	res := NewGeneric("ollama-run", pc, nil).Run(context.Background(), Spec{AgentID: "o", Model: "no-such-model:1b", ReadOnly: true, Dir: t.TempDir()}, c.emit)
	if res.OK() || res.LimitHit || res.Err == nil || !strings.Contains(res.Err.Error(), "file does not exist") {
		t.Fatalf("want the pull error, got %+v", res)
	}
}

// A plain model cannot keep to a writing step's rules: refused before it starts.
func TestGenericRefusesWorkItCannotDo(t *testing.T) {
	pc, det := providerCfg(t, "ollama-run")
	dump := fakeExe(t, &pc, "ollama_real_hi.txt", 0, "")
	var c collector
	res := NewGeneric("ollama-run", pc, nil).Run(context.Background(), Spec{AgentID: "o", Model: "m", Dir: t.TempDir()}, c.emit)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "read-only work only") {
		t.Fatalf("writer on a plain model: %+v", res)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Error("the CLI was started anyway")
	}
	// A CLI with tools and no read-only mode gets no read-only work.
	tools := genericCfg(t, "generic_qwen.yaml")
	tools.Generic.ReadOnlyArgs = nil
	if err := genericPrecheck("q", tools)(Spec{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "read_only_args") {
		t.Errorf("read-only without a read-only mode: %v", err)
	}
	// A session id from the CLI's output goes back on a command line only
	// when it is plain.
	q := genericCfg(t, "generic_qwen.yaml")
	if err := genericPrecheck("q", q)(Spec{Resume: "abc & calc.exe"}); err == nil {
		t.Error("unsafe session id accepted")
	}
	if err := genericPrecheck("q", q)(Spec{Resume: "87570aa5-038f-479e-b61c-142425f4cad5"}); err != nil {
		t.Errorf("plain session id: %v", err)
	}
	_ = det
}

func TestGenericArgs(t *testing.T) {
	pc := genericCfg(t, "generic_qwen.yaml")
	pc.ExtraArgs = []string{"--auth-type", "openai"}
	ro := strings.Join(GenericArgs(pc, Spec{ReadOnly: true, Model: "qwen3.6:35b"}), " ")
	if ro != "--output-format stream-json -m qwen3.6:35b --approval-mode plan --auth-type openai" {
		t.Errorf("read-only args = %s", ro)
	}
	w := strings.Join(GenericArgs(pc, Spec{Resume: "s-1"}), " ")
	if w != "--output-format stream-json --approval-mode auto-edit --resume s-1 --auth-type openai" {
		t.Errorf("writer args = %s", w)
	}
}

// The generic description of Qwen Code (testdata/generic_qwen.yaml) reads
// every recorded Qwen run exactly like the built-in qwen kind does.
func TestGenericDescribesQwen(t *testing.T) {
	g := genericCfg(t, "generic_qwen.yaml").Generic
	for _, file := range []string{"qwen_real_simple.jsonl", "qwen_real_edit.jsonl", "qwen_real_readonly.jsonl", "qwen_real_resume.jsonl", "qwen_real_noauth.jsonl"} {
		builtin := &claudeParser{inputHasCache: true, noCost: true}
		bEvs := feed(t, builtin, file)
		var want Result
		builtin.Finish(&want)
		gp := newGenericParser(g)
		gEvs := feed(t, gp, file)
		var got Result
		gp.Finish(&got)
		sameResult(t, file, got, want)
		for _, k := range []event.Kind{event.FileEdit, event.Message} {
			if countKind(gEvs, k) != countKind(bEvs, k) {
				t.Errorf("%s: %s events: generic %d, built-in %d", file, k, countKind(gEvs, k), countKind(bEvs, k))
			}
		}
	}
}

// The same for Gemini CLI's stream (testdata/generic_gemini.yaml).
func TestGenericDescribesGemini(t *testing.T) {
	g := genericCfg(t, "generic_gemini.yaml").Generic
	for _, file := range []string{"gemini_stream.jsonl", "gemini_quota.jsonl"} {
		builtin := &geminiParser{}
		bEvs := feed(t, builtin, file)
		var want Result
		builtin.Finish(&want)
		gp := newGenericParser(g).(*jsonRuleParser)
		gEvs := append(feed(t, gp, file), gp.Flush()...)
		var got Result
		gp.Finish(&got)
		sameResult(t, file, got, want)
		for _, k := range []event.Kind{event.FileEdit, event.ToolCall, event.Error} {
			if countKind(gEvs, k) != countKind(bEvs, k) {
				t.Errorf("%s: %s events: generic %d, built-in %d", file, k, countKind(gEvs, k), countKind(bEvs, k))
			}
		}
		// The streamed chunks become whole messages.
		if file == "gemini_stream.jsonl" {
			var msgs []string
			for _, e := range gEvs {
				if e.Kind == event.Message {
					msgs = append(msgs, e.Text)
				}
			}
			if len(msgs) != 2 || msgs[0] != "I'll create the file first." || msgs[1] != got.Final {
				t.Errorf("messages = %q", msgs)
			}
		}
	}
}

func sameResult(t *testing.T, file string, got, want Result) {
	t.Helper()
	if got.Final != want.Final || got.SessionID != want.SessionID || got.Tokens != want.Tokens || !reflect.DeepEqual(got.Files, want.Files) {
		t.Errorf("%s:\n generic  %+v\n built-in %+v", file, got, want)
	}
	if (got.Err == nil) != (want.Err == nil) || (got.Err != nil && got.Err.Error() != want.Err.Error()) {
		t.Errorf("%s: err generic %v, built-in %v", file, got.Err, want.Err)
	}
}

// Limits: an exit code, the provider's own patterns, and a JSON line.
func TestGenericLimits(t *testing.T) {
	pc, _ := providerCfg(t, "ollama-run")
	pc.Generic.LimitExitCodes = []int{7}
	fakeExe(t, &pc, "", 7, "slow down")
	var c collector
	res := NewGeneric("ollama-run", pc, nil).Run(context.Background(), Spec{AgentID: "o", Model: "m", ReadOnly: true, Dir: t.TempDir()}, c.emit)
	if !res.LimitHit || countKind(c.evs, event.LimitHit) != 1 {
		t.Errorf("exit code 7: %+v", res)
	}

	pc, _ = providerCfg(t, "ollama-run")
	pc.Generic.LimitPatterns = []string{`credits? (are )?exhausted`}
	fakeExe(t, &pc, "", 1, "Error: your credits are exhausted, try again at 15:05")
	res = NewGeneric("ollama-run", pc, nil).Run(context.Background(), Spec{AgentID: "o", Model: "m", ReadOnly: true, Dir: t.TempDir()}, c.emit)
	if !res.LimitHit || res.ResetAt.IsZero() {
		t.Errorf("own limit pattern: %+v", res)
	}
	// Without the pattern it is an ordinary failure.
	pc.Generic.LimitPatterns = nil
	res = NewGeneric("ollama-run", pc, nil).Run(context.Background(), Spec{AgentID: "o", Model: "m", ReadOnly: true, Dir: t.TempDir()}, c.emit)
	if res.LimitHit || res.Err == nil {
		t.Errorf("no pattern: %+v", res)
	}

	g := &config.GenericCfg{Output: config.OutputJSONL, JSON: []config.JSONRule{
		{Match: map[string]string{"type": "text"}, Text: "text"},
		{Match: map[string]string{"type": "limit"}, Error: "message", Limit: true},
	}}
	p := newGenericParser(g)
	evs := p.Line([]byte(`{"type":"limit","message":"weekly cap reached"}`))
	var r Result
	p.Finish(&r)
	if len(evs) != 1 || evs[0].Kind != event.LimitHit || !r.LimitHit || r.Err == nil {
		t.Errorf("limit line: %+v %+v", evs, r)
	}
}

func TestGenericTextKeepsParagraphsAndSession(t *testing.T) {
	g := &config.GenericCfg{Session: `^session: (\S+)`, ResumeArgs: []string{"--resume", "{session}"},
		Usage: config.UsagePatterns{Input: `in=([\d,]+)`, Cached: `cached=(\d+)`, Output: `out=(\d+)`}, InputExcludesCached: true}
	p := newGenericParser(g).(*textParser)
	for _, l := range []string{"\x1b[1Gfirst paragraph", "", "second", "session: abc-1"} {
		p.Line([]byte(l))
	}
	p.Stderr("\x1b[?25lin=1,200 cached=800 out=30\n")
	var r Result
	p.Finish(&r)
	if r.Final != "first paragraph\n\nsecond\nsession: abc-1" || r.SessionID != "abc-1" {
		t.Errorf("final %q session %q", r.Final, r.SessionID)
	}
	if r.Tokens.Input != 2000 || r.Tokens.Cached != 800 || r.Tokens.Output != 30 {
		t.Errorf("tokens = %+v", r.Tokens)
	}
}

// withProvider returns the default config with one provider replaced and
// enabled, as runner.New sees it.
func withProvider(t *testing.T, name string, pc config.ProviderCfg) *config.Config {
	t.Helper()
	c := config.Default()
	pc.Disabled = false
	c.Providers[name] = pc
	return c
}
