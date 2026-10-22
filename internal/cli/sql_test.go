package cli

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// This server replaces only the external database boundary. The actual CLI,
// configuration loader, pgx authentication, protocol handling and renderer run.
func databaseFixture(t *testing.T, respond func(*pgproto3.Backend, string), closeAfterSQL ...bool) identity {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	who, _ := fixture(t, strings.Replace(validConfig, "port = 5432", fmt.Sprintf("port = %d", port), 1))
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		backend := pgproto3.NewBackend(conn, conn)
		startup, err := backend.ReceiveStartupMessage()
		if err != nil {
			return
		}
		message, ok := startup.(*pgproto3.StartupMessage)
		if !ok {
			t.Error("client attempted SSL or another startup mode")
			return
		}
		if message.Parameters["user"] != "app_local" || message.Parameters["database"] != "app_local" {
			t.Error("startup identity did not come from profile")
			return
		}
		backend.Send(&pgproto3.AuthenticationCleartextPassword{})
		if backend.Flush() != nil {
			return
		}
		backend.SetAuthType(pgproto3.AuthTypeCleartextPassword)
		password, err := backend.Receive()
		if err != nil {
			return
		}
		if p, ok := password.(*pgproto3.PasswordMessage); !ok || p.Password != "secret-never-print" {
			t.Error("password did not come from profile")
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if backend.Flush() != nil {
			return
		}
		for {
			msg, err := backend.Receive()
			if err != nil {
				return
			}
			query, ok := msg.(*pgproto3.Query)
			if !ok {
				return
			}
			if query.String == "SELECT current_database(), current_user" {
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("current_database"), DataTypeOID: 25}, {Name: []byte("current_user"), DataTypeOID: 25}}})
				backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("app_local"), []byte("app_local")}})
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			} else {
				respond(backend, query.String)
			}
			if backend.Flush() != nil {
				return
			}
			if query.String != "SELECT current_database(), current_user" && len(closeAfterSQL) > 0 && closeAfterSQL[0] {
				return
			}
		}
	}()
	return who
}

func TestSQLReturnsRowsThroughCLI(t *testing.T) {
	who := databaseFixture(t, func(b *pgproto3.Backend, sql string) {
		if sql != "SELECT 42 AS answer" {
			t.Errorf("SQL changed: %q", sql)
		}
		b.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("answer"), DataTypeOID: 23}}})
		b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("42")}})
		b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	})
	code, stdout, stderr := invoke(t, who, "SELECT 42 AS answer")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"answer", "42", "SELECT 1", "rows_affected: 1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in %q", want, stdout)
		}
	}
}

func TestSQLRejectsInvalidArgumentsBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{}, {""}, {"  "}, {"SELECT 1", "SELECT 2"}, {"  \\! command"},
		{"SELECT 1", "--timeout", "0"}, {"--timeout=301", "SELECT 1"},
		{"--timeout=1s", "SELECT 1"}, {"--limit=0", "SELECT 1"},
		{"--limit=10001", "SELECT 1"}, {"--max-width=-1", "SELECT 1"},
		{"--max-width=10001", "SELECT 1"}, {"--profiles", "SELECT 1"},
	} {
		code, stdout, stderr := invoke(t, identity{home: "/not-a-config"}, args...)
		if code != 1 || stdout != "" || stderr == "" || strings.Contains(stderr, "configuration") {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestSQLLimitDrainsAllResultsAndTableWidth(t *testing.T) {
	const sql = "SELECT 'abcdef'; SELECT 'must-not-display'; UPDATE items SET active=false"
	who := databaseFixture(t, func(b *pgproto3.Backend, got string) {
		if got != sql {
			t.Errorf("SQL changed: %q", got)
		}
		for i, name := range []string{"first", "second"} {
			b.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte(name), DataTypeOID: 25}}})
			if i == 0 {
				b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("abcdef")}})
			} else {
				b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("must-not-display")}})
			}
			b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		}
		b.Send(&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 3")})
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	})
	code, stdout, stderr := invoke(t, who, "--limit=1", sql, "--max-width", "5")
	if code != 0 || !strings.Contains(stderr, "omitted") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{"abcd…", "UPDATE 3", "rows_affected: 3", "Result 3"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in %q", want, stdout)
		}
	}
	if strings.Contains(stdout, "must-not-display") {
		t.Fatalf("display limit ignored: %q", stdout)
	}
}

