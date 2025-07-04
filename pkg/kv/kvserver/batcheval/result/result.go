// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package result

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/kr/pretty"
)

// LocalResult is data belonging to an evaluated command that is
// only used on the node on which the command was proposed. Note that
// the proposing node may die before the local results are processed,
// so any side effects here are only best-effort.
type LocalResult struct {
	Reply *kvpb.BatchResponse

	// EncounteredIntents stores any intents from other transactions that the
	// request encountered but did not conflict with. They should be handed off
	// to asynchronous intent processing on the proposer, so that an attempt to
	// resolve them is made.
	EncounteredIntents []roachpb.Intent
	// AcquiredLocks stores any newly acquired or re-acquired locks.
	AcquiredLocks []roachpb.LockAcquisition
	// ResolvedLocks stores any resolved lock spans, either with finalized or
	// pending statuses. Unlike AcquiredLocks and EncounteredIntents, values in
	// this slice will represent spans of locks that were resolved.
	ResolvedLocks []roachpb.LockUpdate
	// UpdatedTxns stores transaction records that have been updated by
	// calls to EndTxn, PushTxn, and RecoverTxn.
	UpdatedTxns []*roachpb.Transaction
	// ReportedMissingLocks stores lock acquisition structs that represent locks
	// that have been reported as missing via QueryIntent. Such locks must be
	// reported to the concurrency manager.
	ReportedMissingLocks []roachpb.LockAcquisition
	// EndTxns stores completed transactions. If the transaction
	// contains unresolved intents, they should be handed off for
	// asynchronous intent resolution. A bool in each EndTxnIntents
	// indicates whether or not the intents must be left alone if the
	// corresponding command/proposal didn't succeed. For example,
	// resolving intents of a committing txn should not happen if the
	// commit fails, or we may accidentally make uncommitted values
	// live.
	EndTxns []EndTxnIntents
	// PopulateBarrierResponse will populate a BarrierResponse with the lease
	// applied index and range descriptor when applied.
	PopulateBarrierResponse bool

	// RepopulateRequestResponse will overwrite the SubsumeResponse's
	// LeaseAppliedIndex field with the lease applied index of the
	// SubsumeRequest itself.
	RepopulateSubsumeResponseLAI bool

	// When set (in which case we better be the first range), call
	// GossipFirstRange if the Replica holds the lease.
	GossipFirstRange bool
	// Call MaybeGossipSystemConfig.
	MaybeGossipSystemConfig bool
	// Call MaybeGossipSystemConfigIfHaveFailure
	MaybeGossipSystemConfigIfHaveFailure bool
	// Call MaybeAddToSplitQueue.
	MaybeAddToSplitQueue bool
	// Call MaybeGossipNodeLiveness with the specified Span, if set.
	MaybeGossipNodeLiveness *roachpb.Span

	// Metrics contains counters which are to be passed to the
	// metrics subsystem.
	Metrics *Metrics
}

// IsZero reports whether lResult is the zero value.
func (lResult *LocalResult) IsZero() bool {
	// NB: keep in order.
	return lResult.Reply == nil &&
		lResult.EncounteredIntents == nil &&
		lResult.AcquiredLocks == nil &&
		lResult.ResolvedLocks == nil &&
		lResult.UpdatedTxns == nil &&
		lResult.EndTxns == nil &&
		!lResult.PopulateBarrierResponse &&
		!lResult.RepopulateSubsumeResponseLAI &&
		!lResult.GossipFirstRange &&
		!lResult.MaybeGossipSystemConfig &&
		!lResult.MaybeGossipSystemConfigIfHaveFailure &&
		lResult.MaybeGossipNodeLiveness == nil &&
		lResult.Metrics == nil
}

