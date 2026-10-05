package assistant

import (
	"errors"

	gadget "github.com/HuskerMinion/techo5/echod/internal/feature/muse"
	"github.com/HuskerMinion/techo5/echod/internal/feature/voice"
	"github.com/HuskerMinion/techo5/echod/internal/lib/llm"
)

// screen puts a page up on the device's screen - "weather", "radar" or "calendar" - and says whether it
// could. Set by the display on a device that has a screen; without one the tool is not offered.
var screen func(page string) bool

// SetScreen is how the display offers its pages. Muse is told again, since it was told before there
// was a screen.
func SetScreen(fn func(page string) bool) {
	screen = fn
	gadget.Get().SetCommands(commandsForMuse(tools()))
}

func screenTools() []tool {
	if screen == nil {
		return nil
	}
	return []tool{{llm.Tool{Name: "show_on_screen", Description: "Show a page on this device's screen: the weather forecast, the rain map (radar), or the calendar. The forecast and the radar are for this device's own location only.",
		Parameters: object(map[string]any{"page": map[string]any{"type": "string", "enum": []string{"weather", "radar", "calendar"}}}, "page")},
		func(a map[string]any) (string, error) {
			page := argString(a, "page")
			if !screen(page) {
				return "", errors.New("that page cannot be shown here")
			}
			voice.Get().LookHere()
			return "the " + page + " page is going up on the screen once you have answered", nil
		}}}
}
