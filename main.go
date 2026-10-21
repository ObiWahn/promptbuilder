package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// esc wraps an SGR escape sequence in readline's zero-width markers
// (\x01 / \x02) so bash doesn't miscount the visible prompt length.
// These must be raw bytes, not literal "\[" "\]" text: bash only decodes
// "\[" "\]" once, before command substitution runs, so escapes produced by
// this program's own output have to already be in their final byte form.
func esc(sgr string) string {
	return "\x01\x1b[" + sgr + "m\x02"
}

var reset = esc(sgrReset)

func userColor(username string, uid int) string {
	if uid == 0 {
		return sgrRed // unconditional, matches the original's unconditional root check
	}
	if sgr, ok := configuredColor("user", username); ok {
		return sgr
	}
	// No config file and no match - matches all/prompt's true baseline
	// ("_user_color=$_green") from before host/prompt_colors gets sourced
	// and overrides it. The old per-name table that used to live here is
	// gone; that's what a promptbuilder-colors file is for now.
	return sgrGreen
}

func hostColor(hostname string) string {
	if sgr, ok := configuredColor("host", hostname); ok {
		return sgr
	}
	// Same reasoning as userColor: all/prompt's true baseline
	// ("_host_color=$_green") before host/prompt_colors overrides it.
	return sgrGreen
}

// gitInfo describes what the single `git status` call found.
// Fields are exported so this survives the daemon's JSON round-trip
// (encoding/json silently drops unexported fields).
type gitInfo struct {
	Branch   string
	Detached bool
	Staged   bool // index differs from HEAD (green "+" in the original)
	Unstaged bool // worktree differs from index, or untracked files (red "*")
	Ahead    int
	Behind   int
	HasUp    bool // has an upstream configured, i.e. ahead/behind are meaningful
}

// gitResult is the three-state answer buildPrompt actually needs: not a
// repo at all (show nothing, same as always), a repo with a successful
// fetch (show Info normally), or a repo where every fetch attempt has
// failed so far (show Error, so a real problem is visible instead of
// silently looking identical to "not a repo"). Fields are exported for the
// same JSON round-trip reason as gitInfo.
type gitResult struct {
	IsRepo bool
	OK     bool
	Info   gitInfo
	Error  string
}

// defaultGitTimeout is generous on purpose: it costs nothing for a normal
// repo (git status still returns in tens of ms), but on something like the
// Linux kernel tree - hundreds of thousands of files - a single `git
// status` can legitimately take several seconds even with nothing wrong.
// Killing it prematurely wouldn't just lose that one call: since a timeout
// self-heals by retrying (see refresh/getOrCreate in daemon.go), a repo
// that's genuinely always slower than the timeout would retry forever
// without ever actually finishing. Overridable per environment since
// "generous enough" depends on repo size and disk speed.
const defaultGitTimeout = 5 * time.Second

func gitTimeout() time.Duration {
	return envDuration("PROMPTBUILDER_GIT_TIMEOUT", defaultGitTimeout)
}

// directGitResult is the non-daemon path: used by the client's fallback
// when the daemon is unreachable, and always by --debug/--bench so they
// measure/show the real thing. findRepoRoot (daemon.go, pure Go, no
// subprocess) runs first so "not a repo" never even forks git, and - more
// importantly - so a subsequent git failure is unambiguously a real error
// rather than possibly just meaning "wasn't a repo anyway".
func directGitResult(dir string) gitResult {
	checkDir := dir
	if checkDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			checkDir = cwd
		}
	}
	root, isRepo := findRepoRoot(checkDir)
	if !isRepo {
		return gitResult{}
	}
	gi, ok, errMsg := runGitStatus(root)
	return gitResult{IsRepo: true, OK: ok, Info: gi, Error: errMsg}
}

