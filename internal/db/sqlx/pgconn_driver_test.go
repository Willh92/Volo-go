package sqlx_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

const (
	testSSLRequestCode    uint32 = 80877103
	testCancelRequestCode uint32 = 80877102
)

type mockPGAuthMode int

const (
	mockAuthTrust mockPGAuthMode = iota
	mockAuthCleartext
	mockAuthMD5
	mockAuthSCRAMSHA256
	mockAuthSCRAMBadServerSig
)

type mockPGServerOpts struct {
	authMode                  mockPGAuthMode
	expectedUser              string
	expectedPassword          string
	standardConformingStrings string
	tlsConfig                 *tls.Config
	row1ReadGate              <-chan struct{}
	hangQueryGate             <-chan struct{}
	cancelReceived            chan [8]byte // processID (4B) + secretKey (4B)
	processID                 uint32
	secretKey                 [4]byte
}

type mockPGWire struct {
	conn net.Conn
	rd   *bufio.Reader
	wbuf []byte
}

func newMockPGWire(conn net.Conn) *mockPGWire {
	return &mockPGWire{
		conn: conn,
		rd:   bufio.NewReader(conn),
		wbuf: make([]byte, 0, 512),
	}
}

func (w *mockPGWire) readStartupPacket() (uint32, map[string]string, [8]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(w.rd, lenBuf[:]); err != nil {
		return 0, nil, [8]byte{}, err
	}
	pktLen := binary.BigEndian.Uint32(lenBuf[:])
	if pktLen < 8 || pktLen > 65536 {
		return 0, nil, [8]byte{}, fmt.Errorf("invalid startup packet length %d", pktLen)
	}
	body := make([]byte, pktLen-4)
	if _, err := io.ReadFull(w.rd, body); err != nil {
		return 0, nil, [8]byte{}, err
	}
	code := binary.BigEndian.Uint32(body[0:4])
	if code == testSSLRequestCode {
		return code, nil, [8]byte{}, nil
	}
	if code == testCancelRequestCode {
		var cancelData [8]byte
		if len(body) >= 12 {
			copy(cancelData[:], body[4:12])
		}
		return code, nil, cancelData, nil
	}
	params := make(map[string]string)
	rem := body[4:]
	for len(rem) > 1 {
		if rem[0] == 0 {
			break
		}
		kEnd := bytes.IndexByte(rem, 0)
		if kEnd < 0 {
			break
		}
		k := string(rem[:kEnd])
		rem = rem[kEnd+1:]
		vEnd := bytes.IndexByte(rem, 0)
		if vEnd < 0 {
			break
		}
		v := string(rem[:vEnd])
		rem = rem[vEnd+1:]
		params[k] = v
	}
	return code, params, [8]byte{}, nil
}

func (w *mockPGWire) readFrontendMsg() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(w.rd, hdr[:]); err != nil {
		return 0, nil, err
	}
	msgType := hdr[0]
	msgLen := binary.BigEndian.Uint32(hdr[1:5])
	if msgLen < 4 || msgLen > 1024*1024 {
		return 0, nil, fmt.Errorf("invalid msg len %d", msgLen)
	}
	payload := make([]byte, msgLen-4)
	if len(payload) > 0 {
		if _, err := io.ReadFull(w.rd, payload); err != nil {
			return 0, nil, err
		}
	}
	return msgType, payload, nil
}

func (w *mockPGWire) queueMsg(msgType byte, body []byte) {
	w.wbuf = append(w.wbuf, msgType)
	w.wbuf = binary.BigEndian.AppendUint32(w.wbuf, uint32(4+len(body)))
	w.wbuf = append(w.wbuf, body...)
}

func (w *mockPGWire) flush() error {
	if len(w.wbuf) == 0 {
		return nil
	}
	_, err := w.conn.Write(w.wbuf)
	w.wbuf = w.wbuf[:0]
	return err
}

func (w *mockPGWire) queueAuthOK() {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 0)
	w.queueMsg('R', b[:])
}

func (w *mockPGWire) queueParamStatus(k, v string) {
	b := make([]byte, 0, len(k)+len(v)+2)
	b = append(b, k...)
	b = append(b, 0)
	b = append(b, v...)
	b = append(b, 0)
	w.queueMsg('S', b)
}

func (w *mockPGWire) queueBackendKeyData(pid uint32, secret [4]byte) {
	var b [8]byte
	binary.BigEndian.PutUint32(b[0:4], pid)
	copy(b[4:8], secret[:])
	w.queueMsg('K', b[:])
}

func (w *mockPGWire) queueReadyForQuery(txStatus byte) {
	w.queueMsg('Z', []byte{txStatus})
}

