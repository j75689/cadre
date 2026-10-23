# Storage

Goal: **a small installation needs no database; a large one switches to Postgres.** The core depends on three operations only, and every driver implements all three.

## Three operations

| Operation | Kubernetes API (etcd) | Postgres | Object storage (S3, GCS) |
| --- | --- | --- | --- |
| **single-object conditional write** (fencing) | `resourceVersion` | `UPDATE … WHERE run_id = $x` | `If-Match` / `If-None-Match` (S3, since 2024), generation preconditions (GCS) |
| **append-only log** (thread, conversation history, audit) | does not fit; delegated to object storage | a table | one object per entry |
| **rebuildable index** (board queries and ordering) | informer cache, indexed in memory | SQL | built in memory from a list |

The kubernetes driver uses object storage for its log. A separate `objectstore` driver that uses object storage for everything is possible later.

## Drivers

### `kubernetes` (default, first)
- `Task` and `Team` are CRDs; their status holds control state only: phase, `run_id`, `step`, lease, outbox, pointers.
- Thread, conversation history, run history and files live in object storage (S3, GCS, or any S3-compatible store that enforces conditional writes; see below). One object per entry; the sequence number is claimed with `If-None-Match`, and the Task's `thread.revision` is advanced afterwards so watchers notice.
- Board queries run in memory: the server caches every Task through an informer and builds indexes for ordering and filtering. Argo CD works the same way.
- Fits thousands to tens of thousands of tasks per installation. Limits: about 1.5 MB per etcd object, about 8 GB total by default, a few thousand writes per second per cluster, shared with everything else in the cluster. Finished tasks are archived to object storage after `Team.spec.archiveAfter`, leaving a stub in the index.
- It is the first driver built, on purpose: it is the most constrained of the three, so building it first keeps the core from growing queries only SQL can answer.

### `postgres` (large installations)
- Full SQL queries, high write volume, archival of old data.
- The database is created by the operator: a CloudNativePG `Cluster` with `ScheduledBackup`, a cloud-managed instance through Crossplane, GCP Config Connector or AWS ACK, or simply a connection string in a Secret.
- Schema migrations are driven by the operator: a version change runs the migration Job first, then the rolling update. No advisory locks needed.
- Only common Postgres features (`SKIP LOCKED` is fine). No `LISTEN/NOTIFY`, no advisory locks, so every Postgres-compatible service works.

## Object store requirements

The log relies on `If-None-Match: *` being **enforced** on PutObject: two appenders racing for the same key must get exactly one success and one `412 Precondition Failed`. A store that accepts the header and ignores it loses entries silently, which is worse than a store that rejects the header.

- At startup the driver probes the configured store: it writes a unique scratch key twice with `If-None-Match: *`, requires the second write to fail with 412, then deletes the key. If the probe fails the driver refuses to start.
- Known to enforce the precondition: Amazon S3 (since 2024), Google Cloud Storage (generation preconditions), SeaweedFS, versitygw, LocalStack.
- Known not to enforce it: Garage (accepts the header and overwrites). Do not use it.
- Development and CI use SeaweedFS, pinned to a release tag.

Two more rules keep the log honest on every store:

- **A 412 is only "taken" if the entry exists.** A store backed by a filesystem (SeaweedFS) also answers 412 when the key is the directory of a nested stream. The driver checks the object after a 412 and reports a collision as an error, never as "already exists", because an idempotent caller treats "already exists" as done and would lose the entry.
- **No stream is nested under another stream's entry.** Stream `ns/task/runs` with entry `<runId>` rules out a stream `ns/task/runs/<runId>/...`, since a filesystem cannot hold a file and a directory of the same name. Streams in use: `ns/task/thread` (entries by sequence number), `ns/task/runs` (entries by run id) and `ns/task/events/<runId>` (entries by sequence number).

## StorageProvider

```yaml
apiVersion: cadre.dev/v1alpha1
kind: StorageProvider
metadata: { name: default }
spec:
  driver: kubernetes            # kubernetes | postgres
  blobs:                        # large content always goes here
    s3: { bucket: cadre, secretRef: { name: s3, key: credentials } }
  # postgres:
  #   managed: { provider: cnpg, instances: 3, storage: 20Gi }
  #   external: { secretRef: { name: my-db, key: url } }
```

## Core constraints: no multi-object transactions

etcd and object storage offer conditional writes on **single objects** only, so the core is written to that constraint from day one:

1. **One transition changes one object.** The Task is an aggregate; every transition writes only that Task. The one exception is the Team semaphore: a run's slot is taken in the Team before the Task claim and given back after the Task write that ends the run. Slots are a set of run ids, so taking and giving back are idempotent. A slot that is not given back is never lost: the run's end is also written to the Task's outbox, and the dispatcher gives the slot back before it acks the item. A slot taken by a claim that never landed is removed by a reconciler, which decides on fresh reads of the named Task only, never on the index, and only once the slot is older than twice the lease.
2. **Side effects live inside the object.** "Create a subtask", "notify", "dispatch a run" are written into the Task's embedded outbox together with the state change. A dispatcher applies them idempotently and clears them with another conditional write.
3. **Indexes are always rebuildable.** Board indexes are caches, rebuilt from a list on restart. The Team slot set is not a cache: it is repaired from fresh reads of the Tasks it names, never rebuilt from an index.
4. **Audit history is embedded.** A deliverable's history is an append-only list inside its object, written with its state; it moves to the log when it grows.
5. **Numbering needs no coordination.** Internal ids are ULIDs. The human-facing `#123` is claimed from the Team's counter by conditional write.

## Conformance tests

Every driver must pass the same suite, or one of them will silently misbehave:

- Two parties conditionally write the same object at once; exactly one succeeds.
- Re-delivering the same step advances state exactly once.
- Two parties claim the same work item; exactly one gets it.
- The process is killed and restarted; the index rebuilds completely.
- Outbox items are reprocessed after the dispatcher dies, and take effect exactly once.
- Two managers run at once under chaos (kills, restarts, delayed writes); no task ever runs twice.
