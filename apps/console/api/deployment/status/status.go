package deploymentstatus

// Canonical deployment lifecycle states exposed by the Console backend.
//
// State transitions:
//
//	+----------+
//	|  Create  |
//	+----------+
//	     |
//	     v
//	+-----------+   observe partial progress   +---------+
//	| Deploying | ---------------------------> | Scaling |
//	+-----------+                              +---------+
//	     ^                                          |
//	     |                                          | enough ready replicas
//	     | reconcile still pending                  v
//	     +------------------------------------- +-------+
//	                                           | Ready |
//	                                           +-------+
//
//	Exceptional transitions:
//
//	+-----------+     progress deadline / missing resource     +--------+
//	| Deploying | -------------------------------------------> | Failed |
//	+-----------+                                             +--------+
//
//	+---------+       progress deadline / missing resource     +--------+
//	| Scaling | ---------------------------------------------> | Failed |
//	+---------+                                               +--------+
//
//	+-------+         service missing                          +----------+
//	| Ready | -----------------------------------------------> | Degraded |
//	+-------+                                                 +----------+
//
//	+-----------+     provider resource gone                   +---------+
//	| Any state | -------------------------------------------> | Deleted |
//	+-----------+                                             +---------+
const (
	StatusReady     = "Ready"
	StatusDeploying = "Deploying"
	StatusScaling   = "Scaling"
	StatusFailed    = "Failed"
	StatusDegraded  = "Degraded"
	StatusDeleted   = "Deleted"
)