func (w *mockPGWire) queueError(sev, code, message, detail string) {
	var b []byte
	addField := func(ft byte, val string) {
		if val == "" {
			return
		}
		b = append(b, ft)
		b = append(b, val...)
		b = append(b, 0)
	}
	addField('S', sev)
	addField('V', sev)
	addField('C', code)
	addField('M', message)
	addField('D', detail)
	b = append(b, 0)
	w.queueMsg('E', b)
}

func (w *mockPGWire) queueCommandComplete(tag string) {
	b := append([]byte(tag), 0)
	w.queueMsg('C', b)
}

type mockCol struct {
	name string
	oid  uint32
}

func (w *mockPGWire) queueRowDescription(cols []mockCol) {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, uint16(len(cols)))
	for _, c := range cols {
		b = append(b, c.name...)
		b = append(b, 0)
		b = binary.BigEndian.AppendUint32(b, 0)          // table OID
		b = binary.BigEndian.AppendUint16(b, 0)          // attr num
		b = binary.BigEndian.AppendUint32(b, c.oid)      // type OID
		b = binary.BigEndian.AppendUint16(b, 0xffff)     // size -1
		b = binary.BigEndian.AppendUint32(b, 0xffffffff) // typmod -1
		b = binary.BigEndian.AppendUint16(b, 0)          // text format
	}
	w.queueMsg('T', b)
}

func (w *mockPGWire) queueDataRow(vals [][]byte) {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, uint16(len(vals)))
	for _, v := range vals {
		if v == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)
		} else {
			b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
			b = append(b, v...)
		}
	}
	w.queueMsg('D', b)
}

func startCustomMockPGServer(t *testing.T, opts mockPGServerOpts) (host string, port string) {
	t.Helper()
	if opts.standardConformingStrings == "" {
		opts.standardConformingStrings = "on"
	}
	if opts.processID == 0 {
		opts.processID = 4242
	}
	if opts.secretKey == ([4]byte{}) {
		opts.secretKey = [4]byte{0xde, 0xad, 0xbe, 0xef}
	}

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
			go serveCustomMockPGConn(conn, opts)
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p
}

func startMockPGServer(t *testing.T, row1ReadGate <-chan struct{}) string {
	t.Helper()
	h, p := startCustomMockPGServer(t, mockPGServerOpts{
		authMode:     mockAuthTrust,
		row1ReadGate: row1ReadGate,
	})
	return fmt.Sprintf("postgres://volo:secret@%s:%s/volodb?sslmode=disable", h, p)
}

func serveCustomMockPGConn(rawConn net.Conn, opts mockPGServerOpts) {
	defer rawConn.Close()
	w := newMockPGWire(rawConn)

	code, params, cancelData, err := w.readStartupPacket()
	if err != nil {
		return
	}
	if code == testCancelRequestCode {
		if opts.cancelReceived != nil {
			select {
			case opts.cancelReceived <- cancelData:
			default:
			}
		}
		return
	}
	if code == testSSLRequestCode {
		if opts.tlsConfig != nil {
			if _, err := rawConn.Write([]byte{'S'}); err != nil {
				return
			}
			tlsServer := tls.Server(rawConn, opts.tlsConfig)
			if err := tlsServer.Handshake(); err != nil {
				return
			}
			w = newMockPGWire(tlsServer)
		} else {
			if _, err := rawConn.Write([]byte{'N'}); err != nil {
				return
			}
		}
		code, params, _, err = w.readStartupPacket()
		if err != nil || code == testSSLRequestCode {
			return
		}
	}

	user := params["user"]
	if opts.expectedUser != "" && user != opts.expectedUser {
		w.queueError("FATAL", "28P01", "invalid username", "")
		_ = w.flush()
		return
	}

	if !authenticateMockPG(w, user, opts) {
		return
	}

	w.queueAuthOK()
	w.queueParamStatus("server_version", "16.4")
	w.queueParamStatus("client_encoding", "UTF8")
	w.queueParamStatus("standard_conforming_strings", opts.standardConformingStrings)
	w.queueBackendKeyData(opts.processID, opts.secretKey)
	w.queueReadyForQuery('I')
	if err := w.flush(); err != nil {
		return
	}

	txStatus := byte('I')
	var currentQuery string
	var boundParams [][]byte

	for {
		msgType, payload, err := w.readFrontendMsg()
		if err != nil {
			return
		}
		switch msgType {
		case 'X': // Terminate
			return
		case 'Q': // Simple Query
			q := string(bytes.TrimRight(payload, "\x00"))
			if !handleMockQueryExecution(w, q, nil, false, &txStatus, opts) {
				return
			}
		case 'P': // Parse
			// stmtName\0 + query\0 + int16(numParams)
			nul1 := bytes.IndexByte(payload, 0)
			if nul1 >= 0 {
				rest := payload[nul1+1:]
				nul2 := bytes.IndexByte(rest, 0)
				if nul2 >= 0 {
					currentQuery = string(rest[:nul2])
				}
			}
			w.queueMsg('1', nil) // ParseComplete
		case 'B': // Bind
			boundParams = parseMockBindParams(payload)
			w.queueMsg('2', nil) // BindComplete
		case 'D': // Describe
			// Emitted during Sync handling
		case 'E': // Execute
			// Emitted during Sync handling
		case 'S': // Sync
			if !handleMockQueryExecution(w, currentQuery, boundParams, true, &txStatus, opts) {
				return
			}
			currentQuery = ""
			boundParams = nil
		}
	}
}

