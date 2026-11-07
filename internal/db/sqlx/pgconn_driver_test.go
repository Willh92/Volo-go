package sqlx_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/jackc/pgx/v5/pgproto3"
)

// startMockPGServer starts an in-process PostgreSQL v3 wire-protocol server supporting:
//   - Ping / health SELECT 1
//   - "SELECT stream" (sends Row 1, waits on gateCh before sending Row 2, proving lazy streaming!)
//   - "SELECT multi" (sends 4 rows so caller can Close() after row 1 and verify pool connection reuse)
//   - "SELECT fail_midstream" (sends Row 1 then ErrorResponse, verifying error propagation)
func startMockPGServer(t *testing.T, row1ReadGate <-chan struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock pg: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMockPGConn(conn, row1ReadGate)
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	return fmt.Sprintf("postgres://volo:secret@%s:%s/volodb?sslmode=disable", host, port)
}

func handleMockPGConn(conn net.Conn, row1ReadGate <-chan struct{}) {
	defer conn.Close()
	backend := pgproto3.NewBackend(conn, conn)

	startup, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, isSSL := startup.(*pgproto3.SSLRequest); isSSL {
		_, _ = conn.Write([]byte{'N'})
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
	}

	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 2}})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}

	var currentQuery string
	for {
		msg, err := backend.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Query:
			if !serveMockQuery(backend, m.String, false, row1ReadGate) {
				return
			}
		case *pgproto3.Parse:
			currentQuery = m.Query
			backend.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			backend.Send(&pgproto3.BindComplete{})
		case *pgproto3.Describe:
			// Sent as part of extended query; RowDescription is emitted in Execute/Sync handling below.
		case *pgproto3.Execute:
			// Wait for Sync to flush final ReadyForQuery cleanly.
		case *pgproto3.Sync:
			if !serveMockQuery(backend, currentQuery, true, row1ReadGate) {
				return
			}
			currentQuery = ""
		}
	}
}

func serveMockQuery(backend *pgproto3.Backend, q string, isExtended bool, row1ReadGate <-chan struct{}) bool {
	upper := strings.ToUpper(strings.TrimSpace(q))
	sendRowDesc := func() {
		backend.Send(&pgproto3.RowDescription{
			Fields: []pgproto3.FieldDescription{
				{Name: []byte("id"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1, Format: 0},
				{Name: []byte("num"), DataTypeOID: 20, DataTypeSize: 8, TypeModifier: -1, Format: 0},
			},
		})
	}
	_ = isExtended

	switch {
	case strings.Contains(upper, "FAIL_MIDSTREAM"):
		sendRowDesc()
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("partial"), []byte("1")}})
		backend.Send(&pgproto3.ErrorResponse{
			Severity: "ERROR",
			Code:     "XX000",
			Message:  "simulated midstream failure",
		})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return backend.Flush() == nil

	case strings.Contains(upper, "STREAM"):
		sendRowDesc()
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("r1"), []byte("10")}})
		if err := backend.Flush(); err != nil {
			return false
		}
		if row1ReadGate != nil {
			select {
			case <-row1ReadGate:
			case <-time.After(3 * time.Second):
				return false
			}
		}
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("r2"), []byte("20")}})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return backend.Flush() == nil

	case strings.Contains(upper, "MULTI"):
		sendRowDesc()
		for i := 1; i <= 4; i++ {
			backend.Send(&pgproto3.DataRow{
				Values: [][]byte{
					[]byte(fmt.Sprintf("item-%d", i)),
					[]byte(fmt.Sprintf("%d", i*100)),
				},
			})
		}
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 4")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return backend.Flush() == nil

	default:
		// Ping / simple SELECT 1
		backend.Send(&pgproto3.RowDescription{
			Fields: []pgproto3.FieldDescription{
				{Name: []byte("ok"), DataTypeOID: 20, DataTypeSize: 8, TypeModifier: -1, Format: 0},
			},
		})
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return backend.Flush() == nil
	}
}

