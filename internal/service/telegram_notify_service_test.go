package service

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/config"
)

type rewriteTelegramTransport struct {
	baseURL string
}

func (t rewriteTelegramTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := strings.TrimRight(t.baseURL, "/") + req.URL.Path
	rewritten, err := http.NewRequestWithContext(req.Context(), req.Method, target, req.Body)
	if err != nil {
		return nil, err
	}
	rewritten.Header = req.Header.Clone()
	return http.DefaultTransport.RoundTrip(rewritten)
}

func TestTelegramNotifyServiceSendWithBotTokenUploadsLocalAttachment(t *testing.T) {
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir temp dir failed: %v", err)
	}
	defer func() {
		_ = os.Chdir(originalWD)
	}()

	attachmentPath := filepath.Join("uploads", "telegram", "2026", "03", "demo.txt")
	if err := os.MkdirAll(filepath.Dir(attachmentPath), 0o755); err != nil {
		t.Fatalf("mkdir attachment dir failed: %v", err)
	}
	if err := os.WriteFile(attachmentPath, []byte("hello telegram"), 0o644); err != nil {
		t.Fatalf("write attachment failed: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botbot-token/sendDocument" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		contentType := r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			t.Fatalf("parse media type failed: %v", err)
		}
		if mediaType != "multipart/form-data" {
			t.Fatalf("expected multipart/form-data, got %s", mediaType)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		documentFound := false
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read multipart part failed: %v", err)
			}
			body, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("read part body failed: %v", err)
			}
			if part.FormName() == "document" {
				documentFound = true
				if string(body) != "hello telegram" {
					t.Fatalf("unexpected attachment body: %s", string(body))
				}
				continue
			}
			fields[part.FormName()] = string(body)
		}
		if !documentFound {
			t.Fatalf("expected document part")
		}
		if fields["chat_id"] != "10001" {
			t.Fatalf("unexpected chat_id: %s", fields["chat_id"])
		}
		if fields["caption"] != "<b>Hello</b>" {
			t.Fatalf("unexpected caption: %s", fields["caption"])
		}
		if fields["parse_mode"] != "HTML" {
			t.Fatalf("unexpected parse_mode: %s", fields["parse_mode"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	svc := NewTelegramNotifyService(nil, config.TelegramAuthConfig{})
	svc.httpClient = &http.Client{
		Transport: rewriteTelegramTransport{baseURL: server.URL},
	}

	err = svc.SendWithBotToken(context.Background(), "bot-token", TelegramSendOptions{
		ChatID:        "10001",
		Message:       "<b>Hello</b>",
		ParseMode:     "HTML",
		AttachmentURL: "/uploads/telegram/2026/03/demo.txt",
	})
	if err != nil {
		t.Fatalf("send with local attachment failed: %v", err)
	}
}