func authenticateMockPG(w *mockPGWire, user string, opts mockPGServerOpts) bool {
	switch opts.authMode {
	case mockAuthTrust:
		return true

	case mockAuthCleartext:
		var req [4]byte
		binary.BigEndian.PutUint32(req[:], 3)
		w.queueMsg('R', req[:])
		if err := w.flush(); err != nil {
			return false
		}
		mt, payload, err := w.readFrontendMsg()
		if err != nil || mt != 'p' {
			return false
		}
		gotPass := string(bytes.TrimRight(payload, "\x00"))
		if gotPass != opts.expectedPassword {
			w.queueError("FATAL", "28P01", "password authentication failed", "")
			_ = w.flush()
			return false
		}
		return true

	case mockAuthMD5:
		salt := [4]byte{0x12, 0x34, 0x56, 0x78}
		var req [8]byte
		binary.BigEndian.PutUint32(req[0:4], 5)
		copy(req[4:8], salt[:])
		w.queueMsg('R', req[:])
		if err := w.flush(); err != nil {
			return false
		}
		mt, payload, err := w.readFrontendMsg()
		if err != nil || mt != 'p' {
			return false
		}
		got := string(bytes.TrimRight(payload, "\x00"))
		h1 := md5.Sum([]byte(opts.expectedPassword + user))
		h2 := md5.New()
		h2.Write([]byte(hex.EncodeToString(h1[:])))
		h2.Write(salt[:])
		want := "md5" + hex.EncodeToString(h2.Sum(nil))
		if got != want {
			w.queueError("FATAL", "28P01", "md5 authentication failed", "")
			_ = w.flush()
			return false
		}
		return true

	case mockAuthSCRAMSHA256, mockAuthSCRAMBadServerSig:
		var saslInit []byte
		saslInit = binary.BigEndian.AppendUint32(saslInit, 10)
		saslInit = append(saslInit, "SCRAM-SHA-256\x00\x00"...)
		w.queueMsg('R', saslInit)
		if err := w.flush(); err != nil {
			return false
		}

		mt, payload, err := w.readFrontendMsg()
		if err != nil || mt != 'p' {
			return false
		}
		nulIdx := bytes.IndexByte(payload, 0)
		if nulIdx < 0 || string(payload[:nulIdx]) != "SCRAM-SHA-256" || nulIdx+5 > len(payload) {
			return false
		}
		initLen := int(binary.BigEndian.Uint32(payload[nulIdx+1 : nulIdx+5]))
		clientFirst := string(payload[nulIdx+5 : nulIdx+5+initLen])
		if !strings.HasPrefix(clientFirst, "n,,") {
			return false
		}
		clientFirstBare := clientFirst[3:]
		var clientNonce string
		for _, p := range strings.Split(clientFirstBare, ",") {
			if strings.HasPrefix(p, "r=") {
				clientNonce = p[2:]
			}
		}

		salt := []byte("volo-scram-salt!")
		iterations := 4096
		combinedNonce := clientNonce + "ServerNonceXYZ"
		serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", combinedNonce, base64.StdEncoding.EncodeToString(salt), iterations)

		var saslCont []byte
		saslCont = binary.BigEndian.AppendUint32(saslCont, 11)
		saslCont = append(saslCont, serverFirst...)
		w.queueMsg('R', saslCont)
		if err := w.flush(); err != nil {
			return false
		}

		mt, payload, err = w.readFrontendMsg()
		if err != nil || mt != 'p' {
			return false
		}
		clientFinal := string(payload)
		pIdx := strings.LastIndex(clientFinal, ",p=")
		if pIdx < 0 {
			return false
		}
		clientFinalWithoutProof := clientFinal[:pIdx]
		gotProof, err := base64.StdEncoding.DecodeString(clientFinal[pIdx+3:])
		if err != nil || len(gotProof) != 32 {
			return false
		}

		saltedPass, err := pbkdf2.Key(sha256.New, opts.expectedPassword, salt, iterations, 32)
		if err != nil {
			return false
		}
		macClient := hmac.New(sha256.New, saltedPass)
		macClient.Write([]byte("Client Key"))
		expectedClientKey := macClient.Sum(nil)
		expectedStoredKey := sha256.Sum256(expectedClientKey)

		authMsg := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
		macSig := hmac.New(sha256.New, expectedStoredKey[:])
		macSig.Write([]byte(authMsg))
		clientSig := macSig.Sum(nil)

		recoveredClientKey := make([]byte, 32)
		for i := 0; i < 32; i++ {
			recoveredClientKey[i] = gotProof[i] ^ clientSig[i]
		}
		recoveredStored := sha256.Sum256(recoveredClientKey)
		if !hmac.Equal(recoveredStored[:], expectedStoredKey[:]) {
			w.queueError("FATAL", "28P01", "SCRAM password authentication failed", "")
			_ = w.flush()
			return false
		}

		macSrvKey := hmac.New(sha256.New, saltedPass)
		macSrvKey.Write([]byte("Server Key"))
		srvKey := macSrvKey.Sum(nil)
		macSrvSig := hmac.New(sha256.New, srvKey)
		macSrvSig.Write([]byte(authMsg))
		srvSig := macSrvSig.Sum(nil)
		if opts.authMode == mockAuthSCRAMBadServerSig {
			srvSig[0] ^= 0xff
		}

		var saslFinal []byte
		saslFinal = binary.BigEndian.AppendUint32(saslFinal, 12)
		saslFinal = append(saslFinal, ("v=" + base64.StdEncoding.EncodeToString(srvSig))...)
		w.queueMsg('R', saslFinal)
		return true
	}
	return false
}

