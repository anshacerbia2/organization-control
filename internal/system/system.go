// Package system holds this service's own published identity.
//
// One constant, in one place, on purpose. Every aggregate in this repository publishes events, and
// the source is the same fact for all of them: which system produced the event. Declared per
// package it would be the same string written twice, and the failure mode of a duplicated identity
// is not a compile error — it is two sources appearing in a consumer's stream for one system, with
// nothing in this repository disagreeing out loud.
package system

import "github.com/anshacerbia2/foundation-platform/event"

// Source names this system in every envelope it publishes.
//
// An absolute-path URI reference, which `event.ParseSource` requires. A CloudEvents source is a
// URI-reference, and a bare name is a relative one that a consumer resolving it against its own
// base would resolve differently — so two consumers could disagree about who sent the event.
//
// The path names the system, not the repository and not the deployment. A consumer keys its
// projection on this value, so it must survive a rename of either.
const Source event.Source = "/systems/organization-control"
