#pragma once

#include <Arduino.h>

// Printing on the network printer over IPPS (IPP over TLS, self-signed certificate).
// The EPSON ET-4850 accepts JPEG, PWG/Apple raster and ESC/P-R, but neither PDF nor text.
// Generated images (services/images) go to the printer unchanged as JPEG.
// Documents go by e-mail instead (mail.cpp, print_document): Epson Connect renders HTML.
namespace printer {

bool configured();

// Prints one JPEG as its own job once the printer is idle (waits up to PRINTER_WAIT_MS).
// Blocks; call it from a background task. Returns the job id (-1 = accepted, id unknown),
// or 0 with `error` set.
// landscape: the page turned (orientation-requested 4) for an image wider than high.
int printJpeg(const uint8_t* jpeg, size_t length, const String& jobName, bool color, bool landscape, String& error);

}  // namespace printer
