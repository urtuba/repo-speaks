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
    gh["<b>GitHub</b><br/>External system<br/>Public repos, or a fine-grained token"]

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
- **Clone protection:** the repository size is checked before cloning (1 GB at most), only the latest commit is fetched, and files over a per-project limit (default 5 MB) are deleted from the clone.

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
    Worker->>DB: Claim the oldest due job (UPDATE ... RETURNING)
    Worker->>GH: Ask the API for the repository size
    alt Over 1 GB, or not found
        Worker->>DB: Mark the job failed, save the reason on the resource
    else Size is fine
        Worker->>GH: git clone of the latest commit only
        GH-->>Worker: Files
        Worker->>Files: Write into a new folder, then delete .git and files over the file limit
        Note over Worker,Files: Indexing (open) runs here
        Worker->>DB: Point the resource at the new folder, save the commit and the sync time
        Worker->>Files: Delete older folders, but keep the previous one
        Worker->>DB: Mark the job done
    end
    opt Network or GitHub error
        Worker->>DB: Queue a retry after 1, then 4 minutes. After 3 tries, mark it failed and save the error on the resource
    end
```

- **One open job per resource.** Pressing "Sync now" twice does not start two syncs.
- **A new folder for every sync.** The resource points at a folder only once it is complete, so an agent reads either the old clone or the new one, never half of each. The previous folder stays until the next sync, because a question may still read it. A sync needs room for three clones at most.
- **The timer skips resources tried within the interval**, so a failing repository is not tried again every few minutes.
- **git runs boxed in.** The app builds the clone URL from checked names; git gets a fixed environment, speaks only https, writes symlinks as plain files, skips submodules, and is killed with its helpers after 10 minutes. The token reaches git only as an HTTP header through the environment.
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
        int max_file_bytes "default 5 MB; 1 to 100 MB"
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
        int id PK
        int user_id FK "cascade"
        text token_hash UK "sha-256 of the link token"
        int created_by FK "the owner"
        time expires_at "24 hours"
        time used_at
    }
    resources {
        int id PK
        int project_id FK "cascade"
        text kind "github_repo; markdown later"
        text owner "no case"
        text repo "no case"
        text branch "null = default branch"
        text token "plain text, write-only; only the sync reads it"
        text token_hint "last 4 characters"
        text clone_dir "current folder in repos/project/resource"
        text last_commit "full commit ID"
        time last_synced_at
        text last_error
        time created_at
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
        int attempts "3 at most"
        time run_after
        time locked_at "claimed with UPDATE ... RETURNING"
        text error
        time created_at "done and failed rows go after 7 days"
    }
```

- IDs are integers. Times are Unix seconds. Tables are SQLite `STRICT`.
- Deleting a project deletes everything in it.
- Chats stay when a member leaves, or an agent or account is deleted. Each project chooses how long chats live: forever (default), 30, 90 or 365 days.
- An answer agent's chat holds exactly one question and one answer. The app refuses a second question.
- Session IDs and link tokens are stored only as SHA-256 hashes.
- `member_permissions` references `permissions`, so the database rejects unknown names.
- A resource is unique per project, repository and branch; the default branch counts as one branch. A partial unique index allows one open (queued or running) job per resource.

## Components of the App

Zooming into the App container. Each component is one Go package under `internal/`.

