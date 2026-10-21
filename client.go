package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	// daemonDialTimeout: how long to wait to even connect. Short and
	// separate from the response timeout below - if nothing's listening
	// at all, fail fast and fall back immediately rather than waiting.
	daemonDialTimeout = 150 * time.Millisecond

	// daemonResponseTimeout: how long to wait for an actual response once
	// connected. Needs headroom above runGitStatus's own 1s timeout (see
	// main.go), since the first request for a newly-seen repo blocks on a
	// synchronous fetch (daemon.go's getOrCreate) - using the short dial
	// timeout here too would make every cold-repo request spuriously time
	// out client-side before the daemon's own fetch even finishes.
	daemonResponseTimeout = 2 * time.Second
)

// resolveGit is what the live-PS1 render path calls for git status. It
// tries the caching daemon first and only falls back to a direct
// `git status` call (today's behavior, unchanged) if the daemon isn't
// reachable - the daemon is a pure optimization, never a hard dependency.
func resolveGit() gitResult {
	cwd, err := os.Getwd()
	if err != nil {
		return directGitResult("")
	}

	if gr, usable := tryDaemon(cwd); usable {
		return gr
	}

	spawnDaemon()
	return directGitResult(cwd)
}

// tryDaemon asks the daemon for cached git status. usable reports whether
// the daemon actually answered at all - the caller must fall back to a
// direct git call whenever usable is false, regardless of gr's contents.
func tryDaemon(cwd string) (gr gitResult, usable bool) {
	conn, err := net.DialTimeout("unix", socketPath(), daemonDialTimeout)
	if err != nil {
		return gitResult{}, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(daemonResponseTimeout))

	if _, err := conn.Write([]byte(cwd + "\n")); err != nil {
		return gitResult{}, false
	}

	var resp daemonResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return gitResult{}, false
	}
	return gitResult{IsRepo: resp.IsRepo, OK: resp.OK, Info: resp.Info, Error: resp.Error}, true
}

// spawnDaemon starts `promptbuilder --daemon` fully detached and returns
// immediately without waiting for it to come up. Concurrent callers doing
// this at the same time is fine - the daemon's own Listen() on the socket
// is what prevents duplicates; see spawnDaemon's counterpart in daemon.go
// (runDaemon) and ~/remote-repos/patterns/unix-socket-daemon-spawning.md.
func spawnDaemon() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "--daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}
