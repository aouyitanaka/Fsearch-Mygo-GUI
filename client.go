package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// Hit is one file found by name.
type Hit struct {
	Kind  string `json:"kind"`
	MTime int64  `json:"mtime"`
	Path  string `json:"path"`
	Score int    `json:"score"`
	Size  int64  `json:"size"`
}

// GrepMatch is one matching line of a file found by content.
type GrepMatch struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepFile is a file with the lines of it that matched.
type GrepFile struct {
	Path    string      `json:"path"`
	Matches []GrepMatch `json:"matches"`
}

// Status is the index's state, as `{"op":"status"}` reports it.
type Status struct {
	Entries         int    `json:"entries"`
	Dirs            int    `json:"dirs"`
	ContentDocs     int    `json:"content_docs"`
	ContentPending  int    `json:"content_pending"`
	FullDiskAccess  bool   `json:"full_disk_access"`
	IndexBytes      int    `json:"index_bytes"`
	ContentBytes    int    `json:"content_bytes"`
	Owner           bool   `json:"owner"`
	ReadyForSearch  bool   `json:"-"`
	IndexingMessage string `json:"-"`
}

// NameResult is the answer to a search by name.
type NameResult struct {
	Hits   []Hit
	TookUS int64
}

// GrepResult is the answer to a search inside files.
type GrepResult struct {
	Files    []GrepFile
	TookUS   int64
	Complete bool
	Source   string
	Indexing int
}

// Searcher is what the view needs of fsearch, so tests can stand in for it.
type Searcher interface {
	Search(q string, limit int) (NameResult, error)
	Grep(q string, limit int) (GrepResult, error)
	Status() (Status, error)
}

// IndexingError is returned while the daemon builds its first index.
type IndexingError struct{ Message string }

func (e *IndexingError) Error() string { return e.Message }

// Client talks to the fsearch daemon over its Unix socket, one JSON line per
// request and per answer.
type Client struct {
	// Bin is the fsearch executable, used to start the daemon when none runs.
	Bin  string
	Sock string

	mu   sync.Mutex
	conn net.Conn
	rd   *bufio.Reader
	seq  int
}

// NewClient finds the daemon's socket and the fsearch binary.
func NewClient() *Client {
	// fsearch itself reads $HOME, so the socket is where it puts it even
	// when HOME is not the account's home directory.
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return &Client{
		Sock: filepath.Join(home, "Library", "Application Support", "FSearch", "fsearch.sock"),
		Bin:  findBinary(home),
	}
}

func findBinary(home string) string {
	if p := os.Getenv("FSEARCH_BIN"); p != "" {
		return p
	}
	// The app bundles fsearch in Contents/Resources; `mygo dev` and tests
	// see it in the working directory's resources instead.
	if dir, err := mygo.App.Path(mygo.PathResources); err == nil {
		p := filepath.Join(dir, "fsearch")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("fsearch"); err == nil {
		return p
	}
	p := filepath.Join(home, ".local", "bin", "fsearch")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// connect opens the socket, starting the daemon when nothing listens.
func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("unix", c.Sock, time.Second)
	if err != nil {
		if c.Bin == "" {
			return errors.New("fsearch is not installed: build it from github.com/noahdunnagan/fsearch and run `fsearch install`, or set FSEARCH_BIN")
		}
		// The daemon outlives the GUI: it keeps the index fresh, and its own
		// session means closing this app does not stop it.
		cmd := exec.Command(c.Bin, "serve")
		cmd.SysProcAttr = daemonAttr()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start fsearch daemon: %w", err)
		}
		go cmd.Wait()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			if conn, err = net.DialTimeout("unix", c.Sock, time.Second); err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("fsearch daemon did not come up: %w", err)
		}
	}
	c.conn, c.rd = conn, bufio.NewReaderSize(conn, 1<<20)
	return nil
}

func (c *Client) drop() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn, c.rd = nil, nil
}

// roundTrip sends one request and reads its one-line answer. A stale
// connection (the daemon restarted) is retried once on a fresh one.
func (c *Client) roundTrip(req map[string]any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	req["id"] = c.seq
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.connect(); err != nil {
			return nil, err
		}
		c.conn.SetDeadline(time.Now().Add(timeout))
		if _, err := c.conn.Write(body); err != nil {
			lastErr = err
			c.drop()
			continue
		}
		line, err := c.rd.ReadBytes('\n')
		if err != nil {
			lastErr = err
			c.drop()
			continue
		}
		return line, nil
	}
	return nil, lastErr
}

type envelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func decode(line json.RawMessage, v any) error {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return err
	}
	if !env.OK {
		if strings.HasPrefix(env.Error, "indexing") {
			return &IndexingError{Message: env.Error}
		}
		return errors.New(env.Error)
	}
	return json.Unmarshal(line, v)
}

// Search finds files by name (or, for queries with grep:/regex:/sym:, the
// daemon answers with file matches; use Grep for those).
func (c *Client) Search(q string, limit int) (NameResult, error) {
	line, err := c.roundTrip(map[string]any{"op": "search", "q": q, "limit": limit}, 10*time.Second)
	if err != nil {
		return NameResult{}, err
	}
	var r struct {
		Hits   []Hit `json:"hits"`
		TookUS int64 `json:"took_us"`
	}
	if err := decode(line, &r); err != nil {
		return NameResult{}, err
	}
	return NameResult{Hits: r.Hits, TookUS: r.TookUS}, nil
}

// Grep searches inside files. q may carry filters (ext:, in:, ...); pattern
// is the text looked for.
func (c *Client) Grep(q string, limit int) (GrepResult, error) {
	pattern, filters := splitGrep(q)
	req := map[string]any{"op": "grep", "pattern": pattern, "limit": limit, "budget_ms": 3000}
	if filters != "" {
		req["q"] = filters
	}
	line, err := c.roundTrip(req, 15*time.Second)
	if err != nil {
		return GrepResult{}, err
	}
	var r struct {
		Files    []GrepFile `json:"files"`
		TookUS   int64      `json:"took_us"`
		Complete bool       `json:"complete"`
		Source   string     `json:"source"`
		Indexing int        `json:"indexing"`
	}
	if err := decode(line, &r); err != nil {
		return GrepResult{}, err
	}
	return GrepResult{Files: r.Files, TookUS: r.TookUS, Complete: r.Complete, Source: r.Source, Indexing: r.Indexing}, nil
}

// splitGrep separates the text to look for from filter words such as
// `ext:rs` or `in:~/Developer`, which narrow the files read.
func splitGrep(q string) (pattern, filters string) {
	var words, fs []string
	for _, w := range strings.Fields(q) {
		if i := strings.IndexByte(w, ':'); i > 0 && isFilterKey(w[:i]) {
			fs = append(fs, w)
		} else {
			words = append(words, w)
		}
	}
	return strings.Join(words, " "), strings.Join(fs, " ")
}

func isFilterKey(k string) bool {
	switch k {
	case "ext", "type", "kind", "in", "size", "mtime", "re", "path":
		return true
	}
	return false
}

// Status reports the index's size and whether content is still being read.
func (c *Client) Status() (Status, error) {
	line, err := c.roundTrip(map[string]any{"op": "status"}, 5*time.Second)
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := decode(line, &s); err != nil {
		return Status{}, err
	}
	s.ReadyForSearch = true
	return s, nil
}
