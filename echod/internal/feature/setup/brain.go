package setup

import (
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	gadget "github.com/HuskerMinion/techo5/echod/internal/feature/muse"
	"github.com/HuskerMinion/techo5/echod/internal/feature/wakeword"
	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
	"github.com/HuskerMinion/techo5/echod/internal/lib/speech"
)

// listeningSection is how long the device listens again after an answer, and how many times in a row:
// Home Assistant's two settings for the first wake word, here for a device that has none.
func listeningSection(w http.ResponseWriter, token string) {
	fmt.Fprint(w, `<fieldset><legend>Listening after an answer</legend><form method="post" action="/setup/save">`)
	hidden(w, token, "listening", "sound")
	fmt.Fprintf(w, `<label for="fu">Keep listening for (seconds, 0 for only when asked a question)</label>
	 <input id="fu" name="followup" type="number" min="0" max="30" value="%d">
	 <label for="fus">Times in a row (0 for no limit)</label>
	 <input id="fus" name="followups" type="number" min="0" max="10" value="%d">
	 <p class="note">After an answer the device listens again without the wake word, for this long and this
	  many times. A question the assistant asks is always listened for.</p>
	 <p><button type="submit">Save</button></p></form></fieldset>`,
		int(wakeword.FollowUp(0).Seconds()), wakeword.FollowUps(0))
}

func saveListening(r *http.Request) string {
	fu, err1 := strconv.Atoi(strings.TrimSpace(r.PostFormValue("followup")))
	fus, err2 := strconv.Atoi(strings.TrimSpace(r.PostFormValue("followups")))
	if err1 != nil || err2 != nil || fu < 0 || fu > 30 || fus < 0 || fus > 10 {
		return "listening is 0 to 30 seconds, and 0 to 10 times in a row"
	}
	wakeword.Get().SetFollowUp(0, fu)
	wakeword.Get().SetFollowUps(0, fus)
	return ""
}

