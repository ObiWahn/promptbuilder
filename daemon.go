package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	defaultRefreshInterval = 30 * time.Second
	defaultIdleEvict       = 15 * time.Minute
)

// daemonResponse is what the daemon sends back for one request - the wire
// form of gitResult (main.go), which is what buildPrompt actually needs.
type daemonResponse struct {
	IsRepo bool    `json:"isRepo"`
	OK     bool    `json:"ok"`
	Error  string  `json:"error"`
	Info   gitInfo `json:"info"`
}

func socketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "promptbuilder.sock")
	}
	return fmt.Sprintf("/tmp/promptbuilder-%d.sock", os.Getuid())
}

// findRepoRoot walks up from dir looking for a .git entry, without
// invoking git at all - the daemon's hot path stays free of subprocesses
// for anything except the actual `git status` refresh.
func findRepoRoot(dir string) (string, bool) {
	d := dir
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// repoState is the cached status for one repository, plus the bookkeeping
// needed to refresh and evict it.
type repoState struct {
	mu          sync.Mutex
	info        gitInfo
	found       bool   // a fetch has EVER succeeded for this repo
	lastError   string // most recent failure detail, shown only while found is still false
	lastAccess  time.Time
	lastRefresh time.Time
	watches     []int
	inFlight    bool // a fetch is already running - see doRefresh
}

func (rs *repoState) snapshot() (gitInfo, bool, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.info, rs.found, rs.lastError
}

// doRefresh runs the actual git status fetch for root, guarded so
// overlapping triggers (a periodic tick firing while an inotify-triggered
// fetch on a slow/huge repo is still running, or vice versa) don't pile up
// concurrent git processes for the same repo - they just skip and let the
// one already in flight finish, however long that takes. On failure, the
// previous good cache and lastRefresh are left untouched (see refresh's
// former comment, now here): a failure past findRepoRoot's earlier check
// is always transient, so leaving lastRefresh stale makes housekeep retry
// on its very next tick instead of this repo getting stuck.
func (rs *repoState) doRefresh(root string) {
	rs.mu.Lock()
	if rs.inFlight {
		rs.mu.Unlock()
		return
	}
	rs.inFlight = true
	rs.mu.Unlock()

	gi, ok, errMsg := runGitStatus(root)

	rs.mu.Lock()
	rs.inFlight = false
	if ok {
		rs.info = gi
		rs.found = ok
		rs.lastRefresh = time.Now()
		rs.lastError = ""
	} else {
		// Record the error even though found/info/lastRefresh are left
		// alone - it's only ever shown while found is still false (see
		// handleConn), i.e. exactly the "never successfully fetched yet"
		// case this whole mechanism exists for.
		rs.lastError = errMsg
	}
	rs.mu.Unlock()
}

func (rs *repoState) touch() {
	rs.mu.Lock()
	rs.lastAccess = time.Now()
	rs.mu.Unlock()
}

// daemonState is the whole daemon's in-memory state: one shared inotify
// instance and a map of actively-watched repos.
type daemonState struct {
	mu              sync.Mutex
	repos           map[string]*repoState
	wdToRoot        map[int]string
	inotifyFd       int
	refreshInterval time.Duration
	idleEvict       time.Duration
	started         time.Time
}

func envDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// watchPaths returns the small, fixed set of .git-internal paths that
// matter for this prompt - never the working tree itself, so fd usage
// stays flat no matter how large the repo is.
func watchPaths(root string) []string {
	return []string{
		filepath.Join(root, ".git", "HEAD"),
		filepath.Join(root, ".git", "index"),
		filepath.Join(root, ".git", "refs", "heads"),
		filepath.Join(root, ".git", "refs", "tags"),
		filepath.Join(root, ".git", "packed-refs"),
	}
}

const inotifyMask = syscall.IN_MODIFY | syscall.IN_CREATE | syscall.IN_DELETE |
	syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM

// addWatches must be called with d.mu held.
func (d *daemonState) addWatches(root string) []int {
	var wds []int
	for _, p := range watchPaths(root) {
		wd, err := syscall.InotifyAddWatch(d.inotifyFd, p, inotifyMask)
		if err != nil {
			continue // e.g. no packed-refs yet - fine, the periodic tick covers it
		}
		d.wdToRoot[wd] = root
		wds = append(wds, wd)
	}
	return wds
}

// removeWatches must be called with d.mu held.
func (d *daemonState) removeWatches(wds []int) {
	for _, wd := range wds {
		syscall.InotifyRmWatch(d.inotifyFd, uint32(wd))
		delete(d.wdToRoot, wd)
	}
}

// refresh re-fetches root's status if that repo is still being tracked.
// findRepoRoot already confirmed a .git entry exists before a repo is ever
// tracked, so a failure here is always transient (timeout, lock
// contention, a huge repo just taking a while, ...), never "turns out
// this isn't a repo after all" - see doRefresh for how that's handled.
func (d *daemonState) refresh(root string) {
	d.mu.Lock()
	rs, exists := d.repos[root]
	d.mu.Unlock()
	if !exists {
		return
	}
	rs.doRefresh(root)
}

// getOrCreate returns the cached state for root, doing a synchronous first
// fetch and setting up watches the first time this repo is seen.
func (d *daemonState) getOrCreate(root string) *repoState {
	d.mu.Lock()
	if rs, ok := d.repos[root]; ok {
		d.mu.Unlock()
		rs.touch()
		return rs
	}
	rs := &repoState{lastAccess: time.Now()}
	d.repos[root] = rs
	wds := d.addWatches(root)
	d.mu.Unlock()

	rs.mu.Lock()
	rs.watches = wds
	rs.mu.Unlock()

	rs.doRefresh(root)
	return rs
}

func (d *daemonState) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return
	}
	cwd := sc.Text()

	resp := daemonResponse{}
	if root, isRepo := findRepoRoot(cwd); isRepo {
		resp.IsRepo = true
		rs := d.getOrCreate(root)
		info, ok, errMsg := rs.snapshot()
		resp.OK = ok
		resp.Info = info
		if !ok {
			resp.Error = errMsg
		}
	}
	json.NewEncoder(conn).Encode(resp)
}

