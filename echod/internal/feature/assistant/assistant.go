// Package assistant answers a voice turn on the device itself, for the direct pipeline
// (feature/voice/direct.go): what was heard goes to a chat model with the device's own abilities as
// tools - timers, alarms, the radio, the volume, calling another device - and the model's answer is
// what the device says. See config.Brain.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/home"
	"github.com/HuskerMinion/techo5/echod/internal/feature/voice"
	"github.com/HuskerMinion/techo5/echod/internal/lib/llm"
	"github.com/HuskerMinion/techo5/echod/internal/lib/speech"
	"github.com/HuskerMinion/techo5/echod/internal/lib/triggers"
)

func init() { voice.SetThink(Get().Think) }

// rounds bounds how many times one turn may go back to the model with tool results: a model that
// keeps asking for tools is not going to stop by being asked once more.
const rounds = 6

// memory is how long the conversation so far is kept for a follow-up; a question asked after that is
// a new conversation.
const memory = 5 * time.Minute

// kept is how many earlier messages go back with a new question.
const kept = 12

type Assistant struct {
	mu      sync.Mutex
	history []llm.Message
	lastAt  time.Time
}

var (
	once   sync.Once
	shared *Assistant
)

func Get() *Assistant {
	once.Do(func() { shared = &Assistant{} })
	return shared
}

// Think answers one thing said.
func (a *Assistant) Think(ctx context.Context, heard string) (string, error) {
	// What the screen does on its own when it hears it - the clock back, a camera up - is done, and
	// needs no answer: asked as well, the model took "go home" for a call to make and asked where to.
	if handledHere(heard) {
		return "", nil
	}
	b := config.Get().Brain
	c := &llm.Client{Base: b.LLM, Key: b.Key, Model: b.Model}

	a.mu.Lock()
	if time.Since(a.lastAt) > memory {
		a.history = nil
	}
	past := append([]llm.Message(nil), a.history...)
	a.mu.Unlock()

	msgs := append([]llm.Message{{Role: "system", Content: instructions(b, time.Now())}}, past...)
	user := llm.Message{Role: "user", Content: heard}
	msgs = append(msgs, user)
	added := []llm.Message{user}

	ts := tools()
	for range rounds {
		m, err := c.Chat(ctx, msgs, specs(ts))
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
		added = append(added, m)
		if len(m.ToolCalls) == 0 {
			a.remember(added)
			return speech.Spoken(m.Content), nil
		}
		for _, call := range m.ToolCalls {
			result := run(ts, call)
			slog.Info("assistant: tool", "name", call.Function.Name, "args", call.Function.Arguments, "result", result)
			r := llm.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: result}
			msgs = append(msgs, r)
			added = append(added, r)
		}
	}
	a.remember(added)
	return "Sorry, I got stuck on that one.", nil
}

// handledHere is whether the device acts on what was heard by itself (feature/display).
func handledHere(heard string) bool {
	lang := config.Get().Screen.Language
	return triggers.AboutGoingHome(heard, lang) || (triggers.AboutCamera(heard, lang) && home.Get().MatchCamera(heard) != "")
}

func (a *Assistant) remember(added []llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append(a.history, added...)
	if len(a.history) > kept {
		a.history = a.history[len(a.history)-kept:]
		// Never start on a tool result or a call's answer: the model needs the call they belong to.
		for len(a.history) > 0 && a.history[0].Role != "user" {
			a.history = a.history[1:]
		}
	}
	a.lastAt = time.Now()
}

