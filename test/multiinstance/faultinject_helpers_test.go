//go:build multiinstance

package multiinstance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

// startWithEnv launches one instance whose env layers overrides on top of
// baseEnv - FAULT_INJECT_POINT for a victim, or just a short poll interval,
// lease or visibility timeout for the rescuer that observes recovery, so a
// scenario never has to wait out this package's production defaults.
// startInstance still waits for /health/ready before returning: a fault
// point fires on a specific later operation, never at startup.
func startWithEnv(t *testing.T, binary, logDir, name string, overrides map[string]string) *instance {
	t.Helper()
	env := baseEnv(t)
	for k, v := range overrides {
		env[k] = v
	}
	return startInstance(t, binary, env, name, logDir, "127.0.0.1:"+strconv.Itoa(freePort(t)))
}

// drainBacklog starts a short-lived, un-injected instance so its outbox
// publisher, pending-reference worker and (with sqsToo) SQS consumer - all
// of which start working immediately, see internal/outbox.Publisher.run,
// internal/referenceworker.Worker.run and internal/consumer.Consumer.start -
// catch up on anything an earlier run of these same tests left behind on
// this package's shared Postgres and MiniStack, then stops it. A fault
// point that fires generically (outside a single named transaction, such as
// "before-commit" or an outbox point reached by the publisher's own
// background loop, or the very next SQS message a "before-commit" or
// "after-commit-before-delete" victim happens to receive) would otherwise
// risk killing the victim on that stray row or message before it ever
// reaches the one this scenario creates - observed running these scenarios
// with `-count=3` against the same infrastructure. It waits on the drain
// instance's own outbox_pending_events and pending_reference_active gauges
// reaching zero rather than a fixed sleep, since how much of that backlog an
// earlier run left behind is not bounded; SQS exposes no equivalent
// queue-depth metric through this application, so sqsToo instead gives its
// consumer a few full receive cycles to empty whatever is currently visible.
// The outbox wait in particular has to outlast the full default OUTBOX_LEASE
// (30s): an earlier scenario's victim can die from an unrelated fault point
// while its background outbox publisher happens to be mid-claim on rows from
// that same scenario's wallet-opening, orphaning them under the default lease
// until it naturally expires.
func drainBacklog(t *testing.T, binary, logDir string, sqsToo bool) *instance {
	t.Helper()
	expireForeignPendingReferences(t)
	overrides := map[string]string{}
	if sqsToo {
		overrides["SQS_CONSUMER_ENABLED"] = "true"
		overrides["SQS_CONSUMER_POLL_WAIT"] = "1s"
	}
	drain := startWithEnv(t, binary, logDir, "drain", overrides)
	waitForGaugeZero(t, drain, "outbox_pending_events", 45*time.Second)
	waitForGaugeZero(t, drain, "pending_reference_active", 20*time.Second)
	if sqsToo {
		time.Sleep(5 * time.Second)
	}
	stopInstance(t, drain)
	return drain
}

// expireForeignPendingReferences brings every PENDING_REFERENCE row's
// deadline and next attempt forward to now, so the drain instance's own
// worker claims them right away (the claim in internal/referenceworkerpg
// only takes rows whose next_attempt_at has come, and a row left mid-backoff
// or mid-lease sits minutes in the future) and settles them through the
// ordinary expiry path, letting pending_reference_active reach zero.
//
// Without it this package inherits an ordering dependency from the
// integration suite, which shares this Postgres: those pending rows carry
// the default 24h REFERENCE_WORKER_TTL stamped into pending_expires_at when
// they were inserted, so no configuration on the drain instance can retire
// them, and every scenario here fails on the drain barrier. Running the
// suites in the other order, or against a fresh database, hides it.
//
// It is an UPDATE the wallet_app role already holds (the worker reschedules
// these same rows), never a delete, and it only reaches rows left behind
// before the scenario starts: drainBacklog runs before the scenario creates
// anything of its own.
func expireForeignPendingReferences(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := connectApp(t, ctx)
	if _, err := conn.Exec(ctx, `UPDATE wager_transactions SET pending_expires_at = now(), next_attempt_at = now() WHERE status = 'PENDING_REFERENCE'`); err != nil {
		t.Fatalf("expire leftover pending references: %v", err)
	}
}

