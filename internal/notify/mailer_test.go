package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestParseCleanEmailAddrValidation(t *testing.T) {
	validCases := []struct {
		raw      string
		wantName string
		wantAddr string
	}{
		{"reader@example.com", "", "reader@example.com"},
		{"  Volo Studio <noreply@blog.example.org> ", "Volo Studio", "noreply@blog.example.org"},
		{`"Alice, Editor" <alice.tag+1@sub.domain.co.uk>`, "Alice, Editor", "alice.tag+1@sub.domain.co.uk"},
	}
	for _, tc := range validCases {
		name, addr, err := parseCleanEmailAddr(tc.raw)
		if err != nil {
			t.Fatalf("parseCleanEmailAddr(%q) unexpected error: %v", tc.raw, err)
		}
		if name != tc.wantName || addr != tc.wantAddr {
			t.Fatalf("parseCleanEmailAddr(%q) = (%q, %q), want (%q, %q)", tc.raw, name, addr, tc.wantName, tc.wantAddr)
		}
	}

	invalidCases := []string{
		"",
		"   ",
		"missing-at.example.com",
		"nodot@localhost",
		"two@@example.com",
		".leading@example.com",
		"trailing.@example.com",
		"double..dot@example.com",
		"user@-badlabel.com",
		"user@badlabel-.com",
		"user@example..com",
		"crlf@example.com\r\nBcc: victim@example.com",
		"nul\x00@example.com",
		"angle<inject>@example.com",
		"Display <a@example.com> <b@example.com>",
		"Unclosed <a@example.com",
		"Stray >a@example.com",
		"a@example.com, b@example.com",
	}
	for _, raw := range invalidCases {
		if _, _, err := parseCleanEmailAddr(raw); err == nil {
			t.Fatalf("expected parseCleanEmailAddr(%q) to fail", raw)
		}
	}
}

func TestWriteDotStuffedSMTPData(t *testing.T) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	input := []byte("Line 1\r\n.LeadingDot\r\n.\r\n..TwoDots\nNormal line")
	if err := writeDotStuffedSMTPData(bw, input); err != nil {
		t.Fatalf("writeDotStuffedSMTPData failed: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "Line 1\r\n..LeadingDot\r\n..\r\n...TwoDots\r\nNormal line\r\n.\r\n"
	if buf.String() != want {
		t.Fatalf("dot-stuffed output = %q, want %q", buf.String(), want)
	}
}

type recordedSMTPMessage struct {
	ehloSeen  bool
	authCmd   string
	mailFrom  string
	rcptTo    string
	rawData   string
	quitSeen  bool
	authMode  string
	expectUsr string
	expectPwd string
}

func runMockSMTPPipeServer(t *testing.T, serverConn net.Conn, rec *recordedSMTPMessage) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer serverConn.Close()
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(serverConn)
		writeLine := func(s string) error {
			_, err := serverConn.Write([]byte(s + "\r\n"))
			return err
		}
		if err := writeLine("220 mock.internal ESMTP ready"); err != nil {
			done <- err
			return
		}
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO "):
				rec.ehloSeen = true
				authLine := "250 AUTH PLAIN LOGIN"
				if rec.authMode == "LOGIN_ONLY" {
					authLine = "250 AUTH LOGIN"
				}
				if err := writeLine("250-mock.internal Hello"); err != nil {
					done <- err
					return
				}
				if err := writeLine(authLine); err != nil {
					done <- err
					return
				}
			case strings.HasPrefix(upper, "AUTH PLAIN "):
				rec.authCmd = line
				tok := strings.TrimSpace(line[len("AUTH PLAIN "):])
				wantTok := base64.StdEncoding.EncodeToString([]byte("\x00" + rec.expectUsr + "\x00" + rec.expectPwd))
				if tok != wantTok {
					_ = writeLine("535 5.7.8 Authentication credentials invalid")
					done <- nil
					return
				}
				if err := writeLine("235 2.7.0 Authentication successful"); err != nil {
					done <- err
					return
				}
			case upper == "AUTH LOGIN":
				rec.authCmd = line
				if err := writeLine("334 VXNlcm5hbWU6"); err != nil {
					done <- err
					return
				}
				uLine, err := r.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				if strings.TrimRight(uLine, "\r\n") != base64.StdEncoding.EncodeToString([]byte(rec.expectUsr)) {
					_ = writeLine("535 bad user")
					done <- nil
					return
				}
				if err := writeLine("334 UGFzc3dvcmQ6"); err != nil {
					done <- err
					return
				}
				pLine, err := r.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				if strings.TrimRight(pLine, "\r\n") != base64.StdEncoding.EncodeToString([]byte(rec.expectPwd)) {
					_ = writeLine("535 bad pass")
					done <- nil
					return
				}
				if err := writeLine("235 2.7.0 Authentication successful"); err != nil {
					done <- err
					return
				}
			case strings.HasPrefix(upper, "MAIL FROM:"):
				rec.mailFrom = strings.TrimSpace(line[len("MAIL FROM:"):])
				if err := writeLine("250 2.1.0 Ok"); err != nil {
					done <- err
					return
				}
			case strings.HasPrefix(upper, "RCPT TO:"):
				rec.rcptTo = strings.TrimSpace(line[len("RCPT TO:"):])
				if err := writeLine("250 2.1.5 Ok"); err != nil {
					done <- err
					return
				}
			case upper == "DATA":
				if err := writeLine("354 End data with <CR><LF>.<CR><LF>"); err != nil {
					done <- err
					return
				}
				var dataLines []string
				for {
					dLine, err := r.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					dLine = strings.TrimRight(dLine, "\r\n")
					if dLine == "." {
						break
					}
					// RFC 5321 dot-unstuffing on receiver side
					if strings.HasPrefix(dLine, ".") {
						dLine = dLine[1:]
					}
					dataLines = append(dataLines, dLine)
				}
				rec.rawData = strings.Join(dataLines, "\r\n")
				if err := writeLine("250 2.0.0 Queued"); err != nil {
					done <- err
					return
				}
			case upper == "QUIT":
				rec.quitSeen = true
				_ = writeLine("221 2.0.0 Bye")
				done <- nil
				return
			default:
				_ = writeLine("500 unrecognized command")
			}
		}
	}()
	return done
}

