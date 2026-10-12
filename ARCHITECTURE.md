# Architecture

How Repo Speaks is built, at a high level. Terms follow the [glossary](docs/glossary.md).

> **Status:** early development. Boxes and choices marked *open* are not decided yet.

## Quality goals

In priority order. When two of them conflict, the higher one wins.

| # | Quality | Scenario |
| --- | --- | --- |
| 1 | Easy to self-host | A developer with Docker and an LLM API key gets the first answer within 15 minutes, running one container with one data folder. |
| 2 | Project isolation | Nothing from project A (no file, answer or source) ever reaches a member or agent of project B. Every route and every agent tool is covered by a test. |
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
    App->>DB: Check membership and the agent's rate limit
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

## Sync flow: a repository is brought up to date

A sync starts in three ways: a builder adds a repository, a builder presses "Sync now", or the project's timer fires (every 6 hours by default; off, 1, 6 or 24 hours). All three add the same job row.

```mermaid
sequenceDiagram
    actor Builder
    participant Web as App: web handlers
    participant Worker as App: sync worker
    participant DB as Database
    participant GH as GitHub
    participant Files as File storage

    Builder->>Web: Add a repository, or press "Sync now"
    Web->>DB: Check resources:edit, save the resource, queue a sync job
    Web-->>Builder: "Sync queued"
    Note over Worker,DB: The timer queues the same job when the last sync is older than the interval
    Worker->>DB: Claim the oldest queued job (UPDATE ... RETURNING)
    Worker->>GH: Ask for the repository size (access method: open)
    alt Over the size limit
        Worker->>DB: Mark the job failed, save the reason
    else Size is fine
        Worker->>GH: Fetch only the latest commit
        GH-->>Worker: Files
        Worker->>Files: Write into a new folder, skip files over the file limit
        Worker->>Files: Rename the new folder over the old one
        Note over Worker,Files: Indexing (open) runs here
        Worker->>DB: Save the sync time, mark the job done
    end
    opt Network or GitHub error
        Worker->>DB: Queue a retry with a longer wait. After 3 tries, mark it failed with the error
    end
```

- **One open job per resource.** Pressing "Sync now" twice does not start two syncs.
- **The rename** means an agent reads either the old clone or the new one, never half of each.
- **The web handler only queues.** A slow clone never blocks a page.

## Invite flow: a new person joins a project

```mermaid
sequenceDiagram
    actor Manager
    actor Invitee
    participant App
    participant DB as Database

    Manager->>App: Create an invite: project and preset
    App->>DB: Check members:edit and that the manager holds the whole preset
    App->>DB: Save the token's hash, the preset and the expiry (7 days)
    App-->>Manager: The link, shown once
    Manager-->>Invitee: Sends the link (chat, message, in person)
    Invitee->>App: Opens the link
    App->>DB: Find the invite by hash: unused, not expired, not revoked?
    App->>DB: Does the inviter still hold the whole preset?
    alt Any check fails
        App-->>Invitee: "This link no longer works. Ask for a new one."
    else The link is valid
        alt New person
            App-->>Invitee: Form: pick a username and a password
            Invitee->>App: Username and password
        else Has an account already
            App-->>Invitee: Log in first
            Invitee->>App: Logs in and confirms
        end
        App->>DB: One transaction: create the account if new, add the membership, mark the invite used
        App-->>Invitee: Logged in, the project opens
    end
```

- **Only the hash is stored.** A copy of the database holds no working links.
- **Checked again on use.** A manager who lost their rights, or left, has no working links.
- **"Mark used" only succeeds once** (`UPDATE ... WHERE used_at IS NULL`). Two people opening the same link at once cannot both join.
- An invitee who is already a member sees "You are already a member", and the link stays unused.

## Entity relationships

Planned tables for phase 1. Indexing tables come with the indexing decision.

