package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// defaultFixtureRepo is the jdiff repo, used read-only as a clone source -
// tests never touch it directly, only throwaway clones of it.
const defaultFixtureRepo = "/home/claude/remote-repos/03_PROJECT/diff"

// inotifyTimeout is how long a change may take to show up in the daemon's
// cache. The periodic refresh is disabled in tests (see newTestDaemon), so
// only inotify can explain an update within this window.
const inotifyTimeout = 500 * time.Millisecond

// realGit is resolved once, before any test puts a slow wrapper on $PATH,
// so the tests' own git commands always run at full speed.
var realGit string

func TestMain(m *testing.M) {
	p, err := exec.LookPath("git")
	if err != nil {
		fmt.Fprintln(os.Stderr, "git not found:", err)
		os.Exit(1)
	}
	realGit = p

	// Keep the user's git config (signing, hooks, aliases) out of the
	// fixture repos, and make commits work without a configured identity.
	os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_AUTHOR_NAME", "test")
	os.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	os.Setenv("GIT_COMMITTER_NAME", "test")
	os.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	os.Unsetenv("GIT_OPTIONAL_LOCKS")
	os.Exit(m.Run())
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(realGit, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture clones the jdiff repo into a bare "origin" and a working clone of
// that origin, both under t.TempDir(), so push/fetch stay local.
func fixture(t *testing.T) (work, origin string) {
	t.Helper()
	src := os.Getenv("PROMPTBUILDER_TEST_REPO")
	if src == "" {
		src = defaultFixtureRepo
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		t.Skipf("fixture repo %s not available: %v", src, err)
	}
	tmp := t.TempDir()
	origin = filepath.Join(tmp, "origin.git")
	work = filepath.Join(tmp, "work")
	git(t, tmp, "clone", "-q", "--bare", src, origin)
	git(t, tmp, "clone", "-q", origin, work)
	return work, origin
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	writeFile(t, dir, name, content)
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", "change "+name)
}

// newTestDaemon builds daemon state the same way runDaemon does, but with
// the periodic refresh and eviction pushed out of reach and without the
// socket listener - requests go through handleConn directly (see query).
func newTestDaemon(t *testing.T) *daemonState {
	t.Helper()
	d, err := newDaemonState()
	if err != nil {
		t.Fatal(err)
	}
	d.refreshInterval = time.Hour
	d.idleEvict = time.Hour
	go d.inotifyLoop()
	return d
}

// slowGit puts a git wrapper first on $PATH for the daemon's own git calls:
// it runs the real git right away (so the result reflects the repo state at
// call time) but delays handing the output back by delay.
func slowGit(t *testing.T, delay time.Duration) {
	t.Helper()
	gitWrapper(t, delay)
}

// countGitStatus puts a git wrapper first on $PATH that logs every call
// the daemon makes, and returns a function counting the `status` calls so
// far.
func countGitStatus(t *testing.T) func() int {
	t.Helper()
	log := gitWrapper(t, 0)
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), " status ")
	}
}

func gitWrapper(t *testing.T, delay time.Duration) (logFile string) {
	t.Helper()
	bin := t.TempDir()
	logFile = filepath.Join(bin, "calls.log")
	script := fmt.Sprintf(`#!/usr/bin/env bash
printf ' %%s \n' "$*" >> %q
out=$(%q "$@")
rc=$?
sleep %.3f
printf '%%s\n' "$out"
exit $rc
`, logFile, realGit, delay.Seconds())
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

// commitInDirs makes one commit per dir, in order, so the last one listed
// is the most recent in history.
func commitInDirs(t *testing.T, work string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(work, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		commitFile(t, work, filepath.Join(dir, "f.txt"), "v1\n")
	}
}

// waitFor polls cond until it reports true or timeout passes. cond's
// string describes the current state for the failure message.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s; last state: %s", timeout, what, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitInfo(t *testing.T, d *daemonState, root, what string, cond func(gitInfo) bool) {
	t.Helper()
	waitInfoTimeout(t, d, root, what, inotifyTimeout, cond)
}

