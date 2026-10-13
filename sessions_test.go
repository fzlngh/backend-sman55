package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSessionsDB meniru tabel public.sessions di PostgREST (insert, select, delete dengan filter eq/gt/lt).
type fakeSessionsDB struct {
	mu   sync.Mutex
	rows map[string]map[string]any
}

func (f *fakeSessionsDB) matches(row map[string]any, r *http.Request) bool {
	q := r.URL.Query()
	for col, filter := range q {
		if col == "select" || col == "limit" {
			continue
		}
		op, val, _ := strings.Cut(filter[0], ".")
		got, _ := row[col].(string)
		switch op {
		case "eq":
			if got != val {
				return false
			}
		case "gt":
			if !(got > val) {
				return false
			}
		case "lt":
			if !(got < val) {
				return false
			}
		}
	}
	return true
}

func (f *fakeSessionsDB) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/rest/v1/sessions" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var row map[string]any
		_ = json.NewDecoder(r.Body).Decode(&row)
		f.rows[row["token_hash"].(string)] = row
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		out := []map[string]any{}
		for _, row := range f.rows {
			if f.matches(row, r) {
				out = append(out, map[string]any{"nisn": row["nisn"]})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodDelete:
		for k, row := range f.rows {
			if f.matches(row, r) {
				delete(f.rows, k)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func withFakeSessions(t *testing.T) *fakeSessionsDB {
	t.Helper()
	db := &fakeSessionsDB{rows: map[string]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(db.handler))
	t.Cleanup(server.Close)
	store = &supabaseStore{baseURL: server.URL, key: "k", client: server.Client()}
	return db
}

func TestSessionSurvivesAcrossInstances(t *testing.T) {
	db := withFakeSessions(t)
	ctx := context.Background()

	raw, err := createSession(ctx, sessionKindVoter, "1234567890", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := db.rows[raw]; stored {
		t.Fatal("token mentah tidak boleh disimpan, hanya hash-nya")
	}

	// Simulasi instance lain: tidak ada state di memori selain koneksi ke database yang sama.
	nisn, ok, err := findSession(ctx, sessionKindVoter, raw)
	if err != nil || !ok || nisn != "1234567890" {
		t.Fatalf("findSession = %q, %v, %v", nisn, ok, err)
	}
	if _, ok, _ := findSession(ctx, sessionKindAdmin, raw); ok {
		t.Fatal("sesi pemilih tidak boleh valid sebagai admin")
	}
	if _, ok, _ := findSession(ctx, sessionKindVoter, "salah"); ok {
		t.Fatal("token acak tidak boleh valid")
	}

	if err := deleteSession(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := findSession(ctx, sessionKindVoter, raw); ok {
		t.Fatal("sesi harus hilang setelah dihapus")
	}
}

func TestExpiredSessionRejectedAndPurged(t *testing.T) {
	db := withFakeSessions(t)
	ctx := context.Background()

	raw, err := createSession(ctx, sessionKindAdmin, "", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := findSession(ctx, sessionKindAdmin, raw); ok {
		t.Fatal("sesi kedaluwarsa harus ditolak")
	}
	if err := purgeExpiredSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.rows) != 0 {
		t.Fatalf("sesi kedaluwarsa harus dibersihkan, tersisa %d", len(db.rows))
	}
}

func TestManyConcurrentVotersKeepSessions(t *testing.T) {
	withFakeSessions(t)
	ctx := context.Background()
	const n = 50
	tokens := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], _ = createSession(ctx, sessionKindVoter, "100000000"+string(rune('0'+i%10)), time.Hour)
		}(i)
	}
	wg.Wait()
	for i, raw := range tokens {
		if _, ok, err := findSession(ctx, sessionKindVoter, raw); !ok || err != nil {
			t.Fatalf("sesi %d hilang (ok=%v err=%v)", i, ok, err)
		}
	}
}
