package main

import (
	"context"
	"encoding/json"
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

	if _, err := update(ctx); err == nil || !strings.Contains(err.Error(), "no GIT_PASS") {
		t.Fatalf("without GIT_PASS: %v", err)
	}
	os.WriteFile(filepath.Join(cdRepo, ".env"), []byte("GIT_PASS=x\n"), 0o600) // untracked, like the real one

	res, err := update(ctx)
	if err != nil || res.From != res.To || res.Restarting {
		t.Fatalf("no change: %+v %v", res, err)
	}

	commit(t, dev, "README.md", "docs\n")
	res, err = update(ctx)
	if err != nil || res.From == res.To || res.ServerCode || res.Restarting {
		t.Fatalf("docs only: %+v %v", res, err)
	}

	// A pull that declares a service starts it, and one that changes its watched
	// files restarts it, with no server restart.
	sup = newSupervisor(cdRepo, t.TempDir())
	defer func() { sup.stopAll(); sup = nil }()
	spec := helperSpec(freePort(t), nil)
	spec.Watch = []string{"modules/web"}
	b, _ := json.Marshal(manifest{Services: map[string]serviceSpec{"web": spec}})
	commit(t, dev, "homelab.json", string(b))
	res, err = update(ctx)
	if err != nil || res.Restarting || len(res.Services) != 1 || res.Services[0].Action != "started" {
		t.Fatalf("new service: %+v %v", res, err)
	}
	commit(t, dev, "modules/web/app.txt", "v2\n")
	res, err = update(ctx)
	if err != nil || len(res.Services) != 1 || res.Services[0].Action != "restarted" {
		t.Fatalf("watched change: %+v %v", res, err)
	}
	commit(t, dev, "modules/other.txt", "x\n")
	if res, err = update(ctx); err != nil || len(res.Services) != 0 || res.ShellChanged {
		t.Fatalf("unrelated change: %+v %v", res, err)
	}
	commit(t, dev, "devenv.nix", "{\n  scripts.serve.exec = ''\n    one\n  '';\n  enterShell = ''a'';\n}\n")
	if res, err = update(ctx); err != nil || !res.ShellChanged {
		t.Fatalf("serve script added: %+v %v", res, err)
	}
	commit(t, dev, "devenv.nix", "{\n  scripts.serve.exec = ''\n    one\n  '';\n  enterShell = ''b'';\n}\n")
	if res, err = update(ctx); err != nil || res.ShellChanged {
		t.Fatalf("devenv change outside serve: %+v %v", res, err)
	}
	commit(t, dev, "devenv.nix", "{\n  scripts.serve.exec = ''\n    two\n  '';\n  enterShell = ''b'';\n}\n")
	if res, err = update(ctx); err != nil || !res.ShellChanged {
		t.Fatalf("serve script change: %+v %v", res, err)
	}

	commit(t, dev, "server/main.go", "package main\n\nfunc main() { println() }\n")
	res, err = update(ctx)
	if err != nil || !res.ServerCode || !res.Restarting || res.BuildErr != "" || len(res.Services) != 0 {
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
	if _, err := askpassEnv(); err == nil {
		t.Fatal("no .env should be an error")
	}
	os.WriteFile(filepath.Join(cdRepo, ".env"), []byte("OTHER=1\n"), 0o600)
	os.MkdirAll(filepath.Join(cdRepo, "server"), 0o755)
	os.WriteFile(filepath.Join(cdRepo, "server", ".env"), []byte("# c\nexport GIT_PASS=\"s3cr=t\"\n"), 0o600)
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
