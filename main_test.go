package main

import (
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// fake stands in for the fsearch daemon.
type fake struct {
	hits     []Hit
	files    []GrepFile
	err      error
	searches atomic.Int32
	lastQ    atomic.Value
}

func (f *fake) Search(q string, limit int) (NameResult, error) {
	f.searches.Add(1)
	f.lastQ.Store(q)
	return NameResult{Hits: f.hits, TookUS: 120}, f.err
}

func (f *fake) Grep(q string, limit int) (GrepResult, error) {
	f.searches.Add(1)
	f.lastQ.Store(q)
	return GrepResult{Files: f.files, Complete: true}, f.err
}

func (f *fake) Status() (Status, error) { return Status{Entries: 8_300_000}, nil }

func sampleHits() []Hit {
	return []Hit{
		{Kind: "file", Path: "/Users/ada/Developer/fsearch/src/main.rs", Size: 4096, MTime: 1_760_000_000},
		{Kind: "file", Path: "/Users/ada/Developer/other/main.go", Size: 100, MTime: 1_750_000_000},
		{Kind: "dir", Path: "/Users/ada/Documents/main", MTime: 1_740_000_000},
	}
}

// settle runs the debounce and the search to the end.
func settle(t *testing.T, tt *ui.Tester, a *app) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		tt.Frame()
		if a.idle() {
			return
		}
	}
	t.Fatal("search did not finish")
}

func TestSearchByName(t *testing.T) {
	f := &fake{hits: sampleHits()}
	a := newApp(f)
	tt := ui.NewTester(a.view, 980, 640)

	if !tt.HasText("Search every file on this Mac") {
		t.Fatalf("no hint on an empty query: %q", tt.Texts())
	}
	if err := tt.Click("Search files"); err != nil {
		t.Fatal(err)
	}
	tt.Type("main")
	settle(t, tt, a)

	if got := f.lastQ.Load(); got != "main" {
		t.Errorf("searched %v, want main", got)
	}
	for _, want := range []string{"main.rs", "main.go", "3 files", "4.0 KB"} {
		if !tt.HasText(want) {
			t.Errorf("missing %q in %q", want, tt.Texts())
		}
	}
}

func TestTypingSearchesOnce(t *testing.T) {
	f := &fake{hits: sampleHits()}
	a := newApp(f)
	tt := ui.NewTester(a.view, 980, 640)
	if err := tt.Click("Search files"); err != nil {
		t.Fatal(err)
	}
	for _, ch := range "main" {
		tt.Type(string(ch))
	}
	settle(t, tt, a)
	if n := f.searches.Load(); n != 1 {
		t.Errorf("%d searches for one burst of typing, want 1", n)
	}
}

func TestStaleAnswerIsDropped(t *testing.T) {
	a := newApp(&fake{hits: sampleHits()})
	a.query = "main"
	a.startSearch()
	a.clear() // the user emptied the box before the answer came
	time.Sleep(100 * time.Millisecond)
	a.drain()
	if len(a.rows) != 0 {
		t.Errorf("a stale answer filled %d rows", len(a.rows))
	}
}

func TestContentMode(t *testing.T) {
	f := &fake{files: []GrepFile{{
		Path:    "/Users/ada/Developer/fsearch/src/server.rs",
		Matches: []GrepMatch{{Line: 162, Text: "    let op = v.get(\"op\")"}},
	}}}
	a := newApp(f)
	a.mode = modeContent
	tt := ui.NewTester(a.view, 980, 640)
	if err := tt.Click("Search files"); err != nil {
		t.Fatal(err)
	}
	tt.Type("ext:rs op")
	settle(t, tt, a)
	for _, want := range []string{"server.rs", "162", "1 matches"} {
		if !tt.HasText(want) {
			t.Errorf("missing %q in %q", want, tt.Texts())
		}
	}
}

func TestIndexingNotice(t *testing.T) {
	f := &fake{err: &IndexingError{Message: "indexing: 2.1M entries so far"}}
	a := newApp(f)
	tt := ui.NewTester(a.view, 980, 640)
	if err := tt.Click("Search files"); err != nil {
		t.Fatal(err)
	}
	tt.Type("main")
	settle(t, tt, a)
	if !tt.HasText("indexing: 2.1M entries so far") {
		t.Errorf("texts %q", tt.Texts())
	}
}

func TestError(t *testing.T) {
	a := newApp(&fake{err: errors.New("bad query")})
	tt := ui.NewTester(a.view, 980, 640)
	if err := tt.Click("Search files"); err != nil {
		t.Fatal(err)
	}
	tt.Type("main")
	settle(t, tt, a)
	if !tt.HasText("bad query") {
		t.Errorf("texts %q", tt.Texts())
	}
}

func TestSort(t *testing.T) {
	a := newApp(&fake{})
	a.rows = nameRows(NameResult{Hits: sampleHits()})
	a.sort = ui.SortOrder{Column: "size"}
	a.resort()
	if a.sorted[0].Size != 0 || a.sorted[2].Size != 4096 {
		t.Errorf("ascending by size: %+v", a.sorted)
	}
	a.sort.Descending = true
	a.resort()
	if a.sorted[0].Size != 4096 {
		t.Errorf("descending by size: %+v", a.sorted)
	}
}

func TestSplitGrep(t *testing.T) {
	p, f := splitGrep("ext:rs in:~/Developer apply_dir")
	if p != "apply_dir" || f != "ext:rs in:~/Developer" {
		t.Errorf("got %q, %q", p, f)
	}
	p, f = splitGrep("a:b plain")
	if p != "a:b plain" || f != "" {
		t.Errorf("unknown key kept as text: %q, %q", p, f)
	}
}

func TestFormat(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 5 << 20: "5.0 MB"} {
		if got := formatSize(in); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", in, got, want)
		}
	}
	if got := formatCount(8_300_000); !strings.HasPrefix(got, "8.3") {
		t.Errorf("formatCount = %q", got)
	}
}

// TestSnapshot writes what the window shows to SNAPSHOT_DIR, to look at.
func TestSnapshot(t *testing.T) {
	dir := os.Getenv("SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("SNAPSHOT_DIR not set")
	}
	a := newApp(&fake{hits: sampleHits()})
	tt := ui.NewTester(a.view, 980, 640)
	save := func(name string) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	tt.Frame()
	save("empty.png")
	if err := tt.Click("Search files"); err != nil {
		t.Fatal(err)
	}
	tt.Type("main")
	settle(t, tt, a)
	save("results.png")
}
