# Contributing

## Commit messages

Repo Speaks follows the Go project's commit style.

- **First line:** `<area>: <summary>`. The area is the package or folder the change mainly touches (`permissions`, `sync`, `docs`), or `all` for changes across the tree.
- **Summary:** lowercase, no full stop, about 70 characters at most. It completes the sentence "This change modifies Repo Speaks to ...".
- **Body:** after a blank line, explain why, in full sentences, wrapped at about 72 characters. No Markdown.
- **Issues:** end with `Fixes #12` or `Updates #12`.

```
permissions: reject grants the granter does not hold

A member could pass a permission they did not hold to someone else,
because Grant checked only the target's requirements.

Fixes #12
```

Reference: https://go.dev/doc/contribute#commit_messages
