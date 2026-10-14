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
    llm["<b>LLM API</b><br/>External system<br/>Any OpenAI-compatible host,<br/>such as OpenRouter"]
    gh["<b>GitHub</b><br/>External system<br/>Public repos, or a fine-grained token"]
    bk["<b>Backup storage</b><br/>External system, optional<br/>A mounted folder or S3-compatible storage"]

    builder -- "Sends invite links" --> asker
    builder -- "Creates projects, manages resources and agents" --> rs
    asker -- "Asks questions" --> rs
    rs -- "Sends questions and code context" --> llm
    rs -- "Fetches source code" --> gh
    rs -- "Sends encrypted backups" --> bk
```

## Containers

```mermaid
flowchart TB
    builder["<b>Builder</b><br/>Person"]
    asker["<b>Asker</b><br/>Person"]

    subgraph rs ["Repo Speaks"]
        app["<b>App</b><br/>Go, one process<br/>Serves the UI (server-rendered, htmx),<br/>runs agents and sync workers"]
        db[("<b>Database</b><br/>SQLite file in /data<br/>Accounts, projects, members,<br/>agents, jobs, chats")]
        files[("<b>File storage</b><br/>Files on disk under /data<br/>Repository clones, upgrade copies")]
    end

    llm["<b>LLM API</b><br/>External system"]
    gh["<b>GitHub</b><br/>External system"]
    bk["<b>Backup storage</b><br/>External system, optional"]

    builder -- "Manages projects (HTTPS)" --> app
    asker -- "Asks questions, reads answers (HTTPS)" --> app
    app -- "Reads and writes (SQL)" --> db
    app -- "Clones and reads repositories" --> files
    app -- "Sends questions and code context (HTTPS)" --> llm
    app -- "Fetches source code (HTTPS)" --> gh
    app -- "Writes encrypted backups (file or HTTPS)" --> bk
```

- **One process.** The web UI, the agents and the sync workers run in one Go binary; sync workers and answers are goroutines.
- **Jobs are database rows**, so a restart does not lose a sync in progress. There is no Redis.
- **SQLite** is the database: a file in `/data`, used from inside the Go process through a pure-Go driver.
- **One data folder** (`/data`) holds the database, the clones (`/data/repos/<project>/<resource>`) and the copies made before a schema change (`/data/upgrades`). One volume to mount.
- **Backups are optional and on demand.** The owner presses "Back up now", or cron runs `repo-speaks backup`. A backup is the database only, copied with `VACUUM INTO`, encrypted with a passphrase (OpenPGP), and sent to a mounted folder, S3-compatible storage, or both. Clones are left out; a sync rebuilds them. The targets and the passphrase are environment settings, so the storage keys never sit in the database or in a backup.
- **Upgrades keep a copy.** Before a schema change, the app writes a copy of the database to `/data/upgrades` and keeps the newest 3. Schema changes only go forward, and an older version refuses a newer database.
- **Clone protection:** the repository size is checked before cloning (1 GB at most), only the latest commit is fetched, and files over a per-project limit (default 5 MB) are deleted from the clone.

## Main flow: an asker asks a question

```mermaid
sequenceDiagram
    actor Asker
    participant Web as App: web handlers
    participant Agent as App: agent loop
    participant DB as Database
    participant Files as File storage
    participant LLM as LLM API

    Asker->>Web: Question
    Web->>DB: Check membership and that the agent is ready
    Web->>Web: Check the agent's rate limit (in memory)
    Web->>DB: Save the question and a running answer
    Web->>Agent: Start the answer in the background
    Web-->>Asker: The chat page
    Agent->>Files: Open the clone of each of the agent's repositories (os.Root)
    Agent->>LLM: Base rules, the agent's instructions, file trees and READMEs, the chat so far, the question, the tools
    loop At most 30 tool steps
        LLM->>Agent: Tool call: list files, search text or read a file
        Agent->>Files: Run it inside the clone
        Agent->>DB: Save the step, such as "Reading owner/repo/app.go"
        Agent->>LLM: Tool result
    end
    LLM-->>Agent: The answer, with citations
    Agent->>DB: Save the answer, its model and tokens, and the sources it read
    loop Every 2 seconds while the answer runs
        Asker->>Web: Reload the messages (htmx)
        Web->>DB: Read the steps, then the answer
        Web-->>Asker: The steps, then the answer with its sources
    end
