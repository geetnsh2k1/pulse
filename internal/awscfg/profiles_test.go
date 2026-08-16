package awscfg

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAWSFiles points the package at throwaway config/credentials files.
func writeAWSFiles(t *testing.T, cfg, creds string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	credPath := filepath.Join(dir, "credentials")
	if cfg != "" {
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if creds != "" {
		if err := os.WriteFile(credPath, []byte(creds), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credPath)
}

func TestProfilesParsesBothFiles(t *testing.T) {
	writeAWSFiles(t, `
# a comment
[default]
region = us-east-1

[profile prod]
region = eu-west-1
role_arn = arn:aws:iam::111122223333:role/Admin
source_profile = default

[profile sso-dev]
sso_session = corp
sso_account_id = 444455556666

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start

[services my-services]
lambda =
`, `
[default]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secret

[legacy]
aws_access_key_id = AKIAOLD
`)

	got, err := Profiles()
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}

	// default first, then alphabetical; non-profile sections excluded.
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	want := []string{"default", "legacy", "prod", "sso-dev"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}

	byName := map[string]Profile{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if p := byName["default"]; p.Region != "us-east-1" || p.Source != "config+credentials" {
		t.Errorf("default = %+v, want region us-east-1 and both sources", p)
	}
	if p := byName["prod"]; p.Region != "eu-west-1" || !p.AssumeRor || p.SSO {
		t.Errorf("prod = %+v, want eu-west-1 + assumes role", p)
	}
	if p := byName["sso-dev"]; !p.SSO {
		t.Errorf("sso-dev = %+v, want SSO detected", p)
	}
	if p := byName["legacy"]; p.Source != "credentials" {
		t.Errorf("legacy = %+v, want source credentials", p)
	}
}

func TestProfilesNoFilesIsNotAnError(t *testing.T) {
	writeAWSFiles(t, "", "") // neither file written
	got, err := Profiles()
	if err != nil {
		t.Fatalf("a machine with no AWS setup must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no profiles, got %v", got)
	}
}

func TestProfilesToleratesJunk(t *testing.T) {
	writeAWSFiles(t, "key_before_any_section = 1\n[profile ok]\nregion=us-east-2\n[malformed\n", "")
	got, err := Profiles()
	if err != nil {
		t.Fatalf("malformed config must not break pulse: %v", err)
	}
	if len(got) == 0 || got[0].Name != "ok" {
		t.Fatalf("expected to still find profile ok, got %+v", got)
	}
}

func TestPathsHonorEnvOverrides(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", "/custom/cfg")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/custom/creds")
	if ConfigPath() != "/custom/cfg" || CredentialsPath() != "/custom/creds" {
		t.Errorf("env overrides ignored: %s %s", ConfigPath(), CredentialsPath())
	}
}
