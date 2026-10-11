package main

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

const (
	modeName = iota
	modeContent
)

const (
	resultLimit = 500
	// debounce is how long typing pauses before a search starts, so a burst
	// of keystrokes costs one query.
	debounce = 120 * time.Millisecond
	// contentDebounce is longer: a content search reads files.
	contentDebounce = 350 * time.Millisecond
)

// row is one line of the results table: a file found by name, or one
// matching line of a file found by content.
type row struct {
	Path  string
	Dir   string
	Name  string
	Kind  string
	Size  int64
	MTime int64
	Line  int
	Text  string
}

// app is the state the window shows. The view reads it on the main thread;
// searches run elsewhere and change it through win.Update.
type app struct {
	searcher Searcher
	win      *mygo.Window

	query string
	mode  int

	rows     []row
	sorted   []row
	table    ui.ListState
	selected int
	sort     ui.SortOrder

	// pending is when the query last changed and has not been searched yet.
	pending  time.Time
	searchID int
	busy     bool
	err      string
	// notice is a message that is not an error, such as "indexing…".
	notice string
	took   time.Duration
	status Status
	hasSt  bool

	// queued holds changes made without a window; see apply.
	queued []func()
	mu     sync.Mutex
}

func newApp(s Searcher) *app {
	a := &app{searcher: s, selected: -1}
	a.table.Selected = &a.selected
	a.table.Sort = &a.sort
	a.table.Key = func(i int) any { return i }
	a.table.Label = func(i int) string {
		if i >= 0 && i < len(a.sorted) {
			return a.sorted[i].Name
		}
		return ""
	}
	return a
}

func (a *app) columns() []ui.TableColumn {
	if a.mode == modeContent {
		return []ui.TableColumn{
			{Title: "Name", ID: "name", Width: 200, Sortable: true},
			{Title: "Line", ID: "line", Width: 60, Align: ui.End, Sortable: true},
			{Title: "Match", ID: "text"},
			{Title: "Folder", ID: "dir", Width: 260, Sortable: true},
		}
	}
	return []ui.TableColumn{
		{Title: "Name", ID: "name", Width: 260, MinWidth: 80, Sortable: true},
		{Title: "Folder", ID: "dir", Sortable: true},
		{Title: "Size", ID: "size", Width: 90, Align: ui.End, Sortable: true},
		{Title: "Modified", ID: "mtime", Width: 140, Sortable: true},
	}
}

func (a *app) view(c *ui.Context) {
	a.drain()
	a.tick(c)

	ui.Column(c).Fill().Children(func() {
		a.toolbar(c)
		ui.Divider(c)
		a.results(c).Grow(1)
		ui.Divider(c)
		a.statusBar(c)
	})
}

// tick starts a search once typing has paused, and asks for a frame to
// notice that moment when it is still ahead.
func (a *app) tick(c *ui.Context) {
	if a.pending.IsZero() {
		return
	}
	wait := debounce
	if a.mode == modeContent {
		wait = contentDebounce
	}
	if left := time.Until(a.pending.Add(wait)); left > 0 {
		c.After(left)
		return
	}
	a.pending = time.Time{}
	a.startSearch()
}

func (a *app) toolbar(c *ui.Context) ui.Element {
	return ui.Row(c).Padding(10).Gap(10).Children(func() {
		search := ui.SearchField(c.Key("search"), &a.query).Label("Search files").Grow(1)
		if search.Changed() {
			a.pending = time.Now()
			if strings.TrimSpace(a.query) == "" {
				a.clear()
			}
		}
		if search.Submitted() {
			a.pending = time.Time{}
			a.startSearch()
		}
		if ui.Segmented(c, &a.mode, "Name", "Content").Label("Search in").Changed() {
			a.clear()
			a.sort = ui.SortOrder{}
			a.pending = time.Now()
		}
	})
}

func (a *app) clear() {
	a.searchID++
	a.rows, a.sorted = nil, nil
	a.selected = -1
	a.err, a.notice, a.busy = "", "", false
}

