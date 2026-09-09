# Planned deployment acceptance

Use [planned-acceptance.yml](../.github/workflows/planned-acceptance.yml) for schema-3 candidates. It runs against disposable GitHub-hosted Linux VMs and refuses existing updater or site installations.

## Run a candidate

1. Merge the source and runtime fixes to `main`.
2. Run [release-candidate.yml](../.github/workflows/release-candidate.yml) with an unused updater version, an immutable GEOFlow commit, its matching application version, and a sequence greater than the signed stable release. Approve the protected signing environment through the normal review process.
3. After the candidate build succeeds, dispatch acceptance with its run ID:

   ```bash
   gh workflow run planned-acceptance.yml \
     --repo yaojingang/geoflow-updater --ref main \
     -f candidate_run_id=THE_SUCCESSFUL_CANDIDATE_RUN_ID
   ```

4. Review the workflow jobs and artifacts. A failed or skipped job keeps acceptance incomplete. Fix the cause and rerun. Runtime changes require a newly built candidate; the workflow permits harness-only descendants for debugging an existing immutable candidate.

Candidate builds push run-specific images and sign an isolated TUF repository. Production release tags, stable metadata, and Pages remain unchanged.

## Required evidence on both architectures

| Area | Checks |
|---|---|
| Native execution | Linux amd64 on an amd64 VM; Linux arm64 on an arm64 VM; verified archive hashes and candidate identity |
| Application contract | Fresh migrations, initial administrator setup, backfills, readiness, caches, repeated installation, real ingress switch and streaming |
| Existing managed site | Enroll an initialized stable database, run the signed maintenance upgrade, convert to two application slots, preserve the administrator session |
| Complete restoration | Change protected data after backup, restore PostgreSQL, Redis, storage, environment, version, instance, release and Compose files, and compare migration history |
| Interrupted upgrade | Kill the installed updater at retain-assets, quiesce, scheduler freeze, backup, upgrade, layout, candidate, switch, workers and observe; verify the appropriate recovery policy and a second restart |
| Failed recovery | Fail PostgreSQL restoration, retain the recovery identity and retry state, block ordinary mutations, remove the fault and complete an authorized restoration |
| First-install retry | Interrupt after administrator initialization, retry and repeat installation, retain generated credentials, pass readiness and real login |
| Online fixture | Continue public and authenticated HTTP requests, consume pending jobs on the destination slot, deliver a new-slot Reverb broadcast to an old connection, reconnect, and switch application code back while retaining live writes |

The queue fixture stops the old test consumer before enqueueing twenty jobs for each transition. It records the destination worker's slot and sequence and requires each job to write exactly once. This covers pending-job handover. Production traffic volume, long-running business jobs, browser reconnection behavior, and infrastructure outages need workload-specific testing.

The legacy scheduler test waits for active scheduled children to finish before stopping the idle parent. A durable freeze record lets startup recovery resume the same frozen process after an updater interruption. Native container tests also check the actual process signals and child completion.

## Online fixture scope

The candidate artifact contains an `online-fixture` directory. Its image derives from the exact candidate application image; only its signed upgrade plan changes. The plan allows that candidate's sequence, runs with no pending migrations, and executes migration and cache preparation steps. The artifact records the base image digest, derived image digest, source and target sequences, and plan and manifest hashes.

This fixture tests the online deployment mechanism with identical application code. A production old-version/new-version pair also needs evidence that its schema, queued job payloads, caches and storage remain compatible while both versions run. The ordinary release gate currently rejects production online plans until that acceptance is available. The main candidate can remain a maintenance release and still exercise the online mechanism through the separate fixture.

The release workflow publishes only the main candidate's `targets-source` and application image digests. It does not promote the online fixture.

## Review and publication

Download `planned-acceptance-<run-id>/evidence-template.json`. It includes both container results and all six installed-host results. The release gate checks the required case IDs and candidate identity, so a shortened checklist cannot pass.

The template leaves release operator, security reviewer, and product owner approvals pending. Reviewers fill their own decisions after examining the artifacts. Store the approved evidence in the protected signing environment as described in [the release runbook](release-runbook.md).

Publication requires the same updater commit that built the candidate. A PR merge or runtime fix invalidates an older build for publication. Build and accept the replacement before publishing. Acceptance completion and production publication are separate actions.

For site administrators, follow the [Chinese deployment tutorial](https://github.com/yaojingang/GEOFlow/blob/main/docs/blue-green-deployment-usage.md) or [English deployment tutorial](https://github.com/yaojingang/GEOFlow/blob/main/docs/blue-green-deployment-usage_en.md). Site-level backups and restoration drills should use the site's actual data size and retention policy.