// bareMetricValue reads a single, unlabeled Prometheus metric (gauge or
// counter) off inst's /metrics, failing the test if the series has not been
// registered yet - a metric that has never been observed is not the same as
// one legitimately reading zero, and treating an absent series as 0 would let
// drainBacklog decide a backlog is empty when it has simply never been
// measured.
func bareMetricValue(t *testing.T, inst *instance, name string) float64 {
	t.Helper()
	resp, body := doAt(t, inst, http.MethodGet, "/metrics", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics (instance %s) status = %d, want 200", inst.name, resp.StatusCode)
	}
	prefix := name + " "
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return value
	}
	t.Fatalf("metric %s (instance %s) was not found on /metrics", name, inst.name)
	return 0
}

// waitForGaugeZero polls inst's own gauge named name until it reads zero,
// failing the test if timeout passes first - drainBacklog relies on this to
// know the backlog is actually empty before releasing the victim; silently
// giving up here would let a victim start with a pending backlog and crash
// on the wrong item instead of the one this scenario created.
func waitForGaugeZero(t *testing.T, inst *instance, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if bareMetricValue(t, inst, name) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("gauge %s (instance %s) did not reach zero before %s deadline - the drain never actually emptied the backlog", name, inst.name, timeout)
}

// crashHTTPClient has no timeout of its own: a fault-injected instance kills
// itself mid-request, so the call is expected to end in a connection error,
// not a slow response this client should give up on first.
var crashHTTPClient = &http.Client{}

// doExpectingCrash issues one request against inst and tolerates the
// connection dropping before a response arrives - the expected outcome when
// inst's own fault point kills the process while this request is still
// in flight. It never fails the test on a transport error; a scenario
// asserts the crash separately, with waitExit.
func doExpectingCrash(t *testing.T, inst *instance, method, path, token string, headers map[string]string, body []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, inst.baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := crashHTTPClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	// Either outcome is acceptable: a connection error (the process died
	// before writing any response) or a response that happened to race the
	// crash.
}

// doWageringExpectingCrash posts a wagering operation to inst and tolerates
// the connection dropping before a response arrives - inst's fault point is
// expected to kill it after committing but before this request ever gets a
// reply.
func doWageringExpectingCrash(t *testing.T, inst *instance, token string, in wageringBodyInput, idempotencyKey string) {
	t.Helper()
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	doExpectingCrash(t, inst, http.MethodPost, "/wagering/transactions", token, headers, testclient.WageringBody(in))
}

// waitForSQSConsumerDuplicateMessagesMetric polls inst's own bare, unlabeled
// sqs_consumer_duplicate_messages_total counter (internal/consumer) - the
// inbox-redelivery signal, distinct from the channel-labelled
// wagering_duplicate_attempts_total a domain-operation replay increments -
// until it reaches want or timeout passes.
func waitForSQSConsumerDuplicateMessagesMetric(t *testing.T, inst *instance, want float64, timeout time.Duration) {
	t.Helper()
	waitForMetricAtLeast(t, inst, "sqs_consumer_duplicate_messages_total", want, timeout)
}

// waitForMetricAtLeast polls inst's own bare metric named name until it
// reaches at least want or timeout passes.
func waitForMetricAtLeast(t *testing.T, inst *instance, name string, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := bareMetricValue(t, inst, name); got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s (instance %s) did not reach %.0f before %s deadline", name, inst.name, want, timeout)
}

// requireNoOutputEvents peeks the output queue for window and fails the test
// if any message whose eventId is in want appears - the proof that nothing
// was actually sent on the wire before a rescuer starts. A published_at
// check alone would still pass if the victim had already sent the event and
// only died before confirming it. Every receive sets
// VisibilityTimeout to 0, so a message peeked here stays (or becomes again)
// immediately visible to whichever reader looks for it afterward - this
// call must never consume a delivery a later assertion still needs.
func requireNoOutputEvents(t *testing.T, window time.Duration, want map[string]bool) {
	t.Helper()
	creds := loadSQSTestCreds(t)
	reader := newSQSClient(t, creds.eventsReaderKey, creds.eventsReaderSecret)
	queueURL := sqsQueueURL(outputQueueName())
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		out, err := reader.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 0,
		})
		if err != nil {
			t.Fatalf("peek output queue for unexpected early events: %v", err)
		}
		for _, message := range out.Messages {
			if message.Body == nil {
				continue
			}
			var envelope eventEnvelopeJSON
			if err := json.Unmarshal([]byte(*message.Body), &envelope); err != nil {
				t.Fatalf("decode peeked event envelope: %v", err)
			}
			if want[envelope.EventID] {
				t.Fatalf("event %s already on the output queue before the rescuer started - the victim sent it before crashing", envelope.EventID)
			}
		}
	}
}

