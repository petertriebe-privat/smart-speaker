#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>

// Automations in Home Assistant, created by the assistant (tools create_home_automation,
// list_home_automations, delete_home_automation, find_entities). Device control on a schedule
// or on sensors belongs there: HA runs it locally, also when the assistant or the internet is down.
// Uses HA's config API (/api/config/automation/config/<id>, as the HA UI does; needs an admin
// token). Jarvis only touches automations whose id starts with "jarvis_", never the user's.
// To make the assistant speak or act, an automation fires the events jarvis_announce / jarvis_task
// (services/ha_events).
namespace automations {

// Registers the tools with the registry (assistant/tools). They block ~0.1-2 s (LAN).
void addTools();

}  // namespace automations
