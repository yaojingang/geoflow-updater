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

## Architecture matrix

| Architecture | Host | GEOFlow from/to | PostgreSQL | Redis | Operator | Date | Result |
|---|---|---|---|---|---|---|---|
| linux/amd64 |  |  |  |  |  |  | pending |
| linux/arm64 |  |  |  |  |  |  | pending |

## Required evidence per host

- [ ] Pre-cutover database query shows no legacy apply or rollback row in `queued` or `running` state.
- [ ] Pre-cutover Redis or database queue inspection shows no pending or reserved `system-updates` job.
- [ ] Enrollment rejects unsafe roots and completes for the approved root.
- [ ] `authorization-uri` provisions distinct update, backup, and rollback authenticator entries; the master secret file is root-only and absent from the application container.
- [ ] On a Phase B source deployment, `doctor --json` reports exactly one failure with ID `retired-update-worker`; every other check passes and the website keeps only the signed update handover enabled.
- [ ] The update preflight accepts that single bounded transition failure. Any additional failed or warning check blocks the update.
- [ ] Managed Compose contains no `system-update-queue` service; activation removes the retired container, and post-activation `doctor --json` passes every check including `mutation-authorization` and required containers.
- [ ] Website status, recovery-point listing, and verification work with the control token.
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
