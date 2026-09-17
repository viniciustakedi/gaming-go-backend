// Package queue owns the SQS clients the application uses at runtime. It
// never touches MiniStack's root credentials: it builds one client per IAM
// role the application plays (consumer, publisher), each signed with that
// role's own key, because unsigned or root requests bypass every MiniStack
// policy.
package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/health"
)

// Queues holds the role-scoped clients and the queue URLs resolved from
// them at start. The URLs stay empty until the OnStart hook below fills
// them in; every component that reads them depends on this one starting.
type Queues struct {
	Consumer  *sqs.Client
	Publisher *sqs.Client

	InputQueueName  string
	OutputQueueName string
	DLQQueueName    string

	InputURL  string
	OutputURL string
	DLQURL    string
}

// New builds the consumer and publisher clients. It never falls back to
// ambient AWS credentials: each client is pinned to the role credentials
// config.Load validated, so a misconfigured environment fails loudly
// instead of picking up whatever credentials happen to be around.
func New(cfg config.Config) (*Queues, error) {
	consumer, err := newRoleClient(cfg.SQS, cfg.SQS.ConsumerAccessKeyID, cfg.SQS.ConsumerSecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("queue: build consumer client: %w", err)
	}
	publisher, err := newRoleClient(cfg.SQS, cfg.SQS.PublisherAccessKeyID, cfg.SQS.PublisherSecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("queue: build publisher client: %w", err)
	}
	return &Queues{
		Consumer:        consumer,
		Publisher:       publisher,
		InputQueueName:  cfg.SQS.InputQueueName,
		OutputQueueName: cfg.SQS.OutputQueueName,
		DLQQueueName:    cfg.SQS.DLQQueueName,
	}, nil
}

func newRoleClient(sqsCfg config.SQSConfig, accessKeyID, secretAccessKey string) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(sqsCfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	)
	if err != nil {
		return nil, err
	}
	endpoint := sqsCfg.EndpointURL
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	}), nil
}

// RegisterLifecycle resolves the input, output and DLQ queue URLs on start.
// GetQueueUrl doubles as the reachability and IAM probe: an unreachable
// MiniStack or a denied principal fails start before the process is ready.
// The DLQ is resolved by the consumer client, the same role that later
// sends the permanent-failure envelope to it.
func RegisterLifecycle(lc fx.Lifecycle, q *Queues, cfg config.Config, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, cfg.SQS.StartupTimeout)
			defer cancel()

			inURL, err := q.Consumer.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.InputQueueName)})
			if err != nil {
				return fmt.Errorf("queue: resolve input queue url: %w", err)
			}
			q.InputURL = aws.ToString(inURL.QueueUrl)

			outURL, err := q.Publisher.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.OutputQueueName)})
			if err != nil {
				return fmt.Errorf("queue: resolve output queue url: %w", err)
			}
			q.OutputURL = aws.ToString(outURL.QueueUrl)

			dlqURL, err := q.Consumer.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.DLQQueueName)})
			if err != nil {
				return fmt.Errorf("queue: resolve dlq url: %w", err)
			}
			q.DLQURL = aws.ToString(dlqURL.QueueUrl)

			logger.Info("sqs queues resolved", "inputQueueUrl", q.InputURL, "outputQueueUrl", q.OutputURL, "dlqQueueUrl", q.DLQURL)
			return nil
		},
	})
}

func readinessCheck(q *Queues) health.Named {
	return health.Named{
		Name: "sqs",
		Check: func(ctx context.Context) error {
			if q.InputURL == "" {
				return fmt.Errorf("sqs: input queue url not resolved yet")
			}
			_, err := q.Consumer.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
				QueueUrl:       aws.String(q.InputURL),
				AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
			})
			return err
		},
	}
}
