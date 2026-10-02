# Worktree performance check

I measured a local PostgreSQL database of about 26 MB with 200,001 rows in
one table. A connection to the source database stayed open, so the original
worktree command had to use its dump fallback. Four separate Git worktrees ran
`sprout ensure` at the same time.

| Scenario | Time for all four worktrees |
| --- | ---: |
| Original: each worktree tries the busy source, then dumps it | 6.40 s |
| Revised: one seed built on demand, then four template clones | 1.28 s |
| Revised: seed already prepared | 0.41 s |

The revised cold path was about 5 times faster; the warm path about 15 times
faster in this local test. Individual times were nearly the same as wall time,
confirming that the worktrees ran concurrently. Four simultaneous `create`
commands also succeeded in about 0.5 seconds each with a prepared seed.

With ten worktrees, the median time for `sprout list` over twelve runs
fell from 507 ms to 41 ms. The command now reads PostgreSQL metadata through
one connection and derives names from the paths Git already returned.

The seed is a snapshot. Run `sprout refresh` after changing the clean
source database. Existing worktree databases keep their own data. Refresh
builds a replacement seed before removing the old one; a failed build leaves
the old seed available.

These are local measurements, not a promise for other database sizes or
hardware. The repeatable PostgreSQL integration test is
`TestConcurrentWorktreesAndSeedRefresh` in `pkg/worktree` and runs when
`SPROUT_TEST_ADMIN_URL` points to a disposable PostgreSQL server.
