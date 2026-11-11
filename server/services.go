package main

// Homelab services. homelab.json at the repo root declares which services run and
// how; this file keeps reality in line with it, the way a Kubernetes controller
// does: start what's missing, restart what changed or went unhealthy, stop what
// was removed. A pull that changes a service's files or its manifest entry is
// therefore all it takes to roll it out.
//
// Nothing here may stop the file server: a bad manifest keeps the last good one,
// and a service that won't start just shows as failing on the page.

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	manifestName   = "homelab.json"
	reconcileEvery = 5 * time.Second
	unhealthyLimit = 30 * time.Second // a running service failing health this long is restarted
	stopGrace      = 15 * time.Second
	minBackoff     = 2 * time.Second
	maxBackoff     = 5 * time.Minute
	stableAfter    = time.Minute // a run this long resets the crash backoff
	logLines       = 500
)

type serviceSpec struct {
	Dir     string            `json:"dir"`               // relative to the repo
	Run     []string          `json:"run"`               // command, run in Dir
	Env     map[string]string `json:"env,omitempty"`     // ${ROOT} and ${REPO} are expanded
	Devenv  bool              `json:"devenv,omitempty"`  // run inside the repo's devenv shell
	Port    int               `json:"port,omitempty"`    // where it listens on 127.0.0.1, for health and /svc/<name>/
	Health  string            `json:"health,omitempty"`  // GET path that answers 200 when healthy
	Busy    string            `json:"busy,omitempty"`    // GET path listing folders under the root it's writing into
	Watch   []string          `json:"watch,omitempty"`   // repo paths whose change restarts it
	Enabled *bool             `json:"enabled,omitempty"` // default true
	Startup int               `json:"startupSeconds,omitempty"`
	Title   string            `json:"title,omitempty"`
}

func (s serviceSpec) enabled() bool { return s.Enabled == nil || *s.Enabled }

// startupWindow is how long a new process may fail its health check. The first
// devenv shell of a service builds its whole environment, which takes minutes.
func (s serviceSpec) startupWindow() time.Duration {
	if s.Startup > 0 {
		return time.Duration(s.Startup) * time.Second
	}
	if s.Devenv {
		return 20 * time.Minute
	}
	return time.Minute
}

type manifest struct {
	Services map[string]serviceSpec `json:"services"`
}

func loadManifest(repo string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(filepath.Join(repo, manifestName))
	if os.IsNotExist(err) {
		return manifest{Services: map[string]serviceSpec{}}, nil
	}
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %v", manifestName, err)
	}
	for name, s := range m.Services {
		switch {
		case !validName(name):
			return m, fmt.Errorf("%s: bad service name %q", manifestName, name)
		case len(s.Run) == 0:
			return m, fmt.Errorf("%s: service %s has no run command", manifestName, name)
		case s.Port < 0 || s.Port > 65535:
			return m, fmt.Errorf("%s: service %s has a bad port", manifestName, name)
		case strings.Contains(s.Dir, ".."):
			return m, fmt.Errorf("%s: service %s: dir must stay inside the repo", manifestName, name)
		}
	}
	return m, nil
}

func validName(n string) bool {
	if n == "" || len(n) > 40 {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ring keeps the last lines a service printed.
type ring struct {
	mu    sync.Mutex
	lines []string
}

func (r *ring) add(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > logLines {
		r.lines = append([]string(nil), r.lines[len(r.lines)-logLines:]...)
	}
	r.mu.Unlock()
}

func (r *ring) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string{}, r.lines[len(r.lines)-n:]...)
}

// service is the actual state of one declared service.
type service struct {
	name string
	spec serviceSpec
	hash string

	cmd    *exec.Cmd
	exited chan struct{} // closed when cmd exits
	state  string        // starting, running, unhealthy, exited, crashloop, failed, stopped
	pid    int
	since  time.Time // when the current process started
	lastOK time.Time // last passing health check of the current process
	badAt  time.Time // when health started failing, while running

	restarts  int
	crashes   int // consecutive short runs
	nextStart time.Time
	lastErr   string
	logs      ring
}

type change struct {
	Name   string `json:"name"`
	Action string `json:"action"` // started, restarted, stopped
	Why    string `json:"why,omitempty"`
}

type supervisor struct {
	repo, root, stateDir string
	devenvBin            string
	devenvFlags          []string // the quiet, non-reloading flags this devenv version knows
	health               *http.Client

	recMu sync.Mutex // one reconcile at a time; it may block while stopping
	mu    sync.Mutex // guards everything below
	svcs  map[string]*service
	want  map[string]serviceSpec // last good desired state
	mErr  string                 // why the manifest on disk isn't the one in use
	kick  chan struct{}
	quit  bool
}

