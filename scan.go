package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errCancelled = errors.New("scan cancelled")

const maxTreeChildren = 72
const maxFilesReturned = 5000

type FileEntry struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	modNano  int64
}

type TreeNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path,omitempty"`
	Type     string     `json:"type"`
	Size     int64      `json:"size"`
	Count    int64      `json:"count"`
	Modified int64      `json:"modified,omitempty"`
	Children []TreeNode `json:"children,omitempty"`
	Omitted  int        `json:"omitted,omitempty"`
}

type Status struct {
	Phase     string `json:"phase"`
	Stage     string `json:"stage"`
	Root      string `json:"root"`
	FilesSeen int64  `json:"filesSeen"`
	BytesSeen int64  `json:"bytesSeen"`
	Skipped   int64  `json:"skipped"`
	ElapsedMs int64  `json:"elapsedMs"`
	Error     string `json:"error,omitempty"`
}

type DeleteResult struct {
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type Scanner struct {
	filesSeen, bytesSeen, skipped int64
	cancelFlag                    int32

	mu      sync.Mutex
	running bool
	phase   string
	stage   string
	root    string
	started time.Time
	ended   time.Time
	errMsg  string
	tree    TreeNode
	files   []FileEntry
	index   map[string]FileEntry
}

func NewScanner() *Scanner {
	return &Scanner{phase: "idle", index: map[string]FileEntry{}}
}

func (s *Scanner) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Scanner) Start(root string, excludes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("A scan is already running.")
	}
	s.running = true
	s.phase = "walking"
	s.stage = "Reading folders"
	s.root = root
	s.started = time.Now()
	s.ended = time.Time{}
	s.errMsg = ""
	s.tree = TreeNode{}
	s.files = nil
	s.index = map[string]FileEntry{}
	atomic.StoreInt64(&s.filesSeen, 0)
	atomic.StoreInt64(&s.bytesSeen, 0)
	atomic.StoreInt64(&s.skipped, 0)
	atomic.StoreInt32(&s.cancelFlag, 0)
	go s.run(root, excludes)
	return nil
}

func (s *Scanner) Cancel() { atomic.StoreInt32(&s.cancelFlag, 1) }

func (s *Scanner) cancelled() bool { return atomic.LoadInt32(&s.cancelFlag) == 1 }

func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := time.Now()
	if !s.running && !s.ended.IsZero() {
		end = s.ended
	}
	var elapsed int64
	if !s.started.IsZero() {
		elapsed = end.Sub(s.started).Milliseconds()
	}
	return Status{
		Phase:     s.phase,
		Stage:     s.stage,
		Root:      s.root,
		FilesSeen: atomic.LoadInt64(&s.filesSeen),
		BytesSeen: atomic.LoadInt64(&s.bytesSeen),
		Skipped:   atomic.LoadInt64(&s.skipped),
		ElapsedMs: elapsed,
		Error:     s.errMsg,
	}
}

func (s *Scanner) Results() (TreeNode, []FileEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := append([]FileEntry(nil), s.files...)
	return s.tree, files
}

func (s *Scanner) run(root string, excludes []string) {
	tree, files, err := s.scan(root, excludes)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.ended = time.Now()
	switch {
	case errors.Is(err, errCancelled):
		s.phase = "cancelled"
	case err != nil:
		s.phase = "error"
		s.errMsg = err.Error()
	default:
		s.phase = "done"
		s.stage = "Done"
		s.tree = tree
		s.files = limitFiles(files, maxFilesReturned)
		s.index = make(map[string]FileEntry, len(files))
		for _, f := range files {
			s.index[f.Path] = f
		}
	}
}

func (s *Scanner) scan(root string, excludes []string) (TreeNode, []FileEntry, error) {
	skipPaths := systemSkipPaths()
	files := []FileEntry{}
	var filesMu sync.Mutex

	err := s.walkParallel(root, excludes, skipPaths, func(f FileEntry) {
		filesMu.Lock()
		files = append(files, f)
		filesMu.Unlock()
	})
	if err != nil {
		return TreeNode{}, nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].Size != files[j].Size {
			return files[i].Size > files[j].Size
		}
		return files[i].Path < files[j].Path
	})
	return buildTree(root, files), files, nil
}

