package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	defaultRefreshInterval = 30 * time.Second
	defaultIdleEvict       = 15 * time.Minute

	// defaultWaitBudget: how long a request waits for a refresh that is
	// already running (or was just triggered by inotify) before answering
	// from the cache anyway. A prompt is rendered right after a command, so
	// that's exactly when a refresh is most likely in flight - waiting a bit
	// means the answer reflects the command that just ran, not the one
	// before it. Small repos finish well within this; huge ones get the
	// previous state instead of blocking the prompt.
	defaultWaitBudget = 300 * time.Millisecond
)

// daemonResponse is what the daemon sends back for one request - the wire
// form of gitResult (main.go), which is what buildPrompt actually needs.
type daemonResponse struct {
	IsRepo  bool    `json:"isRepo"`
	OK      bool    `json:"ok"`
	Pending bool    `json:"pending"` // first fetch still running; Info has only the branch from HEAD
	Error   string  `json:"error"`
	Info    gitInfo `json:"info"`
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

// gitDirs resolves root's git directory and common directory without
// invoking git. In a plain repo both are root/.git. In a linked worktree
// (or submodule) .git is a file pointing elsewhere ("gitdir: ..."), and a
// worktree's gitDir holds only its own HEAD/index - refs, packed-refs and
// config live in the common dir it names in its "commondir" file.
func gitDirs(root string) (gitDir, commonDir string) {
	gitDir = filepath.Join(root, ".git")
	if fi, err := os.Stat(gitDir); err == nil && !fi.IsDir() {
		if b, err := os.ReadFile(gitDir); err == nil {
			if p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:"); ok {
				p = strings.TrimSpace(p)
				if !filepath.IsAbs(p) {
					p = filepath.Join(root, p)
				}
				gitDir = filepath.Clean(p)
			}
		}
	}
	commonDir = gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		p := strings.TrimSpace(string(b))
		if !filepath.IsAbs(p) {
			p = filepath.Join(gitDir, p)
		}
		commonDir = filepath.Clean(p)
	}
	return gitDir, commonDir
}

// headInfo reads just the branch (or detached commit) straight from HEAD -
// pure Go, no subprocess - for showing something useful while the real
// status isn't available yet.
func headInfo(root string) gitInfo {
	gitDir, _ := gitDirs(root)
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return gitInfo{}
	}
	head := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		return gitInfo{Branch: strings.TrimPrefix(ref, "refs/heads/")}
	}
	if len(head) >= 7 {
		return gitInfo{Branch: head[:7], Detached: true}
	}
	return gitInfo{}
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
	inFlight    bool          // a fetch is running - see requestRefresh
	dirty       bool          // a trigger arrived while inFlight; fetch once more afterwards
	settled     chan struct{} // closed once inFlight goes back to false
}

func (rs *repoState) snapshot() (gitInfo, bool, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.info, rs.found, rs.lastError
}

// requestRefresh starts a git status fetch for root in the background and
// returns immediately. Triggers that arrive while a fetch is already
// running (a burst of inotify events from one git command, a periodic tick
// on a slow repo) don't start another concurrent git process, but they
// aren't dropped either: they mark the state dirty, and the running fetch
// goes around once more when it's done, so the cache always ends up
// reflecting the latest change.
func (rs *repoState) requestRefresh(root string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.inFlight {
		rs.dirty = true
		return
	}
	rs.inFlight = true
	rs.settled = make(chan struct{})
	go rs.fetchLoop(root)
}

// fetchLoop runs git status until no new trigger arrived during the last
// run. On failure, the previous good cache and lastRefresh are left
// untouched: findRepoRoot already confirmed a .git entry exists, so a
// failure here is always transient (timeout, lock contention, a huge repo
// just taking a while, ...), and leaving lastRefresh stale makes housekeep
// retry on its very next tick instead of this repo getting stuck.
func (rs *repoState) fetchLoop(root string) {
	for {
		gi, ok, errMsg := runGitStatus(root)

		rs.mu.Lock()
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
		if rs.dirty {
			rs.dirty = false
			rs.mu.Unlock()
			continue
		}
		rs.inFlight = false
		close(rs.settled)
		rs.mu.Unlock()
		return
	}
}

// waitSettled blocks until no fetch is running for this repo, or until
// budget runs out, whichever comes first.
func (rs *repoState) waitSettled(budget time.Duration) {
	rs.mu.Lock()
	busy, settled := rs.inFlight, rs.settled
	rs.mu.Unlock()
	if !busy {
		return
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-settled:
	case <-timer.C:
	}
}

func (rs *repoState) touch() {
	rs.mu.Lock()
	rs.lastAccess = time.Now()
	rs.mu.Unlock()
}

