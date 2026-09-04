# Phase C staging rehearsal

This is a release-blocking evidence record for the first release that retires the Laravel update executor. Use disposable Linux staging hosts with production-like storage, PostgreSQL, Redis, and HTTPS ingress. Attach logs with credentials, control tokens, authorization URIs, and six-digit codes removed. Both hosts must use the same downloaded `phase-c-candidate-<run-id>` artifact.

## Candidate preparation

1. Dispatch `release-candidate.yml` with the final updater commit, GEOFlow ref, versions, and release sequence.
2. Download the `phase-c-candidate-<run-id>` artifact and verify the workflow attestation for both updater archives.
3. Serve `candidate/tuf/repository` from a private HTTPS origin reachable by the disposable hosts.
4. Install the candidate archive for each host architecture. Add the following root-owned systemd environment only for the rehearsal, then restart the updater:

   ```text
   GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY=1
   GEOFLOW_UPDATER_TUF_METADATA_URL=https://candidate.example/metadata
   GEOFLOW_UPDATER_TUF_TARGETS_URL=https://candidate.example/targets
   ```

5. Confirm the service environment is readable only by root. Remove the three variables after the rehearsal. Production startup uses the embedded official repository URLs.

## Native GitHub-hosted rehearsal

After the final candidate run succeeds, dispatch `phase-c-rehearsal.yml` from `main` and provide its numeric `candidate_run_id`. The matrix assigns `ubuntu-24.04` to `linux-amd64` and `ubuntu-24.04-arm` to `linux-arm64`. Each job verifies `uname -m`, downloads the named candidate artifact from the exact run, checks its recorded hash and GitHub artifact attestation, and rejects a candidate built from a different updater commit.

Each architecture runs on a fresh GitHub-hosted VM. Its Docker daemon, ports, volumes, `/opt/geoflow-phase-c-rehearsal` deployment, updater state, recovery points, local HTTPS candidate repository, generated credentials, and fault controls exist only inside that VM. The workflow has no route to a developer machine's `localhost`, so a local GEOFlow deployment such as `http://localhost:18080/admin` remains untouched.

The managed app startup keeps the `www-data` worker user and explicitly sets the FPM pool group to the root-owned control token's dynamic group. FPM resets supplementary groups when dropping privileges, so Compose `group_add` alone does not establish worker access. Token, runtime-directory, and socket permissions remain `0640`, `0750`, and `0660`; the master mutation secret remains root-only and unmounted. Rehearsals inspect actual FPM worker IDs/groups before and after upgrade and after rollback, in addition to exercising authenticated HTTP routes.

The packaged systemd unit preserves the runtime directory across service stops and restarts so existing application bind mounts retain the live parent directory. Its root ownership, dynamic group, and `0750` mode remain unchanged; the agent replaces only its socket. The directory remains under volatile `/run` and is cleared at host reboot. Rehearsals wait for an authenticated read-only API response after each restart, then recheck actual website controls and FPM permissions after handover, recovered rollback, resume recovery, and lockout persistence.

For the frozen sequence-1 Phase B template, the rehearsal verifies its exact signed hash before applying the bounded Host healthcheck and worker-group compatibility corrections. It then verifies the corrected hash and exact equality with the candidate template before handover. The signed image bytes and public sequence-1 TUF targets remain unchanged.

The legacy Redis container has no persistent data mount. Seed and immediately read back the Redis recovery marker after the managed handover, so the first signed update backs up a verified baseline in the managed persistent store. Pre-cutover idle checks remain before handover; rollback must still restore the baseline marker and remove the post-update key.

The automated host sequence performs the Phase B handover, scoped authenticator checks, a signed migration update, full backup inspection, manual backup, verification, and rollback through authenticated administrator HTTP routes. It verifies login, super-admin authorization, CSRF, the current administrator password, scoped TOTP relay, controller error handling, 90-day history separation, and read-only detail routes. It then covers forced migration and activation failures, service termination after real migration and resume commands have changed state, persistent `recovery_required` recovery with backoff, distributed anti-spray lockout, and repeated generated-secret scans across the updater journal, container output, and Laravel daily logs.

The retained Phase B environment can keep the legacy administrator layout after the signed update. Rejected-rollback checks therefore require the shared error container and the expected operation-failure message across both layouts. HTTP status, the unchanged current operation, and the sensitive-log scan remain mandatory; a V3-only presentation marker is not a release condition.

The HTTP regression in `go test -race ./...` executes the rehearsal's actual form helpers against a loopback fixture server and requires Bash, curl, and Python 3. It checks both layouts and rejects missing or unrelated errors, unexpected HTTP status, a changed operation, and a failing log gate without starting Docker or systemd.

Faults are introduced by a root-owned Docker command wrapper on the disposable host; the updater archive and signed application images stay byte-for-byte identical to the candidate. Every rollback scenario corrupts PostgreSQL data, Redis, storage, the site environment, and managed deployment files after its recovery point is created. The result remains blocked until those values, the migration fingerprint, managed-file hashes, diagnostics, retired-worker state, and completed operation state all return to the recorded Phase B baseline and stay stable across an updater restart. Uploaded diagnostics are redacted again before the evidence directory is returned to the workflow runner.

After successful reconciliation, the operation JSON omits the zero-valued `reconcile_attempts` field. The rehearsal accepts that omission or an explicit numeric zero, rejects nonzero and invalid values, and then continues every restored-data and restart assertion. Executable regression cases use the actual operation serializer and the rehearsal's post-recovery assertions.

