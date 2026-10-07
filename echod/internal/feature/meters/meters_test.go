package meters

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "a-key-long-enough-to-count"

func send(f *Feature, key, body string) int {
	r := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	f.receive(w, r)
	return w.Code
}

func TestABoardIsTakenOnlyWithTheKey(t *testing.T) {
	keyFile = filepath.Join(t.TempDir(), "meters.key")
	const board = `{"title":"Usage","rows":[{"label":"Current","percent":21,"resets_at":1790000000}]}`
	f := &Feature{}

	if on() {
		t.Error("the path is served with no key file")
	}
	if code := send(f, testKey, board); code != http.StatusForbidden {
		t.Errorf("with no key file: %d", code)
	}
	if err := os.WriteFile(keyFile, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if on() {
		t.Error("a key of a few letters opens the path")
	}
	if err := os.WriteFile(keyFile, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !on() {
		t.Fatal("the path is not served with a key file")
	}
	for name, key := range map[string]string{"none": "", "another": testKey + "x"} {
		if code := send(f, key, board); code != http.StatusForbidden {
			t.Errorf("with %s key: %d", name, code)
		}
	}
	if _, ok := f.Board(); ok {
		t.Fatal("a refused board was kept")
	}

	heard := 0
	f.Changed.Listen(func(struct{}) { heard++ })
	if code := send(f, testKey, board); code != http.StatusNoContent {
		t.Fatalf("with the key: %d", code)
	}
	b, ok := f.Board()
	if !ok || heard != 1 || b.Title != "Usage" || len(b.Rows) != 1 || b.Rows[0].Percent != 21 || b.Rows[0].ResetsAt != 1790000000 {
		t.Errorf("kept %+v, said so %d times", b, heard)
	}
}

func TestABoardIsCutToWhatTheScreenHolds(t *testing.T) {
	long := strings.Repeat("word ", 40)
	raw := `{"title":"` + long + `","note":"two\nlines","rows":[` +
		`{"label":"a","percent":250},{"label":"b","percent":-4},{"label":"c","percent":50},{"label":"d","percent":1},{"label":"e","percent":2}]}`
	b, ok := parse([]byte(raw))
	if !ok {
		t.Fatal("not taken")
	}
	if len([]rune(b.Title)) != maxLabel || b.Note != "two lines" || len(b.Rows) != maxRows {
		t.Errorf("title %q, note %q, %d rows", b.Title, b.Note, len(b.Rows))
	}
	if b.Rows[0].Percent != 100 || b.Rows[1].Percent != 0 || b.Rows[2].Percent != 50 {
		t.Errorf("shares %v %v %v", b.Rows[0].Percent, b.Rows[1].Percent, b.Rows[2].Percent)
	}
	for _, not := range []string{``, `{}`, `{"rows":[]}`, `[1]`, `{"rows":"x"}`} {
		if _, ok := parse([]byte(not)); ok {
			t.Errorf("%q was taken for a board", not)
		}
	}
	if f := (&Feature{}); send(f, "", strings.Repeat("x", maxBody+10)) != http.StatusForbidden {
		t.Error("a body is read before the key is checked")
	}
}
