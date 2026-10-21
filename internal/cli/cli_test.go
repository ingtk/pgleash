package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHelpNeedsNoConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--help"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"Usage:", "--profiles", "--help"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help missing %q: %s", want, stdout.String())
		}
	}
}

const validConfig = `default_profile = "dev"
[profiles.dev]
port = 5432
user = "app_local"
password = "secret-never-print"
database = "app_local"
[profiles.alpha]
port = 6543
user = "other_user"
password = "other-secret"
database = "other_db"
`

func fixture(t *testing.T, content string) (identity, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "pgleash")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return identity{home: home, uid: os.Geteuid()}, path
}

func invoke(t *testing.T, who identity, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, func() (identity, error) { return who, nil })
	return code, stdout.String(), stderr.String()
}

func TestProfilesAreSortedAndNeverExposePasswords(t *testing.T) {
	who, _ := fixture(t, validConfig)
	code, stdout, stderr := invoke(t, who, "--profiles")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected table: %q", stdout)
	}
	for i, want := range [][]string{
		{"NAME", "PORT", "USER", "DATABASE"},
		{"alpha", "6543", "other_user", "other_db"},
		{"dev", "5432", "app_local", "app_local"},
	} {
		if strings.Join(strings.Fields(lines[i]), "|") != strings.Join(want, "|") {
			t.Errorf("row %d = %q, want %v", i, lines[i], want)
		}
	}
	for _, secret := range []string{"secret-never-print", "other-secret", "password"} {
		if strings.Contains(strings.ToLower(stdout+stderr), secret) {
			t.Errorf("output disclosed %q", secret)
		}
	}
}