func TestMailerPureGoSMTPAuthPlainLoginDotStuffingAndCRLFRejection(t *testing.T) {
	ctx := context.Background()

	for _, mode := range []string{"PLAIN", "LOGIN_ONLY"} {
		t.Run(mode, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			rec := &recordedSMTPMessage{
				authMode:  mode,
				expectUsr: "mailer@internal.example",
				expectPwd: "s3cr3t-pass!",
			}
			srvDone := runMockSMTPPipeServer(t, serverConn, rec)

			m := NewMailer()
			m.resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("10.10.0.25")}, nil
			}
			m.dialFn = func(_ context.Context, _, _ string) (net.Conn, error) {
				return clientConn, nil
			}

			cfg := domain.NotificationsSettings{
				SMTPEnabled:   true,
				SMTPHost:      "smtp.internal.example",
				SMTPPort:      25,
				SMTPSecurity:  "plain_internal",
				SMTPAuth:      true,
				SMTPUsername:  "mailer@internal.example",
				SMTPFromName:  "Volo Mailer",
				SMTPFromEmail: "noreply@internal.example",
			}
			// Include lines beginning with '.' in raw HTMLBody so quoted-printable leaves '.' at column 0
			err := m.Send(ctx, cfg, "s3cr3t-pass!", MailMessage{
				To:       "Reader <reader@example.org>",
				Subject:  "Welcome\r\nBcc: evil@attacker.test",
				TextBody: "Hello world",
				HTMLBody: "<div>\r\n.secret-dot-line\r\n.\r\n</div>",
			})
			if err != nil {
				t.Fatalf("Mailer.Send(%s) failed: %v", mode, err)
			}
			if srvErr := <-srvDone; srvErr != nil {
				t.Fatalf("mock SMTP server error: %v", srvErr)
			}
			if !rec.ehloSeen || !rec.quitSeen {
				t.Fatalf("expected EHLO and QUIT, got ehlo=%v quit=%v", rec.ehloSeen, rec.quitSeen)
			}
			if rec.mailFrom != "<noreply@internal.example>" || rec.rcptTo != "<reader@example.org>" {
				t.Fatalf("envelope mismatch: from=%q to=%q", rec.mailFrom, rec.rcptTo)
			}
			if strings.Contains(rec.rawData, "\r\nBcc:") {
				t.Fatalf("Subject CRLF header injection leaked into MIME headers:\n%s", rec.rawData)
			}
			if !strings.Contains(rec.rawData, "\r\n.secret-dot-line\r\n.\r\n") {
				t.Fatalf("expected RFC 5321 dot-stuffed lines to round-trip cleanly, got:\n%s", rec.rawData)
			}
		})
	}

	// Strict CRLF/angle-bracket rejection on recipient and sender addresses
	m := NewMailer()
	baseCfg := domain.NotificationsSettings{
		SMTPEnabled:   true,
		SMTPHost:      "smtp.internal.example",
		SMTPPort:      25,
		SMTPSecurity:  "plain_internal",
		SMTPFromEmail: "noreply@internal.example",
	}
	if err := m.Send(ctx, baseCfg, "", MailMessage{
		To:       "victim@example.com\r\nRCPT TO:<evil@attacker.test>",
		Subject:  "Hi",
		TextBody: "Hi",
	}); err == nil {
		t.Fatalf("expected CRLF in recipient address to be rejected")
	}
	badFromCfg := baseCfg
	badFromCfg.SMTPFromEmail = "noreply@internal.example>\r\nDATA"
	if err := m.Send(ctx, badFromCfg, "", MailMessage{
		To:       "victim@example.com",
		Subject:  "Hi",
		TextBody: "Hi",
	}); err == nil {
		t.Fatalf("expected CRLF/angle-bracket injection in SMTPFromEmail to be rejected")
	}
}

