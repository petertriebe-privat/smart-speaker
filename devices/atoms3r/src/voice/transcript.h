#pragma once

#include <Arduino.h>

// What was said in the conversation. GPT Live sends the transcripts word by word: the serial
// log gets whole lines per speaker, the display the end of the conversation.
namespace transcript {

enum class Speaker { None, User, Assistant };

void clear();  // new conversation
void append(Speaker speaker, const char* text);
void flush();   // logs the pending line now
void update();  // logs the pending line after a short pause

Speaker lastSpeaker();
const String& text();  // for the display: ASCII only, the user's lines start with "> "
// The whole conversation for the memory ("Du: ...\nJarvis: ...\n"), MEMORY_TRANSCRIPT_CHARS at most.
const String& full();
// How the other side is called in the log and full(): "Du" (the user), "Gegenüber" (someone
// The assistant called for the user). Set before a conversation starts.
void setUserName(const char* name);

}  // namespace transcript