func (lResult *LocalResult) String() string {
	if lResult == nil {
		return "LocalResult: nil"
	}
	return fmt.Sprintf("LocalResult (reply: %v, "+
		"#encountered intents: %d, #acquired locks: %d, #resolved locks: %d"+
		"#updated txns: %d #end txns: %d, "+
		"PopulateBarrierResponse:%t RepopulateSubsumeResponse:%t "+
		"GossipFirstRange:%t MaybeGossipSystemConfig:%t "+
		"MaybeGossipSystemConfigIfHaveFailure:%t MaybeAddToSplitQueue:%t "+
		"MaybeGossipNodeLiveness:%s ",
		lResult.Reply,
		len(lResult.EncounteredIntents), len(lResult.AcquiredLocks), len(lResult.ResolvedLocks),
		len(lResult.UpdatedTxns), len(lResult.EndTxns),
		lResult.PopulateBarrierResponse, lResult.RepopulateSubsumeResponseLAI,
		lResult.GossipFirstRange, lResult.MaybeGossipSystemConfig,
		lResult.MaybeGossipSystemConfigIfHaveFailure, lResult.MaybeAddToSplitQueue,
		lResult.MaybeGossipNodeLiveness)
}

// RequiresRaft returns true if the local result needs to go via Raft, e.g. in
// order to apply side effects under Raft.
func (lResult *LocalResult) RequiresRaft() bool {
	// Gossip triggers require raftMu to be held.
	return lResult.MaybeGossipNodeLiveness != nil ||
		lResult.MaybeGossipSystemConfig ||
		lResult.MaybeGossipSystemConfigIfHaveFailure
}

// DetachEncounteredIntents returns (and removes) those encountered
// intents from the LocalEvalResult which are supposed to be handled.
func (lResult *LocalResult) DetachEncounteredIntents() []roachpb.Intent {
	if lResult == nil {
		return nil
	}
	r := lResult.EncounteredIntents
	lResult.EncounteredIntents = nil
	return r
}

// DetachMissingLocks returns (and removes) those locks that have been reported
// missing during an QueryIntentRequest and must be handled.
func (lResult *LocalResult) DetachMissingLocks() []roachpb.LockAcquisition {
	if lResult == nil {
		return nil
	}
	r := lResult.ReportedMissingLocks
	lResult.ReportedMissingLocks = nil
	return r
}

// DetachEndTxns returns (and removes) the EndTxnIntent objects from
// the local result. If alwaysOnly is true, the slice is filtered to
// include only those which have specified returnAlways=true, meaning
// the intents should be resolved regardless of whether the
// EndTxn command succeeded.
func (lResult *LocalResult) DetachEndTxns(alwaysOnly bool) []EndTxnIntents {
	if lResult == nil {
		return nil
	}
	r := lResult.EndTxns
	if alwaysOnly {
		// If alwaysOnly, filter away any !Always EndTxnIntents.
		r = r[:0]
		for _, eti := range lResult.EndTxns {
			if eti.Always {
				r = append(r, eti)
			}
		}
	}
	lResult.EndTxns = nil
	return r
}

// DetachPopulateBarrierResponse returns (and removes) the
// PopulateBarrierResponse value from the local result.
func (lResult *LocalResult) DetachPopulateBarrierResponse() bool {
	if lResult == nil {
		return false
	}
	r := lResult.PopulateBarrierResponse
	lResult.PopulateBarrierResponse = false
	return r
}

// DetachRepopulateSubsumeResponse returns (and removes) the
// RepopulateSubsumeResponse value from the local result.
func (lResult *LocalResult) DetachRepopulateSubsumeResponse() bool {
	if lResult == nil {
		return false
	}
	r := lResult.RepopulateSubsumeResponseLAI
	lResult.RepopulateSubsumeResponseLAI = false
	return r
}

// Result is the result of evaluating a KV request. That is, the
// proposer (which holds the lease, at least in the case in which the command
// will complete successfully) has evaluated the request and is holding on to:
//
// a) changes to be written to disk when applying the command
// b) changes to the state which may require special handling (i.e. code
//
//	execution) on all Replicas
//
// c) data which isn't sent to the followers but the proposer needs for tasks
//
//	it must run when the command has applied (such as resolving intents).
type Result struct {
	Local        LocalResult
	Replicated   kvserverpb.ReplicatedEvalResult
	WriteBatch   *kvserverpb.WriteBatch
	LogicalOpLog *kvserverpb.LogicalOpLog
}

// IsZero reports whether p is the zero value.
func (p *Result) IsZero() bool {
	if !p.Local.IsZero() {
		return false
	}
	if !p.Replicated.IsZero() {
		return false
	}
	if p.WriteBatch != nil {
		return false
	}
	if p.LogicalOpLog != nil {
		return false
	}
	return true
}

