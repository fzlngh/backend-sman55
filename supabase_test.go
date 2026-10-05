package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSupabaseRequestUsesServerKeyAndQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("apikey"); got != "server-only-key" {
			t.Errorf("apikey = %q, want server-only key", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer server-only-key" {
			t.Errorf("authorization = %q, want bearer server-only key", got)
		}
		if got := r.URL.Query().Get("nisn"); got != "eq.1234567890" {
			t.Errorf("nisn filter = %q, want exact voter filter", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"voted":false,"blocked":false}]`))
	}))
	defer server.Close()

	store := &supabaseStore{baseURL: server.URL, key: "server-only-key", client: server.Client()}
	query := url.Values{"nisn": {"eq.1234567890"}}
	var voters []Voter
	if err := store.decode(context.Background(), http.MethodGet, "voters", query, nil, "", &voters); err != nil {
		t.Fatalf("decode returned error: %v", err)
	}
	if len(voters) != 1 || voters[0].NISN != "" || voters[0].Voted || voters[0].Blocked {
		t.Fatalf("unexpected decoded voters: %+v", voters)
	}
}

func TestSupabaseRPCDecodesScalarResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/v1/rpc/cast_vote" {
			t.Errorf("path = %q, want RPC path", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if payload["p_nisn"] != "1234567890" || payload["p_candidate_id"] != float64(2) {
			t.Errorf("unexpected RPC payload: %#v", payload)
		}
		_, _ = w.Write([]byte("true"))
	}))
	defer server.Close()

	store := &supabaseStore{baseURL: server.URL, key: "server-only-key", client: server.Client()}
	var accepted bool
	err := store.decode(context.Background(), http.MethodPost, "rpc/cast_vote", nil,
		map[string]any{"p_nisn": "1234567890", "p_candidate_id": 2}, "", &accepted)
	if err != nil {
		t.Fatalf("decode returned error: %v", err)
	}
	if !accepted {
		t.Fatal("vote RPC result = false, want true")
	}
}

func TestSendSheetVoteOnlyIncludesCandidateAndSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode Sheets payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(payload) != 3 || payload["type"] != "vote" ||
			payload["candidate"] != "Kandidat Satu" || payload["secret"] != "sheet-secret-value" {
			t.Errorf("unexpected Sheets payload: %#v", payload)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	t.Setenv("SHEET_URL", server.URL)
	t.Setenv("SHEET_SECRET", "sheet-secret-value")
	store := &supabaseStore{client: server.Client()}
	store.sendSheetVote("Kandidat Satu")
}
