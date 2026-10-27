package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

// buildBinary builds promptbuilder into a temp dir, for tests that need
// the real binary driven by a real bash.
func buildBinary(t *testing.T) (binDir string) {
	t.Helper()
	binDir = t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "promptbuilder"), ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binDir
}

// Bash expands $(...) and `...` inside PS1 every time it draws the
// prompt, so anything attacker-controlled that ends up in the prompt text
// (a directory name from an extracted archive, the default branch name of
// a cloned repo) must never be executed. Checked with the shipped wiring
// from try-prompt.bash, driven by a real bash. A fake promptbuilder on
// $PATH checks that the binary is called by its full path, never looked
// up.
func TestPromptDoesNotExecuteNames(t *testing.T) {
	bin := buildBinary(t)
	tmp := t.TempDir()

	dirs := []string{
		filepath.Join(tmp, "$(touch${IFS}PWNED_DIR)"),
		filepath.Join(tmp, "`touch${IFS}PWNED_BACKTICK`"),
	}
	repo := filepath.Join(tmp, "repo")
	for _, d := range append(dirs, repo) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-q")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "x")
	git(t, repo, "switch", "-q", "-c", "$(touch${IFS}PWNED_BRANCH)")
	dirs = append(dirs, repo)

	fakeBin := t.TempDir()
	fake := "#!/usr/bin/env bash\ntouch PWNED_PATH_LOOKUP\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "promptbuilder"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, dir := range dirs {
		script := `source "$1" >/dev/null 2>&1
promptbuilder_activate "$3" >/dev/null || exit 1
cd "$2" || exit 1
eval "$PROMPT_COMMAND"
printf '%s' "${PS1@P}"`
		cmd := exec.Command("bash", "--norc", "--noprofile", "-c", script, "bash",
			filepath.Join(mustGetwd(t), "try-prompt.bash"), dir, filepath.Join(bin, "promptbuilder"))
		cmd.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"XDG_RUNTIME_DIR="+filepath.Join(tmp, "no-such-dir"), // no daemon
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash in %q: %v\n%s", dir, err, out)
		}
		name := filepath.Base(dir)
		if dir == repo {
			name = "$(touch${IFS}PWNED_BRANCH)"
		}
		if !strings.Contains(string(out), name) {
			t.Errorf("prompt in %q doesn't show %q literally:\n%q", dir, name, out)
		}
	}

	pwned, _ := filepath.Glob(filepath.Join(tmp, "*", "PWNED_*"))
	more, _ := filepath.Glob(filepath.Join(tmp, "PWNED_*"))
	if all := append(pwned, more...); len(all) > 0 {
		t.Errorf("prompt executed commands from names, created: %v", all)
	}
}

// try-prompt.bash is for trying the repo's own build: without an argument
// it uses the promptbuilder next to it - resolved to an absolute path even
// when sourced via a relative one - and refuses to activate if there is
// none.
func TestTryPromptDefaultsToRepoBuild(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(mustGetwd(t), "try-prompt.bash"))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "try-prompt.bash"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		code := `cd "$1" && source repo/try-prompt.bash >/dev/null; cd /
eval "$PROMPT_COMMAND"
printf 'PROMPT_COMMAND=[%s] out=[%s]' "$PROMPT_COMMAND" "$_pb_ps1"`
		out, _ := exec.Command("bash", "--norc", "--noprofile", "-c", code, "bash", base).CombinedOutput()
		return string(out)
	}

	if out := run(); !strings.Contains(out, "make build") || !strings.Contains(out, "PROMPT_COMMAND=[]") {
		t.Errorf("activated without a repo build:\n%s", out)
	}

	fake := "#!/usr/bin/env bash\nprintf REPO_BUILD\n"
	if err := os.WriteFile(filepath.Join(repo, "promptbuilder"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	out := run()
	if !strings.Contains(out, filepath.Join(repo, "promptbuilder")) || !strings.Contains(out, "out=[REPO_BUILD]") {
		t.Errorf("didn't run the repo build by absolute path:\n%s", out)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// ownSGR matches the escape sequences the prompt itself emits (see esc).
var ownSGR = regexp.MustCompile("\x01\x1b\\[[0-9;]*m\x02")

// assertNoStrayControl fails if s contains any control character other
// than newlines and the prompt's own color sequences - a terminal would
// interpret those (set the title, write the clipboard via OSC 52, clear
// the screen, ...).
func assertNoStrayControl(t *testing.T, what, s string) {
	t.Helper()
	rest := ownSGR.ReplaceAllString(s, "")
	for i, r := range rest {
		bad := r == utf8.RuneError || (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f)
		if bad {
			t.Errorf("%s: stray control character %U at byte %d in %q", what, r, i, rest)
			return
		}
	}
}

func TestNoControlCharsFromDirName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a\x1b]0;evil\x07b\x9bc\x1b[2Jd")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	assertNoStrayControl(t, "prompt", buildPrompt(0, false))
}

func TestNoControlCharsFromEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("debian_chroot", "c\x1b]0;evil\x07")
	t.Setenv("GOPATH", "/g\x1b[2J")
	t.Setenv("TOOLCHAIN_PREFIX", "t\x1b]52;c;ZXZpbA==\x07")
	t.Setenv("DOCKER_HOST", "d\x9b2J")
	t.Setenv("PROMPT_EXTRA", "x\x1b[2Jy")
	assertNoStrayControl(t, "prompt", buildPrompt(0, false))
}

// A hand-made .git (from an archive, say) can put anything in HEAD - git
// itself never creates such a ref name, but the prompt must not trust it.
func TestNoControlCharsFromCraftedHead(t *testing.T) {
	for _, head := range []string{
		"ref: refs/heads/x\x1b]0;evil\x07\n",
		"\x1b[2Jaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", // "detached"
	} {
		repo := t.TempDir()
		git(t, repo, "init", "-q")
		if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte(head), 0o644); err != nil {
			t.Fatal(err)
		}
		assertNoStrayControl(t, "headInfo branch", headInfo(repo).Branch)
		t.Chdir(repo)
		assertNoStrayControl(t, "prompt", buildPrompt(0, false))
	}
}

