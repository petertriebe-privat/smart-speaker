# Several Echos in one household (plan)

As of **2026-10-18**, draft. A second Echo Gen 1 runs with WLAN and SSH ([boot-and-root.md](../hardware/gen1/docs/boot-and-root.md),
[network.md](../hardware/gen1/docs/network.md)), but without the assistant so far. This document defines how
several Echos work together: what each device does itself, what may exist only once in the house,
how one device takes over from a failed one, and how devices are assigned to rooms.

## Starting point

`settings.json: "primary"` today statically divides into a main and a secondary device
(`cmd/assistant/services.go`):

| Main device only | Every device |
| --- | --- |
| SIP registration at the FritzBox, incoming and outgoing landline calls | Conversations at the speaker, wake word, quick commands |
| Telegram, WhatsApp relay, mail commands, mail monitoring | Home Assistant (MCP), presence, all outgoing tools |
| HA events (`haevents`), Google login renewal | Phone over Bluetooth (`RunMobile`) |
| Backups to Drive, writing the archive | Spotify Connect (librespot) |
| Restoring tasks from the backup | Task scheduler (`tasks.Run`) for its own list |

Problems as soon as a second Echo runs:

- **State per device:** Memory (`memory/core.json`), people, tasks, `agenda_said.json`,
  `phone_speaker.json`, chat histories, mail and Telegram offsets live in `/mnt/data/assistant` of each
  device. What the secondary Echo learns or plans, the main device does not know, and without backup rights
  it is lost on a switch.
- **Both hear "Hey Jarvis":** If two Echos are within earshot, both open a conversation, both
  answer, both run tools (duplicate message, duplicate call).
