# Kubernetes

A Helm chart at [`redis-monitor/`](redis-monitor): a Deployment (1 replica by
design, see below), a Service, an optional PVC, and an optional Ingress.

## `values.yaml` vs `values-all.yaml`

Same pattern as `.env.example` / `docker-compose-example.yml` at the repo root:

| File | Purpose |
|---|---|
| [`values.yaml`](redis-monitor/values.yaml) | the chart default — every structural option (probes, security context, service, persistence, ...) plus only the **3 env vars required to boot**: `REDIS_HOST`, `REDIS_PORT`, `REDIS_MONITOR_TOKEN` |
| [`values-all.yaml`](redis-monitor/values-all.yaml) | the same structural options, plus **all 56 env vars** the binary reads, each with a one-liner |

```
# minimal
helm install redis-monitor ./k8s/redis-monitor \
  --set env.REDIS_HOST=your-redis-host \
  --set env.REDIS_PORT=6379 \
  --set env.REDIS_MONITOR_TOKEN=something-long

# everything, documented
helm install redis-monitor ./k8s/redis-monitor -f k8s/redis-monitor/values-all.yaml
```

Both merge with `--set` or a further `-f my-overrides.yaml`, the normal Helm way.

## No Ingress controller

`ingress.enabled` defaults to `false` — leave it off and point your own
reverse proxy (nginx, HAProxy, Traefik, ...) at the Service instead:

- **Proxy in-cluster:** keep `service.type: ClusterIP`, target
  `<release>-redis-monitor.<namespace>.svc.cluster.local:8088`.
- **Proxy outside the cluster:** set `service.type: NodePort` (or
  `LoadBalancer`), and pin `service.nodePort` so the proxy's upstream config
  doesn't change between installs — otherwise Kubernetes assigns a random port
  each time.

## Single replica, by design

Sessions live in process memory and the recorded trend is a local JSON file
under `/data` — neither shared across pods. A second replica doesn't add
capacity, it splits logins and history across pods instead. Keep
`replicaCount: 1`.

## Persistence

`false` in `values.yaml` (`emptyDir` — history is lost on restart), `true` in
`values-all.yaml` (a PVC, size/`storageClass` configurable). Set
`persistence.existingClaim` to reuse a PVC instead of having the chart create
one.

## Generating a TOTP secret

```
kubectl run redis-monitor-totp --rm -it --restart=Never \
  --image=ghcr.io/snyype/redis-monitor:latest \
  -- -totp-secret
```

No shell or local Go toolchain needed — the image prints it. Put the result in
`env.REDIS_MONITOR_TOTP_SECRET`.

## Security defaults

Match the [`scratch`-based image](../Dockerfile): `runAsNonRoot`,
`runAsUser: 10001` (numeric — there's no `/etc/passwd` entry to name it),
`readOnlyRootFilesystem: true` (safe, since the binary only writes under
`/data`), all capabilities dropped. Probes hit `/healthz`/`/readyz` directly
over the pod network, needing no shell or `wget` in the container at all.

One consequence: **`kubectl exec` and `kubectl cp` don't work** here — no
shell for `exec`, no `tar` for `cp`. To inspect `/data` directly, mount the
PVC into another pod rather than reaching into this one.

## Multiple Redis servers

Out of scope — one release per server (its own release name, `env.REDIS_HOST`,
PVC). See [the main README](../README.md#monitoring-more-than-one-redis-server)
for why this isn't a single-binary feature.