func (s *Scanner) walkParallel(root string, excludes, skipPaths []string, addFile func(FileEntry)) error {
	workers := runtime.NumCPU()
	if workers < 4 {
		workers = 4
	}
	if workers > 16 {
		workers = 16
	}

	q := newScanQueue(root)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var walkErr error
	setErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if walkErr == nil {
			walkErr = err
		}
		errMu.Unlock()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				dir, ok := q.pop()
				if !ok {
					return
				}
				if s.cancelled() {
					setErr(errCancelled)
					q.done()
					continue
				}
				s.scanDir(dir, root, excludes, skipPaths, q, addFile)
				q.done()
			}
		}()
	}
	wg.Wait()

	errMu.Lock()
	defer errMu.Unlock()
	return walkErr
}

func (s *Scanner) scanDir(dir, root string, excludes, skipPaths []string, q *scanQueue, addFile func(FileEntry)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		atomic.AddInt64(&s.skipped, 1)
		return
	}
	for _, d := range entries {
		if s.cancelled() {
			return
		}
		path := filepath.Join(dir, d.Name())
		if d.IsDir() {
			if path != root && (shouldSkipDir(path, d.Name(), skipPaths) || isExcluded(path, excludes)) {
				continue
			}
			q.add(path)
			continue
		}
		if !d.Type().IsRegular() {
			continue
		}
		info, err := d.Info()
		if err != nil {
			atomic.AddInt64(&s.skipped, 1)
			continue
		}
		size := info.Size()
		if size <= 0 {
			continue
		}
		addFile(FileEntry{
			Path:     path,
			Name:     d.Name(),
			Size:     size,
			Modified: info.ModTime().Unix(),
			modNano:  info.ModTime().UnixNano(),
		})
		atomic.AddInt64(&s.filesSeen, 1)
		atomic.AddInt64(&s.bytesSeen, size)
	}
}

type scanQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	dirs    []string
	pending int
	closed  bool
}

func newScanQueue(root string) *scanQueue {
	q := &scanQueue{dirs: []string{root}, pending: 1}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *scanQueue) add(dir string) {
	q.mu.Lock()
	if !q.closed {
		q.pending++
		q.dirs = append(q.dirs, dir)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

func (q *scanQueue) pop() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.dirs) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.dirs) == 0 {
		return "", false
	}
	dir := q.dirs[len(q.dirs)-1]
	q.dirs = q.dirs[:len(q.dirs)-1]
	return dir, true
}

func (q *scanQueue) done() {
	q.mu.Lock()
	q.pending--
	if q.pending == 0 {
		q.closed = true
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

type diskNode struct {
	name     string
	path     string
	typ      string
	size     int64
	count    int64
	modified int64
	dirs     map[string]*diskNode
	files    []*diskNode
}

func buildTree(root string, files []FileEntry) TreeNode {
	base := filepath.Base(filepath.Clean(root))
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = root
	}
	r := &diskNode{name: base, path: root, typ: "dir", dirs: map[string]*diskNode{}}
	for _, f := range files {
		addFile(r, root, f)
	}
	return freezeNode(r)
}

func addFile(root *diskNode, rootPath string, f FileEntry) {
	rel, err := filepath.Rel(rootPath, f.Path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	parts := splitPath(rel)
	cur := root
	cur.size += f.Size
	cur.count++
	for _, part := range parts[:len(parts)-1] {
		next := cur.dirs[part]
		if next == nil {
			next = &diskNode{
				name: part,
				path: filepath.Join(cur.path, part),
				typ:  "dir",
				dirs: map[string]*diskNode{},
			}
			cur.dirs[part] = next
		}
		next.size += f.Size
		next.count++
		cur = next
	}
	leaf := &diskNode{
		name:     f.Name,
		path:     f.Path,
		typ:      "file",
		size:     f.Size,
		count:    1,
		modified: f.Modified,
	}
	cur.files = append(cur.files, leaf)
}

func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	})
}

