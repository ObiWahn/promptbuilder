# promptbuilder

A small Go program that builds a colored bash `PS1`, written to replace a
slow `__git_ps1`-based prompt. The old prompt forked `git` up to ~7-9 times
per render (two `diff --quiet` tree-walks, an untracked-file scan, stash
and conflict checks, an upstream `rev-list`, ...); this does the
equivalent with a single `git status --porcelain=v2 --branch` call, plus
an optional background daemon that caches that call per repository so
most renders don't even need to invoke `git` at all.

Measured end-to-end in a real repo (187 files, 134 commits):

| | avg per prompt render |
|---|---|
| old bash prompt (`__git_ps1`) | ~120ms |
| `promptbuilder`, direct (no daemon) | ~50ms |
| `promptbuilder`, daemon-served | ~3-5ms |

## What it shows

```
user@host:path
git: branch [+][*][<\|>\|<>\|=]
» (green if the last command exited 0, red otherwise)
```

- `user@host` and the path are colored per a configurable table (see
  Configuration below), same idea as the original's per-user/per-host
  colors.
- The git line only appears inside a git repository. `+` means staged
  changes, `*` means unstaged changes or untracked files, and one of
  `<` `>` `<>` `=` shows behind/ahead/diverged/in-sync with the upstream
  branch. Branch name is green normally, red if `HEAD` is detached.
- If git status genuinely fails for a tracked repo (not just "this isn't
  a repo" - a real error: timeout, lock contention, a corrupted index,
  ...), the git line shows `git: error: <detail>` instead of silently
  disappearing, so a real problem is visible rather than looking
  identical to "not a repo".
- A few optional lines, shown only if set: `$debian_chroot`, `$GOPATH`
  (unless it's the default `$HOME/.go`), `$TOOLCHAIN_PREFIX`,
  `$DOCKER_HOST`, `$PROMPT_EXTRA`.

## Build / install

```
make build    # ./promptbuilder, for poking around locally
make install  # $BINDIR/promptbuilder, default $BINDIR = ~/.bin
make fmt      # gofmt + go vet
make test     # go test ./...
make clean    # remove build artifacts
```

Always use the installed binary (`make install`, with `~/.bin` on your
`$PATH`) rather than running `./promptbuilder` from this directory for
real use - the daemon re-execs whatever path it was originally invoked
from, so running the local build makes a background daemon depend on a
file you're actively rebuilding.

If a daemon from a previous build is still running, `make restart-daemon`
kills it so a fresh one (matching whatever you just built) picks up on
the next prompt render. Installing a new binary alone doesn't affect an
already-running daemon process.

## Usage

```
promptbuilder <exit_code>              Print the prompt (for PS1 - see below)
promptbuilder --plain <exit_code>      Same, without PS1's zero-width markers
promptbuilder --test <exit_code>       Like --plain, fenced so it can't be
                                        mistaken for your real prompt line
promptbuilder --debug <exit_code>      Diagnostic dump of raw bytes + a
                                        plain-text fallback + a legend
promptbuilder --bench <exit_code> [N]  Time prompt generation in-process
promptbuilder -h | --help
```

`exit_code` (`$?` of the previous command) is required - the binary
refuses to run without it rather than silently defaulting to 0.

Output adapts to where it's going: run directly in a terminal you get
plain visible colors; captured via `$(...)` (as `PS1` does) it includes
the `\x01`/`\x02` readline markers bash needs for correct cursor-position
math. `--debug` and `--bench` always fetch git status directly, bypassing
the daemon, since they're meant to show/measure the real thing.

### Wiring it into bash

```bash
PROMPT_COMMAND='PS1="$(promptbuilder $?)"'
```

To try it in your current shell first, without touching `~/.bashrc.d`:

```bash
source /path/to/bash-prompt/try-prompt.sh
```

This defines `promptbuilder_activate` / `promptbuilder_restore` so you
can toggle back and forth as many times as you like in that shell.

## The caching daemon

`promptbuilder --daemon` is a hidden mode, spawned automatically and never
run directly. The normal CLI is the client: it tries to connect to the
daemon's Unix socket first, and only falls back to running `git status`
itself if no daemon accepts the connection - the daemon is a pure
optimization, never a hard dependency. A daemon that accepts but doesn't
answer within 500ms gets the branch-only `…` form described below rather
than a direct `git status`, which would most likely be just as slow.

- Resolves each repo's root itself (pure Go directory walk for `.git`, no
  git subprocess) and caches status per root.
