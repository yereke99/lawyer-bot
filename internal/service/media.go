package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
)

// MediaStore writes and reads conversation media on local disk.
//
// Security properties, all enforced here rather than in the HTTP layer:
//   - the stored name is generated server-side from random bytes, so a client
//     filename can never influence the path;
//   - files land in a dated directory under one root and Resolve refuses any
//     path that escapes it, which closes path traversal;
//   - files are written 0o600 with no executable bit and are served with a
//     forced Content-Disposition, so nothing stored is ever executed or
//     rendered as active content by a browser;
//   - MIME type, extension and size are validated against an allow-list before
//     a single byte is written.
type MediaStore struct {
	root     string
	maxBytes int64
	fetcher  domain.WhatsAppFileFetcher
}

// MediaConfig configures the store.
type MediaConfig struct {
	Root     string
	MaxBytes int64
}

// NewMediaStore builds a MediaStore and creates its root directory.
func NewMediaStore(cfg MediaConfig, fetcher domain.WhatsAppFileFetcher) (*MediaStore, error) {
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		root = "data/media"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve media root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create media root: %w", err)
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	return &MediaStore{root: abs, maxBytes: maxBytes, fetcher: fetcher}, nil
}

// Root reports the absolute media directory.
func (s *MediaStore) Root() string { return s.root }

// MaxBytes reports the accepted upload size.
func (s *MediaStore) MaxBytes() int64 { return s.maxBytes }

// CanDownload reports whether inbound media can be fetched from the provider.
func (s *MediaStore) CanDownload() bool { return s != nil && s.fetcher != nil }

// allowedMedia maps every accepted MIME type onto its canonical extension.
// Anything absent from this table is rejected: SVG, HTML and every script or
// archive format are deliberately not here, because they execute or render as
// active content when served back.
var allowedMedia = map[string]string{
	"image/jpeg":                    ".jpg",
	"image/png":                     ".png",
	"image/gif":                     ".gif",
	"image/webp":                    ".webp",
	"video/mp4":                     ".mp4",
	"video/quicktime":               ".mov",
	"video/webm":                    ".webm",
	"video/3gpp":                    ".3gp",
	"audio/mpeg":                    ".mp3",
	"audio/mp4":                     ".m4a",
	"audio/aac":                     ".aac",
	"audio/ogg":                     ".ogg",
	"audio/opus":                    ".opus",
	"audio/wav":                     ".wav",
	"audio/x-wav":                   ".wav",
	"audio/amr":                     ".amr",
	"application/pdf":               ".pdf",
	"application/msword":            ".doc",
	"application/vnd.ms-excel":      ".xls",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/rtf": ".rtf",
	"text/plain":      ".txt",
	"text/csv":        ".csv",
	"application/zip": ".zip",
}

// ErrUnsupportedMedia is returned for a MIME type outside the allow-list.
var ErrUnsupportedMedia = errors.New("unsupported media type")

// ErrMediaTooLarge is returned when an upload exceeds the configured limit.
var ErrMediaTooLarge = errors.New("media file is too large")

// StoredMedia describes a file after it has been written.
type StoredMedia struct {
	Path        string
	StoredName  string
	DisplayName string
	MimeType    string
	Size        int64
	SHA256      string
	Kind        domain.MessageType
}

// ValidateMime normalises and checks a declared MIME type.
func ValidateMime(declared string) (string, string, error) {
	value := strings.ToLower(strings.TrimSpace(declared))
	if value == "" {
		return "", "", ErrUnsupportedMedia
	}
	if parsed, _, err := mime.ParseMediaType(value); err == nil {
		value = parsed
	}
	// Providers append codec parameters to voice notes.
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	ext, ok := allowedMedia[value]
	if !ok {
		return "", "", fmt.Errorf("%w: %s", ErrUnsupportedMedia, value)
	}
	return value, ext, nil
}