// instructions is what the model is told before anything is said: what it is, when and where it is,
// and how to talk, which is out loud.
func instructions(b config.Brain, now time.Time) string {
	name := config.Get().Device.Name
	var s strings.Builder
	fmt.Fprintf(&s, "You are the voice assistant of %q, a smart clock and speaker in someone's home. ", name)
	fmt.Fprintf(&s, "The date and time right now are %s, %s, time zone %s: they are correct, so use them ", now.Format("Monday, January 2, 2006"), now.Format("3:04 PM"), now.Location())
	s.WriteString("whenever the time or the date is asked for. The coming days are ")
	var days []string
	for i := 1; i <= 7; i++ {
		d := now.AddDate(0, 0, i)
		days = append(days, d.Format("Monday January 2"))
	}
	s.WriteString(strings.Join(days, ", "))
	s.WriteString(": use these rather than working a date out. ")
	s.WriteString("Everything you write is spoken aloud, exactly as written, so write only the answer - no notes ")
	s.WriteString("to yourself. Answer in one or two short sentences, with no lists, ")
	s.WriteString("markdown, emoji or symbols, and write numbers the way they are said. ")
	s.WriteString("A question that names no place is about this device's own location, even right after ")
	s.WriteString("talking about somewhere else. The screen's forecast and rain map show only this device's ")
	s.WriteString("location: to show one, use show_on_screen, and never describe a map or radar you have not seen. ")
	s.WriteString("Use the tools to do things on this device, then say briefly what you did. Do only what was ")
	s.WriteString("asked: a timer is a timer, never also an alarm. ")
	s.WriteString("An alarm time said without morning or evening is in the morning from 4 to 11 and otherwise the ")
	s.WriteString("next one to come; say which you chose. ")
	s.WriteString("If you cannot do something, say so plainly rather than pretending, and never say you did ")
	s.WriteString("something a tool did not do. ")
	s.WriteString("Asked for music with no kind named, find a music genre (like classic rock, country or pop) by ")
	s.WriteString("genre and play one of those: never a news, talk or sports station. Say the station that ")
	s.WriteString("play_radio says is playing, and if it says a station did not play, say so. ")
	s.WriteString("A station asked for by its frequency, like 106.7, or its call letters is a local one: play it ")
	s.WriteString("by what was said, and if the station that plays is from somewhere else, say where. If a ")
	s.WriteString("frequency is not found, try the call letters of the station you know is on it here, if you do. ")
	s.WriteString("Your tools are the only music this device has: never name a music service, app or account ")
	s.WriteString("that a tool did not name, and never offer one. ")
	if config.Get().MusicAssistant.Set() {
		s.WriteString("Songs, artists, albums and playlists come from the music library: use play_music for them, ")
		s.WriteString("and play_radio only for radio stations. ")
	}
	if b.Search != "" {
		s.WriteString("For anything current or that you are not sure of - sports schedules and scores, news, ")
		s.WriteString("business hours, prices, events - use web_search, then read_page on the most useful result, ")
		s.WriteString("and answer from what they say, not from memory. State only what a tool result actually says: ")
		s.WriteString("never make up or guess dates, times, opponents, scores or facts. If a page does not have it, ")
		s.WriteString("read another result; if none do, say you couldn't find it. A time from a page is said with ")
		s.WriteString("the time zone the page gives it in, like three o'clock Central. ")
	} else {
		s.WriteString("Beyond the tools you cannot look anything up: no internet, no news, no sports schedules or ")
		s.WriteString("scores, and nothing that happened after your training. Never make up dates, times, scores or ")
		s.WriteString("facts about current events; say you can't look that up. General knowledge you are sure of is fine. ")
	}
	s.WriteString("What you hear comes from speech recognition. Only when the words truly make no sense, ask the ")
	s.WriteString("person to say it again; otherwise answer the likeliest meaning.")
	if p := strings.TrimSpace(b.Prompt); p != "" {
		s.WriteString("\n\n")
		s.WriteString(p)
	}
	return s.String()
}

// run does one call, and says what happened in words the model reads.
func run(ts []tool, call llm.ToolCall) string {
	for _, t := range ts {
		if t.Name != call.Function.Name {
			continue
		}
		args := map[string]any{}
		if a := strings.TrimSpace(call.Function.Arguments); a != "" {
			if err := json.Unmarshal([]byte(a), &args); err != nil {
				return "error: the arguments were not JSON: " + err.Error()
			}
		}
		out, err := t.Run(args)
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	}
	return "error: there is no tool called " + call.Function.Name
}
