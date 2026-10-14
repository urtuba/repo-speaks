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
| `REPO_SPEAKS_ALLOW_PRIVATE_LLM` | off | `true` lets models use hosts on private addresses, such as a local Ollama. See [Local models](#local-models-and-private-addresses) |

### Forgotten passwords

The owner makes reset links for other accounts on the Accounts page. If the owner forgets their own password, run this on the server, with the same `REPO_SPEAKS_DATA_DIR`:

```sh
repo-speaks reset-link <username>   # in development: go run . reset-link <username>
```

It prints a path such as `/reset/...`. Put the server's address in front of it and open it in a browser. The link works once, stops after 24 hours, and ends every session of the account. The server can keep running.

### Adding repositories

A builder adds a GitHub repository on the project page, as `owner/repo` or its GitHub address, with an optional branch. The server clones the latest commit when the repository is added, when someone presses "Sync now", and on the project's timer (every 6 hours by default). Repositories over 1 GB are refused, files over the project's limit (5 MB by default) are left out, and submodules are not cloned. Behind a proxy, git uses the server's `HTTPS_PROXY` and `NO_PROXY` settings.

A private repository needs a token. On GitHub, go to Settings, Developer settings, Personal access tokens, Fine-grained tokens, and generate a new token. Pick the repository's owner, choose "Only select repositories" with that one repository, and set the repository permission "Contents" to "Read-only". Paste the token into the Token field. Repo Speaks never shows it again and sends it only to GitHub, but it stores it in plain text: anyone with a copy of the data folder or a backup has it. Keep the data folder readable by the app's user only.

### Adding a model

Agents need an LLM with tool use. Any host with an OpenAI-compatible API works. OpenRouter is the default, because one key reaches many models.

1. On openrouter.ai, create an API key. A credit limit on the key caps what the server can spend.
2. In Repo Speaks, open the project's Settings. Admins (`project:edit`) see the Models section.
3. Enter a name, such as "Fast", and the model ID as OpenRouter shows it, such as `z-ai/glm-5.3-flash`. Keep the base URL `https://openrouter.ai/api/v1`, and paste the key.
4. Press Add model, then Test.

Saving asks the host for its model list and refuses an unknown model ID. OpenRouter also says which models support tool use, so models without it are refused. Other hosts do not say: there, the Test button is the check. It sends one short request with one tool and passes when the model calls the tool.

Each model has its own key. Repo Speaks never shows it again, only its last 4 characters. Changing a model's base URL clears its key, so a key never goes to a host it was not entered for. Keys are stored in plain text, like GitHub tokens: anyone with a copy of the data folder or a backup has them.

Pick the project's default model under the list. The default model cannot be deleted; choose another one first.

### Creating an agent

An agent answers questions about a project's repositories. Builders (`agents:edit`) press New agent on the project page.

- **Kind:** a chat agent holds conversations with follow-up questions. An answer agent takes one question and gives one answer in a set format. The kind cannot change later; to switch, create another agent.
- **Instructions:** what the agent does and how it answers, up to 20000 characters. For an answer agent, describe the format and give an example answer. Only members with `agents:edit` see the instructions.
- **Description:** up to 300 characters. Every member sees it.
- **Model:** the project's default model, or one of the project's models.
- **Repositories:** all of the project's repositories, ones added later too, or only the ones you tick. A repository removed from the project drops out of every agent.
- **Rate limit:** how many questions one member may ask the agent per hour, from 1 to 1000. The default is 20. Chat is not built yet, so nothing counts questions so far. The chat will count them in memory, and a restart will reset the counts.

An agent with no model of its own, in a project with no default model, is "not ready". It can be saved, but it cannot answer. Every member sees "not ready" next to it. An admin fixes it by adding a model or setting the default model.

A model an agent uses cannot be deleted. Give the agent another model first.

### Local models and private addresses

By default, the server refuses to call LLM hosts on private addresses: loopback (`localhost`), private networks, link-local addresses and carrier-grade NAT. A project admin sets a model's base URL, and may not run the server. Without this check, the URL could point at internal systems or at the cloud metadata address `169.254.169.254`, and the server would call them. The check runs each time the server connects, so a DNS change cannot get around it.

To use a model on your own network, such as Ollama, set `REPO_SPEAKS_ALLOW_PRIVATE_LLM=true` and restart. Then use a base URL such as `http://localhost:11434/v1`. Do this only when you trust every project admin. There is no switch in the UI.

The LLM client ignores `HTTPS_PROXY` and `NO_PROXY`. It connects to the host itself, so the private address check sees the real address. A server that can reach the internet only through a proxy cannot reach a model host.

### Behind a reverse proxy

Repo Speaks speaks plain HTTP. Put a reverse proxy such as Caddy or nginx in front of it for HTTPS.

- Keep the original `Host` header (in nginx: `proxy_set_header Host $host;`). Without it, older browsers fail every form with 403.
- Set `Strict-Transport-Security` at the proxy. The app does not send it, because it cannot know whether HTTPS is used.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it is built.

## License

[MIT](LICENSE)
