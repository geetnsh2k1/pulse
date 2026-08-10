// Package awscfg is pulse's single door to a real AWS account: it finds the
// caller's profiles, resolves credentials, and turns the SDK's error prose
// into messages that name the fix. Everything here is read-only — no code
// path in this package mutates anything in AWS.
package awscfg

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile is one entry from the shared AWS config/credentials files. Only
// names and non-secret hints are ever collected — never keys or tokens.
type Profile struct {
	Name      string
	Region    string // as declared in ~/.aws/config, if any
	Source    string // "config", "credentials", or "config+credentials"
	SSO       bool   // uses IAM Identity Center (needs `aws sso login`)
	AssumeRor bool   // assumes a role (role_arn set)
}

// ConfigPath and CredentialsPath honor the standard AWS env overrides.
func ConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".aws", "config")
}

func CredentialsPath() string {
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".aws", "credentials")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

// Profiles lists every profile pulse can see, sorted with "default" first.
// Missing files are not an error: a machine with no AWS setup simply has no
// profiles, and the caller explains what to do about it.
func Profiles() ([]Profile, error) {
	byName := map[string]*Profile{}

	// ~/.aws/config uses [profile name]; the default profile is just [default].
	cfgSections, err := parseINI(ConfigPath())
	if err != nil {
		return nil, err
	}
	for _, s := range cfgSections {
		name := s.name
		if rest, ok := strings.CutPrefix(name, "profile "); ok {
			name = strings.TrimSpace(rest)
		} else if name != "default" {
			continue // [sso-session x], [services x] and friends aren't profiles
		}
		p := ensure(byName, name)
		p.Source = "config"
		p.Region = s.keys["region"]
		p.SSO = s.keys["sso_start_url"] != "" || s.keys["sso_session"] != "" || s.keys["sso_account_id"] != ""
		p.AssumeRor = s.keys["role_arn"] != ""
	}

	// ~/.aws/credentials uses bare [name] sections.
	credSections, err := parseINI(CredentialsPath())
	if err != nil {
		return nil, err
	}
	for _, s := range credSections {
		p := ensure(byName, s.name)
		if p.Source == "config" {
			p.Source = "config+credentials"
		} else {
			p.Source = "credentials"
		}
	}

	out := make([]Profile, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name == "default") != (out[j].Name == "default") {
			return out[i].Name == "default"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ProfileNames is the convenience form used by pickers and completion.
func ProfileNames() ([]string, error) {
	ps, err := Profiles()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return names, nil
}

func ensure(m map[string]*Profile, name string) *Profile {
	if p, ok := m[name]; ok {
		return p
	}
	p := &Profile{Name: name}
	m[name] = p
	return p
}

type iniSection struct {
	name string
	keys map[string]string
}

// parseINI reads the small subset of INI the AWS files use. It is
// deliberately forgiving: an unreadable or malformed line never stops pulse
// from working, because AWS config is optional. A missing file yields nil.
func parseINI(path string) ([]iniSection, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var (
		out []iniSection
		cur *iniSection
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			out = append(out, iniSection{
				name: strings.TrimSpace(line[1 : len(line)-1]),
				keys: map[string]string{},
			})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue // stray key before any section
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur.keys[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out, sc.Err()
}
