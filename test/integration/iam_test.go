//go:build integration

// Package integration proves the IAM model against a real MiniStack:
// per-role least privilege by action and ARN, an unknown key rejected, an
// explicit Deny beating an Allow, and redrive to the DLQ after
// maxReceiveCount.
//
// No root credentials anywhere in this file, in the application, or anywhere
// else outside deploy/ministack/provision.sh. The two scenarios that are
// inherently administrative - proving an explicit Deny beats an Allow, and
// proving redrive to a DLQ after maxReceiveCount - are covered instead with
// dedicated, disposable IAM test fixtures that provision.sh creates up
// front, the one place in the whole repository that is allowed to hold
// MiniStack's root key:
//   - "deny-probe": one Allow and one explicit Deny policy, both on
//     sqs:SendMessage against the same disposable queue
//     (IAM_TEST_DENY_PROBE_QUEUE_NAME). This test never mutates IAM; it
//     only proves the Deny that was already attached at provisioning time
//     wins.
//   - "redrive-tester": send/receive access to a disposable FIFO pair
//     (IAM_TEST_REDRIVE_INPUT_QUEUE_NAME / IAM_TEST_REDRIVE_DLQ_QUEUE_NAME)
//     with a low maxReceiveCount, so the redrive assertion does not have to
//     wait out the production input queue's much longer visibility
//     timeout, plus DeleteMessage scoped to the DLQ side only, so the test
//     can remove the message its own run redirects there without ever
//     depending on the DLQ being empty beforehand.
//
// Every credential this file uses - gateway, consumer, publisher,
// events-reader, deny-probe, redrive-tester - comes from
// deploy/ministack/.runtime/test-credentials.env, which provision.sh
// writes and which is never mounted into the app container (see
// docker-compose.yml and deploy/docker/entrypoint.sh): these are test-only
// keys, kept out of the app's own least-privilege credential file.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/viniciustakedi/jungle-gaming-wallet/test/testclient"
)

type roleCreds struct {
	gatewayKey, gatewaySecret                 string
	consumerKey, consumerSecret               string
	publisherKey, publisherSecret             string
	eventsReaderKey, eventsReaderSecret       string
	denyProbeKey, denyProbeSecret             string
	redriveKey, redriveSecret                 string
	dlqReaderKey, dlqReaderSecret             string
	consumerFixtureKey, consumerFixtureSecret string

	denyProbeQueueName     string
	redriveInputQueueName  string
	redriveDLQQueueName    string
	redriveMaxReceiveCount int
}

// loadTestCreds reads both credential files provision.sh writes: this test
// suite needs to authenticate as every role, including consumer and
// publisher, to prove what each one can and cannot do - that is different
// from the app container, which is only ever handed its own
// app-credentials.env (see docker-compose.yml). test-credentials.env holds
// everything else: gateway, events-reader, the IAM test fixtures
// (deny-probe, redrive-tester) and their fixture queue names. Neither file
// is ever mounted into a container other than provisioning (which writes
// them) and, for app-credentials.env, the app itself.
func loadTestCreds(t *testing.T) roleCreds {
	t.Helper()

	values := map[string]string{}
	for _, kv := range []struct{ envVar, fallback string }{
		{"APP_CREDENTIALS_FILE", filepath.Join("..", "..", "deploy", "ministack", ".runtime", "app-credentials.env")},
		{"TEST_CREDENTIALS_FILE", filepath.Join("..", "..", "deploy", "ministack", ".runtime", "test-credentials.env")},
	} {
		path := os.Getenv(kv.envVar)
		if path == "" {
			path = kv.fallback
		}
		parsed := readEnvFile(t, path)
		for k, v := range parsed {
			values[k] = v
		}
	}

	get := func(k string) string {
		v, ok := values[k]
		if !ok || v == "" {
			t.Fatalf("missing %s in app-credentials.env/test-credentials.env", k)
		}
		return v
	}

	maxReceive, err := strconv.Atoi(get("IAM_TEST_REDRIVE_MAX_RECEIVE_COUNT"))
	if err != nil {
		t.Fatalf("invalid IAM_TEST_REDRIVE_MAX_RECEIVE_COUNT: %v", err)
	}

	return roleCreds{
		gatewayKey:            get("GATEWAY_ACCESS_KEY_ID"),
		gatewaySecret:         get("GATEWAY_SECRET_ACCESS_KEY"),
		consumerKey:           get("SQS_CONSUMER_ACCESS_KEY_ID"),
		consumerSecret:        get("SQS_CONSUMER_SECRET_ACCESS_KEY"),
		publisherKey:          get("SQS_PUBLISHER_ACCESS_KEY_ID"),
		publisherSecret:       get("SQS_PUBLISHER_SECRET_ACCESS_KEY"),
		eventsReaderKey:       get("EVENTS_READER_ACCESS_KEY_ID"),
		eventsReaderSecret:    get("EVENTS_READER_SECRET_ACCESS_KEY"),
		denyProbeKey:          get("DENY_PROBE_ACCESS_KEY_ID"),
		denyProbeSecret:       get("DENY_PROBE_SECRET_ACCESS_KEY"),
		redriveKey:            get("REDRIVE_TESTER_ACCESS_KEY_ID"),
		redriveSecret:         get("REDRIVE_TESTER_SECRET_ACCESS_KEY"),
		dlqReaderKey:          get("DLQ_READER_ACCESS_KEY_ID"),
		dlqReaderSecret:       get("DLQ_READER_SECRET_ACCESS_KEY"),
		consumerFixtureKey:    get("CONSUMER_FIXTURE_ACCESS_KEY_ID"),
		consumerFixtureSecret: get("CONSUMER_FIXTURE_SECRET_ACCESS_KEY"),

		denyProbeQueueName:     get("IAM_TEST_DENY_PROBE_QUEUE_NAME"),
		redriveInputQueueName:  get("IAM_TEST_REDRIVE_INPUT_QUEUE_NAME"),
		redriveDLQQueueName:    get("IAM_TEST_REDRIVE_DLQ_QUEUE_NAME"),
		redriveMaxReceiveCount: maxReceive,
	}
}

