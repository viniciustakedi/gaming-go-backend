#!/bin/sh
# Provisions the queues, redrive, least-privilege IAM users and IAM test
# fixtures this service needs, against a fresh (or already-provisioned)
# MiniStack. Runs once per `docker compose up` in a throwaway container, as
# root - this is the ONLY place in the whole Compose stack, and the ONLY
# place in the whole repository, that ever holds MiniStack's root
# credentials ("test"/"test", which bypass every IAM policy - see
# alignment.md). Every other service, and the integration test suite, only
# ever sees the per-role keys this script writes out at the end.
#
# Idempotent by design: every queue, user, policy and fixture is created
# only if it does not already exist, so a second `docker compose up --build`
# against the same MiniStack state succeeds instead of failing on
# EntityAlreadyExists. Access keys are reused across runs when the
# credentials file already on disk still matches a key IAM actually has for
# that user; otherwise a new one is minted and the file is updated. This
# keeps `docker compose stop && docker compose up --build` from rotating
# credentials the app or a running test suite might still be holding.
set -eu

EP="${MINISTACK_ENDPOINT_URL:-http://ministack:4566}"
REGION="${AWS_DEFAULT_REGION:-us-east-1}"
ACCOUNT_ID="${MINISTACK_ACCOUNT_ID:-000000000000}"
MAX_RECEIVE_COUNT="${WAGER_TRANSACTIONS_MAX_RECEIVE_COUNT:-5}"
RUNTIME_DIR="${IAM_RUNTIME_DIR:-/shared}"
APP_CREDENTIALS_FILE="${APP_CREDENTIALS_FILE:-$RUNTIME_DIR/app-credentials.env}"
TEST_CREDENTIALS_FILE="${TEST_CREDENTIALS_FILE:-$RUNTIME_DIR/test-credentials.env}"
# The UID/GID the app container's non-root user runs as (see Dockerfile).
# app-credentials.env is chowned to it so that user can read a 0600 file it
# does not otherwise own or share a name with in this container's image.
APP_RUNTIME_UID="${APP_RUNTIME_UID:-10001}"
APP_RUNTIME_GID="${APP_RUNTIME_GID:-10001}"

IN_QUEUE="${SQS_INPUT_QUEUE_NAME:-wager-transactions.fifo}"
DLQ_QUEUE="${SQS_DLQ_QUEUE_NAME:-wager-transactions-dlq.fifo}"
OUT_QUEUE="${SQS_OUTPUT_QUEUE_NAME:-wallet-events.fifo}"

# IAM test fixtures (item 1 of the review). These exist only to let the
# integration test suite prove two IAM behaviours - explicit Deny beating
# Allow, and redrive to a DLQ - without ever touching root credentials or a
# production queue itself. Disposable, deterministically named so a rerun
# recognises them.
DENY_PROBE_QUEUE="${IAM_TEST_DENY_PROBE_QUEUE_NAME:-iam-test-deny-probe.fifo}"
REDRIVE_IN_QUEUE="${IAM_TEST_REDRIVE_INPUT_QUEUE_NAME:-iam-test-redrive-in.fifo}"
REDRIVE_DLQ_QUEUE="${IAM_TEST_REDRIVE_DLQ_QUEUE_NAME:-iam-test-redrive-dlq.fifo}"
REDRIVE_MAX_RECEIVE_COUNT="${IAM_TEST_REDRIVE_MAX_RECEIVE_COUNT:-2}"
REDRIVE_VISIBILITY_TIMEOUT="${IAM_TEST_REDRIVE_VISIBILITY_TIMEOUT:-1}"

export AWS_DEFAULT_REGION="$REGION"
export AWS_PAGER=""
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test

aws_root() {
	aws --endpoint-url "$EP" "$@"
}

arn() {
	echo "arn:aws:sqs:${REGION}:${ACCOUNT_ID}:$1"
}

echo "provisioning: waiting for MiniStack at $EP"
i=0
until aws_root sqs list-queues >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -ge 30 ]; then
		echo "provisioning: MiniStack never became reachable at $EP" >&2
		exit 1
	fi
	sleep 1
