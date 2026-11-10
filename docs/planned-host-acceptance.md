# Planned deployment acceptance

Use [planned-acceptance.yml](../.github/workflows/planned-acceptance.yml) for schema-3 candidates that require Updater protocol 5. It runs against disposable GitHub-hosted Linux VMs and refuses existing updater or site installations.

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
| Existing managed site | Enroll the official Core 3.1.0 sequence-3 image, run the signed maintenance upgrade with the exact migration set, convert to two application slots, and verify re-login after the recovery-epoch requirement takes effect |
| Current-version enrollment | Start the exact signed candidate as an unmanaged site, enroll it and convert at the same sequence, reject a duplicate conversion, restore the original layout and enrollment identity, then confirm conversion remains blocked while recovery is held |
| Complete restoration | Change protected data after backup, restore PostgreSQL, Redis, storage, environment, version, instance, release and Compose files, and compare migration history. Verify old credentials fail, fresh credentials can read, business writes are rejected, and queued work stays quarantined |
| Interrupted upgrade | Use a fresh VM per case. Kill the installed updater at retain-assets, quiesce, scheduler freeze, backup, upgrade, layout, candidate, switch, workers and observe, then check recovery and restart durability |
| Failed recovery | Fail PostgreSQL restoration, retain the recovery identity and retry state, block ordinary mutations, remove the fault and complete an authorized restoration |
| First-install retry | Interrupt after administrator initialization, retry and repeat installation, retain generated credentials, pass readiness and real login |
| Online fixture | Continue public and authenticated HTTP requests, consume pending jobs on the destination slot, deliver a new-slot Reverb broadcast to an old connection, reconnect, and switch application code back while retaining live writes |

The queue fixture stops the old test consumer before enqueueing twenty jobs for each transition. It records the destination worker's slot and sequence and requires each job to write exactly once. This covers pending-job handover. Production traffic volume, long-running business jobs, browser reconnection behavior, and infrastructure outages need workload-specific testing.

The legacy scheduler test waits for active scheduled children to finish before stopping the idle parent. A durable freeze record lets startup recovery resume the same frozen process after an updater interruption. Native container tests also check the actual process signals and child completion.

## Current-version enrollment scope

The `enrollment` host mode uses the main schema-3 candidate's signed legacy Compose topology and immutable image digests. It removes the updater control bridge from the initial application container, initializes PostgreSQL and the administrator, starts the real services, and verifies public HTTP health before any updater instance exists. It then stops the site for maintenance and invokes the installed updater's public `enroll` command. The initialized migration history must remain unchanged.

The signed plan must describe a maintenance layout change with identical source and target sequences and no pending migrations. The rehearsal verifies the persisted `enrolled_release_sha256`, converts to the blue-green layout, and confirms that the full recovery point contains the legacy layout and identity. The converted site must reject a duplicate same-sequence plan. The rehearsal then changes PostgreSQL, Redis, storage and configuration, performs an authorized complete restoration, and compares data markers, configuration hashes and migration history with the enrolled baseline. After restarting the installed agent, the restored identity must remain intact. The restored site opens HTTP with background work held and refuses another conversion.

This covers enrollment of the currently signed application version. The separate `upgrade` mode continues to cover an older managed release upgrading to the candidate. Unmanaged older versions and customized Compose deployments require their documented preparation before enrollment.

## Recovery boundary

The upgrade and crash scenarios verify the official signed Core 3.1.0 source commit and application image, with 152 migrations in the source database. The signed target plan determines the exact pending migration set. The success scenario performs the maintenance upgrade and restores its update checkpoint. The enrollment scenario additionally restores the current candidate's own data and layout.

Each interruption runs from its own pristine installed host. Before a backup checkpoint is available, recovery should resume the original deployment without rotating the epoch. After a checkpoint exists, recovery must validate the restored data, advance the epoch and reach `http_ready` with `background_status=held`. The rehearsal checks the durable restore proofs, held background services, Redis queue quarantine, rejection of the previous Web session and API token, and new login with permitted reads and rejected business writes. It then restarts the updater and confirms the state stays held. This is the terminal state for each restored host. Reconciliation and resuming business work follow the separate recovery contract.

## Online fixture scope

The candidate artifact contains an `online-fixture` directory. Its image derives from the exact candidate application image; only its signed upgrade plan changes. The plan allows that candidate's sequence, runs with no pending migrations, and executes migration and cache preparation steps. The artifact records the base image digest, derived image digest, source and target sequences, and plan and manifest hashes.

This fixture tests the online deployment mechanism with identical application code. A production old-version/new-version pair also needs evidence that its schema, queued job payloads, caches and storage remain compatible while both versions run. The ordinary release gate currently rejects production online plans until that acceptance is available. The main candidate can remain a maintenance release and still exercise the online mechanism through the separate fixture.

The release workflow publishes only the main candidate's `targets-source` and application image digests. It does not promote the online fixture.

## Review and publication

Download `planned-acceptance-<run-id>/evidence-template.json`. The schema-2 evidence includes both container results and 30 installed-host results: upgrade, install, online, enrollment and 11 isolated crash cases on each architecture. Together with evidence assembly, the workflow has 33 required jobs. The gate checks each scenario's required case IDs, native architecture, candidate run and harness commit. Full diagnostic logs remain in the corresponding job artifact; the approval template carries the required results and evidence links.

The template leaves release operator, security reviewer, and product owner approvals pending. Reviewers fill their own decisions after examining the artifacts. Store the approved evidence in the protected signing environment as described in [the release runbook](release-runbook.md).

First publication requires evidence from the exact updater commit that built the candidate. Harness-only descendants can diagnose failures; build and accept a new candidate to publish a changed harness. Runtime, template, dependency, trust-root or publication-code changes also require a new candidate. A partially completed publication may resume from a verified metadata-only successor under the [release runbook's recovery gate](release-runbook.md); the candidate identity and its complete signed targets remain fixed. Acceptance completion and production publication are separate actions.

For site administrators, follow the [Chinese deployment tutorial](https://github.com/yaojingang/GEOFlow/blob/main/docs/blue-green-deployment-usage.md) or [English deployment tutorial](https://github.com/yaojingang/GEOFlow/blob/main/docs/blue-green-deployment-usage_en.md). Site-level backups and restoration drills should use the site's actual data size and retention policy.
