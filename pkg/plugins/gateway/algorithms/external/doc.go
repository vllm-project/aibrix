/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

// Package external implements the optional HTTP-based replica-selection
// strategy. The remote service chooses only from a bounded candidate snapshot;
// Gateway retains discovery, target validation, admission, accounting, and the
// final Pod IP/port mutation.
package external
