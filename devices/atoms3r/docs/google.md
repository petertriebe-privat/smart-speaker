# Google services

Everything here is optional. Without Google settings the tools below are simply not registered.

| Tools | API | Access |
|---|---|---|
| `list_events`, `find_free_time`, `create_event`, `update_event`, `delete_event` | Calendar API | OAuth |
| `find_contact`, `save_contact`, `upcoming_birthdays`, caller names on the phone | People API | OAuth |
| memory backups and archive, tables | Drive API, Sheets API | OAuth (`drive.file`) |
| `list_todos`, `add_todo`, `change_todo` | Google Tasks API | OAuth |
| watched Gmail mailboxes (`mail_news`, `search_private_mail`, `read_private_mail`, notices) | Gmail API (read only) | OAuth |
| `find_place`, `travel_time`, `get_weather` | Places API (New), Routes API, Weather, Pollen, Air Quality API | API key |
| `locate_person` (address of a household member's phone, from Home Assistant) | Geocoding API | API key |
| `find_video` | YouTube Data API v3 | API key |
| `summarize_video` | Gemini API | AI Studio key |
| `play_video`, `control_video` | none: DIAL and the YouTube Lounge API in your LAN | — |

A member M5 (`JARVIS_INBOUND 0`) never sends the login link (only the main device does), so an
expired login of its own stayed expired (found 25.10.2026). `GOOGLE_REFRESH_TOKEN_WEB` in
`secrets.h` takes the main Echo's web-client login (the `refresh_token` of its `google.json`):
it is used when no login from a link is stored, and when the stored one is refused (that one is
then deleted).

That copy does not follow when the Echo renews its login by link: it expired, and Google stayed
off on the member (found 08.11.2026). So a member whose login is refused (or that has none) takes
the main device's: the signed peer call `/google_login` (core `serveGoogleLogin`, the primary's
`google.json`, only a web-client login) goes into NVS as a link's login would, and the call is
tried again, once. The Echos and sticks do the same through the mirror of the primary's state
(`applyLogins`, every 2 minutes). Without a reachable primary with a renewed login, the member
says so instead of promising a link.

`update_event` on both devices: the backend fills the parameters it does not change with `""` and
`0` (strict schemas), so those mean unchanged; `time: "all_day"` makes an event all-day. Until
24.10.2026 an empty title or notes replaced the event's own, and changing only the place blanked
the title.

## 1. Cloud project and APIs

In the [Google Cloud Console](https://console.cloud.google.com/) create a project and enable (APIs &
Services → Library): Google Calendar API, People API, Google Drive API, Google Sheets API, Google
Tasks API, Gmail API, Places API (New), Routes API, Geocoding API, Weather API, Pollen API, Air Quality API, YouTube Data
API v3, Gemini API (Generative Language API).

The Workspace **MCP** servers (Drive MCP, Sheets MCP) are not used: they are a developer preview
and need `drive.readonly`, i.e. read access to the whole Drive.

## 2. OAuth login (calendar, contacts, Drive, Sheets, Tasks)

1. OAuth consent screen: user type *External*, add yourself as a user, and set the app to
   **In production** (in *Testing* refresh tokens expire after 7 days). Google shows "unverified
   app" during the login; continue via *Advanced*.
2. Credentials → Create credentials → OAuth client ID → type **Desktop app**; download the JSON.
3. On the PC: `python tools/google_auth.py path/to/client_secret_….json`. It opens the browser
   (tick all permissions), gets a refresh token, checks every API and writes `GOOGLE_CLIENT_ID`,
   `GOOGLE_CLIENT_SECRET` and `GOOGLE_REFRESH_TOKEN` into `local/secrets.h`. Flash again.

### Renewing the login without the PC

A refresh token can still die (app back in *Testing*, password change with Gmail scopes, revoked
in the account). Then the assistant renews it itself: with `GOOGLE_WEB_CLIENT_ID` and
`GOOGLE_WEB_CLIENT_SECRET` set and the WhatsApp relay running, the first `invalid_grant` makes it
send the admin a login link by Telegram (WhatsApp without Telegram; again every 12 h while it is
still expired; tool `google_login` on request). The admin logs in and ticks everything; Google
sends the browser to the relay's `/google` page, the device fetches code and state with its next
poll, exchanges the code and keeps the new refresh token in NVS (`google`/`refresh`), which then
wins over `GOOGLE_REFRESH_TOKEN`. No flashing. Tools tell the model that the link is on its way,
so the assistant does not invent other fixes.

