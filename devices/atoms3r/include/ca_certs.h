#pragma once

// Root certificates of the TLS connections (defined in services/ca_certs.cpp).

// Root CAs for api.openai.com (Google Trust Services, cross-signed by GlobalSign).
// GTS Root R1 and ISRG Root X1 are spares in case the chain changes.
extern const char CA_CERTS[];

// Root CA of the mail provider (MAIL_SMTP_HOST / MAIL_IMAP_HOST). For another provider, replace
// it in ca_certs.cpp with the root of its chain (openssl s_client -connect host:993 -showcerts).
extern const char MAIL_CA[];

// Root CA for api.telegram.org: Go Daddy Root Certificate Authority - G2 (valid until 2037,
// SHA-256 45:14:0B:32:...:19:8E:DA), via Go Daddy Secure Certificate Authority - G2.
extern const char TELEGRAM_CA[];

// Root CA for Meta (graph.facebook.com, lookaside.fbsbx.com: WhatsApp): DigiCert Global Root G2
// (valid until 2038, SHA-256 CB:3C:CB:B7:...:CB:5F), via DigiCert Global G2 TLS RSA SHA256 2020 CA1.
extern const char META_CA[];

// Root CAs for accounts.spotify.com (token endpoint): Starfield Root Certificate Authority - G2
// (until 2037) via Certainly Intermediate R1, and Certainly Root R1 as a spare. api.spotify.com
// is DigiCert Global Root G2 (META_CA).
extern const char SPOTIFY_ACCOUNTS_CA[];

// Root CA for api.tavily.com (AWS): Amazon Root CA 1 (valid until 2038, SHA-256
// 8E:CD:E6:88:...:19:6E), via Amazon RSA 2048 M01.
extern const char AMAZON_CA[];
