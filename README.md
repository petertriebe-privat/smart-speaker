# smart-speaker

A voice assistant for the home that talks directly to OpenAI GPT Live: smart home through Home
Assistant, memory, phone calls, messages, Google services, music and the TV. It runs on one
device or on several that work together as one assistant.

| Device | Folder | Setup guide |
|---|---|---|
| M5Stack AtomS3R + Echo Pyramid (ESP32-S3, C++) | [devices/atoms3r](devices/atoms3r/) | [Setup](devices/atoms3r/README.md#setup) |
| Amazon Echo Gen 1 (rooted, own Linux, Go) | [devices/echo](devices/echo/) | [Unlock and setup](devices/echo/hardware/gen1/README.md) |
| Amazon Echo Dot 2nd gen (rooted, own Linux, Go) | [devices/echo](devices/echo/) | [Unlock and setup](devices/echo/hardware/dot2/README.md) |
| Fire TV stick: the assistant's screen and an assistant of its own (Android app, Java + core) | [devices/android](devices/android/) | [Install](devices/android/README.md#install) |
| Android phone: an assistant of its own with the TV's HUD (Android app, Java + core; first version) | [devices/android](devices/android/) | [Install](devices/android/README.md#the-phone-app) |

| Shared part | Folder |
|---|---|
| The assistant's Go program, running on the Echos, the Fire TV and the phone | [core](core/) |
| Persona and prompts, one source for every device | [shared/prompts](shared/prompts/) |
| Servers next to the devices (MCP servers, agent, WhatsApp relay, deployer, home gateway) | [services](services/) |
| Wake word training | [tools/wakeword](tools/wakeword/) |

## Your own assistant

The assistant is what the household makes of it:

- **Name:** `ASSISTANT_NAME` in the household's secrets (`devices/atoms3r/include/secrets.h`, which
  `tools/make_secrets.py` also turns into the Echo's `jarvis-secrets.json`); default "Jarvis".
  The Home Assistant events and entities carry the name's lower-case slug (`<slug>_announce`,
  `<slug>_task`, `<slug>_conversation`, `binary_sensor.<slug>_presence`); the MQTT topics are
  `jarvis/...`. Paths, the binary (`jarvis ctl`), the apps (`dev.jarvis.*`) and the secrets
  switches (`JARVIS_*`) keep the name "jarvis" whatever the assistant is called.
- **Persona:** `shared/prompts/persona.h` is Jarvis, the default: a composed, politely formal
  British butler with a bone-dry wit, after the films. Another character goes into
  `shared/prompts/persona_local.h` (git-ignored, the same constants), used by the M5 build instead. For the Echo,
  `py -3 core/tools/extract_prompts.py --persona` writes `devices/echo/local/assistant/persona.json`,
  which goes to the Echo's state directory as `persona.json`.
- **Wake word:** the built-in model is "Hey Jarvis" (microWakeWord by Kevin Ahrendt / esphome). An
  own model is set with `wake_model` in the device settings and trained with
  [tools/wakeword](tools/wakeword/).
- **Language:** German today; the prompts are in [shared/prompts](shared/prompts/).

## Features

Everything below works on the Echos and the M5 unless it is marked *(Echo)* or *(M5)*. The Echo is
the household's main device; the M5 is a second speaker. Details: [devices/echo/docs/16-assistant.md](devices/echo/docs/16-assistant.md)
and [devices/atoms3r/docs/architecture.md](devices/atoms3r/docs/architecture.md).

### Conversation
- Wake word on the device ("Hey Jarvis" built in, microWakeWord; or an own model trained on the household's voices,
  [tools/wakeword](tools/wakeword/)) or the action button. After the wake word the light shows at
  once, no cue plays; the user goes on talking right after the wake word. The device takes the first
  words itself and decides before GPT Live gets any audio: no words (an empty transcription
  too), or words not meant for
  the assistant (the TV, people in the room; asked of the Decisions API), end silently; a quick command
  runs without GPT Live; only the rest goes to GPT Live, with the recording. GPT Live connects
  ahead meanwhile, without audio.
