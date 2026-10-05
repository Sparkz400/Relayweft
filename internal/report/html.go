package report

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// HTML writes the report as one self-contained page: inline CSS, light and
// dark through prefers-color-scheme, <details> for everything long. All
// text goes through html/template, and a Content-Security-Policy allows no
// script but the print helper below and loads nothing from anywhere, so
// hostile task text, agent output or diff lines stay inert.
func (d *Data) HTML(w io.Writer) error {
	return pageTmpl.Execute(w, d)
}

// printScript opens every <details> for printing and restores them after.
const printScript = `addEventListener("beforeprint",function(){document.querySelectorAll("details").forEach(function(d){d.dataset.o=d.open?"1":"";d.open=true})});addEventListener("afterprint",function(){document.querySelectorAll("details").forEach(function(d){d.open=d.dataset.o==="1"})});`

func scriptHash() string {
	h := sha256.Sum256([]byte(printScript))
	return "sha256-" + base64.StdEncoding.EncodeToString(h[:])
}

// CSP is the page's Content-Security-Policy.
var CSP = "default-src 'none'; style-src 'unsafe-inline'; script-src '" + scriptHash() + "'; img-src data:; base-uri 'none'; form-action 'none'"

var funcs = template.FuncMap{
	"tok":  func(n int64) string { return event.HumanTokens(n) },
	"dur":  humanDur,
	"when": func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
	"pct":  func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"conf": func(f float64) string { return fmt.Sprintf("%.2f", f) },
	"usd":  func(f float64) string { return fmt.Sprintf("$%.2f", f) },
	"join": strings.Join,
	"csp":  func() string { return CSP },
	"lines": func(n int) string {
		if n == 1 {
			return "1 line"
		}
		return fmt.Sprintf("%d lines", n)
	},
	"quota": func(m map[string]float64, p string) (string, error) {
		v, ok := m[p]
		if !ok {
			return "-", nil
		}
		return fmt.Sprintf("%.0f%%", v*100), nil
	},
	"usage": func(m map[string]event.TokenUsage, p string) event.TokenUsage { return m[p] },
	"statusClass": func(s string) string {
		switch s {
		case "done", "ok":
			return "ok"
		case "failed", "cancelled":
			return "fail"
		}
		return "warn"
	},
	"open": func(fd FileDiff, i int) bool { return i < 8 && len(fd.Lines) <= 200 },
}

func humanDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

var pageTmpl = template.Must(template.New("page").Funcs(funcs).Parse(pageHTML))

