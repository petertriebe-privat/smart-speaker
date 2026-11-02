#pragma once

#include <Arduino.h>

// The assistant as a Telegram bot: the people of the household (assistant/people, the admin from
// TELEGRAM_OWNER_ID in secrets.h) chat with him from their phones (TELEGRAM_BOT_TOKEN). Outgoing only, no server: between conversations the
// registry's worker long-polls api.telegram.org over one kept TLS connection (and holds the
// TLS gate meanwhile, handing both over when another background job wants them). Each message
// from a person runs through the agent with their tools, in their own thread (previous_response_id)
// until the chat is quiet for TELEGRAM_THREAD_MS; then it goes to the memory like a
// conversation. Messages from accounts not in the register are reported to the admin, once,
// and not answered. Voice notes are transcribed and answered
// with a voice note as well (TTS as Ogg Opus); photos go to the model as images.
// Tools: send_telegram; create_image / send_last_image send photos (telegram true).
namespace telegram {

bool configured();  // token and owner set
// Registers send_telegram (assistant/tools) and the polling as background work of the
// registry's worker. With a token but no owner the bot answers everyone with their user id
// (for the setup) and does nothing else.
void addTools();
void setIdle(bool on);  // polling only while no conversation is open

// Sends to a person's chat (assistant/people id, "" = the admin's) over a connection of its own,
// from any task. `html`: Telegram HTML (b, i, a href, code, pre); falls back to plain text if
// Telegram rejects the markup. Long texts go as several messages. Blocks for seconds. Returns
// "sent" or an error.
String send(const String& html, const String& person = "");
// "https://t.me/<bot>?start=<code>": opening it in Telegram sends "/start <code>" to the bot,
// which links the account to the person with this invite code (assistant/people). "" without
// Telegram or when the bot's name cannot be fetched. Blocks for a second the first time.
String inviteLink(const String& code);
// Sends a JPEG (in memory) with a caption to a person's chat. Returns "sent" or an error.
String sendPhoto(const uint8_t* jpeg, size_t length, const String& caption, const String& person = "");
// Sends a file (in memory) as a document to a person's chat. Returns "sent" or an error.
String sendDocument(const uint8_t* data, size_t length, const String& filename, const String& mime,
                    const String& caption, const String& person = "");

}  // namespace telegram
