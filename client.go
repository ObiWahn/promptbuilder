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
	// connected. The daemon itself answers within its wait budget
	// (daemon.go's defaultWaitBudget) no matter how slow git is, so this
	// only needs a little headroom above that.
	daemonResponseTimeout = 500 * time.Millisecond
)

// resolveGit is what the live-PS1 render path calls for git status. It
// tries the caching daemon first and only falls back to a direct
// `git status` call if the daemon isn't reachable at all - the daemon is a
// pure optimization, never a hard dependency.
func resolveGit() gitResult {
	cwd, err := os.Getwd()
	if err != nil {
		return directGitResult("")
	}

	if gr, reached := tryDaemon(cwd); reached {
		return gr
	}

	if socketPath() != "" { // no safe socket location: a daemon couldn't start anyway
		spawnDaemon()
	}
	return directGitResult(cwd)
}

// tryDaemon asks the daemon for cached git status. reached reports whether
// a daemon was there to connect to at all - the caller must fall back to a
// direct git call whenever reached is false, regardless of gr's contents.
// If a daemon was reached but didn't answer in time, running git directly
// would most likely be just as slow, so gr is instead a pending result
// with only the branch, read straight from HEAD.
func tryDaemon(cwd string) (gr gitResult, reached bool) {
	path := socketPath()
	if path == "" {
		return gitResult{}, false
	}
	conn, err := net.DialTimeout("unix", path, daemonDialTimeout)
	if err != nil {
		return gitResult{}, false
	}
	defer conn.Close()
	if !peerIsTrusted(conn) {
		return gitResult{}, false // not our daemon - don't believe a word
	}
	conn.SetDeadline(time.Now().Add(daemonResponseTimeout))

	if _, err := conn.Write([]byte(cwd + "\n")); err != nil {
		return pendingGitResult(cwd), true
	}

	var resp daemonResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return pendingGitResult(cwd), true
	}
	return gitResult{IsRepo: resp.IsRepo, OK: resp.OK, Pending: resp.Pending, Info: resp.Info, Error: resp.Error}, true
}

func pendingGitResult(cwd string) gitResult {
	root, isRepo := findRepoRoot(cwd)
	if !isRepo {
		return gitResult{}
	}
	return gitResult{IsRepo: true, Pending: true, Info: headInfo(root)}
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