const pageCSS = `
:root{--codex:#10A37F;--claude:#D97757;--font:-apple-system,BlinkMacSystemFont,"Segoe UI Variable Text","Segoe UI",Inter,Roboto,"Helvetica Neue",Arial,sans-serif;--mono:ui-monospace,"Cascadia Code","SF Mono","JetBrains Mono",Menlo,Consolas,"Liberation Mono",monospace;
color-scheme:light;--bg:#f5f6f8;--panel:#fff;--panel-2:#f7f8fa;--panel-3:#eef0f3;--border:#e3e6eb;--text:#16191d;--text-2:#353b43;--muted:#646d79;--faint:#9aa2ad;
--router:#0a7fc0;--reviewer:#7a55d6;--ok:#17964f;--fail:#d63c3c;--warn:#c07400;--add-bg:rgba(23,150,79,.10);--add-fg:#116b39;--del-bg:rgba(214,60,60,.09);--del-fg:#a52b2b;--hunk-bg:rgba(10,127,192,.07);
--kw:#8839c7;--str:#3c7a1c;--com:#8a939d;--num:#b5551f;--fn:#2d5fc7}
@media (prefers-color-scheme:dark){:root{color-scheme:dark;--bg:#0b0d10;--panel:#111418;--panel-2:#161a1f;--panel-3:#1c2127;--border:#232931;--text:#e7e9ec;--text-2:#c2c7ce;--muted:#8a919c;--faint:#59616c;
--router:#4FC1FF;--reviewer:#B48EFF;--ok:#3DDC84;--fail:#FF6B6B;--warn:#FFB347;--add-bg:rgba(61,220,132,.10);--add-fg:#7ee2a8;--del-bg:rgba(255,107,107,.10);--del-fg:#ff9a9a;--hunk-bg:rgba(79,193,255,.08);
--kw:#c792ea;--str:#c3e88d;--com:#6a737d;--num:#f78c6c;--fn:#82aaff}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 var(--font);-webkit-font-smoothing:antialiased}
main{max-width:1100px;margin:0 auto;padding:28px 20px 60px}
header{display:flex;flex-wrap:wrap;align-items:center;gap:10px 14px;margin-bottom:6px}
.brand{font-weight:750;letter-spacing:.02em}.brand b{color:var(--codex)}.brand i{font-style:normal;color:var(--claude)}
h1{font-size:20px;line-height:1.35;margin:6px 0 14px;font-weight:650;white-space:pre-wrap;overflow-wrap:anywhere}
h2{font-size:12px;font-weight:650;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);margin:0 0 10px}
section{background:var(--panel);border:1px solid var(--border);border-radius:14px;padding:14px 16px;margin:14px 0;box-shadow:0 1px 2px rgba(0,0,0,.06)}
.meta{display:flex;flex-wrap:wrap;gap:6px 18px;color:var(--muted);font-size:13px}.meta b{color:var(--text-2);font-weight:600}
.pill{display:inline-flex;align-items:center;font-size:11px;font-weight:650;padding:2px 8px;border-radius:99px;text-transform:uppercase;letter-spacing:.05em;background:var(--panel-3);color:var(--muted);white-space:nowrap}
.pill.ok{color:var(--ok);background:color-mix(in srgb,var(--ok) 13%,transparent)}.pill.fail{color:var(--fail);background:color-mix(in srgb,var(--fail) 13%,transparent)}.pill.warn{color:var(--warn);background:color-mix(in srgb,var(--warn) 13%,transparent)}
.codex{color:var(--codex)}.claude{color:var(--claude)}.rule{color:var(--router)}.rev{color:var(--reviewer)}
table{width:100%;border-collapse:collapse;font-size:13px}th{text-align:left;color:var(--muted);font-weight:600;font-size:12px;border-bottom:1px solid var(--border);padding:6px 8px}
td{padding:6px 8px;border-bottom:1px solid var(--border);vertical-align:top;overflow-wrap:anywhere}tr:last-child td{border-bottom:0}td.n{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.mono,code,pre{font-family:var(--mono);font-size:12.5px}
pre{white-space:pre-wrap;overflow-wrap:anywhere;margin:6px 0 0;padding:10px 12px;background:var(--panel-2);border:1px solid var(--border);border-radius:8px;max-height:520px;overflow:auto}
details{margin:4px 0}summary{cursor:pointer;color:var(--text-2)}summary:hover{color:var(--text)}
.muted{color:var(--muted)}.small{font-size:12px}.note{color:var(--warn)}
.file{border:1px solid var(--border);border-radius:10px;margin:10px 0;overflow:hidden}
.file>summary{padding:8px 12px;background:var(--panel-2);font-family:var(--mono);font-size:12.5px;display:flex;gap:10px;align-items:center;list-style:none}
.file>summary::-webkit-details-marker{display:none}.file>summary::before{content:"\25B8";color:var(--faint)}.file[open]>summary::before{content:"\25BE"}
.file>summary .p{flex:1;overflow-wrap:anywhere}.a{color:var(--add-fg)}.d{color:var(--del-fg)}
.st{font-weight:700;width:1.2em;text-align:center}
.diff{font-family:var(--mono);font-size:12.5px;line-height:1.55;overflow-x:auto}
.dl{white-space:pre;padding:0 12px;min-height:1.55em}.dl.add{background:var(--add-bg)}.dl.del{background:var(--del-bg)}
.dl.add .sg{color:var(--add-fg)}.dl.del .sg{color:var(--del-fg)}.dl .sg{display:inline-block;width:1.3em;color:var(--faint);user-select:none}
.dl.hunk{background:var(--hunk-bg);color:var(--router);padding:3px 12px}.dl.note{color:var(--faint)}
.kw{color:var(--kw)}.str{color:var(--str)}.com{color:var(--com);font-style:italic}.num{color:var(--num)}.fn{color:var(--fn)}
.cmd{display:flex;gap:8px;align-items:center}.cmd code{background:var(--panel-2);border:1px solid var(--border);border-radius:7px;padding:6px 10px;overflow-wrap:anywhere}
footer{color:var(--faint);font-size:12px;margin-top:24px;text-align:center}
@media print{body{background:#fff;color:#000}section{box-shadow:none;break-inside:avoid-page}.file{break-inside:auto}pre{max-height:none}.dl{white-space:pre-wrap}}
`

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="{{csp}}">
<meta name="referrer" content="no-referrer">
<meta name="generator" content="Switchyard {{.Version}}">
<title>Switchyard report {{.ID}}</title>
<style>` + pageCSS + `</style>
</head>
<body>
<main>
<header>
  <span class="brand">Switch<b>y</b>ar<i>d</i> task report</span>
  <span class="pill {{statusClass .Status}}">{{.Status}}</span>
  {{if .Mode}}<span class="pill">{{.Mode}}</span>{{end}}
  <span class="muted small mono">{{.ID}}</span>
