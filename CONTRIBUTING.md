# Contributing

Start with the [local setup](docs/setup.md) and [code structure](docs/development.md). Use the synthetic demo or a private temporary profile for development. Do not attach real chats, linked-device credentials, or client tokens to issues or pull requests.

Keep each change focused. Describe the concrete problem, the resulting behavior, and the checks you ran. Update setup or API documentation when behavior changes. A passing test with fake data is useful evidence, but it does not prove live WhatsApp history completeness or native model quality.

Prefer small packages, typed interfaces, and bounded work. Put persistence changes in `internal/archive`, WhatsApp event handling in `internal/ingestion`, and application lifecycle coordination in `internal/app`. Preserve chat and date scope checks, source invalidation, and final response checks when changing search or MCP.

Add a regression test for a bug or a new behavior. Run the relevant package tests and then the checks in [development](docs/development.md). Use `gofmt` for Go changes. Keep comments for decisions and constraints that the code cannot explain; remove redundant narration and unsupported performance claims.

Use synthetic text and media fixtures that may be redistributed. Preserve license notices. Optional model and native-runtime changes need their exact revision, hash, license, and a reproducible test procedure.
