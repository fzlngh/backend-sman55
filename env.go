package main

import (
	"bufio"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// loadDotEnv membaca file .env (KEY=VALUE). Variabel yang sudah ada di environment tidak ditimpa.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// validateEnv menolak start jika konfigurasi rahasia tidak aman.
func validateEnv() {
	if len(os.Getenv("ADMIN_PASSWORD")) < 10 {
		log.Fatal("ADMIN_PASSWORD wajib diisi (minimal 10 karakter). Lihat .env.example")
	}
	if os.Getenv("SHEET_URL") != "" && len(os.Getenv("SHEET_SECRET")) < 16 {
		log.Fatal("SHEET_SECRET wajib diisi (minimal 16 karakter) jika SHEET_URL dipakai")
	}
}

// rateLimit: maks n percobaan per menit per IP.
func rateLimit(n int) func(http.Handler) http.Handler {
	var m sync.Mutex
	hits := map[string][]time.Time{}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			now := time.Now()
			m.Lock()
			recent := hits[ip][:0]
			for _, t := range hits[ip] {
				if now.Sub(t) < time.Minute {
					recent = append(recent, t)
				}
			}
			if len(recent) >= n {
				hits[ip] = recent
				m.Unlock()
				fail(w, 429, "Terlalu banyak percobaan. Coba lagi dalam 1 menit.")
				return
			}
			hits[ip] = append(recent, now)
			m.Unlock()
			next.ServeHTTP(w, r)
		})
	}
}