// housekeep runs once a second: evicts idle repos and refreshes any repo
// whose cache has gone stale past the refresh interval (this is only a
// fallback for plain working-tree edits that inotify can't see - branch/
// staged/upstream changes are already caught instantly via inotifyLoop).
// Exits the whole daemon once nothing is left to track.
func (d *daemonState) housekeep(listener net.Listener) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		d.mu.Lock()
		for root, rs := range d.repos {
			rs.mu.Lock()
			idle := now.Sub(rs.lastAccess) > d.idleEvict
			stale := now.Sub(rs.lastRefresh) > d.refreshInterval
			wds := rs.watches
			rs.mu.Unlock()

			if idle {
				d.removeWatches(wds)
				delete(d.repos, root)
				continue
			}
			if stale {
				go d.refresh(root)
			}
		}
		empty := len(d.repos) == 0
		d.mu.Unlock()

		// A freshly-started daemon is empty too (nothing has connected to
		// it yet) - only treat "empty" as "done, nothing left to track" once
		// it's had at least one idleEvict window to actually receive a
		// request. Without this grace period the daemon exits on its very
		// first tick, before the client that spawned it (which never
		// queries it - just spawns and moves on) has any chance to use it.
		if empty && now.Sub(d.started) > d.idleEvict {
			listener.Close()
			os.Exit(0)
		}
	}
}

// inotifyLoop reads raw inotify events and triggers an immediate refresh
// for whichever repo owns the watch descriptor that fired.
func (d *daemonState) inotifyLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(d.inotifyFd, buf)
		if err != nil || n <= 0 {
			return
		}
		offset := 0
		for offset+syscall.SizeofInotifyEvent <= n {
			raw := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			wd := int(raw.Wd)

			d.mu.Lock()
			root, ok := d.wdToRoot[wd]
			d.mu.Unlock()
			if ok {
				go d.refresh(root)
			}

			offset += syscall.SizeofInotifyEvent + int(raw.Len)
		}
	}
}

// runDaemon is the entry point for `promptbuilder --daemon`: a hidden mode
// spawned by the client, never invoked directly by a user. See
// ~/remote-repos/patterns/unix-socket-daemon-spawning.md for the general
// pattern this follows.
func runDaemon() {
	path := socketPath()

	listener, err := net.Listen("unix", path)
	if err != nil {
		// Something's already bound here. Dial it to find out whether a
		// real daemon is alive (exit quietly, we lost the race) or this is
		// a stale socket left behind by a crash (clean up and retry once).
		if c, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond); dialErr == nil {
			c.Close()
			return
		}
		os.Remove(path)
		listener, err = net.Listen("unix", path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "promptbuilder --daemon: could not bind %s: %v\n", path, err)
			os.Exit(1)
		}
	}

	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptbuilder --daemon: inotify_init1: %v\n", err)
		os.Exit(1)
	}

	d := &daemonState{
		repos:           make(map[string]*repoState),
		wdToRoot:        make(map[int]string),
		inotifyFd:       fd,
		refreshInterval: envDuration("PROMPTBUILDER_REFRESH_INTERVAL", defaultRefreshInterval),
		idleEvict:       envDuration("PROMPTBUILDER_IDLE_EVICT", defaultIdleEvict),
		started:         time.Now(),
	}

	go d.inotifyLoop()
	go d.housekeep(listener)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go d.handleConn(conn)
	}
}
