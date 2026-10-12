# Architecture

How Repo Speaks is built, at a high level. Terms follow the [glossary](docs/glossary.md).

> **Status:** early development. Boxes and choices marked *open* are not decided yet.

## Quality goals

In priority order. When two of them conflict, the higher one wins.

| # | Quality | Scenario |
| --- | --- | --- |
| 1 | Easy to self-host | A developer with Docker and an LLM API key gets the first answer within 15 minutes, running one container with one data folder. |
| 2 | Project isolation | Nothing from project A — no file, answer or source — ever reaches a member or agent of project B. Every route and every agent tool is covered by a test. |
| 3 | Answers you can check | Every answer lists the files it used. When the agent cannot find something, it says so instead of guessing. |

## System context

```mermaid
flowchart TB
    builder["<b>Builder</b><br/>Person<br/>Manages resources and agents"]
    asker["<b>Asker</b><br/>Person<br/>Asks questions about the code"]
    rs["<b>Repo Speaks</b><br/>Software system<br/>Reads your repositories and answers<br/>questions on behalf of the technical team"]
    llm["<b>LLM API</b><br/>External system<br/>OpenAI-compatible (assumed)"]
    gh["<b>GitHub</b><br/>External system<br/>Access method: open"]

    builder -- "Sends invite links" --> asker
    builder -- "Creates projects, manages resources and agents" --> rs
    asker -- "Asks questions" --> rs
    rs -- "Sends questions and code context" --> llm
    rs -- "Fetches source code" --> gh
```

## Containers

```mermaid
flowchart TB
    builder["<b>Builder</b><br/>Person"]
    asker["<b>Asker</b><br/>Person"]

    subgraph rs ["Repo Speaks"]
        app["<b>App</b><br/>Go, one process<br/>Serves the UI (server-rendered, htmx),<br/>runs agents and sync workers"]
        db[("<b>Database</b><br/>SQLite file in /data<br/>Accounts, projects, members,<br/>agents, jobs, chats")]
        files[("<b>File storage</b><br/>Files on disk under /data<br/>Repository clones")]
    end

    llm["<b>LLM API</b><br/>External system"]
    gh["<b>GitHub</b><br/>External system"]

    builder -- "Manages projects (HTTPS)" --> app
    asker -- "Asks questions, reads streamed answers (HTTPS, SSE)" --> app
    app -- "Reads and writes (SQL)" --> db
    app -- "Clones and reads repositories" --> files
    app -- "Sends questions and code context (HTTPS)" --> llm
    app -- "Fetches source code (HTTPS)" --> gh
```

- **One process.** The web UI, the agents and the sync workers run in one Go binary; sync workers are goroutines.
- **Jobs are database rows**, so a restart does not lose a sync in progress. There is no Redis.
- **SQLite** is the database: a file in `/data`, used from inside the Go process through a pure-Go driver.
- **One data folder** (`/data`) holds the database and the clones (`/data/repos/<project>/<resource>`). One volume to mount, one folder to back up.
- **Clone protection:** the repository size is checked before cloning, only the latest commit is fetched, and files over a per-project limit (default 5 MB) are skipped.

## Main flow: an asker asks a question

```mermaid
sequenceDiagram
    actor Asker
    participant App
    participant DB as Database
    participant Files as File storage
    participant LLM as LLM API

    Asker->>App: Question (and text attachments, if the agent allows them)
    App->>DB: Check membership, agents:view and the agent's rate limit
    App->>LLM: Agent instructions, question, available tools
    loop Until the LLM has enough context (indexing approach: open)
        LLM->>App: Tool call, e.g. search or read a file
        App->>Files: Run it, limited to the agent's enabled resources
        App->>LLM: Tool result
    end
    LLM-->>App: Answer, streamed
    App-->>Asker: Answer streamed (SSE), with its sources
    App->>DB: Save the question, answer and sources
```

The loop shows agentic search. If indexing uses embeddings instead, the App searches the index first and sends the closest pieces in one call; the rest of the flow stays the same.

## Code map

Planned layout. Packages are named by what they provide.

```
main.go              read config, wire packages, start the server
internal/
  web/               HTTP handlers, middleware, templates, static files (embedded)
  auth/              accounts, passwords, sessions, invite links
  permissions/       permissions, presets, Grant
  store/             SQLite access and migrations
  sync/              clone and refresh resources; the job worker loop
  agent/             agent settings, conversations, the tool loop and its tools
  llm/               the connection to the LLM provider
  config/            settings from the environment
```

Dependencies point one way: `web` uses the others; `store` and `llm` use nothing from the app. Everything provider-specific stays in `llm`.

## Known risks

| Risk | Impact | Response |
| --- | --- | --- |
| A confident wrong answer | Askers cannot read code, so they cannot spot it | Sources under every answer; the agent says "not found" instead of guessing; builders can review chats |
| Data leaving its boundary | A file in a repository can carry instructions for the agent (prompt injection); attachments reach the LLM provider; a bug could cross projects | Agent tools are read-only and scoped to the agent's resources; attachments are off by default per agent, with a warning; isolation tests (quality goal 2) |
| Cost running away | Agentic search can make many LLM calls for one question | A rate limit per agent; a step limit per question; usage recorded per project |

## Open decisions

- Indexing: agentic search or embeddings
- LLM provider: one SDK or any OpenAI-compatible endpoint
- GitHub access: personal access token or GitHub App
- Login details
