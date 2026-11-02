#pragma once

#include <Arduino.h>

// The assistant on WhatsApp (Cloud API): the people of the household write to the household's number
// from their phones; The assistant answers as that number. Meta delivers incoming messages only by
// webhook, so a small relay (services/whatsapp-relay/, a Cloudflare Worker) takes them and this module
// long-polls it (WHATSAPP_RELAY_URL / WHATSAPP_RELAY_TOKEN); answers go straight to the Graph API
// (WHATSAPP_PHONE_NUMBER_ID, WHATSAPP_ACCESS_TOKEN). Only between conversations, in a task of its
// own. A sender is a person by their phone number (assistant/people); others are reported to the
// admin once and not answered. Like Telegram: one Responses thread per person until the chat is
// quiet for TELEGRAM_THREAD_MS, then it goes to the memory; voice notes are transcribed and
// answered with a voice note too; photos go to the model. Free-form answers are only possible
// within 24 hours of the person's last message (WhatsApp's service window).
// Tools: send_whatsapp.
namespace whatsapp {

bool configured();  // relay and Graph API set in secrets.h
// Registers send_whatsapp (assistant/tools) and starts the polling task.
void addTools();
void setIdle(bool on);  // polling only while no conversation is open

// Sends `text` (WhatsApp formatting) to a person's WhatsApp ("" = the admin), from any task.
// Blocks for a second or two. Returns "sent" or an error.
String send(const String& text, const String& person = "");

}  // namespace whatsapp