func parseMockBindParams(payload []byte) [][]byte {
	// portal\0 + stmt\0 + uint16(numFmt) + numFmt*2 + uint16(numVals) + [int32(len) + bytes]...
	pos := 0
	for k := 0; k < 2; k++ {
		nul := bytes.IndexByte(payload[pos:], 0)
		if nul < 0 {
			return nil
		}
		pos += nul + 1
	}
	if pos+2 > len(payload) {
		return nil
	}
	numFmt := int(binary.BigEndian.Uint16(payload[pos : pos+2]))
	pos += 2 + numFmt*2
	if pos+2 > len(payload) {
		return nil
	}
	numVals := int(binary.BigEndian.Uint16(payload[pos : pos+2]))
	pos += 2
	out := make([][]byte, numVals)
	for i := 0; i < numVals; i++ {
		if pos+4 > len(payload) {
			return out
		}
		vLen := int32(binary.BigEndian.Uint32(payload[pos : pos+4]))
		pos += 4
		if vLen == -1 {
			out[i] = nil
			continue
		}
		cp := make([]byte, vLen)
		copy(cp, payload[pos:pos+int(vLen)])
		pos += int(vLen)
		out[i] = cp
	}
	return out
}

func handleMockQueryExecution(
	w *mockPGWire,
	q string,
	params [][]byte,
	isExtended bool,
	txStatus *byte,
	opts mockPGServerOpts,
) bool {
	trimmed := strings.TrimSpace(q)
	upper := strings.ToUpper(trimmed)

	if trimmed == "" || trimmed == ";" {
		if isExtended {
			w.queueMsg('n', nil) // NoData
		}
		w.queueMsg('I', nil) // EmptyQueryResponse
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil
	}

	switch {
	case strings.HasPrefix(upper, "BEGIN"):
		*txStatus = 'T'
		if isExtended {
			w.queueMsg('n', nil)
		}
		w.queueCommandComplete("BEGIN")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case upper == "COMMIT":
		*txStatus = 'I'
		if isExtended {
			w.queueMsg('n', nil)
		}
		w.queueCommandComplete("COMMIT")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case upper == "ROLLBACK":
		*txStatus = 'I'
		if isExtended {
			w.queueMsg('n', nil)
		}
		w.queueCommandComplete("ROLLBACK")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "HANG_QUERY"):
		if opts.hangQueryGate != nil {
			select {
			case <-opts.hangQueryGate:
			case <-time.After(3 * time.Second):
			}
		}
		w.queueError("ERROR", "57014", "canceling statement due to user request", "")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "UPDATE_ROWS"):
		if isExtended {
			w.queueMsg('n', nil)
		}
		w.queueCommandComplete("UPDATE 3")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "INSERT_ROWS"):
		if isExtended {
			w.queueMsg('n', nil)
		}
		w.queueCommandComplete("INSERT 0 2")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "ECHO_PARAMS"):
		// Echoes 7 typed columns: bool(16), int8(20), float8(701), text(25), bytea(17), timestamptz(1184), nullable_text(25)
		w.queueRowDescription([]mockCol{
			{name: "b_col", oid: 16},
			{name: "i_col", oid: 20},
			{name: "f_col", oid: 701},
			{name: "s_col", oid: 25},
			{name: "raw_col", oid: 17},
			{name: "ts_col", oid: 1184},
			{name: "null_col", oid: 25},
		})
		if isExtended && len(params) == 7 {
			w.queueDataRow(params)
		} else {
			w.queueDataRow([][]byte{
				[]byte("t"),
				[]byte("-9223372036854775800"),
				[]byte("3.14159"),
				[]byte("hello 'pg'"),
				[]byte(`\xdeadbeef`),
				[]byte("2026-11-09 01:02:03+00"),
				nil,
			})
		}
		w.queueCommandComplete("SELECT 1")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "FAIL_SYNTAX"):
		w.queueError("ERROR", "42601", "syntax error at or near \"FAIL_SYNTAX\"", "mock parser detail")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "FAIL_MIDSTREAM"):
		w.queueRowDescription([]mockCol{
			{name: "id", oid: 25},
			{name: "num", oid: 20},
		})
		w.queueDataRow([][]byte{[]byte("partial"), []byte("1")})
		w.queueError("ERROR", "XX000", "simulated midstream failure", "")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "STREAM"):
		w.queueRowDescription([]mockCol{
			{name: "id", oid: 25},
			{name: "num", oid: 20},
		})
		w.queueDataRow([][]byte{[]byte("r1"), []byte("10")})
		if err := w.flush(); err != nil {
			return false
		}
		if opts.row1ReadGate != nil {
			select {
			case <-opts.row1ReadGate:
			case <-time.After(3 * time.Second):
				return false
			}
		}
		w.queueDataRow([][]byte{[]byte("r2"), []byte("20")})
		w.queueCommandComplete("SELECT 2")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	case strings.Contains(upper, "MULTI"):
		w.queueRowDescription([]mockCol{
			{name: "id", oid: 25},
			{name: "num", oid: 20},
		})
		for i := 1; i <= 4; i++ {
			w.queueDataRow([][]byte{
				[]byte(fmt.Sprintf("item-%d", i)),
				[]byte(fmt.Sprintf("%d", i*100)),
			})
		}
		w.queueCommandComplete("SELECT 4")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil

	default:
		w.queueRowDescription([]mockCol{{name: "ok", oid: 20}})
		w.queueDataRow([][]byte{[]byte("1")})
		w.queueCommandComplete("SELECT 1")
		w.queueReadyForQuery(*txStatus)
		return w.flush() == nil
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
			if err := multiRows.Close(); err != nil {
				t.Fatalf("early multiRows.Close failed: %v", err)
			}

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

			if err := store.QueryRowContext(ctx, "SELECT 1").Scan(&pingVal); err != nil || pingVal != 1 {
				t.Fatalf("pooled connection unhealthy after midstream error: val=%d err=%v", pingVal, err)
			}
		})
	}
}

