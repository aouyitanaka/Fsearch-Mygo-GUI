package main

import (
	"os"
	"testing"
	"time"
)

// TestLiveDaemon runs the client against the fsearch daemon of the current
// user. fsearch indexes the whole disk whatever $HOME says, so a first run
// crawls it (about a minute, ~1 GB of memory) and writes
// ~/Library/Application Support/FSearch: it is opt-in.
//
//	FSEARCH_LIVE=1 go test -run TestLiveDaemon -v
func TestLiveDaemon(t *testing.T) {
	if os.Getenv("FSEARCH_LIVE") == "" {
		t.Skip("FSEARCH_LIVE not set: this test starts a real fsearch daemon that indexes the disk")
	}
	c := NewClient()
	if c.Bin == "" {
		t.Skip("no fsearch binary: put it on PATH or set FSEARCH_BIN")
	}

	deadline := time.Now().Add(5 * time.Minute)
	var res NameResult
	var err error
	for {
		res, err = c.Search("go.mod", 20)
		var ie *IndexingError
		if err == nil || !asIndexing(err, &ie) || time.Now().After(deadline) {
			break
		}
		t.Log(ie.Message)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no file named go.mod on the whole disk?")
	}
	h := res.Hits[0]
	if h.Path == "" || h.Kind == "" {
		t.Errorf("incomplete hit %+v", h)
	}

	st, err := c.Status()
	if err != nil || st.Entries == 0 {
		t.Errorf("status %+v, err %v", st, err)
	}

	// Content may still be indexing, so only the shape of the answer is
	// checked, not that anything was found.
	if _, err := c.Grep("ext:go package", 5); err != nil {
		t.Errorf("grep: %v", err)
	}
}
