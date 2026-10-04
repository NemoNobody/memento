# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems.

Use GitHub's private reporting: **Security → Report a vulnerability** on this
repository, or open a draft advisory at
`https://github.com/NemoNobody/memento/security/advisories/new`. You will get a
response within a few days.

## Scope and notes

- `memento` stores facts in **plaintext by design** — never store secrets there
  (see the README). Reports about the store not being encrypted are by design.
- Relevant areas: credential handling (`memento.conf`, environment variables),
  the MCP server (stdio), request construction to OpenSearch, and anything that
  could leak a secret into logs or outputs.
- The OpenSearch cluster itself is out of scope — report those to the OpenSearch
  project or your cluster operator.
