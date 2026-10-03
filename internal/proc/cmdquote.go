package proc

import "strings"

// CmdQuote quotes one argument for the command line of a .cmd shim run as
// cmd.exe /d /s /c "<shim> <args>" (prepare on Windows). The line is read
// twice: by cmd.exe, which only tracks double quotes (it has no backslash
// escapes), and then, through the shim's %*, by the CLI's argv parser (the
// MSVCRT/UCRT rules node and most programs use). Anything with spaces or
// cmd metacharacters is wrapped in double quotes, and
//
//   - an embedded " becomes "" : cmd sees the quote close and reopen at
//     once (so & | < > ^ ( ) stay quoted for it), and the CLI reads "" in
//     a quoted part as one literal quote;
//   - backslashes right before an embedded " or the closing quote are
//     doubled: the CLI reads 2n backslashes + quote as n backslashes and a
//     quote character, where n backslashes + quote would turn a run of
//     odd length into an escaped quote and end the argument elsewhere.
//
// %NAME% (and !NAME! with delayed expansion) cannot be escaped inside
// double quotes: cmd.exe expands a defined variable there. Callers that
// pass values through a shim must keep % out of them (runner's tomlString
// writes it as %).
func CmdQuote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\"&|<>^()%!,;=") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(a); i++ {
		switch c := a[i]; c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes))
			b.WriteString(`""`)
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(c)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}
