# onyx-rest

One REST call with a secret Onyx injects. The token never enters argv, the
agent's context or the caller's process: Onyx's `run_command` puts it in
`ONYX_REST_TOKEN`, onyx-rest adds the auth header, and the response lands in a
`0600` file. Onyx suppresses the stdout and stderr of every secret-injected
child, so `--out` is the only channel back; read it afterwards.

```sh
onyx-rest --base https://<host>/ [--basic-user <user>] <METHOD> <PATH> [--data <json>|@<file>] --out <file>
```

- `--basic-user` → HTTP Basic `base64(user:token)`; without it, `Bearer <token>`.
- Methods: GET POST PUT PATCH DELETE. `--data` must be valid JSON (inline or `@file`).
- `--out` gets `{"status": <int>, "body": <JSON, raw string or null>}`, truncated, mode `0600`.
- Exit 0 on 2xx, 1 on any other HTTP status (`--out` still written), 2 on a refused
  invocation, a missing token or a transport failure (nothing written). 60 s timeout.

## Binding it in Onyx

An Onyx `allowed_commands` entry is an argv **string prefix**. Bind the absolute
path, `--base` and the base **with its trailing slash**:

```
/Users/<you>/.local/bin/onyx-rest --base https://byadf.atlassian.net/
```

That pins the host because onyx-rest refuses everything that could leave it:

- `--base` must be the first argument and appear once (a second `--base` or `--base=` is refused);
- the base may carry nothing after the host but a trailing slash, so a longer
  token such as `https://byadf.atlassian.net/.evil` is refused;
- PATH must start with a single `/`, carry no `://` or `\` before its query, and resolve on the base's scheme and host.

Without the trailing slash the prefix also admits `https://byadf.atlassian.net.evil.test`.

Example (Jira through the Onyx MCP):

```json
{"argv": ["/Users/<you>/.local/bin/onyx-rest", "--base", "https://byadf.atlassian.net/",
          "--basic-user", "<email>", "GET", "/rest/api/3/myself", "--out", "/tmp/jira.json"],
 "env_refs": {"ONYX_REST_TOKEN": "onyx://Jira/<item>/token"}}
```

A caller making many calls should `mint_token` once and pass the `onyxh_…`
handle as the `env_refs` value: every direct resolve spends one of the item's
releases (the Onyx app caps them per item per minute), a handle does not.
