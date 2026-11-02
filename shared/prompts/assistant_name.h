#pragma once

// The assistant's name as users say and hear it (#29). The M5 takes it from secrets.h; the Echo
// gets the prompts with a placeholder (core/tools/extract_prompts.py) and fills in
// assistant_name from its settings at start.
#if __has_include("secrets.h")
#include "secrets.h"
#endif
#ifndef ASSISTANT_NAME
#define ASSISTANT_NAME "Jarvis"
#endif