// brainSection is where the voice answers come from: Home Assistant's Assist pipeline, speech and a
// chat model reached directly, or Muse (config.Brain). A key is never shown: it is written, or left
// alone.
func brainSection(w http.ResponseWriter, token string) {
	b := config.Get().Brain
	fmt.Fprint(w, `<fieldset><legend>Voice assistant</legend><form method="post" action="/setup/save">`)
	hidden(w, token, "brain", "sound")
	fmt.Fprintf(w, `<label for="mode">Answered by</label>
	 <select id="mode" name="mode">
	  <option value=""%s>Home Assistant (its Assist pipeline)</option>
	  <option value="direct"%s>Speech and a chat model, directly</option>
	  <option value="muse"%s>Muse, paired with the Muse app</option>
	 </select>
	 <p class="note">Directly, the device needs no Home Assistant: what is said goes to a speech-to-text
	  server, the words to a chat model that can set timers and alarms, play the radio and call other
	  devices, and the answer to a text-to-speech server. The servers are Wyoming ones, as Home Assistant
	  uses (faster-whisper, Piper), and any chat model with an OpenAI-style endpoint (llama.cpp, Ollama).</p>
	 <label for="stt">Speech to text (host:port)</label>
	 <input id="stt" name="stt" value="%s" placeholder="192.168.1.20:10300" autocomplete="off">
	 <label for="tts">Text to speech (host:port)</label>
	 <input id="tts" name="tts" value="%s" placeholder="192.168.1.20:10200" autocomplete="off">
	 <label for="voice">Voice</label>
	 <input id="voice" name="voice" value="%s" placeholder="en_US-lessac-medium" autocomplete="off">
	 <label for="language">Language</label>
	 <input id="language" name="language" value="%s" placeholder="en" maxlength="8" autocomplete="off">
	 <label for="llm">Chat model endpoint</label>
	 <input id="llm" name="llm" value="%s" placeholder="http://192.168.1.20:8080/v1" autocomplete="off">
	 <label for="model">Model</label>
	 <input id="model" name="model" value="%s" placeholder="only if the server has several" autocomplete="off">
	 <label for="key">Key</label>
	 <input id="key" name="key" type="password" value="" placeholder="%s" autocomplete="off">
	 <p><label><input type="checkbox" name="nokey" value="yes" style="width:auto"> Remove the key</label></p>
	 <label for="search">Web search (a SearXNG server)</label>
	 <input id="search" name="search" value="%s" placeholder="http://192.168.1.20:8888" autocomplete="off">
	 <p class="note">To look up what the chat model cannot know: games, news, opening hours. SearXNG needs
	  its JSON format turned on (search.formats in its settings.yml). Empty: no looking things up.</p>
	 <label for="prompt">Anything the assistant should know</label>
	 <textarea id="prompt" name="prompt" rows="3" maxlength="2000">%s</textarea>`,
		selected(b.Mode == config.BrainHomeAssistant), selected(b.Mode == config.BrainDirect), selected(b.Mode == config.BrainMuse),
		html.EscapeString(b.STT), html.EscapeString(b.TTS), html.EscapeString(b.Voice), html.EscapeString(b.Language),
		html.EscapeString(b.LLM), html.EscapeString(b.Model), keyHint(b.Key != ""), html.EscapeString(b.Search),
		html.EscapeString(b.Prompt))

	m := b.Muse
	fmt.Fprint(w, `<p class="note"><strong>Muse</strong> answers through Meta's Muse, which hears what was
	 said and answers as itself, and can set this device's timers and alarms, play its radio and stop it. It
	 needs a developer's SDK token from gadgets.muse.ai and the device paired with the Muse app (below). Muse
	 answers in words, which the device says in its built-in voice, or in a speech endpoint's with a speech
	 key.</p>
	 <label for="sdk">Muse SDK token</label>`)
	fmt.Fprintf(w, `<input id="sdk" name="sdk" type="password" value="" placeholder="%s" autocomplete="off">
	 <p><label><input type="checkbox" name="nosdk" value="yes" style="width:auto"> Remove the SDK token</label></p>
	 <label for="speechkey">Speech key</label>
	 <input id="speechkey" name="speechkey" type="password" value="" placeholder="%s" autocomplete="off">
	 <p><label><input type="checkbox" name="nospeechkey" value="yes" style="width:auto"> Remove the speech key</label></p>
	 <label for="speechvoice">Speaking voice</label>
	 <select id="speechvoice" name="speechvoice">`, keyHint(m.SDKToken != ""), keyHint(m.Speech.Key != ""))
	chosen := speech.Chosen(m.Speech.Voice, m.Speech.Key)
	for _, v := range speech.Choices() {
		fmt.Fprintf(w, `<option value="%s"%s>%s</option>`, v, selected(v == chosen), v.Label())
	}
	fmt.Fprint(w, `</select>`)
	switch {
	case !builtInVoice():
		fmt.Fprint(w, `<p class="note">This image has no built-in voice, so Muse needs a speech key to answer aloud.</p>`)
	case m.Speech.Key == "":
		fmt.Fprint(w, `<p class="note">With no speech key, Muse's answers use the built-in voice.</p>`)
	}
	fmt.Fprintf(w, `
	 <label for="speechstyle">How to say it</label>
	 <input id="speechstyle" name="speechstyle" value="%s" placeholder="warmly, and not too fast" maxlength="500" autocomplete="off">
	 <label for="speechbase">Speech endpoint (advanced)</label>
	 <input id="speechbase" name="speechbase" value="%s" placeholder="%s" autocomplete="off">
	 <label for="speechmodel">Speech model (advanced)</label>
	 <input id="speechmodel" name="speechmodel" value="%s" placeholder="%s" autocomplete="off">
	 <p class="note">Any OpenAI-style /audio/speech endpoint will do. Empty is OpenAI's own, with its %s model.</p>
	 <p><button type="submit">Save</button></p></form></fieldset>`,
		html.EscapeString(m.Speech.Style), html.EscapeString(m.Speech.Base), speech.DefaultBase,
		html.EscapeString(m.Speech.Model), speech.DefaultModel, speech.DefaultModel)
}

// builtInVoice is whether this image has the device's own voice (speech.Local); a test says so itself.
var builtInVoice = func() bool { return speech.Installed().Available() }

func keyHint(set bool) string {
	if set {
		return "set; leave empty to keep it"
	}
	return "none"
}