var sup *supervisor // nil when the server has no repo to read services from

func newSupervisor(repo, root string) *supervisor {
	s := &supervisor{
		repo:     repo,
		root:     root,
		stateDir: filepath.Join(root, ".homelab"),
		health:   &http.Client{Timeout: 2 * time.Second},
		svcs:     map[string]*service{},
		want:     map[string]serviceSpec{},
		kick:     make(chan struct{}, 1),
	}
	s.devenvBin = findDevenv()
	if s.devenvBin != "" {
		s.devenvFlags = devenvFlags(s.devenvBin)
	}
	os.MkdirAll(s.stateDir, 0o755)
	return s
}

// devenvFlags picks the flags that keep `devenv shell` from drawing its TUI into
// the service log, from reloading the service by itself, and from running stale
// config. Only those this devenv version lists are used, since an unknown flag
// would stop the service from starting at all.
func devenvFlags(bin string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "shell", "--help").CombinedOutput()
	help := string(out)
	var flags []string
	for _, f := range []string{"--no-tui", "--no-reload", "--refresh-eval-cache"} {
		if strings.Contains(help, f) {
			flags = append(flags, f)
		}
	}
	return flags
}

// findDevenv looks past PATH too, since the server may run under an older
// shell, or none.
func findDevenv() string {
	if p, err := exec.LookPath("devenv"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".nix-profile", "bin", "devenv"),
		"/nix/var/nix/profiles/default/bin/devenv",
		"/run/current-system/sw/bin/devenv",
		filepath.Join("/etc/profiles/per-user", os.Getenv("USER"), "bin", "devenv"),
	} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func (s *supervisor) run() {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		s.reconcile()
		select {
		case <-t.C:
		case <-s.kick:
		}
		s.mu.Lock()
		quit := s.quit
		s.mu.Unlock()
		if quit {
			return
		}
	}
}