```mermaid
flowchart TB
    builder["<b>Builder</b><br/>Person"]
    asker["<b>Asker</b><br/>Person"]

    subgraph app ["App (Go, one process)"]
        web["<b>web</b><br/>Pages, forms and streamed answers.<br/>Security headers, sessions"]
        auth["<b>auth</b><br/>Passwords, session tokens,<br/>invite links, login throttle"]
        perms["<b>permissions</b><br/>Permissions, presets,<br/>grant rules"]
        agent["<b>agent</b><br/>Chats, the tool loop,<br/>read-only tools, chat clean-up"]
        llm["<b>llm</b><br/>Talks to the LLM provider<br/>(provider: open)"]
        jobs["<b>jobs</b><br/>Claims job rows, runs timers,<br/>hands each job on"]
        sync["<b>sync</b><br/>Clones and refreshes<br/>one resource"]
        store["<b>store</b><br/>SQL access and migrations"]
    end

    db[("<b>Database</b><br/>SQLite file")]
    files[("<b>File storage</b><br/>Repository clones")]
    llmapi["<b>LLM API</b><br/>External system"]
    gh["<b>GitHub</b><br/>External system"]

    builder -- "Manages projects (HTTPS)" --> web
    asker -- "Asks questions (HTTPS, SSE)" --> web
    web -- "Checks logins" --> auth
    web -- "Checks what a member may do" --> perms
    web -- "Sends questions, streams answers" --> agent
    web -- "Reads and writes data, queues jobs" --> store
    agent -- "Sends prompts and tool results" --> llm
    agent -- "Loads agents, saves messages" --> store
    agent -- "Reads files of the agent's resources" --> files
    jobs -- "Claims jobs, queues timed ones" --> store
    jobs -- "Runs sync jobs" --> sync
    jobs -- "Runs chat clean-up" --> agent
    sync -- "Saves sync results" --> store
    sync -- "Fetches code (HTTPS)" --> gh
    sync -- "Writes clones" --> files
    store -- "SQL" --> db
    llm -- "HTTPS" --> llmapi
```

- **Two entry points.** People come in through `web`. Timers and queued work run in `jobs`. `main.go` starts both.
- **`web` never calls `sync`.** It queues a job row, so the database is the only link between them. A restart loses no work.
- **Only `store` knows SQL, and only `llm` knows the provider.** Neither uses anything else from the app.
- **Where trust changes:** file content from a repository reaches the LLM. A file can carry instructions for the agent (prompt injection), so the agent's tools only read, and only inside the agent's resources.
- Indexing (open) would be one more component between `sync` and `agent`.

## Code map

```
main.go              read config, wire packages, start the web server and the jobs loop
internal/
  web/               HTTP handlers, middleware, templates, static files (embedded)
  auth/              passwords, sessions, invite links, login throttle
  permissions/       permissions, presets, Grant
  store/             SQLite access and migrations
  jobs/              the job loop and timers; hands each job to sync or agent
  sync/              clone and refresh one resource
  agent/             agent settings, chats, the tool loop and its tools, chat clean-up
  llm/               the connection to the LLM provider
  config/            settings from the environment
```

Built so far: `web`, `auth`, `permissions`, `store`, `config`, `jobs`, `sync`. Dependencies point one way: `web` and `jobs` use the others; `store` and `llm` use nothing from the app. `jobs` takes the syncer as a small interface, and `main.go` passes it the real one.

## Known risks

| Risk | Impact | Response |
| --- | --- | --- |
| A confident wrong answer | Askers cannot read code, so they cannot spot it | Sources under every answer; the agent says "not found" instead of guessing; managers can review chats (`chats:view`) |
| Data leaving its boundary | A file in a repository can carry instructions for the agent (prompt injection); attachments reach the LLM provider; a bug could cross projects | Agent tools are read-only and scoped to the agent's resources; attachments are off by default per agent, with a warning; isolation tests (quality goal 2) |
| Cost running away | Agentic search can make many LLM calls for one question | A rate limit per agent; a step limit per question; usage recorded per project |
| A hostile repository | A repository or a name could try to read server files, reach other hosts, fill the disk or hang the worker | Checked names and an app-built URL; git with a fixed environment, https only, symlinks as plain files, no submodules; a size check before cloning; a timeout that kills git's process group |
| Tokens at rest | A copy of the data folder or a backup holds every GitHub token in plain text | Tokens are write-only in the UI and kept out of URLs, arguments, logs and saved errors; fine-grained tokens limit each to one repository, read-only; the data folder must be readable by the app only |

## Open decisions

- Indexing: agentic search or embeddings
- LLM provider: one SDK or any OpenAI-compatible endpoint
- How the owner resets their own password