// watch is one inotify watch descriptor. The same directory can matter to
// several tracked repos (a main checkout and its linked worktrees share
// the common dir), and inotify hands out the same descriptor for the same
// path, so a watch belongs to a set of roots and is only removed once none
// of them need it anymore.
type watch struct {
	path   string
	isRefs bool // somewhere under refs/: every entry matters, and new subdirs get watched too
	roots  map[string]bool
}

// daemonState is the whole daemon's in-memory state: one shared inotify
// instance and a map of actively-watched repos.
type daemonState struct {
	mu              sync.Mutex
	repos           map[string]*repoState
	watches         map[int]*watch
	inotifyFd       int
	refreshInterval time.Duration
	idleEvict       time.Duration
	waitBudget      time.Duration
	started         time.Time

	// evMu serializes reading the inotify fd together with dispatching
	// what was read, so that once a request's drainEvents returns, every
	// event the kernel had queued before it has been turned into a
	// refresh - see handleConn.
	evMu  sync.Mutex
	evBuf []byte
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

func newDaemonState() (*daemonState, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify_init1: %w", err)
	}
	return &daemonState{
		repos:           make(map[string]*repoState),
		watches:         make(map[int]*watch),
		inotifyFd:       fd,
		refreshInterval: envDuration("PROMPTBUILDER_REFRESH_INTERVAL", defaultRefreshInterval),
		idleEvict:       envDuration("PROMPTBUILDER_IDLE_EVICT", defaultIdleEvict),
		waitBudget:      envDuration("PROMPTBUILDER_WAIT_BUDGET", defaultWaitBudget),
		started:         time.Now(),
		evBuf:           make([]byte, 64*1024),
	}, nil
}

// Directories are watched, never the files in them: git replaces HEAD,
// index, refs etc. by writing X.lock and renaming it over X, which would
// silently end a watch on the old file after its first change. A
// directory watch survives that and reports the rename as IN_MOVED_TO X.
const inotifyMask = syscall.IN_CLOSE_WRITE | syscall.IN_CREATE | syscall.IN_DELETE |
	syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM

// gitDirFiles are the entries directly in a git dir (or common dir) that
// affect what the prompt shows. Everything else there (logs, objects,
// FETCH_HEAD, ORIG_HEAD, ...) is noise for this purpose.
var gitDirFiles = map[string]bool{
	"HEAD":        true,
	"index":       true,
	"packed-refs": true,
	"config":      true, // upstream configuration
}

func (w *watch) relevant(name string) bool {
	if strings.HasSuffix(name, ".lock") {
		return false // the rename to the final name is what counts
	}
	return w.isRefs || gitDirFiles[name]
}

// addWatch must be called with d.mu held.
func (d *daemonState) addWatch(root, path string, isRefs bool) (int, bool) {
	wd, err := syscall.InotifyAddWatch(d.inotifyFd, path, inotifyMask)
	if err != nil {
		return 0, false
	}
	w := d.watches[wd]
	if w == nil {
		w = &watch{path: path, isRefs: isRefs, roots: make(map[string]bool)}
		d.watches[wd] = w
	}
	w.roots[root] = true
	return wd, true
}

// addRefsWatches watches dir and every directory below it, so refs in
// nested namespaces (refs/heads/feature/x, refs/remotes/origin/main) are
// covered. Must be called with d.mu held.
func (d *daemonState) addRefsWatches(root, dir string) []int {
	var wds []int
	filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || !e.IsDir() {
			return nil
		}
		if wd, ok := d.addWatch(root, path, true); ok {
			wds = append(wds, wd)
		}
		return nil
	})
	return wds
}

// addWatches must be called with d.mu held. Only .git-internal
// directories are watched - never the working tree itself, so fd usage
// stays flat no matter how large the repo is.
func (d *daemonState) addWatches(root string) []int {
	gitDir, commonDir := gitDirs(root)
	var wds []int
	if wd, ok := d.addWatch(root, gitDir, false); ok {
		wds = append(wds, wd)
	}
	if commonDir != gitDir {
		if wd, ok := d.addWatch(root, commonDir, false); ok {
			wds = append(wds, wd)
		}
	}
	return append(wds, d.addRefsWatches(root, filepath.Join(commonDir, "refs"))...)
}

// removeWatches must be called with d.mu held.
func (d *daemonState) removeWatches(root string, wds []int) {
	for _, wd := range wds {
		w, ok := d.watches[wd]
		if !ok {
			continue
		}
		delete(w.roots, root)
		if len(w.roots) == 0 {
			syscall.InotifyRmWatch(d.inotifyFd, uint32(wd))
			delete(d.watches, wd)
		}
	}
}