func TestProfilesRefuseUntrustedConfigurationFiles(t *testing.T) {
	for _, mode := range []os.FileMode{0640, 0604, 0666, 0700, 0000, 0600 | os.ModeSetuid, 0600 | os.ModeSetgid, 0600 | os.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			who, path := fixture(t, validConfig)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != mode {
				t.Skip("filesystem does not retain the requested special permission bits")
			}
			code, stdout, stderr := invoke(t, who, "--profiles")
			if code != 1 || stdout != "" || stderr == "" {
				t.Fatalf("untrusted file accepted: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
	for _, mode := range []os.FileMode{0400, 0600} {
		t.Run("allow-"+mode.String(), func(t *testing.T) {
			who, path := fixture(t, validConfig)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := invoke(t, who, "--profiles"); code != 0 {
				t.Fatalf("trusted file rejected: %s", stderr)
			}
		})
	}
}

func TestProfilesRejectInvalidConfigurationWithoutLeakingValues(t *testing.T) {
	for name, content := range map[string]string{
		"empty":                 "",
		"malformed":             "password = \"secret-never-print\"\n[broken",
		"unknown-root":          "host = \"secret-never-print\"\n" + validConfig,
		"unknown-profile":       validConfig + "host = \"secret-never-print\"\n",
		"unknown-password-typo": strings.Replace(validConfig, "password =", "pasword =", 1),
		"missing-password":      strings.Replace(validConfig, "password = \"secret-never-print\"\n", "", 1),
		"missing-user":          strings.Replace(validConfig, "user = \"app_local\"\n", "", 1),
		"missing-database":      strings.Replace(validConfig, "database = \"app_local\"\n", "", 1),
		"missing-port":          strings.Replace(validConfig, "port = 5432\n", "", 1),
		"port-zero":             strings.Replace(validConfig, "port = 5432", "port = 0", 1),
		"port-large":            strings.Replace(validConfig, "port = 5432", "port = 65536", 1),
		"port-negative":         strings.Replace(validConfig, "port = 5432", "port = -1", 1),
		"port-string":           strings.Replace(validConfig, "port = 5432", "port = \"secret-never-print\"", 1),
		"empty-user":            strings.Replace(validConfig, "user = \"app_local\"", "user = \"\"", 1),
		"empty-database":        strings.Replace(validConfig, "database = \"app_local\"", "database = \" \"", 1),
		"empty-name":            strings.Replace(validConfig, "profiles.alpha", "profiles.\"\"", 1),
		"empty-default":         strings.Replace(validConfig, "default_profile = \"dev\"", "default_profile = \"\"", 1),
		"unknown-default":       strings.Replace(validConfig, "default_profile = \"dev\"", "default_profile = \"unknown\"", 1),
		"wrong-default-type":    strings.Replace(validConfig, "default_profile = \"dev\"", "default_profile = 123", 1),
		"duplicate-key":         validConfig + "port = 9999\n",
	} {
		t.Run(name, func(t *testing.T) {
			who, _ := fixture(t, content)
			code, stdout, stderr := invoke(t, who, "--profiles")
			if code != 1 || stdout != "" || stderr == "" {
				t.Fatalf("invalid config accepted: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if strings.Contains(stdout+stderr, "secret-never-print") || strings.Contains(stdout+stderr, "other-secret") {
				t.Fatal("configuration error leaked a value")
			}
		})
	}
}

func TestProfilesAcceptDefaultFallbackAndExplicitEmptyPassword(t *testing.T) {
	content := strings.Replace(validConfig, "default_profile = \"dev\"\n", "", 1)
	content = strings.Replace(content, "secret-never-print", "", 1)
	who, _ := fixture(t, content)
	if code, _, stderr := invoke(t, who, "--profiles"); code != 0 {
		t.Fatalf("omitted default or empty password rejected: %s", stderr)
	}
}

func TestProfileAndTableOptions(t *testing.T) {
	who, _ := fixture(t, validConfig)
	for _, args := range [][]string{
		{"--profiles", "--profile", "alpha"},
		{"--profile=dev", "--profiles"},
		{"--format", "table", "--profiles"},
		{"--profiles", "--format=table"},
	} {
		if code, _, stderr := invoke(t, who, args...); code != 0 {
			t.Errorf("%v: %s", args, stderr)
		}
	}
	code, stdout, stderr := invoke(t, who, "--profiles", "--profile", "unknown")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "profile does not exist") {
		t.Fatalf("unknown selection: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestInvalidArgumentsNeverDiscloseTheirValues(t *testing.T) {
	who, _ := fixture(t, validConfig)
	for _, args := range [][]string{
		nil, {"-h"}, {"--host", "secret-input"}, {"--port", "9999"},
		{"--dsn=postgres://secret-input"}, {"--config", "secret-input"},
		{"--profiles", "--password=secret-input"}, {"host=secret-input"},
		{"postgres://secret-input"}, {"--help", "secret-input"},
		{"--profiles", "--help"}, {"--profiles=true"}, {"--help=true"},
		{"--profiles", "--profile"}, {"--profiles", "--profile="},
		{"--profiles", "--format"}, {"--profiles", "--format=json"},
		{"--profiles", "--format=csv"}, {"--profiles", "--format=secret-input"},
		{"--profiles", "extra"}, {"--profiles", "--unknown"},
	} {
		code, stdout, stderr := invoke(t, who, args...)
		if code != 1 || stdout != "" || stderr == "" || strings.Contains(stderr, "secret-input") {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestProfileTableEscapesTerminalControls(t *testing.T) {
	content := `default_profile = "dev"
[profiles.dev]
port = 5432
user = "line\n\t\u001b[31m\u202e"
password = "secret-never-print"
database = "db\r\u0008\u2028"
[profiles."hidden\u001b"]
port = 5433
user = "u"
password = "secret-never-print"
database = "d"
`
	who, _ := fixture(t, content)
	code, stdout, stderr := invoke(t, who, "--profiles")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(strings.Split(strings.TrimSpace(stdout), "\n")) != 3 {
		t.Fatalf("cell escaped the table row: %q", stdout)
	}
	for _, want := range []string{`line\n\t\x1b[31m\u202e`, `db\r\b\u2028`, `hidden\x1b`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing visible control %q in %q", want, stdout)
		}
	}
	if strings.ContainsAny(stdout, "\x1b\r\b\u202e\u2028") {
		t.Fatalf("raw terminal controls in %q", stdout)
	}
}

func TestProfilesIgnoreEnvironmentConfigurationPaths(t *testing.T) {
	who, _ := fixture(t, validConfig)
	other, otherPath := fixture(t, strings.ReplaceAll(validConfig, "app_local", "UNTRUSTED"))
	t.Setenv("HOME", other.home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(other.home, ".config"))
	t.Setenv("PGLEASH_CONFIG", otherPath)
	t.Setenv("PGPASSFILE", otherPath)
	t.Setenv("PGSERVICEFILE", otherPath)
	t.Setenv("PGHOST", "must-not-connect.invalid")
	code, stdout, stderr := invoke(t, who, "--profiles")
	if code != 0 || stderr != "" || strings.Contains(stdout, "UNTRUSTED") || !strings.Contains(stdout, "app_local") {
		t.Fatalf("untrusted environment affected config: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if os.Getenv("HOME") != other.home {
		t.Fatal("CLI modified HOME")
	}
}

func TestHelpDoesNotNeedUserLookup(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr, func() (identity, error) {
		return identity{}, errors.New("secret-user-lookup-error")
	})
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestProfilesRefuseMissingNonregularOrForeignConfiguration(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "fifo", "foreign", "relative-home", "symlink-home"} {
		t.Run(kind, func(t *testing.T) {
			who, path := fixture(t, validConfig)
			switch kind {
			case "missing", "directory", "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "fifo" {
					if err := unix.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign":
				who.uid++
			case "relative-home":
				who.home = "relative"
			case "symlink-home":
				link := who.home + ".link"
				if err := os.Symlink(who.home, link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(link) })
				who.home = link
			}
			done := make(chan string, 1)
			go func() {
				var stdout, stderr bytes.Buffer
				code := run([]string{"--profiles"}, &stdout, &stderr, func() (identity, error) { return who, nil })
				if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
					done <- "untrusted configuration was not rejected"
					return
				}
				done <- ""
			}()
			select {
			case err := <-done:
				if err != "" {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("opening nonregular configuration blocked")
			}
		})
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) {
	return 0, errors.New("secret-output-error")
}

func TestOutputErrorsFailWithoutLeakingUnderlyingError(t *testing.T) {
	who, _ := fixture(t, validConfig)
	for _, args := range [][]string{{"--profiles"}, {"--help"}} {
		var stderr bytes.Buffer
		code := run(args, failedWriter{}, &stderr, func() (identity, error) { return who, nil })
		if code != 1 || !strings.Contains(stderr.String(), "cannot write output") || strings.Contains(stderr.String(), "secret-output-error") {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	}
}

func TestProfilesRefuseSymlinksAndWritableParents(t *testing.T) {
	for _, component := range []string{"config.toml", "pgleash", ".config"} {
		t.Run("symlink-"+component, func(t *testing.T) {
			who, path := fixture(t, validConfig)
			switch component {
			case "pgleash":
				path = filepath.Dir(path)
			case ".config":
				path = filepath.Join(who.home, ".config")
			}
			if err := os.Rename(path, path+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".real", path); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := invoke(t, who, "--profiles")
			if code != 1 || stdout != "" || stderr == "" {
				t.Fatalf("symlink accepted: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
	for _, component := range []string{"home", ".config", "pgleash"} {
		t.Run("writable-"+component, func(t *testing.T) {
			who, path := fixture(t, validConfig)
			switch component {
			case "home":
				path = who.home
			case ".config":
				path = filepath.Join(who.home, ".config")
			default:
				path = filepath.Dir(path)
			}
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := invoke(t, who, "--profiles")
			if code != 1 || stdout != "" || stderr == "" {
				t.Fatalf("writable parent accepted: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestStartupRemovesPGEnvironment(t *testing.T) {
	for _, name := range []string{"PGPASSWORD", "PGPASSFILE", "PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PG_FUTURE_SETTING"} {
		t.Setenv(name, "must-not-be-used")
	}
	t.Setenv("PGLEASH_CONFIG", "/untrusted/config.toml")
	t.Setenv("OTHER_SETTING", "keep")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PG") {
			t.Errorf("PG environment survived startup: %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
	if os.Getenv("OTHER_SETTING") != "keep" {
		t.Fatal("unrelated environment was modified")
	}
}
