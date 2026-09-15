#!/bin/sh
# Sources the app's own least-privilege SQS credentials (consumer,
# publisher) the provisioning service generated, if present, then execs
# wallet-service. The queues and IAM users only exist after the
# provisioning one-shot container has run, and its generated access keys
# are only known at that point - they cannot be baked into this image or
# the Compose file. depends_on with service_completed_successfully
# guarantees the file exists by the time this script runs for the app and
# migrate services. Only this one file is ever mounted here - gateway,
# events-reader and the IAM test fixture keys live in a separate file this
# container never sees, see provision.sh and docker-compose.yml.
set -eu

CREDENTIALS_FILE="${IAM_CREDENTIALS_FILE:-/shared/app-credentials.env}"
if [ -f "$CREDENTIALS_FILE" ]; then
	set -a
	# shellcheck disable=SC1090
	. "$CREDENTIALS_FILE"
	set +a
fi

exec wallet-service "$@"
