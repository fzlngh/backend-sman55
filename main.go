package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

type Voter struct {
	NISN      string     `json:"nisn"`
	Voted     bool       `json:"voted"`
	Blocked   bool       `json:"blocked"`
	BlockedAt *time.Time `json:"blockedAt,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type Candidate struct {
	ID       int64  `json:"id,omitempty"`
	Number   int    `json:"number"`
	Name     string `json:"name"`
	Vision   string `json:"vision"`
	PhotoURL string `json:"photo_url"`
}

type Result struct {
	Candidate
	Votes int64 `json:"votes"`
}

type Stats struct {
	Results []Result `json:"results"`
	Total   int64    `json:"total"`
	Voted   int64    `json:"voted"`
	Blocked int64    `json:"blocked"`
}

var (
	store    *supabaseStore
	mu       sync.Mutex
	sessions = map[string]string{}
	admins   = map[string]time.Time{}
	nisnRe   = regexp.MustCompile(`^\d{5,12}$`)
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func allowedOrigins() []string {
	configured := strings.Split(
		env("ALLOWED_ORIGIN", "http://localhost:3000,https://pemilu-sman55.vercel.app"),
		",",
	)
	origins := make([]string, 0, len(configured)+1)
	seen := make(map[string]bool, len(configured)+1)
	for _, origin := range configured {
		origin = strings.TrimSpace(origin)
		if origin != "" && !seen[origin] {
			origins = append(origins, origin)
			seen[origin] = true
		}
	}
	if !seen["https://pemilu-sman55.vercel.app"] {
		origins = append(origins, "https://pemilu-sman55.vercel.app")
	}
	return origins
}

func token() (string, error) {
	value := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func js(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("gagal menulis respons JSON: %v", err)
	}
}

func fail(w http.ResponseWriter, code int, message string) {
	js(w, code, map[string]string{"error": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		fail(w, http.StatusBadRequest, "Data permintaan tidak valid")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "Data permintaan tidak valid")
		return false
	}
	return true
}

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
			fail(w, http.StatusUnauthorized, "Sesi tidak valid, silakan masuk lagi")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, nisn)))
	})
}

func adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		key := bearer(r)
		expiresAt, found := admins[key]
		ok := found && time.Now().Before(expiresAt)
		if found && !ok {
			delete(admins, key)
		}
		mu.Unlock()
		if !ok {
			fail(w, http.StatusUnauthorized, "Akses admin ditolak")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func listCandidates(ctx context.Context) ([]Candidate, error) {
	query := url.Values{
		"select": {"id,number,name,vision,photo_url"},
		"order":  {"number.asc,id.asc"},
	}
	candidates := make([]Candidate, 0)
	err := store.decode(ctx, http.MethodGet, "candidates", query, nil, "", &candidates)
	return candidates, err
}

func getCandidate(ctx context.Context, id int64) (Candidate, error) {
	query := url.Values{
		"select": {"id,number,name,vision,photo_url"},
		"id":     {"eq." + strconv.FormatInt(id, 10)},
		"limit":  {"1"},
	}
	var candidates []Candidate
	if err := store.decode(ctx, http.MethodGet, "candidates", query, nil, "", &candidates); err != nil {
		return Candidate{}, err
	}
	if len(candidates) == 0 {
		return Candidate{}, errors.New("kandidat tidak ditemukan")
	}
	return candidates[0], nil
}

func loadLegacyData(ctx context.Context) error {
	path := env("LEGACY_DATA_FILE", "data.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var payload json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	var imported bool
	if err := store.decode(ctx, http.MethodPost, "rpc/import_legacy_data", nil,
		map[string]json.RawMessage{"p_data": payload}, "", &imported); err != nil {
		return err
	}
	if imported {
		log.Println("data lama berhasil diimpor ke Supabase")
	} else {
		log.Println("impor data lama dilewati karena sudah pernah diimpor atau tabel telah berisi data")
	}
	return nil
}

func ensureInitialCandidates(ctx context.Context) error {
	candidates, err := listCandidates(ctx)
	if err != nil {
		return err
	}
	if len(candidates) != 0 {
		return nil
	}
	defaults := []Candidate{
		{Number: 1, Name: "Kandidat Satu", Vision: "Visi dan misi kandidat nomor 1."},
		{Number: 2, Name: "Kandidat Dua", Vision: "Visi dan misi kandidat nomor 2."},
	}
	return store.decode(ctx, http.MethodPost, "candidates", nil, defaults, "return=minimal", nil)
}

func main() {
	loadDotEnv()
	validateEnv()

	var err error
	store, err = newSupabaseStore(os.Getenv("SUPABASE_URL"), os.Getenv("SUPABASE_SERVICE_ROLE_KEY"))
	if err != nil {
		log.Fatalf("konfigurasi Supabase tidak valid: %v", err)
	}
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 20*time.Second)
	if err := loadLegacyData(startupCtx); err != nil {
		cancelStartup()
		log.Fatalf("gagal mengimpor data lama: %v", err)
	}
	if err := ensureInitialCandidates(startupCtx); err != nil {
		cancelStartup()
		log.Fatalf("gagal menghubungkan atau menyiapkan Supabase: %v", err)
	}
	cancelStartup()
	log.Println("Supabase REST aktif; service_role hanya digunakan server-side")

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer, middleware.Timeout(30*time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: allowedOrigins(),
		AllowedMethods: []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := listCandidates(r.Context()); err != nil {
			fail(w, http.StatusServiceUnavailable, "Supabase tidak tersedia")
			return
		}
		js(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api", func(r chi.Router) {
		r.Get("/candidates", func(w http.ResponseWriter, r *http.Request) {
			candidates, err := listCandidates(r.Context())
			if err != nil {
				log.Printf("gagal memuat kandidat dari Supabase: %v", err)
				fail(w, http.StatusInternalServerError, "Gagal memuat kandidat")
				return
			}
			js(w, http.StatusOK, candidates)
		})

		r.With(rateLimit(15)).Post("/login", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				NISN string `json:"nisn"`
			}
			if !decodeJSON(w, r, &in) {
				return
			}
			if !nisnRe.MatchString(in.NISN) {
				fail(w, http.StatusBadRequest, "NISN harus terdiri dari 9 sampai 12 digit")
				return
			}
			query := url.Values{
				"select": {"voted,blocked"},
				"nisn":   {"eq." + in.NISN},
			}
			var voters []Voter
			if err := store.decode(r.Context(), http.MethodGet, "voters", query, nil, "", &voters); err != nil {
				log.Printf("gagal memeriksa pemilih di Supabase: %v", err)
				fail(w, http.StatusInternalServerError, "Gagal memeriksa data pemilih")
				return
			}
			if len(voters) == 0 {
				fail(w, http.StatusForbidden, "NISN belum terdaftar. Hubungi admin.")
				return
			}
			if voters[0].Blocked {
				fail(w, http.StatusLocked, "blocked")
				return
			}
			key, err := token()
			if err != nil {
				fail(w, http.StatusInternalServerError, "Gagal membuat sesi")
				return
			}
			mu.Lock()
			sessions[key] = in.NISN
			mu.Unlock()
			js(w, http.StatusOK, map[string]any{"token": key, "voted": voters[0].Voted})
		})

		r.Group(func(r chi.Router) {
			r.Use(voterAuth)
			r.Get("/me", func(w http.ResponseWriter, r *http.Request) {
				nisn := r.Context().Value(ctxKey{}).(string)
				query := url.Values{
					"select": {"voted,blocked"},
					"nisn":   {"eq." + nisn},
				}
				var voters []Voter
				if err := store.decode(r.Context(), http.MethodGet, "voters", query, nil, "", &voters); err != nil {
					log.Printf("gagal memuat status pemilih dari Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal memuat status pemilih")
					return
				}
				if len(voters) == 0 {
					fail(w, http.StatusUnauthorized, "Data pemilih tidak ditemukan")
					return
				}
				js(w, http.StatusOK, map[string]bool{"voted": voters[0].Voted, "blocked": voters[0].Blocked})
			})

			r.Post("/vote", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					CandidateID int64 `json:"candidateId"`
				}
				if !decodeJSON(w, r, &in) {
					return
				}
				if in.CandidateID < 1 {
					fail(w, http.StatusBadRequest, "Kandidat tidak ditemukan")
					return
				}
				candidate, err := getCandidate(r.Context(), in.CandidateID)
				if err != nil {
					var supabaseErr *supabaseError
					if errors.As(err, &supabaseErr) {
						log.Printf("gagal memeriksa kandidat sebelum voting: %v", err)
						fail(w, http.StatusInternalServerError, "Gagal memeriksa kandidat")
					} else {
						fail(w, http.StatusBadRequest, "Kandidat tidak ditemukan")
					}
					return
				}
				var accepted bool
				err = store.decode(r.Context(), http.MethodPost, "rpc/cast_vote", nil, map[string]any{
					"p_nisn": r.Context().Value(ctxKey{}).(string), "p_candidate_id": in.CandidateID,
				}, "", &accepted)
				if err != nil {
					log.Printf("gagal menyimpan suara ke Supabase: %v", err)
					var supabaseErr *supabaseError
					if errors.As(err, &supabaseErr) && strings.Contains(supabaseErr.body, "candidate_not_found") {
						fail(w, http.StatusBadRequest, "Kandidat tidak ditemukan")
						return
					}
					fail(w, http.StatusInternalServerError, "Gagal menyimpan suara")
					return
				}
				if !accepted {
					fail(w, http.StatusForbidden, "Kamu tidak dapat memilih")
					return
				}
				mu.Lock()
				delete(sessions, bearer(r))
				mu.Unlock()
				go store.sendSheetVote(candidate.Name)
				js(w, http.StatusOK, map[string]bool{"ok": true})
			})

			r.Post("/violation", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Reason string `json:"reason"`
				}
				if !decodeJSON(w, r, &in) {
					return
				}
				if len(in.Reason) > 500 {
					fail(w, http.StatusBadRequest, "Alasan pelanggaran terlalu panjang")
					return
				}
				query := url.Values{
					"nisn":  {"eq." + r.Context().Value(ctxKey{}).(string)},
					"voted": {"eq.false"},
				}
				body := map[string]any{"blocked": true, "blocked_at": time.Now().UTC(), "reason": in.Reason}
				if err := store.decode(r.Context(), http.MethodPatch, "voters", query, body, "return=minimal", nil); err != nil {
					log.Printf("gagal menyimpan blokir ke Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal menyimpan status blokir")
					return
				}
				mu.Lock()
				delete(sessions, bearer(r))
				mu.Unlock()
				js(w, http.StatusOK, map[string]bool{"ok": true})
			})
		})

		r.With(rateLimit(5)).Post("/admin/login", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Password string `json:"password"`
			}
			if !decodeJSON(w, r, &in) {
				return
			}
			if subtle.ConstantTimeCompare([]byte(in.Password), []byte(os.Getenv("ADMIN_PASSWORD"))) != 1 {
				fail(w, http.StatusUnauthorized, "Password salah")
				return
			}
			key, err := token()
			if err != nil {
				fail(w, http.StatusInternalServerError, "Gagal membuat sesi admin")
				return
			}
			mu.Lock()
			admins[key] = time.Now().Add(8 * time.Hour)
			mu.Unlock()
			js(w, http.StatusOK, map[string]string{"token": key})
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(adminAuth)
			r.Get("/stats", func(w http.ResponseWriter, r *http.Request) {
				var stats Stats
				if err := store.decode(r.Context(), http.MethodPost, "rpc/admin_stats", nil, map[string]any{}, "", &stats); err != nil {
					log.Printf("gagal memuat statistik Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal memuat hasil pemilihan")
					return
				}
				if stats.Results == nil {
					stats.Results = []Result{}
				}
				js(w, http.StatusOK, stats)
			})

			r.Get("/voters", func(w http.ResponseWriter, r *http.Request) {
				query := url.Values{
					"select": {"nisn,voted,blocked,blocked_at,reason"},
					"order":  {"nisn.asc"},
				}
				voters := make([]Voter, 0)
				if err := store.decode(r.Context(), http.MethodGet, "voters", query, nil, "", &voters); err != nil {
					log.Printf("gagal memuat daftar pemilih Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal memuat daftar pemilih")
					return
				}
				js(w, http.StatusOK, voters)
			})

			r.Post("/voters", func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					NISN []string `json:"nisn"`
				}
				if !decodeJSON(w, r, &in) {
					return
				}
				if len(in.NISN) > 10000 {
					fail(w, http.StatusBadRequest, "Maksimal 10.000 NISN per permintaan")
					return
				}
				valid := make([]Voter, 0, len(in.NISN))
				skipped := make([]string, 0)
				seen := make(map[string]bool, len(in.NISN))
				for _, nisn := range in.NISN {
					nisn = strings.TrimSpace(nisn)
					if !nisnRe.MatchString(nisn) || seen[nisn] {
						skipped = append(skipped, nisn)
						continue
					}
					seen[nisn] = true
					valid = append(valid, Voter{NISN: nisn})
				}
				if len(valid) > 0 {
					query := url.Values{"on_conflict": {"nisn"}}
					inserted := make([]Voter, 0, len(valid))
					err := store.decode(r.Context(), http.MethodPost, "voters", query, valid,
						"resolution=ignore-duplicates,return=representation", &inserted)
					if err != nil {
						log.Printf("gagal menambahkan pemilih ke Supabase: %v", err)
						fail(w, http.StatusInternalServerError, "Gagal menambahkan pemilih")
						return
					}
					added := make(map[string]bool, len(inserted))
					for _, voter := range inserted {
						added[voter.NISN] = true
					}
					for _, voter := range valid {
						if !added[voter.NISN] {
							skipped = append(skipped, voter.NISN)
						}
					}
					js(w, http.StatusOK, map[string]any{"added": len(inserted), "skipped": skipped})
					return
				}
				js(w, http.StatusOK, map[string]any{"added": 0, "skipped": skipped})
			})

			r.Delete("/voters/{nisn}", func(w http.ResponseWriter, r *http.Request) {
				nisn := chi.URLParam(r, "nisn")
				if !nisnRe.MatchString(nisn) {
					fail(w, http.StatusBadRequest, "NISN tidak valid")
					return
				}
				query := url.Values{"nisn": {"eq." + nisn}}
				if err := store.decode(r.Context(), http.MethodDelete, "voters", query, nil, "return=minimal", nil); err != nil {
					log.Printf("gagal menghapus pemilih dari Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal menghapus pemilih")
					return
				}
				js(w, http.StatusOK, map[string]bool{"ok": true})
			})

			r.Post("/voters/{nisn}/unblock", func(w http.ResponseWriter, r *http.Request) {
				nisn := chi.URLParam(r, "nisn")
				if !nisnRe.MatchString(nisn) {
					fail(w, http.StatusBadRequest, "NISN tidak valid")
					return
				}
				query := url.Values{"nisn": {"eq." + nisn}}
				body := map[string]any{"blocked": false, "blocked_at": nil, "reason": ""}
				if err := store.decode(r.Context(), http.MethodPatch, "voters", query, body, "return=minimal", nil); err != nil {
					log.Printf("gagal membuka blokir pemilih di Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal membuka blokir pemilih")
					return
				}
				js(w, http.StatusOK, map[string]bool{"ok": true})
			})

			// Upload foto kandidat: body = bytes gambar mentah (JPEG/PNG/WebP, maks 2 MB).
			r.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
				r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
				data, err := io.ReadAll(r.Body)
				if err != nil || len(data) == 0 {
					fail(w, http.StatusRequestEntityTooLarge, "Foto kosong atau lebih dari 2 MB")
					return
				}
				contentType := http.DetectContentType(data)
				ext := map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"}[contentType]
				if ext == "" {
					fail(w, http.StatusUnsupportedMediaType, "Format foto harus JPG, PNG, atau WebP")
					return
				}
				id := make([]byte, 12)
				if _, err := rand.Read(id); err != nil {
					fail(w, http.StatusInternalServerError, "Gagal membuat nama file")
					return
				}
				photoURL, err := store.uploadObject(r.Context(), "candidate-photos", hex.EncodeToString(id)+"."+ext, contentType, data)
				if err != nil {
					log.Printf("gagal upload foto kandidat: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal mengunggah foto. Pastikan bucket candidate-photos sudah dibuat (jalankan schema.sql)")
					return
				}
				js(w, http.StatusOK, map[string]string{"url": photoURL})
			})

			r.Post("/candidates", func(w http.ResponseWriter, r *http.Request) {
				var candidate Candidate
				if !decodeJSON(w, r, &candidate) {
					return
				}
				candidate.Name = strings.TrimSpace(candidate.Name)
				candidate.Vision = strings.TrimSpace(candidate.Vision)
				candidate.PhotoURL = strings.TrimSpace(candidate.PhotoURL)
				if candidate.PhotoURL != "" && (len(candidate.PhotoURL) > 1000 ||
					!(strings.HasPrefix(candidate.PhotoURL, "https://") || strings.HasPrefix(candidate.PhotoURL, "/"))) {
					fail(w, http.StatusBadRequest, "URL foto harus diawali https:// atau /")
					return
				}
				if candidate.Number < 1 || candidate.Name == "" || candidate.Vision == "" ||
					len(candidate.Name) > 200 || len(candidate.Vision) > 5000 {
					fail(w, http.StatusBadRequest, "Nomor, nama, dan visi kandidat harus diisi dengan benar")
					return
				}
				var inserted []Candidate
				err := store.decode(r.Context(), http.MethodPost, "candidates", nil, []Candidate{candidate},
					"return=representation", &inserted)
				if err != nil {
					log.Printf("gagal menyimpan kandidat ke Supabase: %v", err)
					var supabaseErr *supabaseError
					if errors.As(err, &supabaseErr) && supabaseErr.status == http.StatusConflict {
						fail(w, http.StatusConflict, "Nomor kandidat sudah digunakan")
						return
					}
					fail(w, http.StatusInternalServerError, "Gagal menyimpan kandidat")
					return
				}
				if len(inserted) != 1 {
					fail(w, http.StatusInternalServerError, "Gagal menyimpan kandidat")
					return
				}
				js(w, http.StatusOK, inserted[0])
			})

			r.Delete("/candidates/{id}", func(w http.ResponseWriter, r *http.Request) {
				id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
				if err != nil || id < 1 {
					fail(w, http.StatusBadRequest, "ID kandidat tidak valid")
					return
				}
				query := url.Values{"id": {"eq." + strconv.FormatInt(id, 10)}}
				if err := store.decode(r.Context(), http.MethodDelete, "candidates", query, nil, "return=minimal", nil); err != nil {
					log.Printf("gagal menghapus kandidat dari Supabase: %v", err)
					fail(w, http.StatusInternalServerError, "Gagal menghapus kandidat")
					return
				}
				js(w, http.StatusOK, map[string]bool{"ok": true})
			})
		})
	})

	server := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		log.Println("API berjalan di", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server berhenti: %v", err)
		}
	case <-shutdownCtx.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown gagal: %v", err)
			if closeErr := server.Close(); closeErr != nil {
				log.Printf("gagal menutup server: %v", closeErr)
			}
		}
	}
}