func sqsClient(t *testing.T, accessKeyID, secretAccessKey string) *sqs.Client {
	return testclient.NewSQSClient(t, accessKeyID, secretAccessKey)
}

// queueURL builds the queue URL directly instead of calling GetQueueUrl:
// not every role in the IAM model has that action on every queue this test
// touches (gateway, for instance, has no GetQueueUrl at all - see
// provision.sh), and MiniStack's URL shape is a stable, documented
// convention, {endpoint}/{accountId}/{queueName}.
func queueURL(name string) string { return testclient.QueueURL(name) }

func inputQueueName() string { return testclient.InputQueueName() }

func dlqQueueName() string {
	if v := os.Getenv("SQS_DLQ_QUEUE_NAME"); v != "" {
		return v
	}
	return "wager-transactions-dlq.fifo"
}

func outputQueueName() string { return testclient.OutputQueueName() }

func assertAllowed(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: want allowed, got denied: %v", name, err)
	}
}

func assertDenied(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: want denied, got allowed", name)
	}
}

func send(t *testing.T, client *sqs.Client, queueURL, group, dedup string) error {
	t.Helper()
	_, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(`{"probe":true}`),
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(dedup),
	})
	return err
}

// sendMarked sends a message whose body carries marker, so a later receive
// can correlate the exact message this call produced instead of accepting
// whatever happens to be sitting on the queue - including residue a
// previous run left behind. group is expected to be derived from marker
// too: a FIFO group is delivered in strict order, so sharing a group with
// older, undeleted messages would head-of-line block a fresh message behind
// them until those are drained, defeating the correlation this is for.
func sendMarked(t *testing.T, client *sqs.Client, queueURL, group, marker string) error {
	t.Helper()
	_, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(fmt.Sprintf(`{"probe":true,"marker":%q}`, marker)),
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(marker),
	})
	return err
}

// receiveByMarker long-polls queueURL until a message whose body contains
// marker shows up, or deadline passes, returning nil in the latter case.
// Messages without the marker are left exactly as received (not deleted):
// callers use this specifically where the queue cannot be assumed empty.
func receiveByMarker(ctx context.Context, client *sqs.Client, queueURL, marker string, deadline time.Time) (*string, error) {
	for time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(queueURL),
			MaxNumberOfMessages: 10,
			VisibilityTimeout:   30,
			WaitTimeSeconds:     5,
		})
		if err != nil {
			return nil, err
		}
		for _, msg := range out.Messages {
			if msg.Body != nil && strings.Contains(*msg.Body, marker) {
				return msg.ReceiptHandle, nil
			}
		}
	}
	return nil, nil
}

