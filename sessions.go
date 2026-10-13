package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Session disimpan di tabel public.sessions (Supabase), bukan di memori proses.
// Di Vercel setiap request bisa dilayani instance berbeda, sehingga map di memori
// tidak dapat dipakai untuk menyimpan sesi. Yang disimpan hanya hash SHA-256 token.

const (
	sessionKindVoter = "voter"
	sessionKindAdmin = "admin"

	voterSessionTTL = 3 * time.Hour
	adminSessionTTL = 8 * time.Hour
)

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func sessionTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// createSession membuat token acak, menyimpan hash-nya di Supabase, dan mengembalikan token mentah.
func createSession(ctx context.Context, kind, nisn string, ttl time.Duration) (string, error) {
	raw, err := token()
	if err != nil {
		return "", err
	}
	row := map[string]any{
		"token_hash": hashToken(raw),
		"kind":       kind,
		"expires_at": sessionTime(time.Now().Add(ttl)),
	}
	if nisn != "" {
		row["nisn"] = nisn
	}
	if err := store.decode(ctx, http.MethodPost, "sessions", nil, row, "return=minimal", nil); err != nil {
		return "", fmt.Errorf("gagal menyimpan sesi: %w", err)
	}
	return raw, nil
}

// findSession mengembalikan NISN (kosong untuk admin) dan ok=true bila sesi ada dan belum kedaluwarsa.
// err hanya terisi bila Supabase gagal dijangkau, supaya gangguan sementara tidak dianggap "sesi tidak valid".
func findSession(ctx context.Context, kind, raw string) (nisn string, ok bool, err error) {
	if raw == "" {
		return "", false, nil
	}
	query := url.Values{
		"select":     {"nisn"},
		"token_hash": {"eq." + hashToken(raw)},
		"kind":       {"eq." + kind},
		"expires_at": {"gt." + sessionTime(time.Now())},
		"limit":      {"1"},
	}
	var rows []struct {
		NISN *string `json:"nisn"`
	}
	if err := store.decode(ctx, http.MethodGet, "sessions", query, nil, "", &rows); err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	if rows[0].NISN != nil {
		nisn = *rows[0].NISN
	}
	return nisn, true, nil
}

func deleteSession(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	query := url.Values{"token_hash": {"eq." + hashToken(raw)}}
	return store.decode(ctx, http.MethodDelete, "sessions", query, nil, "return=minimal", nil)
}

// purgeExpiredSessions membersihkan sesi kedaluwarsa; dipanggil saat login.
func purgeExpiredSessions(ctx context.Context) error {
	query := url.Values{"expires_at": {"lt." + sessionTime(time.Now())}}
	return store.decode(ctx, http.MethodDelete, "sessions", query, nil, "return=minimal", nil)
}
