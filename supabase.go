package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type supabaseStore struct {
	baseURL string
	key     string
	client  *http.Client
}

func newSupabaseStore(projectURL, serviceKey string) (*supabaseStore, error) {
	parsed, err := url.Parse(strings.TrimRight(projectURL, "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("SUPABASE_URL harus berupa URL HTTPS yang valid")
	}
	if strings.TrimSpace(serviceKey) == "" {
		return nil, fmt.Errorf("SUPABASE_SERVICE_ROLE_KEY wajib diisi")
	}
	return &supabaseStore{
		baseURL: parsed.String(),
		key:     serviceKey,
		client:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (s *supabaseStore) request(ctx context.Context, method, path string, query url.Values, body any, prefer string) ([]byte, error) {
	endpoint, err := url.Parse(s.baseURL + "/rest/v1/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return nil, fmt.Errorf("URL endpoint Supabase tidak valid: %w", err)
	}
	endpoint.RawQuery = query.Encode()

	var requestBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gagal menyiapkan permintaan Supabase: %w", err)
		}
		requestBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), requestBody)
	if err != nil {
		return nil, fmt.Errorf("gagal membuat permintaan Supabase: %w", err)
	}
	req.Header.Set("apikey", s.key)
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("permintaan ke Supabase gagal: %w", err)
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("gagal membaca respons Supabase: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &supabaseError{status: resp.StatusCode, body: string(response)}
	}
	return response, nil
}

func (s *supabaseStore) decode(ctx context.Context, method, path string, query url.Values, body any, prefer string, target any) error {
	response, err := s.request(ctx, method, path, query, body, prefer)
	if err != nil {
		return err
	}
	if target == nil || len(response) == 0 {
		return nil
	}
	if err := json.Unmarshal(response, target); err != nil {
		return fmt.Errorf("respons Supabase tidak valid: %w", err)
	}
	return nil
}

// uploadObject menyimpan file ke bucket Supabase Storage (publik) dan mengembalikan URL publiknya.
func (s *supabaseStore) uploadObject(ctx context.Context, bucket, name, contentType string, data []byte) (string, error) {
	endpoint := s.baseURL + "/storage/v1/object/" + bucket + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("gagal membuat permintaan upload: %w", err)
	}
	req.Header.Set("apikey", s.key)
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Cache-Control", "max-age=31536000")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload ke Supabase Storage gagal: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &supabaseError{status: resp.StatusCode, body: string(body)}
	}
	return s.baseURL + "/storage/v1/object/public/" + bucket + "/" + name, nil
}

func (s *supabaseStore) sendSheetVote(candidate string) {
	sheetURL := os.Getenv("SHEET_URL")
	if sheetURL == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"type":      "vote",
		"candidate": candidate,
		"secret":    os.Getenv("SHEET_SECRET"),
	})
	if err != nil {
		log.Printf("gagal menyiapkan rekap Sheets: %v", err)
		return
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sheetURL, bytes.NewReader(payload))
		if err != nil {
			cancel()
			log.Printf("URL SHEET_URL tidak valid: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 300))
			closeErr := resp.Body.Close()
			cancel()
			if readErr != nil || closeErr != nil {
				log.Printf("gagal membaca respons Google Sheets (percobaan %d)", attempt)
			} else if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok" {
				return
			} else if strings.TrimSpace(string(body)) == "forbidden" {
				log.Printf("Google Sheets menolak secret; periksa SHEET_SECRET dan Script Properties SECRET")
				return
			} else {
				log.Printf("Google Sheets gagal menerima rekap (percobaan %d, status %d)", attempt, resp.StatusCode)
			}
		} else {
			cancel()
			log.Printf("gagal mengirim rekap ke Google Sheets (percobaan %d): %v", attempt, err)
		}
		if attempt < 3 {
			time.Sleep(2 * time.Second)
		}
	}
}

type supabaseError struct {
	status int
	body   string
}

func (e *supabaseError) Error() string {
	return fmt.Sprintf("Supabase REST mengembalikan HTTP %d: %s", e.status, e.body)
}