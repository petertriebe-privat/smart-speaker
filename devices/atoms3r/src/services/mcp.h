#pragma once

#include <Arduino.h>

// Further MCP servers as tool providers, added by the admin at run time ("verbinde dich mit dem
// MCP-Server von ..."): connect_mcp_server stores name, URL and token in /mcp.json (on the
// device only: tokens never go into the Drive backup), loads the server's tools and offers them
// to the models as "<server>_<tool>" (tools::Provider, like Home Assistant's). Streamable HTTP
// transport: JSON-RPC POSTs with initialize and Mcp-Session-Id, answers as JSON or SSE. HTTPS
// is checked against Mozilla's root CAs (data/cert/x509_crt_bundle.bin, scripts/ca_bundle.py);
// auth is a static token (OAuth logins are not supported).
namespace mcp {

// Registers the provider and connect_mcp_server, disconnect_mcp_server, list_mcp_servers;
// reads /mcp.json (after memory::begin() mounted LittleFS).
void addTools();
// Loads the tools of servers that have none yet (in the tool worker, retrying every
// MCP_RETRY_MS) and closes idle connections. Only between conversations (`allowed`).
void update(bool allowed);
// For the voice instructions: one line per connected server ("" without any), so GPT Live
// knows what it may delegate.
String summary();
void print();  // serial
// Whether the TLS connection being opened to `host` is an MCP request (for the cipher suites in
// main.cpp: MCP servers also get ECDSA, many self-hosted ones have only an ECDSA certificate).
bool connecting(const char* host);

}  // namespace mcp
