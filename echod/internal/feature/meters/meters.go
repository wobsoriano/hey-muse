// Package meters keeps a small board of meters that something else on the network sends: a title,
// a few rows that are each a share of a hundred with the time it starts over, and a line of words.
// The Usage clock style draws it (display/usage_style.go). What the numbers measure is the sender's
// business; tools/usage sends a Claude Code plan's.
//
// A board is taken at /meters on the device's web port, from a sender with the device's meters key.
// The key is a file, and the path is only served while that file is there, so a device nobody set
// this up on has nothing to find.
package meters

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/web"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
	"github.com/HuskerMinion/techo5/echod/internal/lib/hook"
)

// Path is where a board is sent.
const Path = "/meters"

// keyFile holds the key a sender shows. Its being there is what turns the path on.
var keyFile = filepath.Join(layout.StateDir, "meters.key")

const (
	maxBody  = 4096
	maxRows  = 4
	maxLabel = 24
	maxWords = 48
	// minKey is the shortest key taken, so an empty or one-letter file does not open the path.
	minKey = 16
)

// Row is one meter.
type Row struct {
	Label   string  `json:"label"`
	Percent float64 `json:"percent"`
	// ResetsAt is when the meter starts over, in Unix seconds; 0 for one that does not say.
	ResetsAt int64 `json:"resets_at"`
}

// Board is what was last sent.
type Board struct {
	Title string `json:"title"`
	Note  string `json:"note"`
	Rows  []Row  `json:"rows"`
	// At is when it arrived, by this device's clock; zero for a device that has had none.
	At time.Time `json:"-"`
}

// Feature holds the board.
type Feature struct {
	mu    sync.Mutex
	board Board

	// Changed fires when a board arrives, for the screen to draw it.
	Changed hook.Hook[struct{}]
}

var (
	once   sync.Once
	shared *Feature
)

func Get() *Feature {
	once.Do(func() {
		shared = &Feature{}
		web.Handle(Path, "", on, shared.receive)
	})
	return shared
}

func init() { Get() }

// Board is the last board sent, and whether there has been one.
func (f *Feature) Board() (Board, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.board, !f.board.At.IsZero()
}

func key() string {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return ""
	}
	if k := strings.TrimSpace(string(b)); len(k) >= minKey {
		return k
	}
	return ""
}

func on() bool { return key() != "" }

func (f *Feature) receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post a board", http.StatusMethodNotAllowed)
		return
	}
	shown := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if k := key(); k == "" || subtle.ConstantTimeCompare([]byte(shown), []byte(k)) != 1 {
		slog.Warn("meters: a board refused", "from", r.RemoteAddr)
		http.Error(w, "not with that key", http.StatusForbidden)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		http.Error(w, "that is too much for a board", http.StatusRequestEntityTooLarge)
		return
	}
	b, ok := parse(raw)
	if !ok {
		http.Error(w, "that was not a board", http.StatusBadRequest)
		return
	}
	b.At = time.Now()
	f.mu.Lock()
	f.board = b
	f.mu.Unlock()
	f.Changed.Emit(struct{}{})
	w.WriteHeader(http.StatusNoContent)
}

// parse reads a board and cuts it to what the screen has room for. One with no rows is not a board.
func parse(raw []byte) (Board, bool) {
	var b Board
	if json.Unmarshal(raw, &b) != nil || len(b.Rows) == 0 {
		return Board{}, false
	}
	if len(b.Rows) > maxRows {
		b.Rows = b.Rows[:maxRows]
	}
	b.Title, b.Note = clip(b.Title, maxLabel), clip(b.Note, maxWords)
	for i := range b.Rows {
		row := &b.Rows[i]
		row.Label = clip(row.Label, maxLabel)
		// A sender's NaN compares false both ways and would be drawn as it is.
		if !(row.Percent >= 0) {
			row.Percent = 0
		}
		row.Percent = min(row.Percent, 100)
	}
	return b, true
}

// clip is s as one line of at most n letters.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
