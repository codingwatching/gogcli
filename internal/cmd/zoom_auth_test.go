package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steipete/gogcli/internal/zoom"
)

func withTempZoomAuthStore(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GOG_KEYRING_BACKEND", "file")
	t.Setenv("GOG_KEYRING_PASSWORD", "test-pass")
	t.Setenv("GOG_ZOOM_ACCOUNT_ID", "")
	t.Setenv("GOG_ZOOM_CLIENT_ID", "")
	t.Setenv("GOG_ZOOM_CLIENT_SECRET", "")
	return xdg
}

func TestZoomAuthSetupCmd_StoresCredentialsWithoutValidation(t *testing.T) {
	withTempZoomAuthStore(t)
	cmd := &ZoomAuthSetupCmd{
		Alias:        "work",
		AccountID:    "acct",
		ClientID:     "client",
		ClientSecret: "secret",
		SkipValidate: true,
	}
	if err := cmd.Run(newCmdJSONContext(t), &RootFlags{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	creds, err := zoom.LoadCredentials("work")
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.AccountID != "acct" || creds.ClientID != "client" || creds.ClientSecret != "secret" {
		t.Fatalf("unexpected creds: %#v", creds)
	}
}

func TestZoomAuthDoctorCmd_NoCredentials(t *testing.T) {
	withTempZoomAuthStore(t)
	if err := (&ZoomAuthDoctorCmd{}).Run(newCmdJSONContext(t), &RootFlags{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestZoomAuthSetupCmd_NoInputRequiresFlags(t *testing.T) {
	withTempZoomAuthStore(t)
	err := (&ZoomAuthSetupCmd{SkipValidate: true}).Run(context.Background(), &RootFlags{NoInput: true})
	if err == nil {
		t.Fatalf("expected usage error")
	}
}

func TestZoomAuthSetupCmd_DryRunDoesNotStoreCredentials(t *testing.T) {
	xdg := withTempZoomAuthStore(t)
	cmd := &ZoomAuthSetupCmd{
		Alias:        "dry",
		SkipValidate: true,
	}
	out := captureStdout(t, func() {
		err := cmd.Run(newCmdJSONContext(t), &RootFlags{DryRun: true, NoInput: true})
		if ExitCode(err) != 0 {
			t.Fatalf("expected dry-run exit 0, got %v", err)
		}
	})
	if strings.Contains(out, "topsecretvalue") {
		t.Fatalf("dry-run output leaked client secret: %s", out)
	}
	var parsed struct {
		DryRun  bool `json:"dry_run"`
		Request struct {
			Alias           string `json:"alias"`
			ClientSecretSet bool   `json:"client_secret_set"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse dry-run JSON: %v\n%s", err, out)
	}
	if !parsed.DryRun || parsed.Request.Alias != "dry" || parsed.Request.ClientSecretSet {
		t.Fatalf("unexpected dry-run payload: %#v", parsed)
	}
	if _, err := os.Stat(filepath.Join(xdg, "gogcli")); err == nil {
		t.Fatalf("dry-run created config directory")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat config directory: %v", err)
	}
}
