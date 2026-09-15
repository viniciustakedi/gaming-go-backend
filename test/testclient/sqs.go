//go:build integration || multiinstance

package testclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// SQSEndpoint, SQSRegion and SQSAccountID are the MiniStack connection
// defaults both harnesses share (spec, seam 3: "o mesmo cliente de teste
// roda em dois harnesses").
func SQSEndpoint() string { return EnvOrDefault("SQS_ENDPOINT_URL", "http://localhost:4566") }

func SQSRegion() string { return EnvOrDefault("SQS_REGION", "us-east-1") }

func SQSAccountID() string { return EnvOrDefault("MINISTACK_ACCOUNT_ID", "000000000000") }

// InputQueueName and OutputQueueName name the two production FIFOs both
// harnesses send to or read from.
func InputQueueName() string { return EnvOrDefault("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo") }

func OutputQueueName() string { return EnvOrDefault("SQS_OUTPUT_QUEUE_NAME", "wallet-events.fifo") }

// QueueURL mirrors MiniStack's own stable, documented URL shape,
// {endpoint}/{accountId}/{queueName} - not every role has GetQueueUrl on
// every queue it needs to address.
func QueueURL(name string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(SQSEndpoint(), "/"), SQSAccountID(), name)
}

// NewSQSClient builds an SQS client authenticated as one static credential
// pair against SQSEndpoint/SQSRegion.
func NewSQSClient(t testing.TB, accessKeyID, secretAccessKey string) *sqs.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(SQSRegion()),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	)
	if err != nil {
		t.Fatalf("build sqs client: %v", err)
	}
	endpoint := SQSEndpoint()
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

// EventEnvelope mirrors internal/domain/wallet's own outbox envelope - the
// minimal shape both harnesses need to decode a published event's eventId
// and identity fields off the output queue.
type EventEnvelope struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	CausationID   string `json:"causationId"`
}

// SQSWagerEnvelope builds the WagerTransactionRequested envelope the
// consumer (internal/consumer) decodes. occurredAt is a caller-supplied
// RFC3339 timestamp rather than always time.Now(): it is not asserted on by
// either harness, so each keeps whatever value its own tests were already
// written against.
func SQSWagerEnvelope(t testing.TB, messageID string, in WageringBodyInput, key, occurredAt string) []byte {
	t.Helper()
	data := map[string]any{
		"providerId": in.ProviderID, "externalTransactionId": in.ExternalID, "idempotencyKey": key,
		"playerId": in.PlayerID, "walletId": in.WalletID, "roundId": in.RoundID, "gameId": in.GameID,
		"kind": in.Kind, "money": map[string]string{"amount": in.Amount, "currency": in.Currency},
	}
	if in.ReferenceID != nil {
		data["referenceExternalTransactionId"] = *in.ReferenceID
	}
	payload := map[string]any{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": occurredAt, "data": data}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal SQS wager envelope: %v", err)
	}
	return encoded
}

// SendWagerMessage publishes body to the input FIFO's group/dedup pair, the
// way a real gateway would.
func SendWagerMessage(t testing.TB, client *sqs.Client, group, dedup string, body []byte) {
	t.Helper()
	_, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(QueueURL(InputQueueName())), MessageBody: aws.String(string(body)),
		MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(dedup),
	})
	if err != nil {
		t.Fatalf("gateway sends wager message: %v", err)
	}
}