func freezeNode(n *diskNode) TreeNode {
	out := TreeNode{
		Name:     n.name,
		Path:     n.path,
		Type:     n.typ,
		Size:     n.size,
		Count:    n.count,
		Modified: n.modified,
	}
	if n.typ == "file" {
		return out
	}

	children := make([]*diskNode, 0, len(n.dirs)+len(n.files))
	for _, d := range n.dirs {
		children = append(children, d)
	}
	children = append(children, n.files...)
	sort.Slice(children, func(i, j int) bool {
		if children[i].size != children[j].size {
			return children[i].size > children[j].size
		}
		if children[i].typ != children[j].typ {
			return children[i].typ == "dir"
		}
		return children[i].name < children[j].name
	})

	limit := len(children)
	if limit > maxTreeChildren {
		limit = maxTreeChildren
		out.Omitted = len(children) - limit
	}
	out.Children = make([]TreeNode, 0, limit)
	for _, c := range children[:limit] {
		out.Children = append(out.Children, freezeNode(c))
	}
	return out
}

func limitFiles(files []FileEntry, n int) []FileEntry {
	if len(files) <= n {
		return append([]FileEntry(nil), files...)
	}
	return append([]FileEntry(nil), files[:n]...)
}

var skipAnywhere = map[string]bool{
	"$recycle.bin": true, "system volume information": true,
}

func shouldSkipDir(path, name string, skipPaths []string) bool {
	lower := strings.ToLower(name)
	if skipAnywhere[lower] || extraSkipNames[lower] {
		return true
	}
	for _, p := range skipPaths {
		if strings.EqualFold(path, p) {
			return true
		}
	}
	return false
}

func isExcluded(path string, excludes []string) bool {
	for _, e := range excludes {
		if e == "" {
			continue
		}
		if strings.EqualFold(path, e) {
			return true
		}
		prefix := e
		if !os.IsPathSeparator(prefix[len(prefix)-1]) {
			prefix += string(os.PathSeparator)
		}
		if len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func (s *Scanner) Delete(paths []string) []DeleteResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]DeleteResult, 0, len(paths))
	fail := func(p, msg string) {
		results = append(results, DeleteResult{Path: p, Error: msg})
	}

	if s.running {
		for _, p := range paths {
			fail(p, "A scan is still running.")
		}
		return results
	}

	deleted := map[string]bool{}
	for _, p := range paths {
		f, ok := s.index[p]
		if !ok {
			fail(p, "This file wasn't part of the last scan.")
			continue
		}
		if deleted[p] {
			continue
		}
		st, err := os.Lstat(f.Path)
		if err != nil {
			fail(p, "The file is already gone. Scan again to refresh the list.")
			continue
		}
		if !st.Mode().IsRegular() || st.Size() != f.Size || st.ModTime().UnixNano() != f.modNano {
			fail(p, "The file changed after the scan. Scan again to refresh the list.")
			continue
		}
		if err := moveToTrash(f.Path); err != nil {
			fail(p, err.Error())
			continue
		}
		deleted[p] = true
		results = append(results, DeleteResult{Path: p, OK: true})
	}

	if len(deleted) > 0 {
		kept := s.files[:0]
		for _, f := range s.files {
			if !deleted[f.Path] {
				kept = append(kept, f)
			}
		}
		s.files = kept
		for p := range deleted {
			delete(s.index, p)
		}
		all := make([]FileEntry, 0, len(s.index))
		for _, f := range s.index {
			all = append(all, f)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Size != all[j].Size {
				return all[i].Size > all[j].Size
			}
			return all[i].Path < all[j].Path
		})
		s.files = limitFiles(all, maxFilesReturned)
		if s.root != "" {
			s.tree = buildTree(s.root, all)
		}
	}
	return results
}
