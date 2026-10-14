<img src="internal/web/static/logo.svg" width="64" height="64" alt="">

# Repo Speaks

Ask your codebase questions in plain language. A self-hosted chat for people who don't read code.

> **Status:** early development: a server skeleton, nothing usable yet. Terms are defined in the [glossary](docs/glossary.md).

## What it is

A developer connects GitHub repositories and markdown docs to a project, sets up agents, and invites teammates. Teammates such as product managers, operations, marketing, managers or clients ask questions. They get advisory answers with sources, without access to the repository.

## Development

Requires Go 1.27 and git 2.32 or newer. The server checks git at start and stops with a message if it is missing or too old.

```sh
go run .          # start the server on http://localhost:8080
go test ./...     # run all tests
```

On first start, open http://localhost:8080/setup to create the owner account.

| Variable | Default | Meaning |
| --- | --- | --- |
| `REPO_SPEAKS_ADDR` | `:8080` | Address the server listens on |
| `REPO_SPEAKS_DATA_DIR` | `data` | Folder for the database and repository clones |

### Forgotten passwords

The owner makes reset links for other accounts on the Accounts page. If the owner forgets their own password, run this on the server, with the same `REPO_SPEAKS_DATA_DIR`:

```sh
repo-speaks reset-link <username>   # in development: go run . reset-link <username>
```

It prints a path such as `/reset/...`. Put the server's address in front of it and open it in a browser. The link works once, stops after 24 hours, and ends every session of the account. The server can keep running.

### Adding repositories

A builder adds a GitHub repository on the project page, as `owner/repo` or its GitHub address, with an optional branch. The server clones the latest commit when the repository is added, when someone presses "Sync now", and on the project's timer (every 6 hours by default). Repositories over 1 GB are refused, files over the project's limit (5 MB by default) are left out, and submodules are not cloned. Behind a proxy, git uses the server's `HTTPS_PROXY` and `NO_PROXY` settings.

A private repository needs a token. On GitHub, go to Settings, Developer settings, Personal access tokens, Fine-grained tokens, and generate a new token. Pick the repository's owner, choose "Only select repositories" with that one repository, and set the repository permission "Contents" to "Read-only". Paste the token into the Token field. Repo Speaks never shows it again and sends it only to GitHub, but it stores it in plain text: anyone with a copy of the data folder or a backup has it. Keep the data folder readable by the app's user only.

### Behind a reverse proxy

Repo Speaks speaks plain HTTP. Put a reverse proxy such as Caddy or nginx in front of it for HTTPS.

- Keep the original `Host` header (in nginx: `proxy_set_header Host $host;`). Without it, older browsers fail every form with 403.
- Set `Strict-Transport-Security` at the proxy. The app does not send it, because it cannot know whether HTTPS is used.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it is built.

## License

[MIT](LICENSE)