func waitInfoTimeout(t *testing.T, d *daemonState, root, what string, timeout time.Duration, cond func(gitInfo) bool) {
	t.Helper()
	rs := d.getOrCreate(root)
	waitFor(t, timeout, what, func() (bool, string) {
		info, found, errMsg := rs.snapshot()
		return found && cond(info), fmt.Sprintf("found=%v err=%q info=%+v", found, errMsg, info)
	})
}

// settle waits until every change made so far has been processed: queued
// inotify events dispatched and any refresh they started finished. Used
// before an edit whose detection is the thing under test, so that a refresh
// triggered by an earlier step can't pick it up by accident.
func settle(t *testing.T, d *daemonState, root string) {
	t.Helper()
	d.drainEvents()
	d.getOrCreate(root).waitSettled(2 * time.Second)
}

// query does one real request/response round-trip through handleConn.
// raw holds the decoded JSON as a map too, so tests can check fields the
// response struct may not have yet.
func query(t *testing.T, d *daemonState, cwd string) (resp daemonResponse, raw map[string]any) {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	go d.handleConn(server)
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte(cwd + "\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	body, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return resp, raw
}

// Each git command that rewrites .git/index does so via index.lock +
// rename, so the change must be seen every time, not just the first.
func TestRepeatedStagingIsSeen(t *testing.T) {
	work, _ := fixture(t)
	d := newTestDaemon(t)
	waitInfo(t, d, work, "initial status", func(gitInfo) bool { return true })

	for i := 0; i < 3; i++ {
		writeFile(t, work, "staged.txt", fmt.Sprintf("round %d\n", i))
		git(t, work, "add", "staged.txt")
		waitInfo(t, d, work, fmt.Sprintf("Staged after add #%d", i), func(gi gitInfo) bool { return gi.Staged })

		git(t, work, "reset", "-q")
		waitInfo(t, d, work, fmt.Sprintf("not Staged after reset #%d", i), func(gi gitInfo) bool { return !gi.Staged })
	}
}

// Switching between existing branches only rewrites HEAD (via HEAD.lock +
// rename) - every switch must be seen.
func TestRepeatedBranchSwitchIsSeen(t *testing.T) {
	work, _ := fixture(t)
	start := git(t, work, "rev-parse", "--abbrev-ref", "HEAD")
	git(t, work, "branch", "other")
	d := newTestDaemon(t)
	waitInfo(t, d, work, "initial status", func(gitInfo) bool { return true })

	for i := 0; i < 2; i++ {
		for _, b := range []string{"other", start} {
			git(t, work, "switch", "-q", b)
			waitInfo(t, d, work, "branch "+b, func(gi gitInfo) bool { return gi.Branch == b })
		}
	}
}

// A commit on a branch with a "/" updates refs/heads/feature/x, a
// subdirectory of refs/heads.
func TestCommitOnNestedBranchIsSeen(t *testing.T) {
	work, _ := fixture(t)
	d := newTestDaemon(t)
	waitInfo(t, d, work, "initial status", func(gitInfo) bool { return true })

	git(t, work, "switch", "-q", "-c", "feature/x")
	waitInfo(t, d, work, "branch feature/x", func(gi gitInfo) bool { return gi.Branch == "feature/x" })

	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("nested-%d.txt", i)
		writeFile(t, work, name, "x\n")
		git(t, work, "add", name)
		waitInfo(t, d, work, "Staged before commit", func(gi gitInfo) bool { return gi.Staged })
		git(t, work, "commit", "-q", "-m", name)
		waitInfo(t, d, work, "clean after commit", func(gi gitInfo) bool { return !gi.Staged })
	}
}

// git push only updates refs/remotes/origin/<branch> locally - the ahead
// marker must go back to in-sync.
func TestPushIsSeen(t *testing.T) {
	work, _ := fixture(t)
	d := newTestDaemon(t)
	waitInfo(t, d, work, "upstream configured", func(gi gitInfo) bool { return gi.HasUp && gi.Ahead == 0 })

	for i := 0; i < 2; i++ {
		commitFile(t, work, "pushed.txt", fmt.Sprintf("round %d\n", i))
		waitInfo(t, d, work, "Ahead=1 after commit", func(gi gitInfo) bool { return gi.Ahead == 1 })
		git(t, work, "push", "-q")
		waitInfo(t, d, work, "Ahead=0 after push", func(gi gitInfo) bool { return gi.Ahead == 0 })
	}
}