func TestParsePostgresConfig_URLKeywordIPv6FallbacksUnixAndRejections(t *testing.T) {
	// 1. Percent-encoded credentials, IPv6 bracketed endpoint, and query parameters
	cfg1, err := sqlx.ParsePostgresConfig("postgresql://u%3Aname:p%40ss%23word+1@[::1]:15432/volo%2ddb?sslmode=verify-full&connect_timeout=5&application_name=volo_test")
	if err != nil {
		t.Fatalf("ParsePostgresConfig IPv6 URL failed: %v", err)
	}
	if cfg1.Host != "::1" || cfg1.Port != 15432 || cfg1.User != "u:name" || cfg1.Password != "p@ss#word+1" || cfg1.Database != "volo-db" || cfg1.SSLMode != "verify-full" {
		t.Fatalf("unexpected parsed IPv6 URL config: %+v", cfg1)
	}
	if cfg1.Params["connect_timeout"] != "5" || cfg1.Params["application_name"] != "volo_test" {
		t.Fatalf("unexpected Params: %+v", cfg1.Params)
	}

	// 2. Multi-host URL with mixed default/explicit ports (matching SSRF test pattern!)
	cfgMulti, err := sqlx.ParsePostgresConfig("postgres://user:pass@8.8.8.8,169.254.169.254:5433,[2001:db8::1]:5434/blog")
	if err != nil {
		t.Fatalf("ParsePostgresConfig multi-host URL failed: %v", err)
	}
	if cfgMulti.Host != "8.8.8.8" || cfgMulti.Port != 5432 {
		t.Fatalf("unexpected primary host/port: %s:%d", cfgMulti.Host, cfgMulti.Port)
	}
	if len(cfgMulti.Fallbacks) != 2 ||
		cfgMulti.Fallbacks[0].Host != "169.254.169.254" || cfgMulti.Fallbacks[0].Port != 5433 ||
		cfgMulti.Fallbacks[1].Host != "2001:db8::1" || cfgMulti.Fallbacks[1].Port != 5434 {
		t.Fatalf("unexpected multi-host fallbacks: %+v", cfgMulti.Fallbacks)
	}

	// 3. Keyword/value DSN with single-quoted escapes, spaces, multi-host, and multi-port
	cfgKW, err := sqlx.ParsePostgresConfig(`host=db1.example.com,[::1] port=5432,5433 user=postgres password='s3cr\'et \\ pass' dbname=volo sslmode=disable connect_timeout=3`)
	if err != nil {
		t.Fatalf("ParsePostgresConfig keyword DSN failed: %v", err)
	}
	if cfgKW.Host != "db1.example.com" || cfgKW.Port != 5432 || cfgKW.User != "postgres" || cfgKW.Password != `s3cr'et \ pass` || cfgKW.Database != "volo" || cfgKW.SSLMode != "disable" {
		t.Fatalf("unexpected keyword DSN config: %+v", cfgKW)
	}
	if len(cfgKW.Fallbacks) != 1 || cfgKW.Fallbacks[0].Host != "::1" || cfgKW.Fallbacks[0].Port != 5433 {
		t.Fatalf("unexpected keyword fallbacks: %+v", cfgKW.Fallbacks)
	}

	// 4. Unix-socket detection across URL (omitted host, percent-encoded path, ?host=/...) and keyword DSN
	for _, unixDSN := range []string{
		"postgres:///volo",
		"postgres:///volo?host=/run/postgresql",
		"postgres://%2Fvar%2Frun%2Fpostgresql:5433/volo",
		"dbname=volo user=postgres",
		"host=/tmp port=5432 dbname=volo",
	} {
		uc, uErr := sqlx.ParsePostgresConfig(unixDSN)
		if uErr != nil {
			t.Fatalf("expected unix DSN %q to parse cleanly, got %v", unixDSN, uErr)
		}
		if !strings.HasPrefix(uc.Host, "/") {
			t.Fatalf("expected unix socket host starting with '/' for %q, got %q", unixDSN, uc.Host)
		}
	}

	// 5. Malformed DSN rejection suite
	badDSNs := []string{
		"",
		"   ",
		"postgres://user:pass@127.0.0.1/db\x00evil",
		"postgres://user:pass#raw@127.0.0.1/db",
		"postgres://[::1:5432/db",
		"postgres://::1:5432/db",
		"postgres://[not-ipv6]:5432/db",
		"postgres://127.0.0.1:0/db",
		"postgres://127.0.0.1:70000/db",
		"postgres://127.0.0.1:abc/db",
		"postgres://,127.0.0.1/db",
		"postgres://127.0.0.1/db?sslmode=bogus",
		"postgres://127.0.0.1/db?connect_timeout=abc",
		"postgres://127.0.0.1/db?hostaddr=not-an-ip",
		"http://127.0.0.1:5432/db",
		"just_a_bare_word",
		"host=127.0.0.1 password='unterminated",
		"host=127.0.0.1 password='abc'trailing",
		"host=127.0.0.1 password=ab'cd",
		"host=127.0.0.1 password=trailing\\",
		"host=127.0.0.1,127.0.0.2 port=5432,5433,5434",
	}
	for _, bad := range badDSNs {
		if _, bErr := sqlx.ParsePostgresConfig(bad); bErr == nil {
			t.Fatalf("expected malformed DSN %q to be rejected, but it succeeded", bad)
		}
	}
}

