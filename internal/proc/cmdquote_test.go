package proc

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// ucrtArgs splits a command line (without the program name) the way the
// Universal CRT does (parse_cmdline in the UCRT sources; node and most
// MSVC-built CLIs read their argv this way):
//
//   - spaces and tabs separate arguments outside double quotes;
//   - 2n backslashes + " give n backslashes and a quote that toggles
//     quoting; 2n+1 backslashes + " give n backslashes and a literal ";
//     backslashes not followed by " are literal;
//   - inside quotes, "" gives one literal " and quoting goes on.
func ucrtArgs(line string) []string {
	var args []string
	p := 0
	for {
		for p < len(line) && (line[p] == ' ' || line[p] == '\t') {
			p++
		}
		if p >= len(line) {
			return args
		}
		var b strings.Builder
		inQuote := false
		for {
			copyChar := true
			slashes := 0
			for p < len(line) && line[p] == '\\' {
				p++
				slashes++
			}
			if p < len(line) && line[p] == '"' {
				if slashes%2 == 0 {
					if inQuote && p+1 < len(line) && line[p+1] == '"' {
						p++ // "" inside quotes: a literal quote
					} else {
						copyChar = false
						inQuote = !inQuote
					}
				}
				slashes /= 2
			}
			b.WriteString(strings.Repeat(`\`, slashes))
			if p >= len(line) || (!inQuote && (line[p] == ' ' || line[p] == '\t')) {
				break
			}
			if copyChar {
				b.WriteByte(line[p])
			}
			p++
		}
		args = append(args, b.String())
	}
}

// cmdExposed returns the cmd.exe metacharacters of line that are outside
// double quotes. cmd.exe toggles quoting at every " (it knows no backslash
// escapes) and runs & | < > as operators and ^ as an escape outside quotes.
func cmdExposed(line string) string {
	var out []byte
	q := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '"':
			q = !q
		case !q && strings.IndexByte("&|<>^()", c) >= 0:
			out = append(out, c)
		}
	}
	if q {
		out = append(out, '"') // an unbalanced quote: cmd's view and the CLI's differ
	}
	return string(out)
}

func TestUCRTParserReference(t *testing.T) {
	// Examples from Microsoft's "Parsing C command-line arguments" table.
	for line, want := range map[string][]string{
		`"a b c" d e`:          {"a b c", "d", "e"},
		`"ab\"c" "\\" d`:       {`ab"c`, `\`, "d"},
		`a\\\b d"e f"g h`:      {`a\\\b`, "de fg", "h"},
		`a\\\"b c d`:           {`a\"b`, "c", "d"},
		`a\\\\"b c" d e`:       {`a\\b c`, "d", "e"},
		`a"b"" c d`:            {`ab" c d`},
		`"x""y" z`:             {`x"y`, "z"},
		"  lead\ttab  ":        {"lead", "tab"},
		`"trailing\\" next`:    {`trailing\`, "next"},
		`"open\" never closed`: {`open" never closed`},
	} {
		if got := ucrtArgs(line); !slices.Equal(got, want) {
			t.Errorf("ucrtArgs(%s) = %q, want %q", line, got, want)
		}
	}
}

// Every argument survives cmd.exe and the CLI's argv parser unchanged, as
// exactly one argument, with no cmd metacharacter outside quotes.
func TestCmdQuoteRoundTrip(t *testing.T) {
	cases := []string{
		"plain", "", " ", "a b", `C:\Program Files\x`, `C:\`, `C:\dir\`, `C:\dir\\`, `\\server\share\`,
		`"`, `""`, `a"b`, `a\"b`, `a\\"b`, `\"`, `\\"`, `"\`, `say "hi" & bye`, `\"\" a`,
		`a&b`, `a|b`, `a<b>c`, `^`, `a^b`, `(x)`, `%PATH%`, `100%`, `!x!`, `a,b;c=d`,
		"tab\there", `Bash(go test *)`, `model_reasoning_effort=high`,
		// The review's injection: a TOML value that tried to end the
		// argument and add a -c of its own.
		`mcp_servers.x.args=["' \" -c sandbox_mode=danger-full-access \""]`,
		`mcp_servers.x.env={K = "a\\" -c sandbox_mode=danger-full-access \\"b"}`,
		`x\" & calc & "`, `trailing\ "quoted\"`,
	}
	for _, a := range cases {
		q := CmdQuote(a)
		if ex := cmdExposed(q); ex != "" {
			t.Errorf("CmdQuote(%q) = %s exposes %q to cmd.exe", a, q, ex)
		}
		// Next to other arguments, as prepare joins them.
		line := strings.Join([]string{"exec", q, "-c", CmdQuote(a), "-"}, " ")
		want := []string{"exec", a, "-c", a, "-"}
		if got := ucrtArgs(line); !slices.Equal(got, want) {
			t.Errorf("CmdQuote(%q) = %s: argv %q, want %q", a, q, got, want)
		}
	}
}

func TestCmdQuoteExamples(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     "plain",
		`C:\x\y`:    `C:\x\y`,
		"":          `""`,
		"a b":       `"a b"`,
		`C:\a b\`:   `"C:\a b\\"`,
		`a"b`:       `"a""b"`,
		`a\"b`:      `"a\\""b"`,
		`a & b\\`:   `"a & b\\\\"`,
		`x\y "z"\w`: `"x\y ""z""\w"`,
	} {
		if got := CmdQuote(in); got != want {
			t.Errorf("CmdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func ExampleCmdQuote() {
	fmt.Println(CmdQuote(`C:\Users\Nico Schu\`))
	// Output: "C:\Users\Nico Schu\\"
}
