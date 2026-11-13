# Run alerse-sync on Kubernetes

How to run the sync server on a Kubernetes cluster, with manifests from
[`deploy/kubernetes`](../deploy/kubernetes). For one host with Docker, use the
compose file in the [README](../README.md#run-your-own-server) instead.

You need:

- a cluster with an ingress controller (the manifests use ingress-nginx) and
  cert-manager with a `ClusterIssuer` named `letsencrypt`;
- a domain name pointing at the ingress controller;
- PostgreSQL (the compose file runs 17): a managed database, or CloudNativePG in
  the cluster (step 2).

## 1. Get the manifests

```sh
git clone https://github.com/VizzleTF/alerse-sync
cd alerse-sync/deploy/kubernetes
```

| File | What it makes |
|---|---|
| `namespace.yaml` | Namespace `alerse-sync`, under the `restricted` Pod Security level |
| `deployment.yaml` | Two server pods, non-root, read-only, with `/livez` and `/readyz` probes |
| `service.yaml` | Service on port 80 |
| `ingress.yaml` | HTTPS for your domain, with the settings the event stream needs |
| `pdb.yaml` | At most one pod down during node maintenance |
| `networkpolicy.yaml` | Only the ingress controller's namespace reaches the pods |
| `postgres/cluster.yaml` | Optional CloudNativePG cluster, not applied by default |

## 2. Set up the database

**Managed Postgres:** create a database and a user, and keep the URL.

**CloudNativePG** (the operator must be installed):

```sh
kubectl create namespace alerse-sync
kubectl apply -n alerse-sync -f postgres/cluster.yaml
kubectl get secret -n alerse-sync alerse-sync-db-app -o jsonpath='{.data.password}' | base64 -d
```

The database is `arcomium` and its user is `arcomium`, at
`alerse-sync-db-rw:5432`. The server creates its tables on start.

## 3. Write the secret

```sh
cp secret.env.example secret.env
```

Put the URL in `secret.env`:

```sh
DATABASE_URL=postgres://arcomium:<password>@alerse-sync-db-rw:5432/arcomium?sslmode=require
```

`secret.env` is ignored by git. `kubectl apply -k` turns it into the Secret
`alerse-sync`.

## 4. Fit the manifests to your cluster

| What | Where | Change it to |
|---|---|---|
| Domain | `ingress.yaml`, both `sync.example.com` | your domain |
| Ingress class and issuer | `ingress.yaml` | yours, if not `nginx` and `letsencrypt` |
| Pod network | `deployment.yaml`, `TRUSTED_PROXY` | your cluster's pod CIDR, so the per-address limits see clients, not the controller |
| Controller namespace | `networkpolicy.yaml` | the namespace your ingress controller runs in |

Another ingress controller or a Gateway API route works if it does the same
three things as the annotations in `ingress.yaml`:
- lets request bodies up to 32 MiB through;
- does not buffer responses, so change events reach devices at once;
- keeps idle connections open longer than 30 seconds, because the server sends a
  keep-alive every 25 seconds.

For server settings beyond these (limits, retention, a public server), add
variables to the container's `env`; the [README](../README.md#configuration)
lists them all.

## 5. Apply and check

```sh
kubectl apply -k .
kubectl -n alerse-sync rollout status deployment/alerse-sync
curl https://sync.example.com/health
```

`curl` prints `ok` once the server answers and reaches the database. Then set
Alerse's sync server to `https://sync.example.com`.

## Update

Change the image tag in `deployment.yaml` to the new release and apply again.
Pods replace one at a time: each fails `/readyz` for `SHUTDOWN_DELAY` (10 s)
before it stops, and devices reconnect their event streams to the others.

## Problems

| What you see | Why | What to do |
|---|---|---|
| Pods not ready, logs say the database is unreachable | wrong `DATABASE_URL`, or the network policy of your database blocks the pods | check the URL in `secret.env`, apply again, and allow the namespace to reach Postgres |
| Changes reach other devices only after an hour | the proxy buffers the event stream | turn off response buffering for the host (step 4) |
| `413` on sync | the proxy limits request bodies | allow 32 MiB |
| Everyone gets `THROTTLED` at once | `TRUSTED_PROXY` does not cover the controller's pods, so every client counts as one address | set it to the pod CIDR |