```

- **Polling, not streaming.** The answer runs on the server's context, not the request's. The page asks for the messages every 2 seconds, so a reload loses nothing and no connection stays open.
- **Limits.** One answer may make 30 tool calls and run 5 minutes. At the step limit, the model gets one last call that asks it to answer with what it found. Four answers run at a time; more wait their turn.
- **A restart** stops running answers. On start, the server marks answers still running as failed, so the asker can ask again.
- **Sources** are the files the answer cites, kept only when the agent read them during the answer. Each stores the commit the clone had at question start, and links to GitHub at that commit.
- **No index.** The agent searches the clone with its tools. It gets the file tree and the README up front.

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

## Backup flow: the owner presses "Back up now"

```mermaid
sequenceDiagram
    actor Owner
    participant Web as App: web handlers
    participant Worker as App: jobs worker
    participant DB as Database
    participant Data as Data folder
    participant Target as Backup storage

    Owner->>Web: Press "Back up now"
    Web->>DB: Check the owner, queue a backup job (one open at most)
    Web-->>Owner: "Backup queued"; the list reloads every 5 seconds
    Worker->>DB: Claim the job
    Worker->>DB: VACUUM INTO a temporary file in the data folder (0600)
    Worker->>Data: Encrypt it into a second temporary file, then remove the plain copy
    loop Each target
        Worker->>Target: Folder: write a temporary name, then rename. S3: upload
    end
    Worker->>Data: Remove the encrypted temporary file
    Worker->>DB: Record the backup: name, size, targets, error. Mark the job done or failed
```

- **The command does the same.** `repo-speaks backup` runs the same steps without a job, while the server runs, so cron can call it.
- **A failed target does not stop the others.** The record names each failed one. A failed backup is not tried again; the owner sees the error and can press the button again.
- **Errors never show a secret.** The passphrase and the S3 keys are removed from every error before it is saved or logged.
- **Restore runs with the server stopped.** `repo-speaks restore <file>` refuses while the database is open. It decrypts the backup next to the database, runs the integrity check, refuses a newer schema, then moves the current database aside and puts the backup in place.

## Entity relationships

Tables for phase 1. `attachments` is planned and not built yet.

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
    projects ||--o{ models : "has"
    models |o--o| projects : "default of"
    projects ||--o{ agents : "has"
    models |o--o{ agents : "used by"
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
        int default_model_id FK "null = none; restrict"
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
    models {
        int id PK
        int project_id FK "cascade"
        text name "unique per project, no case"
        text base_url "OpenAI-compatible API, no trailing slash"
        text api_key "plain text, write-only; only the llm calls read it"
        text api_key_hint "last 4 characters"
        text model_id "the host's ID, checked on save"
        time created_at
        time updated_at
    }
    agents {
        int id PK
        int project_id FK "cascade"
        text kind "chat or answer; fixed at creation"
        text name "unique per project, no case"
        text description "every member sees it"
        text prompt "agents:edit only; holds the answer format"
        bool all_resources "default true; false = agent_resources"
        int model_id FK "null = project default; no action"
        int rate_limit_per_hour "per member; default 20, 1 to 1000"
        bool attachments_on "comes with attachments"
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
        text content "an answer is Markdown"
        text status "answers only: running, done, failed"
        text error "why an answer failed"
        text progress "one line per step"
        text model_id "the host's model ID that answered"
        int prompt_tokens
        int completion_tokens
        int steps "tool calls, 30 at most"
        time created_at
        time finished_at
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
        int resource_id FK "cascade"
        text path "file in the resource"
        int line_from
        int line_to
        text commit_id "the clone's commit at question start"
    }
    jobs {
        int id PK
        text kind "sync_resource, backup"
        int resource_id FK "cascade; null for a backup"
        text status "queued, running, done, failed"
        int attempts "3 at most"
        time run_after
        time locked_at "claimed with UPDATE ... RETURNING"
        text error
        time created_at "done and failed rows go after 7 days"
    }
    backups {
        int id PK
        text name "the encrypted file's name"
        int size "bytes; 0 when no target got it"
        text targets "folder and S3 without secrets"
        text error "empty when every target got it"
        time started_at
        time finished_at
    }
```