- Setup once: Credentials → Create credentials → OAuth client ID → **Web application**,
  authorized redirect URI `<WHATSAPP_RELAY_URL>/google`; deploy the relay (`npx wrangler deploy`
  in `services/whatsapp-relay/`). Same project as the desktop client, so `drive.file` still covers the assistant's
  files (checked 14.10.: the backups written with the desktop client's token read fine).
- Why not the device flow ("enter this code at google.com/device"): Google allows it only for a
  few scopes, not Calendar, Contacts, Tasks or Gmail.
- Security: the relay page needs no token. The device accepts only the state of its own pending
  link, and the code is useless without the web client's secret and the PKCE verifier, both only
  on the device.
- Device firmware with the token in `secrets.h` only was a trap: a dead token needed the PC and
  a flash, and a task agent running `google_auth.py` on the server renewed only its own token.

Scopes: `calendar.events`, `calendar.readonly` (all calendars, free/busy), `contacts`,
`drive.file` (only files the assistant created: the folder *<Name>* with backups, `Themen`,
`Gespräche`, `Tabellen`), `tasks`, `gmail.readonly` (the watched mailbox, see below). Run the
script again whenever a new scope is added.

### Gmail (watched mailboxes)

The assistant reads a Gmail inbox when it is listed in `WATCHED_MAILBOXES` with host `"gmail"`
(assistant/mail_watch, issue #37): the admin's with secret `""` (the token above), another
person's with their own refresh token. For that, add their Google account as a test user of the
consent screen and run `python tools/google_auth.py --gmail-only client_secret_….json` while
logged in as them: it asks only for `gmail.readonly`, writes nothing and prints the token for
their entry. `gmail.readonly` cannot change or delete anything; sorting into labels and the
trash (a later stage) will need `gmail.modify`, which still cannot delete for good.

### Token for the task agent

`python tools/google_auth.py --agent OUTFILE` (the assistant's client from `secrets.h`, or a client
JSON as second argument) asks for `drive.file`, `gmail.modify` (read and sort; the MCP server
refuses trash and spam), `calendar.readonly` and `contacts.readonly` only and writes client id, secret and refresh token as JSON to `OUTFILE`; that
file goes to the agent's server (`/run/secrets/google.json`), nowhere else. Its own token, so it
can be revoked without touching the assistant; the same client, so `drive.file` shows it the assistant's files.

New mail is found with the history API (`history.list` since the last `historyId`, inbox only,
drafts and own mails skipped); on the first check, or when the starting point is older than
Google keeps it (about a week), the assistant starts from now; the last 14 days are filed into its index
afterwards (`messages.list?q=in:inbox newer_than:14d`). Bodies come as `format=full` (base64url
parts, Latin-1 parts converted). A search is `messages.list` with Gmail's own query syntax plus
`newer_than:<days>d`, then `format=metadata` per hit for the headers and Gmail's preview.

### Calendars

`list_events` reads every calendar that is shown in Google Calendar (hidden ones are skipped) and
labels each entry with its calendar. Only entries in calendars the user **owns or can write** that
are marked **busy** and not declined take the user's time; holidays, school and other subscribed
calendars, and entries marked *free*, are reported as `info`. `find_free_time` uses the same rule.
Nothing about specific calendars is configured.

## 3. API key (Maps, weather, YouTube search)

Credentials → Create credentials → API key → **API restrictions**: Places API (New), Routes API, Geocoding API,
Weather API, Pollen API, Air Quality API, YouTube Data API v3. Put it in `GOOGLE_MAPS_API_KEY`.

The firmware counts the requests per API and calendar month in `/quota.json` and refuses at the
limits in [include/config.h](../include/config.h) (`PLACES_MONTHLY_LIMIT`, `ROUTES_MONTHLY_LIMIT`
1000 each, weather/pollen/air below Google's free tiers); the admin gets a Telegram message at
80 %. Places with phone numbers and opening hours are an *Enterprise* SKU (1000 free a month).
As a second safety net, lower the daily quota per API in the Cloud Console. Serial:
`>maps_usage {}`.

## 4. Gemini key (video summaries)

New Gemini keys must be *authorization keys* bound to a service account. The easiest way:
[Google AI Studio](https://aistudio.google.com/apikey) → *Create API key* in your project; it does
the binding itself. Put it in `GEMINI_API_KEY`. YouTube URLs are a preview feature of the Gemini
API (public videos only; free tier: 8 hours of video a day). The firmware counts the calls
(`GEMINI_MONTHLY_LIMIT`).

## 5. YouTube on the TV

`play_video` needs no configuration: the device finds TVs in the LAN over SSDP (DIAL), starts the
YouTube app, takes its screen id and sends the video over YouTube's Lounge API, the way a phone
casts. Tested with Fire TV sticks and a Samsung smart TV. The TV has to be on. The app resumes
where the TV's account left a video, so `play_video` seeks to the start a few seconds later.

## Moving an old mailbox archive

Versions before the Drive archive kept dossiers and transcripts as mails.
`tools/migrate_archive.py` copies them into Drive (`--dry-run` first); it skips what is there
already, so it can run again.