func TestPGRows_LazyStreamingEarlyClosePoolReuseAndErrors(t *testing.T) {
	for _, simpleProto := range []bool{false, true} {
		name := "extended_protocol"
		if simpleProto {
			name = "simple_protocol"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			gate := make(chan struct{})
			dsn := startMockPGServer(t, gate)

			store, err := sqlx.Open(ctx, sqlx.Options{
				Driver:         "postgres",
				DSN:            dsn,
				SimpleProtocol: simpleProto,
				MaxOpenConns:   1,
				MaxIdleConns:   1,
			})
			if err != nil {
				t.Fatalf("sqlx.Open postgres (%s): %v", name, err)
			}
			defer store.Close()

			// 1. Lazy streaming: Row 2 is gated until we scan Row 1!
			// Under eager Read()/ReadAll(), QueryContext would block forever waiting for Row 2.
			rows, err := store.QueryContext(ctx, "SELECT stream FROM t WHERE 1 = ?", 1)
			if err != nil {
				t.Fatalf("QueryContext stream failed: %v", err)
			}
			if !rows.Next() {
				_ = rows.Close()
				t.Fatalf("expected first streamed row before gate release")
			}
			var id1 string
			var num1 int64
			if err := rows.Scan(&id1, &num1); err != nil {
				_ = rows.Close()
				t.Fatalf("scan row 1: %v", err)
			}
			if id1 != "r1" || num1 != 10 {
				_ = rows.Close()
				t.Fatalf("unexpected row 1: id=%q num=%d", id1, num1)
			}
			// Release Row 2 on the wire
			close(gate)

			if !rows.Next() {
				_ = rows.Close()
				t.Fatalf("expected second streamed row after gate release")
			}
			var id2 string
			var num2 int64
			if err := rows.Scan(&id2, &num2); err != nil {
				_ = rows.Close()
				t.Fatalf("scan row 2: %v", err)
			}
			if id1 != "r1" || id2 != "r2" || num2 != 20 {
				_ = rows.Close()
				t.Fatalf("unexpected row 2 (or buffer aliasing): id1=%q id2=%q num2=%d", id1, id2, num2)
			}
			if rows.Next() {
				_ = rows.Close()
				t.Fatalf("expected EOF after 2 rows")
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("rows.Close: %v", err)
			}

			// 2. Early Close() mid-stream must drain remaining rows and keep single pooled conn reusable!
			multiRows, err := store.QueryContext(ctx, "SELECT multi FROM t")
			if err != nil {
				t.Fatalf("QueryContext multi: %v", err)
			}
			if !multiRows.Next() {
				t.Fatalf("expected row 1 of multi")
			}
			var mID string
			var mNum int64
			if err := multiRows.Scan(&mID, &mNum); err != nil || mID != "item-1" || mNum != 100 {
				t.Fatalf("unexpected first multi row: id=%q num=%d err=%v", mID, mNum, err)
			}
			// Close early with 3 unread DataRows still in flight!
			if err := multiRows.Close(); err != nil {
				t.Fatalf("early multiRows.Close failed: %v", err)
			}

			// Reuse the exact same pooled connection (MaxOpenConns=1) immediately:
			var pingVal int64
			if err := store.QueryRowContext(ctx, "SELECT 1").Scan(&pingVal); err != nil || pingVal != 1 {
				t.Fatalf("pooled connection unhealthy after early Close(): val=%d err=%v", pingVal, err)
			}

			// 3. Mid-stream PostgreSQL ErrorResponse must surface via rows.Err() and leave conn reusable!
			errRows, err := store.QueryContext(ctx, "SELECT fail_midstream FROM t")
			if err != nil {
				t.Fatalf("QueryContext fail_midstream: %v", err)
			}
			if !errRows.Next() {
				t.Fatalf("expected row 1 before midstream error")
			}
			if errRows.Next() {
				t.Fatalf("expected Next()=false on midstream ErrorResponse")
			}
			if errRows.Err() == nil || !strings.Contains(errRows.Err().Error(), "simulated midstream failure") {
				t.Fatalf("expected simulated midstream failure in rows.Err(), got %v", errRows.Err())
			}
			_ = errRows.Close()

			// Connection must STILL be healthy after draining ErrorResponse + ReadyForQuery!
			if err := store.QueryRowContext(ctx, "SELECT 1").Scan(&pingVal); err != nil || pingVal != 1 {
				t.Fatalf("pooled connection unhealthy after midstream error: val=%d err=%v", pingVal, err)
			}
		})
	}
}
