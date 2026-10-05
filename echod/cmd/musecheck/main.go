// Command musecheck checks a pairing against the live Muse service from a laptop: it connects,
// registers, says one thing to Muse in text or as a voice note, and prints what Muse heard and
// what it answered.
//
//	musecheck -state state.json -text "what time is it?"
//	musecheck -state state.json -sdk-token token.txt -wav question.wav
//
// The state file is the JSON of a muse.State. Connecting can rotate the device tokens, which
// kills the old pair, so the file is rewritten the moment that happens. One identity holds one
// session: stop anything else that is using the same pairing first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/muse"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "musecheck:", err)
		os.Exit(1)
	}
}

func run() error {
	statePath := flag.String("state", "", "state file: the JSON of a muse.State, rewritten when the tokens rotate")
	sdkTokenPath := flag.String("sdk-token", "", "file holding the SDK token, if there is one")
	text := flag.String("text", "", "say this to Muse")
	wavPath := flag.String("wav", "", "say this WAV recording to Muse")
	name := flag.String("name", "musecheck", "what the Muse app calls this device")
	timeout := flag.Duration("timeout", 3*time.Minute, "give up after this long")
	verbose := flag.Bool("v", false, "log more")
	flag.Parse()

	if *statePath == "" || (*text == "") == (*wavPath == "") {
		flag.Usage()
		return errors.New("give -state and one of -text or -wav")
	}
	var in muse.AskInput
	if *wavPath != "" {
		wav, err := os.ReadFile(*wavPath)
		if err != nil {
			return err
		}
		in = muse.VoiceNote(wav)
	} else {
		in = muse.Text(*text)
	}
	var sdkToken string
	if *sdkTokenPath != "" {
		b, err := os.ReadFile(*sdkTokenPath)
		if err != nil {
			return err
		}
		sdkToken = strings.TrimSpace(string(b))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	store := muse.FileStore{Path: *statePath}
	if _, err := store.Load(); err != nil {
		return err
	}
	states := make(chan muse.ConnState, 64)
	client, err := muse.New(muse.Config{
		Store:       store,
		DisplayName: *name,
		Version:     "musecheck",
		SDKToken:    sdkToken,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
		OnState: func(s muse.ConnState) {
			select {
			case states <- s:
			default:
			}
		},
	})
	if err != nil {
		return err
	}
	ended := make(chan error, 1)
	go func() { ended <- client.Run(ctx) }()

	for online := false; !online; {
		select {
		case s := <-states:
			online = s.Phase == muse.Online
		case err := <-ended:
			if errors.Is(err, muse.ErrUnpaired) {
				return fmt.Errorf("%s holds no working pairing", *statePath)
			}
			return fmt.Errorf("could not connect: %w", err)
		}
	}

	reply, err := client.Ask(ctx, in, func(e muse.ReplyEvent) {
		switch e := e.(type) {
		case muse.Heard:
			fmt.Println("heard:", e.Text)
		case muse.Settled:
			fmt.Println("settled:", e.Text)
		}
	})
	// The connection is closed and Run has returned before this exits, so a token rotation
	// that was in flight has been saved.
	cancel()
	<-ended
	if err != nil {
		return err
	}
	fmt.Println("reply:", reply.Text)
	return nil
}
