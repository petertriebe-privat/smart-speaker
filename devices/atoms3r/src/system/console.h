#pragma once

// Test commands over USB serial (pio device monitor):
//   s  start / end a conversation         c  the assistant tells a story (during a conversation)
//   h  reload the Home Assistant tools    p  print a test page by mail
//   m  list the mails                     w  log the wake word probability on / off
//   i  generate a test image and mail it to the owner (costs one image)
//   g  show the long-term memory          X  delete the whole memory (capital X)
//   t  list the scheduled tasks           F  phone: FritzBox registration and call
//   H  heap: internal RAM free / largest block (TLS needs ~40 KB in one piece)
//   >name {json}  run a tool of the backend model, e.g. >list_home_automations {}; the output
//                 ends with "[Tool-Ende]" (mail tools: "[Mail] name -> ..." from the mail task)
//   D  dump the tool definitions as JSON between [TOOLS-BEGIN] and [TOOLS-END]; A: the agent's
// tests/device/run_tests.py drives these commands for the regression tests.
namespace console {

void update();

}  // namespace console