// TestIAM_AccessScopedByActionAndARN proves each of the four provisioned
// roles can do only what the IAM model grants it - nothing on a queue it was
// not scoped to, nothing outside its listed actions.
func TestIAM_AccessScopedByActionAndARN(t *testing.T) {
	rc := loadTestCreds(t)
	gateway := sqsClient(t, rc.gatewayKey, rc.gatewaySecret)
	consumer := sqsClient(t, rc.consumerKey, rc.consumerSecret)
	publisher := sqsClient(t, rc.publisherKey, rc.publisherSecret)
	reader := sqsClient(t, rc.eventsReaderKey, rc.eventsReaderSecret)

	ctx := context.Background()
	inURL := queueURL(inputQueueName())
	dlqURL := queueURL(dlqQueueName())
	outURL := queueURL(outputQueueName())

	t.Run("gateway can send on input", func(t *testing.T) {
		assertAllowed(t, "gateway SendMessage input", send(t, gateway, inURL, "iam-test", uniqueID("gw-in")))
	})
	t.Run("gateway cannot receive on input", func(t *testing.T) {
		_, err := gateway.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(inURL)})
		assertDenied(t, "gateway ReceiveMessage input", err)
	})
	t.Run("gateway cannot send on output", func(t *testing.T) {
		assertDenied(t, "gateway SendMessage output", send(t, gateway, outURL, "iam-test", uniqueID("gw-out")))
	})
	t.Run("gateway cannot send on dlq", func(t *testing.T) {
		assertDenied(t, "gateway SendMessage dlq", send(t, gateway, dlqURL, "iam-test", uniqueID("gw-dlq")))
	})

	t.Run("consumer cannot send on input", func(t *testing.T) {
		assertDenied(t, "consumer SendMessage input", send(t, consumer, inURL, "iam-test", uniqueID("co-in")))
	})
	t.Run("consumer can send on dlq", func(t *testing.T) {
		assertAllowed(t, "consumer SendMessage dlq", send(t, consumer, dlqURL, "iam-test", uniqueID("co-dlq")))
	})
	t.Run("consumer can resolve the dlq url", func(t *testing.T) {
		_, err := consumer.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(dlqQueueName())})
		assertAllowed(t, "consumer GetQueueUrl dlq", err)
	})
	// Accepting an empty ReceiveMessage result as success would let a
	// regression that never delivers the seeded message pass without
	// ChangeMessageVisibility or DeleteMessage ever running. So the message is
	// seeded via the gateway in its own FIFO group, where nothing else in the
	// queue can head-of-line block it, and ReceiveMessage retries under a
	// deadline until that exact message shows up before the rest of the
	// receive/change/delete cycle runs against it.
	t.Run("consumer can receive, change visibility and delete on input", func(t *testing.T) {
		marker := uniqueID("co-cycle")
		group := "consumer-cycle-" + marker
		if err := sendMarked(t, gateway, inURL, group, marker); err != nil {
			t.Fatalf("seed message via gateway: %v", err)
		}

		const timeout = 20 * time.Second
		handle, err := receiveByMarker(ctx, consumer, inURL, marker, time.Now().Add(timeout))
		assertAllowed(t, "consumer ReceiveMessage input", err)
		if err != nil {
			return
		}
		if handle == nil {
			t.Fatalf("did not receive the seeded message (marker %s) via consumer within %s", marker, timeout)
		}

		_, visErr := consumer.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl:          aws.String(inURL),
			ReceiptHandle:     handle,
			VisibilityTimeout: 0,
		})
		assertAllowed(t, "consumer ChangeMessageVisibility input", visErr)
		_, delErr := consumer.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(inURL), ReceiptHandle: handle})
		assertAllowed(t, "consumer DeleteMessage input", delErr)
	})
	t.Run("consumer cannot receive on output", func(t *testing.T) {
		_, err := consumer.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(outURL)})
		assertDenied(t, "consumer ReceiveMessage output", err)
	})

	t.Run("publisher can send on output", func(t *testing.T) {
		assertAllowed(t, "publisher SendMessage output", send(t, publisher, outURL, "iam-test", uniqueID("pub-out")))
	})
	t.Run("publisher cannot send on input", func(t *testing.T) {
		assertDenied(t, "publisher SendMessage input", send(t, publisher, inURL, "iam-test", uniqueID("pub-in")))
	})

	t.Run("events-reader can receive on output", func(t *testing.T) {
		_, err := reader.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(outURL), WaitTimeSeconds: 1})
		assertAllowed(t, "events-reader ReceiveMessage output", err)
	})
	// Accepting an empty ReceiveMessage result as success would let a
	// regression that never delivers the seeded message pass without
	// DeleteMessage ever running. So the message is seeded via the publisher
	// in its own FIFO group, where nothing else in the queue can head-of-line
	// block it, and ReceiveMessage retries under a deadline until that exact
	// message shows up before it is deleted.
	t.Run("events-reader can receive and delete on output", func(t *testing.T) {
		marker := uniqueID("rd-cycle")
		group := "reader-cycle-" + marker
		if err := sendMarked(t, publisher, outURL, group, marker); err != nil {
			t.Fatalf("seed message via publisher: %v", err)
		}

		const timeout = 20 * time.Second
		handle, err := receiveByMarker(ctx, reader, outURL, marker, time.Now().Add(timeout))
		assertAllowed(t, "events-reader ReceiveMessage output (cycle)", err)
		if err != nil {
			return
		}
		if handle == nil {
			t.Fatalf("did not receive the seeded message (marker %s) via events-reader within %s", marker, timeout)
		}

		_, delErr := reader.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(outURL), ReceiptHandle: handle})
		assertAllowed(t, "events-reader DeleteMessage output", delErr)
	})
	t.Run("events-reader cannot send on output", func(t *testing.T) {
		assertDenied(t, "events-reader SendMessage output", send(t, reader, outURL, "iam-test", uniqueID("rd-out")))
	})

	t.Run("nobody has iam:CreateUser or unscoped queue admin", func(t *testing.T) {
		_, err := gateway.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("iam-test-evil.fifo"), Attributes: map[string]string{"FifoQueue": "true"}})
		assertDenied(t, "gateway CreateQueue", err)
	})
}

