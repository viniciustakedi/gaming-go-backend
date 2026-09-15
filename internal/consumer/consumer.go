// Package consumer adapts the trusted internal SQS input queue to the shared
// wagering use case. It owns acknowledgement timing: a message is deleted
// only after the Postgres transaction containing both its inbox row and its
// wallet effect has committed.
package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/backoff"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/money"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/operation"
	domainwallet "github.com/viniciustakedi/jungle-gaming-wallet/internal/domain/wallet"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/queue"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletapp"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/walletpg"
)

const consumerName = "wager-transactions"

type Consumer struct {
	queues  *queue.Queues
	pool    *pgxpool.Pool
	useCase *walletapp.ProcessOperationUseCase
	cfg     config.SQSConsumerConfig
	logger  *slog.Logger
	metrics *metrics

	mu              sync.Mutex
	cancel          context.CancelFunc
	workCancel      context.CancelFunc
	done            chan struct{}
	active          map[string]types.Message
	postCancelDrain time.Duration
}

func New(queues *queue.Queues, pool *pgxpool.Pool, useCase *walletapp.ProcessOperationUseCase, cfg config.Config, logger *slog.Logger, registry *prometheus.Registry) *Consumer {
	return &Consumer{queues: queues, pool: pool, useCase: useCase, cfg: cfg.SQS.Consumer, logger: logger, metrics: newMetrics(registry), active: make(map[string]types.Message), postCancelDrain: config.SQSConsumerPostCancelDrain}
}

func RegisterLifecycle(lc fx.Lifecycle, consumer *Consumer) {
	lc.Append(fx.Hook{OnStart: func(context.Context) error { consumer.start(); return nil }, OnStop: func(ctx context.Context) error { return consumer.stop(ctx) }})
}

func (c *Consumer) start() {
	if !c.cfg.Enabled {
		c.logger.Info("sqs consumer disabled")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	workCtx, workCancel := context.WithCancel(context.Background())
	c.cancel, c.workCancel, c.done = cancel, workCancel, make(chan struct{})
	go func() { defer close(c.done); c.run(ctx, workCtx) }()
}

func (c *Consumer) stop(ctx context.Context) error {
	c.mu.Lock()
	cancel, workCancel, done := c.cancel, c.workCancel, c.done
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	deadline := c.cfg.ShutdownTimeout
	if stopDeadline, ok := ctx.Deadline(); ok && time.Until(stopDeadline) < deadline {
		deadline = time.Until(stopDeadline)
	}
	wait, cancelWait := context.WithTimeout(context.Background(), deadline)
	defer cancelWait()
	select {
	case <-done:
		return nil
	case <-wait.Done():
		workCancel()
		// One post-cancellation window covers both releasing active messages and
		// waiting for workers to finish. The active lock held by releaseActive
		// keeps a worker from untracking before its visibility is reset.
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), c.drainTimeout())
		defer cancelDrain()
		// Hold active's lock while releasing: workers cannot untrack an entry
		// until its visibility has been reset, so shutdown never races a final
		// untrack against the handoff back to SQS.
		c.releaseActive(drainCtx)
		// The Fx context can expire at exactly the moment work is cancelled.
		// Keep the dependency barrier until this same bounded drain window ends:
		// config validates it fits in FX_STOP_TIMEOUT, and returning before done
		// would let Fx close pgx or SQS under a worker still unwinding.
		select {
		case <-done:
			return fmt.Errorf("consumer: stop: %w", wait.Err())
		case <-drainCtx.Done():
			return fmt.Errorf("consumer: stop: cancelled workers: %w", drainCtx.Err())
		}
	}
}

func (c *Consumer) drainTimeout() time.Duration {
	if c.postCancelDrain > 0 {
		return c.postCancelDrain
	}
	return config.SQSConsumerPostCancelDrain
}

