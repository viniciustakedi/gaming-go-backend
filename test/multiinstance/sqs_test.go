//go:build multiinstance

package multiinstance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// This file holds only what test/testclient's shared SQS helpers cannot: the
// two credential roles this package's scenarios authenticate as directly
// (gateway, events-reader) and the FIFO envelope/reading calls built on top
// of them (spec, seam 3: "o mesmo cliente de teste roda em dois harnesses").
// The SQS client, queue URL/naming and envelope-building logic itself lives
// in test/testclient and is shared with test/integration's own SQS helpers
// (iam_test.go, sqs_consumer_test.go) - see this ticket's Comments for the
// extraction.

// sqsTestCreds holds only the two roles this package's scenarios need to
// authenticate as directly: gateway, to publish wager requests the way a
// provider's own gateway would, and events-reader, to observe confirmed
// outbox deliveries on wallet-events.fifo. Every instance under test already
// gets its own consumer/publisher credentials through baseEnv.
type sqsTestCreds struct {
	gatewayKey, gatewaySecret           string
	eventsReaderKey, eventsReaderSecret string
}

func testCredentialsFile() string {
	return repoPath("deploy", "ministack", ".runtime", "test-credentials.env")
}

func loadSQSTestCreds(t *testing.T) sqsTestCreds {
	t.Helper()
	values, err := envfile.Read(testCredentialsFile())
	if err != nil {
		t.Fatalf("%v - run scripts/wait-for-integration.sh first", err)
	}
	get := func(key string) string {
		v := values[key]
		if v == "" {
			t.Fatalf("missing %s in %s - run scripts/wait-for-integration.sh first", key, testCredentialsFile())
		}
		return v
	}
	return sqsTestCreds{
		gatewayKey:         get("GATEWAY_ACCESS_KEY_ID"),
		gatewaySecret:      get("GATEWAY_SECRET_ACCESS_KEY"),
		eventsReaderKey:    get("EVENTS_READER_ACCESS_KEY_ID"),
		eventsReaderSecret: get("EVENTS_READER_SECRET_ACCESS_KEY"),
	}
}

func newSQSClient(t *testing.T, accessKeyID, secretAccessKey string) *sqs.Client {
	t.Helper()
	return testclient.NewSQSClient(t, accessKeyID, secretAccessKey)
}

func sqsQueueURL(name string) string { return testclient.QueueURL(name) }

func inputQueueName() string  { return testclient.InputQueueName() }
func outputQueueName() string { return testclient.OutputQueueName() }

// eventEnvelopeJSON is test/testclient's shared minimal outbox envelope
// shape.
type eventEnvelopeJSON = testclient.EventEnvelope

// sqsWagerEnvelope builds the WagerTransactionRequested envelope the
// consumer (internal/consumer) decodes, stamped with the real wall-clock
// time this package's scenarios send at (test/integration instead uses a
// fixed timestamp - occurredAt is asserted by neither).
func sqsWagerEnvelope(t *testing.T, messageID string, in wageringBodyInput, key string) []byte {
	t.Helper()
	return testclient.SQSWagerEnvelope(t, messageID, in, key, time.Now().UTC().Format(time.RFC3339))
}

func sendWagerMessage(t *testing.T, client *sqs.Client, group, dedup string, body []byte) {
	t.Helper()
	testclient.SendWagerMessage(t, client, group, dedup, body)
}

// drainOutputEvents receives from wallet-events.fifo as events-reader until
// deadline, deleting every message it looks at and returning every distinct
// eventId observed mapped to how many times it was delivered - the
// independent count a dedup-by-eventId assertion needs, since a republished
// event can legitimately arrive more than once outside the FIFO's five
// minute deduplication window (spec, "Outbox e eventos").
func drainOutputEvents(t *testing.T, deadline time.Time, want map[string]bool) map[string]int {
	t.Helper()
	creds := loadSQSTestCreds(t)
	reader := newSQSClient(t, creds.eventsReaderKey, creds.eventsReaderSecret)
	queueURL := sqsQueueURL(outputQueueName())
	seen := make(map[string]int)
	remaining := len(want)
	for remaining > 0 && time.Now().Before(deadline) {
		out, err := reader.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		})
		if err != nil {
			t.Fatalf("receive published outbox events as events-reader: %v", err)
		}
		for _, message := range out.Messages {
			if message.Body == nil {
				continue
			}
			var envelope eventEnvelopeJSON
			if err := json.Unmarshal([]byte(*message.Body), &envelope); err != nil {
				t.Fatalf("decode published event envelope: %v", err)
			}
			if _, ok := want[envelope.EventID]; ok {
				if seen[envelope.EventID] == 0 {
					remaining--
				}
				seen[envelope.EventID]++
			}
			if _, err := reader.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle}); err != nil {
				t.Fatalf("delete observed event: %v", err)
			}
		}
	}
	return seen
}