done

queue_exists() {
	aws_root sqs get-queue-url --queue-name "$1" >/dev/null 2>&1
}

# ensure_queue creates a queue only if it is not already there, so a second
# run against the same MiniStack does not fail on QueueAlreadyExists (or,
# worse, on conflicting-attributes errors from recreating it with the same
# name but different settings).
ensure_queue() {
	name="$1"
	attrs="$2"
	if queue_exists "$name"; then
		echo "provisioning: queue $name already exists, skipping create"
	else
		aws_root sqs create-queue --queue-name "$name" --attributes "$attrs" >/dev/null
		echo "provisioning: created queue $name"
	fi
}

echo "provisioning: creating queues"
ensure_queue "$DLQ_QUEUE" "FifoQueue=true"

# RedrivePolicy's value is itself a JSON string, so it needs to be escaped
# twice: once for the outer --attributes JSON, once because SQS stores the
# policy as a string attribute rather than a nested object.
ensure_queue "$IN_QUEUE" "{\"FifoQueue\":\"true\",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$(arn "$DLQ_QUEUE")\\\",\\\"maxReceiveCount\\\":\\\"$MAX_RECEIVE_COUNT\\\"}\"}"

ensure_queue "$OUT_QUEUE" "FifoQueue=true"

echo "provisioning: creating IAM test fixture queues (deny-probe, redrive pair)"
ensure_queue "$DENY_PROBE_QUEUE" "FifoQueue=true"
ensure_queue "$REDRIVE_DLQ_QUEUE" "FifoQueue=true"
ensure_queue "$REDRIVE_IN_QUEUE" "{\"FifoQueue\":\"true\",\"VisibilityTimeout\":\"$REDRIVE_VISIBILITY_TIMEOUT\",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$(arn "$REDRIVE_DLQ_QUEUE")\\\",\\\"maxReceiveCount\\\":\\\"$REDRIVE_MAX_RECEIVE_COUNT\\\"}\"}"

echo "provisioning: creating IAM users and least-privilege policies"

policy() {
	printf '{"Version":"2012-10-17","Statement":[%s]}' "$1"
}
allow() {
	printf '{"Effect":"Allow","Action":%s,"Resource":"%s"}' "$1" "$2"
}
deny() {
	printf '{"Effect":"Deny","Action":%s,"Resource":"%s"}' "$1" "$2"
}

user_exists() {
	aws_root iam get-user --user-name "$1" >/dev/null 2>&1
}

ensure_user() {
	name="$1"
	if user_exists "$name"; then
		echo "provisioning: user $name already exists, skipping create"
	else
		aws_root iam create-user --user-name "$name" >/dev/null
		echo "provisioning: created user $name"
	fi
}

# has_access_key returns success if the given access key id is currently
# listed for the given IAM user.
has_access_key() {
	user="$1"
	key_id="$2"
	[ -n "$key_id" ] || return 1
	aws_root iam list-access-keys --user-name "$user" --query 'AccessKeyMetadata[].AccessKeyId' --output text 2>/dev/null |
		tr '\t' '\n' | grep -qx "$key_id"
}

# ensure_access_key reuses the access key already recorded for $id_var (read
# from a previously-written credentials file, if any) when IAM still has it
# for this user, and otherwise mints a fresh one and overwrites $id_var and
# $secret_var. This is what keeps a second `docker compose up --build` from
# needing to rotate every credential the app or a host test run might
# already be using.
ensure_access_key() {
	user="$1"
	id_var="$2"
	secret_var="$3"

	eval "old_id=\${$id_var:-}"
	if has_access_key "$user" "$old_id"; then
		echo "provisioning: reusing existing access key for $user"
		return 0
	fi

	key=$(aws_root iam create-access-key --user-name "$user" --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text)
	new_id=$(echo "$key" | cut -f1)
	new_secret=$(echo "$key" | cut -f2)
	eval "$id_var=\$new_id"
	eval "$secret_var=\$new_secret"
	echo "provisioning: minted new access key for $user"
}

