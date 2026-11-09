package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func commit(t *testing.T, dir, file, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", file)
	run(t, dir, "git", "push", "-q")
}

func TestUpdate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	tmp := t.TempDir()
	origin, dev := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "dev")
	cdRepo = filepath.Join(tmp, "server-side")
	run(t, tmp, "git", "init", "-q", "--bare", "-b", "main", origin)
	run(t, tmp, "git", "clone", "-q", origin, dev)
	commit(t, dev, "server/go.mod", "module x\n\ngo 1.22\n")
	commit(t, dev, "server/main.go", "package main\n\nfunc main() {}\n")
	run(t, tmp, "git", "clone", "-q", origin, cdRepo)
	ctx := context.Background()

	res, err := update(ctx)
	if err != nil || res.From != res.To || res.Restarting {
		t.Fatalf("no change: %+v %v", res, err)
	}

	commit(t, dev, "README.md", "docs\n")
	res, err = update(ctx)
	if err != nil || res.From == res.To || res.ServerCode || res.Restarting {
		t.Fatalf("docs only: %+v %v", res, err)
	}

	commit(t, dev, "server/main.go", "package main\n\nfunc main() { println() }\n")
	res, err = update(ctx)
	if err != nil || !res.ServerCode || !res.Restarting || res.BuildErr != "" {
		t.Fatalf("server change: %+v %v", res, err)
	}

	commit(t, dev, "server/main.go", "package main\n\nfunc main() { nope }\n")
	res, err = update(ctx)
	if err != nil || res.Restarting || !strings.Contains(res.BuildErr, "nope") {
		t.Fatalf("broken build: %+v %v", res, err)
	}

	os.WriteFile(filepath.Join(cdRepo, "README.md"), []byte("local edit\n"), 0o644)
	commit(t, dev, "README.md", "conflict\n")
	if _, err := update(ctx); err == nil {
		t.Fatal("pull over a local edit should fail")
	}
}

func TestDotenvAndAskpass(t *testing.T) {
	cdRepo = t.TempDir()
	if env, err := askpassEnv(); err != nil || env != nil {
		t.Fatalf("no .env: %v %v", env, err)
	}
	os.WriteFile(filepath.Join(cdRepo, ".env"), []byte("# c\nexport GIT_PASS=\"s3cr=t\"\nOTHER=1\n"), 0o600)
	env, err := askpassEnv()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_PASS=s3cr=t") || !strings.Contains(joined, "GIT_USER=x-access-token") {
		t.Fatalf("env: %v", env)
	}
	t.Setenv("GIT_USER", "u")
	t.Setenv("GIT_PASS", "p")
	if askpass("Username for 'https://github.com': ") != "u" || askpass("Password for 'https://u@github.com': ") != "p" {
		t.Fatal("askpass answers")
	}
}