- Full-duplex speech with GPT Live, interruptible *(Echo: the DSP's echo cancellation; the M5 mutes
  its microphone while it speaks)*; a backend model with the tools behind it.
- Persona with adjustable cheekiness per person, child-friendly for children; name (`ASSISTANT_NAME`), voice and
  language from [shared/prompts](shared/prompts/).
- Quick commands without GPT Live: the house (every light, plug, blind, fan, heating or scene in
  Home Assistant: "Licht in der Küche aus", through HA's own sentences), the time and the date
  ("Wie spät ist es?"), music ("Pause", "lauter", "nächstes Lied", an artist, the favourites, move
  it to another speaker) *(Echo)*, video pause/play *(Echo)*, the mobile phone's call ("geh ran")
  *(Echo)*, timers and alarms *(Echo)*. Rules first, every other first utterance goes to the
  Decisions API (gpt-6-luna, 0.3-0.6 s).
- Thinking harder (`think_harder`), background tasks that report back later, research split into
  parallel runs *(Echo)*; reading web pages and whole websites (Tavily).
- Notices into a running conversation: reminders, results, an urgent mail, an appointment coming
  close.

### Memory and learning
- A profile per person, facts, topics with dossiers, every conversation with its transcript
  (Google Drive archive); `recall` searches it ("Wo waren wir bei der Radtour?").
- Consolidation after every conversation; a nightly dream that tidies the memory, merges doubles,
  closes stale topics and finds patterns across days (only with evidence from several days).
- Behaviour rules from corrections ("Nenn mich nicht Sir"), sorted into a rulebook; rules the assistant
  learns itself from how people react stay tentative until confirmed.
- Learning from behaviour *(Echo)*: quick commands, music, lights, timers, arrivals and departures
  go into a behaviour log; the dream turns routines into habits ("Gewohnheit: …").
- Weekly learning report *(Echo)*: what the assistant learned on its own, numbered, by Telegram or
  WhatsApp; answering "2 stimmt nicht" corrects it.
- Skills the user teaches ("Wenn ich Guten Morgen sage, dann …").
- Preferences read from the rules (briefing hour, mail digest, urgency) instead of fixed settings.

### Initiative and the day *(Echo)*
- Agenda of calendar, to-dos, deadlines from mails and open matters (affairs) kept consistent: a
  cancelled trip takes its bookings with it; done or obsolete matters never come up again.
- Morning briefing in the first conversation of the day; tomorrow's appointments in the evening.
- Initiative: the assistant looks for ways to help on its own (leaving times with traffic, overlapping
  appointments, follow-ups on matters waiting for an answer, parcels, open windows), weighs each
  case and tells it at the right moment, or plans it for later; it learns from accepted and
  declined suggestions. "Heute nicht" pauses the hints.
- The assistant's own to-do list in Google Tasks for matters from mails and conversations.
- Mails settle matters: a receipt or confirmation ("the return is on its way back", "paid") closes
  the matching affair, also one from the user's own to-dos or the calendar of the next two weeks;
  the message about the mail says so and ticks off the to-do (or offers to, by the user's
  `affair_trust`) and offers to delete the appointment.

### Smart home
- Home Assistant over MCP: lights, devices, scenes, climate, covers, shopping list; creates and
  deletes HA automations ("jeden Abend um zehn …").
- HA can make the assistant speak, act or start a conversation (`<slug>_announce`, `<slug>_task`,
  `<slug>_conversation`, the slug being the lower-case name, "jarvis" by default); presence ("Wer ist zu Hause?", "Wo ist Alex?").
- Each speaker is a Home Assistant device over MQTT discovery: state, volume, a stop and a talk
  button, announcements in its room (`notify.send_message`), events for automations (wake word
  with its score, a silent false start `wake_word_silent`, button, conversation; timer *(Echo)*) and diagnostics (memory, WiFi, version, last error); the
  M5 also its microphone switch and presence. The rest of this item is *(Echo)*:
  The speaker's name and room are set in HA's UI; it takes them over everywhere (peer link,
  Spotify Connect, Bluetooth, the SIP caller name). From HA also: timers and alarms, the wake word's
  sensitivity, whether it may become primary, a night mode with its own volume and
  a dimmed ring (when is night: an HA automation the assistant reschedules on request), the ring's
  brightness as a setting, also by voice when the ring is named (never off and no lamp of the
  room: it is the wake word's feedback, so "Licht aus" or "Licht dimmen" leave it alone). An assistant device per household: hints paused per person, each person's last room.
- Reminders ordered aloud at a speaker are said at that speaker; HA's `<slug>_announce` and
  `<slug>_conversation` events take a room *(Echo)*.
- Room presence sensor on the M5 (`room_sensor`, `binary_sensor.<slug>_presence`) *(M5)*.
- Alexa Echos at home through HA: radio, Spotify's own mixes, speaking and announcing.

### Timers *(Echo)*
- "Timer zehn Minuten für die Nudeln", "Weck mich um halb sieben", "Timer aus": set on the device,
  confirmed by a short sentence; ringing in its room until the action button or the wake word.

### Phone
- SIP phone in the LAN (e.g. FritzBox): answers calls with memory and tools, recognises callers,
  calls household members, calls businesses for you and reports back; presses keys in voice menus.
- Follow-ups for calls made on your behalf: retries, call backs that continue the task, a history
  per number *(Echo)*.
- The speaker in calls: listening in to the room from outside, intercom both ways, a running call
  on the speaker, the assistant joining a call as a third party, dialling someone for you *(Echo; the M5
  can listen in)*.
- The mobile phone over Bluetooth: hands-free unit, calls ringing at the Echo with the caller's
  name, pairing by voice *(Echo Gen 1)*.
- Guests: numbers not released get a polite guest mode; the message reaches the admin.

### Messages and mail
- Telegram: chat, voice notes, photos and documents with the same tools and memory; commands
  `/neu`, `/aufgaben`, `/skills`, `/freigabe`.
- WhatsApp on the household's own number: chat, voice notes, photos, documents; images, files and
  reports for people without Telegram *(Echo)*; outside WhatsApp's 24-hour window a template, the
  rest when the person answers *(Echo)*.
- Relaying messages between household members with the sender.
- The assistant's own mailbox: mail commands, sending mail, attachments, e-mail printing.
- Watching the household's private mailboxes (IMAP or Gmail): sorting, notices for what matters,
  an evening digest, search, parcels from shipping mails, tidying released categories into the
  trash or archive.

### Google and the world
- All calendars (create, change, delete, free slots), contacts (read and write), birthdays, Tasks,
  Sheets tables, Drive files.
- Maps: places with opening hours and phone numbers, travel times by car, transit and bike;
  weather, warnings, pollen, air quality; monthly request limits.
- Web search; Gemini for video summaries.

### Music and TV
- Spotify on any Spotify Connect speaker, each Echo one itself *(Echo)*: music, podcasts,
  audiobooks, moving between speakers, playlists, liked songs, history, mixes the assistant puts together;
  per person with their own account; music gets quiet while the assistant listens or talks.
- YouTube on any TV in the LAN (DIAL and the Lounge API), pause and seek, summaries.
- Fire TV: films and series by title, apps, on/off, remote keys.
- The TV as the assistant's screen *(Echo, Fire TV)*: "zeig mir …" shows it on the TV in the room (or a
  named room, or every TV that is on): cards with facts, lists, recipe steps, timers, weather, the
  day, charts, tables, pictures and picture galleries (from the web search), choices to answer by
  number; web pages in a frame; videos with sound. Whatever goes to a screen (a TV or the phone)
  is shown graphically and in detail. A library of templates tells the assistant how each
  kind of answer looks best (the weather of
  the next days as a forecast with a chart, appointments as a timeline, recipes, comparisons,
  values, routes, parcels). The TV chooses between the corner above a running film and the whole
  screen and pauses the film for a long card; a full-screen card stays until closed or the next
  one; a sleeping TV is woken and the assistant waits until it is ready (`show_on_screen`, the app in
  [devices/android](devices/android/), [design and use cases](devices/android/docs/design.md)).
- The assistant visible on the TV *(Echo, Fire TV)*: a 3D hologram in the Echo ring's colours (amber at
  rest, turquoise listening, purple thinking, turquoise pulsing with its voice when speaking), with
  live captions of the conversation in the room (the assistant's newest words, about three lines, so a long
  answer does not cover the card); the console on the Fire TV's home screen; messages the assistant sends
  a person also as a hint on every TV that is on.
