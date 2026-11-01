// gs: terminal client for game-share. Transfers resume after dropped connections,
// restarts and reboots, because the queue and partial files live on disk.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	home, _ := os.UserHomeDir()
	config, err := os.UserConfigDir()
	if err != nil {
		config = home
	}
	dest := flag.String("d", filepath.Join(home, "Downloads", "GameShare"), "download folder")
	stateFile := flag.String("state", filepath.Join(config, "game-share", "queue.json"), "queue file")
	get := flag.String("get", "", "without the UI: download this remote file or folder")
	put := flag.String("put", "", "without the UI: upload this local file or folder")
	into := flag.String("into", "", "remote folder for -put")
	checkOnly := flag.Bool("check", false, "run the speed check and exit, with status 1 on any alert")
	noCheck := flag.Bool("no-check", false, "skip the speed check before starting")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gs [flags] [http://server:8080]\nThe server and queue are remembered between runs.")
		flag.PrintDefaults()
	}
	flag.Parse()

	server := loadSaved(*stateFile).Server
	if flag.NArg() > 0 {
		server = flag.Arg(0)
		if !strings.Contains(server, "://") {
			server = "http://" + server
		}
	}
	if server == "" {
		flag.Usage()
		os.Exit(2)
	}

	e := NewEngine(strings.TrimRight(server, "/"), *dest, *stateFile)
	e.Restore()
	e.Save() // remember the server even if this run stops at the speed check

	if *checkOnly {
		r, ok := e.SpeedCheck()
		if !ok || Verdict(r) {
			os.Exit(1)
		}
		return
	}
	if *get != "" || *put != "" {
		os.Exit(headless(e, *get, *put, *into))
	}

	// Runs before Kick so restored transfers don't skew the measurements.
	if !*noCheck {
		if r, ok := e.SpeedCheck(); ok && Verdict(r) {
			fmt.Print("\n  Press Enter to continue anyway, or Ctrl-C to fix this first. ")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}

	e.Kick()
	p := tea.NewProgram(newModel(e), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if n := e.Pending(); n > 0 {
		fmt.Printf("%d left in the queue. Run gs again to resume.\n", n)
	}
}

// parentOf returns the folder above a remote path, or "" at the top.
func parentOf(remote string) string {
	dir := path.Dir(strings.Trim(remote, "/"))
	if dir == "." {
		return ""
	}
	return dir
}

func headless(e *Engine, get, put, into string) int {
	e.Kick()
	if get != "" {
		if _, err := e.QueueDownload(parentOf(get), get); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if put != "" {
		if _, err := e.QueueUpload(put, into); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	for e.Pending() > 0 {
		time.Sleep(time.Second)
		for _, v := range e.Snapshot() {
			if v.State == active {
				fmt.Printf("%s %s/%s %s/s %s\n", v.Item.Remote, bytes_(v.Sent), bytes_(v.Size), bytes_(int64(v.Speed)), v.Err)
			}
		}
	}
	code := 0
	for _, v := range e.Snapshot() {
		if v.State == failed {
			fmt.Fprintln(os.Stderr, v.Item.Remote+": "+v.Err)
			code = 1
		}
	}
	return code
}
