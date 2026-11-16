# kheck

kheck is an MCP server in front of the Kubernetes API, and the only way agents change the cluster. An agent
asks it to restart, delete a pod, scale or cordon. kheck checks the change against rules you write, makes it,
logs it as the agent that asked (to stderr and over OpenTelemetry), and waits for the workload to come back.

Agents get no Kubernetes write access of their own. Whatever an agent's model decides, and whatever bugs its
harness has, the cluster only changes in the ways kheck allows. And kheck's own ServiceAccount is limited
again by the API server (see [deploy/policy.yaml](deploy/policy.yaml)), in case kheck has bugs too.

## How a change goes

1. **Who's asking.** The agent connects with a Kubernetes ServiceAccount token made for kheck (audience
   `kheck`). kheck checks it with the API server (a TokenReview), and only `callers` get in. A token for
   kheck can't be used against the API server, so kheck can't replay it there either.
2. **Protected?** Nothing in a protected namespace, and nothing labelled `kheck/protected=true` (on the
   namespace, the workload, the pod's owner or the node), is ever changed.
3. **The gates.** Each gate that applies must pass: see below.
4. **Can it work?** A new pod can't fix a bad image or config, its own memory limit, the wrong CPU
   architecture, a failing liveness probe or a pod that can't be scheduled; a pod without a controller
   wouldn't come back; scaling to the count it already has isn't a restart. These are refused as `failed`.
5. **The change**, then a `change` log record with who asked, why, the target and how it went. kheck waits
   up to `settle` for the workload to be ready.

Anything kheck can't check (the API server or metrics down, a query that errors) refuses the change. Every tool answers with:

```json
{"result": "done", "settled": "deployment app/api has 2/2 ready after 14s.", "ready": true}
{"result": "refused", "why": "gate \"one node at a time\": 1 other nodes are already cordoned (w2); at most 0 may be"}
{"result": "failed", "why": "a new pod can't fix this: api-5d4-x: api is ImagePullBackOff (...)"}
```

`refused` means a rule said no: don't work around it, hand it to a person. `failed` means it couldn't be
done as asked.

## Tools

| Tool | Arguments | Does |
|---|---|---|
| `restart` | ref, namespace, kind (deployment, statefulset, daemonset), name, why | `kubectl rollout restart` |
| `delete_pod` | ref, namespace, name, why | deletes one pod for its controller to replace |
| `scale` | ref, namespace, kind (deployment, statefulset), name, replicas, why | sets replicas |
| `cordon`, `uncordon` | ref, node, why | marks a node (un)schedulable; nothing is evicted |
| `gates` | | what's protected, and the gates |

Every change says why. `ref` is optional: what it's for, such as an incident or ticket, logged with it.
To pause agents, hold changes for a person or budget them, put that in the agent's harness, in front of kheck.

## Configuration

```yaml
listen: ":8080"
callers: ["system:serviceaccount:agents:*"] # Kubernetes users that may ask, names or globs
audiences: [kheck]             # a caller's token must be for one of these
metrics:
  url: http://prometheus.monitoring.svc:9090 # any Prometheus-compatible API, for query gates
settle: 90s                    # how long a change waits for its workload to be ready
protect:
  namespaces: [kube-system, flux-system, "cert-*"]
  label: kheck/protected       # "true" on a namespace, workload or node protects it
asleepTaints: [example.com/asleep] # nodes down on purpose aren't counted as down or cordoned
gates:
  - name: one node at a time
    actions: [cordon]
    require:
      controlPlane: false      # never a control-plane node
      otherNodesReady: true    # every other node is Ready
      maxCordoned: 0           # and none is already cordoned
  - name: replicas
    actions: [scale]
    require: {minReplicas: 1, maxReplicas: 10}
  - name: home automation is idle
    actions: [restart, delete_pod]
    namespaces: [home-assistant]
    require:
      # PromQL against metrics.url: it must return something, and nothing zero.
      query: 'sum(rate(http_server_request_duration_seconds_count{k8s_namespace_name="$NAMESPACE"}[5m])) < 1'
```

A gate applies to its `actions` (all when left out) in its `namespaces` (all when left out; a gate with
namespaces never applies to nodes). Every condition in `require` must hold; one left out isn't checked.

| Condition | Holds when |
|---|---|
| `controlPlane: false` | the node isn't a control-plane node |
| `otherNodesReady: true` | every other node is Ready, except asleep ones |
| `maxCordoned: N` | at most N other nodes are already cordoned, not counting asleep ones |
| `minReplicas`, `maxReplicas` | scale sets replicas within them |
| `query` | the PromQL returns at least one sample, and none is zero. `$NAMESPACE`, `$NAME` and `$NODE` are the target |

## Deploying

[deploy/kheck.yaml](deploy/kheck.yaml) has the namespace, ServiceAccount, RBAC, config, Deployment and Service.
[deploy/policy.yaml](deploy/policy.yaml) is the API server's limit on kheck's ServiceAccount: it may only
restart and scale (1–10) workloads without changing what they run, delete pods that have a controller, and
cordon nodes other than control planes, and nothing in a protected namespace or labelled `kheck/protected`.
Keep its namespace list in step with `protect.namespaces`.

Give the agent a token for kheck with a projected ServiceAccount token, and point its MCP client at
`http://kheck.kheck.svc:8080/mcp` with `Authorization: Bearer <token>`, read from the file each time it
connects (the kubelet rotates it):

```yaml
volumes:
  - name: kheck-token
    projected:
      sources:
        - serviceAccountToken: {audience: kheck, path: token, expirationSeconds: 3600}
```

## Logs

Every change, refused or made, is a `change` record:

```json
{"msg":"change","by":"system:serviceaccount:agents:fixer","ref":"INC-3","action":"restart","namespace":"app",
 "kind":"deployment","name":"api","why":"workers deadlocked","result":"done","detail":""}
```

They go to stderr as JSON, and over OTLP/HTTP when `OTEL_EXPORTER_OTLP_ENDPOINT` (or
`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`) is set. The other standard `OTEL_*` variables apply, such as
`OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_RESOURCE_ATTRIBUTES`; the service name is `kheck`.