func TestPGAuth_CleartextMD5AndSCRAMSHA256(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name string
		mode mockPGAuthMode
	}{
		{name: "cleartext", mode: mockAuthCleartext},
		{name: "md5", mode: mockAuthMD5},
		{name: "scram_sha_256", mode: mockAuthSCRAMSHA256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := startCustomMockPGServer(t, mockPGServerOpts{
				authMode:         tc.mode,
				expectedUser:     "volo_admin",
				expectedPassword: "Str0ng!Pass#2026",
			})

			// Valid credentials succeed
			goodDSN := fmt.Sprintf("host=%s port=%s user=volo_admin password='Str0ng!Pass#2026' dbname=volo sslmode=disable", h, p)
			store, err := sqlx.Open(ctx, sqlx.Options{
				Driver:       "postgres",
				DSN:          goodDSN,
				MaxOpenConns: 1,
				MaxIdleConns: 1,
			})
			if err != nil {
				t.Fatalf("%s valid auth failed: %v", tc.name, err)
			}
			var one int64
			if err := store.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("%s SELECT 1 failed: val=%d err=%v", tc.name, one, err)
			}
			_ = store.Close()

			// Wrong password must fail cleanly
			badDSN := fmt.Sprintf("host=%s port=%s user=volo_admin password='WrongPassword' dbname=volo sslmode=disable", h, p)
			if badStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "postgres", DSN: badDSN}); err == nil {
				_ = badStore.Close()
				t.Fatalf("%s expected authentication failure with wrong password", tc.name)
			}
		})
	}

	// Forged SCRAM server signature must be rejected by client!
	hBadSig, pBadSig := startCustomMockPGServer(t, mockPGServerOpts{
		authMode:         mockAuthSCRAMBadServerSig,
		expectedUser:     "volo_admin",
		expectedPassword: "Str0ng!Pass#2026",
	})
	forgedDSN := fmt.Sprintf("host=%s port=%s user=volo_admin password='Str0ng!Pass#2026' dbname=volo sslmode=disable", hBadSig, pBadSig)
	if badStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "postgres", DSN: forgedDSN}); err == nil {
		_ = badStore.Close()
		t.Fatalf("expected forged SCRAM-SHA-256 server signature to be rejected")
	}
}

