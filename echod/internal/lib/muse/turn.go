// Extends linux/src/musegadget/link_client.py in Meta's Muse Gadget SDK, Copyright (c) Meta
// Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (LICENSE in this
// directory). Modified: rewritten in Go, with reply matching added, and the stored user message's
// text kept as what Muse heard. The SDK's Linux client has no reply matching. This one reads the
// chat events that the SDK's ESP32 firmware reads (esp32/components/muse, under the same license).

package muse

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	// maxPendingEvents bounds what is held while the answer to "which message was ours" is on its
	// way. The events race that answer, so the first few always arrive before it.
	maxPendingEvents = 256
	maxReplyMessages = 32
	maxReplyText     = 256 << 10
)

// chatEvent is one line of the chat stream. Fields are read loosely, because a field of the wrong
// type means "not there" to the reference clients and has to mean the same here.
type chatEvent struct {
	Type      string                     `json:"type"`
	Event     string                     `json:"event"`
	MessageID json.RawMessage            `json:"message_id"`
	Payload   map[string]json.RawMessage `json:"payload"`
}

func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func (e *chatEvent) str(key string) string { return rawString(e.Payload[key]) }

func (e *chatEvent) messageID() string {
	if id := e.str("message_id"); id != "" {
		return id
	}
	if id := rawString(e.MessageID); id != "" {
		return id
	}
	return e.str("id")
}

// ready is false only when the event says outright that its text is not final yet.
func (e *chatEvent) ready() bool { return string(e.Payload["display_text_ready"]) != "false" }

type replyMessage struct {
	id   string
	text string
	done bool
}

// turn is one question and the answer adding up to it.
//
// Muse does not link its answer to the question: a reply names no parent, and its text chunks name
// themselves. So a message that starts after ours belongs to the turn, where "after ours" is after
// the stream echoes our message, or after the acknowledgment if the echo came first.
type turn struct {
	messageID string
	pending   []chatEvent
	oursSeen  bool
	messages  []*replyMessage
	// busy is Muse saying it is still working, which holds the turn open through a pause.
	busy      bool
	heard     string
	lastEvent time.Time
	// wake is poked on every change, for whoever is waiting on the answer.
	wake chan struct{}
}

func newTurn() *turn {
	return &turn{lastEvent: time.Now(), wake: make(chan struct{}, 1)}
}

func (t *turn) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// add takes the next event off the stream.
func (t *turn) add(e chatEvent) {
	t.lastEvent = time.Now()
	defer t.poke()
	switch e.Event {
	case "agent.status", "task.status":
		if code := e.Payload["activity_code"]; truthyJSON(code) {
			activity := rawString(code)
			t.busy = activity != "online" && activity != "idle"
		} else if status := e.Payload["status"]; truthyJSON(status) {
			state := rawString(status)
			t.busy = state != "completed" && state != "failed"
		}
	case "message.user", "delta.message_start", "delta.text_append", "delta.message_done", "message.assistant":
		if t.messageID != "" {
			t.apply(e, true)
		} else if len(t.pending) < maxPendingEvents {
			t.pending = append(t.pending, e)
		}
	}
}

// acknowledged says which message was ours, and replays what arrived before that was known.
func (t *turn) acknowledged(messageID string) {
	t.messageID = messageID
	for _, e := range t.pending {
		t.apply(e, false)
	}
	t.pending = nil
	t.poke()
}

func (t *turn) find(id string) *replyMessage {
	for _, m := range t.messages {
		if m.id == id {
			return m
		}
	}
	return nil
}

// apply folds one event into the answer. afterAck is whether the event arrived after the
// acknowledgment, when any new parentless message is taken to follow ours.
func (t *turn) apply(e chatEvent, afterAck bool) {
	id := e.messageID()
	if e.Event == "message.user" {
		if id != t.messageID {
			return
		}
		t.oursSeen = true
		// The stored message carries the words, which for a voice note are the transcript.
		if text := transcript(e.str("display_text")); text != "" && e.ready() {
			t.heard = text
		}
		return
	}

	m := t.find(id)
	if m == nil {
		if e.Event == "delta.text_append" {
			return
		}
		parent := e.str("reply_to_message_id")
		if parent == "" {
			parent = e.str("parent_message_id")
		}
		linked := parent != "" && (parent == t.messageID || t.find(parent) != nil)
		follows := parent == "" && (t.oursSeen || afterAck)
		if !(linked || follows) || len(t.messages) == maxReplyMessages {
			return
		}
		m = &replyMessage{id: id}
		t.messages = append(t.messages, m)
	}

	switch e.Event {
	case "delta.message_start":
	case "delta.text_append":
		m.text = capText(m.text + e.str("text"))
	default:
		final := e.str("display_text")
		if final == "" {
			final = e.str("content")
		}
		if final != "" {
			m.text = capText(final)
		}
		if e.Event == "delta.message_done" || e.ready() {
			m.done = true
		}
	}
}

// capText cuts on a character boundary, so a runaway reply cannot grow without end.
func capText(s string) string {
	if len(s) <= maxReplyText {
		return s
	}
	return strings.ToValidUTF8(s[:maxReplyText], "")
}

// text is the answer so far: every reply message that has words, a blank line between them.
func (t *turn) text() string {
	var parts []string
	for _, m := range t.messages {
		if m.text != "" {
			parts = append(parts, m.text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// whole says whether the answer is complete as far as anyone can tell: there are words, every
// reply message is done, and Muse is not working on more. Nothing on the stream marks the end of a
// turn, so this and a pause are all there is.
func (t *turn) whole() bool {
	if t.busy || len(t.messages) == 0 {
		return false
	}
	for _, m := range t.messages {
		if !m.done {
			return false
		}
	}
	return t.text() != ""
}

// truthyJSON is Python's idea of true for a JSON value, which is what the reference clients test.
func truthyJSON(raw json.RawMessage) bool {
	switch string(raw) {
	case "", "null", "false", "0", `""`, "{}", "[]":
		return false
	}
	return true
}

// transcript is what was said, out of a stored voice note's text: Muse follows the words with a line
// naming the recording, like "[file:audio/wav workspace/user/files/voice_note-64.wav]".
func transcript(display string) string {
	var said []string
	for _, line := range strings.Split(display, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "[file:") {
			said = append(said, line)
		}
	}
	return strings.Join(said, " ")
}