// musePairingSection is the device and the Muse app: what they are to each other now, and the
// buttons that change it. While a pairing is open the status moves on its own, so the page asks
// again every couple of seconds, the way the locked page waits for its press.
func musePairingSection(w http.ResponseWriter, token string) {
	s := gadget.Get().Status()
	fmt.Fprint(w, `<fieldset><legend>Muse pairing</legend>`)
	fmt.Fprintf(w, `<p id="muse-status" style="margin:0"><strong>%s</strong></p>`, html.EscapeString(museWords(s)))
	switch s.Phase {
	case gadget.Pairing:
		fmt.Fprintf(w, `<p class="note">On the phone, in the Muse app: <strong>Settings → Devices</strong>, turn on
		 <strong>Developer mode</strong>, then <strong>Add Device</strong> and pick <strong>%s</strong>. When it
		 asks for Wi-Fi, pick the one network it shows: the device is already online. Pairing stays open for
		 %d minutes.</p>`, html.EscapeString(s.BLEName), int(pairing.DefaultWindow.Minutes()))
		fmt.Fprint(w, `<form method="post" action="/setup/save">`)
		hidden(w, token, "muse-cancel", "sound")
		fmt.Fprint(w, `<p><button type="submit" class="quiet">Stop pairing</button></p></form>`)
	default:
		if s.Phase == gadget.Unpaired {
			fmt.Fprint(w, `<p class="note">Pairing puts the device on Bluetooth for the Muse app to find. Have the
			 phone and the Muse app ready, and the SDK token saved above.</p>`)
		}
		if s.CanPair {
			fmt.Fprint(w, `<form method="post" action="/setup/save" style="display:inline">`)
			hidden(w, token, "muse-pair", "sound")
			fmt.Fprint(w, `<p style="display:inline"><button type="submit">Pair with the Muse app</button></p></form> `)
		} else {
			fmt.Fprint(w, `<p class="note">This build cannot pair: it has no Bluetooth to reach the app with.</p>`)
		}
		if s.Phase != gadget.Unpaired {
			fmt.Fprint(w, `<form method="post" action="/setup/save" style="display:inline">`)
			hidden(w, token, "muse-unpair", "sound")
			fmt.Fprint(w, `<p style="display:inline"><button type="submit" class="quiet">Unpair</button></p></form>`)
		}
	}
	fmt.Fprintf(w, `<script>(()=>{const was=%q;setInterval(async()=>{try{const r=await fetch('/setup/state');
	 const s=await r.json();if(!s.in)return;if(s.muse_phase!==was){location.href='/setup?tab=sound';return}
	 document.querySelector('#muse-status strong').textContent=s.muse}catch(e){}},2000)})()</script></fieldset>`,
		s.Phase.String())
}

// museWords is the status in plain words.
func museWords(s gadget.Status) string {
	switch s.Phase {
	case gadget.Pairing:
		p := s.Progress
		switch p.State {
		case pairing.StateWaiting:
			return "Waiting for the Muse app to find " + s.BLEName + "."
		case pairing.StateConnected:
			return "The phone has found the device."
		case pairing.StateConfirmed:
			return "The app has confirmed the device; finishing."
		case pairing.StateProvisioning:
			return "Checking the pairing with Muse."
		case pairing.StateDone:
			return "Paired."
		case pairing.StateFailed:
			return "That try failed (" + failWords(p) + "). The app can try again."
		}
		return "Pairing."
	case gadget.Standby:
		return "Paired as " + s.Username + ". Muse is not what answers, so it is not connected."
	case gadget.Connecting:
		return "Paired as " + s.Username + ". Connecting to Muse."
	case gadget.Online:
		return "Paired as " + s.Username + " and connected to Muse."
	case gadget.Offline:
		why := ""
		if s.Err != nil {
			why = ": " + s.Err.Error()
		}
		return "Paired as " + s.Username + ", but not connected to Muse" + why + ". Trying again."
	}
	if s.Progress.State == pairing.StateFailed {
		return "Not paired. The last pairing failed: " + failWords(s.Progress) + "."
	}
	if s.Err != nil {
		return "Not paired: " + s.Err.Error()
	}
	return "Not paired."
}

func failWords(p pairing.Progress) string {
	switch p.Reason {
	case pairing.ReasonHandshake:
		return "the phone and the device could not agree a key"
	case pairing.ReasonOffline:
		return "the device could not reach Muse"
	case pairing.ReasonAuthRejected:
		return "Muse did not accept the pairing"
	case pairing.ReasonStorage:
		return "the pairing could not be saved"
	case pairing.ReasonWindowClosed:
		return "nobody paired in time"
	case pairing.ReasonCanceled:
		return "pairing was stopped here"
	}
	return "something went wrong"
}

// saveMusePairing is the pairing panel's buttons.
func saveMusePairing(what string) string {
	g := gadget.Get()
	switch what {
	case "muse-pair":
		if err := g.StartPairing(); err != nil {
			return "could not start pairing: " + err.Error()
		}
		slog.Info("setup page: Muse pairing opened")
	case "muse-cancel":
		g.CancelPairing()
		slog.Info("setup page: Muse pairing stopped")
	case "muse-unpair":
		if err := g.Unpair(); err != nil {
			return "could not unpair: " + err.Error()
		}
		slog.Info("setup page: Muse unpaired")
	}
	return ""
}