func (s *supervisor) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// specHash changes when the manifest entry or any watched path changes in git,
// which is what makes a pull restart exactly the services it touched.
func (s *supervisor) specHash(spec serviceSpec) string {
	b, _ := json.Marshal(spec)
	h := sha1.New()
	h.Write(b)
	watch := spec.Watch
	if len(watch) == 0 && spec.Dir != "" {
		watch = []string{spec.Dir}
	}
	if len(watch) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", s.repo, "ls-tree", "HEAD", "--"}, watch...)...)
		out, err := cmd.Output()
		cancel()
		if err == nil {
			h.Write(out)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// reconcile moves every service one step toward the desired state and reports
// what it started, restarted or stopped.
func (s *supervisor) reconcile() []change {
	s.recMu.Lock()
	defer s.recMu.Unlock()

	m, err := loadManifest(s.repo)
	s.mu.Lock()
	if s.quit {
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		if s.mErr != err.Error() {
			log.Printf("services: %v; keeping the last good manifest", err)
		}
		s.mErr = err.Error()
	} else {
		s.mErr = ""
		s.want = m.Services
	}
	want := s.want
	s.mu.Unlock()

	var changes []change
	// Stop what's no longer wanted.
	s.mu.Lock()
	var gone []*service
	for name, sv := range s.svcs {
		if spec, ok := want[name]; !ok || !spec.enabled() {
			gone = append(gone, sv)
		}
	}
	s.mu.Unlock()
	for _, sv := range gone {
		s.stop(sv)
		s.mu.Lock()
		delete(s.svcs, sv.name)
		s.mu.Unlock()
		changes = append(changes, change{sv.name, "stopped", "removed or disabled in " + manifestName})
		log.Printf("services: stopped %s, no longer wanted", sv.name)
	}

	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := want[name]
		if !spec.enabled() {
			continue
		}
		if c := s.step(name, spec); c != nil {
			changes = append(changes, *c)
		}
	}
	return changes
}

func (s *supervisor) step(name string, spec serviceSpec) *change {
	hash := s.specHash(spec)
	s.mu.Lock()
	sv := s.svcs[name]
	if sv == nil {
		sv = &service{name: name, spec: spec, hash: hash, state: "stopped"}
		s.svcs[name] = sv
		s.mu.Unlock()
		s.start(sv)
		return &change{name, "started", ""}
	}
	if sv.hash != hash {
		s.mu.Unlock()
		s.stop(sv)
		s.mu.Lock()
		sv.spec, sv.hash = spec, hash
		sv.crashes, sv.nextStart = 0, time.Time{}
		sv.restarts++
		s.mu.Unlock()
		s.start(sv)
		log.Printf("services: restarted %s, its spec or files changed", name)
		return &change{name, "restarted", "changed"}
	}
	running := sv.cmd != nil
	due := !running && time.Now().After(sv.nextStart)
	s.mu.Unlock()

	if due {
		s.mu.Lock()
		sv.restarts++
		s.mu.Unlock()
		s.start(sv)
		return nil
	}
	if !running || spec.Port == 0 || spec.Health == "" {
		return nil
	}
	ok := s.check(spec)
	s.mu.Lock()
	now := time.Now()
	restart := false
	switch {
	case ok:
		sv.lastOK, sv.badAt = now, time.Time{}
		sv.state = "running"
	case sv.lastOK.IsZero(): // never came up yet
		if now.Sub(sv.since) > spec.startupWindow() {
			sv.lastErr = fmt.Sprintf("not healthy %s after starting", spec.startupWindow())
			restart = true
		}
	default:
		if sv.badAt.IsZero() {
			sv.badAt = now
		}
		sv.state = "unhealthy"
		if now.Sub(sv.badAt) > unhealthyLimit {
			sv.lastErr = "health check failing for " + unhealthyLimit.String()
			restart = true
		}
	}
	s.mu.Unlock()
	if restart {
		log.Printf("services: restarting %s: %s", name, sv.lastErr)
		s.stop(sv)
		s.mu.Lock()
		sv.restarts++
		s.mu.Unlock()
		s.start(sv)
		return &change{name, "restarted", "unhealthy"}
	}
	return nil
}

func (s *supervisor) check(spec serviceSpec) bool {
	resp, err := s.health.Get(fmt.Sprintf("http://127.0.0.1:%d%s", spec.Port, spec.Health))
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *supervisor) expand(v string) string {
	return os.Expand(v, func(k string) string {
		switch k {
		case "ROOT":
			return s.root
		case "REPO":
			return s.repo
		}
		return "${" + k + "}"
	})
}

// serviceEnv is the server's environment without anything that ties it to the
// shell the server itself was started from, so a nested devenv shell builds the
// environment the repo declares now.
func (s *supervisor) serviceEnv(spec serviceSpec) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "DEVENV_") || strings.HasPrefix(k, "DIRENV_") || strings.HasPrefix(k, "GAMESHARE_") ||
			k == "VIRTUAL_ENV" || k == "IN_NIX_SHELL" || k == "PYTHONPATH" || k == "PYTHONHOME" {
			continue
		}
		env = append(env, kv)
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+s.expand(spec.Env[k]))
	}
	return env
}

func (s *supervisor) command(spec serviceSpec) (*exec.Cmd, error) {
	dir := filepath.Join(s.repo, filepath.FromSlash(spec.Dir))
	argv := make([]string, len(spec.Run))
	for i, a := range spec.Run {
		argv[i] = s.expand(a)
	}
	var cmd *exec.Cmd
	if spec.Devenv {
		if s.devenvBin == "" {
			return nil, errors.New("devenv not found on PATH or in the usual Nix profiles")
		}
		// devenv shell reads devenv.nix from its working directory, so it starts
		// at the repo root and the command then moves into the service's folder.
		args := append([]string{"shell"}, s.devenvFlags...)
		args = append(args, "--", "bash", "-c", `cd "$0" && exec "$@"`, dir)
		cmd = exec.Command(s.devenvBin, append(args, argv...)...)
		cmd.Dir = s.repo
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
	}
	cmd.Env = s.serviceEnv(spec)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	return cmd, nil
}

func (s *supervisor) pidFile(name string) string { return filepath.Join(s.stateDir, name+".pgid") }

// killLeftover ends a process group a previous server left behind, say after a
// kill -9, which would otherwise hold the service's port.
func (s *supervisor) killLeftover(name string) {
	b, err := os.ReadFile(s.pidFile(name))
	if err != nil {
		return
	}
	os.Remove(s.pidFile(name))
	pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pgid <= 1 || pgid == os.Getpid() {
		return
	}
	if syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return
	}
	log.Printf("services: stopping %s left over from a previous run (group %d)", name, pgid)
	for end := time.Now().Add(stopGrace); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}