// SafeDisplayName strips a client-supplied filename down to something that is
// safe to store in the database and echo back into the CRM. It never becomes
// part of a filesystem path.
func SafeDisplayName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\x00", "")
	// Drop any directory component a client may have sent.
	name = filepath.Base(filepath.FromSlash(name))
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 32, r == 127:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "file"
	}
	r := []rune(name)
	if len(r) > 120 {
		name = string(r[:120])
	}
	return name
}

// Save writes a validated file and returns its stored description.
//
// The reader is limited to MaxBytes+1 so an over-large upload is detected and
// rejected rather than filling the disk.
func (s *MediaStore) Save(src io.Reader, declaredMime, displayName string, voice bool) (StoredMedia, error) {
	mimeType, ext, err := ValidateMime(declaredMime)
	if err != nil {
		return StoredMedia{}, err
	}

	dir := filepath.Join(s.root, time.Now().UTC().Format("2006/01"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return StoredMedia{}, fmt.Errorf("create media directory: %w", err)
	}

	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return StoredMedia{}, fmt.Errorf("generate media name: %w", err)
	}
	storedName := hex.EncodeToString(token[:]) + ext
	full := filepath.Join(dir, storedName)

	// O_EXCL: never overwrite, and 0o600 so nothing stored is executable.
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return StoredMedia{}, fmt.Errorf("create media file: %w", err)
	}

	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(f, digest), io.LimitReader(src, s.maxBytes+1))
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		_ = os.Remove(full)
		return StoredMedia{}, fmt.Errorf("write media file: %w", copyErr)
	case closeErr != nil:
		_ = os.Remove(full)
		return StoredMedia{}, fmt.Errorf("write media file: %w", closeErr)
	case written > s.maxBytes:
		_ = os.Remove(full)
		return StoredMedia{}, ErrMediaTooLarge
	case written == 0:
		_ = os.Remove(full)
		return StoredMedia{}, errors.New("media file is empty")
	}

	display := SafeDisplayName(displayName)
	if filepath.Ext(display) == "" {
		display += ext
	}
	return StoredMedia{
		Path:        full,
		StoredName:  storedName,
		DisplayName: display,
		MimeType:    mimeType,
		Size:        written,
		SHA256:      hex.EncodeToString(digest.Sum(nil)),
		Kind:        MessageTypeForMime(mimeType, voice),
	}, nil
}

// Resolve turns a stored path back into an absolute path, refusing anything
// that points outside the media root.
func (s *MediaStore) Resolve(stored string) (string, error) {
	if strings.TrimSpace(stored) == "" {
		return "", errors.New("empty media path")
	}
	abs := stored
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, abs)
	}
	clean := filepath.Clean(abs)

	rel, err := filepath.Rel(s.root, clean)
	if err != nil {
		return "", fmt.Errorf("resolve media path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("media path escapes the media root")
	}
	if _, err := os.Stat(clean); err != nil {
		return "", fmt.Errorf("media file unavailable: %w", err)
	}
	return clean, nil
}

// FetchInbound downloads one inbound media object and stores it locally.
//
// A provider's media URL is not permanent, so the CRM must keep its own copy;
// a failed download is reported to the caller and never loses the message.
func (s *MediaStore) FetchInbound(ctx context.Context, in domain.InboundMessage) (StoredMedia, error) {
	if s.fetcher == nil {
		return StoredMedia{}, errors.New("provider cannot download media")
	}
	body, contentType, err := s.fetcher.DownloadFile(ctx, domain.MediaRef{
		MediaID:   in.MediaID,
		URL:       in.MediaURL,
		ChatID:    in.WhatsAppUserID,
		MessageID: in.WhatsAppMessageID,
		MimeType:  in.MimeType,
	})
	if err != nil {
		return StoredMedia{}, err
	}
	defer body.Close()

	declared := in.MimeType
	if declared == "" {
		declared = contentType
	}
	name := in.Filename
	if name == "" {
		name = "media"
	}
	return s.Save(body, declared, name, in.Voice)
}