</header>
<h1>{{.Task}}</h1>
<div class="meta">
  <span>started <b>{{when .Created}}</b></span>
  {{if not .Updated.IsZero}}<span>last update <b>{{when .Updated}}</b></span>{{end}}
  <span>took <b>{{dur .Duration}}</b></span>
  {{if .Phase}}<span>phase <b>{{.Phase}}</b></span>{{end}}
  {{if .Dir}}<span>in <b class="mono">{{.Dir}}</b></span>{{end}}
</div>
{{range .Notes}}<p class="note small">{{.}}</p>{{end}}

{{if .Summary}}<section><h2>Result</h2><pre>{{.Summary}}</pre></section>{{end}}

{{if .Steps}}<section><h2>Plan</h2>
{{if .PlanSummary}}<p class="muted">{{.PlanSummary}}</p>{{end}}
<table><thead><tr><th>Step</th><th>Kind</th><th>Role</th><th>Route</th><th>Depends on</th><th>Result</th></tr></thead><tbody>
{{range .Steps}}<tr>
  <td><b class="mono">{{.ID}}</b> {{.Title}}{{if .Files}}<div class="muted small mono">{{join .Files ", "}}</div>{{end}}
  {{if .BestOf}}<div class="muted small">{{.BestOf}}</div>{{end}}
  {{if .Final}}<details><summary class="small">final answer</summary><pre>{{.Final}}</pre></details>{{end}}
  {{if .Err}}<details><summary class="small">error</summary><pre>{{.Err}}</pre></details>{{end}}</td>
  <td>{{.Kind}}</td><td>{{.Role}}</td><td class="mono small">{{.Route}}</td><td class="mono small">{{join .DependsOn ", "}}</td>
  <td><span class="pill {{statusClass .Result}}">{{.Result}}</span></td>
</tr>{{end}}
</tbody></table></section>{{end}}

{{if .Routes}}<section><h2>Routing decisions</h2>
<table><thead><tr><th>Agent / step</th><th>Role</th><th>Route</th><th>Rule</th><th>Reason</th><th>Conf.</th><th>Outcome</th><th class="n">Tokens</th><th class="n">Time</th></tr></thead><tbody>
{{range .Routes}}<tr>
  <td class="mono small">{{.Agent}}<div class="muted">{{.Step}}{{if gt .Attempt 1}} #{{.Attempt}}{{end}}</div></td>
  <td>{{.Role}}</td>
  <td class="mono small"><span class="{{.Provider}}">{{.Provider}}</span>:{{.Model}}{{if .Effort}} @{{.Effort}}{{end}}</td>
  <td class="rule small">{{.Rule}}{{if .Judged}} <span class="pill">judged</span>{{end}}{{if .Fallback}} <span class="pill warn">fallback</span>{{end}}</td>
  <td class="small">{{.Reason}}{{if .Final}}<details><summary class="small">answer</summary><pre>{{.Final}}</pre></details>{{end}}{{if .Error}}<details><summary class="small">error</summary><pre>{{.Error}}</pre></details>{{end}}</td>
  <td class="n">{{if .Confidence}}{{conf .Confidence}}{{end}}</td>
  <td>{{if .LimitHit}}<span class="pill fail">limit</span>{{else if not .Ran}}<span class="pill">not run</span>{{else if .OK}}<span class="pill ok">ok</span>{{else}}<span class="pill fail">failed</span>{{end}}</td>
  <td class="n">{{if .Ran}}{{tok .Tokens.Total}}{{end}}</td><td class="n">{{if .Ran}}{{dur .Duration}}{{end}}</td>
</tr>{{end}}
</tbody></table></section>{{end}}

{{if .Reviews}}<section><h2>Reviews</h2>
{{range .Reviews}}<details{{if not .Approve}} open{{end}}><summary><span class="pill {{if .Approve}}ok{{else}}fail{{end}}">{{if .Approve}}approved{{else}}changes requested{{end}}</span> <span class="rev">{{.Checkpoint}}</span> <span class="muted small mono">{{.Provider}}:{{.Model}}</span></summary>{{if .Advice}}<pre>{{.Advice}}</pre>{{end}}</details>
{{end}}</section>{{end}}

