package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealNtfyPayload posts every event to a real ntfy server and reads
// the messages back, checking that title, body, priority, tags and the
// click link arrive exactly as meant, with non-ASCII text intact. Opt-in:
//
//	RW_REAL_NTFY_URL=https://ntfy.sh/<long random topic> go test ./internal/notify -run RealNtfy
func TestRealNtfyPayload(t *testing.T) {
	topic := os.Getenv("RW_REAL_NTFY_URL")
	if topic == "" {
		t.Skip("set RW_REAL_NTFY_URL to a ntfy topic URL to post real notifications")
	}
	marker := fmt.Sprintf("m%d", time.Now().UnixNano())
	since := time.Now().Add(-2 * time.Second).Unix()
	w := Webhook{URL: topic}
	type want struct {
		title, message, tag, click string
		priority                   int
	}
	hostile := "@everyone <!channel> [click](https://example.com/x) **b** `c` Grüße 日本 🚀 \"q\" & <tag>"
	cases := []struct {
		m Message
		w want
	}{
		{Message{Event: EventDone, Title: "Relayweft: done", Body: "ok " + marker + "\n" + hostile, Source: "projekt-ä"},
			want{"Relayweft: done · projekt-ä", "ok " + marker + "\n" + hostile, "white_check_mark", "", 0}},
		{Message{Event: EventFailed, Title: "Relayweft: failed " + hostile, Body: "fail " + marker, Source: "日本"},
			want{"Relayweft: failed " + hostile + " · 日本", "fail " + marker, "x", "", 4}},
		{Message{Event: EventWaiting, Title: "Relayweft: waiting", Body: "wait " + marker},
			want{"Relayweft: waiting", "wait " + marker, "bell", "", 4}},
		{Message{Event: EventWatch, Title: "Relayweft: watch", Body: "watch " + marker, Link: "https://github.com/sparkz400/relayweft/pull/1?a=b&c=ä"},
			want{"Relayweft: watch", "watch " + marker, "eyes", "https://github.com/sparkz400/relayweft/pull/1?a=b&c=%C3%A4", 0}},
		{Message{Event: EventTest, Title: "Relayweft: test", Body: "test " + marker, Link: "javascript:alert(1)"},
			want{"Relayweft: test", "test " + marker, "wave", "", 0}},
		{Message{Event: EventLimit, Title: "Relayweft: codex hit its limit", Body: "limit " + marker + " " + strings.Repeat("ü", 2000)},
			want{"Relayweft: codex hit its limit", "limit " + marker + " " + strings.Repeat("ü", maxBody-len([]rune("limit "+marker+" "))-1) + "…", "hourglass", "", 0}},
	}
	for _, c := range cases {
		if err := Post(context.Background(), w, c.m); err != nil {
			t.Fatalf("%s: %v", c.m.Event, err)
		}
	}

	type ntfyMsg struct {
		Event, Title, Message, Click string
		Priority                     int
		Tags                         []string
	}
	got := map[string]ntfyMsg{}
	// ntfy writes its message cache in batches: poll until all are there.
	for try := 0; try < 10 && len(got) < len(cases); try++ {
		time.Sleep(time.Second)
		resp, err := http.Get(fmt.Sprintf("%s/json?poll=1&since=%d", topic, since))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m ntfyMsg
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Event != "message" || !strings.Contains(m.Message, marker) {
				continue
			}
			got[strings.Fields(m.Message)[0]] = m
		}
		resp.Body.Close()
	}
	for _, c := range cases {
		key := strings.Fields(c.w.message)[0]
		m, ok := got[key]
		if !ok {
			t.Errorf("%s: not found on the topic", c.m.Event)
			continue
		}
		if m.Title != c.w.title {
			t.Errorf("%s: title\n got %q\nwant %q", c.m.Event, m.Title, c.w.title)
		}
		if m.Message != c.w.message {
			t.Errorf("%s: message\n got %q\nwant %q", c.m.Event, m.Message, c.w.message)
		}
		if len(m.Tags) != 1 || m.Tags[0] != c.w.tag {
			t.Errorf("%s: tags %v, want [%s]", c.m.Event, m.Tags, c.w.tag)
		}
		if m.Click != c.w.click {
			t.Errorf("%s: click %q, want %q", c.m.Event, m.Click, c.w.click)
		}
		if m.Priority != c.w.priority {
			t.Errorf("%s: priority %d, want %d", c.m.Event, m.Priority, c.w.priority)
		}
	}
}
