package consumer

import "testing"

// The public boundary exercised by the integration suite is the input SQS
// queue. This small red/green test protects the transport-independent part
// of that boundary: an envelope must carry the idempotency key that lets an
// identical HTTP operation and SQS operation classify as one attempt.
func TestDecodeEnvelope_RequiresIdempotencyKey(t *testing.T) {
	_, err := decodeEnvelope([]byte(`{"type":"WagerTransactionRequested","messageId":"m-1","data":{"providerId":"provider-a"}}`))
	if err == nil {
		t.Fatal("decodeEnvelope() error = nil, want malformed envelope")
	}
}
