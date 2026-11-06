# The assistant on an Android phone: design

The phone app (issue #75): the design, the decisions and what was measured. Built since
01.11.2026 and in daily use on the owner's phone, at home and in the car: the app in
`devices/android/phone` ([its README](../../android/README.md#the-phone-app)); the state of each
step is under [Steps](#steps).

## UI (decided 01.11.2026)

- The Fire TV app's console and HUD with the 3D hologram, upright: the hologram on top, the
  conversation or the assistant's card below it, the assistant's button (the orb) at the bottom. Sideways it is the
  TV's HUD.
- The button: a tap starts a conversation that goes on hands free (GPT Live hears when the user is
  done) and a tap ends it; held, it is push to talk. The microphone is open only then.

History: the first plan (21.10.2026) was a Kotlin app without the Go core, with the home assistant
attached to the phone's GPT Live session as a sideband. Replaced on 01.11.2026 by the plan below,
after the Fire TV app (#108) showed that an Android app can carry `core/` as an assistant of its own.

## Decisions (01.11.2026)

- **The app is an assistant of its own**, like the Fire TV app: `core/` built for Android inside the
  APK (`devices/android/tools/install.sh … --core`), with Home Assistant, Spotify, Google and the
  shared memory. The app is a new module `devices/android/phone/` on the `shared/` library (HUD,
  cards, overlay).
- **It never becomes the leader** (`primary: false`, as the M5): the Telegram, WhatsApp and mail
  pollers, the task scheduler, backups and the memory consolidation run only on the leader
  (`devices/echo/docs/18-several-echos.md`), so they cost the phone nothing.
- **"Hey Google" stays the default assistant.** the assistant starts from its app icon, a Quick Settings
  tile, a widget, the Android Auto app and, in the car, its own wake word.
- **No VPN on the phone.** The way home is an HTTPS endpoint with client certificates (mTLS) on
  the existing VPS and Pi path (below).
- **Android Auto app** with its own UI and voice.

## Mobile peer mode

The peer link today is made for speakers in the LAN: a UDP heartbeat every 2 s, wake word
arbitration, the leader election, and a state copy mirrored from the leader. On a phone the
heartbeat alone keeps the radio awake. A peer mode `mobile`:

- no heartbeat, no wake word arbitration, never a candidate in the election;
- the state copy (memory, people, rules, skills, agenda, HA's entity list) is pulled at session
  start when older than a few minutes, and at night while charging on Wi-Fi;
- writes (`remember`, the transcript, shared tools) go to the leader; when it cannot be reached
  they wait in an outbox and go when it can. The leader stays the only writer;
- the leader sees the phone only while a request comes in, and does not report it as missing.

Its own credentials: a `PEER_KEY` (or client certificate) per mobile device that the leader can
revoke, never the household's shared key. Identity and permissions come from that credential on
the leader, never from a person field the client sends.

The secrets the core needs (OpenAI key, Google, Spotify) live on the phone as on the stick, in the
app's private storage, encrypted with the Android Keystore. A lost phone: revoke its certificate,
rotate the keys.

## The way home: mTLS over the VPS and the Pi

Most of the traffic needs no way home at all:

| Target | Way |
| --- | --- |
| GPT Live, Spotify, Google | straight to the internet |
| Home Assistant | its existing public address, HA's own login (a long-lived token) |
| the leader (state copy, writes, shared tools) | the mobile endpoint below |

```
phone ──HTTPS + client cert──▶ VPS (public, port 443) ──WireGuard (existing)──▶ Pi (LAN) ──▶ leader
```

- The VPS forwards by SNI without decrypting (nginx `stream` with `ssl_preread`, TLS passthrough),
  so the assistant traffic is decrypted only at home. If port 443 on the VPS already terminates TLS
  for HA, it needs the SNI split in front of it. To check on the VPS.
- The Pi terminates TLS, checks the client certificate against our own small CA (one certificate
  per device, a revocation list) and passes the request to the leader. The Pi's nginx already
  proxies HA over the same tunnel; the new piece is a server block, the CA and a stream entry on the
  VPS.
- **Finding the leader:** the leader can change (failover). Either the Pi tries the Echos in order
  and a non-leader answers with a redirect to the leader, or every assistant accepts mobile requests
  and forwards them to the leader over the peer link. To decide when building it; the second
  keeps the Pi's configuration static.
- The endpoint on the assistant side is small and accepts only the mobile requests: pull the state
  copy, push writes, run a shared tool, hand over a transcript. It never opens a microphone and
  never streams audio.
- The household's host names stay out of the repository: the app gets the endpoint, the CA and its
  certificate from the install script, like the other secrets (`local/`,
  git-ignored).
- Advantages over Tailscale on the phone: no VPN slot taken (Android allows only one), no VPN
  symbol, nothing running between requests. Cost: a public port on the VPS, protected by the
  client certificate.

## Activation

- **"Hey Google" stays.** A third-party app cannot use the always-on DSP hotword, and only the
  default assistant gets the side key and the screen context. The assistant starts from its icon, a Quick
  Settings tile, a widget, the Android Auto app, or its wake word.
- **Wake word "Jarvis" (the default name) while the car is connected:** the app sees Android Auto through
  `CarConnection` (and a registered car over Bluetooth through `CompanionDeviceManager`) and runs a
  microphone foreground service with the wake word only then. The model is the Echos'
  microWakeWord model through LiteRT (`core/internal/wakeword` is the reference; "Jarvis" alone
  needs the household's own model from #61).
- **Which microphone listens in the car:** listening permanently through the car's microphone
  would keep Bluetooth HFP open (the car shows a call, music in call quality). So the phone's
  own microphone listens for the wake word, and the conversation then uses the car's microphone
  (built so, 01.11.2026).
- **Measured in the car (02.–03.11.2026):** with music the phone's microphone clips (peak 32768);
  the wake word fired on music and real calls alike at 0.86–0.99. Transcribing the 3 s before a
  detection to confirm it was tried offline on all 41 saved hits: at home real ones read the
  wake word, but every clipped car clip read empty, real or false, so it would reject real calls
  and was not built. The false starts all went to GPT Live because the first words' empty
  transcription counted as a failure; since 03.11.2026 it counts as no words (capture.go): a
  false start is a pause of the music for ~4 s, ended unnoticed. So that a real call does not
  run into that silence, the app plays a short tone in the car once its microphone listens
  (AudioBridge.cue). Noise the transcription turns into a made-up sentence (an Italian one,
  rated 0.70 "meant for the assistant") opened GPT Live too: QUICK_ADDRESSED_QUESTION now names
  song lyrics and text that is not plain German or makes no sense (that sentence: 0.00; "Was
  ist mein nächster Termin?" 0.96).
- **Next to "Hey Google":** both listen at once (Google through its DSP, the assistant through the CPU).
  To measure that neither blocks the other's microphone.
- At home the phone has no wake word (the Echos listen there).

## Android Auto

- An app on the Car App Library 1.4. There is no assistant category for third parties; the app
  declares IoT and weather. Weather apps may draw on the car screen's surface
  (`MapWithContentTemplate`, car API 7+, permissions `ACCESS_SURFACE` and `MAP_TEMPLATES`): the
  HUD runs live there (`car/CarSurface.java`), the hologram moving in its state's colour with the
  turn's words, or the last card in full size until the next question. Beside it Android Auto
  keeps a panel it sizes and places itself (the state and Sprechen / Unterbrechen / Beenden);
  a full screen without it would need the navigation category (`NavigationTemplate`), which was
  not taken: the car could then offer the assistant for navigation and clash with Google Maps
  (owner, 03.11.2026). Cars below API 7 get a `PaneTemplate` with rows and a picture of the card
  rendered off screen (`CardRenderer.java`).
- The car's microphone: `CarAudioRecord` gives it only while the app is in front on the car
  screen, so it cannot listen for the wake word while Maps or Spotify is shown. The phone's own
  microphone listens for the wake word; the conversation then runs over the car's hands-free
  (the call path), the music paused.
- Sideloaded, the car app runs only in the Desktop Head Unit: with Android Auto's developer
  setting "unknown sources" on and the Play Store as installer (`install_phone.sh`,
  `-i com.android.vending`) Android Auto listed it there and showed its notifications (02.11.2026).
  A real car refuses it (`CarProjectionValidator`: `PACKAGE_FAILED_ALL_CHECKS`, 06.11.2026) with
  any installer: there Android Auto takes apps from a trusted source only, Google Play. Parked;
  the idea of a small car app from Play's internal test track: #5.
- Tested on the PC with the Desktop Head Unit (`<Android SDK>/extras/google/auto`, Android
  Auto's developer setting "start head unit server", `adb forward tcp:5277 tcp:5277`,
  `desktop-head-unit.exe -a -i hybrid`; its console takes `tap x y` and `screenshot <file>`).
- Car features:
  - "Navigiere nach …": `CarContext.startCarApp(ACTION_NAVIGATE, geo:…)` starts Google Maps'
    navigation on the car screen;
  - "Spiel …": the Spotify tools with the phone as Spotify Connect device, other apps through
    their media session;
  - "Ruf … an": the contact through the existing Google contact search or the phone's contacts,
    then `ACTION_CALL`;
  - messages: read and answer through the notification listener (below), answers spoken, sending
    only after a confirmation.

## Phone tools

| Level | How | Examples |
| --- | --- | --- |
| 1 | intents and system APIs | alarm, timer, call, SMS, navigation, open an app, torch, volume, do not disturb, media control |
| 2 | `NotificationListenerService` with RemoteInput | read, summarize and answer messenger notifications (WhatsApp) |
| 3 | `AccessibilityService`: the screen as a compact text tree; `phone_screen`, `phone_tap`, `phone_type`, `phone_scroll`, `phone_back` | operate any app, read older chats, the screen ("what is on my screen?") |
| camera | `CameraX`, a photo per question to GPT Live | see below |

- Calendar, mail, contacts and the like the core already has through its Google tools.
- The notification listener sees only what arrived as notification (unread messages); older chat
  history needs level 3 (open the chat, read the screen). Messages from third parties go to OpenAI
  for a summary.
- The app advertises only tools it can provide with its current permissions; they exist only for
  the phone's own sessions (a device scope in `tools.Context`), never on the Echos.
- Sending messages and similar needs a confirmation. Steps that need an unlocked phone ask for
  `requestDismissKeyguard`. The accessibility service listens to no events while idle.
- These abilities go into a phone-only prompt (`LIVE_PHONE_ABILITIES`), not into the shared prompts.

## Camera

Only while the app is visible (Android allows no camera in the background), never while driving.
Use cases: "what is this?" (plants, devices, a warning light in the car), a letter or receipt
summarized, filed in Drive and its dates put into the calendar, a sign or menu translated, the
fridge looked at for the shopping list, a type plate read and its manual found. A still photo per
question to GPT Live first; a live camera picture later if it proves useful.

## What the Go side needs

- The peer mode `mobile` (above) and the mobile endpoint in `internal/peer`.
- A `phone` board in `internal/board`, as `firetv`: microphone and speaker through a local socket to
  the app, which bridges them to `AudioRecord` (`VOICE_COMMUNICATION` for the phone's AEC) and
  `AudioTrack`; the phone tools as calls from the core to the app over the same socket.
- A device scope for the phone tools in `tools.Context` and the tool selection.

## Steps

1. Phone module with `core/`, the board, the assistant's button in the app (HA, Spotify, Google work as on
   the stick). Built and in use since 01.11.2026: the core on arm64, the button, our own echo
   canceller on the loudspeaker (SpeexDSP), battery (the core 0.29 s of CPU a minute with the
   screen off, 02.11.2026).
2. Mobile peer mode and the mTLS endpoint (VPS, Pi, the assistant): state copy, writes, revoked
   certificate refused. Built 01.11.2026 (`services/home-gateway`, `core/internal/hometunnel`):
   the broker, HA and the primary's calls over the gateway at home and away; away, the primary's
   calls go over the tunnel when no heartbeat shows it, and no state mirror is pulled (only in the
   LAN). Not built yet: no heartbeat on mobile data (the phone still broadcasts), the outbox for
   writes while the gateway is unreachable.
3. Phone tools levels 1 and 2: alarm, call, navigation, music, WhatsApp notifications. Built
   01.11.2026 (`phone_navigate`, `phone_call`, `phone_sms`, `phone_messages`, `phone_reply`,
   `phone_media`, `phone_open_app`, `phone_alarm`); `phone_messages` checked through the core.
   WhatsApp to someone without a waiting notification needs level 3 (accessibility).
4. Android Auto app. Built 01.11.2026, in the car since 02.11.2026; the HUD live on the car
   screen since 03.11.2026 (above), tested in the Desktop Head Unit. Navigation, calls and music
   come from the phone tools (step 3). `CarAudioRecord` is not used (above).
5. Wake word while the car is connected: in use; false starts with music measured and quietened
   (Activation, 03.11.2026). Open: better hits under music need car clips in the wake word
   training (#61); "Hey Google" next to it not yet measured.
6. Camera, then level 3 (accessibility).

## Open questions

- The first music app besides Spotify.
- Accepted battery cost per day and the monthly GPT Live budget for the phone.
- Whether port 443 on the VPS needs the SNI split.
