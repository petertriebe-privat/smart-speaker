#pragma once

#include <Arduino.h>
#include <vector>

// The user's to-do lists in Google Tasks: what they see in the Google
// Tasks app on their phone. Over the Google account of services/google (scope tasks). Tools
// (slow, the admin's account: Private): list_todos, add_todo, change_todo. A task's id for the
// model is "<list id>/<task id>". The shopping list stays in Home Assistant.
namespace todos {

void addTools();

// An open to-do with a due date, for the agenda (assistant/agenda).
struct Todo {
    String id;     // "<list id>/<task id>"
    String title;
    String due;    // YYYY-MM-DD
    String list;   // its list's title when there are several lists, else ""
};
// The open to-dos of all lists due on `lastDay` (YYYY-MM-DD) or earlier, overdue ones included.
// Blocks for seconds (one request per list).
bool due(const String& lastDay, std::vector<Todo>& out, String& error);

}  // namespace todos
