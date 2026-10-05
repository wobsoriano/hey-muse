// techo5-pico — the device's own voice: SVOX Pico as a small helper the daemon runs per utterance.
//
// The daemon is static Go with no C in it, and Pico is C. So this process sits between: it reads the
// text to say from stdin, UTF-8, to its end, and writes the voice to stdout as it is made — int16
// little-endian, mono, at 16 kHz, which is what Pico makes and what the device plays. Anything that
// goes wrong is said on stderr and the exit status is not 0.
//
//   techo5-pico <voice data directory>
//
// The directory holds the two files of the one voice there is, en-US (tools/pico/voice).
//
// Build: tools/linux/build-pico.sh (zig cc, armv7 musl, static).

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "picoapi.h"
#include "picodefs.h"

// What the engine's authors give it for one English voice.
#define ENGINE_MEMORY 2500000
#define VOICE "techo5"

// stdout is written 100 ms of voice at a time: the daemon hands audio on in pieces no smaller than
// that, and a write per engine step (4 ms) would wake it twenty-five times for each.
#define OUT_BYTES 3200

static pico_System pico;
static pico_Engine engine;

static void fail(const char *what, pico_Status status)
{
	pico_Retstring why = "";
	if (pico && status != PICO_OK) {
		pico_getSystemStatusMessage(pico, status, why);
	}
	fprintf(stderr, "techo5-pico: %s%s%s\n", what, why[0] ? ": " : "", why);
	exit(1);
}

static void load(const char *dir, const char *file)
{
	char path[1024];
	pico_Retstring name;
	pico_Resource resource = NULL;
	pico_Status status;
	if (snprintf(path, sizeof(path), "%s/%s", dir, file) >= (int)sizeof(path)) {
		fail("the voice data path is too long", PICO_OK);
	}
	FILE *f = fopen(path, "rb");
	if (!f) {
		fprintf(stderr, "techo5-pico: %s: %s\n", path, strerror(errno));
		exit(1);
	}
	fclose(f);
	if ((status = pico_loadResource(pico, (const pico_Char *)path, &resource)) != PICO_OK
	    || (status = pico_getResourceName(pico, resource, name)) != PICO_OK
	    || (status = pico_addResourceToVoiceDefinition(pico, (const pico_Char *)VOICE, (const pico_Char *)name)) != PICO_OK) {
		fprintf(stderr, "techo5-pico: %s: ", path);
		fail("not voice data the engine takes", status);
	}
}

int main(int argc, char **argv)
{
	if (argc != 2) {
		fprintf(stderr, "usage: techo5-pico <voice data directory> < text > pcm\n");
		return 2;
	}

	// The engine reads plain text. Anything outside ASCII, emoji above all, would be spelled out or
	// stall it, and a newline is only a space. The terminator is sent too: it tells the engine the
	// text is complete.
	size_t n = 0, room = 4096;
	char *text = malloc(room);
	for (int c; text && (c = getchar()) != EOF;) {
		if (c >= 0x80 || c == 0) {
			continue;
		}
		text[n++] = c == '\n' || c == '\r' ? ' ' : (char)c;
		if (n + 1 == room) {
			text = realloc(text, room *= 2);
		}
	}
	if (!text) {
		fail("out of memory", PICO_OK);
	}
	if (ferror(stdin)) {
		perror("techo5-pico: stdin");
		return 1;
	}
	text[n++] = 0;

	pico_Status status;
	void *memory = malloc(ENGINE_MEMORY);
	if (!memory) {
		fail("out of memory", PICO_OK);
	}
	if ((status = pico_initialize(memory, ENGINE_MEMORY, &pico)) != PICO_OK
	    || (status = pico_createVoiceDefinition(pico, (const pico_Char *)VOICE)) != PICO_OK) {
		fail("the engine did not start", status);
	}
	load(argv[1], "en-US_ta.bin");
	load(argv[1], "en-US_lh0_sg.bin");
	if ((status = pico_newEngine(pico, (const pico_Char *)VOICE, &engine)) != PICO_OK) {
		fail("the engine did not take the voice", status);
	}

	static char outbuf[OUT_BYTES];
	setvbuf(stdout, outbuf, _IOFBF, sizeof(outbuf));
	for (size_t sent = 0; sent < n;) {
		pico_Int16 took = 0;
		size_t rest = n - sent;
		if ((status = pico_putTextUtf8(engine, (const pico_Char *)text + sent, (pico_Int16)(rest > 256 ? 256 : rest), &took)) != PICO_OK) {
			fail("the engine did not take the text", status);
		}
		sent += (size_t)took;
		do {
			short samples[256];
			pico_Int16 bytes = 0, kind = 0;
			status = pico_getData(engine, samples, sizeof(samples), &bytes, &kind);
			if (bytes > 0 && fwrite(samples, 1, (size_t)bytes, stdout) != (size_t)bytes) {
				perror("techo5-pico: stdout");
				return 1;
			}
		} while (status == PICO_STEP_BUSY);
		if (status != PICO_STEP_IDLE) {
			fail("the engine stopped", status);
		}
	}
	if (fflush(stdout) != 0) {
		perror("techo5-pico: stdout");
		return 1;
	}
	return 0;
}
