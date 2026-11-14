// The assistant's Drive folder as an Obsidian vault, as core/internal/vault: every note is Markdown
// with properties (YAML front matter) and wikilinks; file names are what a wikilink can reach and
// Windows can store. Notes of the old text format are read too (history, header); converting them
// is the Echo's work (core/internal/google/archive.go, Tidy).
#pragma once

#include <Arduino.h>

namespace vault {

constexpr const char* MIME = "text/markdown";
constexpr const char* EXT = ".md";
constexpr const char* HISTORY_MARKER = "\n## Verlauf\n";      // dossier: header, then entries
constexpr const char* LEGACY_HISTORY_MARKER = "\nVerlauf:\n";  // the same in the old text format
constexpr const char* ENTRY_PREFIX = "\n### ";                 // a history entry "### <time> – <heading>"

// Text as a note's file name (without extension) that a wikilink reaches.
String fileName(const String& text);
// A wikilink to the note `name`, with `label` when it differs.
String link(const String& name, const String& label);
// File names of a conversation ("2026-11-14 18.38 – title"), of the night's review, and of an
// archive note from its mail subject ("Thema [key]: title", "Gespräch <time> – title", "Traum <day>").
String conversationName(const String& at, const String& title);
String dreamName(const String& day);
String nameFromSubject(const String& subject);

// The notes. `keywords` and `topics` are lists "a, b"; `with` "" = the household; `summary` "" = not
// summarized.
String conversationNote(const String& at, const String& title, const String& with, const String& channel,
                        const String& summary, const String& keywords, const String& topics, const String& transcript);
String dreamNote(const String& day, const String& diary, const String& report);
String topicHeader(const String& key, const String& title, const String& status, const String& keywords,
                   const String& updated, const String& created, bool done);
// A history entry: at `at`, `heading` (a conversation's title), from the note `from` ("" = none).
String entry(const String& at, const String& heading, const String& from, const String& text);

// A dossier's history after its marker (either format, old entries in the new one); its header
// ends at headerEnd (-1: no history).
String history(const String& dossier);
int headerEnd(const String& dossier);
// A note without its front matter; its first two lines with text (for a search hit).
String body(const String& text);
String head(const String& text);

}  // namespace vault
