# The "Hey Muse" wake word

`hey_muse.tflite` and `hey_muse.json` are a microWakeWord model for the phrase "hey muse". The image
carries it beside the wake words TECHO5 fetches (tools/fetch-inputs.py), so every unit offers it under
Sound, Wake word, and a unit that has lost it gets it back at its next start.

## How it was made

Trained in October 2026 with
[microWakeWord-Trainer-AppleSilicon](https://github.com/TaterTotterson/microWakeWord-Trainer-AppleSilicon)
at commit `b8f7457`, which is built on [microWakeWord](https://github.com/kahrendt/microWakeWord)
(Apache 2.0):

    ./train_microwakeword_macos.sh "hey_muse" 50000 100 --language en --tts-mode hybrid

- 50,000 synthetic recordings of the phrase, from four speech models. After the trainer's own
  screening they were about 24,000 from Qwen3-TTS, 12,500 from Piper, 12,200 from MOSS-TTS and 2,200
  from OmniVoice. No recording of a real person was used.
- Mixed with room impulse responses (MIT), AudioSet, the Free Music Archive and CHiME-Home, and trained
  against microWakeWord's own sets of speech, dinner party talk and non-speech sound.
- 40,000 steps. The trainer's calibration chose a cutoff of 0.99 over a window of 6, at which it
  measured 98.6% of the phrase recognized and 0.21 false wakes an hour on 9.7 hours of ambient sound.

## What was tested

In the engine the device runs (zserge/microwakeword): 97% of 400 of the synthetic recordings woke it at
the device's default sensitivity, and nothing did in 50 minutes of AudioSet or 133 minutes of music. On
one Echo Show 5 2nd gen, one person found it reliable in one room. It has not been tried with other
voices, accents or rooms, which is why it is offered as a choice and is not what a new unit listens
for.

`hey_muse.json` is the manifest in the form the device reads. The trainer's own, with its calibration
figures, is not kept here.
