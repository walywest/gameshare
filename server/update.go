package main

// CD mode. Started with `serve --cd`, the server accepts POST /api/update: it runs
// git pull in its own repo and, if server/ changed and still builds, exits with
// restartCode so the serve loop rebuilds and restarts it. Transfers resume after
// the restart on their own.
//
// The pull authenticates with GIT_PASS (and GIT_USER, if set) from .env at the repo
// root or in server/, read on every update so editing .env needs no restart. git is
// never allowed to prompt: without GIT_PASS the update stops with an error.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const restartCode = 75 // the serve loop restarts on this exit status, and stops on any other

var (
	cdRepo        string // repo root, set only in CD mode
	runningCommit string // commit the running server was built from, in CD mode
	goBin         = "go"
	updating      sync.Mutex
)

type updateResult struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Log        string `json:"log,omitempty"`      // commits pulled, one per line
	ServerCode bool   `json:"serverCode"`         // whether server/ changed
	BuildErr   string `json:"buildErr,omitempty"` // set when the new code doesn't build
	Restarting bool   `json:"restarting"`
	// Services the pull started, restarted or stopped. Empty when the server
	// restarts, since the new process starts them all again.
	Services []change `json:"services,omitempty"`
	// ShellChanged means the serve script itself changed, which only takes
	// effect when serve is restarted by hand.
	ShellChanged bool `json:"shellChanged,omitempty"`
	// ManifestErr says the pulled homelab.json is broken, so services keep
	// running as the last good one declared.
	ManifestErr string `json:"manifestErr,omitempty"`
}

func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !updating.TryLock() {
		http.Error(w, "an update is already running", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := update(ctx)
	if err != nil {
		updating.Unlock()
		log.Printf("update failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
	if !res.Restarting {
		updating.Unlock()
		return
	}
	// The lock stays held, so nothing else starts before the exit.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		log.Printf("updated %s -> %s, stopping services and restarting", res.From, res.To)
		stopServices()
		os.Exit(restartCode)
	}()
}

func update(ctx context.Context) (updateResult, error) {
	var res updateResult
	before, err := git(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	auth, err := askpassEnv()
	if err != nil {
		return res, err
	}
	pullCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	_, err = git(pullCtx, auth, "pull", "--ff-only")
	cancel()
	if err != nil {
		return res, err
	}
	after, err := git(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	res.From, res.To = short(before), short(after)
	if before == after {
		return res, nil
	}
	res.ShellChanged = serveScript(ctx, before) != serveScript(ctx, after)
	res.Log, _ = git(ctx, nil, "log", "--oneline", "-20", before+".."+after)
	changed, err := git(ctx, nil, "diff", "--name-only", before, after, "--", "server")
	if err != nil {
		return res, err
	}
	if res.ServerCode = changed != ""; !res.ServerCode {
		if sup != nil {
			res.Services = sup.reconcile()
			sup.mu.Lock()
			res.ManifestErr = sup.mErr
			sup.mu.Unlock()
		}
		return res, nil
	}
	if out, err := build(ctx); err != nil {
		res.BuildErr = out
		log.Printf("pulled %s -> %s but it doesn't build, keeping the running version:\n%s", res.From, res.To, out)
		return res, nil
	}
	res.Restarting = true
	return res, nil
}

// serveScript is the serve loop's text in devenv.nix at a commit. It runs for as
// long as serve does, so a change to it is the one thing an update can't apply.
func serveScript(ctx context.Context, commit string) string {
	nix, err := git(ctx, nil, "show", commit+":devenv.nix")
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(nix, "scripts.serve.exec")
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, "\n  '';")
	return body
}

// build checks that the pulled server compiles. The serve loop builds it again for
// real, from Go's cache.
func build(ctx context.Context) (string, error) {
	tmp, err := os.MkdirTemp("", "game-share-build-*")
	if err != nil {
		return err.Error(), err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", filepath.Join(tmp, "game-share"), ".")
	cmd.Dir = filepath.Join(cdRepo, "server")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func git(ctx context.Context, env []string, args ...string) (string, error) {
	// No stored credential helpers and no prompts of any kind: no terminal, no
	// desktop password dialog, no ssh questions. Later entries in Env win.
	// A checkout cloned over SSH would ask for an SSH password and ignore GIT_PASS,
	// so GitHub is always reached over HTTPS.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cdRepo, "-c", "credential.helper=", "-c", "core.askPass=",
		"-c", "url.https://github.com/.insteadOf=git@github.com:",
		"-c", "url.https://github.com/.insteadOf=ssh://git@github.com/"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS=", "GIT_ASKPASS=", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	cmd.Env = append(cmd.Env, env...)
	cmd.WaitDelay = 5 * time.Second // don't wait forever on a helper still holding the output pipe
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// askpassEnv makes git ask this same binary for the credentials, so the password
// never lands in a file or on a command line.
func askpassEnv() ([]string, error) {
	var vars map[string]string
	var tried []string
	for _, name := range []string{filepath.Join(cdRepo, ".env"), filepath.Join(cdRepo, "server", ".env")} {
		v, err := readDotenv(name)
		if err != nil {
			return nil, err
		}
		tried = append(tried, name)
		if v["GIT_PASS"] != "" {
			vars = v
			break
		}
	}
	if vars == nil {
		return nil, fmt.Errorf("no GIT_PASS in %s", strings.Join(tried, " or "))
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	user := vars["GIT_USER"]
	if user == "" {
		user = "x-access-token" // GitHub accepts any user name with a token
	}
	return []string{"GIT_ASKPASS=" + self, "GAMESHARE_ASKPASS=1", "GIT_USER=" + user, "GIT_PASS=" + vars["GIT_PASS"]}, nil
}

// askpass answers git's prompt when the binary runs as GIT_ASKPASS.
func askpass(prompt string) string {
	if strings.HasPrefix(prompt, "Username") {
		return os.Getenv("GIT_USER")
	}
	return os.Getenv("GIT_PASS")
}

// readDotenv reads KEY=value lines, allowing comments, `export` and quotes. A
// missing file is not an error.
func readDotenv(name string) (map[string]string, error) {
	vars := map[string]string{}
	f, err := os.Open(name)
	if os.IsNotExist(err) {
		return vars, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		vars[strings.TrimSpace(key)] = val
	}
	return vars, sc.Err()
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
