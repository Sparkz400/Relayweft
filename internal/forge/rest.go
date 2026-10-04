package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// rest is the HTTP side of the GitLab and Gitea clients: JSON in and out,
// the token in an Authorization header, and reads that go on without a
// token the forge rejected (a stale token must not block a public
// repository).
type rest struct {
	kind     Kind
	base     string
	token    string
	scheme   string // Authorization scheme: Bearer (GitLab), token (Gitea)
	http     *http.Client
	notes    io.Writer
	rejected bool
}

func newRest(kind Kind, base, token, scheme string, notes io.Writer) *rest {
	return &rest{kind: kind, base: strings.TrimRight(base, "/"), token: token, scheme: scheme,
		http: &http.Client{Timeout: 60 * time.Second}, notes: notes}
}

func (c *rest) Kind() Kind     { return c.kind }
func (c *rest) HasToken() bool { return c.token != "" && !c.rejected }
func (c *rest) Rejected() bool { return c.rejected }
func (c *rest) name() string   { return c.kind.Name() }

func (c *rest) note(s string) {
	if c.notes != nil {
		fmt.Fprintln(c.notes, s)
	}
}

// do sends one request; in (if not nil) is sent as JSON and the answer is
// decoded into out (if not nil).
func (c *rest) do(method, path string, in, out any) error {
	resp, err := c.send(method, path, "application/json", in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s %s: bad response: %w", c.name(), method, path, err)
	}
	return nil
}

// pages GETs path page by page until a short page; param names the page
// size parameter (per_page on GitLab, limit on Gitea).
func pages[T any](c *rest, path, param string, per int) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; page <= maxPages; page++ {
		var xs []T
		if err := c.do(http.MethodGet, fmt.Sprintf("%s%s%s=%d&page=%d", path, sep, param, per, page), nil, &xs); err != nil {
			return nil, err
		}
		all = append(all, xs...)
		if len(xs) < per {
			break
		}
	}
	return all, nil
}

// text GETs path and returns the body, at most max bytes (ErrTooLarge
// beyond that).
func (c *rest) text(path string, max int64) (string, error) {
	resp, err := c.send(http.MethodGet, path, "text/plain", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", fmt.Errorf("%s GET %s: %w", c.name(), path, err)
	}
	if int64(len(data)) > max {
		return "", fmt.Errorf("%s GET %s: %w (over %d bytes)", c.name(), path, ErrTooLarge, max)
	}
	return string(data), nil
}

// maxLogRead caps how much of a job log is read to find its tail.
const maxLogRead = 64 << 20

// tailOf GETs path and keeps the last max bytes (lineTail).
func (c *rest) tailOf(path string, max int) (string, error) {
	resp, err := c.send(http.MethodGet, path, "text/plain", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	t := &tail{max: max + 1} // one more: was the first line cut?
	if _, err := io.Copy(t, io.LimitReader(resp.Body, maxLogRead)); err != nil {
		return "", fmt.Errorf("%s GET %s: %w", c.name(), path, err)
	}
	return lineTail(string(t.buf), max), nil
}

// lineTail keeps the last n bytes of s. When that cuts s, it starts at the
// first whole line after the cut; a log that fits keeps its first line.
func lineTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// From the byte before the kept part: a newline there means the kept
	// part starts a line.
	cut := s[len(s)-n-1:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		return cut[i+1:]
	}
	return s[len(s)-n:] // one line longer than n: keep its end
}

// tail keeps the last max bytes written to it.
type tail struct {
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0:0], t.buf[over:]...)
	}
	return len(p), nil
}

// send sends one request and returns a 2xx response (the caller closes
// its body); any other status is an *APIError.
func (c *rest) send(method, path, accept string, in any) (*http.Response, error) {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = b
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "switchyard")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	authed := c.HasToken()
	if authed {
		req.Header.Set("Authorization", c.scheme+" "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s %s: %w", c.name(), method, path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized && authed && method == http.MethodGet {
		resp.Body.Close()
		c.note("note: " + c.name() + " rejected the token (401); continuing without it")
		c.rejected = true
		return c.send(method, path, accept, in)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ae := &APIError{Forge: c.kind, Status: resp.StatusCode, Method: method, Path: path, Message: restMessage(data)}
		hint := c.kind.TokenHint()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			if authed || c.rejected {
				ae.Hint = "The token (" + hint + ") was rejected; create a new one"
			} else {
				ae.Hint = "This needs a token: " + hint
			}
		case http.StatusNotFound:
			if authed {
				ae.Hint = "The token may lack access to this repository"
			} else {
				ae.Hint = "If the repository is private, set a token (" + hint + ")"
			}
		case http.StatusForbidden:
			ae.Hint = "The token may lack the needed permission (api scope on GitLab; repository and issue write on Gitea)"
		}
		return nil, ae
	}
	return resp, nil
}

// restMessage reads an error body: GitLab's {"message": "..."},
// {"message": {"field": ["..."]}} or {"error": "..."}, Gitea's
// {"message": "...", "errors": [...]}.
func restMessage(data []byte) string {
	var e struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
		Errors  []string        `json:"errors"`
	}
	if json.Unmarshal(data, &e) != nil {
		return strings.TrimSpace(string(data))
	}
	var parts []string
	var s string
	var fields map[string][]string
	var list []string
	switch {
	case json.Unmarshal(e.Message, &s) == nil:
		parts = append(parts, s)
	case json.Unmarshal(e.Message, &list) == nil:
		parts = append(parts, list...)
	case json.Unmarshal(e.Message, &fields) == nil:
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k+" "+strings.Join(fields[k], ", "))
		}
	}
	if e.Error != "" {
		parts = append(parts, e.Error)
	}
	parts = append(parts, e.Errors...)
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}
