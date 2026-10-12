# Repo Speaks

Ask your codebase questions in plain language. A self-hosted chat for people who don't read code.

> **Status:** early development: a server skeleton, nothing usable yet. Terms are defined in the [glossary](docs/glossary.md).

## What it is

A developer connects GitHub repositories and markdown docs to a project, sets up agents, and invites teammates. Teammates such as product people, clinicians or support staff ask questions. They get advisory answers with sources, without access to the repository.

## Development

Requires Go 1.27.

```sh
go run .          # start the server on http://localhost:8080
go test ./...     # run all tests
```

On first start, open http://localhost:8080/setup to create the owner account.

| Variable | Default | Meaning |
| --- | --- | --- |
| `REPO_SPEAKS_ADDR` | `:8080` | Address the server listens on |
| `REPO_SPEAKS_DATA_DIR` | `data` | Folder for the database and repository clones |

### Behind a reverse proxy

Repo Speaks speaks plain HTTP. Put a reverse proxy such as Caddy or nginx in front of it for HTTPS.

- Keep the original `Host` header (in nginx: `proxy_set_header Host $host;`). Without it, older browsers fail every form with 403.
- Set `Strict-Transport-Security` at the proxy. The app does not send it, because it cannot know whether HTTPS is used.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it is built.

## License

[MIT](LICENSE)
