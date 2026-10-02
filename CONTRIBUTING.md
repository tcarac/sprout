# Contributing to Sprout

Thanks for helping make isolated PostgreSQL development easier. Bug reports,
documentation fixes, benchmarks, and code improvements are welcome.

## Report an issue

Open a [GitHub issue](https://github.com/tcarac/sprout/issues) with the Sprout
command, expected result, actual result, operating system, PostgreSQL version,
and steps to reproduce. For performance reports, include the database size,
number of worktrees, timings, and whether the seed was already prepared. Remove
credentials and private data from logs before sharing them.

## Submit a change

1. Fork the repository or create a feature branch. Keep `main` clean.
2. Make one focused change and add tests when behavior changes.
3. Run `go test -short ./...` and `go vet ./...`.
4. Open a pull request to `main`. Describe the change and how you verified it.

CI runs the unit suite, vet, Docker build, and a PostgreSQL integration test.
All four checks must pass before a pull request can merge. `main` rejects
direct pushes, force pushes, and deletion. Review approvals are welcome but are
not mandatory, so a solo maintainer can merge passing pull requests.

To run the PostgreSQL test locally, point `SPROUT_TEST_ADMIN_URL` at a
**disposable** server where the role may create and drop databases:

```sh
SPROUT_TEST_ADMIN_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -race -run TestConcurrentWorktreesAndSeedRefresh ./pkg/worktree
```

## License

By contributing, you agree that your contribution is licensed under the
[MIT License](LICENSE). Sprout includes code derived from pgbranch; its
original copyright notice is retained.
