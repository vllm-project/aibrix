# apps/console/api/planner

Async batch scheduling between the Console BFF and the metadata service (MDS).
Console enqueues a job; the planner submits it to MDS later and serves a
merged read view back to Console.

## Public surface

Console imports `planner/api` only. The interface is:

The package's full external contract is one Go interface with five
methods, all defined in `api/interface.go`:

| Method                              | Audience       | Returns                       | Purpose                                                                            |
| ----------------------------------- | -------------- | ----------------------------- | ---------------------------------------------------------------------------------- |
| `Planner.Enqueue`                   | Console BFF    | `*EnqueueResult`              | Accept a new job; return a queued view immediately.                                |
| `Planner.GetJob`                    | Console BFF    | `*JobView`                    | Single merged planner + MDS read.                                                  |
| `Planner.ListJobs`                  | Console BFF    | `*ListJobsResponse`           | Paginated `JobView` list (cursor-based).                                           |
| `Planner.GetQueueStats`             | ops dashboards | `*QueueStatsView`             | Planner-internal queue depth & worker activity (`queued` / `claimed` / retryable). |
| `Planner.GetProvisionResourceStats` | ops dashboards | `*ProvisionResourceStatsView` | Planner demand-side resource accounting (`Total` / `InUse`).                       |




## Status

MVP. The interfaces and request/response types are in place; concrete
implementations (in-memory store, MDS-backed `BatchClient`, Worker loop,
`Planner` impl) land in follow-up PRs.