```mermaid
erDiagram
    users ||--o{ sessions : "logs in with"
    users ||--o{ memberships : "has"
    projects ||--o{ memberships : "has"
    memberships ||--o{ member_permissions : "holds"
    projects ||--o{ invites : "has"
    users ||--o{ invites : "creates"
    users ||--o{ password_resets : "gets"
    projects ||--o{ resources : "has"
    projects ||--o{ agents : "has"
    agents ||--o{ agent_resources : "picks"
    resources ||--o{ agent_resources : "picked by"
    permissions ||--o{ member_permissions : "names"
    projects ||--o{ chats : "has"
    agents ||--o{ chats : "answers in"
    users ||--o{ chats : "starts"
    chats ||--o{ messages : "contains"
    messages ||--o{ attachments : "has"
    messages ||--o{ message_sources : "cites"
    resources ||--o{ jobs : "synced by"

    users {
        int id PK
        text username UK "3-32 chars"
        text password_hash "argon2id, PHC string"
        bool is_owner "at most one row true"
        time created_at
    }
    sessions {
        blob id_hash PK "sha-256 of cookie value"
        int user_id FK "cascade"
        time expires_at "24h idle, moved forward"
        time created_at
    }
    projects {
        int id PK
        text name
        int attachment_max_bytes "default 262144, max 1048576"
        int chat_retention_days "null = forever; 30, 90, 365"
        int sync_interval_hours "null = off; 1, 6 (default), 24"
        time created_at
    }
    memberships {
        int project_id PK,FK "cascade"
        int user_id PK,FK "cascade"
        int invited_by FK "null for owner-made"
        time created_at
    }
    member_permissions {
        int project_id PK,FK "cascade with membership"
        int user_id PK,FK
        text permission PK,FK "must exist in permissions"
    }
    permissions {
        text name PK "filled by migrations"
    }
    agent_resources {
        int agent_id PK,FK "cascade"
        int resource_id PK,FK "cascade"
    }
    invites {
        int id PK
        int project_id FK "cascade"
        blob token_hash UK
        text preset "asker, builder, manager, admin"
        int created_by FK
        time expires_at "7 days"
        time used_at "null until used"
        int used_by FK
        time revoked_at
    }
    password_resets {
        blob token_hash PK
        int user_id FK "cascade"
        time expires_at
        time used_at
    }
    resources {
        int id PK
        int project_id FK "cascade"
        text kind "github_repo, markdown"
        text location "repo URL or path"
        text branch
        time last_synced_at
        text last_error
    }
    agents {
        int id PK
        int project_id FK "cascade"
        text kind "chat or answer; fixed at creation"
        text name
        text description "every member sees it"
        text prompt "agents:edit only; holds the answer format"
        bool attachments_on "default false"
        bool all_resources "default true; false = agent_resources"
        time created_at
        time updated_at
    }
    chats {
        int id PK
        int project_id FK "cascade"
        int agent_id FK "set null: deleted agent"
        int user_id FK "set null: deleted account"
        text title
        time created_at
    }
    messages {
        int id PK
        int chat_id FK "cascade"
        text role "user, assistant"
        text content
        time created_at
    }
    attachments {
        int id PK
        int message_id FK "cascade"
        text filename
        int size_bytes
        text content "valid UTF-8 only"
    }
    message_sources {
        int message_id PK,FK "cascade"
        int position PK
        int resource_id FK
        text path "file in the resource"
        int line_from
        int line_to
    }
    jobs {
        int id PK
        text kind "sync_resource"
        int resource_id FK "cascade; null for project jobs"
        text status "queued, running, done, failed"
        int attempts
        time run_after
        time locked_at "claimed with UPDATE ... RETURNING"
        text error
    }
```

- IDs are integers. Times are Unix seconds. Tables are SQLite `STRICT`.
- Deleting a project deletes everything in it.
- Chats stay when a member leaves, or an agent or account is deleted. Each project chooses how long chats live: forever (default), 30, 90 or 365 days.
- An answer agent's chat holds exactly one question and one answer. The app refuses a second question.
- Session IDs and link tokens are stored only as SHA-256 hashes.
- `member_permissions` references `permissions`, so the database rejects unknown names.

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
| A confident wrong answer | Askers cannot read code, so they cannot spot it | Sources under every answer; the agent says "not found" instead of guessing; managers can review chats (`chats:view`) |
| Data leaving its boundary | A file in a repository can carry instructions for the agent (prompt injection); attachments reach the LLM provider; a bug could cross projects | Agent tools are read-only and scoped to the agent's resources; attachments are off by default per agent, with a warning; isolation tests (quality goal 2) |
| Cost running away | Agentic search can make many LLM calls for one question | A rate limit per agent; a step limit per question; usage recorded per project |

## Open decisions

- Indexing: agentic search or embeddings
- LLM provider: one SDK or any OpenAI-compatible endpoint
- GitHub access: personal access token or GitHub App
- How the owner resets their own password
