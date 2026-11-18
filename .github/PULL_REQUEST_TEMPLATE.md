## What and why

<!-- The problem, the change, and what you expect a reviewer to check. -->

## Checks

- [ ] `make ci` is green locally (gofmt, vet, build, tests, golangci-lint)
- [ ] Interface or behavior changed → README/docs updated in the same PR
- [ ] No secrets, private hostnames or internal URLs in the diff — including
      test fixtures: the store is plaintext by design
- [ ] New behavior has a test that would fail without the change
