---
title: Export Reports to a Webhook
description: Configure durable HTTP delivery of final Certification reports without an object store or an attached CLI process.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---

Configure a `ReportExportPolicy` once to receive the final JSON report for each new matching Certification. The controller records delivery intent before starting its Workflows. A separately deployed exporter prepares an immutable snapshot, then POSTs the report to your receiver. The CLI does not need to stay connected.

Certification outcome and delivery outcome are independent: a failed test can have a successfully delivered report. `PASSED`, `FAILED`, and `INCOMPLETE` reports are sent. Running reports are not sent as final results. This version supports Certification sources; WorkloadRun is not a policy source.

## Install the exporter separately

Use a dedicated reporting namespace such as `nvcre-reports`. Keep it separate from `nvcre` and every namespace used for temporary certification runs. It contains policies, delivery records, authentication Secrets, and the ConfigMaps used internally for registration and compressed report snapshots.

The exporter chart is available in this repository at `helm/report-exporter`. It is installed as a separate Helm release; these instructions do not assume a published exporter OCI chart. Use the chart and an image built from the same checkout. The image must contain both `/manager` and `/report-exporter`; older NVCRE images do not include the exporter.

From the repository root, build and publish an image to a registry your cluster can read:

```bash
NVCRE_IMAGE_REPOSITORY=registry.example.com/platform/nvcre
NVCRE_IMAGE_TAG=report-export-dev

make docker-build IMG="$NVCRE_IMAGE_REPOSITORY:$NVCRE_IMAGE_TAG"
docker push "$NVCRE_IMAGE_REPOSITORY:$NVCRE_IMAGE_TAG"
```

Apply the CRDs from the same checkout, then install the exporter:

```bash
kubectl apply --server-side -f helm/cluster-readiness-engine/crds/

helm upgrade --install nvcre-report-exporter ./helm/report-exporter \
  --namespace nvcre-reports --create-namespace \
  --set image.repository="$NVCRE_IMAGE_REPOSITORY" \
  --set image.tag="$NVCRE_IMAGE_TAG" \
  --wait
```

For private registries, create an image pull Secret in `nvcre-reports` and set `imagePullSecrets[0].name`. Set `image.digest` to a verified digest to pin the exact image; it takes precedence over `image.tag`.

The exporter has read access to the source resources needed to build reports. Its write access is confined to the reporting namespace. It reads authentication Secrets with namespaced `get` requests; it does not list or watch Secrets. Only trusted operators should be allowed to create or edit policies, exports, registration records, snapshots, or Secrets in this namespace.

## Configure the receiver and enable registration

Create a token Secret from a protected file; the token is never part of the policy or report:

```bash
kubectl create secret generic report-webhook-token \
  --namespace nvcre-reports \
  --from-file=token=/secure/path/report-webhook-token
```

Edit the URL, source namespaces, and label selector in `config/samples/report-export/policy.yaml`, then apply it:

```bash
kubectl apply -f config/samples/report-export/policy.yaml
```

The sample selects Certifications in `gpu-validation` with the label `reports.example.com/export: "true"`. An empty selector selects all new Certifications in the explicitly listed namespaces. Omit `bearerTokenSecretRef` for a receiver that does not require bearer authentication. The Secret must be in the policy's namespace. Updating the Secret takes effect on the next attempt.

For CLI runs, create a stable source namespace beforehand and select it with
`nvcrectl certification run -n gpu-validation ...` (or the Certification manifest's
namespace). The CLI's default generated namespace will not match a policy's
explicit namespace list. A namespace that already existed is retained by CLI
cleanup, so the same policy can cover subsequent runs.

Edit `config/samples/report-export/manager-values.yaml` to set a stable cluster identifier and the reporting namespace. Enable the feature on the main chart, retaining any existing deployment overrides in your values file:

```bash
helm upgrade --install nvcre ./helm/cluster-readiness-engine \
  --namespace nvcre --create-namespace \
  -f config/samples/report-export/manager-values.yaml \
  --set manager.image.repository="$NVCRE_IMAGE_REPOSITORY" \
  --set manager.image.tag="$NVCRE_IMAGE_TAG" \
  --set metrics.serviceMonitor.enabled=false \
  --wait
```

Enable `metrics.serviceMonitor.enabled` if the Prometheus Operator is installed. The main chart does not install Kubeflow Trainer; follow the [deployment guide](../operations/deployment.md) if the cluster does not already have the workload dependencies.

Run Certifications against this installation without `--setup`. The CLI setup
command uses the chart defaults and does not carry these report-export Helm
overrides. After a full reset, reinstall the manager with the same reporting
values before starting new runs; the policy and pending deliveries are retained.

Create subsequent Certifications with the selected label, for example in their manifest:

```yaml
metadata:
  name: gpu-cluster-cert
  namespace: gpu-validation
  labels:
    reports.example.com/export: "true"
```

