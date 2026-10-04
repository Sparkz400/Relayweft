package notify

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	old := lookupEnv
	lookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	t.Cleanup(func() { lookupEnv = old })
}

func TestWebhookKind(t *testing.T) {
	for url, want := range map[string]string{
		"https://hooks.slack.com/services/T0/B0/x":         KindSlack,
		"https://discord.com/api/webhooks/1/abc":           KindDiscord,
		"https://discordapp.com/api/webhooks/1/abc":        KindDiscord,
		"https://ptb.discord.com/api/webhooks/1/abc":       KindDiscord,
		"https://discord.com/channels/1/2":                 "",
		"https://ntfy.sh/my-topic":                         KindNtfy,
		"https://ntfy.example.org/topic":                   KindNtfy,
		"https://example.org/hook":                         "",
		"https://hooks.slack.com.evil.example/services/x":  "",
		"https://evil.example/api/webhooks/discord.com/xy": "",
	} {
		if got := (Webhook{URL: url}).kind(url); got != want {
			t.Errorf("kind(%s) = %q, want %q", url, got, want)
		}
	}
	if got := (Webhook{URL: "https://example.org/x", Kind: "NTFY"}).kind("https://example.org/x"); got != KindNtfy {
		t.Errorf("explicit kind = %q", got)
	}
}

func TestWebhookValidate(t *testing.T) {
	ok := []Webhook{
		{URL: "https://ntfy.sh/t"},
		{URL: "https://example.org/x", Kind: "json"},
		{URL: "http://192.168.1.5:8080/sy", Kind: "ntfy"},
		{URL: "${SY_HOOK}"}, // checked when sent
		{URL: "https://ntfy.sh/t", Events: []string{"done", "Failed", "watch"}},
	}
	for _, w := range ok {
		if err := w.Validate(); err != nil {
			t.Errorf("%+v: %v", w, err)
		}
	}
	bad := map[string]Webhook{
		"url is empty":       {URL: " "},
		"kind must be":       {URL: "https://ntfy.sh/t", Kind: "teams"},
		`event "finished"`:   {URL: "https://ntfy.sh/t", Events: []string{"finished"}},
		"must start with":    {URL: "ftp://ntfy.sh/t"},
		"cannot be told":     {URL: "https://example.org/x"},
		"must start with ht": {URL: "ntfy.sh/topic"},
	}
	for want, w := range bad {
		if err := w.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: got %v, want %q", w, err, want)
		}
	}
}

func TestWebhookWants(t *testing.T) {
	all := Webhook{}
	some := Webhook{Events: []string{"failed", "WATCH"}}
	for _, ev := range Events {
		if !all.Wants(ev) {
			t.Errorf("no events list should want %s", ev)
		}
	}
	if some.Wants(EventDone) || !some.Wants(EventFailed) || !some.Wants(EventWatch) || !some.Wants(EventTest) {
		t.Errorf("filtered wants wrong")
	}
	if Wanted([]Webhook{some}, EventDone) || !Wanted([]Webhook{some, all}, EventDone) || Wanted(nil, EventTest) {
		t.Errorf("Wanted wrong")
	}
}

// capture builds the request for w and m and returns it with its body.
func capture(t *testing.T, w Webhook, m Message) (*http.Request, string) {
	t.Helper()
	req, err := w.request(context.Background(), m)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	b, _ := io.ReadAll(req.Body)
	if req.Method != http.MethodPost {
		t.Errorf("method %s", req.Method)
	}
	return req, string(b)
}

var hostile = Message{
	Event:  EventFailed,
	Title:  "Switchyard: failed",
	Body:   "<!channel> @everyone [click](https://evil.example) *bold* & more\r\nline\x00two",
	Source: "my repo",
	Link:   "https://github.com/o/r/pull/7",
}

func TestSlackPayload(t *testing.T) {
	req, body := capture(t, Webhook{URL: "https://hooks.slack.com/services/T/B/x"}, hostile)
	var p struct{ Text string }
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", req.Header.Get("Content-Type"))
	}
	if strings.Contains(p.Text, "<!channel>") || !strings.Contains(p.Text, "&lt;!channel&gt;") {
		t.Errorf("mention not escaped: %q", p.Text)
	}
	if !strings.HasPrefix(p.Text, "*Switchyard: failed · my repo*\n") || !strings.Contains(p.Text, "& more"[:0]+"&amp; more") {
		t.Errorf("text = %q", p.Text)
	}
	if strings.Contains(p.Text, "\x00") || strings.Contains(p.Text, "\r") || !strings.Contains(p.Text, "linetwo") {
		t.Errorf("control characters kept: %q", p.Text)
	}
	if !strings.HasSuffix(p.Text, "\nhttps://github.com/o/r/pull/7") {
		t.Errorf("no link: %q", p.Text)
	}
}

