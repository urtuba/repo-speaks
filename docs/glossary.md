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
| Model | An LLM a project's agents can use: a name, the base URL of an OpenAI-compatible host, an API key and the host's model ID, such as `z-ai/glm-5.3-flash`. Once saved, the key is never shown; pages show its last 4 characters. |
| Default model | The model a project's agents use unless an agent picks another. Set with `project:edit`. It cannot be deleted while it is the default. |
| Job | A piece of background work, such as a sync, kept as a row in the database: queued, running, done or failed. |
| Agent | Instructions, the resources it reads, an optional model and a rate limit, inside one project. A chat agent or an answer agent. Every member sees its name, kind and description; only `agents:edit` sees its instructions. |
| Chat agent | An agent for conversations: many questions and answers in one chat. |
| Answer agent | An agent that takes one question and gives one answer, in the format its instructions set. No follow-ups. |
| Not ready | An agent with no model of its own, in a project with no default model. It can be saved but cannot answer. |
| Rate limit | How many questions one member may ask one agent per hour. Set per agent, 20 by default. |
| Chat | A conversation between one member and one agent. With an answer agent, it holds one question and one answer. The member who started it can always read it; members with `chats:view` can read every chat. |
| Answer | An agent's reply to one question: text with its sources. It runs in the background and is running, done or failed. |
| Step | One tool call an agent makes while it answers, such as reading a file. One answer may take 30 steps. |
| Source | A file listed under an answer, with its lines, so a reader can check it. It links to GitHub at the commit the agent read. |

## Words to avoid

| Avoid | Use |
| --- | --- |
| User (for a role) | Asker, builder, manager or admin. "User" alone is ambiguous. |
| Source (for GitHub access) | Connection |
| Owner (for a project role) | Admin |
| Citation | Source |
| Q&A agent | Answer agent |
| Provider | Host, for the service a model runs on, such as OpenRouter; model, for the record in a project |