func (s *supervisor) start(sv *service) {
	s.killLeftover(sv.name)
	s.mu.Lock()
	spec := sv.spec
	s.mu.Unlock()
	fail := func(err error) {
		s.mu.Lock()
		sv.state, sv.lastErr = "failed", err.Error()
		sv.nextStart = time.Now().Add(s.backoff(sv))
		s.mu.Unlock()
		sv.logs.add("homelab: " + err.Error())
		log.Printf("services: %s: %v", sv.name, err)
	}
	if spec.Port > 0 && portBusy(spec.Port) {
		fail(fmt.Errorf("port %d is already in use by something else", spec.Port))
		return
	}
	cmd, err := s.command(spec)
	if err != nil {
		fail(err)
		return
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		pw.Close()
		fail(err)
		return
	}
	os.WriteFile(s.pidFile(sv.name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	exited := make(chan struct{})
	s.mu.Lock()
	sv.cmd, sv.exited = cmd, exited
	sv.pid, sv.since = cmd.Process.Pid, time.Now()
	sv.lastOK, sv.badAt = time.Time{}, time.Time{}
	sv.state = "starting"
	if spec.Port == 0 || spec.Health == "" {
		sv.state = "running"
	}
	s.mu.Unlock()
	log.Printf("services: started %s (pid %d)", sv.name, cmd.Process.Pid)
	sv.logs.add(fmt.Sprintf("homelab: started, pid %d", cmd.Process.Pid))

	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			sv.logs.add(line)
			log.Printf("[%s] %s", sv.name, line)
		}
		io.Copy(io.Discard, pr)
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		s.mu.Lock()
		ran := time.Since(sv.since)
		sv.cmd = nil
		sv.pid = 0
		if sv.state != "stopping" {
			msg := "exited"
			if err != nil {
				msg = err.Error()
			}
			sv.lastErr = "process " + msg
			if ran >= stableAfter {
				sv.crashes = 0
			}
			sv.crashes++
			delay := s.backoff(sv)
			sv.nextStart = time.Now().Add(delay)
			sv.state = "exited"
			if sv.crashes >= 3 {
				sv.state = "crashloop"
			}
			log.Printf("services: %s %s after %s, retrying in %s", sv.name, msg, ran.Round(time.Second), delay)
			sv.logs.add(fmt.Sprintf("homelab: %s, retrying in %s", msg, delay))
		}
		s.mu.Unlock()
		os.Remove(s.pidFile(sv.name))
		close(exited)
		s.poke()
	}()
}

// backoff must be called with s.mu held.
func (s *supervisor) backoff(sv *service) time.Duration {
	d := minBackoff
	for i := 1; i < sv.crashes && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func portBusy(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// stop ends a service's whole process group: SIGTERM, then SIGKILL after the grace.
func (s *supervisor) stop(sv *service) {
	s.mu.Lock()
	cmd, exited := sv.cmd, sv.exited
	if cmd == nil {
		sv.state = "stopped"
		s.mu.Unlock()
		return
	}
	sv.state = "stopping"
	s.mu.Unlock()
	pgid := cmd.Process.Pid
	syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopGrace):
		log.Printf("services: %s didn't stop in %s, killing it", sv.name, stopGrace)
		syscall.Kill(-pgid, syscall.SIGKILL)
		<-exited
	}
	syscall.Kill(-pgid, syscall.SIGKILL) // anything in the group still hanging on
	s.mu.Lock()
	sv.state = "stopped"
	s.mu.Unlock()
	sv.logs.add("homelab: stopped")
}