// outputReader is a persistent, stateful reader of the output queue, used
// where a scenario needs to observe two separate waves of delivery (before
// and after a rescuer) as one continuous session: rawDeliveries is the
// independent count of every delivery observed per eventId, applied is the
// reader-side dedup tracking which eventIds have already been treated as
// processed exactly once, and payloads keeps every raw body observed per
// eventId, so a same-eventId assertion can compare a republished delivery
// against the original one byte for byte.
type outputReader struct {
	client        *sqs.Client
	applied       map[string]bool
	rawDeliveries map[string]int
	payloads      map[string][][]byte
}

func newOutputReader(t *testing.T) *outputReader {
	t.Helper()
	creds := loadSQSTestCreds(t)
	return &outputReader{
		client:        newSQSClient(t, creds.eventsReaderKey, creds.eventsReaderSecret),
		applied:       map[string]bool{},
		rawDeliveries: map[string]int{},
		payloads:      map[string][][]byte{},
	}
}

// drain receives from the output queue until deadline, deleting every
// message whose eventId is in want (leaving anything else exactly as
// received, since a shared queue can carry another test's residue) and
// folding each delivery into the reader's running state. It returns how
// many new deliveries among want this call observed - the count a caller
// bounding a single wave (such as "exactly the victim's own send, before
// the rescuer exists") checks against.
func (r *outputReader) drain(t *testing.T, deadline time.Time, want map[string]bool) int {
	t.Helper()
	queueURL := sqsQueueURL(outputQueueName())
	newDeliveries := 0
	for time.Now().Before(deadline) {
		out, err := r.client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
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
			if !want[envelope.EventID] {
				continue
			}
			newDeliveries++
			r.rawDeliveries[envelope.EventID]++
			r.payloads[envelope.EventID] = append(r.payloads[envelope.EventID], []byte(*message.Body))
			r.applied[envelope.EventID] = true
			if _, err := r.client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle}); err != nil {
				t.Fatalf("delete observed event: %v", err)
			}
		}
	}
	return newDeliveries
}

// instancePublishedEventIDs parses inst's own JSON log for every "outbox
// event published" line (internal/outbox.Publisher.publish) and returns the
// eventIds it names. outbox_events carries no instance-identity column, so
// this is the only way to attribute a given row's published_at (Postgres's
// own clock, see outboxPublishedAt) to the specific instance that confirmed
// it - the dispute scenario's overlap proof needs both halves together.
func instancePublishedEventIDs(t *testing.T, inst *instance) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(inst.logPath)
	if err != nil {
		t.Fatalf("read log for instance %s: %v", inst.name, err)
	}
	ids := map[string]bool{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry struct {
			Msg     string `json:"msg"`
			EventID string `json:"eventId"`
		}
		// Not every line in this combined stdout/stderr log is a JSON slog
		// record - a `-race` report (see requireNoRaceOutput) is plain text -
		// so a line that fails to decode simply carries no event to add.
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Msg == "outbox event published" && entry.EventID != "" {
			ids[entry.EventID] = true
		}
	}
	return ids
}

// startTrioWithEnv is startTrio (harness_test.go) with overrides layered onto
// baseEnv - kept separate rather than changing startTrio itself, whose
// override-free signature other scenarios depend on.
func startTrioWithEnv(t *testing.T, overrides map[string]string) []*instance {
	t.Helper()
	binary := buildBinary(t)
	env := baseEnv(t)
	for k, v := range overrides {
		env[k] = v
	}
	logDir := t.TempDir()

	names := [3]string{"a", "b", "c"}
	instances := make([]*instance, len(names))
	for i, name := range names {
		instances[i] = startInstance(t, binary, env, name, logDir, "127.0.0.1:"+strconv.Itoa(freePort(t)))
	}
	return instances
}

// restartTrioWithEnv is restartTrio (harness_test.go) with overrides layered
// onto baseEnv for the restarted instances - see startTrioWithEnv.
func restartTrioWithEnv(t *testing.T, instances []*instance, overrides map[string]string) []*instance {
	t.Helper()
	for _, inst := range instances {
		killInstance(t, inst)
	}

	binary := buildBinary(t)
	env := baseEnv(t)
	for k, v := range overrides {
		env[k] = v
	}
	logDir := t.TempDir()

	names := [3]string{"a-restarted", "b-restarted", "c-restarted"}
	restarted := make([]*instance, len(names))
	for i, name := range names {
		restarted[i] = startInstance(t, binary, env, name, logDir, "127.0.0.1:"+strconv.Itoa(freePort(t)))
	}
	return restarted
}
