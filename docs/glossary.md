# Glossary

One meaning per word. Code, UI and docs use these terms.

| Term | Meaning |
| --- | --- |
| Instance | One deployment of Repo Speaks, such as `repospeaks.example.com`. |
| Owner | The person who set up the instance. Has every permission in every project. Used only for the instance. |
| Account | A person's login on the instance. One account can have many memberships. |
| Project | Holds members, resources and agents. |
| Member | An account added to a project, with a permission set. |
| Permission | One thing a member may do in a project, written `<area>:view` or `<area>:edit`, such as `agents:edit`. |
| Preset | A named starting permission set: asker, builder, manager or admin. |
| Asker | A member who views and asks questions, and changes nothing. |
| Builder | A member who manages resources and agents. |
| Manager | A builder who can also invite members (`members:edit`) and read every chat (`chats:view`). |
| Admin | A manager who can also edit the project (`project:edit`). |
| Invite | A one-time link that adds a person to a project with a preset. |
| Connection | Repo Speaks' access to GitHub. |
| Resource | One repository or markdown folder added to a project through a connection. |
| Sync | Bringing a resource up to date with GitHub. |
| Clone | The copy of a repository's latest commit on the server. Agents read the clone, never GitHub itself. |
| Token | A GitHub fine-grained personal access token that lets Repo Speaks read one private repository. Once saved, it is never shown; pages show its last 4 characters. |
| Job | A piece of background work, such as a sync, kept as a row in the database: queued, running, done or failed. |
| Agent | Instructions, enabled resources and a rate limit, inside one project. A chat agent or an answer agent. |
| Chat agent | An agent for conversations: many questions and answers in one chat. |
| Answer agent | An agent that takes one question and gives one answer, in the format its instructions set. No follow-ups. |
| Chat | A conversation between one member and one agent. With an answer agent, it holds one question and one answer. The member who started it can always read it; members with `chats:view` can read every chat. |
| Source | A file or document listed under an answer, so a reader can check it. |

## Words to avoid

| Avoid | Use |
| --- | --- |
| User (for a role) | Asker, builder, manager or admin. "User" alone is ambiguous. |
| Source (for GitHub access) | Connection |
| Owner (for a project role) | Admin |
| Citation | Source |
| Q&A agent | Answer agent |