func (c *Consumer) run(receiveCtx, workCtx context.Context) {
	semaphore := make(chan struct{}, c.cfg.Concurrency)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		pollWait := c.cfg.PollWait
		if shutdownPollBudget := c.cfg.ShutdownTimeout - config.SQSConsumerPostCancelDrain; shutdownPollBudget > 0 && pollWait > shutdownPollBudget {
			pollWait = shutdownPollBudget
		}
		// Do not cancel the HTTP request with receiveCtx. Some SQS-compatible
		// servers can finish a cancelled long poll and hide its messages after
		// the client has discarded the response. Let this bounded request return
		// so the cancellation check below can explicitly release that batch. The
		// configured poll wait is a ceiling: reserve the post-cancel drain so a
		// stop never needs to abandon an in-flight request to meet its deadline.
		pollCtx, cancelPoll := context.WithTimeout(context.Background(), pollWait+time.Second)
		output, err := c.queues.Consumer.ReceiveMessage(pollCtx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(c.queues.InputURL), MaxNumberOfMessages: 10, WaitTimeSeconds: int32(seconds(pollWait)), VisibilityTimeout: int32(seconds(c.cfg.VisibilityTimeout)), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll}})
		cancelPoll()
		if err != nil {
			if receiveCtx.Err() != nil {
				return
			}
			c.logger.Error("sqs receive failed", "error", err)
			continue
		}
		if receiveCtx.Err() != nil {
			// A receive already in flight may still return a successful batch
			// after cancellation. None of it may be dispatched during shutdown,
			// so return every receipt immediately rather than leave it hidden.
			c.releaseMessages(output.Messages)
			return
		}
		for index, message := range output.Messages {
			select {
			case <-receiveCtx.Done():
				c.releaseMessages(output.Messages[index:])
				return
			default:
			}
			select {
			case semaphore <- struct{}{}:
			case <-receiveCtx.Done():
				// Messages after index were received but never dispatched, hence
				// are not in active. Return them immediately instead of making
				// shutdown wait for their original visibility timeout.
				c.releaseMessages(output.Messages[index:])
				return
			}
			if receiveCtx.Err() != nil {
				<-semaphore
				c.releaseMessages(output.Messages[index:])
				return
			}
			workers.Add(1)
			go func(message types.Message) {
				defer func() { <-semaphore; workers.Done() }()
				c.track(message)
				defer c.untrack(message)
				c.handle(workCtx, message)
			}(message)
		}
	}
}

func (c *Consumer) handle(parent context.Context, message types.Message) {
	started := time.Now()
	sqsID := aws.ToString(message.MessageId)
	envelope, err := decodeEnvelope([]byte(aws.ToString(message.Body)))
	if err != nil {
		c.permanent(parent, message, permanentReason(err), err, logFields{messageID: sqsID})
		return
	}
	fields := logFields{messageID: envelope.MessageID, correlationID: envelope.MessageID, walletID: envelope.Data.WalletID, providerID: envelope.Data.ProviderID}
	if !c.allowedProvider(envelope.Data.ProviderID) {
		c.permanent(parent, message, "provider_not_allowed", errors.New("provider is not in allowlist"), fields)
		return
	}
	input, err := envelope.input()
	if err != nil {
		c.permanent(parent, message, permanentReason(err), err, fields)
		return
	}
	prepared, err := c.useCase.Prepare(input)
	if err != nil {
		c.permanent(parent, message, permanentReason(err), err, fields)
		return
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.ProcessingTimeout)
	defer cancel()
	result, duplicate, err := c.process(ctx, envelope.MessageID, prepared)
	if err != nil {
		if isPermanent(err) {
			c.permanent(parent, message, permanentReason(err), err, fields)
		} else {
			c.retry(parent, message, err, fields)
		}
		return
	}
	if duplicate {
		// This metric counts an SQS delivery replay identified by the inbox
		// (same messageId and payload hash), not a domain-operation replay.
		// The latter is recorded below through RecordOutcome with channel SQS.
		c.metrics.duplicates.Inc()
	} else {
		c.useCase.RecordOutcome(walletapp.ChannelSQS, result, nil, time.Since(started))
	}
	fields.transactionID = result.TransactionID
	if err := c.delete(parent, message); err != nil {
		c.logger.Error("sqs delete after commit failed", "error", err, "messageId", fields.messageID, "transactionId", fields.transactionID, "walletId", fields.walletID, "providerId", fields.providerID)
		return
	}
	c.logger.Info("sqs message processed", "messageId", fields.messageID, "correlationId", fields.correlationID, "transactionId", fields.transactionID, "walletId", fields.walletID, "providerId", fields.providerID, "duplicate", duplicate)
}

func (c *Consumer) process(ctx context.Context, messageID string, prepared walletapp.PreparedOperation) (walletapp.ProcessOperationResult, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		result, duplicate, retry, err := c.processOnce(ctx, messageID, prepared)
		if !retry {
			return result, duplicate, err
		}
	}
	return walletapp.ProcessOperationResult{}, false, operation.ErrTemporarilyUnavailable
}