- Refreshes instantly via `inotify` on the git dir (`HEAD`, `index`,
  `packed-refs`, `config`) and every directory under `refs/` (local
  branches including `feature/x`-style names, `refs/remotes/*` so
  push/fetch update ahead/behind, tags). Directories are watched rather
  than the files themselves, since git replaces those files by renaming
  `X.lock` over them. Linked worktrees (`.git` as a file) are followed to
  their git dir and common dir. A periodic tick
  (`PROMPTBUILDER_REFRESH_INTERVAL`, default `30s`) remains as the fallback
  for plain unstaged edits that never touch `.git/`.
- A change that lands while a refresh is already running is never
  dropped: the running `git status` is simply repeated once it finishes.
- Each request first processes any inotify events already queued (the
  command that just finished produced them), then waits up to
  `PROMPTBUILDER_WAIT_BUDGET` (default `300ms`) for a running refresh, so
  the prompt shows the result of the command that just ran rather than
  the one before. If git is slower than that, the previous state is
  shown; for a repo seen for the first time, the branch is read straight
  from `HEAD` and the flags show as `…` until the first status is in.
- The daemon runs `git --no-optional-locks status`, so it never rewrites
  `.git/index` or competes with your own git commands for `index.lock`.
- Evicts a repo's cache and watches after `PROMPTBUILDER_IDLE_EVICT`
  (default `15m`) of no requests, and exits itself once nothing is left
  to track - a new daemon spawns on demand the next time one's needed.
- A failed fetch (timeout, lock contention, a huge repo just taking a
  while) never gets stuck: it doesn't overwrite good cached data, and it
  leaves the repo's refresh timer stale so the very next tick (~1s)
  retries, instead of the repo appearing broken for a full refresh
  interval or longer.
- `PROMPTBUILDER_GIT_TIMEOUT` (default `5s`) bounds how long a single
  `git status` call is allowed to take before being treated as failed
  (and retried per the point above). The default is generous on purpose:
  it costs nothing when git is fast, but a very large repo can
  legitimately take several seconds.

Singleton safety (only one daemon ever runs) comes from the daemon's own
`Listen()` on the socket path being the actual lock - see
`~/remote-repos/patterns/unix-socket-daemon-spawning.md` for the general
pattern this follows, independent of this project.

## Configuration

### Colors

User/host colors come from, in this order (first match wins, checked per
line - a user file doesn't need to repeat everything a system file
already covers):

1. `~/.config/promptbuilder/colors` (your personal overrides)
2. `/etc/promptbuilder/colors`, or `/usr/local/etc/promptbuilder/colors`
   if that file doesn't exist (the machine's defaults)
3. A built-in fallback table if neither file exists

Format, one rule per line:

```
# kind   glob-pattern   color
user     root           red
user     *admin         red
host     agents-vm      yellow
```

`kind` is `user` or `host`; colors are `black red green yellow blue
magenta cyan white default` (`default`/`reset` = terminal's own color).
Blank lines and lines starting with `#` are ignored.

### Daemon timing

Read once at daemon startup, no live reload:

```
PROMPTBUILDER_REFRESH_INTERVAL=30s   # periodic fallback refresh
PROMPTBUILDER_IDLE_EVICT=15m         # per-repo cache/watch eviction
PROMPTBUILDER_GIT_TIMEOUT=5s         # per-call timeout before retrying
PROMPTBUILDER_WAIT_BUDGET=300ms      # max wait per request for a running refresh
```

## Files

```
main.go         CLI, prompt rendering, direct (non-daemon) git status
daemon.go       the caching daemon: repo cache, inotify, eviction
client.go       daemon client + the detached-spawn logic
colors.go       user/host color config loading
GNUmakefile     build / install / fmt / test / restart-daemon / clean
try-prompt.sh   session-scoped way to try the prompt without editing bashrc
```