// stopAll runs before the server exits, so services save their state.
func (s *supervisor) stopAll() {
	s.mu.Lock()
	s.quit = true
	list := make([]*service, 0, len(s.svcs))
	for _, sv := range s.svcs {
		list = append(list, sv)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, sv := range list {
		wg.Add(1)
		go func(sv *service) { defer wg.Done(); s.stop(sv) }(sv)
	}
	wg.Wait()
}

func (s *supervisor) restart(name string) error {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	s.mu.Lock()
	sv := s.svcs[name]
	s.mu.Unlock()
	if sv == nil {
		return fmt.Errorf("no running service %s", name)
	}
	s.stop(sv)
	s.mu.Lock()
	sv.crashes, sv.nextStart = 0, time.Time{}
	sv.restarts++
	s.mu.Unlock()
	s.start(sv)
	return nil
}

type serviceView struct {
	Name     string `json:"name"`
	Title    string `json:"title,omitempty"`
	State    string `json:"state"`
	Healthy  bool   `json:"healthy"`
	PID      int    `json:"pid,omitempty"`
	Since    string `json:"since,omitempty"`
	Restarts int    `json:"restarts"`
	LastErr  string `json:"lastErr,omitempty"`
	Hash     string `json:"hash"`
	Port     int    `json:"port,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
	Devenv   bool   `json:"devenv"`
	Retry    string `json:"retryAt,omitempty"`
}

func (s *supervisor) views() []serviceView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []serviceView{}
	for _, sv := range s.svcs {
		v := serviceView{Name: sv.name, Title: sv.spec.Title, State: sv.state, PID: sv.pid, Restarts: sv.restarts,
			LastErr: sv.lastErr, Hash: sv.hash, Port: sv.spec.Port, Devenv: sv.spec.Devenv}
		v.Healthy = sv.cmd != nil && (sv.state == "running")
		if sv.cmd != nil {
			v.Since = sv.since.Format(time.RFC3339)
		} else if sv.nextStart.After(time.Now()) {
			v.Retry = sv.nextStart.Format(time.RFC3339)
		}
		if sv.spec.Port > 0 {
			v.Proxy = "/svc/" + sv.name + "/"
		}
		out = append(out, v)
	}
	// Declared but disabled services still show, so the page reflects the manifest.
	for name, spec := range s.want {
		if _, ok := s.svcs[name]; !ok && !spec.enabled() {
			out = append(out, serviceView{Name: name, Title: spec.Title, State: "disabled", Port: spec.Port, Devenv: spec.Devenv})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (s *supervisor) service(name string) (*service, serviceSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sv := s.svcs[name]
	if sv == nil {
		return nil, serviceSpec{}, false
	}
	return sv, sv.spec, sv.cmd != nil
}

// busyPaths asks each running service what it's writing into, as paths under
// the served root. A service that doesn't answer in time is skipped: deleting
// shouldn't hang on it.
func (s *supervisor) busyPaths() []string {
	s.mu.Lock()
	var specs []serviceSpec
	for _, sv := range s.svcs {
		if sv.cmd != nil && sv.spec.Busy != "" && sv.spec.Port > 0 {
			specs = append(specs, sv.spec)
		}
	}
	s.mu.Unlock()
	var paths []string
	client := &http.Client{Timeout: time.Second}
	for _, spec := range specs {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", spec.Port, spec.Busy))
		if err != nil {
			continue
		}
		var body struct {
			Paths []string `json:"paths"`
		}
		if resp.StatusCode == http.StatusOK {
			json.NewDecoder(resp.Body).Decode(&body)
		}
		resp.Body.Close()
		paths = append(paths, body.Paths...)
	}
	return paths
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func handleServices(w http.ResponseWriter, r *http.Request) {
	res := map[string]any{
		"commit": runningCommit, "cd": cdRepo != "", "started": started.Format(time.RFC3339),
		"services": []serviceView{},
	}
	if sup != nil {
		res["services"] = sup.views()
		sup.mu.Lock()
		res["manifestErr"] = sup.mErr
		sup.mu.Unlock()
	} else {
		res["manifestErr"] = "the server doesn't know its repo, so it runs no services (start it with serve)"
	}
	writeJSON(w, res)
}

func handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	if sup == nil {
		http.Error(w, "no services", http.StatusNotFound)
		return
	}
	sv, _, _ := sup.service(r.PathValue("name"))
	if sv == nil {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	writeJSON(w, map[string]any{"lines": sv.logs.tail(n)})
}

func handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	if sup == nil {
		http.Error(w, "no services", http.StatusNotFound)
		return
	}
	if err := sup.restart(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleReconcile(w http.ResponseWriter, r *http.Request) {
	if sup == nil {
		http.Error(w, "no services", http.StatusNotFound)
		return
	}
	changes := sup.reconcile()
	if changes == nil {
		changes = []change{}
	}
	writeJSON(w, map[string]any{"changes": changes})
}

// handleProxy forwards /svc/<name>/... to the service on 127.0.0.1.
func handleProxy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if sup == nil {
		http.Error(w, "no services", http.StatusNotFound)
		return
	}
	sv, spec, running := sup.service(name)
	if sv == nil || spec.Port == 0 {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		sup.mu.Lock()
		json.NewEncoder(w).Encode(map[string]any{"error": name + " is not running (" + sv.state + ")", "state": sv.state})
		sup.mu.Unlock()
		return
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", spec.Port)}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, "/svc/"+name+"/")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"error": name + " didn't answer: " + err.Error()})
		},
	}
	p.ServeHTTP(w, r)
}