func (c *Consumer) processOnce(ctx context.Context, messageID string, prepared walletapp.PreparedOperation) (walletapp.ProcessOperationResult, bool, bool, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return walletapp.ProcessOperationResult{}, false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := uuid.NewV7()
	if err != nil {
		return walletapp.ProcessOperationResult{}, false, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, id.String(), consumerName, messageID, prepared.Hash())
	if err != nil {
		return walletapp.ProcessOperationResult{}, false, false, err
	}
	if tag.RowsAffected() == 0 {
		var hash string
		if err := tx.QueryRow(ctx, `SELECT payload_hash FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`, consumerName, messageID).Scan(&hash); err != nil {
			return walletapp.ProcessOperationResult{}, false, false, err
		}
		if hash != prepared.Hash() {
			return walletapp.ProcessOperationResult{}, false, false, errInboxHashMismatch
		}
		if err := tx.Commit(ctx); err != nil {
			return walletapp.ProcessOperationResult{}, false, false, err
		}
		return walletapp.ProcessOperationResult{}, true, false, nil
	}
	result, err := c.useCase.ExecuteInTx(ctx, walletpg.NewRepositories(tx), prepared)
	if errors.Is(err, walletapp.ErrRetryInNewTransaction) {
		return walletapp.ProcessOperationResult{}, false, true, nil
	}
	if err != nil {
		return walletapp.ProcessOperationResult{}, false, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE inbox_messages SET completed_at = now() WHERE consumer_name = $1 AND message_id = $2`, consumerName, messageID); err != nil {
		return walletapp.ProcessOperationResult{}, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return walletapp.ProcessOperationResult{}, false, false, err
	}
	return result, false, false, nil
}

func (c *Consumer) permanent(ctx context.Context, message types.Message, reason string, cause error, fields logFields) {
	if err := c.sendDLQ(ctx, message); err != nil {
		c.retry(ctx, message, fmt.Errorf("send dlq: %w", err), fields)
		return
	}
	if err := c.delete(ctx, message); err != nil {
		c.logger.Error("sqs delete after dlq send failed", "error", err, "messageId", fields.messageID, "reason", reason)
		return
	}
	c.metrics.dlq.WithLabelValues(reason).Inc()
	c.logger.Warn("sqs message sent to dlq", "messageId", fields.messageID, "correlationId", fields.correlationID, "walletId", fields.walletID, "providerId", fields.providerID, "reason", reason, "error", cause)
}

func (c *Consumer) retry(ctx context.Context, message types.Message, cause error, fields logFields) {
	count, delay := receiveCount(message), backoff.Exponential(receiveCount(message)-1, c.cfg.RetryBase, c.cfg.RetryMax)
	if err := c.changeVisibility(ctx, message, delay); err != nil {
		c.logger.Error("sqs retry visibility change failed", "error", err, "messageId", fields.messageID)
		return
	}
	c.metrics.retries.Inc()
	c.logger.Warn("sqs message will retry", "messageId", fields.messageID, "correlationId", fields.correlationID, "walletId", fields.walletID, "providerId", fields.providerID, "receiveCount", count, "retryAfter", delay, "error", cause)
}

func (c *Consumer) sendDLQ(ctx context.Context, message types.Message) error {
	group := message.Attributes["MessageGroupId"]
	if group == "" {
		group = "invalid"
	}
	_, err := c.queues.Consumer.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(c.queues.DLQURL), MessageBody: message.Body, MessageGroupId: aws.String(group), MessageDeduplicationId: message.MessageId})
	return err
}
func (c *Consumer) delete(ctx context.Context, message types.Message) error {
	_, err := c.queues.Consumer.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.queues.InputURL), ReceiptHandle: message.ReceiptHandle})
	return err
}
func (c *Consumer) changeVisibility(ctx context.Context, message types.Message, delay time.Duration) error {
	_, err := c.queues.Consumer.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(c.queues.InputURL), ReceiptHandle: message.ReceiptHandle, VisibilityTimeout: int32(seconds(delay))})
	return err
}
func (c *Consumer) track(message types.Message) {
	c.mu.Lock()
	c.active[aws.ToString(message.ReceiptHandle)] = message
	c.mu.Unlock()
}
func (c *Consumer) untrack(message types.Message) {
	c.mu.Lock()
	delete(c.active, aws.ToString(message.ReceiptHandle))
	c.mu.Unlock()
}
func (c *Consumer) releaseActive(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, message := range c.active {
		if err := c.changeVisibility(ctx, message, 0); err != nil {
			c.logger.Error("release message visibility", "error", err, "messageId", aws.ToString(message.MessageId))
		}
	}
}
func (c *Consumer) releaseMessages(messages []types.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), config.SQSConsumerPostCancelDrain)
	defer cancel()
	for _, message := range messages {
		if err := c.changeVisibility(ctx, message, 0); err != nil {
			c.logger.Error("release undispatched message visibility", "error", err, "messageId", aws.ToString(message.MessageId))
		}
	}
}
func (c *Consumer) allowedProvider(provider string) bool {
	for _, candidate := range c.cfg.ProviderAllowlist {
		if provider == candidate {
			return true
		}
	}
	return false
}

type envelope struct {
	Type       string       `json:"type"`
	MessageID  string       `json:"messageId"`
	OccurredAt string       `json:"occurredAt"`
	Data       envelopeData `json:"data"`
}
type envelopeData struct {
	ProviderID            string `json:"providerId"`
	ExternalTransactionID string `json:"externalTransactionId"`
	PlayerID              string `json:"playerId"`
	WalletID              string `json:"walletId"`
	RoundID               string `json:"roundId"`
	GameID                string `json:"gameId"`
	Kind                  string `json:"kind"`
	IdempotencyKey        string `json:"idempotencyKey"`
	Money                 struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"money"`
	ReferenceExternalTransactionID *string `json:"referenceExternalTransactionId,omitempty"`
}

func decodeEnvelope(body []byte) (envelope, error) {
	var decoded envelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return envelope{}, errors.New("malformed envelope")
	}
	if decoded.Type == "" || decoded.MessageID == "" || decoded.Data.IdempotencyKey == "" {
		return envelope{}, errMalformedEnvelope
	}
	if decoded.Type != "WagerTransactionRequested" {
		return envelope{}, errUnknownEnvelopeType
	}
	return decoded, nil
}
func (e envelope) input() (walletapp.ProcessOperationInput, error) {
	amount, err := money.Parse(e.Data.Money.Amount, money.Currency(e.Data.Money.Currency))
	if err != nil {
		return walletapp.ProcessOperationInput{}, err
	}
	return walletapp.ProcessOperationInput{Request: operation.Request{ProviderID: e.Data.ProviderID, ExternalTransactionID: e.Data.ExternalTransactionID, PlayerID: e.Data.PlayerID, WalletID: e.Data.WalletID, RoundID: e.Data.RoundID, GameID: e.Data.GameID, Kind: domainwallet.WagerKind(e.Data.Kind), Money: amount, ReferenceExternalTransactionID: e.Data.ReferenceExternalTransactionID}, IdempotencyKey: e.Data.IdempotencyKey, CorrelationID: e.MessageID, CausationID: e.MessageID, Channel: walletapp.ChannelSQS}, nil
}

