#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# deploy.sh — build the DRA stub controller, deploy into Kind UAT cluster.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="kube-system"
SERVICE_NAME="dra-stub"
KIND_CLUSTER="${KIND_CLUSTER_UAT:-cre-test-uat}"
IMAGE="dra-stub:local"

# ─── 1. Create DeviceClass stubs ───
echo "==> Creating DeviceClass stubs..."
kubectl apply -f - <<'EOF'
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: gpu.nvidia.com
---
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: roce.networking.k8s.aws
EOF

# ─── 2. Build binary and Docker image ───
echo "==> Building dra-stub..."
BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR"' EXIT

CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH:-$(go env GOARCH)}" go build -ldflags="-s -w" \
    -o "$BUILD_DIR/dra-stub" "$SCRIPT_DIR/main.go"

cat > "$BUILD_DIR/Dockerfile" <<'DOCKEREOF'
FROM gcr.io/distroless/static:nonroot
COPY dra-stub /dra-stub
ENTRYPOINT ["/dra-stub"]
DOCKEREOF

docker build -t "$IMAGE" "$BUILD_DIR" --quiet
kind load docker-image "$IMAGE" --name "$KIND_CLUSTER"

# ─── 3. Create RBAC ───
echo "==> Creating RBAC..."
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${SERVICE_NAME}
rules:
- apiGroups: ["resource.nvidia.com"]
  resources: ["computedomains"]
  verbs: ["list", "watch", "get"]
- apiGroups: ["resource.k8s.io"]
  resources: ["resourceclaimtemplates"]
  verbs: ["list", "create", "get"]
- apiGroups: ["resource.k8s.io"]
  resources: ["resourceclaims"]
  verbs: ["list", "get", "patch"]
- apiGroups: ["resource.k8s.io"]
  resources: ["resourceclaims/status"]
  verbs: ["update", "patch"]
- apiGroups: ["resource.k8s.io"]
  resources: ["resourceclaims/binding"]
  verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${SERVICE_NAME}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${SERVICE_NAME}
subjects:
- kind: ServiceAccount
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
EOF

# ─── 4. Deploy ───
echo "==> Deploying dra-stub..."
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: ${SERVICE_NAME}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${SERVICE_NAME}
  template:
    metadata:
      labels:
        app: ${SERVICE_NAME}
    spec:
      serviceAccountName: ${SERVICE_NAME}
      # The stub must run on a real Kind node, not a simulated KWOK node.
      nodeSelector:
        node-role.kubernetes.io/control-plane: ""
      containers:
      - name: dra-stub
        image: ${IMAGE}
        imagePullPolicy: Never
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
EOF

# ─── 5. Wait for rollout ───
echo "==> Waiting for dra-stub deployment to be ready..."
kubectl -n "$NAMESPACE" rollout status deployment/"$SERVICE_NAME" --timeout=60s

echo "==> DRA stub controller deployed successfully."