func (a *app) results(c *ui.Context) ui.Element {
	if strings.TrimSpace(a.query) == "" {
		return a.hint(c)
	}
	if len(a.sorted) == 0 {
		return ui.Column(c).Center().Children(func() {
			switch {
			case a.err != "":
				ui.Text(c, a.err).TextColor(c.Theme().Danger)
			case a.notice != "":
				ui.Text(c, a.notice).TextColor(c.Theme().TextMuted)
			case a.busy:
				ui.Spinner(c)
			default:
				ui.Text(c, "No results").TextColor(c.Theme().TextMuted)
			}
		})
	}

	rows := a.sorted
	t := c.Theme()
	table := ui.Table(c, &a.table, a.columns(), len(rows), func(i, col int) {
		r := rows[i]
		if a.mode == modeContent {
			switch col {
			case 0:
				ui.Text(c, r.Name).SingleLine()
			case 1:
				ui.Textf(c, "%d", r.Line).TextColor(t.TextMuted)
			case 2:
				ui.Text(c, strings.TrimSpace(r.Text)).SingleLine()
			case 3:
				ui.Text(c, r.Dir).SingleLine().TextColor(t.TextMuted)
			}
			return
		}
		switch col {
		case 0:
			ui.Text(c, r.Name).SingleLine()
		case 1:
			ui.Text(c, r.Dir).SingleLine().TextColor(t.TextMuted)
		case 2:
			if r.Kind == "dir" {
				ui.Text(c, "—").TextColor(t.TextMuted)
			} else {
				ui.Text(c, formatSize(r.Size)).TextColor(t.TextMuted)
			}
		case 3:
			ui.Text(c, formatTime(r.MTime)).TextColor(t.TextMuted)
		}
	}).Label("Results")

	if table.Changed() && a.sortChanged() {
		a.resort()
	}
	if table.Submitted() {
		a.open(a.selected)
	}
	return table.ContextMenu(func(m *ui.Menu) {
		has := a.selected >= 0 && a.selected < len(a.sorted)
		if m.Item("Open").Disabled(!has).Chosen() {
			a.open(a.selected)
		}
		if m.Item("Reveal in Finder").Disabled(!has).Chosen() {
			a.reveal(a.selected)
		}
		m.Separator()
		if m.Item("Copy Path").Disabled(!has).Chosen() {
			a.copyPath(a.selected)
		}
	})
}

func (a *app) hint(c *ui.Context) ui.Element {
	t := c.Theme()
	return ui.Column(c).Center().Gap(6).Children(func() {
		ui.Text(c, "Search every file on this Mac").FontSize(18).Bold()
		ui.Text(c, "Words are fuzzy and forgive a typo in longer ones.").TextColor(t.TextMuted)
		ui.Text(c, "Try  ext:rs main   type:image mtime:<7d   in:~/Developer readme").TextColor(t.TextMuted)
		ui.Text(c, "Content mode searches inside files:  ext:go func  in:~/Developer").TextColor(t.TextMuted)
	})
}

func (a *app) statusBar(c *ui.Context) ui.Element {
	t := c.Theme()
	return ui.Row(c).Padding(6, 10).Gap(10).Children(func() {
		switch {
		case a.busy:
			ui.Spinner(c)
			ui.Text(c, "Searching…").TextColor(t.TextMuted)
		case a.err != "":
			ui.Text(c, a.err).TextColor(t.Danger).SingleLine()
		case strings.TrimSpace(a.query) != "":
			unit := "files"
			if a.mode == modeContent {
				unit = "matches"
			}
			msg := fmt.Sprintf("%d %s", len(a.rows), unit)
			if len(a.rows) >= resultLimit {
				msg = fmt.Sprintf("first %d %s", resultLimit, unit)
			}
			ui.Text(c, msg+" · "+formatDuration(a.took)).TextColor(t.TextMuted)
		}
		ui.Spacer(c)
		if a.hasSt {
			if a.status.ContentPending > 0 {
				ui.Badge(c, fmt.Sprintf("indexing content: %d left", a.status.ContentPending))
			}
			ui.Text(c, fmt.Sprintf("%s files indexed", formatCount(a.status.Entries))).TextColor(t.TextMuted)
		}
	})
}

// startSearch runs the current query on a goroutine. Answers of searches the
// user has typed past are dropped.
func (a *app) startSearch() {
	q := strings.TrimSpace(a.query)
	if q == "" {
		a.clear()
		return
	}
	a.searchID++
	id, mode := a.searchID, a.mode
	a.busy, a.err, a.notice = true, "", ""

	go func() {
		start := time.Now()
		var rows []row
		var err error
		// took is the time fsearch says it spent, which is what the user
		// wants to see; the round trip is the fallback.
		var took time.Duration
		if mode == modeContent {
			var res GrepResult
			if res, err = a.searcher.Grep(q, resultLimit); err == nil {
				rows = grepRows(res, resultLimit)
				took = time.Duration(res.TookUS) * time.Microsecond
			}
		} else {
			var res NameResult
			if res, err = a.searcher.Search(q, resultLimit); err == nil {
				rows = nameRows(res)
				took = time.Duration(res.TookUS) * time.Microsecond
			}
		}
		if took == 0 {
			took = time.Since(start)
		}
		a.apply(func() {
			if id != a.searchID {
				return
			}
			a.busy = false
			a.took = took
			a.selected = -1
			if err != nil {
				a.rows, a.sorted = nil, nil
				var ie *IndexingError
				if asIndexing(err, &ie) {
					a.notice = ie.Message
					a.watchIndexing()
				} else {
					a.err = err.Error()
				}
				return
			}
			a.rows = rows
			a.resort()
		})
		a.refreshStatus()
	}()
}

