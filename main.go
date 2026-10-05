package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

type Voter struct {
	NISN      string `json:"nisn"`
	Voted     bool   `json:"voted"`
	Blocked   bool   `json:"blocked"`
	BlockedAt string `json:"blockedAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
type Candidate struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	Name   string `json:"name"`
	Vision string `json:"vision"`
}
type DB struct {
	Voters     map[string]*Voter `json:"voters"`
	Candidates []Candidate       `json:"candidates"`
	Counts     map[int]int       `json:"counts"` // anonim: tidak ada relasi NISN -> pilihan
	NextID     int               `json:"nextId"`
}

var (
	mu = sync.Mutex{}
	db = &DB{Voters: map[string]*Voter{}, Counts: map[int]int{}, NextID: 3, Candidates: []Candidate{
		{1, 1, "Kandidat Satu", "Visi dan misi kandidat nomor 1."},
		{2, 2, "Kandidat Dua", "Visi dan misi kandidat nomor 2."},
	}}
	sessions = map[string]string{}
	admins   = map[string]time.Time{} // token -> kedaluwarsa
	nisnRe   = regexp.MustCompile(`^\d{10}$`)
	dataFile string
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func save() { b, _ := json.Marshal(db); os.WriteFile(dataFile, b, 0600) }
func load() {
	if b, err := os.ReadFile(dataFile); err == nil {
		json.Unmarshal(b, db)
	}
	if db.Voters == nil {
		db.Voters = map[string]*Voter{}
	}
	if db.Counts == nil {
		db.Counts = map[int]int{}
	}
}
func token() string { b := make([]byte, 24); rand.Read(b); return hex.EncodeToString(b) }
func js(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) { js(w, code, map[string]string{"error": msg}) }
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

type ctxKey struct{}

func voterAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		nisn, ok := sessions[bearer(r)]
		mu.Unlock()
		if !ok {
			fail(w, 401, "Sesi tidak valid, silakan masuk lagi")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, nisn)))
	})
}
func adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		exp, found := admins[bearer(r)]
		ok := found && time.Now().Before(exp)
		if found && !ok {
			delete(admins, bearer(r))
		}
		mu.Unlock()
		if !ok {
			fail(w, 401, "Akses admin ditolak")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sendSheet(payload map[string]any) {
	url := os.Getenv("SHEET_URL")
	if url == "" {
		log.Println("[sheet] SHEET_URL kosong, suara tidak dikirim ke spreadsheet")
		return
	}
	payload["secret"] = os.Getenv("SHEET_SECRET")
	b, _ := json.Marshal(payload)
	c := &http.Client{Timeout: 30 * time.Second}
	for i := 1; i <= 3; i++ {
		resp, err := c.Post(url, "application/json", bytes.NewReader(b))
		if err != nil {
			log.Printf("[sheet] percobaan %d error: %v", i, err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		resp.Body.Close()
		text := strings.TrimSpace(string(body))
		if resp.StatusCode == 200 && text == "ok" {
			log.Println("[sheet] suara terkirim")
			return
		}
		log.Printf("[sheet] percobaan %d gagal: status=%d respon=%q", i, resp.StatusCode, text)
		if text == "forbidden" {
			log.Println("[sheet] SHEET_SECRET tidak sama dengan properti SECRET di Apps Script")
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func main() {
	loadDotEnv()
	validateEnv()
	dataFile = env("DATA_FILE", "data.json")
	load()
	log.Println("[sheet] pengiriman ke spreadsheet aktif:", os.Getenv("SHEET_URL") != "")
	r := chi.NewRouter()
	r.Use(middleware.Logger, middleware.Recoverer, middleware.Timeout(30*time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{env("ALLOWED_ORIGIN", "http://localhost:3000")},
		AllowedMethods: []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
	}))

	r.Route("/api", func(r chi.Router) {
		r.Get("/candidates", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			js(w, 200, db.Candidates)
		})
		r.With(rateLimit(15)).Post("/login", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				NISN string `json:"nisn"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			defer mu.Unlock()
			v, ok := db.Voters[in.NISN]
			switch {
			case !ok:
				fail(w, 403, "NISN belum terdaftar. Hubungi admin.")
			case v.Blocked:
				fail(w, 423, "blocked")
			default:
				t := token()
				sessions[t] = in.NISN
				js(w, 200, map[string]any{"token": t, "voted": v.Voted})
			}
		})
		r.Group(func(r chi.Router) {
			r.Use(voterAuth)
			r.Get("/me", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				v := db.Voters[r.Context().Value(ctxKey{}).(string)]
				js(w, 200, map[string]any{"voted": v.Voted, "blocked": v.Blocked})
			})
			r.Post("/vote", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					CandidateID int `json:"candidateId"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				nisn := r.Context().Value(ctxKey{}).(string)
				mu.Lock()
				v := db.Voters[nisn]
				if v == nil || v.Blocked || v.Voted {
					mu.Unlock()
					fail(w, 403, "Kamu tidak dapat memilih")
					return
				}
				name := ""
				for _, c := range db.Candidates {
					if c.ID == in.CandidateID {
						name = c.Name
					}
				}
				if name == "" {
					mu.Unlock()
					fail(w, 400, "Kandidat tidak ditemukan")
					return
				}
				v.Voted = true
				db.Counts[in.CandidateID]++
				delete(sessions, bearer(r))
				save()
				mu.Unlock()
				// anonim: hanya nama kandidat, tanpa NISN dan tanpa waktu
				go sendSheet(map[string]any{"type": "vote", "candidate": name})
				js(w, 200, map[string]bool{"ok": true})
			})
			r.Post("/violation", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Reason string `json:"reason"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				nisn := r.Context().Value(ctxKey{}).(string)
				mu.Lock()
				if v := db.Voters[nisn]; v != nil && !v.Voted {
					v.Blocked, v.Reason, v.BlockedAt = true, in.Reason, time.Now().Format(time.RFC3339)
					save()
				}
				delete(sessions, bearer(r))
				mu.Unlock()
				js(w, 200, map[string]bool{"ok": true})
			})
		})

		r.With(rateLimit(5)).Post("/admin/login", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Password string `json:"password"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if subtle.ConstantTimeCompare([]byte(in.Password), []byte(os.Getenv("ADMIN_PASSWORD"))) != 1 {
				fail(w, 401, "Password salah")
				return
			}
			t := token()
			mu.Lock()
			admins[t] = time.Now().Add(8 * time.Hour)
			mu.Unlock()
			js(w, 200, map[string]string{"token": t})
		})
		r.Route("/admin", func(r chi.Router) {
			r.Use(adminAuth)
			r.Get("/stats", func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				type row struct {
					Candidate
					Votes int `json:"votes"`
				}
				rows := []row{}
				for _, c := range db.Candidates {
					rows = append(rows, row{c, db.Counts[c.ID]})
				}
				voted, blocked := 0, 0
				for _, v := range db.Voters {
					if v.Voted {
						voted++
					}
					if v.Blocked {
						blocked++
					}
				}
				js(w, 200, map[string]any{"results": rows, "total": len(db.Voters), "voted": voted, "blocked": blocked})
			})
			r.Get("/voters", func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				out := []*Voter{}
				for _, v := range db.Voters {
					out = append(out, v)
				}
				js(w, 200, out)
			})
			r.Post("/voters", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					NISN []string `json:"nisn"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				added, skipped := 0, []string{}
				mu.Lock()
				defer mu.Unlock()
				for _, n := range in.NISN {
					n = strings.TrimSpace(n)
					if _, dup := db.Voters[n]; !nisnRe.MatchString(n) || dup {
						skipped = append(skipped, n)
						continue
					}
					db.Voters[n] = &Voter{NISN: n}
					added++
				}
				save()
				js(w, 200, map[string]any{"added": added, "skipped": skipped})
			})
			r.Delete("/voters/{nisn}", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				delete(db.Voters, chi.URLParam(r, "nisn"))
				save()
				mu.Unlock()
				js(w, 200, map[string]bool{"ok": true})
			})
			r.Post("/voters/{nisn}/unblock", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if v := db.Voters[chi.URLParam(r, "nisn")]; v != nil {
					v.Blocked, v.Reason, v.BlockedAt = false, "", ""
					save()
				}
				js(w, 200, map[string]bool{"ok": true})
			})
			r.Post("/candidates", func(w http.ResponseWriter, r *http.Request) {
				var c Candidate
				json.NewDecoder(r.Body).Decode(&c)
				mu.Lock()
				defer mu.Unlock()
				c.ID = db.NextID
				db.NextID++
				db.Candidates = append(db.Candidates, c)
				save()
				js(w, 200, c)
			})
			r.Delete("/candidates/{id}", func(w http.ResponseWriter, r *http.Request) {
				id, _ := strconv.Atoi(chi.URLParam(r, "id"))
				mu.Lock()
				defer mu.Unlock()
				nc := []Candidate{}
				for _, c := range db.Candidates {
					if c.ID != id {
						nc = append(nc, c)
					}
				}
				db.Candidates = nc
				save()
				js(w, 200, map[string]bool{"ok": true})
			})
		})
	})
	addr := ":" + env("PORT", "8080")
	log.Println("API berjalan di", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}