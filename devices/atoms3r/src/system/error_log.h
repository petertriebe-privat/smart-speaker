#pragma once

#include <Arduino.h>

// Errors worth reading later, kept in LittleFS (/errors.log) with the time: the serial log is
// gone unless a PC listens (a call once broke off and nobody could tell why). Failed
// sessions, broken-off calls, network trouble, the reason of every restart. Serial "L" prints
// it. The file is cut to its newer half beyond ERROR_LOG_BYTES.
namespace errorlog {

void begin();  // after LittleFS is mounted (memory::begin): logs why the device (re)started
// Appends one line (printf style) and prints it on the serial port. Any task.
void add(const char* format, ...) __attribute__((format(printf, 1, 2)));
void print();
String last();  // the newest line since the start, with its time ("" if none): HA's diagnostics

}  // namespace errorlog
