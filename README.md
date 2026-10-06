# TECHO5 Muse

An Amazon Echo Show 5 that answers as [Muse](https://gadgets.muse.ai), Meta's AI agent. Say the wake
word, ask something, and your own Muse answers out loud. No Alexa, no Amazon cloud, and no Home
Assistant needed.

This is a fork of [TECHO5](https://github.com/HuskerMinion/techo5) by HuskerMinion, which replaces
the Echo's Android with a small Linux and one Go daemon. TECHO5 does the hard part: the kernel, the
microphones, echo cancellation, the wake word, the screen. This fork adds Muse as one more choice for
who answers. Everything else about the device is TECHO5's, and its own README is kept here as
[README.upstream.md](README.upstream.md).

> **Not made by Meta or Amazon.** This is a hobby project. Neither company made it or endorses it.
> Using Muse this way is covered by Meta's [Gadget SDK Terms](https://gadgets.muse.ai/sdk-terms).

## What this fork adds

- **Muse as the voice assistant.** A third choice beside Home Assistant and TECHO5's direct
  pipeline, picked on the setup page. Home Assistant keeps working for anyone who uses it.
- **Pairing with the Muse app over the Echo's own Bluetooth.** No other device is involved. The Echo
  then shows up in the Muse app's device list like any Muse gadget.
- **A voice with no server at home.** Answers are spoken through an OpenAI-style speech endpoint, or
  by a built-in voice (SVOX Pico) that needs no key and no internet.
- **A "Hey Muse" wake word,** beside TECHO5's own, chosen on the device.
- **Commands offered to Muse.** The device tells Muse it can set timers and alarms, change the
  volume, play the radio and show a few screen pages. Muse has not been seen to use them yet.
- **The Muse character on screen** during a turn, if you generate it from your own copy of Meta's
  SDK. The character is Meta's and is not in this repository.

Smart home control is not part of this. The Echo is the voice. Reaching your lights and speakers is
Muse's own business, for example through [Muse Home Link](https://gadgets.muse.ai/home-link).

## What you need

- An Echo Show 5 that TECHO5 supports, with its bootloader unlocked.
  [TECHO5's getting started guide](docs/getting-started.md) covers which models and how.
- A Muse subscription, the Muse app, and an SDK token of your own from
  [gadgets.muse.ai](https://gadgets.muse.ai/settings/sdk-tokens). The token is personal. Never
  publish it.
- Optional: a key for an OpenAI-style speech endpoint, for a more natural voice.

## Install

On an Echo Show 5 2nd gen with its bootloader unlocked and LineageOS 18.1 on it, which is where
[TECHO5's getting started guide](docs/getting-started.md) leaves you:

```sh
git clone https://github.com/wobsoriano/techo5-muse
cd techo5-muse
python3 tools/install-show.py
```

It needs Python 3, `adb` and `fastboot`, on Windows, Linux or macOS. It downloads the latest release
of this fork, checks it against the fork's signing key, and asks once before it erases LineageOS.
[docs/install.md](docs/install.md) is TECHO5's guide to the same installer.

A device already running upstream TECHO5 does not move to this fork by itself, since it trusts
upstream's key. [docs/releasing-the-fork.md](docs/releasing-the-fork.md) says how to move one over.

Then follow [docs/muse.md](docs/muse.md): choose Muse on the setup page, paste your token, and pair
in the Muse app. A device on this fork finds later releases of it by itself.

The release carries a boot image for the Show 5 2nd gen only, the one board this was tried on.

## What your voice does

- The wake word is detected on the device. Nothing is sent while it waits.
- After the wake word, what you say (60 seconds at most) goes to Muse as a recording. Muse keeps it
  on the message in your Muse account and transcribes it there. Anyone who speaks to the device is
  recorded into your account the same way.
- With a cloud voice, the text of each answer goes to the speech provider. With the built-in voice,
  nothing else leaves the device.

[docs/muse.md](docs/muse.md) has the details.

## What was tested

On one Echo Show 5 2nd gen (cronos), in October 2026:

- Installing the image and booting from it, and reconnecting to Muse after a reboot.
- Pairing from the Muse app on an iPhone.
- Wake word, question, spoken answer, with the cloud voice and with the built-in voice.
- The character on screen.
- The "Hey Muse" wake word, with one voice.

Not tested: Muse actually calling the device's commands (it never has, in two days of use), pairing
from Android, the Show 5 1st gen, the Show 8, the Spot and the Dot, pairing while
Bluetooth earbuds are connected, the setup page's pairing button (pairing was done from the command
line), and this fork with a Home Assistant server attached.

An answer took 7 to 11 seconds from the end of the question, most of it Muse thinking.

## Credits

- [TECHO5](https://github.com/HuskerMinion/techo5) by HuskerMinion, which this is a fork of, and
  [EchoLocal](echod/README.upstream.md) by Yuri Gelfand, which TECHO5 is built on. Both MIT.
- Meta's [Muse Gadget SDK](https://github.com/facebookincubator/muse-gadget-sdk), Apache 2.0. The
  Muse client and the pairing code here are ports of it.
- SVOX Pico, Apache 2.0, for the built-in voice.

[NOTICE](NOTICE) lists everything and its license.

## License

MIT, as TECHO5 is. See [LICENSE](LICENSE), [echod/LICENSE](echod/LICENSE) and [NOTICE](NOTICE).
