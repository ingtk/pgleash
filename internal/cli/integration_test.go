//go:build integration

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

func postgresFixture(t *testing.T, version string) (identity, int) {
	t.Helper()
	output, err := exec.Command("docker", "run", "--rm", "-d", "--label", "pgleash.integration=true", "-e", "POSTGRES_USER=app_local", "-e", "POSTGRES_DB=app_local", "-e", "POSTGRES_PASSWORD=secret-never-print", "-p", "127.0.0.1::5432", "postgres:"+version).CombinedOutput()
	if err != nil {
		t.Fatalf("start test-owned PostgreSQL: %v: %s", err, output)
	}
	id := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatalf("unexpected container identity %q", id)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "--force", "--volumes", id).CombinedOutput(); err != nil {
			t.Errorf("remove test container: %v: %s", err, out)
		}
	})
	output, err = exec.Command("docker", "port", id, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("test port: %v: %s", err, output)
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(string(output)))
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("unexpected test binding %q", output)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	who, _ := fixture(t, strings.Replace(validConfig, "port = 5432", fmt.Sprintf("port = %d", port), 1))
	deadline := time.Now().Add(40 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cfg, err := pgconn.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=app_local password=secret-never-print dbname=app_local sslmode=disable passfile=''", port))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		conn, err := pgconn.ConnectConfig(ctx, cfg)
		if err == nil {
			conn.Close(ctx)
			cancel()
			break
		}
		cancel()
		if time.Now().After(deadline) {
			t.Fatalf("PostgreSQL did not become ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return who, port
}

func TestPostgresIntegration(t *testing.T) {
	for _, version := range []string{"14", "18"} {
		t.Run(version, func(t *testing.T) {
			who, port := postgresFixture(t, version)
			check := func(t *testing.T, sql string, code int, wants ...string) (string, string) {
				t.Helper()
				got, out, err := invoke(t, who, sql)
				if got != code {
					t.Fatalf("code=%d want %d stdout=%q stderr=%q", got, code, out, err)
				}
				if code != 0 && out != "" {
					t.Fatalf("partial output on failure: %q", out)
				}
				for _, want := range wants {
					if !strings.Contains(out+err, want) {
						t.Fatalf("missing %q stdout=%q stderr=%q", want, out, err)
					}
				}
				return out, err
			}
			t.Run("multiple-results-and-write", func(t *testing.T) {
				check(t, "CREATE TABLE items(id int PRIMARY KEY, active bool DEFAULT true); INSERT INTO items(id) VALUES(1) RETURNING id; SELECT 'first' AS label; SELECT 'second' AS label", 0, "CREATE TABLE", "INSERT 0 1", "first", "second", "Result 4")
				check(t, "UPDATE items SET active=false WHERE id=1; DELETE FROM items WHERE id=1", 0, "UPDATE 1", "DELETE 1")
				check(t, "SELECT 'empty' WHERE NOT EXISTS(SELECT FROM items)", 0, "empty")
			})
			t.Run("explicit-transaction", func(t *testing.T) {
				check(t, "BEGIN; INSERT INTO items(id) VALUES(2); COMMIT", 0, "BEGIN", "COMMIT")
				check(t, "SELECT 'persisted' WHERE EXISTS(SELECT FROM items WHERE id=2)", 0, "persisted")
			})
			t.Run("implicit-error-rolls-back", func(t *testing.T) {
				check(t, "INSERT INTO items(id) VALUES(3); SELECT 1/0", 1, "SQLSTATE 22012", "do not retry")
				check(t, "SELECT 'rolled-back' WHERE NOT EXISTS(SELECT FROM items WHERE id=3)", 0, "rolled-back")
			})
			t.Run("prior-commit-survives-error", func(t *testing.T) {
				check(t, "BEGIN; INSERT INTO items(id) VALUES(4); COMMIT; SELECT 1/0", 1, "SQLSTATE 22012", "changes may already")
				check(t, "SELECT 'committed' WHERE EXISTS(SELECT FROM items WHERE id=4)", 0, "committed")
			})
			t.Run("open-transaction-rolls-back", func(t *testing.T) {
				check(t, "BEGIN; INSERT INTO items(id) VALUES(5)", 1, "transaction left open")
				check(t, "SELECT 'not-committed' WHERE NOT EXISTS(SELECT FROM items WHERE id=5)", 0, "not-committed")
			})
			t.Run("display-limit-keeps-later-effects", func(t *testing.T) {
				code, out, err := invoke(t, who, "SELECT generate_series(1,5); INSERT INTO items(id) VALUES(6) RETURNING id", "--limit=2")
				if code != 0 || !strings.Contains(err, "omitted") || !strings.Contains(out, "INSERT 0 1") {
					t.Fatalf("code=%d out=%q err=%q", code, out, err)
				}
				check(t, "SELECT 'later-insert' WHERE EXISTS(SELECT FROM items WHERE id=6)", 0, "later-insert")
			})
			t.Run("null-empty-width-and-controls", func(t *testing.T) {
				code, out, err := invoke(t, who, `SELECT NULL::text AS n, ''::text AS e, 'あいうえおか' AS txt, E'\033[31m\n' AS ctrl`, "--max-width=5")
				if code != 0 || err != "" || !strings.Contains(out, "NULL") || !strings.Contains(out, "あいうえ…") || strings.ContainsRune(out, '\x1b') {
					t.Fatalf("code=%d out=%q err=%q", code, out, err)
				}
			})
			t.Run("copy-client-streaming-rejected", func(t *testing.T) {
				for _, sql := range []string{"COPY items FROM STDIN", "COPY items TO STDOUT"} {
					start := time.Now()
					check(t, sql, 1, "COPY client streaming")
					if time.Since(start) > 2*time.Second {
						t.Fatal("COPY did not fail promptly")
					}
				}
				check(t, "COPY (SELECT 42) TO '/tmp/pgleash-server-copy'", 0, "COPY 1")
			})
			t.Run("server-and-client-timeout", func(t *testing.T) {
				for _, sql := range []string{"SELECT pg_sleep(3)", "SET statement_timeout=0; SELECT pg_sleep(3)"} {
					start := time.Now()
					code, out, err := invoke(t, who, sql, "--timeout=1")
					if code != 1 || out != "" || (!strings.Contains(err, "timed out") && !strings.Contains(err, "57014")) || time.Since(start) > 3*time.Second {
						t.Fatalf("code=%d out=%q err=%q", code, out, err)
					}
				}
			})
			t.Run("retained-capacity", func(t *testing.T) { check(t, "SELECT repeat('x',16777216)", 1, "16 MiB") })
			t.Run("password-env-and-passfile-not-used", func(t *testing.T) {
				other, path := fixture(t, strings.Replace(strings.Replace(validConfig, "port = 5432", fmt.Sprintf("port = %d", port), 1), "password = \"secret-never-print\"", "password = \"\"", 1))
				passfile := path + ".pgpass"
				if err := os.WriteFile(passfile, []byte("*:*:*:*:secret-never-print\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PGPASSWORD", "secret-never-print")
				t.Setenv("PGPASSFILE", passfile)
				code, out, err := invoke(t, other, "SELECT 1")
				if code != 1 || out != "" || strings.Contains(err, "secret-never-print") {
					t.Fatalf("external password was used or leaked: code=%d out=%q err=%q", code, out, err)
				}
			})
			t.Run("database-identity-mismatch", func(t *testing.T) {
				proxy := databaseProxy(t, port)
				mismatch, _ := fixture(t, strings.Replace(validConfig, "port = 5432", fmt.Sprintf("port = %d", proxy), 1))
				code, out, err := invoke(t, mismatch, "CREATE TABLE pgleash_must_not_exist(id int)")
				if code != 1 || out != "" || !strings.Contains(err, "does not match profile") {
					t.Fatalf("code=%d out=%q err=%q", code, out, err)
				}
				postgres, _ := fixture(t, strings.Replace(strings.Replace(validConfig, "port = 5432", fmt.Sprintf("port = %d", port), 1), "database = \"app_local\"", "database = \"postgres\"", 1))
				code, out, err = invoke(t, postgres, "SELECT 'not-executed' WHERE to_regclass('pgleash_must_not_exist') IS NULL")
				if code != 0 || !strings.Contains(out, "not-executed") {
					t.Fatalf("unverified SQL executed: code=%d out=%q err=%q", code, out, err)
				}
			})
		})
	}
}

// A test-owned proxy changes only the startup database, simulating an endpoint
// that authenticates the requested user but routes to a different database.
func databaseProxy(t *testing.T, targetPort int) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		client.SetDeadline(time.Now().Add(10 * time.Second))
		startup, err := pgproto3.NewBackend(client, client).ReceiveStartupMessage()
		if err != nil {
			return
		}
		message, ok := startup.(*pgproto3.StartupMessage)
		if !ok {
			return
		}
		message.Parameters["database"] = "postgres"
		server, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", targetPort), time.Second)
		if err != nil {
			return
		}
		defer server.Close()
		server.SetDeadline(time.Now().Add(10 * time.Second))
		encoded, err := message.Encode(nil)
		if err != nil {
			return
		}
		if _, err = server.Write(encoded); err != nil {
			return
		}
		go func() { io.Copy(server, client); server.(*net.TCPConn).CloseWrite() }()
		io.Copy(client, server)
	}()
	return listener.Addr().(*net.TCPAddr).Port
}