// watchIndexing searches again once the first index is built.
func (a *app) watchIndexing() {
	go func() {
		for {
			time.Sleep(time.Second)
			st, err := a.searcher.Status()
			if err != nil {
				var ie *IndexingError
				if asIndexing(err, &ie) {
					a.apply(func() { a.notice = ie.Message })
					continue
				}
				return
			}
			a.apply(func() {
				a.status, a.hasSt = st, true
				if a.notice != "" {
					a.notice = ""
					a.pending = time.Now().Add(-debounce)
				}
			})
			return
		}
	}()
}

func (a *app) refreshStatus() {
	st, err := a.searcher.Status()
	if err != nil {
		return
	}
	a.apply(func() { a.status, a.hasSt = st, true })
}

// apply changes the state on the main thread. Without a window, as in tests,
// the change waits in a queue that the next frame drains, which is the same
// thread discipline win.Update gives.
func (a *app) apply(fn func()) {
	if a.win != nil {
		a.win.Update(fn)
		return
	}
	a.mu.Lock()
	a.queued = append(a.queued, fn)
	a.mu.Unlock()
}

func (a *app) drain() {
	a.mu.Lock()
	q := a.queued
	a.queued = nil
	a.mu.Unlock()
	for _, fn := range q {
		fn()
	}
}

// idle reports whether no search is waiting, running or unapplied.
func (a *app) idle() bool {
	a.mu.Lock()
	n := len(a.queued)
	a.mu.Unlock()
	return n == 0 && a.pending.IsZero() && !a.busy
}

func (a *app) sortChanged() bool { return true }

// resort orders the rows as the table's sort says; with none, in the order
// fsearch ranked them.
func (a *app) resort() {
	a.sorted = append(a.sorted[:0], a.rows...)
	if a.sort.Column == "" {
		return
	}
	key := func(r row) (string, int64) {
		switch a.sort.Column {
		case "name":
			return strings.ToLower(r.Name), 0
		case "dir":
			return strings.ToLower(r.Dir), 0
		case "size":
			return "", r.Size
		case "mtime":
			return "", r.MTime
		case "line":
			return "", int64(r.Line)
		}
		return "", 0
	}
	sort.SliceStable(a.sorted, func(i, j int) bool {
		si, ni := key(a.sorted[i])
		sj, nj := key(a.sorted[j])
		less := si < sj || (si == sj && ni < nj)
		if a.sort.Descending {
			return si > sj || (si == sj && ni > nj)
		}
		return less
	})
	a.selected = -1
}

func (a *app) path(i int) (string, bool) {
	if i < 0 || i >= len(a.sorted) {
		return "", false
	}
	return a.sorted[i].Path, true
}

func (a *app) open(i int) {
	if p, ok := a.path(i); ok {
		if err := mygo.Shell.OpenPath(p); err != nil {
			a.err = err.Error()
		}
	}
}

func (a *app) reveal(i int) {
	if p, ok := a.path(i); ok {
		mygo.Shell.ShowItemInFolder(p)
	}
}

func (a *app) copyPath(i int) {
	if p, ok := a.path(i); ok {
		mygo.Clipboard.WriteText(p)
	}
}

func nameRows(res NameResult) []row {
	rows := make([]row, 0, len(res.Hits))
	for _, h := range res.Hits {
		rows = append(rows, row{
			Path: h.Path, Dir: filepath.Dir(h.Path), Name: filepath.Base(h.Path),
			Kind: h.Kind, Size: h.Size, MTime: h.MTime,
		})
	}
	return rows
}

func grepRows(res GrepResult, limit int) []row {
	var rows []row
	for _, f := range res.Files {
		for _, m := range f.Matches {
			rows = append(rows, row{
				Path: f.Path, Dir: filepath.Dir(f.Path), Name: filepath.Base(f.Path),
				Kind: "file", Line: m.Line, Text: m.Text,
			})
			if len(rows) >= limit {
				return rows
			}
		}
	}
	return rows
}

func main() {
	a := newApp(NewClient())
	mygo.App.WhenReady(func() {
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "FSearch",
			Width:     980,
			Height:    640,
			MinWidth:  560,
			MinHeight: 360,
			StateKey:  "main",
			Content:   ui.View(a.view),
		})
		go a.refreshStatus()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
