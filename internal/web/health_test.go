package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/health"
)

func TestHealthEndpoint(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05.000")
	os.WriteFile(filepath.Join(dir, "rw-health.log"), []byte(fmt.Sprintf(
		"%s start pid=999999001 ver=dev cmd=run\n%s panic pid=999999001 in=task log=crash-x.log\n", at, at)), 0o644)
	healthOptions = health.Options{Dir: dir, NoLeftovers: true}
	defer func() { healthOptions = health.Options{} }()
	env := newEnv(t, nil)
	if res, _ := env.do("GET", "/api/health", nil, map[string]string{SessionHeader: ""}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d", res.StatusCode)
	}
	var r health.Report
	env.call("GET", "/api/health?days=7", nil, &r)
	if r.Days != 7 || len(r.Crashes) != 1 || r.Crashes[0].Cmd != "run" || r.Criterion.Met {
		t.Fatalf("report = %+v", r)
	}
}
