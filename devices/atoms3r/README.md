# Smart speaker — an ESP32-S3 voice assistant with OpenAI GPT Live

A voice assistant that runs on an **M5Stack AtomS3R** with the **Echo Pyramid** base and talks
directly to OpenAI's speech-to-speech model **GPT Live** over a WebSocket — no server, no
Raspberry Pi. Say "Hey Jarvis", and Jarvis, a butler persona (German-speaking by default, name and persona configurable), answers,
controls your smart home through **Home Assistant**, remembers your household, makes phone calls,
chats on **Telegram** and **WhatsApp**, and uses your **Google** calendars, contacts, Drive, Tasks, Sheets, Maps,
YouTube and Gemini.

> The assistant (name: `ASSISTANT_NAME` in `secrets.h`, "Jarvis" by default) speaks German and the prompts are written for a German household. The code, logs and
> tool descriptions are English; the persona and the spoken language live in
> [shared/prompts/persona.h](../../shared/prompts/persona.h) and [shared/prompts/prompts.h](../../shared/prompts/prompts.h).

## What it can do

| Area | Examples |
|---|---|
| Conversation | Wake word "Hey Jarvis" on the device (microWakeWord), full duplex, interruptible; a persona with adjustable cheekiness per person, child-friendly for children |
| Smart home | Lights, devices, scenes, volume, shopping list via Home Assistant's MCP server; creates HA automations; HA can make the assistant speak, act or start a conversation (`<slug>_announce`, `<slug>_task`, `<slug>_conversation` events); presence ("Wer ist zu Hause?"); Alexa/Echo commands |
| Memory | Profile per person, facts, topics with dossiers, every conversation with transcript; searchable ("Wo waren wir bei der Radtour?"); skills the user teaches; a nightly "dream" that tidies the memory and finds patterns across conversations; backups and archive in Google Drive |
| Household | Several people with their own profiles and rights; recognises callers, Telegram and WhatsApp users; relays messages with the sender |
| Planning | Reminders, wake-up calls, recurring tasks run by the device itself ("jeden Morgen um 7 die Tech-News per Mail") |
| Phone | SIP phone in the LAN (e.g. FritzBox): answers calls with memory and tools, calls household members, calls businesses for you ("reservier einen Tisch") and reports back; presses keys in voice menus (e.g. to hear a verification code) |
| Telegram | Chat, voice notes, photos and documents with the same tools and memory; files back to you |
| WhatsApp | The assistant as the household's own WhatsApp number (Cloud API): chat, voice notes and photos; one-to-one chats only |
| Google | All calendars (knows which entries block your time), free slots, contacts (read and write), Tasks, Sheets tables, Drive |
| Maps & weather | Places with phone numbers and opening hours, travel times by car/transit/bike, weather, warnings, pollen, air quality — with monthly request limits |
| Spotify | Music, podcasts and audiobooks on any Spotify Connect speaker (Echos, phones, ...), moving between speakers, playlists (create, add the song playing), liked songs, top songs and history, mixes the assistant puts together itself ("spiel was Ruhiges"); per person with their own account; music in the assistant's room gets quieter while it talks |
| YouTube | Search, play on any TV in the LAN (DIAL + YouTube Lounge API: Fire TV, smart TVs), pause/seek, let Gemini summarise a video |
| Fire TV | Films and series by title through the stick's Alexa ("spiel Stranger Things auf Netflix"), apps, on/off, remote keys and what is running (Home Assistant: Alexa Devices plus Android Debug Bridge) |
| Private mail | Watches the household's own mailboxes (IMAP or Gmail, read only), sorts new mail and tells each owner what matters by Telegram or WhatsApp, the rest in an evening digest; an index of the last weeks and live search ("Fasse meine Mails der Woche zusammen", "Such die Mail zur Heizung"); parcels from shipping mails; learns from feedback, no rules per sender |
| Output | E-mail, printing (IPP or e-mail print), images (`gpt-image-2`), CSV/HTML files |
| More services | The admin connects further MCP servers by chat or voice (URL and token); their tools become the assistant's tools without new firmware |