// git status runs a repo's core.fsmonitor hook - a command taken from
// .git/config, which an extracted archive or shared directory controls.
// The daemon's git calls must not run it.
func TestGitCallsDoNotRunRepoFsmonitor(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	writeFile(t, repo, "f.txt", "x\n")
	git(t, repo, "add", "f.txt")
	git(t, repo, "commit", "-q", "-m", "x")

	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(hook, []byte("#!/usr/bin/env bash\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.fsmonitor", hook)

	// Precondition: plain git status really does run it, otherwise this
	// test would prove nothing.
	git(t, repo, "status", "--porcelain")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("precondition: git status didn't run the fsmonitor hook: %v", err)
	}
	os.Remove(marker)

	if _, ok, errMsg := runGitStatus(repo); !ok {
		t.Fatalf("runGitStatus: %s", errMsg)
	}
	recentDirs(repo)
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the daemon's git calls ran the repo's core.fsmonitor hook")
	}
}

// Without $XDG_RUNTIME_DIR the socket lives under the shared temp dir,
// where another user could have created things first. Only a directory
// that's ours, private, and not a symlink may be used.
func TestFallbackSocketDirMustBePrivate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", tmp)

	p := socketPath()
	if p == "" || !strings.HasPrefix(p, tmp+string(os.PathSeparator)) {
		t.Fatalf("socketPath() = %q, want a path under $TMPDIR %s", p, tmp)
	}
	dir := filepath.Dir(p)
	if dir == tmp {
		t.Fatalf("socketPath() = %q directly in the shared temp dir, want a private subdir", p)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("socket dir not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 || !fi.IsDir() {
		t.Fatalf("socket dir mode %v, want drwx------", fi.Mode())
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if p := socketPath(); p != "" {
		t.Errorf("socket dir readable by others: socketPath() = %q, want refused", p)
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Chmod(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if p := socketPath(); p != "" {
		t.Errorf("socket dir is a symlink: socketPath() = %q, want refused", p)
	}
}

func withTrustedUID(t *testing.T, uid int) {
	t.Helper()
	old := trustedUID
	trustedUID = uid
	t.Cleanup(func() { trustedUID = old })
}

// A process of another user listening on the socket path must not be
// believed - its answer ends up in the prompt.
func TestClientIgnoresDaemonOfOtherUser(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ln, err := net.Listen("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			c.Read(buf)
			c.Write([]byte(`{"isRepo":true,"ok":true,"info":{"Branch":"$(evil)"}}` + "\n"))
			c.Close()
		}
	}()

	withTrustedUID(t, os.Getuid()+1) // pretend the listener is someone else
	if gr, reached := tryDaemon(t.TempDir()); reached {
		t.Errorf("client used an answer from another user's process: %+v", gr)
	}
}

// The daemon runs git status as its owner in whatever directory a request
// names - only its owner may ask.
func TestDaemonChecksPeerUser(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	for _, tc := range []struct {
		uid  int
		want bool
	}{{os.Getuid(), true}, {os.Getuid() + 1, false}} {
		withTrustedUID(t, tc.uid)
		client, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		server, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if got := peerIsTrusted(server); got != tc.want {
			t.Errorf("peerIsTrusted with trustedUID=%d (peer uid %d) = %v, want %v",
				tc.uid, syscall.Getuid(), got, tc.want)
		}
		client.Close()
		server.Close()
	}
}