- What the TV shows is context *(Echo, Fire TV)*: the card on the TV in a room and the end of the
  room's last conversation go into the next conversation there, on any of the assistant's devices (begun at
  the speaker, continued with the remote): "was steht da beim Donnerstag?" needs no new search.
- An assistant of its own on a Fire TV stick *(Fire TV)*: the same program as on the Echos runs in the
  app; hold the remote's microphone key and talk, it answers from the TV (Realtime API,
  `gpt-realtime-mini`, other voice models selectable in Home Assistant) and shows every answer
  with content on the screen; joins the Echos (peer link, Home Assistant device) or works without
  them; Fire TV's Alexa can be switched off. The stick keeps itself working after a restart
  (Alexa's voice app off again, the microphone granted, over Home Assistant's ADB), and the
  primary device starts the app on a stick where it does not run.
- An assistant of its own on an Android phone *(Phone, first version, #75)*: the same program in the
  app, never the household's primary; the Fire TV's HUD and 3D hologram upright, the hologram on
  top, the conversation and the assistant's cards below it; its button at the bottom: tap it to talk
  (it hears when you are done, tap again to end), or hold it while you speak (push to talk as on
  the Fire TV); while the app is open in front it also hears the wake word. The microphone is open only
  then. On the phone's loudspeaker it sounds as media and an echo canceller of our own (SpeexDSP)
  takes its voice off the microphone, so you can interrupt it by voice; on earbuds or in the car
  the call path. A Home Assistant device like the speakers; the landline stays with the speakers.
  The assistant operates the phone: navigation (on the car's screen in the car), calls and SMS by
  contact name, the messengers' waiting messages read out and answered (WhatsApp, Signal …), its
  music, apps, alarms and timers; calls and messages after a confirmation. Music on the phone
  pauses while it talks and plays on after. Notifications from the assistant on the phone and the car's
  screen ("schick mir das aufs Handy", a tap opens the card); a long research runs in the
  background and it comes back by itself with the result. The hologram plays along under the
  fingers: a violet plasma filament to each finger as in a plasma ball, it charges until it explodes, it turns with a swipe, a double tap sends
  a shockwave.
  In the car: the HUD live on the Android Auto screen, the hologram moving in its state's colour
  with the turn's words or the card it showed in full size, beside a small panel with Sprechen /
  Unterbrechen / Beenden (older cars: rows with a picture of the card); and the wake word heard
  while Android Auto is connected: the music pauses, a short tone says the car's microphone
  listens, and a start nobody speaks to ends unnoticed.
  Away from home on mobile data too, without a VPN: the house's services over the home gateway
  (the Pi) and the VPS, with a client certificate per phone ([services/home-gateway](services/home-gateway/README.md)).
- The Echo's ring as a music display *(Echo)*.

### Household and rights
- Several people with their own profiles, rules, accounts and rights; released by voice from the
  Google contacts, with a welcome mail (Telegram link, WhatsApp link, phone).
- Children are talked to child-friendly and may not call strangers; other people never reach the
  admin's private accounts.

### Several devices *(Echo; the M5 as a member in part)*
- Several Echos as one assistant: one answers the wake word (the louder one), shared memory, tasks
  and people on the primary; every Echo rings for the landline and can take the call.
- The primary role moves to another Echo when one fails.
- Intercom between Echos; a room per Echo for announcements and conversations.

### More
- Images (`gpt-image-2`) printed, mailed or sent to the chat, right away also during a conversation
  *(Echo, Fire TV; the M5 after it)*, in portrait or landscape (printed sideways on A4); documents
  and CSV/HTML files.
- Further MCP servers connected by voice or chat: their tools become the assistant's tools.
- The server agent (services/agent-mcp; OpenHands with Claude Code or Codex): long tasks on a Linux server,
  reports by Telegram, announced only on a speaker the owner names, a call back on request; with
  `/freigabe an` changes wait for the owner's tap in Telegram.
- The assistant improves itself with a safety net *(Echo)*: the server agent builds a fix or feature on a branch
  and opens a pull request, the CI tests and builds it, and the link comes by Telegram; the
  owner's merge deploys it on every Echo through the CI on the Raspberry Pi, which checks that the
  new version runs, rolls back when not, and reports it in Telegram (`services/deployer`; the CI pipeline itself is set up per repository and is not part of this repository yet).
  The server agent reads every Echo's status, logs and errors for the analysis, only reading and through the
  Pi (`ssh jarvis-diag`, skill `jarvis`) *(Echo)*. The Fire TV app is built and installed on every
  stick the same way, as an update that keeps its setup.
- Delivery: results, reminders and hints the assistant brings unasked go to the phone (Telegram or
  WhatsApp); on a speaker only when the user names it (or, for hints, where the presence sensor
  sees someone), a phone call only when asked or very important.
- Backups of the memory in Google Drive, an error log, a self-test; tests on the devices (`jarvis
  ctl`, the M5's serial suite) and an evaluation bench for prompts and tools *(Echo)*.

## Contributing

Ideas and bug reports are welcome as issues; security problems please privately (see
[SECURITY.md](SECURITY.md)). To change something, fork the repository and open a pull request to
`main`. It is merged when the checks pass and a code owner ([CODEOWNERS](.github/CODEOWNERS))
approved it. The checks ([ci.yml](.github/workflows/ci.yml)) run only for the parts a pull request
changes: Go (gofmt, vet, staticcheck, tests with the race detector, govulncheck, the ARM builds),
the M5 firmware (build, cppcheck), the Android apps (lint, builds), Python (ruff, the services'
tests), shell scripts (shellcheck), the workflows (actionlint) and a scan for secrets. Workflows
from a fork's pull request run once the owner approved them. Please keep household data (names,
addresses, devices) out of everything you send: your own values belong in the git-ignored files
described above.

## Thank you

The Echos in this project only run our own software because other people opened them first and
shared what they found. Without their work we would not have got anywhere. Our heartfelt thanks:

- **André Zwing** for [EchoRoot](https://gitlab.com/AndreRH/echoroot) and
  [u-boot-echo](https://gitlab.com/AndreRH/u-boot-echo): the
  [wiring](https://andrerh.gitlab.io/echoroot/CIRCUIT.html) that tells you what to solder to the
  Echo Gen 1, the [setup](https://andrerh.gitlab.io/echoroot/SETUP.html) and the base image. Its
  SD card gave us the open bootloader for our first root shell; its image gave us the SSH server
  and the start scripts that taught us how the Echo's audio, WiFi and LEDs come up. Everything we
  built on the Echo Gen 1 stands on this foundation.
- **Kent Sørensen** for finding the 3.3 V output on TP19 (credited on EchoRoot's wiring page).
- **MWR Labs** (now [Reversec](https://labs.reversec.com/advisories/2017/08/amazon-echo-rooting))
  for the 2017 research that showed the Echo's debug pads and its boot from an external SD card.
- **xyz** for [amonet](https://github.com/xyzz/amonet), the MediaTek bootrom exploit, **k4y0z**
  for its first port to the Echo Dot 2nd gen and **R0rt1z2** for
  [continuing and releasing it with TWRP](https://github.com/R0rt1z2/amonet/tree/mt8163-biscuit):
  the unlock of our Echo Dot is entirely their work.
- **bboe** for [firebreak](https://github.com/bboe/firebreak), **wilbowes** for the
  [EchoMuse rooting notes](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md) and
  **HuskerMinion** for [TECHO5](https://github.com/HuskerMinion/techo5), which guided us
  through the Dot's unlock and its own Linux.

Other third-party parts (wake word model, TensorFlow Lite Micro, certificates): see
[NOTICE](NOTICE).

## License

[PolyForm Noncommercial License 1.0.0](LICENSE): free for noncommercial use; commercial use needs a
separate license from the author. Third-party parts are under their own licenses (see [NOTICE](NOTICE)).
