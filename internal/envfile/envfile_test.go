package envfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/envfile"
)

func writeFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestRead_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.env")

	_, err := envfile.Read(path)
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention path %q", err.Error(), path)
	}
	if strings.Contains(err.Error(), "scripts/wait-for-integration.sh") {
		t.Errorf("error %q suggests the test-only helper script, want a neutral production message", err.Error())
	}
}

func TestRead_CommentAndBlankLineIgnored(t *testing.T) {
	path := writeFile(t, "# a comment\n\nGATEWAY_ACCESS_KEY_ID=AKIAEXAMPLE\n")

	values, err := envfile.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("want 1 value, got %d: %v", len(values), values)
	}
	if got := values["GATEWAY_ACCESS_KEY_ID"]; got != "AKIAEXAMPLE" {
		t.Errorf("GATEWAY_ACCESS_KEY_ID = %q, want %q", got, "AKIAEXAMPLE")
	}
}

func TestRead_ValueWithEqualsSpaceQuotesAndDollar(t *testing.T) {
	path := writeFile(t, `SQS_CONSUMER_SECRET_ACCESS_KEY=abc=def "quoted value" $NOT_EXPANDED`+"\n")

	values, err := envfile.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := `abc=def "quoted value" $NOT_EXPANDED`
	if got := values["SQS_CONSUMER_SECRET_ACCESS_KEY"]; got != want {
		t.Errorf("SQS_CONSUMER_SECRET_ACCESS_KEY = %q, want %q", got, want)
	}
}

func TestRead_LineWithoutEquals(t *testing.T) {
	path := writeFile(t, "GATEWAY_ACCESS_KEY_ID=AKIAEXAMPLE\nSQS_CONSUMER_SECRET_ACCESS_KEY topsecretvalue\n")

	_, err := envfile.Read(path)
	if err == nil {
		t.Fatal("want error for line without '=', got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not mention path %q", msg, path)
	}
	if !strings.Contains(msg, "2") {
		t.Errorf("error %q does not mention line number 2", msg)
	}
	if strings.Contains(msg, "topsecretvalue") {
		t.Errorf("error %q leaks the value, want it omitted", msg)
	}
}

func TestRead_InvalidKey(t *testing.T) {
	path := writeFile(t, "gateway-access-key-id=AKIAEXAMPLE\n")

	_, err := envfile.Read(path)
	if err == nil {
		t.Fatal("want error for invalid key, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not mention path %q", msg, path)
	}
	if !strings.Contains(msg, "1") {
		t.Errorf("error %q does not mention line number 1", msg)
	}
	if strings.Contains(msg, "AKIAEXAMPLE") {
		t.Errorf("error %q leaks the value, want it omitted", msg)
	}
}
