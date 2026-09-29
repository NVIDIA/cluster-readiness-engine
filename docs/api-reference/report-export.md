---
title: ReportExportPolicy and ReportExport
description: Persistent webhook configuration and per-Certification report delivery records.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---

Both resources use `nvcre.nvidia.com/v1alpha1` and are namespaced. Create policies in the dedicated reporting namespace configured on the manager. The controller creates one export per matching policy and Certification UID. See [Export Reports to a Webhook](../how-to-guides/export-reports.md) for installation and a complete example.

## ReportExportPolicy

Short name: `rep`.

| Spec field | Default | Meaning |
|------------|---------|---------|
| `source.kind` | `Certification` | Supported source kind |
| `source.namespaces` | Required | Explicit set of 1–64 source namespaces |
| `source.selector` | Empty | Kubernetes label selector applied to Certification labels; empty matches all in the selected namespaces |
| `webhook.url` | Required | POST destination; HTTPS is required unless the exporter explicitly permits HTTP |
| `webhook.bearerTokenSecretRef.name` | Unset | Authentication Secret in the policy/export namespace |
| `webhook.bearerTokenSecretRef.key` | Unset | Secret data key containing the bearer token |
| `webhook.timeout` | `10s` | Per-attempt timeout; 1 second to 5 minutes |
| `retry.initialBackoff` | `10s` | Initial retry delay; 1 second to 1 hour |
| `retry.maxBackoff` | `15m` | Maximum exponential backoff; at least initialBackoff, at most 24 hours |
| `retry.maxAttempts` | `100` | Attempt limit per cycle, including the first attempt; 1–10,000 |
| `retry.maxElapsedTime` | `24h` | Delivery cycle deadline, starting with the first attempt on a stored snapshot; 1 second to 30 days |
| `retention.succeeded` | `168h` | Successful payload retention; 1 hour to 365 days |
| `retention.failed` | `720h` | Failed payload retention; 1 hour to 365 days |
| `suspend` | `false` | Stop new registrations without cancelling existing deliveries |

Durations use strings such as `10s`, `15m`, or `168h`. Policies must exist before the Certification is created; historical runs are not automatically backfilled. The selected policy revision is frozen at registration. Subsequent policy changes apply to later registrations. Tokens are read from the referenced Secret for every attempt, so token rotation also applies to pending deliveries.

A Certification can match at most 32 policies, with at most 64 KiB of frozen
registration settings. Exceeding either limit stops registration with an error
before Workflows start.

## ReportExport

Short name: `rex`. Exports are normally controller-created. The policy and source are references, not owners; deleting either does not delete an existing export. Its immutable snapshot ConfigMap is owned by the export in the same reporting namespace.

| Spec field | Meaning |
|------------|---------|
| `sourceRef` | Source `kind`, `namespace`, `name`, and immutable Kubernetes `uid` |
| `policyRef` | Selected policy `name`, `uid`, and `generation` |
| `clusterID` | Stable operator-supplied cluster identity |
| `reportID` | Stable report identity used in the payload and `Idempotency-Key` header |
| `webhook`, `retry`, `retention` | Settings frozen from the selected policy |
| `cancel` | Mutable boolean; stop subsequent preparation/delivery attempts |
| `retryNonce` | Mutable string; changing it requests a new bounded retry cycle for a retained failed delivery |

Only `cancel` and `retryNonce` may change after creation. Manual retries retain the destination, JSON bytes, report identifier, and cumulative attempts. Cancellation does not undo a request already in flight.

| Status field | Meaning |
|--------------|---------|
| `phase`, `reason`, `message` | Export lifecycle and sanitized diagnostic details |
| `result` | Final certification result: `PASSED`, `FAILED`, or `INCOMPLETE` |
| `reportID` | Report identity associated with the export status |
| `conditions` | Includes `SnapshotReady`, independently of remote delivery success |
| `snapshotRef.name` | Immutable ConfigMap holding gzip-compressed JSON under `binaryData["report.json.gz"]` |
| `sha256` | Digest of the original JSON bytes |
| `payloadBytes` | Uncompressed JSON size |
| `attempts`, `cycleAttempts` | Cumulative and current-cycle attempt counts |
| `lastHTTPStatus` | Most recent response status, when one was received |
| `snapshotStartedAt` | Start of snapshot preparation |
| `cycleStartedAt` | Start of the current retry cycle |
| `firstAttemptTime`, `lastAttemptTime`, `nextAttemptTime` | Delivery attempt timestamps |
| `completionTime` | End of the current delivery cycle |
| `observedRetryNonce` | Last manual retry request processed |

`SnapshotReady=True` is the local durability signal used by controlled source cleanup. Remote acceptance does not affect the Certification's success or failure. A receiver's 2xx response acknowledges receipt; consumers must handle duplicate delivery by `reportID`.

Phases are `WaitingForResult`, `SnapshotReady`, `Retrying`, `Delivered`, `Failed`,
`Cancelled`, and `Skipped`. After retention expires, the snapshot reference is
removed while the compact record and its readiness history may remain.

The versioned request envelope contains `schemaVersion`, `reportID`, `source`, `generatedAt`, and `report`. `source` includes `clusterID` plus the source reference fields, and optional `reason` and `message` from the terminal Certification condition. `report` uses the existing CLI JSON report model. Snapshot limits are 8 MiB before compression and 512 KiB compressed; exceeding either bound reports `PayloadTooLarge` and does not truncate the report.

API read failures or corrupt result data cannot produce an apparently complete empty report. Pending source measurements delay preparation; measurements that could not be produced after an early failure may be absent. A source deleted before completing is recorded as `SourceDeletedBeforeCompletion`.

Once execution has finished, report preparation retries for up to five minutes.
Unresolved collection, measurement or persistence errors then record
`SnapshotBuildFailed` and keep source cleanup blocked. Resolve the cause and
change `retryNonce` to retry preparation, or explicitly cancel the export.