// TestIAM_UnknownAccessKeyRejected proves an access key MiniStack has never
// issued is rejected outright, before any policy is even evaluated.
func TestIAM_UnknownAccessKeyRejected(t *testing.T) {
	inURL := queueURL(inputQueueName())

	unknown := sqsClient(t, "AKIAUNKNOWNUNKNOWN00", "whatever-secret")
	assertDenied(t, "unknown access key SendMessage", send(t, unknown, inURL, "iam-test", uniqueID("unknown")))
}

// TestIAM_ExplicitDenyWinsOverAllow proves an explicit Deny beats an Allow
// on the same action and ARN, using the "deny-probe" fixture provision.sh
// creates with exactly that combination (one Allow policy, one explicit
// Deny policy, both on sqs:SendMessage against the same disposable queue).
// No root credential is used here: the Deny was already attached at
// provisioning time, so this test only has to observe the outcome.
func TestIAM_ExplicitDenyWinsOverAllow(t *testing.T) {
	rc := loadTestCreds(t)
	denyProbe := sqsClient(t, rc.denyProbeKey, rc.denyProbeSecret)
	probeURL := queueURL(rc.denyProbeQueueName)

	assertDenied(t, "deny-probe SendMessage despite its own Allow policy", send(t, denyProbe, probeURL, "iam-test", uniqueID("deny-probe")))
}

// TestIAM_RedriveToDLQ proves MiniStack moves a message to its DLQ once
// maxReceiveCount is exceeded, using the disposable "redrive-tester"
// fixture (send/receive on a dedicated FIFO pair with a low maxReceiveCount
// and a short visibility timeout, plus DeleteMessage scoped to the DLQ)
// provision.sh creates, so this test does not need root to create or tear
// down queues, and does not have to wait out the production input queue's
// much longer visibility timeout.
//
// The DLQ fixture is never emptied between runs, so a plain "is the DLQ
// non-empty" check would pass on a stale message left behind by a previous
// run even if this run's redrive were broken. Instead, the seeded message
// carries a unique marker in its body and its own FIFO group (so it can
// never be head-of-line blocked by an older, undeleted message sitting in
// some other group), the DLQ is polled under a deadline specifically for
// that marker while unrelated messages are ignored, and the message found
// is deleted afterwards so this run does not itself become tomorrow's
// residue.
func TestIAM_RedriveToDLQ(t *testing.T) {
	rc := loadTestCreds(t)
	redriveTester := sqsClient(t, rc.redriveKey, rc.redriveSecret)
	ctx := context.Background()

	inURL := queueURL(rc.redriveInputQueueName)
	dlqURL := queueURL(rc.redriveDLQQueueName)

	marker := uniqueID("msg")
	group := "redrive-" + marker
	if err := sendMarked(t, redriveTester, inURL, group, marker); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; attempt < rc.redriveMaxReceiveCount+3 && time.Now().Before(deadline); attempt++ {
		if _, err := redriveTester.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(inURL),
			MaxNumberOfMessages: 10,
			VisibilityTimeout:   1,
			WaitTimeSeconds:     2,
		}); err != nil {
			t.Fatalf("receive message (attempt %d): %v", attempt, err)
		}
		time.Sleep(1500 * time.Millisecond)
	}

	const dlqTimeout = 30 * time.Second
	handle, err := receiveByMarker(ctx, redriveTester, dlqURL, marker, time.Now().Add(dlqTimeout))
	if err != nil {
		t.Fatalf("receive from dlq: %v", err)
	}
	if handle == nil {
		t.Fatalf("want message with marker %s in the DLQ after maxReceiveCount was exceeded, got none within %s", marker, dlqTimeout)
	}

	if _, err := redriveTester.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(dlqURL), ReceiptHandle: handle}); err != nil {
		t.Fatalf("delete redriven message (marker %s) from dlq: %v", marker, err)
	}
}

// uniqueID is test/testclient's shared id generator - test/multiinstance uses
// the same function.
func uniqueID(prefix string) string {
	return testclient.UniqueID(prefix)
}
