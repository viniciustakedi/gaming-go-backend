package pg

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func writeCredentialsFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture credentials file: %v", err)
	}
	return path
}

func TestAppDSN_SwapsUserinfoForWalletApp(t *testing.T) {
	t.Parallel()

	path := writeCredentialsFile(t, "# generated\nWALLET_APP_PASSWORD=s3cr3t\n")

	dsn, err := AppDSN("postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable", path)
	if err != nil {
		t.Fatalf("AppDSN() error = %v", err)
	}
	want := "postgres://wallet_app:s3cr3t@localhost:5432/wallet?sslmode=disable"
	if dsn != want {
		t.Errorf("AppDSN() = %q, want %q", dsn, want)
	}
}

func TestAppDSN_MissingPasswordFails(t *testing.T) {
	t.Parallel()

	path := writeCredentialsFile(t, "SOME_OTHER_KEY=value\n")

	if _, err := AppDSN("postgres://wallet:wallet@localhost:5432/wallet", path); err == nil {
		t.Fatal("want error when WALLET_APP_PASSWORD is absent, got nil")
	}
}

func TestAppDSN_MissingFileFails(t *testing.T) {
	t.Parallel()

	if _, err := AppDSN("postgres://wallet:wallet@localhost:5432/wallet", filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("want error when the credentials file does not exist, got nil")
	}
}

func TestAppDSN_PasswordWithBase64URLSafeCharsPreserved(t *testing.T) {
	t.Parallel()

	// provision.sh generates WALLET_APP_PASSWORD from base64 with '+/'
	// mapped to '-_' and '=' padding stripped, so the alphabet a real
	// password is drawn from overlaps heavily with these; envfile.Read
	// treats the value as a literal, and dsn.go must round-trip it exactly
	// rather than trimming or otherwise reinterpreting it.
	password := "ab=cd+ef/gh-ij"
	path := writeCredentialsFile(t, "WALLET_APP_PASSWORD="+password+"\n")

	dsn, err := AppDSN("postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable", path)
	if err != nil {
		t.Fatalf("AppDSN() error = %v", err)
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", dsn, err)
	}
	got, ok := parsed.User.Password()
	if !ok {
		t.Fatalf("AppDSN() dsn %q carries no password", dsn)
	}
	if got != password {
		t.Errorf("AppDSN() password = %q, want %q", got, password)
	}
}

func TestAppDSN_InvalidOwnerDSNFails(t *testing.T) {
	t.Parallel()

	path := writeCredentialsFile(t, "WALLET_APP_PASSWORD=s3cr3t\n")

	if _, err := AppDSN("://not a url", path); err == nil {
		t.Fatal("want error for an unparseable DSN, got nil")
	}
}