func TestPGTLSModesAndMultiHostFallbackRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Generate ephemeral self-signed ECDSA certificate for 127.0.0.1
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	caFile := filepath.Join(t.TempDir(), "root.crt")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tlsCert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}

	h, p := startCustomMockPGServer(t, mockPGServerOpts{
		authMode:         mockAuthSCRAMSHA256,
		expectedUser:     "tls_user",
		expectedPassword: "tls_secret",
		tlsConfig:        &tls.Config{Certificates: []tls.Certificate{tlsCert}},
	})

	// Allocate a guaranteed closed port for primary to verify multi-host fallback -> secondary live TLS server!
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, deadPort, _ := net.SplitHostPort(deadLn.Addr().String())
	_ = deadLn.Close()

	multiTLSDSN := fmt.Sprintf(
		"postgres://tls_user:tls_secret@127.0.0.1:%s,%s:%s/volo?sslmode=verify-full&sslrootcert=%s",
		deadPort, h, p, caFile,
	)
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver:       "postgres",
		DSN:          multiTLSDSN,
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatalf("multi-host verify-full TLS open failed: %v", err)
	}
	defer store.Close()

	var okVal int64
	if err := store.QueryRowContext(ctx, "SELECT 1").Scan(&okVal); err != nil || okVal != 1 {
		t.Fatalf("TLS query failed: val=%d err=%v", okVal, err)
	}
}

func TestPGStandardConformingStringsOffRejectedInSimpleMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, p := startCustomMockPGServer(t, mockPGServerOpts{
		authMode:                  mockAuthTrust,
		standardConformingStrings: "off",
	})
	dsn := fmt.Sprintf("postgres://volo:secret@%s:%s/volodb?sslmode=disable", h, p)
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver:         "postgres",
		DSN:            dsn,
		SimpleProtocol: true,
		MaxOpenConns:   1,
		MaxIdleConns:   1,
	})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer store.Close()

	var dummy int64
	err = store.QueryRowContext(ctx, "SELECT ?::bigint", "unsafe").Scan(&dummy)
	if err == nil || !strings.Contains(err.Error(), "standard_conforming_strings=on") {
		t.Fatalf("expected standard_conforming_strings=on rejection, got %v", err)
	}
}

