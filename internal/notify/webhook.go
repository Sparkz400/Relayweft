package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Webhook events: what a notification is about. A webhook gets every
// event unless its events list names some.
const (
	EventDone    = "done"    // a task finished ok
	EventFailed  = "failed"  // a task failed
	EventLimit   = "limit"   // a provider hit its usage limit
	EventWaiting = "waiting" // rw waits for you (an approval, a budget question)
	EventWatch   = "watch"   // rw watch ran a follow-up round, or a watched PR was merged or closed
	EventTest    = "test"    // rw notify --test (always sent)
)

// Events lists the events a webhook can choose.
var Events = []string{EventDone, EventFailed, EventLimit, EventWaiting, EventWatch}

// Webhook kinds: the payload format.
const (
	KindSlack   = "slack"   // Slack incoming webhook
	KindDiscord = "discord" // Discord channel webhook
	KindNtfy    = "ntfy"    // ntfy.sh or a self-hosted ntfy topic URL
	KindJSON    = "json"    // a plain JSON POST, for anything else
)

// Kinds lists the webhook kinds.
var Kinds = []string{KindSlack, KindDiscord, KindNtfy, KindJSON}

// Webhook is one notify.webhooks entry. URL and Token may use ${VAR}: they
// are filled in from the environment when a message is sent, so the
// secret does not have to be in the config file.
type Webhook struct {
	URL    string   `yaml:"url" json:"url"`                                // the webhook; ${VAR} is read from your environment
	Kind   string   `yaml:"kind,omitempty" json:"kind,omitempty"`          // "" = from the URL's host
	Token  string   `yaml:"token,omitempty" json:"token,omitempty"`        // ntfy access token, or json's bearer token
	Events []string `yaml:"events,omitempty,flow" json:"events,omitempty"` // done, failed, limit, waiting, watch (default: all)
}

// Message is one notification.
type Message struct {
	Event  string
	Title  string // e.g. "Relayweft: done"
	Body   string
	Source string // the project folder's name ("" = none)
	Link   string // a page to open (a pull request), optional
}

// Wants reports whether the webhook takes this event.
func (w Webhook) Wants(event string) bool {
	if event == EventTest || len(w.Events) == 0 {
		return true
	}
	for _, e := range w.Events {
		if strings.EqualFold(e, event) {
			return true
		}
	}
	return false
}

// Validate checks what can be checked before ${VAR}s are filled in.
func (w Webhook) Validate() error {
	if strings.TrimSpace(w.URL) == "" {
		return errors.New("url is empty")
	}
	if w.Kind != "" && !contains(Kinds, strings.ToLower(w.Kind)) {
		return fmt.Errorf("kind must be one of %s, got %q", strings.Join(Kinds, ", "), w.Kind)
	}
	for _, e := range w.Events {
		if !contains(Events, strings.ToLower(e)) {
			return fmt.Errorf("event %q: want %s", e, strings.Join(Events, ", "))
		}
	}
	if envRef.MatchString(w.URL) {
		return nil // checked when sent
	}
	if _, err := parseURL(w.URL); err != nil {
		return err
	}
	if w.kind(w.URL) == "" {
		return errors.New("set kind (slack, discord, ntfy or json): it cannot be told from the URL")
	}
	return nil
}

// Name is how errors and rw notify refer to the webhook: its kind and
// host, never the path (Slack and Discord URLs are secrets, so is an ntfy
// topic).
func (w Webhook) Name() string {
	raw, _ := expand(w.URL)
	host := "?"
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Host != "" {
		host = u.Host
	} else if envRef.MatchString(w.URL) {
		host = w.URL
	}
	if k := w.kind(raw); k != "" {
		return k + " " + host
	}
	return host
}

// Redacted returns a copy for display (rw bugreport): the URL keeps its
// scheme and host, the token is hidden; ${VAR} references are kept.
func (w Webhook) Redacted() Webhook {
	if strings.TrimSpace(envRef.ReplaceAllString(w.URL, "")) != "" {
		if u, err := url.Parse(w.URL); err == nil && u.Host != "" {
			w.URL = u.Scheme + "://" + u.Host + "/<hidden>"
		} else {
			w.URL = "<hidden>"
		}
	}
	if strings.TrimSpace(envRef.ReplaceAllString(w.Token, "")) != "" {
		w.Token = "<hidden>"
	}
	return w
}

// kind is the configured kind, or the one the URL's host tells.
func (w Webhook) kind(rawURL string) string {
	if w.Kind != "" {
		return strings.ToLower(w.Kind)
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "hooks.slack.com":
		return KindSlack
	case (host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com")) && strings.HasPrefix(u.Path, "/api/webhooks/"):
		return KindDiscord
	case host == "ntfy.sh" || strings.HasPrefix(host, "ntfy."):
		return KindNtfy
	}
	return ""
}

func parseURL(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("url is not a valid URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("url must start with https:// (or http:// for a server on your network)")
	}
	return u, nil
}

// envRef is a ${VAR} reference, as in the MCP settings.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// lookupEnv is os.LookupEnv; tests replace it.
var lookupEnv = os.LookupEnv

// expand fills in ${VAR}s and returns the names that are not set.
func expand(s string) (string, []string) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		v, ok := lookupEnv(m[2 : len(m)-1])
		if !ok || v == "" {
			missing = append(missing, m[2:len(m)-1])
		}
		return v
	})
	return out, missing
}

// webhookTimeout bounds one post: a dead server must not hold up the end
// of an overnight run for long.
const webhookTimeout = 10 * time.Second

