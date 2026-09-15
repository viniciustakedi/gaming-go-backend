package operation

// Attempt holds the stored identity needed to classify a submission attempt.
type Attempt struct {
	IdempotencyKey        string
	PayloadHash           string
	ExternalTransactionID string
}

// AttemptKind is whether a submission is new, a replay, or a conflict.
type AttemptKind string

const (
	NewAttempt      AttemptKind = "NEW"
	Replay          AttemptKind = "REPLAY"
	AttemptConflict AttemptKind = "CONFLICT"
)

// AttemptDecision is the idempotency outcome before any new movement is persisted.
type AttemptDecision struct {
	Kind  AttemptKind
	Error *Error
}

// ClassifyAttempt classifies an incoming attempt against records found by idempotency key and external ID.
// A reused idempotency key takes precedence when both lookups conflict; a matching key hash remains a replay.
func ClassifyAttempt(incoming Attempt, existingByKey, existingByExternal *Attempt) AttemptDecision {
	if existingByKey != nil {
		if existingByKey.PayloadHash == incoming.PayloadHash {
			return AttemptDecision{Kind: Replay}
		}
		return AttemptDecision{Kind: AttemptConflict, Error: ErrIdempotencyKeyReused}
	}
	if existingByExternal != nil && existingByExternal.IdempotencyKey != incoming.IdempotencyKey {
		return AttemptDecision{Kind: AttemptConflict, Error: ErrExternalTransactionIDConflict}
	}
	return AttemptDecision{Kind: NewAttempt}
}
