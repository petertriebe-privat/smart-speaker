# Release signatures

One scheme for every device (issue #15): ECDSA P-256 over SHA-256 of a file's exact bytes, the
signature next to it as `<file>.sig` in DER (ASN.1). The ESP32's mbedTLS has no Ed25519; Go's
`ecdsa.VerifyASN1`, Java's `Signature.getInstance("SHA256withECDSA")`, mbedTLS'
`mbedtls_pk_verify` and `openssl dgst -sha256 -verify` all read it.
`services/deployer/release.py` makes and checks the files.

- `manifest.json` per release: `version`, `channel` (`rel` or `dev`), `commit`, `date`,
  `sequence`, `artifacts` (`file`, `size`, `sha256`), optional `min_version`.
- `releases.json`, the index of the releases on the Pi: `sequence`, `date`, `current` per channel
  (a dev device runs the newest of all), `releases` (`version`, `channel`, `commit`, `date`,
  `manifest_sha256`).

Rules for every verifier: check the signature before reading the JSON, refuse an index whose
`sequence` is lower than the last one seen, install only a version the signed index lists, and
check each artifact's size and sha256 against its signed manifest.

`testdata/` holds the same vectors for every language: `test-key.pub` (a throwaway key; its
private half was deleted after signing), `manifest.json` and `releases.json` with their
signatures. A verifier must accept both and refuse them with any byte changed. The
`.gitattributes` there keeps git from converting the line endings of the signed bytes.