# Load whatever credentials a previous run already wrote, so ensure_access_key
# above can tell "still valid" apart from "needs a new one". Safe to source:
# both files are only ever written by this same script, in the KEY=VALUE
# shape read below.
for f in "$APP_CREDENTIALS_FILE" "$TEST_CREDENTIALS_FILE"; do
	if [ -f "$f" ]; then
		# shellcheck disable=SC1090
		. "$f"
	fi
done

# --- Application roles (mounted into the app container) ---

ensure_user gateway
GATEWAY_POLICY="$(policy "$(allow '["sqs:SendMessage"]' "$(arn "$IN_QUEUE")")")"
aws_root iam put-user-policy --user-name gateway --policy-name gateway-sqs --policy-document "$GATEWAY_POLICY" >/dev/null
ensure_access_key gateway GATEWAY_ACCESS_KEY_ID GATEWAY_SECRET_ACCESS_KEY

ensure_user consumer
CONSUMER_STMT="$(allow '["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:ChangeMessageVisibility","sqs:GetQueueUrl","sqs:GetQueueAttributes"]' "$(arn "$IN_QUEUE")"),$(allow '["sqs:SendMessage","sqs:GetQueueUrl"]' "$(arn "$DLQ_QUEUE")")"
aws_root iam put-user-policy --user-name consumer --policy-name consumer-sqs --policy-document "$(policy "$CONSUMER_STMT")" >/dev/null
ensure_access_key consumer SQS_CONSUMER_ACCESS_KEY_ID SQS_CONSUMER_SECRET_ACCESS_KEY

ensure_user publisher
PUBLISHER_POLICY="$(policy "$(allow '["sqs:SendMessage","sqs:GetQueueUrl"]' "$(arn "$OUT_QUEUE")")")"
aws_root iam put-user-policy --user-name publisher --policy-name publisher-sqs --policy-document "$PUBLISHER_POLICY" >/dev/null
ensure_access_key publisher SQS_PUBLISHER_ACCESS_KEY_ID SQS_PUBLISHER_SECRET_ACCESS_KEY

ensure_user events-reader
READER_POLICY="$(policy "$(allow '["sqs:ReceiveMessage","sqs:DeleteMessage"]' "$(arn "$OUT_QUEUE")")")"
aws_root iam put-user-policy --user-name events-reader --policy-name events-reader-sqs --policy-document "$READER_POLICY" >/dev/null
ensure_access_key events-reader EVENTS_READER_ACCESS_KEY_ID EVENTS_READER_SECRET_ACCESS_KEY

# --- IAM test fixtures (test-only, never mounted into the app) ---

ensure_user deny-probe
# Two separate policies on purpose - one Allow, one explicit Deny, on the
# same action and ARN - so the integration test can prove the explicit Deny
# always wins without ever mutating IAM itself at test time (which would
# need root). Order of attachment does not matter: IAM's evaluation always
# lets an explicit Deny beat any Allow.
DENY_PROBE_ALLOW="$(policy "$(allow '["sqs:SendMessage"]' "$(arn "$DENY_PROBE_QUEUE")")")"
DENY_PROBE_DENY="$(policy "$(deny '["sqs:SendMessage"]' "$(arn "$DENY_PROBE_QUEUE")")")"
aws_root iam put-user-policy --user-name deny-probe --policy-name deny-probe-allow --policy-document "$DENY_PROBE_ALLOW" >/dev/null
aws_root iam put-user-policy --user-name deny-probe --policy-name deny-probe-deny --policy-document "$DENY_PROBE_DENY" >/dev/null
ensure_access_key deny-probe DENY_PROBE_ACCESS_KEY_ID DENY_PROBE_SECRET_ACCESS_KEY