// httpClient posts webhooks. It does not follow redirects: a redirected
// POST turns into a GET, and the token must not travel on.
var httpClient = &http.Client{
	Timeout:       webhookTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// maxBody caps the text sent: chat messages have limits (Discord: 2000
// characters) and nobody reads more on a phone.
const maxBody = 1500

// Post sends m to w. Its errors never contain the URL or the token.
func Post(ctx context.Context, w Webhook, m Message) error {
	req, err := w.request(ctx, m)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// *url.Error repeats the URL: keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drain for connection reuse
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	msg := strings.Join(strings.Fields(strings.ToValidUTF8(string(b), "")), " ")
	if msg != "" {
		return fmt.Errorf("%s: %s", resp.Status, clip(msg, 200))
	}
	return errors.New(resp.Status)
}

// request builds the HTTP request for w's kind.
func (w Webhook) request(ctx context.Context, m Message) (*http.Request, error) {
	raw, missing := expand(w.URL)
	if len(missing) > 0 {
		return nil, fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	u, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	token, missing := expand(w.Token)
	if len(missing) > 0 {
		return nil, fmt.Errorf("environment variable %s (token) is not set", strings.Join(missing, ", "))
	}
	title := cleanLine(m.Title)
	if s := cleanLine(m.Source); s != "" {
		title += " · " + s
	}
	body := clip(cleanText(m.Body), maxBody)
	link := ""
	if l, err := url.Parse(strings.TrimSpace(m.Link)); err == nil && (l.Scheme == "https" || l.Scheme == "http") && l.Host != "" {
		link = asciiURL(l.String())
	}

	var payload []byte
	contentType := "application/json"
	header := http.Header{}
	switch k := w.kind(raw); k {
	case KindSlack:
		text := "*" + slackEscape(title) + "*"
		if body != "" {
			text += "\n" + slackEscape(body)
		}
		if link != "" {
			text += "\n" + slackEscape(link)
		}
		payload, _ = json.Marshal(map[string]any{"text": text})
	case KindDiscord:
		text := "**" + discordEscape(title) + "**"
		if body != "" {
			text += "\n" + discordEscape(body)
		}
		if link != "" {
			text += "\n" + link
		}
		payload, _ = json.Marshal(map[string]any{
			"content": clip(text, 2000),
			// Never ping: a task summary may contain @everyone.
			"allowed_mentions": map[string]any{"parse": []string{}},
		})
	case KindNtfy:
		payload, contentType = []byte(body), "text/plain; charset=utf-8"
		if body == "" {
			payload = []byte(title)
		}
		// ntfy reads RFC 2047 encoded headers, so titles need not be ASCII.
		header.Set("Title", mime.BEncoding.Encode("UTF-8", title))
		header.Set("Tags", ntfyTags[m.Event])
		if m.Event == EventFailed || m.Event == EventWaiting {
			header.Set("Priority", "high")
		}
		if link != "" {
			header.Set("Click", link)
		}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
	case KindJSON:
		payload, _ = json.Marshal(map[string]string{
			"event": m.Event, "title": title, "body": body, "source": cleanLine(m.Source), "link": link,
		})
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
	default:
		return nil, errors.New("set kind (slack, discord, ntfy or json): it cannot be told from the URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("url is not a valid URL")
	}
	for k, v := range header {
		if v[0] != "" {
			req.Header[k] = v
		}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "relayweft")
	return req, nil
}

// ntfyTags are ntfy's emoji short codes per event.
var ntfyTags = map[string]string{
	EventDone:    "white_check_mark",
	EventFailed:  "x",
	EventLimit:   "hourglass",
	EventWaiting: "bell",
	EventWatch:   "eyes",
	EventTest:    "wave",
}

// Broadcast posts m to every webhook that wants its event, in parallel,
// and waits. Each failure is reported with the webhook's number and Name.
func Broadcast(ctx context.Context, hooks []Webhook, m Message) error {
	errs := make([]error, len(hooks))
	var wg sync.WaitGroup
	for i, w := range hooks {
		if !w.Wants(m.Event) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Post(ctx, w, m); err != nil {
				errs[i] = fmt.Errorf("webhook %d (%s): %w", i+1, w.Name(), err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Wanted reports whether any webhook takes the event.
func Wanted(hooks []Webhook, event string) bool {
	for _, w := range hooks {
		if w.Wants(event) {
			return true
		}
	}
	return false
}

// Sender posts messages in the background. Wait blocks until every post
// has finished (each is bounded by a 10 s timeout), so a command can send
// its last message and still exit promptly.
type Sender struct {
	OnError func(error) // called from the posting goroutine; may be nil
	wg      sync.WaitGroup
}

// Send posts m to the webhooks that want it without blocking.
func (s *Sender) Send(hooks []Webhook, m Message) {
	if !Wanted(hooks, m.Event) {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := Broadcast(context.Background(), hooks, m); err != nil && s.OnError != nil {
			s.OnError(err)
		}
	}()
}

// Wait blocks until every message sent so far has been posted or failed.
func (s *Sender) Wait() { s.wg.Wait() }

// slackEscape escapes Slack's control characters, which also keeps
// <!channel> mentions and <url|text> links out of task text.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// discordEscape backslash-escapes Discord's markdown, so task text shows
// as typed and cannot hide a link behind other words.
func discordEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\*_~`|>[]()#-<:@", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cleanText drops control characters except newlines and tabs, and
// invalid UTF-8.
func cleanText(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\r\n", "\n")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s))
}

// cleanLine is cleanText on one line.
func cleanLine(s string) string { return strings.Join(strings.Fields(cleanText(s)), " ") }

// asciiURL percent-encodes every byte of s outside printable ASCII.
// url.URL.String keeps a raw query as given, so a link could carry UTF-8
// into ntfy's Click header, where HTTP allows only ASCII.
func asciiURL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// clip shortens s to n runes, ending in "…".
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
