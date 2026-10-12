# Bundled cloudflared 2026.10.0 notices

Source: cloudflare/cloudflared commit `18cdfe0a6fc7b72a0702d255a1f984e776ce0498`.
The root Apache-2.0 LICENSE is retained verbatim. The upstream tree has no root NOTICE.
The upstream release tag is unsigned and executable build metadata says
`vcs.modified=true`; artifact hashes do not establish signed or reproducible provenance.

`modules.json` records the exact compiled module inventories of all three official
executables and the 68 distinct actual module/version pairs after replacements.
Each actual module was fetched with `go mod download` and its module sum matched
executable build metadata. `modules/` retains every discovered LICENSE*, COPYING*,
NOTICE* and PATENTS* file in its original hierarchy, including nested third-party
material. The quic-go and CLI fork replacements are included under their actual
module paths. Go runtime license texts are retained for Go 1.26.8 (Linux) and
Go 1.27.1 (Darwin); Darwin system libraries are supplied by macOS, not redistributed.

The helper runs under Cloudflare service terms and privacy policy. License files
are redistribution notices, not an account, security, compliance or release certification.
Official references: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/license/
and https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/copyrights/.

The tracked `github.com/klauspost/compress@v1.18.0/s2/cmd/internal/readahead/LICENSE` uses LF line endings. Its inventory SHA256 records the tracked bytes, rather than the upstream CRLF representation. The legal text and helper binary download hashes are unchanged.
