#!/usr/bin/env bash
# env-up.sh — idempotently bring up the parallax kind dev environment.
#
# Reuses kapture's e2e recipe conventions (Appendix A.5): a 2-node kind cluster,
# Gateway API standard CRDs, Envoy Gateway, MinIO object storage, and a
# lightweight Prometheus scraping at 5s. Safe to re-run.
set -euo pipefail

# ---- Configuration (override via environment) --------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-parallax-dev}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.31.2}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.2.1}"
ENVOY_GATEWAY_VERSION="${ENVOY_GATEWAY_VERSION:-v1.2.4}"
MONITORING_NS="${MONITORING_NS:-monitoring}"
MINIO_NS="${MINIO_NS:-minio}"
ENVOY_NS="${ENVOY_NS:-envoy-gateway-system}"
KUBECONFIG_OUT="${KUBECONFIG_OUT:-hack/kind-kubeconfig}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---- Helpers -----------------------------------------------------------------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || die "required tool '$1' not found on PATH"
}

require kind
require kubectl
require helm

# ---- 1. kind cluster ---------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "kind cluster '${CLUSTER_NAME}' already exists — reusing"
else
  log "creating kind cluster '${CLUSTER_NAME}' (2 nodes)"
  cat <<EOF | kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
EOF
fi

log "writing kubeconfig to ${KUBECONFIG_OUT}"
kind get kubeconfig --name "${CLUSTER_NAME}" > "${REPO_ROOT}/${KUBECONFIG_OUT}"
export KUBECONFIG="${REPO_ROOT}/${KUBECONFIG_OUT}"

kubectl wait --for=condition=Ready nodes --all --timeout=120s

# ---- 2. Gateway API standard CRDs -------------------------------------------
log "installing Gateway API ${GATEWAY_API_VERSION} standard CRDs"
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"

# ---- 3. Envoy Gateway --------------------------------------------------------
log "installing Envoy Gateway ${ENVOY_GATEWAY_VERSION}"
helm upgrade --install envoy-gateway \
  oci://docker.io/envoyproxy/gateway-helm \
  --version "${ENVOY_GATEWAY_VERSION}" \
  --namespace "${ENVOY_NS}" --create-namespace \
  --wait --timeout 5m
kubectl -n "${ENVOY_NS}" rollout status deploy/envoy-gateway --timeout=180s || \
  warn "envoy-gateway rollout not confirmed; continuing"

# ---- 4. MinIO object storage (single-node, dev creds) ------------------------
log "installing MinIO (single-node dev) in namespace ${MINIO_NS}"
kubectl get ns "${MINIO_NS}" >/dev/null 2>&1 || kubectl create ns "${MINIO_NS}"
kubectl apply -n "${MINIO_NS}" -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: minio-creds
type: Opaque
stringData:
  # Dev-only credentials. DO NOT use outside a throwaway kind cluster.
  rootUser: parallax
  rootPassword: parallax-dev-secret
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  labels: { app: minio }
spec:
  replicas: 1
  selector: { matchLabels: { app: minio } }
  template:
    metadata:
      labels: { app: minio }
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        fsGroup: 1000
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: minio
          image: quay.io/minio/minio:latest
          args: ["server", "/data", "--console-address", ":9001"]
          env:
            - name: MINIO_ROOT_USER
              valueFrom: { secretKeyRef: { name: minio-creds, key: rootUser } }
            - name: MINIO_ROOT_PASSWORD
              valueFrom: { secretKeyRef: { name: minio-creds, key: rootPassword } }
          ports:
            - { name: s3, containerPort: 9000 }
            - { name: console, containerPort: 9001 }
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: data, mountPath: /data }
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: minio
  labels: { app: minio }
spec:
  selector: { app: minio }
  ports:
    - { name: s3, port: 9000, targetPort: 9000 }
    - { name: console, port: 9001, targetPort: 9001 }
EOF
kubectl -n "${MINIO_NS}" rollout status deploy/minio --timeout=180s || \
  warn "minio rollout not confirmed; continuing"

# ---- 5. Lightweight Prometheus (5s scrape) -----------------------------------
# NOTE: 5s scrape interval matches parallax's SLI measurement windows — short
# trials need fine-grained samples (Study measure windows are minutes, and SLIs
# like queue_saturation_peak use [window:15s] sub-queries). Do NOT raise it.
log "installing lightweight Prometheus (5s scrape) in namespace ${MONITORING_NS}"
kubectl get ns "${MONITORING_NS}" >/dev/null 2>&1 || kubectl create ns "${MONITORING_NS}"
kubectl apply -n "${MONITORING_NS}" -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: prometheus
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: prometheus-parallax-dev
rules:
  - apiGroups: [""]
    resources: ["nodes", "nodes/metrics", "services", "endpoints", "pods"]
    verbs: ["get", "list", "watch"]
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: prometheus-parallax-dev
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: prometheus-parallax-dev
subjects:
  - kind: ServiceAccount
    name: prometheus
    namespace: monitoring
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
data:
  prometheus.yml: |
    global:
      scrape_interval: 5s          # parallax SLI fidelity — keep at 5s
      evaluation_interval: 5s
    scrape_configs:
      - job_name: kubernetes-pods
        kubernetes_sd_configs: [{ role: pod }]
        relabel_configs:
          - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
            action: keep
            regex: "true"
          - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_port, __address__]
            action: replace
            regex: (.+);(.+):.+
            replacement: $2:$1
            target_label: __address__
          - source_labels: [__meta_kubernetes_namespace]
            target_label: namespace
          - source_labels: [__meta_kubernetes_pod_name]
            target_label: pod
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prometheus
  labels: { app: prometheus }
spec:
  replicas: 1
  selector: { matchLabels: { app: prometheus } }
  template:
    metadata:
      labels: { app: prometheus }
    spec:
      serviceAccountName: prometheus
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        fsGroup: 65534
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: prometheus
          image: prom/prometheus:v2.55.1
          args:
            - --config.file=/etc/prometheus/prometheus.yml
            - --storage.tsdb.path=/prometheus
            - --storage.tsdb.retention.time=2h
          ports:
            - { name: http, containerPort: 9090 }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: config, mountPath: /etc/prometheus }
            - { name: data, mountPath: /prometheus }
      volumes:
        - name: config
          configMap: { name: prometheus-config }
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: prometheus
  labels: { app: prometheus }
spec:
  selector: { app: prometheus }
  ports:
    - { name: http, port: 9090, targetPort: 9090 }
EOF
kubectl -n "${MONITORING_NS}" rollout status deploy/prometheus --timeout=180s || \
  warn "prometheus rollout not confirmed; continuing"

# ---- Next steps --------------------------------------------------------------
cat <<EOF

$(log "parallax dev environment '${CLUSTER_NAME}' is up")

  KUBECONFIG   ${REPO_ROOT}/${KUBECONFIG_OUT}
  export KUBECONFIG=${REPO_ROOT}/${KUBECONFIG_OUT}

  Prometheus   http://prometheus.${MONITORING_NS}:9090  (5s scrape)
  MinIO S3     http://minio.${MINIO_NS}:9000            (console :9001, user 'parallax')
  Envoy GW     namespace ${ENVOY_NS}

Next steps:
  make tools && make generate         # code-gen (buf + controller-gen)
  make manifests                      # CRDs -> config/crd + charts/parallax/crds
  make build                          # build operator + plugins (CGO_ENABLED=0)
  kubectl apply -k config/crd         # install CRDs
  go run ./cmd/parallax apply --local examples/studies/capture-agent-throughput.yaml

Tear down with: hack/env-down.sh  (or: make env-down)
EOF
