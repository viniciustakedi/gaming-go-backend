package consumer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/queue"
)

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

func TestConsumerStopSharesPostCancelDrainBudget(t *testing.T) {
	const (
		shutdownTimeout = 100 * time.Millisecond
		postCancelDrain = 200 * time.Millisecond
		margin          = 50 * time.Millisecond
	)

	releaseStarted := make(chan struct{})
	unblockRelease := make(chan struct{})
	var releaseStartOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releaseStartOnce.Do(func() { close(releaseStarted) })
		<-unblockRelease
	}))
	t.Cleanup(func() {
		close(unblockRelease)
		server.Close()
	})

	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	consumer := &Consumer{
		queues: &queue.Queues{
			Consumer: sqs.NewFromConfig(awsCfg, func(options *sqs.Options) {
				options.BaseEndpoint = aws.String(server.URL)
			}),
			InputURL: "http://queue.test/input",
		},
		cfg:        config.SQSConsumerConfig{ShutdownTimeout: shutdownTimeout},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cancel:     func() {},
		workCancel: func() {},
		done:       make(chan struct{}), // A worker that never unwinds.
		active: map[string]types.Message{
			"receipt": {ReceiptHandle: aws.String("receipt")},
		},
		postCancelDrain: postCancelDrain,
	}

	started := time.Now()
	err := consumer.stop(context.Background())
	elapsed := time.Since(started)
	select {
	case <-releaseStarted:
	default:
		t.Fatal("stop() did not release the active message")
	}
	if err == nil {
		t.Fatal("stop() error = nil, want cancellation deadline error")
	}
	if limit := shutdownTimeout + postCancelDrain + margin; elapsed > limit {
		t.Fatalf("stop() took %s, want at most %s with one post-cancel drain", elapsed, limit)
	}
}