- IDs are integers. Times are Unix seconds. Tables are SQLite `STRICT`.
- Deleting a project deletes everything in it.
- Chats stay when a member leaves, or an agent or account is deleted. Each project chooses how long chats live: forever (default), 30, 90 or 365 days.
- An answer agent's chat holds exactly one question and one answer. The app refuses a second question.
- Session IDs and link tokens are stored only as SHA-256 hashes.
- `member_permissions` references `permissions`, so the database rejects unknown names.
- A resource is unique per project, repository and branch; the default branch counts as one branch. A partial unique index allows one open (queued or running) job per resource. A second one allows one open job per kind among jobs without a resource, so there is one open backup at most.
- `backups` stands alone: a record of each backup this server made, for the Backups page. The backup settings are not in the database.
- A model's name is unique per project. The same model ID may appear twice, with other hosts or keys. Changing a model's base URL clears its key in the same update.
- The project's default model cannot be deleted. The app refuses first; the foreign key is `ON DELETE RESTRICT` in case a code path forgets.
- An agent may pick a model of its own; otherwise it uses the project's default. With neither, it is "not ready". A model an agent uses cannot be deleted either. The app refuses first and names the agents. The foreign key is `NO ACTION`, which SQLite checks at the end of the statement, so deleting a project still deletes its agents and models in any order.
- An agent with `all_resources` off reads only the resources in `agent_resources`. The app adds only resources of the agent's own project. A deleted resource drops out by cascade.
- The rate limit is stored on the agent. The app counts questions per member in memory, so a restart resets the counts.
- Only answers have a status. A chat has at most one running answer: a question waits until the last answer is done or failed.
- A source keeps the commit the answer read, so its link never shifts after a sync. Deleting the resource deletes its sources; the answer keeps its text.

## Components of the App

Zooming into the App container. Each component is one Go package under `internal/`.

```mermaid
flowchart TB
    builder["<b>Builder</b><br/>Person"]
    asker["<b>Asker</b><br/>Person"]

    subgraph app ["App (Go, one process)"]
        web["<b>web</b><br/>Pages and forms; polls running answers.<br/>Security headers, sessions"]
        auth["<b>auth</b><br/>Passwords, session tokens,<br/>invite links, login throttle"]
        perms["<b>permissions</b><br/>Permissions, presets,<br/>grant rules"]
        agent["<b>agent</b><br/>The answer loop, read-only tools,<br/>sources, rate limit"]
        llm["<b>llm</b><br/>Calls OpenAI-compatible hosts.<br/>Blocks private addresses"]
        jobs["<b>jobs</b><br/>Claims job rows, runs timers,<br/>hands each job on"]
        sync["<b>sync</b><br/>Clones and refreshes<br/>one resource"]
        backup["<b>backup</b><br/>Copies, encrypts and sends<br/>the database; restores it"]
        store["<b>store</b><br/>SQL access, migrations,<br/>copies before upgrades"]
    end

    db[("<b>Database</b><br/>SQLite file")]
    files[("<b>File storage</b><br/>Repository clones")]
    llmapi["<b>LLM API</b><br/>External system"]
    gh["<b>GitHub</b><br/>External system"]
    bk["<b>Backup storage</b><br/>External system, optional"]

    builder -- "Manages projects (HTTPS)" --> web
    asker -- "Asks questions (HTTPS)" --> web
    web -- "Checks logins" --> auth
    web -- "Checks what a member may do" --> perms
    web -- "Sends questions" --> agent
    web -- "Reads and writes data, queues jobs" --> store
    web -- "Checks and tests models" --> llm
    agent -- "Sends prompts and tool results" --> llm
    agent -- "Loads agents, saves answers and steps" --> store
    agent -- "Reads files of the agent's resources" --> files
    jobs -- "Claims jobs, queues timed ones" --> store
    jobs -- "Runs sync jobs" --> sync
    jobs -- "Runs backup jobs" --> backup
    backup -- "Copies the database, records backups" --> store
    backup -- "Encrypted files" --> bk
    sync -- "Saves sync results" --> store
    sync -- "Fetches code (HTTPS)" --> gh
    sync -- "Writes clones" --> files
    store -- "SQL" --> db
    llm -- "HTTPS" --> llmapi
```

