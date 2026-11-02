#pragma once

#include <Arduino.h>

// The peer link of a network of devices (config.h MEMBER, #66), the same protocol as the Echos'
// (core/internal/peer, devices/echo/docs/18-several-echos.md). UDP datagrams to the LAN's
// broadcast address on PEER_PORT, each signed with HMAC-SHA256 under PEER_KEY:
//
//   J1 <hex HMAC of the JSON> <JSON message>
//
// a heartbeat every 5 s, and a claim when the wake word was heard here. Calls to another device
// (the leader's tools, context, conversations) are HTTP on its PEER_PORT + 1, request and answer
// signed in the header X-Jarvis-Auth: "<unix ms> <nonce> <hex HMAC of '<ms> <nonce> <path> <body>'>".
//
// Wake arbitration: the device that heard the wake word starts its conversation at once and
// claims with the microphone's level in the second before; after CLAIM_WINDOW_MS the loudest
// claimant keeps it (ties: priority, then name), the others drop theirs before it says anything.
// A device with a conversation open answers a claim with its own level (busy), so the user it
// talks to stays with it. This device never takes the leader role (no_lead in its heartbeat).
namespace peer {

void begin();   // after network::begin(); nothing without PEER_KEY
void update();  // main loop: receive, heartbeats, the claim's decision

// The wake word was heard here (score 0..1). True: answer at once (no other device present);
// else the decision comes after the claim window: takeLost().
bool claim(float score);
bool deciding();             // a claim waits for the window: the conversation does not connect yet
bool takeLost(String& why);  // once, after a lost claim: drop the conversation silently

// Other devices heard from in the last 15 s, and the leader among them.
int alive();
bool leaderPresent();
// One signed call on the leader (POST path with the JSON body); its answer in `answer`. Blocks up
// to timeoutMs: from a worker task, never the main loop. False (with the reason in `answer`) if
// no leader is present or the call failed.
bool callLeader(const char* path, const String& body, String& answer, uint32_t timeoutMs);

// Calls from the other devices to this one (the leader routes announcements and conversations
// to this room): `handler` answers the JSON body of POST `path` with a JSON text. It runs in the
// main loop: it must not block (hand slow work to a worker).
using Handler = String (*)(const String& body);
void handle(const char* path, Handler handler);

// Tests (serial O): as if no leader were reachable, for the emergency memory (assistant/shared).
void setOffline(bool on);
bool offline();

String describe();  // the link for the console and the instructions: devices, rooms, roles
String leaderName();  // "" without a leader
void print();       // serial

}  // namespace peer
