#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// Scheduled tasks, like ChatGPT Tasks / cron: the firmware decides *when* (a fixed schedule it
// computes itself, no model polling a checklist), the model only *what*.
//   reminder  a fixed text, announced at the time (in a conversation the assistant says it itself)
//   prompt    an instruction the agent runs with all tools (web search, Home Assistant, mail,
//             speak); it delivers by mail, announcement and/or Telegram, or only acts (lights). A condition
//             ("only if it rains") lets it stay silent.
// Stored in LittleFS (/tasks.json), backed up to the mailbox (restored when missing). A run
// missed by more than TASK_CATCHUP_S (device off, long conversation) is skipped.
namespace tasks {

void begin();   // after memory::begin() (mounts LittleFS)
void update();  // main loop: starts due tasks
void setIdle(bool on);  // prompt tasks and the backup only run between conversations

// For the prompts: the planned tasks with id, schedule and next run ("" if none).
String context();
bool exists(int id);  // a task with this id is stored here

// Runs a task once right now, not stored (events from Home Assistant, call reports) for `person`
// (assistant/people id, "" = the admin). Main loop only: a reminder during a conversation goes
// to the model directly.
void runNow(const char* kind, const String& title, const String& text, const String& deliver,
            const String& person = "");

// Registers create_task, list_tasks, delete_task, pause_task and run_task (assistant/tools).
void addTools();

}  // namespace tasks