ensure_user redrive-tester
# DeleteMessage is scoped to the DLQ only, not the input queue: the test
# needs to remove the message it correlates in the DLQ after redrive so
# repeated runs do not accumulate residue there, but redrive itself must
# still happen through maxReceiveCount, never through the tester deleting
# the input queue's own message.
REDRIVE_TESTER_STMT="$(allow '["sqs:SendMessage","sqs:ReceiveMessage"]' "$(arn "$REDRIVE_IN_QUEUE")"),$(allow '["sqs:SendMessage","sqs:ReceiveMessage","sqs:DeleteMessage"]' "$(arn "$REDRIVE_DLQ_QUEUE")")"
aws_root iam put-user-policy --user-name redrive-tester --policy-name redrive-tester-sqs --policy-document "$(policy "$REDRIVE_TESTER_STMT")" >/dev/null
ensure_access_key redrive-tester REDRIVE_TESTER_ACCESS_KEY_ID REDRIVE_TESTER_SECRET_ACCESS_KEY

echo "provisioning: writing credential files"

# Least privilege for the app container: only the two roles it actually
# uses at runtime (consumer, publisher). Gateway, events-reader and every
# IAM test fixture key are deliberately kept out of this file - see
# docker-compose.yml, which mounts only this file into the app service, and
# deploy/docker/entrypoint.sh, which sources only this file's variable
# names.
umask 077
mkdir -p "$RUNTIME_DIR"
chmod 0700 "$RUNTIME_DIR"

cat >"$APP_CREDENTIALS_FILE" <<EOF
# Generated by deploy/ministack/provision.sh on every \`docker compose up\`.
# The application's own least-privilege MiniStack keys - consumer and
# publisher only, never root, never gateway/events-reader/test fixtures.
SQS_CONSUMER_ACCESS_KEY_ID=${SQS_CONSUMER_ACCESS_KEY_ID}
SQS_CONSUMER_SECRET_ACCESS_KEY=${SQS_CONSUMER_SECRET_ACCESS_KEY}
SQS_PUBLISHER_ACCESS_KEY_ID=${SQS_PUBLISHER_ACCESS_KEY_ID}
SQS_PUBLISHER_SECRET_ACCESS_KEY=${SQS_PUBLISHER_SECRET_ACCESS_KEY}
EOF
chmod 0600 "$APP_CREDENTIALS_FILE"
chown "${APP_RUNTIME_UID}:${APP_RUNTIME_GID}" "$APP_CREDENTIALS_FILE" 2>/dev/null || true

cat >"$TEST_CREDENTIALS_FILE" <<EOF
# Generated by deploy/ministack/provision.sh on every \`docker compose up\`.
# Test-only MiniStack keys and fixture names: never mounted into the app
# container, read only by the integration suite running on the host (or in
# a dedicated test container). Never root.
GATEWAY_ACCESS_KEY_ID=${GATEWAY_ACCESS_KEY_ID}
GATEWAY_SECRET_ACCESS_KEY=${GATEWAY_SECRET_ACCESS_KEY}
EVENTS_READER_ACCESS_KEY_ID=${EVENTS_READER_ACCESS_KEY_ID}
EVENTS_READER_SECRET_ACCESS_KEY=${EVENTS_READER_SECRET_ACCESS_KEY}
DENY_PROBE_ACCESS_KEY_ID=${DENY_PROBE_ACCESS_KEY_ID}
DENY_PROBE_SECRET_ACCESS_KEY=${DENY_PROBE_SECRET_ACCESS_KEY}
REDRIVE_TESTER_ACCESS_KEY_ID=${REDRIVE_TESTER_ACCESS_KEY_ID}
REDRIVE_TESTER_SECRET_ACCESS_KEY=${REDRIVE_TESTER_SECRET_ACCESS_KEY}
IAM_TEST_DENY_PROBE_QUEUE_NAME=${DENY_PROBE_QUEUE}
IAM_TEST_REDRIVE_INPUT_QUEUE_NAME=${REDRIVE_IN_QUEUE}
IAM_TEST_REDRIVE_DLQ_QUEUE_NAME=${REDRIVE_DLQ_QUEUE}
IAM_TEST_REDRIVE_MAX_RECEIVE_COUNT=${REDRIVE_MAX_RECEIVE_COUNT}
EOF
chmod 0600 "$TEST_CREDENTIALS_FILE"

echo "provisioning: done, credentials written to $APP_CREDENTIALS_FILE and $TEST_CREDENTIALS_FILE"