// getOrCreate returns the cached state for root, setting up watches and
// starting a first fetch in the background the first time this repo is
// seen. It never blocks on git - callers wait via waitSettled, bounded.
func (d *daemonState) getOrCreate(root string) *repoState {
	d.mu.Lock()
	if rs, ok := d.repos[root]; ok {
		d.mu.Unlock()
		rs.touch()
		return rs
	}
	rs := &repoState{lastAccess: time.Now()}
	rs.watches = d.addWatches(root)
	d.repos[root] = rs
	d.mu.Unlock()

	rs.requestRefresh(root)
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
		// The command that just finished (the reason this prompt is being
		// rendered) has already queued its inotify events - make sure
		// they've triggered a refresh before deciding whether to wait.
		d.drainEvents()
		rs := d.getOrCreate(root)
		rs.waitSettled(d.waitBudget)
		info, ok, errMsg := rs.snapshot()
		resp.OK = ok
		resp.Info = info
		if !ok {
			resp.Error = errMsg
			if errMsg == "" {
				// Nothing has finished yet, neither success nor failure.
				resp.Pending = true
				resp.Info = headInfo(root)
			}
		}
	}
	json.NewEncoder(conn).Encode(resp)
}

// housekeep runs once a second: evicts idle repos and refreshes any repo
// whose cache has gone stale past the refresh interval (this is only a
// fallback for plain working-tree edits that inotify can't see - branch/
// staged/upstream changes are already caught instantly via inotify).
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
				d.removeWatches(root, wds)
				delete(d.repos, root)
				continue
			}
			if stale {
				rs.requestRefresh(root)
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

// waitReadable blocks until fd has data to read. Reports false if fd is
// no longer usable.
func waitReadable(fd int) bool {
	var set syscall.FdSet
	bitsPer := int(unsafe.Sizeof(set.Bits[0])) * 8
	for {
		set = syscall.FdSet{}
		set.Bits[fd/bitsPer] |= 1 << (uint(fd) % uint(bitsPer))
		_, err := syscall.Select(fd+1, &set, nil, nil, nil)
		if err == nil {
			return true
		}
		if err != syscall.EINTR {
			return false
		}
	}
}

// inotifyLoop waits for inotify events and dispatches them as they come.
// The fd is non-blocking so that handleConn can drain it too (see
// drainEvents); waiting happens here via select instead.
func (d *daemonState) inotifyLoop() {
	for waitReadable(d.inotifyFd) {
		d.drainEvents()
	}
}

// drainEvents reads every event currently queued on the inotify fd and
// triggers refreshes for the repos they belong to.
func (d *daemonState) drainEvents() {
	d.evMu.Lock()
	defer d.evMu.Unlock()
	for {
		n, err := syscall.Read(d.inotifyFd, d.evBuf)
		if err != nil || n <= 0 {
			return // EAGAIN: queue is empty
		}
		d.dispatch(d.evBuf[:n])
	}
}

// dispatch handles one buffer of raw inotify events. Must be called with
// d.evMu held.
func (d *daemonState) dispatch(buf []byte) {
	touched := make(map[string]bool)
	for offset := 0; offset+syscall.SizeofInotifyEvent <= len(buf); {
		raw := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		nameStart := offset + syscall.SizeofInotifyEvent
		offset = nameStart + int(raw.Len)
		name := strings.TrimRight(string(buf[nameStart:offset]), "\x00")
		wd := int(raw.Wd)

		d.mu.Lock()
		switch {
		case raw.Mask&syscall.IN_Q_OVERFLOW != 0:
			// Events were lost - no telling which repos changed.
			for root := range d.repos {
				touched[root] = true
			}
		case raw.Mask&syscall.IN_IGNORED != 0:
			// The watched directory is gone (e.g. an emptied refs subdir
			// was removed) or the watch was removed by removeWatches.
			delete(d.watches, wd)
		default:
			w, ok := d.watches[wd]
			if !ok || !w.relevant(name) {
				break
			}
			if w.isRefs && raw.Mask&syscall.IN_ISDIR != 0 && raw.Mask&(syscall.IN_CREATE|syscall.IN_MOVED_TO) != 0 {
				// A new ref namespace (first push to a remote, first
				// feature/* branch). Its refs may already have been
				// written before this watch exists - the refresh
				// triggered below runs after it does, so it sees them.
				for root := range w.roots {
					wds := d.addRefsWatches(root, filepath.Join(w.path, name))
					if rs := d.repos[root]; rs != nil {
						rs.mu.Lock()
						rs.watches = append(rs.watches, wds...)
						rs.mu.Unlock()
					}
				}
			}
			for root := range w.roots {
				touched[root] = true
			}
		}
		d.mu.Unlock()
	}

	for root := range touched {
		d.mu.Lock()
		rs := d.repos[root]
		d.mu.Unlock()
		if rs != nil {
			rs.requestRefresh(root)
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

	d, err := newDaemonState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptbuilder --daemon: %v\n", err)
		os.Exit(1)
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