func TestPGExtendedAndSimpleParamsNullsTransactionsAndCancelRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hangGate := make(chan struct{})
	defer close(hangGate)
	cancelCh := make(chan [8]byte, 2)

	h, p := startCustomMockPGServer(t, mockPGServerOpts{
		authMode:       mockAuthTrust,
		hangQueryGate:  hangGate,
		cancelReceived: cancelCh,
		processID:      0x11223344,
		secretKey:      [4]byte{0x55, 0x66, 0x77, 0x88},
	})
	dsn := fmt.Sprintf("postgres://volo:secret@%s:%s/volodb?sslmode=disable", h, p)

	for _, simpleProto := range []bool{false, true} {
		mode := "extended"
		if simpleProto {
			mode = "simple"
		}
		t.Run(mode, func(t *testing.T) {
			store, err := sqlx.Open(ctx, sqlx.Options{
				Driver:         "postgres",
				DSN:            dsn,
				SimpleProtocol: simpleProto,
				MaxOpenConns:   2,
				MaxIdleConns:   1,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			// 1. Parameterized query & scanning of bool, int64, float64, string, []byte, time.Time, and NULL
			refTime := time.Date(2026, 11, 9, 1, 2, 3, 0, time.UTC)
			row := store.QueryRowContext(ctx,
				"SELECT ECHO_PARAMS WHERE b=? AND i=? AND f=? AND s=? AND r=? AND ts=? AND n IS ?",
				true, int64(-9223372036854775800), float64(3.14159), "hello 'pg'", []byte{0xde, 0xad, 0xbe, 0xef}, refTime, nil,
			)
			var (
				gotBool  bool
				gotInt   int64
				gotFloat float64
				gotStr   string
				gotBytes []byte
				gotTime  time.Time
				gotNull  sql.NullString
			)
			if err := row.Scan(&gotBool, &gotInt, &gotFloat, &gotStr, &gotBytes, &gotTime, &gotNull); err != nil {
				t.Fatalf("Scan ECHO_PARAMS (%s) failed: %v", mode, err)
			}
			if !gotBool || gotInt != -9223372036854775800 || gotFloat != 3.14159 || gotStr != "hello 'pg'" ||
				!bytes.Equal(gotBytes, []byte{0xde, 0xad, 0xbe, 0xef}) || !gotTime.Equal(refTime) || gotNull.Valid {
				t.Fatalf("unexpected decoded values (%s): bool=%v int=%d float=%g str=%q bytes=%x time=%v null=%+v",
					mode, gotBool, gotInt, gotFloat, gotStr, gotBytes, gotTime, gotNull)
			}

			// 2. RowsAffected parsing for INSERT & UPDATE + Transactions (WithTx commit & rollback)
			resIns, err := store.ExecContext(ctx, "INSERT_ROWS INTO t VALUES (?)", "a")
			if err != nil {
				t.Fatalf("ExecContext INSERT_ROWS: %v", err)
			}
			if n, _ := resIns.RowsAffected(); n != 2 {
				t.Fatalf("expected INSERT RowsAffected=2, got %d", n)
			}

			var committed atomic.Bool
			if err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
				resUpd, e := tx.ExecContext(ctx, "UPDATE_ROWS t SET v = ?", 99)
				if e != nil {
					return e
				}
				if n, _ := resUpd.RowsAffected(); n != 3 {
					return fmt.Errorf("expected UPDATE RowsAffected=3, got %d", n)
				}
				committed.Store(true)
				return nil
			}); err != nil || !committed.Load() {
				t.Fatalf("WithTx commit failed: %v", err)
			}

			sentinel := errors.New("abort tx")
			if err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
				_, _ = tx.ExecContext(ctx, "UPDATE_ROWS t SET v = ?", 1)
				return sentinel
			}); !errors.Is(err, sentinel) {
				t.Fatalf("expected rollback sentinel error, got %v", err)
			}

			// 3. Syntax ErrorResponse surfaces SQLSTATE & Detail and keeps pooled connection healthy
			if _, err := store.ExecContext(ctx, "FAIL_SYNTAX"); err == nil || !strings.Contains(err.Error(), "42601") {
				t.Fatalf("expected SQLSTATE 42601 error, got %v", err)
			}
			var ping int64
			if err := store.QueryRowContext(ctx, "SELECT 1").Scan(&ping); err != nil || ping != 1 {
				t.Fatalf("pool unhealthy after SQL error: %v", err)
			}
		})
	}

	// 4. Context cancellation sends CancelRequest packet (80877102) with BackendKeyData (PID + SecretKey)
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver:       "postgres",
		DSN:          dsn,
		MaxOpenConns: 2,
		MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	qCtx, qCancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer qCancel()
	_, err = store.ExecContext(qCtx, "SELECT HANG_QUERY")
	if err == nil {
		t.Fatalf("expected canceled HANG_QUERY to return error")
	}

	select {
	case cancelPayload := <-cancelCh:
		gotPID := binary.BigEndian.Uint32(cancelPayload[0:4])
		if gotPID != 0x11223344 || !bytes.Equal(cancelPayload[4:8], []byte{0x55, 0x66, 0x77, 0x88}) {
			t.Fatalf("unexpected CancelRequest payload: pid=0x%x secret=%x", gotPID, cancelPayload[4:8])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive PostgreSQL CancelRequest (80877102) packet on context cancellation")
	}
}
