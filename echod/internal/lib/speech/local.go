package speech

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Helper is the program that is the device's own voice: SVOX Pico (tools/pico/techo5-pico.c), which
// is C where the daemon has none. Looked up on PATH and next to the daemon. HelperData is where the
// image keeps the voice's data.
const (
	Helper     = "techo5-pico"
	HelperData = "/usr/share/techo5/pico"
)

// Local is the device's own voice: the helper and the directory of voice data it is given.
type Local struct {
	Bin  string
	Data string
}

// Installed is the device's own voice where the image puts it.
func Installed() Local {
	bin, err := exec.LookPath(Helper)
	if err != nil {
		bin = Helper
		if exe, err := os.Executable(); err == nil {
			bin = filepath.Join(filepath.Dir(exe), Helper)
		}
	}
	return Local{Bin: bin, Data: HelperData}
}

// Available is whether the helper and its data are there: an image built without the helper has no
// voice of its own.
func (l Local) Available() bool {
	bin, err := os.Stat(l.Bin)
	if err != nil || bin.IsDir() {
		return false
	}
	data, err := os.Stat(l.Data)
	return err == nil && data.IsDir()
}

// helperQuit is how long a killed helper is given to be gone before its pipes are closed under it.
const helperQuit = time.Second

// Speak says text and calls out with the samples as they are made, at 16 kHz. The helper runs for
// this one utterance: on the Show its first audio is out in about 60 ms and five seconds of voice
// in under 200, and a process that is not there between answers cannot hang or leak. It returns when the voice has ended, out has failed, or ctx is done,
// and the helper is gone by then.
func (l Local) Speak(ctx context.Context, text string, out func(samples []int16) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.Bin, l.Data)
	cmd.Stdin = strings.NewReader(text)
	cmd.WaitDelay = helperQuit
	var said bytes.Buffer
	cmd.Stderr = &said
	pcm, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("speech: the built-in voice: %w", err)
	}
	serr := stream(pcm, out)
	if serr != nil {
		// out failed, or the pipe did: either way nobody is listening any more.
		cancel()
	}
	werr := cmd.Wait()
	switch {
	case ctx.Err() != nil && serr == nil:
		return ctx.Err()
	case serr != nil:
		return serr
	case werr != nil:
		why := strings.TrimSpace(said.String())
		if len(why) > maxError {
			why = why[:maxError]
		}
		if why == "" {
			why = werr.Error()
		}
		return fmt.Errorf("speech: the built-in voice: %s", why)
	}
	return nil
}