Create the policy before creating the Certification. Policies do not automatically backfill historical Certifications. A matching policy revision is frozen when the run is registered; editing, suspending, or deleting that policy does not change deliveries already registered. Multiple matching policies create independent deliveries.

## Receive the JSON report

The exporter sends an HTTPS POST with `Content-Type: application/json`, `Idempotency-Key: <reportID>`, and an `Authorization: Bearer …` header when configured. It validates TLS certificates and does not follow redirects. For development against a plain HTTP service, explicitly set the exporter chart's `allowHTTP=true`.

The JSON envelope wraps the existing structured report. This abbreviated example shows its identity fields:

```json
{
  "schemaVersion": "v1",
  "reportID": "<stable-report-id>",
  "source": {
    "clusterID": "production-helsinki-1",
    "kind": "Certification",
    "namespace": "gpu-validation",
    "name": "gpu-cluster-cert",
    "uid": "<certification-uid>"
  },
  "generatedAt": "2026-09-30T10:00:00Z",
  "report": {
    "name": "gpu-cluster-cert",
    "platform": "aws",
    "gpu": "gb200",
    "totalNodes": 16,
    "categories": [],
    "failedNodes": [],
    "result": "PASSED"
  }
}
```

The actual `report` contains the category results, available measurements, failed groups, and other fields used by the CLI report. Every attempt for an export uses the same stored JSON bytes and identifier, including manual retries.

Your receiver should durably save or enqueue the report before returning 2xx. A successful response records acceptance, not completion of downstream processing. Deduplicate requests by `reportID` or `Idempotency-Key`: if an acknowledgement is lost, NVCRE may send the accepted report again.

Transport failures, HTTP 408, 429, and 5xx retry with exponential backoff and jitter. `Retry-After` is honored for 429 and 503 within the delivery deadline. Redirects and other 4xx responses end the current delivery cycle. By default a cycle allows 100 attempts over 24 hours, with a 10-second request timeout and backoff from 10 seconds up to 15 minutes.

## Inspect and recover a delivery

```bash
kubectl get reportexportpolicies -n nvcre-reports
kubectl get reportexports -n nvcre-reports
kubectl describe reportexport <export-name> -n nvcre-reports
```

`status.result` is the certification result; `status.phase`, `reason`, and `message` describe export progress. `SnapshotReady=True` means the payload is durable and controlled source cleanup may proceed, even if the receiver is unavailable. Attempt counts, the next retry time, the last HTTP status, and the payload digest appear in the export status.

After fixing an endpoint or authentication problem, request a new bounded cycle for a retained failed delivery by changing `retryNonce`:

```bash
kubectl patch reportexport <export-name> -n nvcre-reports \
  --type=merge -p '{"spec":{"retryNonce":"retry-2026-09-30-1"}}'
```

Use a different nonce for each request. The payload, destination, identifier, and cumulative attempt count stay unchanged. A payload that has expired cannot be retried. Destination changes belong in the policy for subsequent runs; existing export inputs are immutable.

Cancel an individual export, or suspend registration of new exports:

```bash
kubectl patch reportexport <export-name> -n nvcre-reports \
  --type=merge -p '{"spec":{"cancel":true}}'

kubectl patch reportexportpolicy platform-reports -n nvcre-reports \
  --type=merge -p '{"spec":{"suspend":true}}'
```

Cancellation prevents further attempts but cannot recall an in-flight request. Policy suspension leaves registered deliveries running. Do not delete a pending export to bypass the source cleanup guard; cancel it explicitly.

## Retention and cleanup

Snapshots are retained for seven days after successful delivery and thirty days after final failure by default. A compact export record remains while the original Certification UID still exists, preventing a resync from sending that run again. When the source is gone and retention has elapsed, the record can be removed.

Each snapshot is limited to 8 MiB of JSON and 512 KiB after compression. An oversized report produces `PayloadTooLarge`; it is never truncated. The source remains available until an operator resolves or explicitly cancels the export. Set ResourceQuotas and monitor the reporting namespace's ConfigMap storage according to the number and size of reports you retain.

Controlled Certification deletion waits for the snapshot, not for the remote receiver. Deleting a Certification while it is still running records `SourceDeletedBeforeCompletion` instead of fabricating a final failed report. A CLI wait timeout alone does not end the Certification. Cleanup stops if the snapshot guard cannot complete.

`nvcrectl setup reset` retains ReportExportPolicy and ReportExport resources and CRDs. The separately installed exporter and reporting namespace remain available to send frozen payloads after the main controller and source CRDs are removed. Keep that installation until pending deliveries are handled. Removing finalizers manually or deleting an entire source/reporting namespace bypasses this guarantee.

Do not change or disable the manager's reporting namespace while existing Certifications still have registered exports. Their cleanup guard remains active; restore the original reporting configuration to finish safe cleanup.

See the [report export API reference](../api-reference/report-export.md) for fields and defaults.