// A change that lands while a refresh is already running must not be
// dropped: the final cached state has to match the final repo state.
func TestChangeDuringRefreshIsNotLost(t *testing.T) {
	work, _ := fixture(t)
	d := newTestDaemon(t)
	waitInfo(t, d, work, "upstream configured", func(gi gitInfo) bool { return gi.HasUp })

	slowGit(t, 300*time.Millisecond)
	commitFile(t, work, "a.txt", "a\n") // triggers a refresh that sees Ahead=1 ...
	time.Sleep(100 * time.Millisecond)
	commitFile(t, work, "b.txt", "b\n") // ... and this lands while it is still running

	waitInfoTimeout(t, d, work, "Ahead=2", 2*time.Second, func(gi gitInfo) bool { return gi.Ahead == 2 })
}

// The prompt asks right after a command finishes - the answer must already
// reflect that command, not the state one prompt earlier.
func TestRequestRightAfterChangeIsFresh(t *testing.T) {
	work, _ := fixture(t)
	d := newTestDaemon(t)
	waitInfo(t, d, work, "upstream configured", func(gi gitInfo) bool { return gi.HasUp && gi.Ahead == 0 })

	for i := 1; i <= 5; i++ {
		commitFile(t, work, "fresh.txt", fmt.Sprintf("round %d\n", i))
		resp, _ := query(t, d, work)
		if !resp.OK || resp.Info.Ahead != i {
			t.Fatalf("round %d: response right after commit: ok=%v ahead=%d, want ok=true ahead=%d",
				i, resp.OK, resp.Info.Ahead, i)
		}
	}
}

// Several prompts (e.g. multiple terminals) asking about a repo the daemon
// hasn't seen yet, while git is slow: none of them may block for long, and
// none may get an "error" with no detail.
func TestConcurrentFirstRequests(t *testing.T) {
	work, _ := fixture(t)
	d := newTestDaemon(t)
	slowGit(t, time.Second)

	const n = 5
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			resp, raw := query(t, d, work)
			took := time.Since(t0)
			if took > 500*time.Millisecond {
				errs <- fmt.Sprintf("request took %v", took)
			}
			pending, _ := raw["pending"].(bool)
			if !resp.OK && resp.Error == "" && !pending {
				errs <- fmt.Sprintf("ok=false with empty error and not pending: %+v", raw)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// In a linked worktree .git is a file ("gitdir: ...") - changes there must
// still be seen.
func TestWorktreeIsWatched(t *testing.T) {
	work, _ := fixture(t)
	wt := filepath.Join(filepath.Dir(work), "wt")
	git(t, work, "worktree", "add", "-q", "-b", "wtbranch", wt)
	d := newTestDaemon(t)
	waitInfo(t, d, wt, "branch wtbranch", func(gi gitInfo) bool { return gi.Branch == "wtbranch" })

	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("wt-%d.txt", i)
		writeFile(t, wt, name, "x\n")
		git(t, wt, "add", name)
		waitInfo(t, d, wt, "Staged in worktree", func(gi gitInfo) bool { return gi.Staged })
		git(t, wt, "commit", "-q", "-m", name)
		waitInfo(t, d, wt, "clean after commit in worktree", func(gi gitInfo) bool { return !gi.Staged })
	}
}

// The daemon's status call must not rewrite .git/index: that would contend
// for index.lock with the user's own git commands and fire inotify on
// itself.
func TestStatusDoesNotWriteIndex(t *testing.T) {
	work, _ := fixture(t)
	file := strings.SplitN(git(t, work, "ls-files"), "\n", 2)[0]
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(work, file), future, future); err != nil {
		t.Fatal(err)
	}

	index := filepath.Join(work, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, errMsg := runGitStatus(work); !ok {
		t.Fatalf("runGitStatus failed: %s", errMsg)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	inode := func(fi os.FileInfo) uint64 { return fi.Sys().(*syscall.Stat_t).Ino }
	if inode(before) != inode(after) || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf(".git/index was rewritten by runGitStatus (inode %d -> %d, mtime %v -> %v)",
			inode(before), inode(after), before.ModTime(), after.ModTime())
	}
}
