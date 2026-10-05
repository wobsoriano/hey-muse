package pairing_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

// sdkEnv names the SDK checkout's linux/src directory. The SDK is not part of this repository.
const sdkEnv = "MUSE_GADGET_SDK"

// pipeLink is a Transport whose phone is another process.
type pipeLink struct {
	mu  sync.Mutex
	mtu int
	to  io.Writer
}

func (l *pipeLink) MTU() int          { return l.mtu }
func (l *pipeLink) Disconnect() error { return nil }

func (l *pipeLink) Send(packets [][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range packets {
		if _, err := fmt.Fprintf(l.to, "N %s\n", hex.EncodeToString(p)); err != nil {
			return err
		}
	}
	return nil
}

// TestSDKPhone pairs with a phone built from the SDK's own Python pairing and framing code, so the
// two implementations have to agree on the transcript, the keys, the records and the chunking.
func TestSDKPhone(t *testing.T) {
	sdk := os.Getenv(sdkEnv)
	if sdk == "" {
		t.Skipf("%s is not set", sdkEnv)
	}
	if _, err := os.Stat(sdk + "/musegadget/pairing.py"); err != nil {
		t.Skipf("%s does not hold the SDK: %v", sdkEnv, err)
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv is not installed")
	}
	for _, mtu := range []int{23, 185} {
		t.Run(fmt.Sprintf("mtu %d", mtu), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, uv, "run", "--no-project", "--with", "cryptography",
				"python", "testdata/sdk_phone.py", sdk, fmt.Sprint(mtu))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdin, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Skipf("could not start uv: %v", err)
			}
			defer cmd.Wait()
			defer stdin.Close()
			lines := bufio.NewScanner(stdout)
			if !lines.Scan() || lines.Text() != "READY" {
				t.Skipf("the SDK phone did not start (no cryptography package offline?): %s", stderr.String())
			}

			saved := make(chan pairing.Result, 1)
			c, err := pairing.New(pairing.Config{
				Device:    device,
				Transport: &pipeLink{mtu: mtu, to: stdin},
				SDKToken:  "mgst_interop",
				Network: pairing.AlreadyOnline{
					Reachable: func(context.Context) bool { return true },
					SSID:      func() string { return "HomeNet" },
				},
				Verify: func(context.Context, pairing.Result) error { return nil },
				Save:   func(r pairing.Result) error { saved <- r; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Run(ctx) }()

			finished := false
			for lines.Scan() {
				kind, payload, _ := strings.Cut(lines.Text(), " ")
				if kind == "DONE" {
					finished = true
					break
				}
				packet, err := hex.DecodeString(payload)
				if kind != "W" || err != nil {
					t.Fatalf("unexpected line from the SDK phone: %q", lines.Text())
				}
				c.OnWrite(packet)
			}
			if !finished {
				cancel()
				t.Fatalf("the SDK phone gave up: %s", stderr.String())
			}
			if err := <-done; err != nil {
				t.Fatalf("Run = %v", err)
			}
			want := pairing.Result{
				AccessToken: "interop-access", RefreshToken: "interop-refresh", TokenType: "device", Username: "someone",
				APIURL: "https://legacy-api.example", APIURLV2: "https://api.example", NoiseHost: "noise.example",
			}
			if got := <-saved; got != want {
				t.Fatalf("saved %+v", got)
			}
		})
	}
}