Download `phase-c-rehearsal-linux-amd64`, `phase-c-rehearsal-linux-arm64`, and the combined `phase-c-rehearsal-<run-id>` artifact. Both architecture `result.json` files must say `pass`. Review their check list and operation records, then complete the three human approvals in `evidence-template.json`. A workflow result does not supply or impersonate those approvals.

## Architecture matrix

| Architecture | Host | GEOFlow from/to | PostgreSQL | Redis | Operator | Date | Result |
|---|---|---|---|---|---|---|---|
| linux/amd64 | GitHub `ubuntu-24.04` x64 | 2.3.0 → 3.0.0 | 18 | 8 | GitHub Actions |  | pending |
| linux/arm64 | GitHub `ubuntu-24.04-arm` | 2.3.0 → 3.0.0 | 18 | 8 | GitHub Actions |  | pending |

## Required evidence per host

- [ ] Pre-cutover database query shows no legacy apply or rollback row in `queued` or `running` state.
- [ ] Pre-cutover Redis or database queue inspection shows no pending or reserved `system-updates` job.
- [ ] Enrollment rejects unsafe roots and completes for the approved root.
- [ ] `authorization-uri` provisions distinct update, backup, and rollback authenticator entries; the master secret file is root-only and absent from the application container.
- [ ] On a Phase B source deployment, `doctor --json` reports exactly one failure with ID `retired-update-worker`; every other check passes and the website keeps only the signed update handover enabled.
- [ ] The update preflight accepts that single bounded transition failure. Any additional failed or warning check blocks the update.
- [ ] Managed Compose contains no `system-update-queue` service; activation removes the retired container, and post-activation `doctor --json` passes every check including `mutation-authorization` and required containers.
- [ ] Website status, recovery-point listing, and verification work with the control token.
- [ ] Actual non-root FPM workers retain the dynamic Updater group; token/directory/socket ownership and `0640`/`0750`/`0660` modes remain unchanged through handover, upgrade, and rollback.
- [ ] Website update, backup, and rollback fail without a six-digit code.
- [ ] A valid code starts exactly one matching mutation; cross-operation use and replay both fail.
- [ ] Five consecutive invalid codes within one scope or distributed across mutation scopes trigger a persistent 15-minute lockout. After expiry, a successful code clears its accepted scope while failures recorded in other scopes continue to count toward the aggregate anti-spray budget. Record only the attempts required for this check.
- [ ] Full backup includes PostgreSQL, Redis, storage, environment, version marker, and updater deployment state.
- [ ] Signed update with a migration succeeds and records every durable operation stage.
- [ ] A later manual backup leaves website rollback fixed to the newest pre-update checkpoint; older and manual recovery points remain available to root through the CLI.
- [ ] Forced migration and activation failures each restore Phase B data, configuration, deployment state, and the prior version; the restored Compose does not restart `system-update-queue`, the operation reaches `rolled_back`, and no later reconciliation repeats the restore.
- [ ] Updater restart during migration reconciles safely.
- [ ] Updater restart during resume reconciles safely.
- [ ] A stable recovery failure is persisted as `recovery_required`, the Unix-socket status API stays available, mutations remain blocked, retry attempts back off, and the operation converges after the fault is removed.
- [ ] Administrator history shows the latest 90 days, archive view shows older rows, and every legacy detail page is read-only.
- [ ] Application and updater logs contain no administrator password, control token, authorization URI, secret, or six-digit code.

## Approval

| Role | Name | Date | Decision | Evidence link |
|---|---|---|---|---|
| Release operator |  |  | pending |  |
| Security reviewer |  |  | pending |  |
| Product owner |  |  | pending |  |

Any failed or missing item keeps publication blocked. Record a linked issue, repeat the affected architecture, and obtain fresh approvals after remediation.

## Machine-readable publication evidence

Copy `candidate/candidate.json` unchanged into the `candidate` field of a JSON document with this shape:

```json
{
  "schema_version": 1,
  "candidate": {},
  "architectures": {
    "linux-amd64": {"result": "pass", "evidence_link": "https://..."},
    "linux-arm64": {"result": "pass", "evidence_link": "https://..."}
  },
  "approvals": {
    "release_operator": {"name": "...", "decision": "approved"},
    "security_reviewer": {"name": "...", "decision": "approved"},
    "product_owner": {"name": "...", "decision": "approved"}
  }
}
```

Encode the completed file with base64 and store it as `PHASE_C_REHEARSAL_EVIDENCE_B64` in the protected `release-signing` environment. Dispatch `release.yml` with the same candidate run ID and the lowercase SHA-256 of the decoded JSON. Publication verifies the protected evidence, candidate identity, source commits, archive attestations and hashes, signed target bytes, image digests, both architectures, and all approvals before changing public state.

An explicitly authorized emergency release may use the repository-owner risk waiver documented in [the release runbook](release-runbook.md#emergency-super-administrator-risk-waiver). The waiver covers only the missing amd64 and arm64 host rehearsal and approvals. Candidate identity, cryptographic verification, signed target bytes, multi-architecture image indexes, protected release environment, and TUF publication controls remain mandatory. The published `publication-authorization.json` identifies the accepted gap so the rehearsal can be completed and tracked after release.
