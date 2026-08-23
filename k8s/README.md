# Kubernetes

A Helm chart at [`redis-monitor/`](redis-monitor) — a Deployment (1 replica by
design, see below), a Service, an optional PVC, and an optional Ingress.

```
helm install redis-monitor ./k8s/redis-monitor \
  --set env.REDIS_HOST=your-redis-host \
  --set env.REDIS_PORT=6379 \
  --set env.REDIS_MONITOR_TOKEN=something-long
```

## Two values files, same pattern as `.env.example` / `docker-compose-example.yml`

| File | Purpose |
|---|---|
| [`values.yaml`](redis-monitor/values.yaml) | the chart default — every structural option (probes, security context, service, persistence, ...) plus only the **3 env vars actually required to boot**: `REDIS_HOST`, `REDIS_PORT`, `REDIS_MONITOR_TOKEN` |
| [`values-all.yaml`](redis-monitor/values-all.yaml) | the same structural options, plus **every one of the 56 env vars** the binary reads, each with a one-liner — install with it to see (and override) everything at once |

```
helm install redis-monitor ./k8s/redis-monitor -f k8s/redis-monitor/values-all.yaml
```

Both are deep-merged by Helm the normal way, so `-f values-all.yaml --set env.REDIS_HOST=...`
or a third `-f my-overrides.yaml` layered on top both work as expected.

## No Ingress controller? Bring your own proxy

`ingress.enabled` defaults to `false`. If your cluster has no Ingress
controller, leave it off and point your own reverse proxy (nginx, HAProxy,
Traefik, ...) at the Service instead:

- **Proxy runs in-cluster:** leave `service.type: ClusterIP` and target
  `<release>-redis-monitor.<namespace>.svc.cluster.local:8088` directly.
- **Proxy runs outside the cluster:** set `service.type: NodePort` and,
  optionally, pin `service.nodePort` to a fixed port (otherwise Kubernetes
  picks a random one from the node-port range on every install) so the proxy's
  upstream config doesn't have to change between installs. `LoadBalancer`
  works the same way if your cluster/cloud provisions one.

## Single replica, by design

`replicaCount` defaults to `1` and should stay there. Sessions live in
process memory and the recorded trend is a local JSON file under `/data` —
neither is shared across pods, so a second replica doesn't add capacity, it
splits logins (a session created by pod A is invisible to pod B) and history
(each pod records its own partial series) across replicas. If you need this
behind a load balancer for HA, put a single pod behind it, not several.

## Persistence

`persistence.enabled` is `false` in `values.yaml` (an `emptyDir` — the
recorded trend is lost on every pod restart) and `true` in `values-all.yaml`
(a PVC, size and `storageClass` configurable, so the trend survives restarts
and rescheduling). Set `persistence.existingClaim` to reuse a PVC you already
created instead of having the chart make one.

## Generating a TOTP secret

No need for a local Go toolchain or `kubectl exec` — the image itself prints
one:

```
kubectl run redis-monitor-totp --rm -it --restart=Never \
  --image=ghcr.io/snyype/redis-monitor:latest \
  -- -totp-secret
```

Put the printed secret in `env.REDIS_MONITOR_TOTP_SECRET`.

## Security defaults

The chart's defaults assume the [`scratch`-based image](../Dockerfile):
`runAsNonRoot: true` / `runAsUser: 10001` (matching the numeric UID baked into
the image — there is no `/etc/passwd` entry to name it), `readOnlyRootFilesystem: true`
(safe, since the binary only ever writes under the `/data` mount), and all
Linux capabilities dropped. Liveness and readiness probes hit `/healthz` and
`/readyz` directly over the pod network — unlike the Dockerfile's own
`HEALTHCHECK`, a Kubernetes probe needs no shell or `wget` inside the
container at all.

One consequence worth knowing: **`kubectl exec` and `kubectl cp` don't work**
against this image — `exec` because there's no shell to run, and `cp` because
it shells out to `tar` inside the container, which also isn't there. Reach
`/data` through the PVC itself (mount it into another pod) if you need to
inspect the recorded trend directly, rather than through the running pod.

## Multiple Redis servers

Out of scope today — this chart, like the binary itself, points at exactly
one Redis server per release. To monitor several, install the chart once per
server (different release names, e.g. `helm install redis-monitor-prod ...` /
`helm install redis-monitor-staging ...`), each with its own `env.REDIS_HOST`
and its own PVC. See the "Monitoring more than one Redis server" note in the
[main README](../README.md#monitoring-more-than-one-redis-server) for why
this isn't a single-binary feature (yet).