- **Three entry points.** People come in through `web`. Timers and queued work run in `jobs`. Answers run in `agent`, in the background. `main.go` starts all three.
- **`web` never calls `sync`.** It queues a job row, so the database is the only link between them. A restart loses no work.
- **Only `store` knows SQL, and only `llm` talks to LLM hosts.** Neither uses anything else from the app.
- **`llm` speaks one API:** OpenAI-compatible chat completions with tools, so any host that speaks it works. It lists a host's models, checks a model ID on save, sends the Test request, and runs the calls of each answer.
- **`llm` guards the server.** A project admin sets a model's base URL, so the client refuses loopback, private, link-local, multicast and carrier-grade NAT addresses unless the operator allows them. The check runs in the dialer, on the address dialed, so a DNS change cannot get around it. It follows no redirects, ignores proxy settings, and caps time (30 seconds, or 2 minutes for a call of an answer) and reply sizes.
- **Where trust changes:** file content from a repository reaches the LLM. A file can carry instructions for the agent (prompt injection), so the agent's tools only read, and only inside the agent's resources. Every file access goes through an `os.Root` on the clone, so a symlink or a `..` cannot leave it. The model's answer is untrusted too: it renders without raw HTML, and its links show as text.
- **`agent` takes one thing from `sync`:** `ClonePath`, where a clone lives.
- **`backup` never sees a page.** The web handler queues a backup job and reads the records, like a sync. The page shows the targets without secrets.

## Code map

```
main.go              read config, wire packages, start the web server, the jobs loop and the agents;
                     the reset-link, backup and restore commands
internal/
  web/               HTTP handlers, middleware, templates, static files (embedded)
  auth/              passwords, sessions, invite links, login throttle
  permissions/       permissions, presets, Grant
  store/             SQLite access and migrations
  jobs/              the job loop and timers; hands each job to sync
  sync/              clone and refresh one resource
  backup/            encrypted backups to a folder or S3, and restore
  agent/             the answer loop, its read-only tools, sources, rate limit
  llm/               OpenAI-compatible client, base URL check, private address guard
  config/            settings from the environment
```

Built so far: every package above. Chat retention and attachments come later. Dependencies point one way: `web` and `jobs` use the others; `agent` uses `store`, `llm` and the clone path of `sync`; `store` and `llm` use nothing from the app. `jobs` takes the syncer and the backup service as small interfaces, and `main.go` passes it the real ones. `backup` uses `store` and `config`.

## Known risks

| Risk | Impact | Response |
| --- | --- | --- |
| A confident wrong answer | Askers cannot read code, so they cannot spot it | Sources under every answer; the agent says "not found" instead of guessing; managers can review chats (`chats:view`) |
| Data leaving its boundary | A file in a repository can carry instructions for the agent (prompt injection); attachments reach the LLM host; a bug could cross projects | Agent tools are read-only and scoped to the agent's resources; every file access goes through `os.Root`, so links cannot leave the clone; attachments are off by default per agent, with a warning; isolation tests (quality goal 2) |
| Prompt injection in the answer | A file tells the model to write a link that sends chat text to another site, or to show its instructions | The base rules say to ignore instructions in files and not to repeat the instructions; answers render without raw HTML, links show as text and images never load; only the app builds clickable source links. The model can still be misled, so the rules lower the risk but do not remove it |
| Cost running away | Agentic search can make many LLM calls for one question | A rate limit per member on each agent; 30 steps and 5 minutes per question; each answer records its model and tokens (totals come later) |
| A hostile repository | A repository or a name could try to read server files, reach other hosts, fill the disk or hang the worker | Checked names and an app-built URL; git with a fixed environment, https only, symlinks as plain files, no submodules; a size check before cloning; a timeout that kills git's process group |
| Tokens and keys at rest | A copy of the data folder holds every GitHub token and LLM API key in plain text, and so do the copies made before upgrades | Backups are encrypted with a passphrase (OpenPGP, AES-256), and the storage keys stay in the environment, out of every backup; tokens and keys are write-only in the UI and kept out of URLs, arguments, logs and errors; fine-grained tokens limit each to one repository, read-only; a model's key is cleared when its URL changes; the data folder and every copy are readable by the app only (0600) |
| Losing data | A disk dies, or an upgrade goes wrong | Optional encrypted backups off the disk, by button or by cron, to a folder or S3; a copy before each schema change, the newest 3 kept; an older version refuses a newer database; restore keeps the old database aside. A lost passphrase means lost backups, and the docs say so |
| A model URL that points inside | A project admin could make the server call internal systems or the cloud metadata address, and read the replies in an error | Private addresses are refused at connect time unless the operator sets `REPO_SPEAKS_ALLOW_PRIVATE_LLM`; no redirects, no proxy; errors show the status and at most 200 characters of the host's message |

## Open decisions

- How the owner resets their own password