// runGitStatus returns the repo state needed for the prompt, for the
// repository at dir. A single `git status --porcelain=v2 --branch` call
// covers branch name, staged vs. unstaged dirty state, and ahead/behind-
// upstream counts, which is why this is fast compared to the old
// __git_ps1-based prompt (that script forks git several times per render:
// rev-parse, symbolic-ref, describe, diff-index, ls-files, rev-list, ...).
// The daemon (daemon.go) calls this same function to refresh its cache.
// The bool/string result is (success, "") or (failure, error detail) -
// callers only reach this once they already know dir is a real repo (via
// findRepoRoot), so a failure here is always a genuine error, never "turns
// out this wasn't a repo".
func runGitStatus(dir string) (gitInfo, bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v2", "--branch")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return gitInfo{}, false, fmt.Sprintf("timed out after %s", gitTimeout())
		}
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[:i] // keep it to one line - this becomes part of the prompt
			}
			return gitInfo{}, false, msg
		}
		return gitInfo{}, false, err.Error()
	}

	var gi gitInfo
	oid := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			gi.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.oid "):
			oid = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "# branch.ab "):
			gi.HasUp = true
			for _, f := range strings.Fields(strings.TrimPrefix(line, "# branch.ab ")) {
				n, _ := strconv.Atoi(strings.TrimLeft(f, "+-"))
				if strings.HasPrefix(f, "+") {
					gi.Ahead = n
				} else if strings.HasPrefix(f, "-") {
					gi.Behind = n
				}
			}
		case line == "" || strings.HasPrefix(line, "#"):
			// ignore (blank line, or other header lines like branch.upstream)
		case strings.HasPrefix(line, "? "):
			gi.Unstaged = true // untracked file
		case strings.HasPrefix(line, "u "):
			gi.Unstaged = true // unmerged/conflict - surfaced as dirty for now
		case strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 "):
			fields := strings.Fields(line)
			if len(fields) >= 2 && len(fields[1]) == 2 {
				xy := fields[1]
				if xy[0] != '.' {
					gi.Staged = true
				}
				if xy[1] != '.' {
					gi.Unstaged = true
				}
			}
		}
	}

	gi.Detached = gi.Branch == "(detached)"
	if gi.Detached && len(oid) >= 7 {
		gi.Branch = oid[:7]
	}
	return gi, true, ""
}

func displayPath(home, cwd string) string {
	if cwd == home {
		return "~"
	}
	if strings.HasPrefix(cwd, home+"/") {
		return "~" + strings.TrimPrefix(cwd, home)
	}
	return cwd
}

// buildPrompt renders the full prompt, markers and all, for the given
// exit code of the previously run command. useDaemon controls whether git
// status goes through the caching daemon (the live-PS1 render path) or is
// always fetched directly (--debug/--bench, which are diagnostic tools and
// should measure/show the real thing, not a cached answer).
func buildPrompt(exitCode int, useDaemon bool) string {
	// Deliberately not os/user.Current(): it pulls in cgo for NSS lookups,
	// which makes the binary dynamically link against libc and pay dynamic
	// linker + NSS resolution cost on every single fork+exec. A bash shell
	// always exports $USER/$HOME already, and os.Getuid() is a plain
	// syscall - so there's no need for it here.
	username := os.Getenv("USER")
	uid := os.Getuid()
	home := os.Getenv("HOME")

	hostname, _ := os.Hostname()
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "?"
	}

	var sb strings.Builder

	sb.WriteString(esc(userColor(username, uid)))
	sb.WriteString(username + "@")
	sb.WriteString(reset)
	sb.WriteString(esc(hostColor(hostname)))
	sb.WriteString(hostname)
	sb.WriteString(reset)
	sb.WriteString(esc(userColor(username, uid)))
	sb.WriteString(":" + displayPath(home, cwd))
	sb.WriteString(reset)
	sb.WriteString("\n")

	// $debian_chroot is presence-checked (shown even if set-but-empty), same
	// as the original's "${debian_chroot+chroot: $debian_chroot}".
	if v, set := os.LookupEnv("debian_chroot"); set {
		sb.WriteString("chroot: " + v + "\n")
	}

	fetchGit := func() gitResult { return directGitResult("") }
	if useDaemon {
		fetchGit = resolveGit
	}
	if gr := fetchGit(); !gr.IsRepo {
		// not a repo - show nothing, same as always
	} else if !gr.OK {
		// A repo, but every fetch attempt so far has failed - surface it
		// instead of silently looking identical to "not a repo", which is
		// exactly what made the underlying bug confusing to notice.
		sb.WriteString("git: ")
		sb.WriteString(esc(sgrRed))
		sb.WriteString("error: " + gr.Error)
		sb.WriteString(reset)
		sb.WriteString("\n")
	} else {
		gi := gr.Info
		branchSGR := sgrGreen // matches the original's ok_color for a real branch
		if gi.Detached {
			branchSGR = sgrRed // matches the original's bad_color when detached
		}
		sb.WriteString("git: ")
		sb.WriteString(esc(branchSGR))
		sb.WriteString(gi.Branch)
		sb.WriteString(reset)
		// The original inserts exactly one space before the flags block as a
		// whole (only if there's anything to show), never between the
		// individual flags themselves - i.e. "master *>", not "master * >".
		if gi.Staged || gi.Unstaged || gi.HasUp {
			sb.WriteString(" ")
		}
		if gi.Staged {
			sb.WriteString(esc(sgrGreen)) // "+", same as original's ok_color
			sb.WriteString("+")
			sb.WriteString(reset)
		}
		if gi.Unstaged {
			sb.WriteString(esc(sgrRed)) // "*", same as original's bad_color
			sb.WriteString("*")
			sb.WriteString(reset)
		}
		if gi.HasUp {
			switch {
			case gi.Ahead > 0 && gi.Behind > 0:
				sb.WriteString("<>")
			case gi.Ahead > 0:
				sb.WriteString(">")
			case gi.Behind > 0:
				sb.WriteString("<")
			default:
				sb.WriteString("=")
			}
		}
		sb.WriteString("\n")
	}

	// $GOPATH: shown only if non-empty and not the default $HOME/.go,
	// matching the original's _get_go_PROMPT.
	if gopath := os.Getenv("GOPATH"); gopath != "" && gopath != home+"/.go" {
		sb.WriteString("go: " + gopath + "\n")
	}

	// $TOOLCHAIN_PREFIX, $DOCKER_HOST, $PROMPT_EXTRA: all presence-checked
	// and shown verbatim (or prefixed), same as the original.
	if v, set := os.LookupEnv("TOOLCHAIN_PREFIX"); set {
		sb.WriteString(v + "\n")
	}
	if v, set := os.LookupEnv("DOCKER_HOST"); set {
		sb.WriteString("DOCKER_HOST=" + v + "\n")
	}
	if v, set := os.LookupEnv("PROMPT_EXTRA"); set {
		sb.WriteString(v + "\n")
	}

	exitSGR := sgrBoldGreen
	if exitCode != 0 {
		exitSGR = sgrBoldRed
	}
	sb.WriteString(esc(exitSGR))
	sb.WriteString("\xc2\xbb ") // "»"
	sb.WriteString(reset)

	return sb.String()
}