// coalesceBool ORs rhs into lhs and then zeroes rhs.
func coalesceBool(lhs *bool, rhs *bool) {
	*lhs = *lhs || *rhs
	*rhs = false
}

// MergeAndDestroy absorbs the supplied EvalResult while validating that the
// resulting EvalResult makes sense. For example, it is forbidden to absorb
// two lease updates or log truncations, or multiple splits and/or merges.
//
// The passed EvalResult must not be used once passed to Merge.
func (p *Result) MergeAndDestroy(q Result) error {
	if q.Replicated.State != nil {
		if q.Replicated.State.RaftAppliedIndex != 0 {
			return errors.AssertionFailedf("must not specify RaftAppliedIndex")
		}
		if q.Replicated.State.LeaseAppliedIndex != 0 {
			return errors.AssertionFailedf("must not specify LeaseAppliedIndex")
		}
		if p.Replicated.State == nil {
			p.Replicated.State = &kvserverpb.ReplicaState{}
		}
		if q.Replicated.State.RaftAppliedIndexTerm != 0 {
			return errors.AssertionFailedf("must not specify RaftAppliedIndexTerm")
		}
		if p.Replicated.State.Desc == nil {
			p.Replicated.State.Desc = q.Replicated.State.Desc
		} else if q.Replicated.State.Desc != nil {
			return errors.AssertionFailedf("conflicting RangeDescriptor")
		}
		q.Replicated.State.Desc = nil

		if p.Replicated.State.Lease == nil {
			p.Replicated.State.Lease = q.Replicated.State.Lease
		} else if q.Replicated.State.Lease != nil {
			return errors.AssertionFailedf("conflicting Lease")
		}
		q.Replicated.State.Lease = nil

		if p.Replicated.State.TruncatedState == nil {
			p.Replicated.State.TruncatedState = q.Replicated.State.TruncatedState
			p.Replicated.RaftExpectedFirstIndex = q.Replicated.RaftExpectedFirstIndex
		} else if q.Replicated.State.TruncatedState != nil {
			return errors.AssertionFailedf("conflicting TruncatedState")
		}
		q.Replicated.State.TruncatedState = nil

		if q.Replicated.State.GCThreshold != nil {
			if p.Replicated.State.GCThreshold == nil {
				p.Replicated.State.GCThreshold = q.Replicated.State.GCThreshold
			} else {
				p.Replicated.State.GCThreshold.Forward(*q.Replicated.State.GCThreshold)
			}
			q.Replicated.State.GCThreshold = nil
		}

		if p.Replicated.State.GCHint == nil {
			p.Replicated.State.GCHint = q.Replicated.State.GCHint
		} else if q.Replicated.State.GCHint != nil {
			return errors.AssertionFailedf("conflicting GC hint")
		}
		q.Replicated.State.GCHint = nil

		if p.Replicated.State.Version == nil {
			p.Replicated.State.Version = q.Replicated.State.Version
		} else if q.Replicated.State.Version != nil {
			return errors.AssertionFailedf("conflicting Version")
		}
		q.Replicated.State.Version = nil

		if q.Replicated.State.Stats != nil {
			return errors.AssertionFailedf("must not specify Stats")
		}
		if q.Replicated.State.ForceFlushIndex != (roachpb.ForceFlushIndex{}) {
			return errors.AssertionFailedf("must not specify ForceFlushIndex")
		}
		if (*q.Replicated.State != kvserverpb.ReplicaState{}) {
			log.Fatalf(context.TODO(), "unhandled EvalResult: %s",
				pretty.Diff(*q.Replicated.State, kvserverpb.ReplicaState{}))
		}
		q.Replicated.State = nil
	}

	if p.Replicated.RaftTruncatedState == nil {
		p.Replicated.RaftTruncatedState = q.Replicated.RaftTruncatedState
		p.Replicated.RaftExpectedFirstIndex = q.Replicated.RaftExpectedFirstIndex
	} else if q.Replicated.RaftTruncatedState != nil {
		return errors.AssertionFailedf("conflicting RaftTruncatedState")
	}
	q.Replicated.RaftTruncatedState = nil
	q.Replicated.RaftExpectedFirstIndex = 0

	if p.Replicated.State != nil && p.Replicated.State.TruncatedState != nil &&
		p.Replicated.RaftTruncatedState != nil {
		return errors.AssertionFailedf("conflicting RaftTruncatedState")
	}

	if p.Replicated.Split == nil {
		p.Replicated.Split = q.Replicated.Split
	} else if q.Replicated.Split != nil {
		return errors.AssertionFailedf("conflicting Split")
	}
	q.Replicated.Split = nil

	if p.Replicated.Merge == nil {
		p.Replicated.Merge = q.Replicated.Merge
	} else if q.Replicated.Merge != nil {
		return errors.AssertionFailedf("conflicting Merge")
	}
	q.Replicated.Merge = nil

	if p.Replicated.ChangeReplicas == nil {
		p.Replicated.ChangeReplicas = q.Replicated.ChangeReplicas
	} else if q.Replicated.ChangeReplicas != nil {
		return errors.AssertionFailedf("conflicting ChangeReplicas")
	}
	q.Replicated.ChangeReplicas = nil

	if p.Replicated.ComputeChecksum == nil {
		p.Replicated.ComputeChecksum = q.Replicated.ComputeChecksum
	} else if q.Replicated.ComputeChecksum != nil {
		return errors.AssertionFailedf("conflicting ComputeChecksum")
	}
	q.Replicated.ComputeChecksum = nil

	if p.Replicated.RaftLogDelta == 0 {
		p.Replicated.RaftLogDelta = q.Replicated.RaftLogDelta
	} else if q.Replicated.RaftLogDelta != 0 {
		return errors.AssertionFailedf("conflicting RaftLogDelta")
	}
	q.Replicated.RaftLogDelta = 0

	if p.Replicated.AddSSTable == nil {
		p.Replicated.AddSSTable = q.Replicated.AddSSTable
	} else if q.Replicated.AddSSTable != nil {
		return errors.AssertionFailedf("conflicting AddSSTable")
	}
	q.Replicated.AddSSTable = nil

	if p.Replicated.LinkExternalSSTable == nil {
		p.Replicated.LinkExternalSSTable = q.Replicated.LinkExternalSSTable
	} else if q.Replicated.LinkExternalSSTable != nil {
		return errors.AssertionFailedf("conflicting LinkExternalSSTable")
	}
	q.Replicated.LinkExternalSSTable = nil

	if p.Replicated.Excise == nil {
		p.Replicated.Excise = q.Replicated.Excise
	} else if q.Replicated.Excise != nil {
		return errors.AssertionFailedf("conflicting Excise")
	}
	q.Replicated.Excise = nil

	if p.Replicated.MVCCHistoryMutation == nil {
		p.Replicated.MVCCHistoryMutation = q.Replicated.MVCCHistoryMutation
	} else if q.Replicated.MVCCHistoryMutation != nil {
		p.Replicated.MVCCHistoryMutation.Spans = append(p.Replicated.MVCCHistoryMutation.Spans,
			q.Replicated.MVCCHistoryMutation.Spans...)
	}
	q.Replicated.MVCCHistoryMutation = nil

	if p.Replicated.PrevLeaseProposal == nil {
		p.Replicated.PrevLeaseProposal = q.Replicated.PrevLeaseProposal
	} else if q.Replicated.PrevLeaseProposal != nil {
		return errors.AssertionFailedf("conflicting lease expiration")
	}
	q.Replicated.PrevLeaseProposal = nil

	if p.Replicated.PriorReadSummary == nil {
		p.Replicated.PriorReadSummary = q.Replicated.PriorReadSummary
	} else if q.Replicated.PriorReadSummary != nil {
		return errors.AssertionFailedf("conflicting prior read summary")
	}
	q.Replicated.PriorReadSummary = nil

	if !p.Replicated.IsProbe {
		p.Replicated.IsProbe = q.Replicated.IsProbe
	}
	q.Replicated.IsProbe = false

	if q.Replicated.DoTimelyApplicationToAllReplicas {
		p.Replicated.DoTimelyApplicationToAllReplicas = true
	}
	q.Replicated.DoTimelyApplicationToAllReplicas = false

	if p.Local.EncounteredIntents == nil {
		p.Local.EncounteredIntents = q.Local.EncounteredIntents
	} else {
		p.Local.EncounteredIntents = append(p.Local.EncounteredIntents, q.Local.EncounteredIntents...)
	}
	q.Local.EncounteredIntents = nil

	if p.Local.AcquiredLocks == nil {
		p.Local.AcquiredLocks = q.Local.AcquiredLocks
	} else {
		p.Local.AcquiredLocks = append(p.Local.AcquiredLocks, q.Local.AcquiredLocks...)
	}
	q.Local.AcquiredLocks = nil

	if p.Local.ResolvedLocks == nil {
		p.Local.ResolvedLocks = q.Local.ResolvedLocks
	} else {
		p.Local.ResolvedLocks = append(p.Local.ResolvedLocks, q.Local.ResolvedLocks...)
	}
	q.Local.ResolvedLocks = nil

	if p.Local.ReportedMissingLocks == nil {
		p.Local.ReportedMissingLocks = q.Local.ReportedMissingLocks
	} else {
		p.Local.ReportedMissingLocks = append(p.Local.ReportedMissingLocks, q.Local.ReportedMissingLocks...)
	}
	q.Local.ReportedMissingLocks = nil

	if p.Local.UpdatedTxns == nil {
		p.Local.UpdatedTxns = q.Local.UpdatedTxns
	} else {
		p.Local.UpdatedTxns = append(p.Local.UpdatedTxns, q.Local.UpdatedTxns...)
	}
	q.Local.UpdatedTxns = nil

	if p.Local.EndTxns == nil {
		p.Local.EndTxns = q.Local.EndTxns
	} else {
		p.Local.EndTxns = append(p.Local.EndTxns, q.Local.EndTxns...)
	}
	q.Local.EndTxns = nil

	if !p.Local.PopulateBarrierResponse {
		p.Local.PopulateBarrierResponse = q.Local.PopulateBarrierResponse
	} else {
		// PopulateBarrierResponse is only valid for a single Barrier response.
		return errors.AssertionFailedf("multiple PopulateBarrierResponse results")
	}
	q.Local.PopulateBarrierResponse = false

	if !p.Local.RepopulateSubsumeResponseLAI {
		p.Local.RepopulateSubsumeResponseLAI = q.Local.RepopulateSubsumeResponseLAI
	} else {
		// RepopulateSubsumeResponseLAI is only valid for a single Subsume response.
		return errors.AssertionFailedf("multiple RepopulateSubsumeResponseLAI results")
	}
	q.Local.RepopulateSubsumeResponseLAI = false

	if p.Local.MaybeGossipNodeLiveness == nil {
		p.Local.MaybeGossipNodeLiveness = q.Local.MaybeGossipNodeLiveness
	} else if q.Local.MaybeGossipNodeLiveness != nil {
		return errors.AssertionFailedf("conflicting MaybeGossipNodeLiveness")
	}
	q.Local.MaybeGossipNodeLiveness = nil

	coalesceBool(&p.Local.GossipFirstRange, &q.Local.GossipFirstRange)
	coalesceBool(&p.Local.MaybeGossipSystemConfig, &q.Local.MaybeGossipSystemConfig)
	coalesceBool(&p.Local.MaybeGossipSystemConfigIfHaveFailure, &q.Local.MaybeGossipSystemConfigIfHaveFailure)
	coalesceBool(&p.Local.MaybeAddToSplitQueue, &q.Local.MaybeAddToSplitQueue)

	if p.Local.Metrics == nil {
		p.Local.Metrics = q.Local.Metrics
	} else if q.Local.Metrics != nil {
		p.Local.Metrics.Add(*q.Local.Metrics)
	}
	q.Local.Metrics = nil

	if q.LogicalOpLog != nil {
		if p.LogicalOpLog == nil {
			p.LogicalOpLog = q.LogicalOpLog
		} else {
			p.LogicalOpLog.Ops = append(p.LogicalOpLog.Ops, q.LogicalOpLog.Ops...)
		}
	}
	q.LogicalOpLog = nil

	if !q.IsZero() {
		log.Fatalf(context.TODO(), "unhandled EvalResult: %s", pretty.Diff(q, Result{}))
	}

	return nil
}
