#pragma once

// Events from Home Assistant to the assistant: a permanent WebSocket to HA's API in the LAN
// (/api/websocket, admin token) subscribed to two event types that any HA automation can fire
// with the action `event:` (no YAML changes in HA needed):
//   jarvis_announce  {text}                          the assistant says the text
//   jarvis_task      {title, instruction, deliver}   the assistant carries out the instruction with
//                                                    his tools (like a scheduled prompt task)
//   jarvis_conversation {text, person?, insist?}     the assistant starts a conversation at the speaker
//                                                    (text: the occasion; a greeting by default)
// Push, not polling: the washing machine finishing, the door opening, a sensor alarm.
namespace ha_events {

void begin();
void update();  // main loop; reconnects on its own

}  // namespace ha_events
