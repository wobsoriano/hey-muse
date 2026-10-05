package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gadget "github.com/HuskerMinion/techo5/echod/internal/feature/muse"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
)

// What Muse may do on this device: the tools the chat model has, narrowed to the ones that act on
// the device itself. Nothing that looks things up for it (the web, the weather, the calendar), nothing
// that reaches another device or the music library, and nothing that runs anything. Muse reads the
// descriptions to decide when to call a command, so each says when.
//
// The table is the whole of the choice: a tool that is not in it is not offered.
var museCommands = map[string]string{
	"start_timer":    "Use when asked to set or start a timer, a countdown, on this device.",
	"list_timers":    "Use when asked what timers are running on this device, or how long is left.",
	"cancel_timers":  "Use when asked to cancel or stop a timer on this device: the one named, or all of them.",
	"set_alarm":      "Use when asked to set an alarm on this device, or to be woken at a time.",
	"list_alarms":    "Use when asked what alarms or reminders are set on this device.",
	"delete_alarm":   "Use when asked to delete, remove or turn off an alarm or reminder on this device.",
	"stop":           "Use when asked to stop, be quiet, or silence what this device is ringing or playing.",
	"set_volume":     "Use when asked to set the volume of this device, louder or quieter.",
	"find_radio":     "Use when asked for radio stations by name, call letters or genre, before playing one.",
	"play_radio":     "Use when asked to play the radio, or a station, on this device.",
	"save_station":   "Use when asked to keep or save a radio station on this device.",
	"list_stations":  "Use when asked which radio stations this device has.",
	"show_on_screen": "Use when asked to show the weather forecast, the rain map or the calendar on this device's screen.",
}

func init() { gadget.Get().SetCommands(commandsForMuse(tools())) }

// commandsForMuse turns the tools in the table into Muse commands, named so they read as the
// device's. A tool whose arguments cannot be said as flat typed parameters is left out, since that
// is all Muse can send.
func commandsForMuse(ts []tool) []muse.Command {
	var out []muse.Command
	for _, t := range ts {
		description, ok := museCommands[t.Name]
		if !ok {
			continue
		}
		required, optional, ok := museParams(t.Parameters)
		if !ok {
			continue
		}
		run := t.Run
		out = append(out, muse.Command{
			Name:        "echo." + t.Name,
			Description: description + " " + t.Description,
			Required:    required,
			Optional:    optional,
			Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
				args := map[string]any{}
				if len(raw) > 0 {
					if err := json.Unmarshal(raw, &args); err != nil {
						return nil, fmt.Errorf("the arguments were not a JSON object: %w", err)
					}
				}
				result, err := run(args)
				if err != nil {
					return nil, err
				}
				return map[string]string{"result": result}, nil
			},
		})
	}
	return out
}

// museParams reads a tool's JSON Schema as Muse's parameter lists. Only an object of string, number,
// integer or boolean properties can be said; an enum goes into the description, since Muse has no
// type for it.
func museParams(schema map[string]any) (required, optional []muse.Param, ok bool) {
	props, _ := schema["properties"].(map[string]any)
	must := map[string]bool{}
	if names, _ := schema["required"].([]string); names != nil {
		for _, n := range names {
			must[n] = true
		}
	}
	for name, raw := range props {
		prop, _ := raw.(map[string]any)
		var kind muse.ParamType
		switch prop["type"] {
		case "string":
			kind = muse.String
		case "number", "integer":
			kind = muse.Integer
		case "boolean":
			kind = muse.Boolean
		default:
			return nil, nil, false
		}
		description, _ := prop["description"].(string)
		if values, _ := prop["enum"].([]string); len(values) > 0 {
			description = strings.TrimSpace(description + " One of: " + strings.Join(values, ", ") + ".")
		}
		p := muse.Param{Name: name, Type: kind, Description: description}
		if must[name] {
			required = append(required, p)
		} else {
			optional = append(optional, p)
		}
	}
	return required, optional, true
}