func TestConnectionLookingArgumentsAreOnlySQL(t *testing.T) {
	for _, sql := range []string{"host=secret-input", "postgres://secret-input"} {
		who := databaseFixture(t, func(b *pgproto3.Backend, got string) {
			if got != sql {
				t.Errorf("SQL changed: %q", got)
			}
			b.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "syntax error"})
			b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		})
		code, stdout, stderr := invoke(t, who, sql)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "SQLSTATE 42601") || strings.Contains(stderr, "secret-input") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	}
}

func TestSQLResultCapacityFailsWithoutPartialOutput(t *testing.T) {
	for name, value := range map[string]string{"retained": strings.Repeat("x", 16<<20), "rendered": strings.Repeat("\x1b", 5<<20)} {
		t.Run(name, func(t *testing.T) {
			who := databaseFixture(t, func(b *pgproto3.Backend, sql string) {
				b.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("value"), DataTypeOID: 25}}})
				b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(value)}})
				b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			})
			code, stdout, stderr := invoke(t, who, "SELECT large_value", "--max-width=0")
			if code != 1 || stdout != "" || !strings.Contains(stderr, "16 MiB") || !strings.Contains(stderr, "changes may already") {
				t.Fatalf("code=%d stdout bytes=%d stderr=%q", code, len(stdout), stderr)
			}
		})
	}
}

func TestSQLCommentsAndSemicolonsAreNotParsedByCLI(t *testing.T) {
	for _, sql := range []string{"-- comment\nSELECT ';' AS value", "DO $$ BEGIN PERFORM 1; END $$; SELECT 1"} {
		who := databaseFixture(t, func(b *pgproto3.Backend, got string) {
			if got != sql {
				t.Errorf("SQL was rewritten: %q", got)
			}
			b.Send(&pgproto3.CommandComplete{CommandTag: []byte("DO")})
			b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		})
		if code, out, err := invoke(t, who, sql); code != 0 {
			t.Fatalf("code=%d out=%q err=%q", code, out, err)
		}
	}
}

func TestSQLConnectionEnvironmentCannotOverrideProfile(t *testing.T) {
	who := databaseFixture(t, func(b *pgproto3.Backend, sql string) {
		b.Send(&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")})
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	})
	for _, name := range []string{"PGHOST", "PGPORT", "PGPASSWORD", "PGDATABASE", "PGUSER", "PGSERVICE", "PGSERVICEFILE", "PGPASSFILE", "PGSSLMODE", "PGOPTIONS"} {
		t.Setenv(name, "untrusted-do-not-use")
	}
	if code, out, err := invoke(t, who, "UPDATE items SET active=false"); code != 0 || !strings.Contains(out, "UPDATE 1") {
		t.Fatalf("code=%d out=%q err=%q", code, out, err)
	}
}

func TestSQLFailuresNeverPrintPartialRowsOrSecrets(t *testing.T) {
	for _, kind := range []string{"error", "disconnect", "copy-in", "copy-out", "open-transaction"} {
		t.Run(kind, func(t *testing.T) {
			who := databaseFixture(t, func(b *pgproto3.Backend, sql string) {
				b.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("partial"), DataTypeOID: 25}}})
				b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("must-not-print")}})
				switch kind {
				case "error":
					b.Send(&pgproto3.ErrorResponse{Code: "22012", Severity: "ERROR", Message: "secret-never-print\n\x1bserver-error"})
				case "disconnect":
					return
				case "copy-in":
					b.Send(&pgproto3.CopyInResponse{})
				case "copy-out":
					b.Send(&pgproto3.CopyOutResponse{})
				case "open-transaction":
					b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
					b.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
				}
				if kind == "error" {
					b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				}
			}, kind == "disconnect")
			code, out, err := invoke(t, who, "SELECT value", "--timeout=1")
			if code != 1 || out != "" || strings.Contains(err, "secret-never-print") || strings.ContainsRune(err, '\x1b') || !strings.Contains(err, "do not retry") {
				t.Fatalf("code=%d out=%q err=%q", code, out, err)
			}
			if kind == "error" && (!strings.Contains(err, "SQLSTATE 22012") || !strings.Contains(err, `\n\x1b`)) {
				t.Fatalf("unsafe or missing server error: %q", err)
			}
		})
	}
}

func TestSQLConnectionHasFiveSecondDeadline(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
	}()
	who, _ := fixture(t, strings.Replace(validConfig, "port = 5432", fmt.Sprintf("port = %d", listener.Addr().(*net.TCPAddr).Port), 1))
	start := time.Now()
	code, out, stderr := invoke(t, who, "SELECT 1", "--timeout=1")
	elapsed := time.Since(start)
	if code != 1 || out != "" || !strings.Contains(stderr, "timed out") || elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("deadline=%s code=%d out=%q err=%q", elapsed, code, out, stderr)
	}
}
