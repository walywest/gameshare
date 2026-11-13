package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperService isn't a test: the supervisor runs the test binary as a fake
// service, which serves /health and /busy on HELPER_PORT, or crashes at once.
func TestHelperService(t *testing.T) {
	if os.Getenv("HELPER_SERVICE") != "1" {
		t.Skip("helper process")
	}
	if os.Getenv("HELPER_CRASH") == "1" {
		os.Exit(3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/busy", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"paths":["FitFast/Game"]}`) })
	mux.HandleFunc("/echo/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.URL.Path, " ", os.Getenv("HELPER_ROOT")) })
	fmt.Println("helper up")
	http.ListenAndServe("127.0.0.1:"+os.Getenv("HELPER_PORT"), mux)
	os.Exit(0)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeManifest(t *testing.T, repo string, services map[string]serviceSpec) {
	t.Helper()
	b, _ := json.Marshal(manifest{Services: services})
	if err := os.WriteFile(filepath.Join(repo, manifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func helperSpec(port int, extra map[string]string) serviceSpec {
	env := map[string]string{"HELPER_SERVICE": "1", "HELPER_PORT": fmt.Sprint(port), "HELPER_ROOT": "${ROOT}"}
	for k, v := range extra {
		env[k] = v
	}
	return serviceSpec{Run: []string{os.Args[0], "-test.run=^TestHelperService$"}, Env: env,
		Port: port, Health: "/health", Busy: "/busy", Startup: 10}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateOf(s *supervisor, name string) (string, int) {
	for _, v := range s.views() {
		if v.Name == name {
			return v.State, v.PID
		}
	}
	return "", 0
}

func TestSupervisor(t *testing.T) {
	repo, served := t.TempDir(), t.TempDir()
	port := freePort(t)
	writeManifest(t, repo, map[string]serviceSpec{"web": helperSpec(port, nil)})
	s := newSupervisor(repo, served)
	sup = s
	defer func() { s.stopAll(); sup = nil }()

	if ch := s.reconcile(); len(ch) != 1 || ch[0].Action != "started" {
		t.Fatalf("first reconcile: %+v", ch)
	}
	waitFor(t, "healthy", func() bool { s.reconcile(); st, _ := stateOf(s, "web"); return st == "running" })
	_, pid := stateOf(s, "web")

	// Nothing changed: nothing happens.
	if ch := s.reconcile(); len(ch) != 0 {
		t.Fatalf("steady state changed things: %+v", ch)
	}

	// The proxy reaches it, with the path rewritten and ${ROOT} expanded.
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/svc/{name}/", handleProxy)
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/svc/web/echo/x", nil))
	if body, _ := io.ReadAll(w.Body); w.Code != 200 || string(body) != "/echo/x "+served {
		t.Fatalf("proxy: %d %q", w.Code, body)
	}
	if got := s.busyPaths(); len(got) != 1 || got[0] != "FitFast/Game" {
		t.Fatalf("busy: %v", got)
	}

	// A broken manifest keeps the last good one running.
	os.WriteFile(filepath.Join(repo, manifestName), []byte("{nope"), 0o644)
	s.reconcile()
	if st, p := stateOf(s, "web"); st != "running" || p != pid {
		t.Fatalf("broken manifest disturbed the service: %s %d", st, p)
	}
	s.mu.Lock()
	mErr := s.mErr
	s.mu.Unlock()
	if mErr == "" {
		t.Fatal("broken manifest not reported")
	}

	// A changed spec restarts it.
	writeManifest(t, repo, map[string]serviceSpec{"web": helperSpec(port, map[string]string{"X": "2"})})
	if ch := s.reconcile(); len(ch) != 1 || ch[0].Action != "restarted" {
		t.Fatalf("spec change: %+v", ch)
	}
	waitFor(t, "healthy again", func() bool { s.reconcile(); st, _ := stateOf(s, "web"); return st == "running" })
	if _, p := stateOf(s, "web"); p == pid {
		t.Fatal("same pid after a restart")
	}

	// Killed from outside: brought back.
	_, pid = stateOf(s, "web")
	p, _ := os.FindProcess(pid)
	p.Kill()
	waitFor(t, "restart after a kill", func() bool {
		s.reconcile()
		st, np := stateOf(s, "web")
		return st == "running" && np != 0 && np != pid
	})

	// Removed from the manifest: stopped, and the proxy says so.
	writeManifest(t, repo, map[string]serviceSpec{})
	if ch := s.reconcile(); len(ch) != 1 || ch[0].Action != "stopped" {
		t.Fatalf("removal: %+v", ch)
	}
	if portBusy(port) {
		t.Fatal("port still open after stop")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/svc/web/health", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("proxy to removed service: %d", w.Code)
	}
}

func TestSupervisorCrashLoop(t *testing.T) {
	repo, served := t.TempDir(), t.TempDir()
	port := freePort(t)
	writeManifest(t, repo, map[string]serviceSpec{"bad": helperSpec(port, map[string]string{"HELPER_CRASH": "1"})})
	s := newSupervisor(repo, served)
	sup = s
	defer func() { s.stopAll(); sup = nil }()

	s.reconcile()
	waitFor(t, "exit", func() bool { st, _ := stateOf(s, "bad"); return st == "exited" })
	// Backoff: an immediate reconcile doesn't start it again.
	s.reconcile()
	if st, _ := stateOf(s, "bad"); st != "exited" {
		t.Fatalf("restarted before its backoff: %s", st)
	}
	waitFor(t, "crashloop", func() bool { s.reconcile(); st, _ := stateOf(s, "bad"); return st == "crashloop" })
	v := s.views()[0]
	if !strings.Contains(v.LastErr, "exit status 3") || v.Retry == "" {
		t.Fatalf("crash not reported: %+v", v)
	}

	// The proxy answers 503 with the state while it's down.
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/svc/{name}/", handleProxy)
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/svc/bad/x", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "not running") {
		t.Fatalf("proxy to a down service: %d %s", w.Code, w.Body)
	}
}

func TestDeleteRefusesBusyServicePaths(t *testing.T) {
	root = t.TempDir()
	partDir = filepath.Join(root, ".uploads")
	os.MkdirAll(filepath.Join(root, "FitFast", "Game"), 0o755)
	os.MkdirAll(filepath.Join(root, "FitFast", "Other"), 0o755)
	os.WriteFile(filepath.Join(root, "FitFast", "Game", "a.rar"), []byte("x"), 0o644)
	port := freePort(t)
	repo := t.TempDir()
	writeManifest(t, repo, map[string]serviceSpec{"ff": helperSpec(port, nil)})
	s := newSupervisor(repo, root)
	sup = s
	defer func() { s.stopAll(); sup = nil }()
	waitFor(t, "healthy", func() bool { s.reconcile(); st, _ := stateOf(s, "ff"); return st == "running" })

	for _, p := range []string{"FitFast/Game", "FitFast/Game/a.rar", "FitFast"} {
		if c := deleteReq(p); c != http.StatusConflict {
			t.Errorf("%s: got %d, want 409", p, c)
		}
	}
	// A folder still downloading can't be zipped half-done or have its archives cleared.
	for _, p := range []string{"FitFast/Game", "FitFast"} {
		if c := zipReq(p).Code; c != http.StatusConflict {
			t.Errorf("zip %s: got %d, want 409", p, c)
		}
		if c := archivesReq(p).Code; c != http.StatusConflict {
			t.Errorf("archives %s: got %d, want 409", p, c)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "FitFast", "Game", "a.rar")); err != nil {
		t.Error("a busy folder's archive was deleted")
	}
	if c := deleteReq("FitFast/Other"); c != http.StatusOK {
		t.Errorf("unrelated folder: got %d", c)
	}
}

func TestManifestValidation(t *testing.T) {
	repo := t.TempDir()
	if m, err := loadManifest(repo); err != nil || len(m.Services) != 0 {
		t.Fatalf("missing manifest: %v %v", m, err)
	}
	for _, body := range []string{
		`{"services":{"Bad Name":{"run":["x"]}}}`,
		`{"services":{"a":{}}}`,
		`{"services":{"a":{"run":["x"],"dir":"../etc"}}}`,
		`{"services":{"a":{"run":["x"],"typo":1}}}`,
	} {
		os.WriteFile(filepath.Join(repo, manifestName), []byte(body), 0o644)
		if _, err := loadManifest(repo); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	// The real manifest in this repo must parse.
	if _, err := loadManifest(".."); err != nil {
		t.Fatalf("repo's %s: %v", manifestName, err)
	}
}