// saveBrain keeps the form, refusing what could not work rather than keeping it to fail at the next
// wake word.
func saveBrain(r *http.Request) string {
	v := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	was := config.Get().Brain
	b := config.Brain{
		Mode:     config.BrainMode(v("mode")),
		STT:      v("stt"),
		TTS:      v("tts"),
		Voice:    v("voice"),
		Language: v("language"),
		LLM:      strings.TrimRight(v("llm"), "/"),
		Model:    v("model"),
		Search:   strings.TrimRight(v("search"), "/"),
		Prompt:   strings.TrimSpace(r.PostFormValue("prompt")),
		Muse: config.Muse{Speech: config.Speech{
			Base:  strings.TrimRight(v("speechbase"), "/"),
			Model: v("speechmodel"),
			Voice: v("speechvoice"),
			Style: v("speechstyle"),
		}},
	}
	if b.Mode != config.BrainHomeAssistant && b.Mode != config.BrainDirect && b.Mode != config.BrainMuse {
		return "that is not a way of answering this device knows"
	}
	for _, hp := range []struct{ what, v string }{{"speech to text", b.STT}, {"text to speech", b.TTS}} {
		if hp.v == "" {
			continue
		}
		if _, port, err := net.SplitHostPort(hp.v); err != nil || port == "" {
			return hp.what + " should be host:port, like 192.168.1.20:10300"
		}
	}
	for _, u := range []struct{ what, v, like string }{
		{"the chat model endpoint", b.LLM, "http://192.168.1.20:8080/v1"},
		{"the search server", b.Search, "http://192.168.1.20:8888"},
		{"the speech endpoint", b.Muse.Speech.Base, speech.DefaultBase},
	} {
		if u.v == "" {
			continue
		}
		p, err := url.Parse(u.v)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return u.what + " should be an address like " + u.like
		}
	}
	if b.Mode == config.BrainDirect && (b.STT == "" || b.TTS == "" || b.LLM == "") {
		return "answering directly needs all three: speech to text, text to speech and the chat model"
	}
	if strings.ContainsAny(b.Voice+b.Language+b.Model+b.Muse.Speech.Model+b.Muse.Speech.Style, "\r\n") {
		return "the voice, language, model and how to say it are one line each"
	}
	if b.Muse.Speech.Voice != "" && !slices.Contains(speech.Choices(), speech.Voice(b.Muse.Speech.Voice)) {
		return "that is not a speaking voice the speech endpoint has"
	}

	// A secret posted is a secret to write, one ticked away is one to clear, and nothing posted keeps
	// what is there: which is what Muse's own have to be checked against before anything is saved.
	type secret struct{ field, remove, what string }
	secrets := []secret{{"key", "nokey", "key"}, {"sdk", "nosdk", "SDK token"}, {"speechkey", "nospeechkey", "speech key"}}
	willHave := map[string]bool{"key": was.Key != "", "sdk": was.Muse.SDKToken != "", "speechkey": was.Muse.Speech.Key != ""}
	for _, s := range secrets {
		if strings.ContainsAny(r.PostFormValue(s.field), "\r\n") {
			return "a " + s.what + " is one line"
		}
		switch {
		case r.PostFormValue(s.remove) == "yes":
			willHave[s.field] = false
		case strings.TrimSpace(r.PostFormValue(s.field)) != "":
			willHave[s.field] = true
		}
	}
	if b.Mode == config.BrainMuse && !willHave["sdk"] {
		return "answering through Muse needs the Muse SDK token"
	}
	// The speech key is needed only where nothing else can speak: with no key, or with the built-in
	// voice chosen, the device's own voice is all there is.
	if b.Mode == config.BrainMuse && !builtInVoice() && (!willHave["speechkey"] || speech.Voice(b.Muse.Speech.Voice) == speech.BuiltIn) {
		return "this image has no built-in voice, so answering through Muse needs the speech key and one of its voices"
	}

	if err := config.Set().Brain().Set(b); err != nil {
		return "could not save it: " + err.Error()
	}
	set := map[string]func(string) error{
		"key":       config.Set().Brain().SetKey,
		"sdk":       config.Set().Brain().SetSDKToken,
		"speechkey": config.Set().Brain().SetSpeechKey,
	}
	for _, s := range secrets {
		switch value := strings.TrimSpace(r.PostFormValue(s.field)); {
		case r.PostFormValue(s.remove) == "yes":
			if err := set[s.field](""); err != nil {
				return "could not remove the " + s.what + ": " + err.Error()
			}
		case value != "":
			if err := set[s.field](value); err != nil {
				return "could not save the " + s.what + ": " + err.Error()
			}
		}
	}
	slog.Info("setup page: the voice assistant was set", "mode", b.Mode, "stt", b.STT, "tts", b.TTS, "llm", b.LLM,
		"speech", b.Muse.Speech.Base)
	gadget.Get().Wake()
	return ""
}