func TestDispatcherNotifyCommentReplyApproved(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	secret := "0123456789abcdef0123456789abcdef-notify-secret"
	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey: &secret,
		DataDir:   &dataDir,
		UploadDir: &uploadDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "notify-test.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	settings := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
	if _, err := settings.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	notifPatch, _ := json.Marshal(map[string]any{
		"smtp_enabled":          true,
		"smtp_host":             "smtp.internal.example",
		"smtp_port":             25,
		"smtp_security":         "plain_internal",
		"smtp_auth":             false,
		"smtp_from_email":       "noreply@internal.example",
		"notify_reply_by_email": true,
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainNotifications, notifPatch, 0); err != nil {
		t.Fatal(err)
	}

	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatal(err)
	}
	disp := NewDispatcher(DispatcherOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settings,
		Catalog:   cat,
		PublicURL: "https://blog.example.com",
	})

	var (
		mu        sync.Mutex
		gotRec    *recordedSMTPMessage
		delivered = make(chan struct{}, 1)
	)
	disp.Mailer().resolver = func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.20.0.5")}, nil
	}
	disp.Mailer().dialFn = func(_ context.Context, _, _ string) (net.Conn, error) {
		cConn, sConn := net.Pipe()
		rec := &recordedSMTPMessage{}
		mu.Lock()
		gotRec = rec
		mu.Unlock()
		done := runMockSMTPPipeServer(t, sConn, rec)
		go func() {
			<-done
			select {
			case delivered <- struct{}{}:
			default:
			}
		}()
		return cConn, nil
	}

	// Insert an article and a parent comment with encrypted notify email
	now := time.Now().UnixMilli()
	_, err = store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('art-1', 'u-1', '', 'Hello Volo', 'hello-volo', '', '', 'Body', '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 1, 1, 0, 0, ?, ?)`,
		now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	encEmail, err := cfg.KeyRing.SealString("parent-reader@example.org", config.CommentNotifyEmailAAD("cmt-parent"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES ('cmt-parent', ?, 'art-1', '', '', '', 'g', 'f', 'Parent Reader', '', '', ?, 'en-US', '', '', '', 'Great post!', ?, 0, ?, ?)`,
		domain.TargetTypeArticle, encEmail, domain.CommentStatusApproved, now, now)
	if err != nil {
		t.Fatal(err)
	}

	disp.NotifyCommentReplyApproved(ctx, domain.Comment{
		ID:         "cmt-reply-1",
		TargetType: domain.TargetTypeArticle,
		TargetID:   "art-1",
		ParentID:   "cmt-parent",
		AuthorName: "Moderator Approved Guest",
		ContentMD:  "Thanks for reading!",
		Status:     domain.CommentStatusApproved,
	})

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for NotifyCommentReplyApproved SMTP delivery")
	}
	_ = disp.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if gotRec == nil || gotRec.rcptTo != "<parent-reader@example.org>" {
		t.Fatalf("expected reply notification to <parent-reader@example.org>, got %+v", gotRec)
	}
	if !strings.Contains(gotRec.rawData, "#comment-cmt-reply-1") {
		t.Fatalf("expected comment anchor URL in email body, got:\n%s", gotRec.rawData)
	}
}
