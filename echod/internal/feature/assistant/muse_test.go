package assistant

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/lib/llm"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
)

// Muse gets the device's own abilities and nothing that looks things up, reaches another device or
// plays from the library, each named as the device's and with a parameter list Muse can send.
func TestCommandsForMuseAreTheSafeOnes(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	was := screen
	screen = func(string) bool { return true }
	t.Cleanup(func() { screen = was })

	cmds := commandsForMuse(tools())
	var names []string
	byName := map[string]muse.Command{}
	for _, c := range cmds {
		names = append(names, c.Name)
		byName[c.Name] = c
	}
	slices.Sort(names)
	want := []string{"echo.cancel_timers", "echo.delete_alarm", "echo.find_radio", "echo.list_alarms", "echo.list_stations",
		"echo.list_timers", "echo.play_radio", "echo.save_station", "echo.set_alarm", "echo.set_volume", "echo.show_on_screen",
		"echo.start_timer", "echo.stop"}
	if !slices.Equal(names, want) {
		t.Fatalf("commands %v, want %v", names, want)
	}
	for _, c := range cmds {
		if !strings.HasPrefix(c.Description, "Use when") || c.Handler == nil {
			t.Errorf("%s: description %q, handler %v", c.Name, c.Description, c.Handler != nil)
		}
	}

	timer := byName["echo.start_timer"]
	if len(timer.Required) != 1 || timer.Required[0].Name != "seconds" || timer.Required[0].Type != muse.Integer {
		t.Errorf("start_timer requires %+v", timer.Required)
	}
	if len(timer.Optional) != 1 || timer.Optional[0].Name != "label" || timer.Optional[0].Type != muse.String {
		t.Errorf("start_timer offers %+v", timer.Optional)
	}
	radio := byName["echo.find_radio"]
	for _, p := range radio.Required {
		if p.Name == "by" && !strings.Contains(p.Description, "One of: name, genre") {
			t.Errorf("find_radio's by: %q", p.Description)
		}
	}
	page := byName["echo.show_on_screen"]
	if len(page.Required) != 1 || !strings.Contains(page.Required[0].Description, "weather, radar, calendar") {
		t.Errorf("show_on_screen requires %+v", page.Required)
	}

	// Without a screen there is nothing to show on.
	screen = nil
	for _, c := range commandsForMuse(tools()) {
		if c.Name == "echo.show_on_screen" {
			t.Error("show_on_screen offered with no screen")
		}
	}
}

// A command runs the tool with Muse's arguments and hands back what the model would have read, and
// an argument that is not JSON is refused rather than run.
func TestMuseCommandRunsTheTool(t *testing.T) {
	var got map[string]any
	cmds := commandsForMuse([]tool{{llm.Tool{Name: "stop", Description: "Stop.", Parameters: object(map[string]any{"why": str("why")})},
		func(a map[string]any) (string, error) { got = a; return "stopped", nil }}})
	if len(cmds) != 1 || cmds[0].Name != "echo.stop" {
		t.Fatalf("commands %+v", cmds)
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"why":"asked"}`)} {
		payload, err := cmds[0].Handler(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if result := payload.(map[string]string)["result"]; result != "stopped" {
			t.Errorf("with %s: %q", raw, result)
		}
	}
	if got["why"] != "asked" {
		t.Errorf("the tool got %v", got)
	}
	if _, err := cmds[0].Handler(context.Background(), json.RawMessage(`[`)); err == nil {
		t.Error("a broken argument ran the tool")
	}
}

// Only flat, typed parameters can be said to Muse.
func TestMuseParamsRefuseWhatMuseCannotSend(t *testing.T) {
	if _, _, ok := museParams(object(map[string]any{"items": map[string]any{"type": "array"}})); ok {
		t.Error("an array parameter was offered")
	}
	required, optional, ok := museParams(object(map[string]any{"on": map[string]any{"type": "boolean"}, "n": num("how many")}, "n"))
	if !ok || len(required) != 1 || required[0].Type != muse.Integer || len(optional) != 1 || optional[0].Type != muse.Boolean {
		t.Errorf("required %+v, optional %+v, ok %v", required, optional, ok)
	}
}
