package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

type cell struct {
	text string
	null bool
}

const resultCapacity = 16 << 20

var errResultCapacity = errors.New("result exceeds 16 MiB capacity")

type statementResult struct {
	fields []pgproto3.FieldDescription
	rows   [][]cell
	tag    string
}
type sqlBatch struct {
	results []statementResult
	omitted bool
}
type sqlFailure struct {
	cause           error
	possibleChanges bool
}

func (e *sqlFailure) Error() string { return e.cause.Error() }
func (e *sqlFailure) Unwrap() error { return e.cause }

func sqlDiagnostic(err error, password string) string {
	message := err.Error()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		message = "SQLSTATE " + pgErr.Code + ": " + pgErr.Message
	}
	if password != "" {
		message = strings.ReplaceAll(message, password, "[redacted]")
	}
	var failure *sqlFailure
	if errors.As(err, &failure) && failure.possibleChanges {
		message += "; database changes may already be committed or outcome may be unknown; do not retry automatically"
	}
	return displayText(message)
}

func connectionConfig(p profile, timeout int) (*pgconn.Config, error) {
	// Parse only a constant template. Profile strings never enter a DSN parser.
	// Empty passfile prevents reading the driver's default ~/.pgpass, including
	// when the explicitly configured password is empty. PG* was cleared at startup.
	cfg, err := pgconn.ParseConfig("host=127.0.0.1 port=1 user=pgleash database=pgleash password='' passfile='' sslmode=disable sslcert='' sslkey='' sslrootcert='' connect_timeout=5 target_session_attrs=any")
	if err != nil {
		return nil, fmt.Errorf("cannot initialize connection settings")
	}
	cfg.Host = "127.0.0.1"
	cfg.Port = uint16(*p.Port)
	cfg.User = *p.User
	cfg.Password = *p.Password
	cfg.Database = *p.Database
	cfg.TLSConfig = nil
	cfg.Fallbacks = nil
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams = map[string]string{"client_encoding": "UTF8", "application_name": "pgleash", "standard_conforming_strings": "on", "statement_timeout": strconv.Itoa(timeout * 1000)}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(*p.Port))
	cfg.DialFunc = func(ctx context.Context, network, requested string) (net.Conn, error) {
		if network != "tcp" || requested != address {
			return nil, fmt.Errorf("connection target is not approved")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", address)
	}
	cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		if host != "127.0.0.1" {
			return nil, fmt.Errorf("hostname resolution is disabled")
		}
		return []string{"127.0.0.1"}, nil
	}
	return cfg, nil
}

func databaseError(err error, fallback string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("database operation timed out")
	}
	return fmt.Errorf("%s", fallback)
}

func executeSQL(p profile, opts options) (batch sqlBatch, err error) {
	cfg, err := connectionConfig(p, opts.timeout)
	if err != nil {
		return batch, err
	}
	setup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.ConnectConfig(setup, cfg)
	if err != nil {
		return batch, databaseError(err, "cannot connect to approved database")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn.Close(ctx)
	}()
	identity, err := conn.Exec(setup, "SELECT current_database(), current_user").ReadAll()
	if err != nil {
		return batch, databaseError(err, "cannot verify database identity")
	}
	if len(identity) != 1 || len(identity[0].Rows) != 1 || len(identity[0].Rows[0]) != 2 || string(identity[0].Rows[0][0]) != *p.Database || string(identity[0].Rows[0][1]) != *p.User {
		return batch, fmt.Errorf("connected database or user does not match profile; SQL was not executed")
	}
	if err := conn.SyncConn(setup); err != nil {
		return batch, databaseError(err, "cannot synchronize database connection")
	}
	hijacked, err := conn.Hijack()
	if err != nil {
		return batch, fmt.Errorf("cannot initialize SQL execution")
	}
	defer hijacked.Conn.Close()
	if err = hijacked.Conn.SetDeadline(time.Now().Add(time.Duration(opts.timeout) * time.Second)); err != nil {
		return batch, fmt.Errorf("cannot set SQL deadline")
	}
	started := false
	defer func() {
		if err != nil {
			err = &sqlFailure{err, started}
		}
	}()
	frontend := hijacked.Frontend
	frontend.SendQuery(&pgproto3.Query{String: opts.sql})
	started = true // A failed write can still have reached the server.
	if err = frontend.Flush(); err != nil {
		return batch, databaseError(err, "cannot send SQL")
	}
	var current statementResult
	displayed := 0
	retained := int64(0)
	reserve := func(size int64) error {
		if size > resultCapacity-retained {
			return errResultCapacity
		}
		retained += size
		return nil
	}
	for {
		message, receiveErr := frontend.Receive()
		if receiveErr != nil {
			return batch, databaseError(receiveErr, "cannot read SQL response")
		}
		switch msg := message.(type) {
		case *pgproto3.RowDescription:
			size := int64(len(msg.Fields)) * 48
			for _, field := range msg.Fields {
				size += int64(len(field.Name))
			}
			if err := reserve(size); err != nil {
				return batch, err
			}
			current.fields = append([]pgproto3.FieldDescription(nil), msg.Fields...)
			for i := range current.fields {
				current.fields[i].Name = append([]byte(nil), current.fields[i].Name...)
			}
		case *pgproto3.DataRow:
			if len(msg.Values) != len(current.fields) {
				return batch, fmt.Errorf("invalid SQL row description")
			}
			if displayed >= opts.limit {
				batch.omitted = true
				continue
			}
			displayed++
			size := int64(24 + len(msg.Values)*24)
			for _, value := range msg.Values {
				size += int64(len(value))
			}
			if err := reserve(size); err != nil {
				return batch, err
			}
			row := make([]cell, len(msg.Values))
			for i, value := range msg.Values {
				row[i] = cell{text: string(value), null: value == nil}
			}
			current.rows = append(current.rows, row)
		case *pgproto3.CommandComplete:
			if err := reserve(int64(64 + len(msg.CommandTag))); err != nil {
				return batch, err
			}
			current.tag = string(msg.CommandTag)
			batch.results = append(batch.results, current)
			current = statementResult{}
		case *pgproto3.ReadyForQuery:
			if msg.TxStatus != 'I' {
				return batch, fmt.Errorf("transaction left open; connection closed without committing")
			}
			return batch, nil
		case *pgproto3.ErrorResponse:
			return batch, pgconn.ErrorResponseToPgError(msg)
		case *pgproto3.CopyInResponse, *pgproto3.CopyOutResponse, *pgproto3.CopyBothResponse:
			return batch, fmt.Errorf("COPY client streaming is not supported; connection closed")
		case *pgproto3.NoticeResponse, *pgproto3.NotificationResponse, *pgproto3.ParameterStatus, *pgproto3.EmptyQueryResponse:
			// These carry no ordinary query result. Do not echo server notices.
		default:
			return batch, fmt.Errorf("unexpected database protocol response")
		}
	}
}
