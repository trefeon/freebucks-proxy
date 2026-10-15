# Moved → [trefeon/freelane](https://github.com/trefeon/freelane)

This repository is **deprecated and no longer maintained**. The project has
been renamed and republished at a new address:

### 👉 **[github.com/trefeon/freelane](https://github.com/trefeon/freelane)**

Please open issues and pull requests there instead. This repository is kept
read-only as an archive for anyone who still has it pinned.

**What changed at the new home**

| | Here (archived) | [freelane](https://github.com/trefeon/freelane) |
|---|---|---|
| Module / binary / image | `freebuff-proxy` | `freelane` |
| Go module path | `freebuff-proxy/backend/...` | `freelane/backend/...` |
| Container image | `ghcr.io/trefeon/freebucks-proxy` | `ghcr.io/trefeon/freelane` |
| CI | archived | GitHub-hosted runners, forking works |

The upstream wire protocol, the pooled routing model and the wire field names
are unchanged — only the project name and the published tree differ.

**Migration**

```sh
# old
import "freebuff-proxy/backend/internal/config"
docker pull ghcr.io/trefeon/freebucks-proxy:latest

# new
import "freelane/backend/internal/config"
docker pull ghcr.io/trefeon/freelane:latest
```

Configuration is read from the same environment variables; see
[`.env.example`](https://github.com/trefeon/freelane/blob/main/.env.example)
in the new repository.

License: MIT, in both repositories.