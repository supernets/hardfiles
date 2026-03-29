package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// testConf sets up a test config using temp directories.
func testConf(t *testing.T) (filesDir, bgDir, wwwDir string) {
	t.Helper()
	filesDir = t.TempDir()
	bgDir = t.TempDir()
	wwwDir = t.TempDir()

	conf = Config{
		Webroot:         wwwDir,
		Lport:           "0",
		Vhost:           "test.local",
		Filelen:         6,
		Folder:          filesDir,
		Bgfolder:        bgDir,
		MaxUploadMB:     10,
		TTLHours:        24,
		RateLimitPerMin: 100,
	}
	rateLimiter = NewRateLimiter(conf.RateLimitPerMin)
	return
}

// buildUploadRequest creates a multipart/form-data POST with an optional password.
func buildUploadRequest(t *testing.T, filename, content, password string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if password != "" {
		_ = mw.WriteField("password", password)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatalf("write content: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// ---- Upload tests ----------------------------------------------------------

func TestUploadHappyPath(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := buildUploadRequest(t, "hello.txt", "hello world", "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(body, "https://test.local/f/") {
		t.Fatalf("unexpected response body: %q", body)
	}
}

func TestUploadWithPassword(t *testing.T) {
	filesDir, _, _ := testConf(t)
	handler := newMux()

	req := buildUploadRequest(t, "secret.txt", "top secret", "hunter2")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := strings.TrimSpace(rr.Body.String())
	fileID := strings.TrimPrefix(body, "https://test.local/f/")

	// Meta sidecar must exist.
	metaPath := filepath.Join(filesDir, fileID+".meta")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("expected .meta sidecar at %s: %v", metaPath, err)
	}
}

func TestUploadExceedsSizeLimit(t *testing.T) {
	testConf(t)
	conf.MaxUploadMB = 1 // 1 MB limit for this test
	rateLimiter = NewRateLimiter(conf.RateLimitPerMin)
	handler := newMux()

	// Build a body larger than 1 MB
	bigContent := strings.Repeat("x", 2*1024*1024)
	req := buildUploadRequest(t, "big.bin", bigContent, "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUploadNoFile(t *testing.T) {
	testConf(t)
	handler := newMux()

	// POST with only a password field, no file
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("password", "nope")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUploadMIMEDetection(t *testing.T) {
	testConf(t)
	handler := newMux()

	// PNG magic bytes
	pngHeader := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 100)
	req := buildUploadRequest(t, "image.png", pngHeader, "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	fileID := strings.TrimPrefix(body, "https://test.local/f/")
	if !strings.HasSuffix(fileID, ".png") {
		t.Fatalf("expected .png extension, got %q", fileID)
	}
}

func TestUploadTempFileCleanedOnError(t *testing.T) {
	testConf(t)
	// Make the files dir read-only so CreateTemp succeeds but Rename fails.
	// Instead, verify no temp files linger after a bad request (no file field).
	handler := newMux()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Any response is fine; verify no .upload-* temp files remain.
	entries, _ := os.ReadDir(conf.Folder)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// ---- Download tests --------------------------------------------------------

func writeTestFile(t *testing.T, filesDir, name, content string) string {
	t.Helper()
	path := filepath.Join(filesDir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

func TestDownloadHappyPath(t *testing.T) {
	filesDir, _, _ := testConf(t)
	handler := newMux()

	writeTestFile(t, filesDir, "abc123.txt", "file contents")
	req := httptest.NewRequest(http.MethodGet, "/f/abc123.txt", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "file contents") {
		t.Fatalf("body missing file contents: %q", rr.Body.String())
	}
}

func TestDownload404(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := httptest.NewRequest(http.MethodGet, "/f/doesnotexist.txt", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestDownloadPasswordProtectedGETPrompt(t *testing.T) {
	filesDir, _, _ := testConf(t)
	handler := newMux()

	writeTestFile(t, filesDir, "locked.txt", "secret")
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass123"), bcrypt.MinCost)
	meta := &FileMeta{PasswordHash: string(hash)}
	_ = writeMeta(filepath.Join(filesDir, "locked.txt.meta"), meta)

	req := httptest.NewRequest(http.MethodGet, "/f/locked.txt", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (password prompt), got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "PASSWORD") {
		t.Fatalf("expected password prompt in body")
	}
}

func TestDownloadPasswordCorrect(t *testing.T) {
	filesDir, _, _ := testConf(t)
	handler := newMux()

	writeTestFile(t, filesDir, "locked2.txt", "secret content")
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.MinCost)
	meta := &FileMeta{PasswordHash: string(hash)}
	_ = writeMeta(filepath.Join(filesDir, "locked2.txt.meta"), meta)

	body := strings.NewReader("password=correct")
	req := httptest.NewRequest(http.MethodPost, "/f/locked2.txt", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "secret content") {
		t.Fatalf("expected file content in response")
	}
}

func TestDownloadPasswordWrong(t *testing.T) {
	filesDir, _, _ := testConf(t)
	handler := newMux()

	writeTestFile(t, filesDir, "locked3.txt", "secret content")
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.MinCost)
	meta := &FileMeta{PasswordHash: string(hash)}
	_ = writeMeta(filepath.Join(filesDir, "locked3.txt.meta"), meta)

	body := strings.NewReader("password=wrong")
	req := httptest.NewRequest(http.MethodPost, "/f/locked3.txt", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "WRONG PASSWORD") {
		t.Fatalf("expected WRONG PASSWORD in body")
	}
}

// ---- Path traversal tests --------------------------------------------------

func TestPathTraversalFile(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := httptest.NewRequest(http.MethodGet, "/f/../etc/passwd", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Should either be 400 or 404 — not 200
	if rr.Code == http.StatusOK {
		t.Fatalf("path traversal succeeded: got 200")
	}
}

func TestPathTraversalBg(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := httptest.NewRequest(http.MethodGet, "/bg/../etc/passwd", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("path traversal on /bg/ succeeded: got 200")
	}
}

func TestPathTraversalStatic(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := httptest.NewRequest(http.MethodGet, "/static/../etc/passwd", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("path traversal on /static/ succeeded: got 200")
	}
}

// ---- Corrupt meta ----------------------------------------------------------

func TestCorruptMetaFailOpen(t *testing.T) {
	filesDir, _, _ := testConf(t)
	handler := newMux()

	writeTestFile(t, filesDir, "corrupt.txt", "open content")
	// Write invalid JSON to the meta sidecar.
	if err := os.WriteFile(filepath.Join(filesDir, "corrupt.txt.meta"), []byte("{bad json"), 0644); err != nil {
		t.Fatalf("write corrupt meta: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/f/corrupt.txt", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Should serve file (fail open), not error.
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (fail open), got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---- Index page ------------------------------------------------------------

func TestIndexPage(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "HARDFILES") {
		t.Fatalf("index page missing HARDFILES")
	}
}

func TestIndexWithBackgrounds(t *testing.T) {
	_, bgDir, _ := testConf(t)
	handler := newMux()

	// Create a fake gif in the bg dir.
	gifPath := filepath.Join(bgDir, "test.gif")
	if err := os.WriteFile(gifPath, []byte("GIF89a"), 0644); err != nil {
		t.Fatalf("write gif: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "/bg/test.gif") {
		t.Fatalf("expected bg URL in index body")
	}
}

func TestIndexEmptyBackgrounds(t *testing.T) {
	testConf(t)
	handler := newMux()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with empty bg dir, got %d", rr.Code)
	}
}

// ---- NameGen ---------------------------------------------------------------

func TestNameGen(t *testing.T) {
	const length = 8
	ambiguous := "0OIl1"
	seen := make(map[string]bool)

	for i := 0; i < 100; i++ {
		id, err := genID(length)
		if err != nil {
			t.Fatalf("genID error: %v", err)
		}
		if len(id) != length {
			t.Errorf("expected length %d, got %d: %q", length, len(id), id)
		}
		for _, c := range ambiguous {
			if strings.ContainsRune(id, c) {
				t.Errorf("id %q contains ambiguous char %c", id, c)
			}
		}
		seen[id] = true
	}
	// With a 54-char alphabet and length 8, collision in 100 draws is astronomically unlikely.
	if len(seen) < 90 {
		t.Errorf("too many collisions: only %d unique IDs in 100 draws", len(seen))
	}
}

// ---- Shred -----------------------------------------------------------------

func TestShred(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shred_me.txt")
	original := "sensitive data here"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := shredFile(path); err != nil {
		t.Fatalf("shredFile: %v", err)
	}

	// File should be gone after shredding.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists after shred")
	}
}

// ---- Rate limiter ----------------------------------------------------------

func TestRateLimiterAllowsUnderLimit(t *testing.T) {
	rl := NewRateLimiter(5)
	for i := 0; i < 5; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
}

func TestRateLimiterBlocksOverLimit(t *testing.T) {
	rl := NewRateLimiter(3)
	for i := 0; i < 3; i++ {
		rl.Allow("1.2.3.4")
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("4th request should be blocked")
	}
}

func TestRateLimiterDifferentIPs(t *testing.T) {
	rl := NewRateLimiter(2)
	for i := 0; i < 2; i++ {
		rl.Allow("10.0.0.1")
	}
	// Different IP should still be allowed.
	if !rl.Allow("10.0.0.2") {
		t.Fatal("different IP should not be rate limited")
	}
}

func TestRateLimiterResetsAfterWindow(t *testing.T) {
	rl := NewRateLimiter(2)
	rl.Allow("5.5.5.5")
	rl.Allow("5.5.5.5")
	// Manually age the timestamps so they fall outside the window.
	rl.mu.Lock()
	rec := rl.records["5.5.5.5"]
	rl.mu.Unlock()
	rec.mu.Lock()
	for i := range rec.timestamps {
		rec.timestamps[i] = time.Now().Add(-2 * time.Minute)
	}
	rec.mu.Unlock()

	if !rl.Allow("5.5.5.5") {
		t.Fatal("should be allowed after window resets")
	}
}

// ---- Upload returns URL that can be downloaded -----------------------------

func TestUploadThenDownload(t *testing.T) {
	testConf(t)
	handler := newMux()

	// Upload
	req := buildUploadRequest(t, "round.txt", "round trip content", "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d", rr.Code)
	}
	uploadedURL := strings.TrimSpace(rr.Body.String())
	fileID := strings.TrimPrefix(uploadedURL, "https://test.local/f/")

	// Download
	req2 := httptest.NewRequest(http.MethodGet, "/f/"+fileID, nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "round trip content") {
		t.Fatalf("downloaded content mismatch: %q", rr2.Body.String())
	}
}

// ---- Integration: rate limit via HTTP handler ------------------------------

func TestUploadRateLimitedViaHandler(t *testing.T) {
	testConf(t)
	conf.RateLimitPerMin = 2
	rateLimiter = NewRateLimiter(conf.RateLimitPerMin)
	handler := newMux()

	makeUpload := func() int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "f.txt")
		fmt.Fprint(fw, "data")
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.RemoteAddr = "9.9.9.9:1234"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr.Code
	}

	if c := makeUpload(); c != http.StatusOK {
		t.Fatalf("1st upload: expected 200, got %d", c)
	}
	if c := makeUpload(); c != http.StatusOK {
		t.Fatalf("2nd upload: expected 200, got %d", c)
	}
	if c := makeUpload(); c != http.StatusTooManyRequests {
		t.Fatalf("3rd upload: expected 429, got %d", c)
	}
}