// stripMarkers removes the \x01/\x02 zero-width markers but leaves the
// ANSI color codes in place - useful for testing directly in a terminal,
// where those markers serve no purpose (they only matter inside PS1).
func stripMarkers(s string) string {
	s = strings.ReplaceAll(s, "\x01", "")
	s = strings.ReplaceAll(s, "\x02", "")
	return s
}

// stripAllEscapes removes both the zero-width markers and the ANSI color
// codes themselves, leaving plain text - so content can be checked
// independently of whether any escape sequence is rendering correctly.
func stripAllEscapes(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0x01, 0x02:
			continue
		case 0x1b:
			// skip the ESC[...m sequence
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && s[j] != 'm' {
					j++
				}
			}
			i = j
			continue
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// visualize makes every non-printing byte visible, caret-notation style
// (like `cat -v`), so escape-sequence problems can be seen without piping
// through an external tool.
func visualize(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == 0x1b:
			out.WriteString("\\e")
		case b == '\n':
			out.WriteByte('\n')
		case b < 0x20:
			out.WriteByte('^')
			out.WriteByte(b + 64)
		case b == 0x7f:
			out.WriteString("^?")
		default:
			out.WriteByte(b)
		}
	}
	return out.String()
}

const usage = `promptbuilder - generates this shell's colored PS1, called fresh on each prompt render

Usage:
  promptbuilder <exit_code>     Print the prompt. exit_code is $? of the previous
                                 command and is required - it's what colors the
                                 prompt symbol, and a missing value usually means
                                 PROMPT_COMMAND was wired up wrong.

                                 Output adapts to where it's going: run directly in
                                 a terminal, you get plain visible colors - useful
                                 to eyeball what the prompt would actually look
                                 like. Captured via "$(...)" (as PS1 does), it
                                 includes \x01/\x02 readline markers so bash's
                                 cursor-position math stays correct.

  promptbuilder --plain <exit_code>
                                 Force the plain (no-marker) form even when output
                                 is being captured, e.g. piping to a file or less.
  promptbuilder --test <exit_code>
                                 Like the plain form, but fenced with blank lines
                                 and a boundary so the rendered prompt can't be
                                 mistaken for the real prompt line you ran it from.
  promptbuilder --debug <exit_code>
                                 Diagnostic dump: the raw output with every control
                                 character made visible (^A, \e, ^B, ...), plus a
                                 fully plain-text version with all escapes stripped,
                                 so you can tell "escapes are eating my output"
                                 apart from "the content itself is wrong".
  promptbuilder --bench <exit_code> [iterations]
                                 Time how long generating the prompt takes,
                                 in-process (default 200 iterations, reports
                                 min/avg/max). Note: this does NOT include the
                                 fork+exec cost of invoking the promptbuilder
                                 binary itself, which is unavoidable overhead
                                 in real PS1 usage - only the shell-level
                                 comparison against the old bash prompt
                                 captures that part.
  promptbuilder -h | --help     Show this help.

Bash integration:
  PROMPT_COMMAND='PS1="$(promptbuilder $?)"'
`

