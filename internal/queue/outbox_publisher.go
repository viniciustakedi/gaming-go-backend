package queue

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/outbox"
)

type outboxPublisher struct{ queues *Queues }

// NewOutboxPublisher adapts the publisher-role SQS client to outbox.Queue.
func NewOutboxPublisher(queues *Queues) outbox.Queue { return outboxPublisher{queues: queues} }

func (p outboxPublisher) Send(ctx context.Context, payload []byte, groupID, deduplicationID string) error {
	_, err := p.queues.Publisher.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queues.OutputURL),
		MessageBody:            aws.String(string(payload)),
		MessageGroupId:         aws.String(groupID),
		MessageDeduplicationId: aws.String(deduplicationID),
	})
	return err
}
