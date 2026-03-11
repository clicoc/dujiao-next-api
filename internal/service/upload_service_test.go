package service

import (
	"bytes"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/config"
)

func TestUploadServiceSaveFileAllowsArchiveForTelegramScene(t *testing.T) {
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

	cfg := &config.Config{}
	cfg.Upload.MaxSize = 10 * 1024 * 1024
	cfg.Upload.AllowedTypes = []string{"image/jpeg", "image/png"}
	cfg.Upload.AllowedExtensions = []string{".jpg", ".png"}
	service := NewUploadService(cfg)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "demo.zip")
	if err != nil {
		t.Fatalf("create form file failed: %v", err)
	}
	if _, err := part.Write([]byte("fake zip content")); err != nil {
		t.Fatalf("write form content failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer failed: %v", err)
	}

	reader := multipart.NewReader(&body, writer.Boundary())
	form, err := reader.ReadForm(1024 * 1024)
	if err != nil {
		t.Fatalf("read form failed: %v", err)
	}
	files := form.File["file"]
	if len(files) != 1 {
		t.Fatalf("expected one file, got %d", len(files))
	}

	savedPath, err := service.SaveFile(files[0], "telegram")
	if err != nil {
		t.Fatalf("save file failed: %v", err)
	}
	if filepath.Ext(savedPath) != ".zip" {
		t.Fatalf("expected .zip saved path, got %s", savedPath)
	}
	if _, err := os.Stat(filepath.Join(tempDir, strings.TrimPrefix(savedPath, "/"))); err != nil {
		t.Fatalf("saved file not found: %v", err)
	}
}