// isTerminalStdout reports whether stdout is an interactive terminal rather
// than a pipe or command-substitution capture. No terminal library needed -
// character devices are what a real tty shows up as via Stat().
func isTerminalStdout() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func main() {
	// Hidden mode, spawned by the client (client.go's spawnDaemon) and
	// never invoked directly by a user - checked before the normal flag
	// parsing below since it takes no exit_code argument.
	if len(os.Args) > 1 && os.Args[1] == "--daemon" {
		runDaemon()
		return
	}

	mode := "normal"
	var codeArg, itersArg string
	haveCode := false

	for _, a := range os.Args[1:] {
		switch a {
		case "-h", "--help":
			fmt.Print(usage)
			return
		case "--plain":
			mode = "plain"
		case "--test":
			mode = "test"
		case "--debug":
			mode = "debug"
		case "--bench":
			mode = "bench"
		default:
			if !haveCode {
				codeArg = a
				haveCode = true
			} else {
				itersArg = a
			}
		}
	}

	if !haveCode {
		fmt.Fprintln(os.Stderr, "promptbuilder: missing required exit_code argument (pass $? from PROMPT_COMMAND)")
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	exitCode, err := strconv.Atoi(codeArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptbuilder: exit_code must be an integer, got %q\n", codeArg)
		os.Exit(1)
	}

	if mode == "bench" {
		iters := 200
		if itersArg != "" {
			if v, err := strconv.Atoi(itersArg); err == nil && v > 0 {
				iters = v
			}
		}
		runBench(exitCode, iters)
		return
	}

	// --debug always fetches git status directly, so it shows/diagnoses
	// the real thing rather than a cached answer.
	out := buildPrompt(exitCode, mode != "debug")

	switch mode {
	case "plain":
		fmt.Print(stripMarkers(out))
	case "test":
		header := strings.Repeat("─", 20) + " promptbuilder --test " + strings.Repeat("─", 20)
		fmt.Println()
		fmt.Println(header)
		fmt.Println()
		fmt.Print(stripMarkers(out))
		fmt.Println() // the real prompt has no trailing newline; add one so the boundary below doesn't run into it
		fmt.Println()
		fmt.Println(strings.Repeat("─", utf8.RuneCountInString(header)))
		fmt.Println()
	case "debug":
		fmt.Println("=== raw output, control chars made visible ===")
		fmt.Println(visualize(out))
		fmt.Println("=== plain text, all escapes stripped ===")
		fmt.Println(stripAllEscapes(out))
		fmt.Println("=== legend ===")
		fmt.Println("^A / ^B  = \\x01 / \\x02, readline's zero-width markers (only meaningful inside PS1)")
		fmt.Println("\\e[Nm    = ANSI color/style codes (see below for which N maps to what)")
		exitLabel := sgrBoldGreen + " (green)"
		if exitCode != 0 {
			exitLabel = sgrBoldRed + " (red)"
		}
		fmt.Printf("this run: user color=%s host color=%s exit color=%s\n",
			userColor(os.Getenv("USER"), os.Getuid()), hostColor(mustHostname()), exitLabel)
	default:
		if isTerminalStdout() {
			fmt.Print(stripMarkers(out))
		} else {
			fmt.Print(out)
		}
	}
}

func mustHostname() string {
	h, _ := os.Hostname()
	return h
}

// runBench times buildPrompt in-process over n iterations and reports
// min/avg/max. It intentionally does not measure the fork+exec cost of
// running the promptbuilder binary itself - that overhead is unavoidable
// and best measured at the shell level, comparing against the old prompt.
// It always fetches git status directly (bypassing the daemon) so it
// measures the real per-call cost, not a cached answer.
func runBench(exitCode, n int) {
	durs := make([]time.Duration, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		buildPrompt(exitCode, false)
		durs[i] = time.Since(t0)
	}
	total := time.Since(start)

	min, max, sum := durs[0], durs[0], time.Duration(0)
	for _, d := range durs {
		sum += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	avg := sum / time.Duration(n)

	fmt.Printf("promptbuilder --bench: %d iterations, %v total\n", n, total)
	fmt.Printf("  avg=%v  min=%v  max=%v\n", avg, min, max)
	fmt.Println("(in-process only - does not include fork+exec of the promptbuilder binary itself)")
}