- **Failure:** If the main device is off, there is no telephony, no chats, no mail commands.
- **Same Spotify name** (the assistant's name) on both devices.
- A phone call can only be heard and conducted on the device with the SIP registration.

## Principles

1. **Roles instead of devices.** Every Echo is a *room device* (speaker, microphone, ring, button,
   music). Exactly one is additionally the *house assistant* (leader): it accepts incoming things and
   owns the shared state. The role moves on a failure, the room device does not.
2. **One writer per state.** Shared state is written only on the leader and mirrored to the
   others. A secondary device does not write itself but asks the leader to.
3. **Everything works alone.** Every device must work fully without its partners (conversation, HA,
   music). The group is an improvement, not a prerequisite: if no partner is reachable,
   the device acts as if it were alone.
4. **Telephony does not depend on the election.** Every device registers with the assistant's account; the
   FritzBox rings all of them (tested, see Telephony). The phone fallback therefore works immediately, without waiting for a leader election.
5. **One protocol for everything:** The *peer link* (below) carries heartbeat, wake-word arbitration,
   tool calls, state mirroring and audio.

## What runs where

| Part | Where | Reason |
| --- | --- | --- |
| Conversation (GPT Live), wake word, quick commands, confirmation clips | every device | room function |
| Ring, button, mute, volume, self-test, error and health log | every device | hardware |
| Spotify Connect | every device, own name ("<name> Kitchen") | music per room; `move` between Echos exists already |
| Home Assistant (MCP tools), presence, weather, web search, maps, Fire TV, YouTube | every device | stateless, outgoing only |
| Outgoing messages (send Telegram/WhatsApp/mail), images, files | every device | outgoing only; for chat history see state |
| SIP registration | every device, **the same account** (one binding per device) | rings everywhere, fallback without election |
| Independent call by the assistant with a task (`phone_call`) | the device where it was requested | own registration, own `phoneSession` |
| Answering an incoming call to the assistant | leader (secondary device only without a leader) | one assistant on the phone |
| Phone over Bluetooth (HFP) | the device the phone is connected to | a phone usually holds only one hands-free connection |
| Telegram, WhatsApp, mail pollers, mail monitoring, HA events | leader only | duplicate pollers take messages away from each other (Telegram 409, relay `after`) |
| Google login renewal, backups, archive, dream/consolidation | leader only | one writer |
| Task scheduler (reminders, wake-up calls, scheduled jobs) | leader only, output in the target room | otherwise executed twice |
| Memory, people, tasks, agenda, speaker permissions, quotas, chat histories | leader writes, all read a copy | one writer |
| Wake-word clips, volume, Spotify login, TV cache | every device, local | device-specific |

## Peer link

A small service in the assistant (`internal/peer`), LAN only (and over Tailscale as a fallback path):

- **Partners** from the device setting (`peers: ["echo-kitchen", …]`, host names or addresses,
  not in the repository). Later optionally via mDNS.
- **Authenticated** with a shared key (`PEER_KEY` in `secrets.h`, HMAC over every
  message with a timestamp against replay). The link can run tools and open a microphone;
  without the key it accepts nothing.
- **UDP** for heartbeat and wake-word arbitration (short, lossy is fine), **TCP**
  (HTTP) for tool calls and state, **RTP-like UDP** for audio (20 ms frames, 16 kHz PCM;
  the LAN carries this uncompressed, ~256 kbit/s).
- **Heartbeat** every 2 s: device, room, role, leader epoch, priority, version, state (free /
  conversation / call / music), SIP registered, phone connected, state revision (version number).
- **Version check:** different protocol version → no group, warning in the error log; this way
  a half-rolled-out update breaks nothing.

Memory need: a few hundred KB; the Echo has room for it, and so does the M5 for heartbeat and
wake arbitration.

## Leader election and fallback

- Every device has a **priority** (`settings.json: priority`). At startup the device with the
  highest priority among the reachable devices becomes leader, but only if there is no leader yet.
- **Lease:** The leader confirms its role with every heartbeat. If it is missing for **45 s** (WLAN
  dropouts should not trigger a change), the device with the next priority takes over, with a
  higher *epoch*.
- **Against split brain** (one device's WLAN disturbed, but it keeps running): only a device that
  itself reaches Home Assistant and the internet and sees the old leader neither on the LAN nor over
  Tailscale may take over. If an old leader returns and sees a higher epoch, it steps down immediately.
  Remaining risk for a few seconds: two Telegram pollers (Telegram allows only one, 409) and
  two relay pollers (the offset is mirrored too).
- **Sticky:** If the original main device returns, the new leader stays leader. Back only
  on command (`ctl leader <device>`, by voice "move the house assistant back to the living room") or at night
  when both are free. Jumping back and forth costs state reconciliation and poller starts.
- **Handover:** The old leader stops pollers and scheduler, sends the latest state, the new one
  starts them. A failed leader can no longer send anything; then the last mirror applies
  (lag of seconds) and, as a fallback, the Drive backup.
- **Notification:** The leader notifies the admin by Telegram/WhatsApp when a device is missing for longer than
  10 minutes, and after a takeover who the house assistant now is.
- `settings.json: "primary"` becomes `priority`; `primary: true` corresponds to the highest priority,
  `primary: false` to "never leader" (this is how the M5 stays for now).

## Shared state

- **Leader writes, mirror to all:** After every change to a state file (`store` package)
  the leader sends the file with a version number to the partners; they write it to a
  mirror folder and read from there (memory in the prompt, people, tasks in the agenda).
  A secondary device that returns fetches the whole state at the first heartbeat.
- **Writing from the secondary device:** Tools that change state (`remember`, `create_task`,
  `add_person`, `speaker_access`, `mail_rule`, …) get the flag
  `Leader` in the registry. On a secondary device the leader runs them (tool call over the peer link with person
  and channel); the result returns to the conversation as normal. Memory entries from the
  conversation summary take the same path.
- **No leader reachable:** The tool answers with an error ("House assistant not reachable,
  I will remember it as soon as it is back") and puts the call in a small queue that is
  worked off at the next leader. After a takeover there is a leader again.
- Staying local: wake clips, volume, librespot access, `youtube_tv.txt`, error and health log.

## Wake word: who answers?

Both Echos within earshot detect the same "Hey Jarvis". Arbitration as with Alexa:

1. On a detection the device immediately sends a *claim* by UDP: time, wake peak,
   microphone level of the utterance (louder = closer).
2. It waits **200 ms** for claims from others (the conversation connects in the meantime,
   the recording runs; the user does not notice).
3. The highest value wins; on a tie the higher priority. The others abort silently
   (ring briefly off, no clip, music not lowered or immediately back).
4. The **button** always wins on its device, without arbitration.
5. No partner reachable → no arbitration, the device answers.

Cost: one UDP packet per detection, 200 ms wait that disappears in the connection setup. The
wake clips get the outcome (`won`/`lost`) for evaluation.

## Telephony (landline)

**Setup:** All Echos register with the **same** IP phone account of the FritzBox
(`SIP_USER`/`SIP_PASSWORD` as today); a second account is not needed. Tested on 2026-10-18
with a SIP test client from the PC next to Echo 1:
- A query registration (REGISTER without Contact) listed Echo 1 as the only binding; after the
  test client registered, the FritzBox held **both bindings** (Echo 1 unchanged), after
  its deregistration (Expires 0) only Echo 1 again.
- A call to the assistant's number arrived as INVITE **at the test client too** (it answered only with
  180 Ringing); 4 s later, when Echo 1 had answered, the FritzBox sent it CANCEL. So it
  forks to all bindings of an account and ends the rest as soon as one answers.
- Consequence for the assistant: Every device registers as today, a secondary device too. Only the leader
  answers automatically; the others ring (ring, ringtone) and answer only on a button press.
  Every registration needs its own Contact address (own IP; port 5060 may be the same).

**Incoming call on the assistant's number:**

| Situation | Behavior |
| --- | --- |
| It rings | The FritzBox rings all Echos. Each shows the ringing pattern and plays a ringtone |
| Nobody presses | The leader answers immediately as today and the assistant conducts the call (decided 2026-10-18). The FritzBox ends the ringing of the others; all Echos keep showing "assistant is on the phone", and on each one can listen in and join (below) |
| Button on an Echo while it rings | **This** Echo answers, as a human: speaker and microphone, without GPT Live (like `dial_for_me`). No detour over the peer link, the FritzBox connects directly |
| "Jarvis, pick up" on an Echo | This Echo answers, the assistant conducts the call there |
| Leader off | It is not registered, the FritzBox rings only the others. A secondary device answers if it sees no reachable registered leader; fallback without election |
| The assistant conducts the call on Echo A, the user is at Echo B | Button on B or "move the call here": listen in/speak along on B (modes `listen`/`talk`). The call's audio runs over the peer link from A to B and B's microphone back; A mixes it into the existing bridge like its own microphone |
| Conference ("Jarvis" during a human phone call) | On the Echo where the call runs, as today (#54) |
| Hand over | "Hand the call over to the kitchen": as above, B takes over speaker and microphone, A releases them |

**Outgoing:** Every device dials through its own registration; `dial_for_me` and `phone_call` run where
they are requested. Call reports and the activity log go to the leader (state).

**Dial-in from outside** (`connect_speaker`): The caller names a room ("connect me to the
kitchen"); without a room the device where someone last spoke, otherwise the leader's.
`speaker_access` remains a list in the shared state.

## Phone over Bluetooth

- The phone is paired with every Echo, but for calls usually connects to only one
  (iPhone: one hands-free device; Android with multipoint perhaps two). That device is the
  *phone host* and reports this in the heartbeat.
- When the phone rings, all Echos show it. Button on another Echo → answer at the host, audio
  over the peer link to the device pressed (the same building block as for the landline).
- `mobile_call` (calling by voice) on a secondary device goes to the host.

## Rooms and Home Assistant

**Assignment:** `settings.json` per device gets `room` (display name) and `ha_area` (area ID in HA).
The values exist only on the device, not in the repository.

**What the room does:**

- **Context in the prompt:** "You are talking to someone in the room *Kitchen*." Without a named room,
  "turn on the light", "how warm is it", "blinds down" apply to this room (HA intents with `area`).
  The same mechanism as the context for date and person.
- **Spotify name** "<name> *Room*"; `play_music` without a speaker plays in the own room (already
  today), "bring the music here" works between Echos (already `move_here` today).
- **Outputs in the right room** (`speak`, reminders, wake-up calls, `start_conversation`,
  HA events, mail notices). Target in this order:
  1. explicitly named room ("remind me in the kitchen", "announcement everywhere");
  2. the room of the conversation in which the task arose (the task stores it);
  3. where the person currently is: presence per room from HA (motion/presence sensors of the area),
     otherwise the Echo where they last spoke (up to 30 min);
  4. otherwise the leader's device (or all, depending on type: a wake-up call goes to the person's
     bedroom, `people` gets a default room for this).
  The leader plans, the target device speaks (tool call over the peer link).
- **`speak` and `start_conversation`** get a field `room` (`""` = here, `"all"` = everywhere).

**Making the Echo visible in HA:**

- Preferably **MQTT Discovery** (if HA has a broker): each Echo as an HA device with
  `suggested_area`, entities state (`sensor`: free/listening/speaking/call), volume
  (`number`), microphone muted (`switch`), house assistant (`binary_sensor`), online (availability), plus
  a command topic for announcements. With that HA can build automations per room ("when the front door
  opens, say in the hallway …") and the Echo appears in the area.
- Without a broker: states over the REST API (`POST /api/states/…`, no area assignment possible) and
  announcements through the existing tool. This is the weaker solution.
- Later: read the area assignment from HA instead of entering it (the device finds its area via its
  own HA device ID).

## Further cases

| Case | Behavior |
| --- | --- |
| Two people talk to two Echos at the same time | two independent conversations, both write to memory via the leader |
| `tell_other_conversation` | also across devices (conversations visible in the heartbeat) |
| Intercom between rooms ("call the kitchen") | peer audio directly, without GPT Live; the same building block as the call on another Echo (phase 7) |
| Announcement everywhere | all devices; running conversations take priority as today (`Announcer.Busy`) |
| Synchronous music in several rooms | not with librespot alone; separate topic (phase 8, own issue) |
| End-of-conversation tone, ducking | local; a conversation in the kitchen does not lower the music in the living room |
| Leader device is on the phone, secondary device gets "call X" | the secondary device dials itself (own registration), independently |
| Update | Deploy to all devices one after another; the protocol version prevents old and new from exchanging state |
| An Echo without WLAN | keeps working alone, but has no GPT Live without internet anyway; ring yellow |
| M5 | member of the group since 2026-10-19 (#66): heartbeat with `no_lead` (never leader, does not count in the election) and `occupied` (its presence sensor), wake arbitration, tools and conversations via the leader, emergency memory with journal without a leader, `/speak` and `/start_conversation` to it. Details: `devices/atoms3r/docs/architecture.md`, *A network of devices* |
| Echo Dot 2 | in the group since 2026-10-20, `no_lead` in its `device.json` (a setting, like the M5 never leader): it lost the other devices' broadcast heartbeats in bursts, at 18:49 took over the main role from a healthy living-room Echo, and the Echos followed the higher epoch. Back with `ctl primary` on the living room. Cause in the Dot's WLAN: #72 |

## Operation

- **Separate device settings:** `settings.json` from the repository is the template, the
  device-specific values (`primary`, `wake_word`, `room`, `ha_area`, `spotify_name`, later
  `priority`, `peers`) live next to it in `device.json` and override it. Source:
  `devices/echo/local/devices/<host>.json` (private); without it the device keeps its file.
- **SIP access:** shared from `secrets.h`, no device-specific secrets needed.
- **Deploy:** `deploy.sh` with `ECHO_HOST=echo2` (already today via `local/ssh/config`) and
  `--all` for all devices one after another, tests once beforehand.
- **Tailscale:** second name (`assistant-echo2`), so the server reaches both.
- **Docs:** after implementation this document becomes the description; `16-assistant.md` links to it.

## Phases

| Phase | Content | Benefit afterwards |
| --- | --- | --- |
| 1 | Device settings (`device.json`), deploy for two devices, own Spotify name, Echo 2 as secondary device (`primary: false`) | second Echo speaks, controls HA and music — usable immediately, as long as the two do not hear each other |
| 2 | Peer link (heartbeat, key, version) and wake-word arbitration | two Echos within earshot answer only once |
| 3 | SIP registration on every Echo (only the leader answers automatically), ringing on all Echos, answering by button on any Echo, phone fallback | landline everywhere, even if an Echo fails |
| 4 | Shared state: mirroring, `Leader` tools, queue | one memory, one task list |
| 5 | Leader election, takeover of pollers and scheduler, notification to the admin | Telegram, WhatsApp, mail, HA events survive a failure |
| 6 | Rooms: prompt context, HA area, outputs in the right room, MQTT Discovery | "light on" means this room; reminders where one is |
| 7 | Audio over the peer link: call and phone on another Echo, intercom between rooms | every call on every Echo |
| 8 | (optional) synchronous music in several rooms | — |
| — | M5 in the group: heartbeat, wake arbitration, state at the leader, emergency memory | #66, running since 2026-10-19 |

**Status:** Phase 1 has run since 2026-10-18 (`device.json`, deploy with `ECHO_HOST`, installer for
main-A and main-B, Echo 2 as a secondary device without wake word; details in
[16-assistant.md](16-assistant.md#main-and-secondary-device)). Open from phase 1: deploy to all devices in
one run, room for Echo 2, Tailscale for Echo 2. Phase 3 (landline) has also run since
2026-10-18, with one difference from the plan: the fallback needs no leader election, the secondary device
recognizes from the FritzBox's answer whether it is registered alone. Phase 2 (peer link,
wake-word arbitration) runs too; deviating from the plan, the conversation starts immediately and the
loser aborts (instead of waiting 200 ms), and the partners find each other by broadcast without an
address list (`peers` only as an exception). Phase 4 runs in a first form: instead of mirroring the state,
the secondary device asks the main device (tools, context, end of conversation); mirroring
is done for phase 5 (takeover). Phase 5 has run since 2026-10-19 (details in
[16-assistant.md](16-assistant.md)); deviating from the plan 120 s instead of 45 s until takeover, because an
ordinary restart of the main device already takes ~50 s, and the check over Tailscale is dropped
(Echo 2 has no Tailscale yet), what counts is the reachability of Home Assistant. Phase 6 runs
without MQTT (room in the context, `speak`/`start_conversation` with room), phase 7 is built and checked
with unit tests, a real call is pending. The M5 has been in the group since 2026-10-19 (#66).
Open: enter rooms, MQTT (#65), phase 8, Tailscale for Echo 2. Issues: #58 (phases 1 and
6), #55 (real calls of phases 3 and 7), #65 (6), #66 (M5); done: #62 (2), #32 (4), #64 (5).
Repeated calls of `/tool` and `/conversation` with the same `id` (the M5's journal) are run by
the main device only once (`replays` in `cmd/assistant/shared.go`, the last 200).

Order: phase 2 before everything else, as soon as both Echos can hear each other; phase 4 before phase 5,
because a takeover without mirrored state would lose memory and tasks.

## Decisions (2026-10-18)

1. **Earshot:** For development both Echos stand next to each other, later in rooms that hear each other quietly with
   open doors. Until phase 2 is done, the wake word on Echo 2 is therefore off
   (`wake_word: false` in its device settings; button works). Quietly means: the arbitration
   by level decides clearly, but without it both would answer.
2. **Answering:** The assistant keeps answering incoming calls immediately. Listening in and joining at any
   Echo (phase 7) is the way for that, not an answer delay.
3. **MQTT:** HA has no broker yet; Mosquitto will be set up together in phase 6.
4. **Leader role:** stays with the new device after a change (sticky), no switching back.
5. **Phone:** Android. Multipoint is not needed: the Echo the phone is connected to
   shares the call with the others over the peer link.
