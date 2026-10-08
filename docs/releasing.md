# Releasing

A release is a `vX.Y.Z` tag on `main` (see `release.yml`). Before tagging,
check that tslink's security guarantees hold for the commit, as
[SECURITY.md](../SECURITY.md) describes them:

1. CI is green on the commit: lint, the Go tests (including the check that
   every published guarantee is guarded), the network namespace tests as
   root, and the end-to-end test with its probes.
2. The weekly fuzz run on `main` succeeded within the last 7 days:
   `test/security/report.sh` checks that it succeeded and how recently.
   Check by hand whether a failing input is still open.
3. The red team ran on the testbed for this commit without a failure:
   `test/security/testbed/provision.sh`, `test/security/redteam.sh`, then
   `test/security/testbed/teardown.sh`.
4. `test/security/report.sh` reports nothing missing. It writes the report
   outside the repository; keep it with the release's records.
5. The guarantees marked `Manual:` in SECURITY.md were checked by hand.

The release notes list the published guarantees and how each was verified:
copy the "Guarantees and their tests" section of the report.
