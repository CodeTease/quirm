package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/CodeTease/quirm/pkg/config"
	"golang.org/x/sync/singleflight"
)

// Mock Storage
type MockStorage struct {
	GetObjectFunc func(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

func (m *MockStorage) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if m.GetObjectFunc != nil {
		return m.GetObjectFunc(ctx, key)
	}
	return nil, 0, errors.New("not implemented")
}

func (m *MockStorage) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", nil
}

func (m *MockStorage) Health(ctx context.Context) error {
	return nil
}

func TestValidateSignature(t *testing.T) {
	secret := "my-secret-key"
	path := "/images/test.jpg"

	t.Run("Valid signature", func(t *testing.T) {
		params := url.Values{}
		params.Set("w", "100")
		params.Set("h", "200")

		toSign := path + "?h=200&w=100" // Sorted
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(toSign))
		expectedSig := hex.EncodeToString(mac.Sum(nil))

		params.Set("s", expectedSig)

		if !validateSignature(path, params, secret) {
			t.Errorf("Expected signature to be valid")
		}
	})

	t.Run("Invalid signature", func(t *testing.T) {
		params := url.Values{}
		params.Set("w", "100")
		params.Set("s", "invalid_signature")

		if validateSignature(path, params, secret) {
			t.Errorf("Expected signature to be invalid")
		}
	})

	t.Run("Tampered params", func(t *testing.T) {
		params := url.Values{}
		params.Set("w", "100")

		toSign := path + "?w=100"
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(toSign))
		sig := hex.EncodeToString(mac.Sum(nil))
		params.Set("s", sig)

		params.Set("w", "200")

		if validateSignature(path, params, secret) {
			t.Errorf("Expected signature to be invalid after tampering")
		}
	})

	t.Run("Expired signature", func(t *testing.T) {
		expiredTime := time.Now().Add(-1 * time.Hour).Unix()
		params := url.Values{}
		params.Set("expires", fmt.Sprintf("%d", expiredTime))

		toSign := path + "?expires=" + fmt.Sprintf("%d", expiredTime)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(toSign))
		sig := hex.EncodeToString(mac.Sum(nil))
		params.Set("s", sig)

		if validateSignature(path, params, secret) {
			t.Errorf("Expected signature to be expired")
		}
	})
}

func TestParseImageOptions(t *testing.T) {
	t.Run("Basic parsing", func(t *testing.T) {
		params := url.Values{}
		params.Set("w", "100")
		params.Set("h", "200")
		params.Set("fit", "cover")
		params.Set("format", "webp")

		opts := parseImageOptions(params, nil)

		if opts.Width != 100 {
			t.Errorf("Expected Width 100, got %d", opts.Width)
		}
		if opts.Height != 200 {
			t.Errorf("Expected Height 200, got %d", opts.Height)
		}
		if opts.Fit != "cover" {
			t.Errorf("Expected Fit cover, got %s", opts.Fit)
		}
		if opts.Format != "webp" {
			t.Errorf("Expected Format webp, got %s", opts.Format)
		}
	})

	t.Run("Presets strict mode", func(t *testing.T) {
		presets := map[string]string{
			"small": "w=50&h=50&fit=contain",
		}

		params := url.Values{}
		params.Set("preset", "small")
		params.Set("w", "1000") // Should be ignored

		opts := parseImageOptions(params, presets)

		if opts.Width != 50 {
			t.Errorf("Expected Width 50 from preset, got %d", opts.Width)
		}
		if opts.Height != 50 {
			t.Errorf("Expected Height 50 from preset, got %d", opts.Height)
		}
		if opts.Fit != "contain" {
			t.Errorf("Expected Fit contain from preset, got %s", opts.Fit)
		}
	})
}

func TestHTTPStatusCodes(t *testing.T) {
	os.Setenv("ALLOWED_DOMAINS", "example.com")
	defer os.Unsetenv("ALLOWED_DOMAINS")
	os.Setenv("DEFAULT_IMAGE_PATH", "") // Ensure no default image fallback for 404 test
	defer os.Unsetenv("DEFAULT_IMAGE_PATH")

	cfgMgr := config.NewManager()

	mockS3 := &MockStorage{}
	h := &Handler{
		ConfigManager: cfgMgr,
		S3:            mockS3,
		Group:         &singleflight.Group{},
		CacheDir:      os.TempDir(), // Use temp dir
	}

	t.Run("Allowed Domain", func(t *testing.T) {
		// Mock S3 to return 404 so we don't panic or need real file, 
		// but 404 means it PASSED the domain check.
		mockS3.GetObjectFunc = func(ctx context.Context, key string) (io.ReadCloser, int64, error) {
			return nil, 0, errors.New("NotFound")
		}

		req := httptest.NewRequest("GET", "/image.jpg", nil)
		req.Header.Set("Referer", "http://example.com/page")
		w := httptest.NewRecorder()

		h.HandleRequest(w, req)

		// Should be 404 (NotFound) not 403
		if w.Code == http.StatusForbidden {
			t.Errorf("Did not expect 403 Forbidden for allowed domain")
		}
		if w.Code != http.StatusNotFound {
			t.Errorf("Expected 404 NotFound, got %d", w.Code)
		}
	})

	t.Run("Forbidden Domain", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/image.jpg", nil)
		req.Header.Set("Referer", "http://evil.com/page")
		w := httptest.NewRecorder()

		h.HandleRequest(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("Expected 403 Forbidden, got %d", w.Code)
		}
	})
	
	t.Run("File Not Found", func(t *testing.T) {
		mockS3.GetObjectFunc = func(ctx context.Context, key string) (io.ReadCloser, int64, error) {
			return nil, 0, errors.New("key does not exist: NotFound")
		}
		
		req := httptest.NewRequest("GET", "/missing.jpg", nil)
		req.Header.Set("Referer", "http://example.com/page") // Allow it
		w := httptest.NewRecorder()
		
		h.HandleRequest(w, req)
		
		if w.Code != http.StatusNotFound {
			t.Errorf("Expected 404 NotFound, got %d", w.Code)
		}
	})
}