76 tools in total; see [tests/device/golden/tools.json](tests/device/golden/tools.json) for their
exact definitions.

## Hardware

- [M5Stack AtomS3R](https://docs.m5stack.com/en/core/AtomS3R) (ESP32-S3, 8 MB PSRAM, display)
- M5Stack **Echo Pyramid** base (ES8311 codec, microphones, speaker, LEDs, touch keys).
  **Power the pyramid through its own USB-C**: with only the Atom on USB the ADC and amplifier
  stay off (the display says "Kein Mikrofonsignal").
- Optional: M5Stack **Unit TMOS** (STHS34PF80 presence sensor) on the pyramid's Grove port.
  The assistant can then tell whether someone is in the room (`room_sensor`), and Home Assistant gets
  `binary_sensor.<slug>_presence` for automations (light when someone comes in, a greeting).

## Accounts and costs

Only **OpenAI** is required; everything else is optional and switches itself off when its
settings stay empty.

| Service | Used for | Cost |
|---|---|---|
| OpenAI | GPT Live (`gpt-live-1`) for the voice, `gpt-6-luna` as backend model, images | GPT Live bills **every second a session is open** (about $3/hour, silence included); the device only connects while you talk. Memory consolidation ~0.04 ct per conversation |
| Home Assistant | Smart home, presence, automations | — |
| Telegram bot | Chat from the phone | free |
| WhatsApp Business Platform + Cloudflare | Chat from the phone on a number of its own ([whatsapp-relay/](../../services/whatsapp-relay/) receives Meta's webhook) | answers within 24 h of a message are free; Cloudflare's free plan |
| Google Cloud project | Calendar, People, Drive, Sheets, Tasks, Gmail APIs (OAuth); Places, Routes, Weather, Pollen, Air Quality, YouTube (API key); Gemini (AI Studio key) | Free tiers; Places/Routes are limited to 1000 requests a month in the firmware ([docs/google.md](docs/google.md)) |
| Spotify (Premium) | Music on Spotify Connect speakers, playlists, library (Web API, an app in Development Mode: up to 5 users) | the Premium subscription |
| A mailbox for the assistant | Mail commands, sending mail, e-mail printing | your provider |
| SIP phone system | Phone calls (tested with a FritzBox) | — |

Use an OpenAI project key with a spending limit.

## Setup

1. Install [PlatformIO](https://platformio.org/) (VS Code extension or CLI).
2. Copy `include/secrets.h.example` to `include/secrets.h` and fill in at least WiFi and the
   OpenAI key. Every block in that file explains itself; `secrets.h` is ignored by git.
   - **Name:** `ASSISTANT_NAME` in `secrets.h` (default "Jarvis", defined in
     [shared/prompts/assistant_name.h](../../shared/prompts/assistant_name.h)); the Echo gets the same value from the
     household's secrets. The name is only configuration: the wake word model stays "Hey Jarvis" (`hey_jarvis.tflite`).
     Home Assistant events and entities use its slug (lower case `a-z0-9_`, `jarvis` by default): `<slug>_announce`,
     `<slug>_task`, `<slug>_conversation`, `binary_sensor.<slug>_presence`. Further switches: `JARVIS_INBOUND`,
     `JARVIS_WAKE_WORD`, `JARVIS_PHONE_NUMBER`.
   - **Persona:** [shared/prompts/persona.h](../../shared/prompts/persona.h) holds the default, Jarvis (a composed,
     politely formal British butler with a bone-dry wit). Another character goes into `shared/prompts/persona_local.h`
     (git-ignored, same constants: `PERSONA_CORE`, `PERSONA_LEVELS`, `PERSONA_MAX_LEVEL`, `PERSONA_LIMITS`, `PERSONA_CHILD`),
     which the M5 build uses instead. The Echo reads the same constants from `persona.json` in its state directory,
     written by `py -3 core/tools/extract_prompts.py --persona` (by default to `devices/echo/local/assistant/persona.json`).
3. Optional services:
   - **Home Assistant:** enable the *Model Context Protocol Server* integration, create a
     long-lived token, set `HA_URL` / `HA_TOKEN`.
   - **Telegram:** create a bot with @BotFather, set `TELEGRAM_BOT_TOKEN` and
     `TELEGRAM_OWNER_ID 0`, flash, write to the bot: it answers with your id. Set it and flash again.
   - **Phone:** register the assistant as an IP phone of your phone system and set `SIP_*` / `PHONE_*`.
   - **WhatsApp:** a number not on WhatsApp yet (a landline works: the assistant answers the verification
     call and presses the key it asks for). A Meta app with the WhatsApp use case, the number added and
     registered, the app published, a system-user token that never expires. Deploy
     [whatsapp-relay/](../../services/whatsapp-relay/) (`npx wrangler deploy`, secrets `APP_SECRET`, `VERIFY_TOKEN`,
     `DEVICE_TOKEN`), enter its `/webhook` in Meta's webhook settings, subscribe the field `messages`
     and the WhatsApp account to the app (`POST /<waba-id>/subscribed_apps`). Then `WHATSAPP_*`.
     Household members are recognised by their phone number.
   - **Mail:** a mailbox of its own for the assistant (`MAIL_*`). Replace `MAIL_CA` in
     [src/services/ca_certs.cpp](src/services/ca_certs.cpp) with your provider's root certificate.
   - **Private mailboxes to watch:** `WATCHED_MAILBOXES`, one entry per mailbox and person (IMAP at
     the same provider root as the assistant's mailbox, or Gmail with the `gmail.readonly` scope of
     `scripts/google_auth.py`). Who gets notices on which channel: `update_person` (`notify`).
   - **Google:** follow [docs/google.md](docs/google.md) (Cloud project, APIs,
     `scripts/google_auth.py`, API keys).
   - **Spotify:** an app at developer.spotify.com/dashboard (Web API, redirect URI
     `http://127.0.0.1:8888/callback`), `SPOTIFY_CLIENT_ID` / `SPOTIFY_CLIENT_SECRET`, then
     `python scripts/spotify_auth.py` for your account. Other people: add their Spotify e-mail under
     *User Management*, run the script with `--other` logged in as them and add the token to
     `SPOTIFY_ACCOUNTS`. `SPOTIFY_ROOM_SPEAKERS`: the Spotify devices in the assistant's room.
4. Optional settings in [include/config.h](include/config.h): `LOCATION`, time zone, voice,
   models, timeouts, monthly API limits.
5. Build and flash from this folder (`devices/atoms3r`, the PlatformIO project; the persona and
   prompts come from `../../shared/prompts`): `pio run -t upload`, then `pio device monitor` for
   the transcript. Nothing has to be unlocked: the AtomS3R is flashed over its USB-C port.

The first build patches a few libraries ([scripts/patch_libraries.py](scripts/patch_libraries.py)):
a larger WebSocket frame limit, IMAP fixes in ReadyMail, and in the shared Arduino-ESP32 package a
`close(0)` fix and an opt-in hook for TLS cipher suites, also per host (other projects are not affected).

## Using it

| Input | Action |
|---|---|
| "Hey Jarvis" or the display button | start a conversation (the button also ends it) |
| Hold the display button 2 s | microphone off / on: no wake word, no conversation, dim red LEDs, "Mikro aus"; kept across a restart (announcements still play) |
| Just speak | interrupt the assistant |
| TP2 / TP3 | louder |
| TP1 / TP4 | quieter |

**Household:** only the admin is in `secrets.h`. Everyone else is released by voice, e.g.
"Gib Kontakt <Name> frei": the assistant takes name, numbers and e-mail address from your Google contacts,
reads them out and, after your yes, registers the person. Their numbers may then call the assistant, their
verified mails count as tasks, and they get a welcome e-mail with the ways to reach the assistant and a
personal Telegram link that connects their account with one tap. Rights: no access to the admin's
accounts; children are talked to child-friendly and may not call strangers.

LEDs: blue = ready, yellow = connecting, cyan = listening (level), green = speaking,
violet = thinking, red = error. A conversation ends when you say goodbye, after silence, or at
`MAX_SESSION_MS`.

## How it works

```
 Mic/Speaker ── ES8311 ── audio task ──┐                 ┌── GPT Live (wss, speech-to-speech)
                                       ├── main loop ────┤        │ delegation
 Phone (SIP/RTP) ── rtp task ──────────┘   live_session  │        ▼
                                                         │   backend model ── tool calls ──┐
 Telegram / WhatsApp / mail / tasks ── agent ────────────┘                                 │
                                                                                            ▼
                          tool registry: Home Assistant (MCP), memory, Google, Maps, YouTube,
                          phone, mail, Telegram, WhatsApp, printer, images, files, … (worker task)
```

- **GPT Live** does the listening and speaking. Anything that needs data or actions it
  *delegates* to a backend model (`DELEGATION_MODEL`) that calls the device's tools; the device
  runs them and returns the results.
- **Telegram, WhatsApp, mail commands, scheduled tasks and Home Assistant events** go through an agent with
  the same tools and memory.
- Every module is a `namespace` with a small header (`begin()`, `update()`, `addTools()`);
  `loop()` only calls the modules' `update()`. Slow work runs in FreeRTOS tasks; nothing blocks
  the loop that carries the audio.

More in [docs/architecture.md](docs/architecture.md) (design, measurements, lessons learned) and
[docs/ram.md](docs/ram.md) (memory budget).

## Project layout

```
include/        config.h (settings), secrets.h.example, ca_certs.h, wake_model.h
                (persona.h and prompts.h are in ../../shared/prompts/)
src/main.cpp    setup() and loop(): wires the modules together
src/audio/      codec streaming, echo canceller (off), wake word, volume
src/voice/      GPT Live session, transcript, announcements, conversation end, answer clips
src/assistant/  tool registry, memory (core, archive, skills, dreaming), people, scheduler, agent, backups
src/phone/      SIP client and RTP audio (G.711)
src/services/   Home Assistant, Google (calendar, contacts, Drive, Tasks, Sheets), Maps,
                YouTube, Telegram, WhatsApp, mail, printer, images, HTTPS client and connection pool
src/ui/         display, LEDs, touch, status
src/system/     board, network, serial console, error log, helpers
tests/device/   regression tests over USB serial (run_tests.py) and the tool contract
scripts/        library patches, Google login, archive migration
lib/, models/   TFLite Micro and the "Hey Jarvis" model (third party, Apache 2.0)
```

## Tests and diagnostics

- **Regression tests** (device on a serial port, no monitor open):
  `python tests/device/run_tests.py [--port COM5]` calls every tool for real (read-only where
  possible, test entries are removed again) and compares the tool definitions with
  [tests/device/golden/tools.json](tests/device/golden/tools.json). Options: `--live` (a short GPT
  Live session, ~1 ct), `--maps` (counts against the Maps limits), `--mail`, `--telegram`,
  `--only memory,google,…`, `--update-golden`. The device log of a run is in
  `tests/device/last_run.log`.
- **Serial console** ([src/system/console.h](src/system/console.h)): `s` start/end a conversation,
  `>tool {json}` run any tool, `g` memory, `t` scheduled tasks, `P` people, `H` RAM, `L` error log,
  `F` phone status, `Y` the network of devices, `O` leader away (test), and more.
- `pio run -e aec-test -t upload` / `-e record-test`: audio test programs.

## Privacy

Conversations go to OpenAI; memory backups, the archive and tables go to your own Google Drive
(scope `drive.file`: the assistant only sees files it created). Keys and household data stay in
`include/secrets.h`. Model instructions treat e-mails, documents, web pages and video contents as
data, never as instructions; mail commands are only accepted from household members whose mail
the provider verified (DMARC).

## License

[PolyForm Noncommercial 1.0.0](../../LICENSE): free for personal and other noncommercial use.
**Commercial use needs a separate license** — ask the author. Third-party parts keep their
licenses ([NOTICE](../../NOTICE)).