var (
	errInboxHashMismatch   = errors.New("consumer: inbox message id has a different payload hash")
	errMalformedEnvelope   = errors.New("consumer: malformed envelope")
	errUnknownEnvelopeType = errors.New("consumer: unknown envelope type")
)

func isPermanent(err error) bool {
	var domainErr *operation.Error
	return errors.Is(err, errInboxHashMismatch) || (errors.As(err, &domainErr) && domainErr.Classification() != operation.Unavailable)
}
func permanentReason(err error) string {
	if errors.Is(err, errMalformedEnvelope) {
		return "MALFORMED_ENVELOPE"
	}
	if errors.Is(err, errUnknownEnvelopeType) {
		return "UNKNOWN_ENVELOPE_TYPE"
	}
	if errors.Is(err, errInboxHashMismatch) {
		return "message_id_hash_mismatch"
	}
	var domainErr *operation.Error
	if errors.As(err, &domainErr) {
		return string(domainErr.Code())
	}
	return "permanent_processing_failure"
}
func receiveCount(message types.Message) int {
	var n int
	_, _ = fmt.Sscanf(message.Attributes["ApproximateReceiveCount"], "%d", &n)
	if n < 1 {
		return 1
	}
	return n
}
func seconds(d time.Duration) int {
	n := int(math.Ceil(d.Seconds()))
	if n < 0 {
		return 0
	}
	return n
}

type logFields struct{ messageID, correlationID, transactionID, walletID, providerID string }
type metrics struct {
	duplicates prometheus.Counter
	retries    prometheus.Counter
	dlq        *prometheus.CounterVec
}

func newMetrics(registry *prometheus.Registry) *metrics {
	m := &metrics{duplicates: prometheus.NewCounter(prometheus.CounterOpts{Name: "sqs_consumer_duplicate_messages_total", Help: "SQS messages recognised by their inbox record."}), retries: prometheus.NewCounter(prometheus.CounterOpts{Name: "sqs_consumer_retries_total", Help: "SQS messages whose visibility was rescheduled."}), dlq: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sqs_consumer_dlq_total", Help: "SQS messages sent explicitly to the dead-letter queue."}, []string{"reason"})}
	registry.MustRegister(m.duplicates, m.retries, m.dlq)
	return m
}