func TestDiscordPayload(t *testing.T) {
	m := hostile
	m.Body = strings.Repeat("long ", 1000) + hostile.Body
	_, body := capture(t, Webhook{URL: "https://discord.com/api/webhooks/1/abc"}, m)
	var p struct {
		Content         string
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("mentions not disabled: %s", body)
	}
	if n := utf8.RuneCountInString(p.Content); n > 2000 {
		t.Errorf("content has %d characters; Discord takes 2000", n)
	}
	_, body = capture(t, Webhook{URL: "https://discord.com/api/webhooks/1/abc"}, hostile)
	json.Unmarshal([]byte(body), &p)
	for _, want := range []string{`**Switchyard\: failed · my repo**`, `\[click\]\(https\:`, `\@everyone`, `\*bold\*`} {
		if !strings.Contains(p.Content, want) {
			t.Errorf("content missing %q: %q", want, p.Content)
		}
	}
}

func TestNtfyPayload(t *testing.T) {
	setEnv(t, map[string]string{"NTFY_TOKEN": "tk_secret"})
	m := hostile
	m.Title = "Switchyard: failed – Grüße"
	req, body := capture(t, Webhook{URL: "https://ntfy.sh/sy-topic", Token: "${NTFY_TOKEN}"}, m)
	if !strings.HasPrefix(body, "<!channel> @everyone") || !strings.Contains(body, "linetwo") {
		t.Errorf("body = %q", body)
	}
	title, err := new(mime.WordDecoder).DecodeHeader(req.Header.Get("Title"))
	if err != nil || title != "Switchyard: failed – Grüße · my repo" {
		t.Errorf("title %q (%v), raw %q", title, err, req.Header.Get("Title"))
	}
	for k, want := range map[string]string{
		"Priority": "high", "Tags": "x", "Click": "https://github.com/o/r/pull/7",
		"Authorization": "Bearer tk_secret", "Content-Type": "text/plain; charset=utf-8",
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	req, _ = capture(t, Webhook{URL: "https://ntfy.sh/sy-topic"}, Message{Event: EventDone, Title: "t", Link: "javascript:alert(1)"})
	if req.Header.Get("Priority") != "" || req.Header.Get("Click") != "" || req.Header.Get("Authorization") != "" {
		t.Errorf("headers = %v", req.Header)
	}
}

// A link with non-ASCII in its query reached ntfy.sh as raw UTF-8 in the
// Click header (found against the real server); headers must be ASCII.
func TestNtfyClickIsASCII(t *testing.T) {
	m := Message{Event: EventWatch, Title: "t", Link: "https://github.com/o/r/pull/1?a=b&c=ä x"}
	req, _ := capture(t, Webhook{URL: "https://ntfy.sh/sy-topic"}, m)
	if got, want := req.Header.Get("Click"), "https://github.com/o/r/pull/1?a=b&c=%C3%A4%20x"; got != want {
		t.Errorf("Click = %q, want %q", got, want)
	}
}

func TestJSONPayload(t *testing.T) {
	req, body := capture(t, Webhook{URL: "https://example.org/hook", Kind: "json", Token: "abc"}, hostile)
	var p map[string]string
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if p["event"] != "failed" || p["source"] != "my repo" || p["link"] != hostile.Link || !strings.HasPrefix(p["body"], "<!channel>") {
		t.Errorf("payload = %v", p)
	}
	if req.Header.Get("Authorization") != "Bearer abc" {
		t.Errorf("auth %q", req.Header.Get("Authorization"))
	}
}

func TestWebhookEnvMissing(t *testing.T) {
	setEnv(t, map[string]string{"SET": "https://ntfy.sh/x"})
	if _, err := (Webhook{URL: "${UNSET_HOOK}"}).request(context.Background(), hostile); err == nil || !strings.Contains(err.Error(), "UNSET_HOOK is not set") {
		t.Errorf("got %v", err)
	}
	if _, err := (Webhook{URL: "${SET}", Token: "${NO_TOKEN}"}).request(context.Background(), hostile); err == nil || !strings.Contains(err.Error(), "NO_TOKEN (token) is not set") {
		t.Errorf("got %v", err)
	}
	if _, err := (Webhook{URL: "${SET}"}).request(context.Background(), hostile); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestPostAgainstServer(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+" "+r.Header.Get("Title")+" "+string(b))
		mu.Unlock()
		switch r.URL.Path {
		case "/ok-secret-topic":
			w.WriteHeader(http.StatusOK)
		case "/moved-secret":
			http.Redirect(w, r, "/ok-secret-topic", http.StatusFound)
		default:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":40301,"error":"forbidden: topic   reserved"}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	ok := Webhook{URL: srv.URL + "/ok-secret-topic", Kind: KindNtfy}
	if err := Post(ctx, ok, Message{Event: EventDone, Title: "done", Body: "all good"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if len(got) != 1 || got[0] != "/ok-secret-topic done all good" {
		t.Errorf("server got %q", got)
	}

	err := Post(ctx, Webhook{URL: srv.URL + "/denied-secret-topic", Kind: KindNtfy}, Message{Event: EventDone, Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") || !strings.Contains(err.Error(), "topic reserved") {
		t.Errorf("got %v", err)
	}
	// A redirect is not followed (the POST would turn into a GET).
	err = Post(ctx, Webhook{URL: srv.URL + "/moved-secret", Kind: KindNtfy}, Message{Event: EventDone, Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("redirect: %v", err)
	}
	// A dead server: the error names neither the URL nor its path.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	err = Post(ctx, Webhook{URL: deadURL + "/dead-secret-topic", Kind: KindNtfy}, Message{Event: EventDone, Title: "x"})
	if err == nil {
		t.Fatal("dead server: no error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), deadURL) {
		t.Errorf("error leaks the URL: %v", err)
	}

	// Broadcast: numbered errors, only the hooks that want the event.
	hooks := []Webhook{
		ok,
		{URL: srv.URL + "/denied-secret-topic", Kind: KindNtfy},
		{URL: srv.URL + "/ok-secret-topic", Kind: KindNtfy, Events: []string{EventWatch}},
	}
	mu.Lock()
	got = nil
	mu.Unlock()
	err = Broadcast(ctx, hooks, Message{Event: EventFailed, Title: "f"})
	if err == nil || !strings.Contains(err.Error(), "webhook 2 (ntfy 127.0.0.1:") || strings.Contains(err.Error(), "webhook 1") || strings.Contains(err.Error(), "secret") {
		t.Errorf("broadcast: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("broadcast posted %d times, want 2 (the watch-only hook skips failed): %q", len(got), got)
	}

	// Sender: Wait returns after the posts, errors go to OnError.
	var errs []error
	s := Sender{OnError: func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }}
	mu.Lock()
	got = nil
	mu.Unlock()
	s.Send(hooks, Message{Event: EventWatch, Title: "w"})
	s.Send(hooks[:1], Message{Event: EventDone, Title: "d"})
	s.Send(hooks[2:], Message{Event: EventDone, Title: "skipped"}) // nobody wants it
	s.Wait()
	if len(got) != 4 || len(errs) != 1 {
		t.Errorf("sender: %d posts %q, errors %v", len(got), got, errs)
	}
}

func TestWebhookRedactedAndName(t *testing.T) {
	setEnv(t, map[string]string{"HOOK": "https://ntfy.sh/topic-from-env"})
	w := Webhook{URL: "https://hooks.slack.com/services/T/B/secret", Token: "tok"}.Redacted()
	if w.URL != "https://hooks.slack.com/<hidden>" || w.Token != "<hidden>" {
		t.Errorf("redacted = %+v", w)
	}
	w = Webhook{URL: "${HOOK}", Token: "${TOKEN}"}.Redacted()
	if w.URL != "${HOOK}" || w.Token != "${TOKEN}" {
		t.Errorf("env refs should stay: %+v", w)
	}
	if n := (Webhook{URL: "https://hooks.slack.com/services/T/B/secret"}).Name(); n != "slack hooks.slack.com" {
		t.Errorf("name %q", n)
	}
	if n := (Webhook{URL: "${HOOK}"}).Name(); n != "ntfy ntfy.sh" {
		t.Errorf("env name %q", n)
	}
	if n := (Webhook{URL: "${MISSING}"}).Name(); n != "${MISSING}" {
		t.Errorf("missing env name %q", n)
	}
}
