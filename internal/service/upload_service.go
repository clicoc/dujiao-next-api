package service

import (
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dujiao-next/internal/config"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/google/uuid"
)

var allowedUploadScenes = map[string]struct{}{
	"product":  {},
	"post":     {},
	"banner":   {},
	"editor":   {},
	"common":   {},
	"category": {},
	"telegram": {},
}

// UploadService 文件上传服务
type UploadService struct {
	cfg *config.Config
}

// NewUploadService 创建文件上传服务实例
func NewUploadService(cfg *config.Config) *UploadService {
	return &UploadService{cfg: cfg}
}

// SaveFile 保存上传的文件
func (s *UploadService) SaveFile(file *multipart.FileHeader, scene string) (string, error) {
	normalizedScene := normalizeUploadScene(scene)

	// 验证文件大小
	if file.Size > s.cfg.Upload.MaxSize {
		return "", fmt.Errorf("文件大小超过限制（最大 %d MB）", s.cfg.Upload.MaxSize/1024/1024)
	}

	// 获取文件扩展名
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if normalizedScene != "telegram" && len(s.cfg.Upload.AllowedExtensions) > 0 {
		if ext == "" || !isAllowedExtension(ext, s.cfg.Upload.AllowedExtensions) {
			return "", fmt.Errorf("文件扩展名不被允许: %s", ext)
		}
	}

	// 验证文件类型
	src, err := file.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()

	// 读取文件头部识别 MIME 类型
	buffer := make([]byte, 512)
	_, err = src.Read(buffer)
	if err != nil && err != io.EOF {
		return "", err
	}
	if _, err := src.Seek(0, 0); err != nil { // 重置文件读取位置
		return "", err
	}

	contentType := http.DetectContentType(buffer)
	if normalizedScene != "telegram" && len(s.cfg.Upload.AllowedTypes) > 0 {
		allowed := false
		for _, t := range s.cfg.Upload.AllowedTypes {
			if strings.EqualFold(contentType, t) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("文件类型不被允许: %s", contentType)
		}
	}

	if strings.HasPrefix(contentType, "image/") {
		if _, err := src.Seek(0, 0); err != nil {
			return "", err
		}
		width, height, err := decodeImageDimensions(src, contentType)
		if err != nil {
			return "", err
		}
		if s.cfg.Upload.MaxWidth > 0 && width > s.cfg.Upload.MaxWidth {
			return "", fmt.Errorf("图片宽度超过限制（最大 %d）", s.cfg.Upload.MaxWidth)
		}
		if s.cfg.Upload.MaxHeight > 0 && height > s.cfg.Upload.MaxHeight {
			return "", fmt.Errorf("图片高度超过限制（最大 %d）", s.cfg.Upload.MaxHeight)
		}
	}

	if _, err := src.Seek(0, 0); err != nil {
		return "", err
	}
	// 生成唯一文件名
	filename := fmt.Sprintf("%s%s", uuid.New().String(), ext)
	now := time.Now()
	year := now.Format("2006")
	month := now.Format("01")
	savePath := filepath.Join("uploads", normalizedScene, year, month, filename)

	// 确保上传目录存在
	if err := os.MkdirAll(filepath.Dir(savePath), 0755); err != nil {
		return "", err
	}

	// 保存文件
	dst, err := os.Create(savePath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	if err != nil {
		return "", err
	}

	// 返回相对路径，由前端根据环境配置拼接完整 URL
	return fmt.Sprintf("/uploads/%s/%s/%s/%s", normalizedScene, year, month, filename), nil
}

func normalizeUploadScene(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "common"
	}
	if _, ok := allowedUploadScenes[value]; ok {
		return value
	}
	return "common"
}

func isAllowedExtension(ext string, allowed []string) bool {
	for _, allowedExt := range allowed {
		normalized := strings.ToLower(strings.TrimSpace(allowedExt))
		if normalized == "" {
			continue
		}
		if !strings.HasPrefix(normalized, ".") {
			normalized = "." + normalized
		}
		if strings.EqualFold(ext, normalized) {
			return true
		}
	}
	return false
}

func decodeImageDimensions(src io.ReadSeeker, contentType string) (int, int, error) {
	if strings.EqualFold(contentType, "image/webp") {
		width, height, err := decodeWebPDimensions(src)
		if err != nil {
			return 0, 0, fmt.Errorf("无法解析 WebP 图片: %w", err)
		}
		return width, height, nil
	}

	if _, err := src.Seek(0, 0); err != nil {
		return 0, 0, err
	}
	cfg, _, err := image.DecodeConfig(src)
	if err != nil {
		return 0, 0, fmt.Errorf("无法解析图片: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

func decodeWebPDimensions(src io.ReadSeeker) (int, int, error) {
	if _, err := src.Seek(0, 0); err != nil {
		return 0, 0, err
	}

	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return 0, 0, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("无效的 WebP 文件头")
	}

	for {
		chunkHeader := make([]byte, 8)
		if _, err := io.ReadFull(src, chunkHeader); err != nil {
			return 0, 0, err
		}
		chunkType := string(chunkHeader[0:4])
		chunkSize := int(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		if chunkSize < 0 {
			return 0, 0, fmt.Errorf("无效的 WebP chunk")
		}

		data := make([]byte, chunkSize)
		if _, err := io.ReadFull(src, data); err != nil {
			return 0, 0, err
		}

		if chunkType == "VP8X" {
			if len(data) < 10 {
				return 0, 0, fmt.Errorf("VP8X chunk 长度不足")
			}
			width := 1 + int(data[4]) + int(data[5])<<8 + int(data[6])<<16
			height := 1 + int(data[7]) + int(data[8])<<8 + int(data[9])<<16
			return width, height, nil
		}
		if chunkType == "VP8 " {
			if len(data) < 10 {
				return 0, 0, fmt.Errorf("VP8 chunk 长度不足")
			}
			width := int(binary.LittleEndian.Uint16(data[6:8]) & 0x3FFF)
			height := int(binary.LittleEndian.Uint16(data[8:10]) & 0x3FFF)
			return width, height, nil
		}
		if chunkType == "VP8L" {
			if len(data) < 5 {
				return 0, 0, fmt.Errorf("VP8L chunk 长度不足")
			}
			if data[0] != 0x2f {
				return 0, 0, fmt.Errorf("VP8L 签名无效")
			}
			bits := binary.LittleEndian.Uint32(data[1:5])
			width := int(bits&0x3FFF) + 1
			height := int((bits>>14)&0x3FFF) + 1
			return width, height, nil
		}

		if chunkSize%2 == 1 {
			if _, err := src.Seek(1, io.SeekCurrent); err != nil {
				return 0, 0, err
			}
		}
	}
}
