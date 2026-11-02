# Patches two libraries in .pio/libdeps before building (plain text replacements, logged).
#
# links2004/WebSockets: receive limit 15 KB -> 256 KB. GPT Live echoes the whole session in
#   session.started (prompts with the memory notes and all tool schemas: 58 KB on 28.09., growing
#   with every tool and conversation); beyond the limit the library drops the connection with
#   close code 1009 (the limit was 64 KB until 28.09.). The limit is a plain #define
#   (no #ifndef), so a build flag cannot change it. Received frames above 4 KB come from PSRAM.
#
# mobizt/ReadyMail: APPEND without quotes around the mailbox name. A provider's sent folder is
#   "Sent Items"; unquoted, the space broke the command (copies of sent mails, services/mail).
#   And its text source checks available() before every character through size() = strlen() of
#   the whole text: quadratic. Storing the 21 KB memory core took 18 s of CPU (2 s when it was
#   smaller) and starved the main loop meanwhile; now it checks for the end of the string.
#
# Arduino-ESP32 2.x WiFiClientSecure (framework package): stop_ssl_socket() ends with a memset
#   that leaves socket = 0 instead of -1, so every second stop() of a connection (ReadyMail,
#   our code and the destructor each stop it) calls close(0). Descriptor 0 is the first open
#   LittleFS file: another task's file was closed under it ("assert failed: lfs_file_close",
#   the memory's core file came back empty). Fixed the same way in Arduino-ESP32 3.x.
#   And an opt-in list of TLS cipher suites (weak symbol ssl_client_ciphersuites, nullptr =
#   mbedTLS' defaults, so other projects on this PC are unchanged): Google's servers sent the
#   ESP32 ECDSA certificates (P-256 and P-384), whose checks run in software: 2.1-2.9 s per
#   connection against 0.7 s for Telegram's RSA certificate (30.09.). src/main.cpp offers only
#   ECDHE-RSA suites, so servers send their RSA certificates (hardware-accelerated).
Import("env")  # noqa: F821 (PlatformIO)

import pathlib

PATCHES = [  # (path, old, new, what); "framework:" = in the Arduino framework package
    ("WebSockets/src/WebSockets.h",
     "#define WEBSOCKETS_MAX_DATA_SIZE (15 * 1024)\n#define WEBSOCKETS_USE_BIG_MEM\n#define GET_FREE_HEAP ESP.getFreeHeap()",
     "#define WEBSOCKETS_MAX_DATA_SIZE (256 * 1024)\n#define WEBSOCKETS_USE_BIG_MEM\n#define GET_FREE_HEAP ESP.getFreeHeap()",
     "Empfangsgrenze 256 KB"),
    ("WebSockets/src/WebSockets.h",  # copies patched to 64 KB before 28.09.
     "#define WEBSOCKETS_MAX_DATA_SIZE (64 * 1024)\n#define WEBSOCKETS_USE_BIG_MEM",
     "#define WEBSOCKETS_MAX_DATA_SIZE (256 * 1024)\n#define WEBSOCKETS_USE_BIG_MEM",
     "Empfangsgrenze 64 KB -> 256 KB"),
    ("ReadyMail/src/imap/IMAPSend.h",
     '"APPEND %s%s%s {%d}"',
     '"APPEND \\"%s\\"%s%s {%d}"',
     "APPEND mit Anfuehrungszeichen um den Ordnernamen"),
    ("ReadyMail/src/core/ReadyCodec.h",
     "            return index < (int)size();",
     "            return str[index] != 0;  // patched (scripts/patch_libraries.py): size() is strlen, per character",
     "available() ohne strlen pro Zeichen"),
    ("framework:libraries/WiFiClientSecure/src/ssl_client.cpp",
     "    ssl_client->handshake_timeout = handshake_timeout;\n    ssl_client->socket_timeout = socket_timeout;\n}",
     "    ssl_client->handshake_timeout = handshake_timeout;\n    ssl_client->socket_timeout = socket_timeout;\n"
     "    ssl_client->socket = -1;  // patched (scripts/patch_libraries.py): else close(0) on the next stop()\n}",
     "stop() schliesst nicht mehr Deskriptor 0"),
    # Copies patched before 03.10. (without the per-host hook) are brought up to date first; on a
    # fresh framework these two find nothing and stay quiet (fifth field: optional).
    ("framework:libraries/WiFiClientSecure/src/ssl_client.cpp",
     "__attribute__((weak)) const int* ssl_client_ciphersuites = nullptr;\n\n",
     "__attribute__((weak)) const int* ssl_client_ciphersuites = nullptr;\n"
     "__attribute__((weak)) const int* (*ssl_client_ciphersuites_for)(const char* host) = nullptr;\n\n",
     "Cipher-Suites pro Host (Deklaration nachgeruestet)", True),
    ("framework:libraries/WiFiClientSecure/src/ssl_client.cpp",
     "    if (ssl_client_ciphersuites) mbedtls_ssl_conf_ciphersuites(&ssl_client->ssl_conf, ssl_client_ciphersuites);  // patched\n",
     "    if (const int* suites = ssl_client_ciphersuites_for ? ssl_client_ciphersuites_for(hostname) : ssl_client_ciphersuites)\n"
     "        mbedtls_ssl_conf_ciphersuites(&ssl_client->ssl_conf, suites);  // patched\n",
     "Cipher-Suites pro Host (Aufruf nachgeruestet)", True),
    ("framework:libraries/WiFiClientSecure/src/ssl_client.cpp",
     "int start_ssl_client(sslclient_context *ssl_client,",
     "// patched (scripts/patch_libraries.py): a firmware may define the TLS cipher suites to offer\n"
     "// (0-terminated); other projects keep mbedTLS' defaults.\n"
     "__attribute__((weak)) const int* ssl_client_ciphersuites = nullptr;\n"
     "__attribute__((weak)) const int* (*ssl_client_ciphersuites_for)(const char* host) = nullptr;\n\n"
     "int start_ssl_client(sslclient_context *ssl_client,",
     "Cipher-Suites waehlbar (Deklaration)"),
    ("framework:libraries/WiFiClientSecure/src/ssl_client.cpp",
     "                                           MBEDTLS_SSL_PRESET_DEFAULT)) != 0) {\n"
     "        return handle_error(ret);\n    }\n",
     "                                           MBEDTLS_SSL_PRESET_DEFAULT)) != 0) {\n"
     "        return handle_error(ret);\n    }\n"
     "    if (const int* suites = ssl_client_ciphersuites_for ? ssl_client_ciphersuites_for(hostname) : ssl_client_ciphersuites)\n"
     "        mbedtls_ssl_conf_ciphersuites(&ssl_client->ssl_conf, suites);  // patched\n",
     "Cipher-Suites waehlbar (Aufruf)"),
]

libdeps = pathlib.Path(env.subst("$PROJECT_LIBDEPS_DIR")) / env.subst("$PIOENV")  # noqa: F821
framework = pathlib.Path(env.PioPlatform().get_package_dir("framework-arduinoespressif32"))  # noqa: F821
for path, old, new, what, *optional in PATCHES:
    file = framework / path[len("framework:"):] if path.startswith("framework:") else libdeps / path
    if not file.exists():
        continue
    text = file.read_text(encoding="utf-8")
    if new in text:  # patched already (the new text may contain the old one)
        continue
    if old in text:
        file.write_text(text.replace(old, new), encoding="utf-8")
        print(f"{path.split('/')[-1]}: {what}")
    elif not optional:
        print(f"WARNUNG: {path} unbekannt, nicht gepatcht ({what})")