{{if .Checks}}<section><h2>Checks</h2>
<table><thead><tr><th>Command</th><th>Kind</th><th>Tests</th><th>Result</th><th class="n">Time</th></tr></thead><tbody>
{{range .Checks}}<tr><td class="mono">{{.Command}}</td><td>{{.Kind}}</td><td class="small">{{.Tests}}</td><td><span class="pill {{if .OK}}ok{{else}}fail{{end}}">{{if .OK}}pass{{else}}fail{{end}}</span></td><td class="n">{{dur .Duration}}</td></tr>{{end}}
</tbody></table></section>{{end}}

{{if or .Limits .Merges}}<section><h2>Limits and merges</h2>
{{range .Limits}}<div class="small"><span class="pill fail">limit</span> <span class="{{.Provider}}">{{.Provider}}</span>:{{.Model}} <span class="muted mono">{{.Agent}}</span> {{.Text}}</div>{{end}}
{{range .Merges}}<div class="small"><span class="pill {{if .OK}}ok{{else}}fail{{end}}">merge</span> <span class="mono">{{.Step}}</span> {{.Text}}</div>{{end}}
</section>{{end}}

<section><h2>Cost</h2>
{{if .HasCost}}<table><thead><tr><th>Provider</th><th class="n">Fresh tokens</th><th class="n">Input</th><th class="n">Cached</th><th class="n">Output</th><th class="n">Limit before</th><th class="n">Limit after</th></tr></thead><tbody>
{{$c := .Cost}}{{range .Providers}}{{$u := usage $c.PerProvider .}}<tr><td class="{{.}}">{{.}}</td><td class="n">{{tok $u.Total}}</td><td class="n">{{tok $u.Input}}</td><td class="n">{{tok $u.Cached}}</td><td class="n">{{tok $u.Output}}</td><td class="n">{{quota $c.QuotaBefore .}}</td><td class="n">{{quota $c.QuotaAfter .}}</td></tr>{{end}}
</tbody></table>
{{if .Cost.CostUSD}}<p class="small">≈ <b>{{usd .Cost.CostUSD}}</b> API-equivalent for the Claude part (not billed on a subscription).</p>{{end}}
{{else}}<p class="muted">{{if .CostLine}}{{.CostLine}}{{else}}no cost recorded{{end}}</p>{{end}}
</section>

{{with .Diff}}<section><h2>Changes</h2>
<p class="small"><b>{{len .Files}}</b> file(s), <span class="a">+{{.Add}}</span> <span class="d">-{{.Del}}</span> <span class="muted mono">{{.Before}} → {{.After}}</span></p>
{{if .Undone}}<p class="note small">This task was undone since; the diff shows what it had changed.</p>{{end}}
{{if .Truncated}}<p class="note small">Large diff: some lines are not shown. The full diff: <code>git diff {{.Before}} {{.After}}</code></p>{{end}}
{{range $i, $f := .Files}}<details class="file"{{if open $f $i}} open{{end}}><summary><span class="st {{if eq .Status "A"}}a{{else if eq .Status "D"}}d{{end}}">{{.Status}}</span><span class="p">{{.Path}}</span>{{if .Binary}}<span class="muted">binary</span>{{else}}<span class="a">+{{.Add}}</span><span class="d">-{{.Del}}</span>{{end}}</summary>
{{if .Lines}}<div class="diff">{{range .Lines}}<div class="dl {{.Kind}}">{{range .Tokens}}{{if .Class}}<span class="{{.Class}}">{{.Text}}</span>{{else}}{{.Text}}{{end}}{{end}}</div>{{end}}</div>{{end}}
{{if .Truncated}}<p class="muted small" style="padding:0 12px">{{if .Lines}}more lines not shown{{else}}content not shown (size limit){{end}}</p>{{end}}
</details>{{else}}<p class="muted">no file changes</p>{{end}}
</section>{{end}}

{{if .UndoCmd}}<section><h2>Undo</h2><div class="cmd"><code>{{.UndoCmd}}</code></div>
<p class="muted small">Restores the files this task changed; files edited since are 3-way merged, and nothing is written on a conflict. Run it on the machine where the task ran.</p></section>{{end}}

<footer>Generated by Switchyard {{.Version}} on {{when .Generated}}. Contains the task text, agent answers and the repo diff; no environment or credentials.</footer>
</main>
<script>` + printScript + `</script>
</body>
</html>
`
