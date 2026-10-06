# Muse

A TECHO5 device can hand its voice turns to [Muse](https://gadgets.muse.ai), Meta's AI agent,
in place of Home Assistant's Assist pipeline or the direct pipeline. After the wake word, what you
say goes to your own Muse as a voice note. Muse answers in words, and the device says them.

The Echo is the voice and nothing more. Muse answers as itself, with whatever your Muse account can
already do. Smart home control is Muse's own business, for example through Muse Home Link, and
nothing here adds it. Muse does get a short list of things it can do on the device itself. They
are timers, alarms, the volume, the radio and three pages on the screen.

It is off until you choose it, and choosing it takes the device's voice turns away from Home
Assistant. Everything else the device does with Home Assistant carries on.

> **Not made by Meta.** TECHO5 is a hobby project. Meta did not make it and does not endorse it.
> Muse is Meta's. Using it this way is covered by Meta's
> [Gadget SDK Terms](https://gadgets.muse.ai/sdk-terms), which you accept when you create an SDK
> token. Read them before you pair.

## What you need

- A Muse subscription, and the Muse app on your phone.
- An SDK token of your own from
  [gadgets.muse.ai](https://gadgets.muse.ai/settings/sdk-tokens) (Account → SDK tokens). It starts
  with `mgst_`. The token is personal. Do not publish it, do not put it in an image or a release,
  and do not share it with another household. One token covers at most 50 devices.
- A TECHO5 image that has Muse in it. To have the built-in voice, the image also needs the
  `techo5-pico` helper ([building.md](building.md#optional-the-built-in-voice)).
- A key for an OpenAI-style speech endpoint, only if you want a cloud voice in place of the
  built-in voice.

Meta's terms do not allow selling a device that carries the SDK. Take the token off the device and
unpair it before you give the device away.

## What the device does with your voice

Read this before you pair, and tell the people you live with.

- The wake word is detected on the device. Nothing leaves the device until the wake word is heard.
- After the wake word, the device records until the speaker stops, for at most 60 seconds. It
  sends that recording to Muse as a WAV voice note.
- Muse stores the recording on the message in your Muse account and transcribes it.
- Anyone who speaks to the device after the wake word is recorded into your account the same way.
  Muse cannot tell a guest from you.
- With a cloud voice, the text of Muse's answer goes to the speech provider. That is OpenAI unless
  you set another endpoint.
- With the built-in voice, nothing else leaves the device.
- The pairing credentials are kept on the device in `/data/misc/techo5/muse.json`, readable only
  by root. The SDK token and the speech key are kept with the device's other settings. A
  diagnostics bundle records that the mode is `muse` and none of those secrets.

## Set it up

1. Open the device's setup page (Settings → Privacy → Setup page) and go to **Sound & Voice**.
2. Under **Voice assistant**, set **Answered by** to **Muse, paired with the Muse app**.
3. Paste your token into **Muse SDK token**.
4. Choose a **Speaking voice**. See [Choose a voice](#choose-a-voice).
5. Press **Save**. The page refuses the save if the token is missing, or if the image has no
   built-in voice and you gave no speech key.
6. Under **Muse pairing**, press **Pair with the Muse app**. The device goes on Bluetooth for 10
   minutes under a name like `MuseGadgetA1B2C3`, and the panel shows that name.
7. On the phone, open the Muse app and go to **Settings → Devices**. Turn on **Developer mode**.
8. Tap **Add Device** (the **+** at the top right) and pick the `MuseGadget` name from step 6.
9. Muse warns that this is a community device. Continue if the device is yours.
10. When the app asks for Wi-Fi, pick the one network it shows. The device is already online, so
    the app asks for no password.

The panel follows the pairing on its own and ends on **Paired as** with your Muse user name **and
connected to Muse**. The device now shows up in the Muse app's device list. Say the wake word and
ask something.

The pairing survives a reboot. The device reconnects by itself.

Pairing was tested with `musepair` from the command line, not from this panel. See
[What was tested](#what-was-tested) and [Pair from the command line](#pair-from-the-command-line).

Pairing keeps the setup secrets from someone who is only listening. It does not prove who the phone
is, so it cannot stop someone in the middle. The device is open to pairing only while the 10 minutes
run. Pair on a network you trust, and press **Stop pairing** if you change your mind.

## The wake word

The image carries a wake word of its own, **Hey Muse**, beside TECHO5's. Choose it on the device
(swipe down from the top, **Sound**, **Wake word**). It is a choice and not what a new device listens
for: it was trained on synthetic voices only and tried by one person, on one Show 5, in one room.
[tools/wake/README.md](../tools/wake/README.md) says how it was made.

## Choose a voice

Muse answers in text, so the device has to say it.

| Voice | What it needs | What leaves the device |
|---|---|---|
| **Built in** | The `techo5-pico` helper in the image | Nothing more |
| A cloud voice (`alloy`, `coral` and nine others) | A **Speech key** | The answer's text, to the speech endpoint |

The built-in voice is SVOX Pico, in U.S. English only. It sounds like a machine from 2009, and it
is fast. It makes 5 seconds of speech in about 200 ms on a Show 5.

With no speech key, the built-in voice speaks whatever the setting says. With a key, the cloud
voice speaks, and the built-in voice takes over for an answer when the endpoint fails before it
has said anything (a bad key, a spent key, no network).

**How to say it** takes a few words of direction for a model that accepts them, such as "warmly,
and not too fast". **Speech endpoint** and **Speech model** are for another OpenAI-style
`/audio/speech` server. Left empty, they mean `https://api.openai.com/v1` and `gpt-4o-mini-tts`.

On a Show or a Spot the voice can also be changed on the device, under Settings → Sound →
**Speaking voice**.

## What Muse can do on the device

Muse is offered these commands and decides for itself when to call one. They act on this device
only. There is nothing here that reaches another device, your music library, the web or Home
Assistant.

| Command | What it does |
|---|---|
| `echo.start_timer`, `echo.list_timers`, `echo.cancel_timers` | Timers on this device |
| `echo.set_alarm`, `echo.list_alarms`, `echo.delete_alarm` | Alarms and reminders on this device |
| `echo.stop` | Stops what the device is ringing or playing |
| `echo.set_volume` | Sets the volume |
| `echo.find_radio`, `echo.play_radio`, `echo.save_station`, `echo.list_stations` | Internet radio |
| `echo.show_on_screen` | Shows the forecast, the rain map or the calendar |

The list is a table in `echod/internal/feature/assistant/muse.go`. A tool that is not in that table
is not offered.

## The avatar

When Muse answers, a Show can draw Muse's character in the middle of the screen for a turn. The
character listens, thinks and talks as the turn does. With the character, a turn shows no words: the
answer is spoken, and what Muse heard and answered is in your Muse chat. Without it, a turn shows the
words as it does for any other assistant. The Spot and the Dot do not draw it.

The character is Meta's. Meta says its Apache license does not cover the character, so neither the
character nor the code that draws it is in this repository or in any release. You make the pictures
from your own checkout of Meta's SDK, for your own device. Do not publish them.

You need a C compiler, [uv](https://docs.astral.sh/uv/) and a checkout of
[muse-gadget-sdk](https://github.com/facebookincubator/muse-gadget-sdk).

```sh
uv run --with pillow tools/muse-avatar/make_sprites.py --sdk ~/src/muse-gadget-sdk
scp -r build/muse-avatar root@<address>:/data/misc/techo5/
ssh root@<address> killall techo5
```

The daemon restarts and logs `Muse avatar loaded`. To take the character off again, delete
`/data/misc/techo5/muse-avatar` and restart the daemon the same way. The details are in
[tools/muse-avatar/README.md](../tools/muse-avatar/README.md).

## How long an answer takes

On an Echo Show 5 2nd gen, a turn took 7 to 11 seconds from the end of speech to the end of the
answer. Muse's own reply was 4 to 8 seconds of that. The device starts speaking at the first pause
in Muse's answer.

## What was tested

Tested on one Echo Show 5 2nd gen (`cronos`) on 2026-10-05:

- The image installed into the spare slot, and the device booted from it.
- Pairing from the Muse app on an iPhone, over the Echo's own Bluetooth.
- The device in the Muse app's device list.
- Wake word, to Muse, to a spoken answer with the cloud voice.
- The built-in voice.
- The avatar on the screen.
- Muse calling device commands (a timer, the volume).
- Reconnecting after a reboot.

Not tested:

- Pairing from the Muse app on Android.
- The Echo Show 5 1st gen, the Show 8, the Spot and the Dot.
- Pairing while Bluetooth earbuds are connected.
- The setup page's **Muse pairing** panel driven from a browser. Pairing was done with `musepair`.

If you try one of these, an issue that says what happened helps the next person.

## Troubleshooting

**The iPhone connects, then drops in the middle of setup.** bluetoothd's battery plugin reads a
connecting phone's battery level. An iPhone answers that only over a bonded link, so bluetoothd
asks the phone to bond and drops it when the phone refuses. The image starts bluetoothd with
`--noplugin=battery` for this reason. If you see the drop, the image is older than that fix. Check
with `ps | grep bluetoothd` on the device.

**Setup stalls after the Wi-Fi step.** bluetoothd offers an ATT MTU of 517 by default. The Muse
app on Android writes packets that fill the MTU, and Android refuses a write over 512 bytes. The
image sets `ExchangeMTU = 256` in `/etc/bluetooth/main.conf`, which is what Meta's own installer
sets. Check that the line is there. Android pairing itself was not tested here.

**The panel says "Paired as ..., but not connected to Muse".** The device keeps trying. The reason
follows the colon. The daemon's log has the rest.

**The pairing was revoked.** When Muse refuses the device's token outright, for example after you
remove the device in the Muse app, the log says `muse: pairing revoked; the device has to be paired
again`. The device drops the credentials and the panel goes back to **Not paired**. The device
keeps its identity, so pairing again brings it back under the same name. This path is covered by
tests and was not seen on the test device.

**Unpair.** Press **Unpair** in the **Muse pairing** panel. The device forgets its tokens and
keeps its identity. Muse is not told, so also remove the device in the Muse app under **Settings →
Devices**. To forget the identity too, delete `/data/misc/techo5/muse.json` and restart the
daemon.

**"Pair with the Muse app" answers "save the Muse SDK token first".** The Muse app does not pair a
device that has no token. Save the token and try again.

**A second device or tool is using the same pairing.** One identity holds one session. Running
`musecheck` with a copy of the device's `muse.json` while the device is connected makes each drop
the other.

## Pair from the command line

`musepair` runs one pairing on the device and writes the result to a file. It is how the tested
device was paired. It needs SSH turned on
([Set the SSH authorized keys](actions.md#set-the-ssh-authorized-keys)).

Build it on your computer and copy it over, with your token in a file:

```sh
cd echod
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o ../bin/musepair-arm ./cmd/musepair
scp ../bin/musepair-arm token.txt root@<address>:/tmp/
```

On the device, pair into the file the daemon reads, then restart the daemon:

```sh
chmod 600 /tmp/token.txt
/tmp/musepair-arm -sdk-token-file /tmp/token.txt -out /data/misc/techo5/muse.json
rm /tmp/token.txt
killall techo5
```

`musepair` logs the `MuseGadget` name to pick in the Muse app, then each step of the pairing, and
ends with `musepair: paired`. It writes the file only once Muse has accepted the tokens. The setup
page still needs the SDK token saved and **Answered by** set to Muse.

`MUSEPAIR_DEBUG=1` logs every write. `-window 20m` keeps pairing open longer.

## Check a pairing

`musecheck` connects to Muse with a pairing file, says one thing, and prints what Muse heard and
what Muse answered. It runs on your computer.

```sh
cd echod
go run ./cmd/musecheck -state state.json -text "what time is it?"
go run ./cmd/musecheck -state state.json -sdk-token token.txt -wav question.wav
```

Two things to know before you point it at a device's own `muse.json`:

- Stop the device's connection first. Set **Answered by** back to Home Assistant, which leaves the
  pairing in place and disconnected.
- Connecting can rotate the tokens, and the old pair stops working at that moment. `musecheck`
  rewrites its state file when that happens. Copy the file back to the device afterward, or the
  device's copy is dead.

A pairing made for `musecheck` alone, with `musepair -out` to a file of its own, avoids both.

## How it works

- `echod/internal/lib/muse`: the client, a port to Go of the Linux client in Meta's Muse Gadget
  SDK. One WebSocket to the owner's Muse carries a Noise XX session. Inside it, one stream brings
  commands, one brings everything Muse says, and each thing said to Muse is a stream of its own.
- `echod/internal/lib/muse/pairing`: the Bluetooth setup conversation the Muse app holds with a
  gadget. `echod/internal/lib/bluez/gatt.go` puts it on the air through bluetoothd.
- `echod/internal/feature/muse`: owns the pairing file and keeps the client up while Muse is what
  answers.
- `echod/internal/feature/voice/muse.go`: the turn. The first pause in Muse's answer is taken as
  the whole answer and spoken there and then.
- `echod/internal/lib/speech`: the cloud voice and the built-in voice.
- `echod/internal/lib/avatar` and `echod/internal/feature/display/render_avatar.go`: the character
  on the turn screen.

The ported code is Apache-2.0. [NOTICE](../NOTICE) lists the files.
